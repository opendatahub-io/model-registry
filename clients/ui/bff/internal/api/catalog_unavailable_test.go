package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kubeflow/hub/ui/bff/internal/integrations/httpclient"
	"github.com/stretchr/testify/require"
)

func TestCatalogUnavailableSurvivesRepositoryWrapping(t *testing.T) {
	upstream := &httpclient.HTTPError{StatusCode: 503, ErrorResponse: httpclient.ErrorResponse{Code: "CatalogUnavailable", Message: "Ask an administrator to correct the selected images or retry activation.", Stage: "Loading", Reason: "BenchmarkContentInvalid", AttemptID: "current"}}
	app := &App{}
	res := httptest.NewRecorder()
	app.serverErrorResponse(res, httptest.NewRequest(http.MethodGet, "/api/v1/model_catalog/models", nil), fmt.Errorf("fetching catalog: %w", upstream))
	require.Equal(t, 503, res.Code)
	require.Equal(t, "no-store", res.Header().Get("Cache-Control"))
	require.JSONEq(t, `{"error":{"code":"CatalogUnavailable","message":"Ask an administrator to correct the selected images or retry activation.","stage":"Loading","reason":"BenchmarkContentInvalid","attemptID":"current"}}`, res.Body.String())
}

func TestCatalogUnavailableWithoutReadyEndpoints(t *testing.T) {
	app := &App{}
	res := httptest.NewRecorder()
	upstream := &httpclient.HTTPError{StatusCode: 503, ErrorResponse: httpclient.ErrorResponse{Code: "503", Message: "no endpoints"}}
	app.serverErrorResponse(res, httptest.NewRequest(http.MethodGet, "/api/v1/mcp_catalog/servers", nil), upstream)
	require.Equal(t, 503, res.Code)
	require.Contains(t, res.Body.String(), "CatalogUnavailable")
	require.Contains(t, res.Body.String(), "Ask an administrator")
	require.Equal(t, "no-store", res.Header().Get("Cache-Control"))
}
