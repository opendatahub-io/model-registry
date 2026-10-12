package activation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testIdentity() Identity {
	return Identity{CatalogUID: "uid", AttemptID: "attempt", Images: Images{Catalog: "registry/catalog@sha256:" + strings.Repeat("a", 64), Benchmark: "registry/benchmark@sha256:" + strings.Repeat("b", 64)}}
}

func TestServingGateFencesReplicasAndLiveCatalog(t *testing.T) {
	id := testIdentity()
	for _, tc := range []struct {
		name, state, identity, reason string
		generation                    int
		deleted, apiFailure, lostDB   bool
		want                          int
	}{
		{name: "committed", state: "Succeeded", generation: 1, want: 200},
		{name: "pending", state: "Pending", generation: 1, want: 503},
		{name: "failed", state: "Failed", generation: 1, want: 503},
		{name: "old replica", state: "Succeeded", identity: "replacement", generation: 1, reason: "AttemptSuperseded", want: 503},
		{name: "spec edit", state: "Succeeded", generation: 2, want: 503},
		{name: "deleted", state: "Succeeded", generation: 1, deleted: true, want: 503},
		{name: "API unavailable", state: "Succeeded", generation: 1, apiFailure: true, want: 503},
		{name: "database recreated", state: "Succeeded", generation: 1, lostDB: true, reason: "DatasetUnavailable", want: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intent := Intent{Version: ProtocolVersion, CatalogName: "catalog", Generation: 1, Identity: id, State: tc.state, Stage: "Activation", Reason: "Activation" + tc.state}
			if tc.identity != "" {
				intent.AttemptID = tc.identity
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.apiFailure {
					w.WriteHeader(503)
					return
				}
				var value any
				if strings.Contains(r.URL.Path, "/catalogs/") {
					metadata := map[string]any{"uid": "uid", "generation": tc.generation}
					if tc.deleted {
						metadata["deletionTimestamp"] = "2026-10-12T10:00:00Z"
					}
					value = map[string]any{"metadata": metadata}
				} else {
					data, _ := json.Marshal(intent)
					value = controlMap{Data: map[string]string{"intent.json": string(data)}}
				}
				require.NoError(t, json.NewEncoder(w).Encode(value))
			}))
			defer server.Close()
			token := filepath.Join(t.TempDir(), "token")
			writeFile(t, token, "test")
			c := &Control{Client: server.Client(), BaseURL: server.URL, TokenPath: token, IntentName: "intent", Namespace: "ns", Identity: id}
			handler := c.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("data")) }), func(context.Context) error {
				if tc.lostDB {
					return errors.New("lost database")
				}
				return nil
			})
			for _, path := range []string{"/api/model_catalog/v1/models", "/api/mcp_catalog/v1alpha1/mcp_servers", "/readyz"} {
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, httptest.NewRequest("GET", path, nil))
				require.Equal(t, tc.want, res.Code, path)
				require.Equal(t, "no-store", res.Header().Get("Cache-Control"))
				if tc.reason != "" {
					require.Contains(t, res.Body.String(), tc.reason)
				}
			}
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, httptest.NewRequest("GET", "/healthz", nil))
			require.Equal(t, 200, res.Code)
			res = httptest.NewRecorder()
			handler.ServeHTTP(res, httptest.NewRequest("GET", "/statusz", nil))
			require.Equal(t, 200, res.Code)
			var status map[string]any
			require.NoError(t, json.Unmarshal(res.Body.Bytes(), &status))
			require.Equal(t, tc.want == 200, status["available"])
			if tc.want == 200 {
				require.Equal(t, "CatalogAvailable", status["code"])
			}
		})
	}
}

func TestReportPreservesOwnershipAndResourceVersion(t *testing.T) {
	metadata := json.RawMessage(`{"name":"report","resourceVersion":"8","labels":{"app":"catalog"},"ownerReferences":[{"uid":"uid"}]}`)
	var written controlMap
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cm := controlMap{APIVersion: "v1", Kind: "ConfigMap", Metadata: metadata}
		if r.Method == http.MethodPut {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&written))
			cm = written
		}
		require.NoError(t, json.NewEncoder(w).Encode(cm))
	}))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "token")
	writeFile(t, token, "test")
	c := &Control{Client: server.Client(), BaseURL: server.URL, TokenPath: token, ReportName: "report", Identity: testIdentity()}
	require.NoError(t, c.Report(context.Background(), nil))
	require.JSONEq(t, string(metadata), string(written.Metadata))
	var report Report
	require.NoError(t, json.Unmarshal([]byte(written.Data["report.json"]), &report))
	require.Equal(t, c.Identity, report.Identity)
	require.Equal(t, "Succeeded", report.State)
}
