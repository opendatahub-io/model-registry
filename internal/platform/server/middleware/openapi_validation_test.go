package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// passHandler returns 200 OK for every request that reaches it.
func passHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func sampleRoutes() []RouteDefinition {
	return []RouteDefinition{
		{Method: "GET", Pattern: "/api/v1/items"},
		{Method: "POST", Pattern: "/api/v1/items"},
		{Method: "GET", Pattern: "/api/v1/items/{id}"},
		{Method: "PATCH", Pattern: "/api/v1/items/{id}"},
		{Method: "GET", Pattern: "/api/v1/wildcard/*"},
	}
}

// --- 405 Method Not Allowed ---

func TestMethodNotAllowed(t *testing.T) {
	handler := OpenAPIValidationMiddleware(sampleRoutes(), passHandler())

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"PUT to GET/POST route", "PUT", "/api/v1/items"},
		{"DELETE to GET/POST route", "DELETE", "/api/v1/items"},
		{"POST to GET/PATCH route", "POST", "/api/v1/items/123"},
		{"PUT to GET/PATCH route", "PUT", "/api/v1/items/123"},
		{"DELETE to GET/PATCH route", "DELETE", "/api/v1/items/123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
			assert.NotEmpty(t, rec.Header().Get("Allow"), "Allow header must be set")
			assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")

			var body map[string]string
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
			assert.Equal(t, "Method Not Allowed", body["code"])
			assert.Contains(t, body["message"], tt.method)
		})
	}
}

func TestMethodNotAllowed_AllowHeader(t *testing.T) {
	handler := OpenAPIValidationMiddleware(sampleRoutes(), passHandler())

	req := httptest.NewRequest("DELETE", "/api/v1/items", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	allow := rec.Header().Get("Allow")
	assert.Contains(t, allow, "GET")
	assert.Contains(t, allow, "POST")
}

// --- Allowed methods pass through ---

func TestAllowedMethodPassesThrough(t *testing.T) {
	handler := OpenAPIValidationMiddleware(sampleRoutes(), passHandler())

	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		contentType string
	}{
		{"GET items list", "GET", "/api/v1/items", "", ""},
		{"POST items", "POST", "/api/v1/items", `{"name":"new"}`, "application/json"},
		{"GET single item", "GET", "/api/v1/items/42", "", ""},
		{"PATCH single item", "PATCH", "/api/v1/items/42", `{"name":"upd"}`, "application/merge-patch+json"},
		{"GET wildcard", "GET", "/api/v1/wildcard/anything/here", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			if tt.body != "" {
				req = httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
				req.Header.Set("Content-Type", tt.contentType)
			} else {
				req = httptest.NewRequest(tt.method, tt.path, nil)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

// --- OPTIONS always passes through (for CORS) ---

func TestOptionsPassesThrough(t *testing.T) {
	handler := OpenAPIValidationMiddleware(sampleRoutes(), passHandler())

	req := httptest.NewRequest("OPTIONS", "/api/v1/items", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- Unknown paths pass through (downstream returns 404) ---

func TestUnknownPathPassesThrough(t *testing.T) {
	handler := OpenAPIValidationMiddleware(sampleRoutes(), passHandler())

	req := httptest.NewRequest("DELETE", "/api/v1/unknown", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// No matching pattern -> pass through to next handler.
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- Content-Type validation ---

func TestMissingContentType(t *testing.T) {
	handler := OpenAPIValidationMiddleware(sampleRoutes(), passHandler())

	body := strings.NewReader(`{"name":"test"}`)
	req := httptest.NewRequest("POST", "/api/v1/items", body)
	// Deliberately omit Content-Type.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Content-Type")
}

func TestUnsupportedContentType(t *testing.T) {
	handler := OpenAPIValidationMiddleware(sampleRoutes(), passHandler())

	body := strings.NewReader(`name=test`)
	req := httptest.NewRequest("POST", "/api/v1/items", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
}

func TestValidContentTypes(t *testing.T) {
	handler := OpenAPIValidationMiddleware(sampleRoutes(), passHandler())

	types := []string{
		"application/json",
		"application/json; charset=utf-8",
		"application/merge-patch+json",
		"Application/JSON",
	}

	for _, ct := range types {
		t.Run(ct, func(t *testing.T) {
			body := strings.NewReader(`{"name":"test"}`)
			req := httptest.NewRequest("POST", "/api/v1/items", body)
			req.Header.Set("Content-Type", ct)

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

func TestNoBodySkipsContentTypeCheck(t *testing.T) {
	handler := OpenAPIValidationMiddleware(sampleRoutes(), passHandler())

	// POST with no body should not require Content-Type.
	req := httptest.NewRequest("POST", "/api/v1/items", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestGetSkipsContentTypeCheck(t *testing.T) {
	handler := OpenAPIValidationMiddleware(sampleRoutes(), passHandler())

	// GET request never needs Content-Type even if body is present.
	req := httptest.NewRequest("GET", "/api/v1/items", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- Pattern-to-regex conversion ---

func TestPatternToRegex(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		match   bool
	}{
		{"/api/v1/items", "/api/v1/items", true},
		{"/api/v1/items", "/api/v1/items/", false},
		{"/api/v1/items", "/api/v1/other", false},
		{"/api/v1/items/{id}", "/api/v1/items/123", true},
		{"/api/v1/items/{id}", "/api/v1/items/abc-def", true},
		{"/api/v1/items/{id}", "/api/v1/items/", false},
		{"/api/v1/items/{id}", "/api/v1/items", false},
		{"/api/v1/sources/{sid}/models/*", "/api/v1/sources/s1/models/foo", true},
		{"/api/v1/sources/{sid}/models/*", "/api/v1/sources/s1/models/foo/bar", true},
		{"/api/v1/{a}/items/{b}", "/api/v1/x/items/y", true},
		{"/api/v1/{a}/items/{b}", "/api/v1/x/items/", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"->"+tt.path, func(t *testing.T) {
			re := patternToRegex(tt.pattern)
			got := re.MatchString(tt.path)
			assert.Equal(t, tt.match, got,
				"pattern=%q path=%q regex=%s", tt.pattern, tt.path, re.String())
		})
	}
}

// --- Empty / nil routes ---

func TestEmptyRoutesPassThrough(t *testing.T) {
	handler := OpenAPIValidationMiddleware(nil, passHandler())

	req := httptest.NewRequest("GET", "/anything", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}
