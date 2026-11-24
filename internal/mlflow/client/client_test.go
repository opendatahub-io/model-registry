package client_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedRequest captures what the client actually sent.
type recordedRequest struct {
	Method  string
	Path    string
	Query   string
	Body    map[string]any
	RawBody string
	Header  http.Header
}

func newRecordingServer(t *testing.T, handler func(recordedRequest) (int, string)) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	recorded := &[]recordedRequest{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := []byte{}
		body := map[string]any{}
		if r.Body != nil {
			raw, _ = io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &body)
			}
		}
		entry := recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body, RawBody: string(raw), Header: r.Header.Clone()}
		*recorded = append(*recorded, entry)

		status, payload := handler(entry)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)

	return server, recorded
}

// guardVerbs returns 405 for requests to the guarded paths using any
// method other than the expected one, mirroring a REST server that
// rejects the wrong verb.
func guardVerbs(expected map[string]string, next func(recordedRequest) (int, string)) func(recordedRequest) (int, string) {
	return func(req recordedRequest) (int, string) {
		if method, ok := expected[req.Path]; ok && req.Method != method {
			return http.StatusMethodNotAllowed, `{"error_code": "METHOD_NOT_ALLOWED", "message": "wrong verb"}`
		}
		return next(req)
	}
}

func TestClientVerbsAndPaths(t *testing.T) {
	server, recorded := newRecordingServer(t, guardVerbs(map[string]string{
		"/api/2.0/mlflow/registered-models/search":     http.MethodGet,
		"/api/2.0/mlflow/registered-models/delete-tag": http.MethodDelete,
		"/api/2.0/mlflow/model-versions/search":        http.MethodGet,
		"/api/2.0/mlflow/model-versions/delete-tag":    http.MethodDelete,
	}, func(req recordedRequest) (int, string) {
		switch {
		case req.Path == "/api/2.0/mlflow/registered-models/create" && req.Method == http.MethodPost:
			return 200, `{"registered_model": {"name": "m"}}`
		case req.Path == "/api/2.0/mlflow/registered-models/get" && req.Method == http.MethodGet:
			return 200, `{"registered_model": {"name": "m"}}`
		case req.Path == "/api/2.0/mlflow/registered-models/search" && req.Method == http.MethodGet:
			return 200, `{"registered_models": [], "next_page_token": "tok-2"}`
		case req.Path == "/api/2.0/mlflow/registered-models/update" && req.Method == http.MethodPatch:
			return 200, `{}`
		case req.Path == "/api/2.0/mlflow/registered-models/set-tag" && req.Method == http.MethodPost:
			return 200, `{}`
		case req.Path == "/api/2.0/mlflow/registered-models/delete-tag" && req.Method == http.MethodDelete:
			return 200, `{}`
		case req.Path == "/api/2.0/mlflow/registered-models/alias" && req.Method == http.MethodPost:
			return 200, `{}`
		case req.Path == "/api/2.0/mlflow/model-versions/create" && req.Method == http.MethodPost:
			return 200, `{"model_version": {"name": "m", "version": "1"}}`
		case req.Path == "/api/2.0/mlflow/model-versions/get" && req.Method == http.MethodGet:
			return 200, `{"model_version": {"name": "m", "version": "1"}}`
		case req.Path == "/api/2.0/mlflow/model-versions/search" && req.Method == http.MethodGet:
			return 200, `{"model_versions": []}`
		case req.Path == "/api/2.0/mlflow/model-versions/update" && req.Method == http.MethodPatch:
			return 200, `{}`
		case req.Path == "/api/2.0/mlflow/model-versions/set-tag" && req.Method == http.MethodPost:
			return 200, `{}`
		case req.Path == "/api/2.0/mlflow/model-versions/delete-tag" && req.Method == http.MethodDelete:
			return 200, `{}`
		}
		return 404, `{"error_code": "RESOURCE_DOES_NOT_EXIST", "message": "unexpected path"}`
	}))

	c, err := client.New(server.URL, server.Client())
	require.NoError(t, err)
	ctx := t.Context()

	_, err = c.CreateRegisteredModel(ctx, client.CreateRegisteredModelRequest{Name: "m", Tags: []client.Tag{{Key: "k", Value: "v"}}})
	require.NoError(t, err)

	_, err = c.GetRegisteredModel(ctx, "m")
	require.NoError(t, err)

	_, err = c.SearchRegisteredModels(ctx, client.SearchRequest{Filter: "name = 'm'", MaxResults: 50})
	require.NoError(t, err)

	require.NoError(t, c.UpdateRegisteredModel(ctx, client.UpdateRegisteredModelRequest{Name: "m", Description: "d"}))
	require.NoError(t, c.SetRegisteredModelTag(ctx, "m", "rhoai.owner", "alice"))
	require.NoError(t, c.DeleteRegisteredModelTag(ctx, "m", "rhoai.owner"))
	require.NoError(t, c.SetRegisteredModelAlias(ctx, "m", "rhoai-x", "1"))

	_, err = c.CreateModelVersion(ctx, client.CreateVersionRequest{Name: "m", Source: "s3://bucket/a.onnx"})
	require.NoError(t, err)

	_, err = c.GetModelVersion(ctx, "m", "1")
	require.NoError(t, err)

	_, err = c.SearchModelVersions(ctx, client.SearchRequest{Filter: "name = 'm'"})
	require.NoError(t, err)

	require.NoError(t, c.UpdateModelVersion(ctx, client.UpdateVersionRequest{Name: "m", Version: "1", Description: "d"}))
	require.NoError(t, c.SetModelVersionTag(ctx, "m", "1", "rhoai.versionName", "1.0.0"))
	require.NoError(t, c.DeleteModelVersionTag(ctx, "m", "1", "rhoai.versionName"))

	// The JSON tag array round-trips as an array of objects.
	create := (*recorded)[0]
	tags, ok := create.Body["tags"].([]any)
	require.True(t, ok, "tags must be a JSON array")
	require.Len(t, tags, 1)
	tag := tags[0].(map[string]any)
	require.Equal(t, "k", tag["key"])
	require.Equal(t, "v", tag["value"])

	// Pagination tokens are passed through and returned.
	require.Equal(t, "tok-2", mustSearchModels(t, c).NextPageToken)
}

