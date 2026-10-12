package activation

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Control uses the pod's projected service-account credentials. Runtime RBAC
// permits reading intent and updating only this attempt's outcome ConfigMap.
// Reads are uncached; watches or mounted ConfigMaps cannot revoke serving promptly.
type Control struct {
	Client     *http.Client
	BaseURL    string
	Namespace  string
	IntentName string
	ReportName string
	TokenPath  string
	Identity   Identity
}

type controlMap struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   json.RawMessage   `json:"metadata"`
	Data       map[string]string `json:"data"`
}

func ControlFromEnvironment() (*Control, error) {
	name := os.Getenv("CATALOG_ACTIVATION_INTENT")
	if name == "" {
		return nil, nil
	}
	id := Identity{CatalogUID: os.Getenv("CATALOG_UID"), AttemptID: os.Getenv("CATALOG_ACTIVATION_ATTEMPT"), Images: Images{Catalog: os.Getenv("CATALOG_DATA_IMAGE"), Benchmark: os.Getenv("CATALOG_BENCHMARK_IMAGE")}}
	// A bootstrap pod may pull tagged release images, but cannot activate them.
	// The operator first resolves its init image IDs and replaces it with a pinned pod.
	ca, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid Kubernetes service-account CA")
	}
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("missing Kubernetes API address")
	}
	return &Control{Client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}}, BaseURL: "https://" + net.JoinHostPort(host, port), Namespace: os.Getenv("POD_NAMESPACE"), IntentName: name, ReportName: os.Getenv("CATALOG_ACTIVATION_REPORT"), TokenPath: "/var/run/secrets/kubernetes.io/serviceaccount/token", Identity: id}, nil
}

func (c *Control) request(ctx context.Context, method, name string, input any) (*controlMap, error) {
	return c.apiRequest(ctx, method, "/api/v1/namespaces/"+url.PathEscape(c.Namespace)+"/configmaps/"+url.PathEscape(name), input)
}

func (c *Control) apiRequest(ctx context.Context, method, name string, input any) (*controlMap, error) {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	endpoint := c.BaseURL + name
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	token, err := os.ReadFile(c.TokenPath)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("activation control %s returned HTTP %d", method, resp.StatusCode)
	}
	var cm controlMap
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cm); err != nil {
		return nil, err
	}
	return &cm, nil
}

func (c *Control) Intent(ctx context.Context) (*Intent, error) {
	cm, err := c.request(ctx, http.MethodGet, c.IntentName, nil)
	if err != nil {
		return nil, err
	}
	var intent Intent
	if err := json.Unmarshal([]byte(cm.Data["intent.json"]), &intent); err != nil {
		return nil, err
	}
	if intent.Version != ProtocolVersion {
		return nil, fmt.Errorf("unsupported activation protocol %q", intent.Version)
	}
	// A spec edit, retry request or deletion revokes a previously published
	// authorization even before the controller observes it.
	catalog, err := c.apiRequest(ctx, http.MethodGet, "/apis/aihub.opendatahub.io/v1alpha1/namespaces/"+url.PathEscape(c.Namespace)+"/catalogs/"+url.PathEscape(intent.CatalogName), nil)
	if err != nil {
		return nil, err
	}
	var metadata struct {
		UID               string            `json:"uid"`
		Generation        int64             `json:"generation"`
		DeletionTimestamp *string           `json:"deletionTimestamp"`
		Annotations       map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(catalog.Metadata, &metadata); err != nil {
		return nil, err
	}
	if metadata.UID != intent.CatalogUID || metadata.Generation != intent.Generation || metadata.DeletionTimestamp != nil || metadata.Annotations["aihub.opendatahub.io/retry-activation"] != intent.RetryToken {
		return nil, fmt.Errorf("activation intent does not match the current Catalog")
	}
	return &intent, nil
}

func (c *Control) Current(ctx context.Context) error {
	intent, err := c.Intent(ctx)
	if err != nil {
		return err
	}
	if intent.Identity != c.Identity || (intent.State != "Pending" && intent.State != "Succeeded" && intent.State != "Failed") || (intent.Stage == "Selection" || intent.Stage == "Import") {
		return fmt.Errorf("activation attempt is superseded or blocked: %s", intent.Reason)
	}
	return c.Identity.Validate()
}

func (c *Control) Report(ctx context.Context, failure *Failure) error {
	// A late process only has access to its own report object. It never writes
	// intent or the report of a replacement attempt.
	cm, err := c.request(ctx, http.MethodGet, c.ReportName, nil)
	if err != nil {
		return err
	}
	report := Report{Version: ProtocolVersion, Identity: c.Identity, State: "Succeeded", Stage: "Activation", Reason: "ActivationSucceeded", Message: "Required catalog and benchmark content committed", ActivatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if failure != nil {
		report.State, report.Stage, report.Reason, report.Message, report.ActivatedAt = "Failed", failure.Stage, failure.Reason, failure.Message, ""
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["report.json"] = string(data)
	_, err = c.request(ctx, http.MethodPut, c.ReportName, cm)
	return err // A conflict must never silently overwrite another observation.
}

// Handler applies the same gate to all API paths and readiness, including direct
// pod access. Health and diagnostic status remain reachable while the gate closes.
func (c *Control) Handler(next http.Handler, committed func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		intent, err := c.Intent(r.Context())
		status := http.StatusServiceUnavailable
		response := map[string]any{"code": "CatalogUnavailable", "stage": "Activation", "reason": "AuthorizationUnavailable", "message": "Catalog data is temporarily unavailable."}
		if err == nil {
			response["attemptID"], response["stage"], response["reason"], response["message"] = intent.AttemptID, intent.Stage, intent.Reason, intent.Message
			switch {
			case intent.Identity != c.Identity:
				response["reason"], response["message"] = "AttemptSuperseded", "This replica does not contain the current catalog dataset. Retry through the catalog service."
			case intent.State == "Succeeded":
				if committed(r.Context()) == nil {
					status = http.StatusOK
				} else {
					response["reason"], response["message"] = "DatasetUnavailable", "The committed catalog dataset is unavailable. Activation must complete before data can be served."
				}
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/statusz" {
			response["available"] = status == http.StatusOK
			if status == http.StatusOK {
				response["code"] = "CatalogAvailable"
			}
			status = http.StatusOK
		} else if status == http.StatusOK {
			next.ServeHTTP(w, r)
			return
		}
		if status == http.StatusServiceUnavailable {
			response["message"] = fmt.Sprint(response["message"]) + " Ask an administrator to check Catalog status and correct the selected images or retry activation."
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(response)
	})
}
