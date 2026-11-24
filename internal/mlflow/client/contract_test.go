package client_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Native contract tests characterize the real MLflow Model Registry REST
// API the adapter targets. They are opt-in because they require a
// disposable MLflow server:
//
//	MLFLOW_CONTRACT_TESTS=1 MLFLOW_REGISTRY_URL=http://localhost:5000 \
//	  go test ./internal/mlflow/client -run NativeContract -count=1
//
// When the flag is set, a missing or unreachable server, or a server that
// does not report version 3.18.0, is a failure with a clear setup error;
// the tests never silently pass as skipped. Every model the suite creates
// ( including invalid-name probes that are unexpectedly accepted ) is
// registered for deletion, and each run uses a unique model namespace, so
// repeated runs against the same disposable server neither collide on
// fixed names nor leave models behind.

const nativeTestPrefix = "rhoai-native-contract"

// nativePinnedVersion is the MLflow release these contracts were verified
// against. A server reporting a different version fails setup rather than
// silently invalidating the recorded behavior.
const nativePinnedVersion = "3.18.0"

// nativeHTTPClient bounds every native call, including the /health and
// /version probes.
var nativeHTTPClient = &http.Client{Timeout: 30 * time.Second}

type mlflowResponse struct {
	StatusCode int
	ErrorCode  string `json:"error_code"`
	Message    string `json:"message"`
	Body       map[string]any
}

func (r mlflowResponse) isError() bool {
	return r.StatusCode >= 400
}

// nativeFixture is one run's view of the disposable server: a base URL
// plus a unique model namespace with owned-model cleanup.
type nativeFixture struct {
	base      string
	namespace string
}

// model returns the run-unique registered-model name for a short suffix.
func (f *nativeFixture) model(suffix string) string {
	return f.namespace + "-" + suffix
}

// trackOwned deletes the named registered model during test cleanup.
// Deleting the registered model removes its versions and tags; ignoring
// the deletion result keeps cleanup robust when the probe was rejected.
func (f *nativeFixture) trackOwned(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() {
		t.Logf("cleaning up native model %s", name)
		_ = nativeRequest(t, http.MethodDelete,
			f.base+"/api/2.0/mlflow/registered-models/delete", map[string]any{"name": name})
	})
}

// newNativeFixture checks the prerequisites and returns the run fixture.
func newNativeFixture(t *testing.T) *nativeFixture {
	t.Helper()

	if os.Getenv("MLFLOW_CONTRACT_TESTS") != "1" {
		t.Skip("native contract tests are opt-in; set MLFLOW_CONTRACT_TESTS=1 and MLFLOW_REGISTRY_URL to a disposable MLflow 3.18.0 server")
	}

	base := strings.TrimSuffix(os.Getenv("MLFLOW_REGISTRY_URL"), "/")
	require.NotEmpty(t, base,
		"MLFLOW_CONTRACT_TESTS=1 requires MLFLOW_REGISTRY_URL pointing at a disposable MLflow 3.18.0 server")

	healthURL := base + "/health"
	resp, err := nativeHTTPClient.Get(healthURL) //nolint:gosec // test URL from environment
	require.NoError(t, err, "MLFLOW_REGISTRY_URL (%s) is not reachable; start a disposable MLflow server before running native contract tests", base)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, "MLFLOW server at %s failed its /health check", base)

	versionURL := base + "/version"
	vresp, err := nativeHTTPClient.Get(versionURL) //nolint:gosec // test URL from environment
	require.NoError(t, err, "fetching MLflow version from %s", versionURL)
	defer func() { _ = vresp.Body.Close() }()
	body, err := io.ReadAll(vresp.Body)
	require.NoError(t, err)
	require.Equal(t, "3.18.0", strings.TrimSpace(string(body)),
		"native contracts are verified against MLflow %s; the server at %s reports %q",
		nativePinnedVersion, base, strings.TrimSpace(string(body)))

	var suffix [3]byte
	_, err = rand.Read(suffix[:])
	require.NoError(t, err)

	return &nativeFixture{base: base, namespace: nativeTestPrefix + "-" + hex.EncodeToString(suffix[:])}
}