func mustSearchModels(t *testing.T, c *client.Client) *client.SearchRegisteredModelsResponse {
	t.Helper()
	result, err := c.SearchRegisteredModels(t.Context(), client.SearchRequest{})
	require.NoError(t, err)
	return result
}

func TestClientSearchQuery(t *testing.T) {
	const filter = `name = 'ünïcode "quoted" model'`
	searchRequest := client.SearchRequest{
		Filter:     filter,
		MaxResults: 7,
		OrderBy:    []string{"name ASC", "last_updated_timestamp DESC"},
		PageToken:  "page-2",
	}

	t.Run("registered models", func(t *testing.T) {
		server, recorded := newRecordingServer(t, func(req recordedRequest) (int, string) {
			if req.Path == "/api/2.0/mlflow/registered-models/search" && req.Method == http.MethodGet {
				return 200, `{"registered_models": []}`
			}
			return 405, `{}`
		})
		c, err := client.New(server.URL, server.Client())
		require.NoError(t, err)

		_, err = c.SearchRegisteredModels(t.Context(), searchRequest)
		require.NoError(t, err)
		assertSearchQuery(t, (*recorded)[0])
	})

	t.Run("model versions", func(t *testing.T) {
		server, recorded := newRecordingServer(t, func(req recordedRequest) (int, string) {
			if req.Path == "/api/2.0/mlflow/model-versions/search" && req.Method == http.MethodGet {
				return 200, `{"model_versions": []}`
			}
			return 405, `{}`
		})
		c, err := client.New(server.URL, server.Client())
		require.NoError(t, err)

		_, err = c.SearchModelVersions(t.Context(), searchRequest)
		require.NoError(t, err)
		assertSearchQuery(t, (*recorded)[0])
	})

	t.Run("base path preserved", func(t *testing.T) {
		server, recorded := newRecordingServer(t, func(req recordedRequest) (int, string) {
			if req.Path == "/prefix/api/2.0/mlflow/registered-models/search" {
				return 200, `{"registered_models": []}`
			}
			return 404, `{}`
		})
		c, err := client.New(server.URL+"/prefix/", server.Client())
		require.NoError(t, err)

		_, err = c.SearchRegisteredModels(t.Context(), searchRequest)
		require.NoError(t, err)
		require.Len(t, *recorded, 1)
		require.Equal(t, "/prefix/api/2.0/mlflow/registered-models/search", (*recorded)[0].Path)
		assertSearchQuery(t, (*recorded)[0])
	})
}

