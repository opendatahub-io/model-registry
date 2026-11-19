package mlflow_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	platformproxy "github.com/kubeflow/hub/internal/platform/proxy"
	"github.com/kubeflow/hub/internal/proxy"
	"github.com/kubeflow/hub/internal/server/middleware"
	v1 "github.com/kubeflow/hub/internal/server/openapi/v1"
	"github.com/kubeflow/hub/internal/server/openapi/v1alpha3"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRegistryHTTPServer builds a real proxy-shaped server over one
// backend fixture: both generated REST controllers behind the
// production validation middleware, and the actual readiness dispatch
// for the health endpoints.
func newRegistryHTTPServer(t *testing.T, backend *backendFixture) *httptest.Server {
	t.Helper()

	v1alpha3Controller := v1alpha3.NewModelRegistryServiceAPIController(
		v1alpha3.NewModelRegistryServiceAPIService(backend.service))
	v1Controller := v1.NewModelRegistryServiceAPIController(
		v1.NewModelRegistryServiceAPIService(backend.service))

	mux := chi.NewRouter()
	mux.Use(chimiddleware.Logger)
	for _, route := range v1alpha3Controller.OrderedRoutes() {
		mux.Method(route.Method, route.Pattern, route.HandlerFunc)
	}
	for _, route := range v1Controller.OrderedRoutes() {
		mux.Method(route.Method, route.Pattern, route.HandlerFunc)
	}

	readiness := platformproxy.GeneralReadinessHandler(
		proxy.NewModelRegistryHealthChecker(backend.service))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/readyz/isDirty") ||
			strings.HasSuffix(r.URL.Path, "/readyz/health") {
			readiness.ServeHTTP(w, r)
			return
		}
		middleware.WrapWithValidation(nil, mux).ServeHTTP(w, r)
	})

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func doRequest(t *testing.T, method, url string, body any) (int, string) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, payload)
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(raw)
}