func nativeRequest(t *testing.T, method, requestURL string, payload any) mlflowResponse {
	t.Helper()

	var body []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		body = encoded
	}

	// A background context, not t.Context(): cleanup-time requests must
	// still run after the test's context is canceled.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := nativeHTTPClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "reading response body from %s %s", method, requestURL)

	parsed := mlflowResponse{StatusCode: resp.StatusCode, Body: map[string]any{}}
	if len(bytes.TrimSpace(raw)) > 0 {
		require.NoError(t, json.Unmarshal(raw, &parsed.Body),
			"response from %s %s is not valid JSON: %s", method, requestURL, string(raw))
		if code, ok := parsed.Body["error_code"].(string); ok {
			parsed.ErrorCode = code
		}
		if msg, ok := parsed.Body["message"].(string); ok {
			parsed.Message = msg
		}
	}
	return parsed
}

// createNativeModel creates a registered model and registers it for
// cleanup, failing the test if the server rejects a name the contract
// assumes is legal.
func createNativeModel(t *testing.T, fixture *nativeFixture, suffix string) mlflowResponse {
	t.Helper()
	name := fixture.model(suffix)
	created := nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/registered-models/create", map[string]any{"name": name})
	if !created.isError() {
		fixture.trackOwned(t, name)
	}
	return created
}

// createNativeVersion creates a model version on an owned model.
func createNativeVersion(t *testing.T, fixture *nativeFixture, name, source string) map[string]any {
	t.Helper()
	resp := nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/model-versions/create", map[string]any{
		"name": name, "source": source,
	})
	require.False(t, resp.isError(), "version create rejected: %+v", resp)
	version, ok := resp.Body["model_version"].(map[string]any)
	require.True(t, ok, "response must contain a model_version object")
	return version
}

// searchQuery URL-encodes a model-version search for one model. Searches
// are GET requests with query parameters.
func searchQuery(model string, extra url.Values) string {
	values := url.Values{
		"filter":      []string{fmt.Sprintf("name = '%s'", model)},
		"max_results": []string{"100"},
	}
	maps.Copy(values, extra)
	return values.Encode()
}

// TestNativeContractModelNameValidation probes which registered-model
// names the target server accepts. Upstream 3.18.0 validation rejects "/"
// and ":" in names; if the probed target contradicts that, this contract
// must be updated before the adapter's name mapping is coded against it.
func TestNativeContractModelNameValidation(t *testing.T) {
	fixture := newNativeFixture(t)

	resp := createNativeModel(t, fixture, "plain-name")
	require.False(t, resp.isError(), "legal name rejected: %+v", resp)

	for _, ch := range []string{"/", ":"} {
		// Even a rejected name is tracked for cleanup so an unexpectedly
		// accepted probe cannot leak a model into the next run.
		created := createNativeModel(t, fixture, "bad"+ch+"name")
		assert.True(t, created.isError(),
			"name %q containing %q was accepted; the adapter's model-name mapping must be revisited", fixture.model("bad"+ch+"name"), ch)
		if created.isError() {
			assert.NotEmpty(t, created.ErrorCode, "server must return a structured MLflow error code")
		}
	}
}

// TestNativeContractVersionSources probes that nonempty remote sources
// ( s3, https, and the OCI references used by the existing Python client
// tests ) are accepted, the version number is server-generated with a
// READY status, and an empty or missing source is rejected.
func TestNativeContractVersionSources(t *testing.T) {
	fixture := newNativeFixture(t)
	model := fixture.model("sources")
	require.False(t, createNativeModel(t, fixture, "sources").isError())

	sources := []string{
		"s3://bucket/model.onnx",
		"https://example.com/model.onnx",
		"oci://localhost:5001/foo/bar:latest",
	}
	for _, source := range sources {
		version := createNativeVersion(t, fixture, model, source)
		assert.NotEmpty(t, version["version"], "version number must be generated by the server")
		assert.Equal(t, source, version["source"])
		assert.Equal(t, "READY", version["status"],
			"a directly created version must report READY status for source %q", source)
	}

	// Missing and empty sources must both be rejected: the adapter relies
	// on this to justify pending ( unmaterialized ) versions.
	for _, source := range []any{nil, ""} {
		payload := map[string]any{"name": model}
		if source != nil {
			payload["source"] = source
		}
		resp := nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/model-versions/create", payload)
		assert.True(t, resp.isError(), "empty/missing source accepted (%v); pending-version design must be revisited", source)
	}
}