func assertSearchQuery(t *testing.T, request recordedRequest) {
	t.Helper()
	require.Equal(t, http.MethodGet, request.Method)
	require.Empty(t, request.RawBody, "searches must not carry a JSON body")

	values, err := url.ParseQuery(request.Query)
	require.NoError(t, err)
	require.Equal(t, []string{`name = 'ünïcode "quoted" model'`}, values["filter"])
	require.Equal(t, []string{"7"}, values["max_results"])
	require.Equal(t, []string{"name ASC", "last_updated_timestamp DESC"}, values["order_by"])
	require.Equal(t, []string{"page-2"}, values["page_token"])
}

func TestClientDeleteTags(t *testing.T) {
	t.Run("registered model", func(t *testing.T) {
		server, recorded := newRecordingServer(t, func(req recordedRequest) (int, string) {
			if req.Path == "/api/2.0/mlflow/registered-models/delete-tag" && req.Method == http.MethodDelete {
				return 200, `{}`
			}
			return 405, `{}`
		})
		c, err := client.New(server.URL, server.Client())
		require.NoError(t, err)

		require.NoError(t, c.DeleteRegisteredModelTag(t.Context(), "model", "rhoai.owner"))
		require.Len(t, *recorded, 1)
		require.Equal(t, http.MethodDelete, (*recorded)[0].Method)
		require.Equal(t, map[string]any{"name": "model", "key": "rhoai.owner"}, (*recorded)[0].Body)
	})

	t.Run("model version", func(t *testing.T) {
		server, recorded := newRecordingServer(t, func(req recordedRequest) (int, string) {
			if req.Path == "/api/2.0/mlflow/model-versions/delete-tag" && req.Method == http.MethodDelete {
				return 200, `{}`
			}
			return 405, `{}`
		})
		c, err := client.New(server.URL, server.Client())
		require.NoError(t, err)

		require.NoError(t, c.DeleteModelVersionTag(t.Context(), "model", "5", "rhoai.versionName"))
		require.Len(t, *recorded, 1)
		require.Equal(t, http.MethodDelete, (*recorded)[0].Method)
		require.Equal(t, map[string]any{"name": "model", "version": "5", "key": "rhoai.versionName"}, (*recorded)[0].Body)
	})
}