// TestRealControllerHTTPContract drives both generated REST versions
// and the readiness dispatch against a fully started MLflow backend:
// registration, numeric IDs, PATCH semantics, protected fields,
// pagination tokens, 404/409, every unsupported family 501, and 503
// for network reads and uncertain creates — with sanitized bodies.
func TestRealControllerHTTPContract(t *testing.T) {
	const secret = "controller-synthetic-secret"
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, version := range []string{"v1", "v1alpha3"} {
				t.Run(version, func(t *testing.T) {
					prefix := "/api/model_registry/" + version
					backend := newBackendFixture(t, fixture)
					var failure atomic.Int32
					backend.service = aliasTestService(t, backend, aliasTransport(func(r *http.Request) (*http.Response, error) {
						mode := failure.Load()
						if (mode == 1 && strings.HasSuffix(r.URL.Path, "/registered-models/search")) ||
							(mode == 2 && strings.HasSuffix(r.URL.Path, "/registered-models/create")) {
							return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Request: r,
								Body: io.NopCloser(strings.NewReader(`{"error_code":"TEMPORARILY_UNAVAILABLE","message":"Authorization: Bearer ` + secret + ` https://user:` + secret + `@registry.example?token=` + secret + `"}`))}, nil
						}
						return http.DefaultTransport.RoundTrip(r)
					}))
					server := newRegistryHTTPServer(t, backend)

					// The readiness endpoints fail before startup completes.
					status, body := doRequest(t, http.MethodGet, server.URL+"/readyz/health", nil)
					assert.Equal(t, http.StatusServiceUnavailable, status)
					assert.NotContains(t, body, backend.stub.url, "the failure body must be sanitized")
					status, _ = doRequest(t, http.MethodGet, server.URL+"/readyz/isDirty", nil)
					assert.Equal(t, http.StatusServiceUnavailable, status)
					stopLoop := startHealthyBackend(t, backend)
					status, _ = doRequest(t, http.MethodGet, server.URL+"/readyz/health", nil)
					assert.Equal(t, http.StatusOK, status)
					status, _ = doRequest(t, http.MethodGet, server.URL+"/readyz/isDirty", nil)
					assert.Equal(t, http.StatusOK, status)

					stopLoop()
					for _, path := range []string{"/readyz/health", "/readyz/isDirty"} {
						status, body = doRequest(t, http.MethodGet, server.URL+path, nil)
						require.Equal(t, http.StatusServiceUnavailable, status, body)
						require.NotEmpty(t, body)
					}
					status, body = doRequest(t, http.MethodGet, server.URL+prefix+"/registered_models", nil)
					require.Equal(t, http.StatusOK, status, body)
					var empty openapi.RegisteredModelList
					require.NoError(t, json.Unmarshal([]byte(body), &empty))
					require.Zero(t, empty.Size)

					// Registration returns a numeric id.
					status, body = doRequest(t, http.MethodPost, server.URL+prefix+"/registered_models",
						map[string]any{"name": "http-model", "owner": "team-a"})
					require.Equal(t, http.StatusCreated, status, body)
					var created openapi.RegisteredModel
					require.NoError(t, json.Unmarshal([]byte(body), &created))
					require.NotNil(t, created.Id)
					numericID(t, *created.Id)
					assert.Equal(t, "team-a", *created.Owner)

					// Direct read by id.
					status, body = doRequest(t, http.MethodGet,
						server.URL+prefix+"/registered_models/"+*created.Id, nil)
					require.Equal(t, http.StatusOK, status, body)

					// PATCH is a partial update: an omitted field keeps its prior
					// value and the protected name is unchanged.
					status, body = doRequest(t, http.MethodPatch,
						server.URL+prefix+"/registered_models/"+*created.Id,
						map[string]any{"description": "edited"})
					require.Equal(t, http.StatusOK, status, body)
					var patched openapi.RegisteredModel
					require.NoError(t, json.Unmarshal([]byte(body), &patched))
					assert.Equal(t, "edited", *patched.Description)
					assert.Equal(t, "team-a", *patched.Owner, "an omitted field keeps its prior value")
					assert.Equal(t, "http-model", patched.Name, "the name is protected")

					// Register two versions and their independent primaries entirely
					// through the generated handlers, including logical-name encoding.
					var versionIDs []string
					var artifactIDs []string
					for _, name := range []string{"release/2026", "release:2026"} {
						status, body = doRequest(t, http.MethodPost, server.URL+prefix+"/model_versions",
							map[string]any{"name": name, "registeredModelId": *created.Id, "description": "version description", "author": "alice"})
						require.Equal(t, http.StatusCreated, status, body)
						var version openapi.ModelVersion
						require.NoError(t, json.Unmarshal([]byte(body), &version))
						require.NotNil(t, version.Id)
						numericID(t, *version.Id)
						versionIDs = append(versionIDs, *version.Id)
						status, body = doRequest(t, http.MethodPost, server.URL+prefix+"/model_versions/"+*version.Id+"/artifacts",
							map[string]any{"artifactType": "model-artifact", "name": "http-model", "uri": "s3://bucket/" + url.PathEscape(name) + ".onnx", "modelFormatName": "onnx"})
						require.Equal(t, http.StatusCreated, status, body)
						var artifact openapi.Artifact
						require.NoError(t, json.Unmarshal([]byte(body), &artifact))
						require.NotNil(t, artifact.ModelArtifact)
						require.NotNil(t, artifact.ModelArtifact.Id)
						numericID(t, *artifact.ModelArtifact.Id)
						artifactIDs = append(artifactIDs, *artifact.ModelArtifact.Id)
						status, body = doRequest(t, http.MethodGet, server.URL+prefix+"/model_artifact?name=http-model&parentResourceId="+*version.Id, nil)
						require.Equal(t, http.StatusOK, status, body)
						var found openapi.ModelArtifact
						require.NoError(t, json.Unmarshal([]byte(body), &found))
						require.Equal(t, artifact.ModelArtifact.Id, found.Id)
					}
					require.NotEqual(t, artifactIDs[0], artifactIDs[1])
					status, body = doRequest(t, http.MethodPatch, server.URL+prefix+"/model_versions/"+versionIDs[0],
						map[string]any{"description": "version edited", "customProperties": map[string]any{"score": map[string]any{"metadataType": "MetadataDoubleValue", "double_value": 1.0}, "rank": map[string]any{"metadataType": "MetadataIntValue", "int_value": "9007199254740993"}}})
					require.Equal(t, http.StatusOK, status, body)
					var editedVersion openapi.ModelVersion
					require.NoError(t, json.Unmarshal([]byte(body), &editedVersion))
					require.Equal(t, "version edited", *editedVersion.Description)
					require.Equal(t, "alice", *editedVersion.Author)
					for query, count := range map[string]int{"id = " + versionIDs[0]: 1, "registeredModelId = " + *created.Id: 2, "score.double_value = 1.0": 1, "score.int_value = 1": 0, "rank = 9007199254740992": 0, "rank = 9007199254740993": 1} {
						status, body = doRequest(t, http.MethodGet, server.URL+prefix+"/model_versions?filterQuery="+url.QueryEscape(query), nil)
						require.Equal(t, http.StatusOK, status, body)
						var versions openapi.ModelVersionList
						require.NoError(t, json.Unmarshal([]byte(body), &versions))
						require.Len(t, versions.Items, count, query)
					}
					// A standalone artifact PATCH exercises handler read-before-update.
					status, body = doRequest(t, http.MethodPost, server.URL+prefix+"/model_artifacts", map[string]any{"name": "standalone", "uri": "s3://bucket/standalone.onnx", "modelFormatName": "onnx"})
					require.Equal(t, http.StatusCreated, status, body)
					var standalone openapi.ModelArtifact
					require.NoError(t, json.Unmarshal([]byte(body), &standalone))
					require.NotNil(t, standalone.Id)
					numericID(t, *standalone.Id)
					status, body = doRequest(t, http.MethodPatch, server.URL+prefix+"/model_artifacts/"+*standalone.Id, map[string]any{"description": "standalone edited"})
					require.Equal(t, http.StatusOK, status, body)
					var editedArtifact openapi.ModelArtifact
					require.NoError(t, json.Unmarshal([]byte(body), &editedArtifact))
					require.Equal(t, standalone.Id, editedArtifact.Id)
					require.Equal(t, standalone.Uri, editedArtifact.Uri)
					require.Equal(t, "onnx", *editedArtifact.ModelFormatName)
					require.Equal(t, "standalone edited", *editedArtifact.Description)
					for path, count := range map[string]int{"/model_artifacts": 3, "/artifacts": 3, "/model_versions/" + versionIDs[0] + "/artifacts": 1} {
						status, body = doRequest(t, http.MethodGet, server.URL+prefix+path, nil)
						require.Equal(t, http.StatusOK, status, body)
						var list struct {
							Items []json.RawMessage `json:"items"`
						}
						require.NoError(t, json.Unmarshal([]byte(body), &list))
						require.Len(t, list.Items, count, path)
					}
					for path, count := range map[string]int{"/model_versions": 2, "/model_artifacts": 3, "/artifacts": 3} {
						assertHTTPPages(t, server.URL+prefix+path, count)
					}

					// Pagination tokens walk pages without repeats.
					status, body = doRequest(t, http.MethodPost, server.URL+prefix+"/registered_models",
						map[string]any{"name": "http-model-2"})
					require.Equal(t, http.StatusCreated, status, body)

					status, body = doRequest(t, http.MethodGet,
						server.URL+prefix+"/registered_models?pageSize=1", nil)
					require.Equal(t, http.StatusOK, status, body)
					var page openapi.RegisteredModelList
					require.NoError(t, json.Unmarshal([]byte(body), &page))
					require.Equal(t, int32(1), page.Size)
					require.NotNil(t, page.NextPageToken)
					firstID := *page.Items[0].Id

					status, body = doRequest(t, http.MethodGet,
						server.URL+prefix+"/registered_models?pageSize=1&nextPageToken="+page.NextPageToken, nil)
					require.Equal(t, http.StatusOK, status, body)
					var next openapi.RegisteredModelList
					require.NoError(t, json.Unmarshal([]byte(body), &next))
					require.Equal(t, int32(1), next.Size)
					assert.NotEqual(t, firstID, *next.Items[0].Id, "the second page must not repeat the first row")
					require.Empty(t, next.NextPageToken, "the last page has no cursor")
					status, body = doRequest(t, http.MethodGet, server.URL+prefix+"/registered_models?nextPageToken=invalid", nil)
					require.Equal(t, http.StatusBadRequest, status, body)
					require.NotEmpty(t, body)

					// 404 for an unknown id, 409 for a duplicate create.
					status, body = doRequest(t, http.MethodGet,
						server.URL+prefix+"/registered_models/999999999", nil)
					assert.Equal(t, http.StatusNotFound, status)
					require.NotEmpty(t, body)
					require.NotContains(t, body, secret)
					status, body = doRequest(t, http.MethodPost, server.URL+prefix+"/registered_models",
						map[string]any{"name": "http-model"})
					assert.Equal(t, http.StatusConflict, status)
					require.NotEmpty(t, body)
					require.NotContains(t, body, secret)

					// Unsupported families are checked through this controller.
					unsupportedPaths := []string{
						prefix + "/serving_environments", prefix + "/inference_services",
					}
					if version == "v1alpha3" {
						unsupportedPaths = append(unsupportedPaths, prefix+"/experiments")
					}
					for _, path := range unsupportedPaths {
						status, body = doRequest(t, http.MethodGet, server.URL+path, nil)
						assert.Equal(t, http.StatusNotImplemented, status, "%s: %s", path, body)
						require.NotEmpty(t, body)
						require.NotContains(t, body, secret)
					}

					// A failed native read surfaces as 503 with a sanitized body.
					failure.Store(1)
					status, body = doRequest(t, http.MethodGet, server.URL+prefix+"/registered_models", nil)
					assert.Equal(t, http.StatusServiceUnavailable, status, body)
					require.NotEmpty(t, body)
					assert.NotContains(t, body, backend.stub.url, "the failure body must be sanitized")
					require.NotContains(t, body, secret)
					failure.Store(0)

					// An uncertain native create surfaces as 503 with a sanitized
					// body and leaves no committed row.
					failure.Store(2)
					status, body = doRequest(t, http.MethodPost, server.URL+prefix+"/registered_models",
						map[string]any{"name": "uncertain-model"})
					assert.Equal(t, http.StatusServiceUnavailable, status, body)
					require.NotEmpty(t, body)
					assert.NotContains(t, body, backend.stub.url, "the failure body must be sanitized")
					require.NotContains(t, body, secret)
					failure.Store(0)
					_, err := backend.service.GetRegisteredModelByParams(new("uncertain-model"), nil)
					assert.Error(t, err, "an uncertain create must not publish a resource")
				})
			}
		})
	}
}

func assertHTTPPages(t *testing.T, endpoint string, count int) {
	t.Helper()
	seen := map[string]bool{}
	token := ""
	for range count {
		status, body := doRequest(t, http.MethodGet, endpoint+"?pageSize=1&nextPageToken="+url.QueryEscape(token), nil)
		require.Equal(t, http.StatusOK, status, body)
		var page struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &page))
		require.Len(t, page.Items, 1)
		require.False(t, seen[page.Items[0].ID], "pagination repeated an ID")
		seen[page.Items[0].ID] = true
		token = page.NextPageToken
		if token == "" {
			break
		}
	}
	require.Len(t, seen, count)
	require.Empty(t, token, "pagination must terminate with an empty cursor")
}

// numericID asserts an id is a positive decimal string.
func numericID(t *testing.T, id string) {
	t.Helper()
	parsed, err := parseID(id)
	require.NoError(t, err)
	require.Positive(t, parsed)
	require.LessOrEqual(t, parsed, int32(999999999))
}