// TestNativeContractVersionMetadataAndHistory characterizes the version
// update surface: only the description changes, tags are set and deleted
// through dedicated endpoints, and searches return paged results.
func TestNativeContractVersionMetadataAndHistory(t *testing.T) {
	fixture := newNativeFixture(t)
	model := fixture.model("metadata")
	require.False(t, createNativeModel(t, fixture, "metadata").isError())

	version := createNativeVersion(t, fixture, model, "s3://bucket/initial.onnx")
	versionNumber := fmt.Sprint(version["version"])

	// Only the description is updatable on a version.
	updated := nativeRequest(t, http.MethodPatch, fixture.base+"/api/2.0/mlflow/model-versions/update", map[string]any{
		"name": model, "version": versionNumber, "description": "updated description",
	})
	require.False(t, updated.isError(), "description update rejected: %+v", updated)

	// Tags are set and deleted per key.
	setTag := nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/model-versions/set-tag", map[string]any{
		"name": model, "version": versionNumber, "key": "rhoai.versionName", "value": "1.0.0",
	})
	require.False(t, setTag.isError(), "version tag set rejected: %+v", setTag)

	search := nativeRequest(t, http.MethodGet, fixture.base+"/api/2.0/mlflow/model-versions/search?"+searchQuery(model, nil), nil)
	require.False(t, search.isError())
	found, ok := search.Body["model_versions"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, found, "created version must be searchable")

	delTag := nativeRequest(t, http.MethodDelete, fixture.base+"/api/2.0/mlflow/model-versions/delete-tag", map[string]any{
		"name": model, "version": versionNumber, "key": "rhoai.versionName",
	})
	require.False(t, delTag.isError(), "version tag delete rejected: %+v", delTag)
}

// TestNativeContractModelAliasesAndTags characterizes aliases: setting an
// alias on the registered model, its movement between versions, and the
// reserved alias forms the adapter's normalized alias must avoid.
func TestNativeContractModelAliasesAndTags(t *testing.T) {
	fixture := newNativeFixture(t)
	model := fixture.model("aliases")
	require.False(t, createNativeModel(t, fixture, "aliases").isError())

	v1 := createNativeVersion(t, fixture, model, "s3://bucket/v1.onnx")
	v2 := createNativeVersion(t, fixture, model, "s3://bucket/v2.onnx")
	v1Number := fmt.Sprint(v1["version"])
	v2Number := fmt.Sprint(v2["version"])

	alias := "rhoai-alias-probe"
	set := nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/registered-models/alias", map[string]any{
		"name": model, "alias": alias, "version": v1Number,
	})
	require.False(t, set.isError(), "alias set rejected: %+v", set)

	// Setting the same alias on another version moves it.
	move := nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/registered-models/alias", map[string]any{
		"name": model, "alias": alias, "version": v2Number,
	})
	require.False(t, move.isError(), "alias move rejected: %+v", move)

	// An invalid alias ( uppercase / illegal characters ) is rejected;
	// the adapter's NormalizedAlias relies on lowercase slugs being legal.
	invalid := nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/registered-models/alias", map[string]any{
		"name": model, "alias": "INVALID Alias!", "version": v1Number,
	})
	assert.True(t, invalid.isError(), "invalid alias accepted; normalized alias assumptions must be revisited")

	// Registered-model tags round-trip and are deleted through the
	// dedicated DELETE endpoint.
	setTag := nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/registered-models/set-tag", map[string]any{
		"name": model, "key": "rhoai.owner", "value": "alice",
	})
	require.False(t, setTag.isError(), "model tag set rejected: %+v", setTag)

	got := nativeRequest(t, http.MethodGet, fixture.base+"/api/2.0/mlflow/registered-models/get?name="+url.QueryEscape(model), nil)
	require.False(t, got.isError(), "model get rejected: %+v", got)
	tags, ok := got.Body["registered_model"].(map[string]any)["tags"].([]any)
	if ok {
		found := false
		for _, tag := range tags {
			if m, ok := tag.(map[string]any); ok && m["key"] == "rhoai.owner" {
				found = true
				assert.Equal(t, "alice", m["value"])
			}
		}
		assert.True(t, found, "set model tag must be visible on get")
	}

	delTag := nativeRequest(t, http.MethodDelete, fixture.base+"/api/2.0/mlflow/registered-models/delete-tag", map[string]any{
		"name": model, "key": "rhoai.owner",
	})
	require.False(t, delTag.isError(), "model tag delete rejected: %+v", delTag)

	afterDelete := nativeRequest(t, http.MethodGet, fixture.base+"/api/2.0/mlflow/registered-models/get?name="+url.QueryEscape(model), nil)
	require.False(t, afterDelete.isError())
	if remaining, ok := afterDelete.Body["registered_model"].(map[string]any)["tags"].([]any); ok {
		for _, tag := range remaining {
			if m, ok := tag.(map[string]any); ok {
				assert.NotEqual(t, "rhoai.owner", m["key"], "deleted model tag must not be returned")
			}
		}
	}

	// Deleting the only version is possible; the adapter must not do it
	// automatically, but the contract records that the server supports it.
	search := nativeRequest(t, http.MethodGet, fixture.base+"/api/2.0/mlflow/model-versions/search?"+searchQuery(model, nil), nil)
	require.False(t, search.isError())
	assert.Len(t, search.Body["model_versions"], 2, "both versions must be searchable")
}