// TestClientAmbiguousMutationResponses proves a 2xx response that cannot be
// decoded or does not identify the created object is an ambiguous outcome,
// never a definitive failure or a trigger for a second create.
func TestClientAmbiguousMutationResponses(t *testing.T) {
	oversized := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"registered_model": {"name": "`))
		chunk := make([]byte, 1<<20)
		remaining := int64(32<<20 + 2)
		for remaining > 0 {
			n := min(remaining, int64(len(chunk)))
			if _, err := w.Write(chunk[:n]); err != nil {
				return
			}
			remaining -= n
		}
	}

	cases := []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"truncated json", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"registered_model": {"name":`))
		}},
		{"empty body", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
		}},
		{"oversized body", oversized},
		{"missing model identity", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"registered_model": {"description": "no name"}}`))
		}},
		{"missing version identity", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"model_version": {"name": "m"}}`))
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				testCase.handler(w, r)
			}))
			t.Cleanup(server.Close)

			c, err := client.New(server.URL, server.Client())
			require.NoError(t, err)

			_, modelErr := c.CreateRegisteredModel(t.Context(), client.CreateRegisteredModelRequest{Name: "m"})
			requireAmbiguous(t, modelErr)
			_, versionErr := c.CreateModelVersion(t.Context(), client.CreateVersionRequest{Name: "m", Source: "s3://bucket/model.onnx"})
			requireAmbiguous(t, versionErr)
			require.Equal(t, 2, requests, "an undecodable create response must not trigger a second create")
		})
	}
}

func requireAmbiguous(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	typed, ok := client.AsError(err)
	require.True(t, ok, "expected a client.Error, got %T: %v", err, err)
	assert.True(t, typed.Ambiguous, "a malformed success response is an unknown outcome, not a definitive failure")
	mapped := mlflow.MapClientError(err)
	require.Error(t, mapped)
	assert.Equal(t, http.StatusServiceUnavailable, api.ErrToStatus(mapped))
}

func TestClientBaseURLPrefixAndQueryEscaping(t *testing.T) {
	server, recorded := newRecordingServer(t, func(req recordedRequest) (int, string) {
		if req.Path == "/prefix/api/2.0/mlflow/model-versions/get" {
			return 200, `{"model_version": {}}`
		}
		return 404, `{}`
	})

	c, err := client.New(server.URL+"/prefix/", server.Client())
	require.NoError(t, err)

	_, err = c.GetModelVersion(t.Context(), "model with spaces&stuff", "12")
	require.NoError(t, err)

	require.Len(t, *recorded, 1)
	query := (*recorded)[0].Query
	require.Contains(t, query, "name=model+with+spaces%26stuff")
	require.Contains(t, query, "version=12")
}

func TestClientStatusErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		// call is the request under test; defaults to a registered-model
		// create when nil.
		call  func(*client.Client) error
		check func(*testing.T, error)
	}{
		{401, `{"error_code": "PERMISSION_DENIED", "message": "unauthorized"}`, nil, func(t *testing.T, err error) {
			typed := requireClientError(t, err)
			assert.True(t, typed.IsUnauthorized())
			assert.Equal(t, "PERMISSION_DENIED", typed.Code)
		}},
		{403, `{"error_code": "PERMISSION_DENIED", "message": "forbidden"}`, nil, func(t *testing.T, err error) {
			typed := requireClientError(t, err)
			assert.True(t, typed.IsForbidden())
		}},
		{404, `{"error_code": "RESOURCE_DOES_NOT_EXIST", "message": "nope"}`, nil, func(t *testing.T, err error) {
			typed := requireClientError(t, err)
			assert.True(t, typed.IsNotFound())
		}},
		{400, `{"error_code": "INVALID_PARAMETER_VALUE", "message": "bad name"}`, nil, func(t *testing.T, err error) {
			typed := requireClientError(t, err)
			assert.True(t, typed.IsInvalidInput())
		}},
		{429, `{"error_code": "RESOURCE_LIMIT_EXCEEDED", "message": "slow down"}`, nil, func(t *testing.T, err error) {
			typed := requireClientError(t, err)
			assert.False(t, typed.IsInvalidInput())
		}},
		{500, `{"error_code": "INTERNAL_ERROR", "message": "boom"}`, func(c *client.Client) error {
			// A tag write is an idempotent mutation: a 500 is a definitive
			// failure the service may safely retry. ( Creates receiving a
			// 5xx are covered separately and stay ambiguous. )
			return c.SetRegisteredModelTag(t.Context(), "m", "rhoai.owner", "alice")
		}, func(t *testing.T, err error) {
			typed := requireClientError(t, err)
			assert.False(t, typed.Ambiguous)
		}},
		{503, `not json at all`, nil, func(t *testing.T, err error) {
			typed := requireClientError(t, err)
			assert.Equal(t, 503, typed.Status)
			assert.NotContains(t, err.Error(), "not json", "raw upstream bodies are not forwarded")
		}},
	}

	for _, testCase := range cases {
		t.Run(http.StatusText(testCase.status), func(t *testing.T) {
			server, _ := newRecordingServer(t, func(recordedRequest) (int, string) {
				return testCase.status, testCase.body
			})
			c, err := client.New(server.URL, server.Client())
			require.NoError(t, err)

			var requestErr error
			if testCase.call != nil {
				requestErr = testCase.call(c)
			} else {
				_, requestErr = c.CreateRegisteredModel(t.Context(), client.CreateRegisteredModelRequest{Name: "m"})
			}
			require.Error(t, requestErr)
			testCase.check(t, requestErr)
		})
	}
}

func requireClientError(t *testing.T, err error) *client.Error {
	t.Helper()
	typed, ok := client.AsError(err)
	require.True(t, ok, "expected a client.Error, got %T: %v", err, err)
	return typed
}

func TestClientMalformedJSON(t *testing.T) {
	server, _ := newRecordingServer(t, func(recordedRequest) (int, string) {
		return 200, `{"registered_model": {"name":`
	})
	c, err := client.New(server.URL, server.Client())
	require.NoError(t, err)

	_, err = c.GetRegisteredModel(t.Context(), "m")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "client.Error")
}

func TestClientCreateConnectionDroppedIsAmbiguous(t *testing.T) {
	// The server reads the create request and then drops the connection
	// without responding: the mutation outcome is unknown.
	dropped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = r.Body.Read(make([]byte, 4096))
		if hijacker, ok := w.(http.Hijacker); ok {
			conn, _, _ := hijacker.Hijack()
			_ = conn.Close()
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(dropped.Close)

	c, err := client.New(dropped.URL, dropped.Client())
	require.NoError(t, err)

	_, err = c.CreateModelVersion(t.Context(), client.CreateVersionRequest{Name: "m", Source: "s3://bucket/a.onnx"})
	require.Error(t, err)
	typed := requireClientError(t, err)
	assert.True(t, typed.Ambiguous, "a dropped create connection must be ambiguous, never blindly retried")

	// Reads are not mutations: the same failure is a definitive
	// (non-ambiguous) unavailability, surfaced as a typed 503 error so the
	// REST layer maps it to Service Unavailable.
	_, err = c.GetRegisteredModel(t.Context(), "m")
	require.Error(t, err)
	readErr, isTyped := client.AsError(err)
	require.True(t, isTyped, "read transport failures are typed client errors")
	assert.Equal(t, http.StatusServiceUnavailable, readErr.Status)
	assert.False(t, readErr.Ambiguous)
}

func TestClientCancellation(t *testing.T) {
	block := make(chan struct{})
	server, _ := newRecordingServer(t, func(recordedRequest) (int, string) {
		<-block
		return 200, `{}`
	})
	t.Cleanup(func() { close(block) })

	c, err := client.New(server.URL, server.Client())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.CreateRegisteredModel(ctx, client.CreateRegisteredModelRequest{Name: "m"})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestClientInvalidBaseURL(t *testing.T) {
	_, err := client.New("ftp://registry", nil)
	require.Error(t, err)

	_, err = client.New("://bad url", nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "://bad url", "invalid URLs are not echoed verbatim")
}

func TestClientDefaultTimeout(t *testing.T) {
	c, err := client.New("http://localhost:1", nil)
	require.NoError(t, err)
	_, err = c.GetRegisteredModel(t.Context(), "m")
	require.Error(t, err)
}

func TestRedactURL(t *testing.T) {
	redacted := client.RedactURL("https://example.com/path?token=abc&X-Amz-Signature=sig&safe=1")
	assert.NotContains(t, redacted, "abc")
	assert.NotContains(t, redacted, "sig")
	assert.Contains(t, redacted, "safe=1")

	assert.NotContains(t, client.RedactURL("https://user:pass@example.com/"), "pass")
}

func TestTransportConfig(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(" token-value \n"), 0o600))
	passwordFile := filepath.Join(dir, "password")
	require.NoError(t, os.WriteFile(passwordFile, []byte(" secret \n"), 0o600))

	t.Run("bearer token", func(t *testing.T) {
		server, recorded := newRecordingServer(t, func(recordedRequest) (int, string) {
			return 200, `{}`
		})
		httpClient, err := client.TransportConfig{BearerTokenFile: tokenFile}.HTTPClient()
		require.NoError(t, err)

		c, err := client.New(server.URL, httpClient)
		require.NoError(t, err)
		require.NoError(t, c.SetRegisteredModelTag(t.Context(), "m", "k", "v"))
		require.Equal(t, "Bearer token-value", (*recorded)[0].Header.Get("Authorization"))
	})

	t.Run("basic auth", func(t *testing.T) {
		server, recorded := newRecordingServer(t, func(recordedRequest) (int, string) {
			return 200, `{}`
		})
		httpClient, err := client.TransportConfig{Username: "user", PasswordFile: passwordFile}.HTTPClient()
		require.NoError(t, err)

		c, err := client.New(server.URL, httpClient)
		require.NoError(t, err)
		require.NoError(t, c.SetRegisteredModelTag(t.Context(), "m", "k", "v"))
		authorization := (*recorded)[0].Header.Get("Authorization")
		require.True(t, strings.HasPrefix(authorization, "Basic "))
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authorization, "Basic "))
		require.NoError(t, err)
		require.Equal(t, "user:secret", string(decoded))
	})

	t.Run("incoming authorization is not forwarded", func(t *testing.T) {
		server, recorded := newRecordingServer(t, func(recordedRequest) (int, string) {
			return 200, `{}`
		})
		httpClient, err := client.TransportConfig{BearerTokenFile: tokenFile}.HTTPClient()
		require.NoError(t, err)

		// Simulate an inbound request carrying a foreign token by sending
		// one on the outgoing request; the transport must replace it.
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			server.URL+"/api/2.0/mlflow/registered-models/get?name=m", nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer inbound-user-token")
		response, err := httpClient.Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())

		require.Equal(t, "Bearer token-value", (*recorded)[0].Header.Get("Authorization"))
	})

	t.Run("simultaneous modes rejected", func(t *testing.T) {
		_, err := client.TransportConfig{BearerTokenFile: tokenFile, Username: "u", PasswordFile: passwordFile}.HTTPClient()
		require.Error(t, err)
	})

	t.Run("partial basic auth rejected", func(t *testing.T) {
		_, err := client.TransportConfig{Username: "u"}.HTTPClient()
		require.Error(t, err)
	})

	t.Run("bad CA file rejected", func(t *testing.T) {
		_, err := client.TransportConfig{CAFile: filepath.Join(dir, "nope.pem")}.HTTPClient()
		require.Error(t, err)

		empty := filepath.Join(dir, "empty.pem")
		require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))
		_, err = client.TransportConfig{CAFile: empty}.HTTPClient()
		require.Error(t, err)
	})
}

// TestClientRedirectNeverForwardsCredentials proves the registry client
// never follows redirects: the destination ( a different host ) receives
// zero requests, so credentials cannot leak to it, and the mutation is
// reported as an uncertain outcome rather than a replayed create.
func TestClientRedirectNeverForwardsCredentials(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("review-fake-token\n"), 0o600))
	passwordFile := filepath.Join(dir, "password")
	require.NoError(t, os.WriteFile(passwordFile, []byte("review-fake-secret\n"), 0o600))

	authModes := map[string]func(t *testing.T) *http.Client{
		"no auth": func(t *testing.T) *http.Client { return nil },
		"bearer": func(t *testing.T) *http.Client {
			httpClient, err := client.TransportConfig{BearerTokenFile: tokenFile}.HTTPClient()
			require.NoError(t, err)
			return httpClient
		},
		"basic": func(t *testing.T) *http.Client {
			httpClient, err := client.TransportConfig{Username: "user", PasswordFile: passwordFile}.HTTPClient()
			require.NoError(t, err)
			return httpClient
		},
	}

	for mode, httpClient := range authModes {
		for _, status := range []int{301, 302, 303, 307, 308} {
			t.Run(fmt.Sprintf("%s/%d", mode, status), func(t *testing.T) {
				// The destination is reached through a different host name
				// so a followed redirect would be observable.
				destinationHits := 0
				destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					destinationHits++
					w.WriteHeader(200)
				}))
				t.Cleanup(destination.Close)
				redirectTarget := strings.Replace(destination.URL, "127.0.0.1", "localhost", 1)

				sourceHits := 0
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					sourceHits++
					w.Header().Set("Location", redirectTarget)
					w.WriteHeader(status)
				}))
				t.Cleanup(source.Close)

				c, err := client.New(source.URL, httpClient(t))
				require.NoError(t, err)

				_, err = c.CreateModelVersion(t.Context(), client.CreateVersionRequest{
					Name: "m", Source: "s3://bucket/model.onnx",
				})
				require.Error(t, err)
				typed, ok := client.AsError(err)
				require.True(t, ok, "expected a client.Error, got %T: %v", err, err)
				assert.True(t, typed.Ambiguous, "an unexpected redirect leaves the mutation outcome unknown")
				mapped := mlflow.MapClientError(err)
				require.Error(t, mapped)
				assert.Equal(t, http.StatusServiceUnavailable, api.ErrToStatus(mapped))

				mutationHits := sourceHits
				_, err = c.GetRegisteredModel(t.Context(), "m")
				require.Error(t, err, "a redirected read is a failure, not an empty response")

				assert.Equal(t, 1, mutationHits, "the original create must be dispatched exactly once")
				assert.Zero(t, destinationHits, "the redirect destination must receive zero requests")
			})
		}
	}

	t.Run("https to http destination", func(t *testing.T) {
		destinationHits := 0
		destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			destinationHits++
			w.WriteHeader(200)
		}))
		t.Cleanup(destination.Close)

		source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", destination.URL)
			w.WriteHeader(307)
		}))
		t.Cleanup(source.Close)

		c, err := client.New(source.URL, source.Client())
		require.NoError(t, err)

		_, err = c.CreateModelVersion(t.Context(), client.CreateVersionRequest{Name: "m", Source: "s3://b/m.onnx"})
		require.Error(t, err)
		typed, ok := client.AsError(err)
		require.True(t, ok)
		assert.True(t, typed.Ambiguous)
		assert.Zero(t, destinationHits, "the http destination must receive zero requests")
	})
}

func TestClientRejectsCredentialBearingURL(t *testing.T) {
	_, err := client.New("https://user:review-fake-secret@registry.example.com", nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "review-fake-secret", "rejected URLs are not echoed")
}

// TestClientUpstreamErrorSanitization proves upstream error bodies — which
// may contain credentials, userinfo, or signed URLs — never reach Go error
// strings or the mapped client-facing errors.
func TestClientUpstreamErrorSanitization(t *testing.T) {
	const secret = "review-fake-secret"
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"json bearer credential", 400, `{"error_code": "INVALID_PARAMETER_VALUE", "message": "token ` + secret + ` rejected"}`},
		{"plain basic credential", 404, "Authorization: Basic " + secret + " is invalid"},
		{"userinfo url", 429, `{"error_code": "RESOURCE_LIMIT_EXCEEDED", "message": "cannot reach https://user:` + secret + `@example.com/path"}`},
		{"signed source url", 500, `{"error_code": "INTERNAL_ERROR", "message": "failed to fetch https://example.com/model?X-Amz-Signature=` + secret + `"}`},
		{"mixed-case query key", 503, `{"error_code": "TEMPORARILY_UNAVAILABLE", "message": "upstream https://example.com/m?Token=` + secret + ` unavailable"}`},
		{"raw html", 400, "<html><body>password " + secret + "</body></html>"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, _ := newRecordingServer(t, func(recordedRequest) (int, string) {
				return testCase.status, testCase.body
			})
			c, err := client.New(server.URL, server.Client())
			require.NoError(t, err)

			_, err = c.CreateModelVersion(t.Context(), client.CreateVersionRequest{Name: "m", Source: "s3://b/m.onnx"})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), secret)

			mapped := mlflow.MapClientError(err)
			require.Error(t, mapped)
			assert.NotContains(t, mapped.Error(), secret)
		})
	}
}

func TestClientBodyIsClosedAndBounded(t *testing.T) {
	// A slow, large body must not hang the client: the response is
	// bounded and the body closed.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"registered_model": {"name": "`))
		for range 1 << 12 {
			_, _ = w.Write([]byte("a"))
		}
	}))
	t.Cleanup(server.Close)

	c, err := client.New(server.URL, server.Client())
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := c.GetRegisteredModel(context.Background(), "m")
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("client did not terminate on a large malformed body")
	}
}