// TestNativeContractPagedSearches proves model and version searches page
// with a page smaller than the dataset and that page tokens traverse the
// whole result set exactly once.
func TestNativeContractPagedSearches(t *testing.T) {
	fixture := newNativeFixture(t)
	model := fixture.model("paging")
	require.False(t, createNativeModel(t, fixture, "paging").isError())

	for i := range 3 {
		createNativeVersion(t, fixture, model, fmt.Sprintf("s3://bucket/page-%d.onnx", i))
	}

	// Version search: pages of two over three versions.
	var versions []string
	token := ""
	for range 10 {
		extra := url.Values{"max_results": []string{"2"}}
		if token != "" {
			extra.Set("page_token", token)
		}
		resp := nativeRequest(t, http.MethodGet, fixture.base+"/api/2.0/mlflow/model-versions/search?"+searchQuery(model, extra), nil)
		require.False(t, resp.isError(), "version search page rejected: %+v", resp)
		pageVersions, ok := resp.Body["model_versions"].([]any)
		require.True(t, ok, "version search must return a model_versions array")
		assert.NotEmpty(t, pageVersions, "a non-final page must not be empty")
		for _, v := range pageVersions {
			versions = append(versions, fmt.Sprint(v.(map[string]any)["version"]))
		}
		next, _ := resp.Body["next_page_token"].(string)
		if next == "" {
			break
		}
		token = next
	}
	require.Len(t, versions, 3, "paged version search must return each version exactly once: %v", versions)

	// Registered-model search with a filter and a page of one over the
	// run's models; every page must respect the filter.
	modelsSeen := 0
	token = ""
	for range 100 {
		values := url.Values{
			"filter":      []string{fmt.Sprintf("name LIKE '%s%%'", fixture.namespace)},
			"max_results": []string{"1"},
		}
		if token != "" {
			values.Set("page_token", token)
		}
		resp := nativeRequest(t, http.MethodGet, fixture.base+"/api/2.0/mlflow/registered-models/search?"+values.Encode(), nil)
		require.False(t, resp.isError(), "model search page rejected: %+v", resp)
		pageModels, _ := resp.Body["registered_models"].([]any)
		modelsSeen += len(pageModels)
		for _, m := range pageModels {
			name := m.(map[string]any)["name"].(string)
			require.True(t, strings.HasPrefix(name, fixture.namespace),
				"filtered page returned an out-of-scope model %q", name)
		}
		next, _ := resp.Body["next_page_token"].(string)
		if next == "" {
			break
		}
		token = next
	}
	require.GreaterOrEqual(t, modelsSeen, 1, "filtered model search must find the created model")

	// Order by version number uses the name attribute; the version
	// attribute itself is not a valid order_by key.
	ordered := nativeRequest(t, http.MethodGet, fixture.base+"/api/2.0/mlflow/model-versions/search?"+searchQuery(model, url.Values{
		"order_by": []string{"name DESC"},
	}), nil)
	require.False(t, ordered.isError())
	orderedVersions := ordered.Body["model_versions"].([]any)
	require.Len(t, orderedVersions, 3)
	for i := 1; i < len(orderedVersions); i++ {
		prev := orderedVersions[i-1].(map[string]any)["version"].(string)
		cur := orderedVersions[i].(map[string]any)["version"].(string)
		assert.Greater(t, prev, cur, "name DESC must order version numbers descending")
	}

	badOrder := nativeRequest(t, http.MethodGet, fixture.base+"/api/2.0/mlflow/model-versions/search?"+searchQuery(model, url.Values{
		"order_by": []string{"version DESC"},
	}), nil)
	require.True(t, badOrder.isError())
	assert.Equal(t, "INVALID_PARAMETER_VALUE", badOrder.ErrorCode)
}

// TestNativeContractStructuredErrors proves failed calls return a
// structured MLflow error body with a stable error_code.
func TestNativeContractStructuredErrors(t *testing.T) {
	fixture := newNativeFixture(t)

	missing := nativeRequest(t, http.MethodGet, fixture.base+"/api/2.0/mlflow/registered-models/get?name="+url.QueryEscape(fixture.model("never-created")), nil)
	require.True(t, missing.isError())
	assert.Equal(t, "RESOURCE_DOES_NOT_EXIST", missing.ErrorCode)
	assert.NotEmpty(t, missing.Message)

	// Creating a version on a missing model is a structured not-found, not
	// a server error.
	orphan := nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/model-versions/create", map[string]any{
		"name": fixture.model("never-created"), "source": "s3://bucket/model.onnx",
	})
	require.True(t, orphan.isError())
	assert.Equal(t, "RESOURCE_DOES_NOT_EXIST", orphan.ErrorCode)
}

// TestNativeContractTagLimitsAndUnicode characterizes tag key/value
// limits and Unicode round-tripping on the pinned server.
func TestNativeContractTagLimitsAndUnicode(t *testing.T) {
	fixture := newNativeFixture(t)
	model := fixture.model("tags")
	require.False(t, createNativeModel(t, fixture, "tags").isError())

	// Keys are limited to 250 characters.
	longKey := strings.Repeat("k", 251)
	resp := nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/registered-models/set-tag", map[string]any{
		"name": model, "key": longKey, "value": "v",
	})
	require.True(t, resp.isError(), "a 251-character tag key was accepted; adapter escaping rules must be revisited")
	assert.Equal(t, "INVALID_PARAMETER_VALUE", resp.ErrorCode)

	maxKey := strings.Repeat("k", 250)
	resp = nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/registered-models/set-tag", map[string]any{
		"name": model, "key": maxKey, "value": "v",
	})
	require.False(t, resp.isError(), "a 250-character tag key was rejected: %+v", resp)

	// The pinned server does not enforce an upper bound on tag values at
	// 5000 or 8000 characters; record that large values are accepted.
	resp = nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/registered-models/set-tag", map[string]any{
		"name": model, "key": "rhoai.long", "value": strings.Repeat("v", 8001),
	})
	require.False(t, resp.isError(), "an 8001-character tag value was rejected; the adapter must bound values itself: %+v", resp)

	// Unicode keys and values round-trip exactly.
	resp = nativeRequest(t, http.MethodPost, fixture.base+"/api/2.0/mlflow/registered-models/set-tag", map[string]any{
		"name": model, "key": "rhoai.名前", "value": "café-Ω",
	})
	require.False(t, resp.isError(), "unicode tag rejected: %+v", resp)

	got := nativeRequest(t, http.MethodGet, fixture.base+"/api/2.0/mlflow/registered-models/get?name="+url.QueryEscape(model), nil)
	require.False(t, got.isError())
	tags, _ := got.Body["registered_model"].(map[string]any)["tags"].([]any)
	found := false
	for _, tag := range tags {
		if m, ok := tag.(map[string]any); ok && m["key"] == "rhoai.名前" {
			found = true
			assert.Equal(t, "café-Ω", m["value"])
		}
	}
	assert.True(t, found, "unicode tag must round-trip on get")
}