// failingTransport fails every request with a fixed transport error.
type failingTransport struct{ err error }

func (f *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

// TestClientUnavailableReadsMapToServiceUnavailable proves a read that fails
// at the transport layer surfaces as a typed, non-ambiguous 503 client error
// (F5): previously reads returned a plain error that the REST layer mapped
// to HTTP 500.
func TestClientUnavailableReadsMapToServiceUnavailable(t *testing.T) {
	cases := []struct {
		name       string
		cause      error
		underlying error
	}{
		{"network failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, nil},
		{"canceled", context.Canceled, context.Canceled},
		{"deadline", context.DeadlineExceeded, context.DeadlineExceeded},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			c, err := client.New("http://registry.invalid", &http.Client{Transport: &failingTransport{err: testCase.cause}})
			require.NoError(t, err)

			_, err = c.GetRegisteredModel(t.Context(), "m")
			require.Error(t, err)
			typed := requireClientError(t, err)
			assert.Equal(t, http.StatusServiceUnavailable, typed.Status)
			assert.False(t, typed.Ambiguous, "a failed read has no unknown side effect")
			if testCase.underlying != nil {
				assert.ErrorIs(t, err, testCase.underlying, "the underlying cause must stay reachable")
			}
			// The shared error mapping must produce 503, not 500.
			assert.Equal(t, http.StatusServiceUnavailable, api.ErrToStatus(mlflow.MapClientError(err)))
			assert.NotContains(t, err.Error(), "registry.invalid", "transport URLs are not echoed")
		})
	}
}

// TestClientUndecodableReadIsTypedServiceUnavailable proves unreadable or
// undecodable read responses also surface as typed 503 errors rather than
// plain errors that map to 500.
func TestClientUndecodableReadIsTypedServiceUnavailable(t *testing.T) {
	server, _ := newRecordingServer(t, func(recordedRequest) (int, string) {
		return 200, `{"registered_model": {"name":`
	})
	c, err := client.New(server.URL, server.Client())
	require.NoError(t, err)

	_, err = c.GetRegisteredModel(t.Context(), "m")
	require.Error(t, err)
	typed := requireClientError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, typed.Status)
	assert.False(t, typed.Ambiguous)
	assert.Equal(t, http.StatusServiceUnavailable, api.ErrToStatus(mlflow.MapClientError(err)))
}

// TestClientCreateGatewayFailuresRemainAmbiguous proves a create that the
// native side may already have applied, followed by a gateway failure
// (502/503/504 or an unstructured 500), keeps its unknown-outcome marking
// (F4) and is never replayed by the transport.
func TestClientCreateGatewayFailuresRemainAmbiguous(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server, recorded := newRecordingServer(t, func(recordedRequest) (int, string) {
				// The native side has accepted (applied) the create; the
				// gateway then swallows the success response.
				return status, `gateway error`
			})
			c, err := client.New(server.URL, server.Client())
			require.NoError(t, err)

			_, err = c.CreateModelVersion(t.Context(), client.CreateVersionRequest{
				Name: "gateway-model", Source: "s3://bucket/model.onnx",
			})
			require.Error(t, err)
			typed := requireClientError(t, err)
			assert.True(t, typed.Ambiguous, "an applied create followed by a gateway failure has an unknown outcome")
			assert.Equal(t, http.StatusServiceUnavailable, api.ErrToStatus(mlflow.MapClientError(err)))
			require.Len(t, *recorded, 1, "the transport must never replay a create")
		})
	}
}

// TestClientIdempotentMutationServerErrorsAreNotAmbiguous proves safe
// (retryable) mutations receiving a 5xx response stay non-ambiguous: their
// outcome is a definitive failure the service may retry.
func TestClientIdempotentMutationServerErrorsAreNotAmbiguous(t *testing.T) {
	server, _ := newRecordingServer(t, func(recordedRequest) (int, string) {
		return http.StatusInternalServerError, `{"error_code": "INTERNAL_ERROR", "message": "boom"}`
	})
	c, err := client.New(server.URL, server.Client())
	require.NoError(t, err)

	err = c.SetRegisteredModelTag(t.Context(), "m", "rhoai.owner", "alice")
	require.Error(t, err)
	typed := requireClientError(t, err)
	assert.False(t, typed.Ambiguous, "a failed tag write is safely retryable")
}
