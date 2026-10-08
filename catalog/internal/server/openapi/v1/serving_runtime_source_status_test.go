package v1

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog"
	"github.com/kubeflow/hub/catalog/internal/db/models"
	model "github.com/kubeflow/hub/catalog/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const servingRuntimeSourcesRoute = "/api/model_catalog/v1/sources?assetType=serving_runtimes"

func servingRuntimeSourceStatusRouter(t *testing.T, repo models.CatalogSourceRepository) http.Handler {
	t.Helper()
	runtimeSources := serving_runtimecatalog.NewServingRuntimeSourceCollection()
	require.NoError(t, runtimeSources.Merge("test", map[string]basecatalog.PluginSource{
		"available":     {ID: "available", Name: "Available runtime"},
		"partial":       {ID: "partial", Name: "Partial runtime"},
		"error":         {ID: "error", Name: "Failed runtime"},
		"disabled":      {ID: "disabled", Name: "Disabled runtime", Enabled: new(false)},
		"uninitialized": {ID: "uninitialized", Name: "New runtime"},
	}))
	modelSources := catalog.NewSourceCollection()
	require.NoError(t, modelSources.Merge("test", map[string]catalog.ModelSource{
		"model": {CatalogSource: model.CatalogSource{Id: "model", Name: "Model source"}},
	}))
	service := NewModelCatalogServiceAPIService(&mockModelProvider{}, modelSources, nil, nil,
		catalog.NewLabelCollection(), repo, WithServingRuntimeSources(runtimeSources))
	return NewRouter(NewModelCatalogServiceAPIController(service))
}

func requestServingRuntimeSourceStatus(t *testing.T, router http.Handler, method, path string, status int, body any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	require.Equal(t, status, recorder.Code, "%s %s: %s", method, path, recorder.Body.String())
	if body == nil {
		assert.Empty(t, recorder.Body.String())
		return
	}
	assert.Equal(t, "application/json; charset=UTF-8", recorder.Header().Get("Content-Type"))
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), body))
}

func TestServingRuntimeSourcesRouterStatuses(t *testing.T) {
	repo := &fakeSourceStatusRepository{statuses: map[string]models.SourceStatus{
		"available": {Status: basecatalog.SourceStatusAvailable},
		"partial":   {Status: basecatalog.SourceStatusPartiallyAvailable, Error: "one runtime could not be loaded"},
		"error":     {Status: basecatalog.SourceStatusError, Error: "catalog file could not be read"},
		"disabled":  {Status: basecatalog.SourceStatusDisabled},
		"model":     {Status: basecatalog.SourceStatusAvailable},
	}}
	router := servingRuntimeSourceStatusRouter(t, repo)
	var list model.CatalogSourceList
	requestServingRuntimeSourceStatus(t, router, http.MethodGet, servingRuntimeSourcesRoute, http.StatusOK, &list)
	require.Len(t, list.Items, 5)
	assert.Equal(t, int32(5), list.Size)
	for _, source := range list.Items {
		t.Run(source.Id, func(t *testing.T) {
			assert.Equal(t, model.CATALOGASSETTYPE_SERVING_RUNTIMES, source.GetAssetType())
			assert.Equal(t, source.Id != "disabled", source.GetEnabled())
			expected, persisted := repo.statuses[source.Id]
			assert.Equal(t, persisted, source.HasStatus())
			assert.Equal(t, model.CatalogSourceStatus(expected.Status), source.GetStatus())
			assert.Equal(t, expected.Error != "", source.HasError())
			assert.Equal(t, expected.Error, source.GetError())

			var status model.SourceStatus
			requestServingRuntimeSourceStatus(t, router, http.MethodGet,
				"/api/model_catalog/v1/sources/"+source.Id+"/status", http.StatusOK, &status)
			assert.Equal(t, source.HasStatus(), status.HasStatus())
			assert.Equal(t, source.GetStatus(), status.GetStatus())
			assert.Equal(t, source.HasError(), status.HasError())
			assert.Equal(t, source.GetError(), status.GetError())
		})
	}
}

func TestServingRuntimeSourcesRouterClearStatus(t *testing.T) {
	repo := &fakeSourceStatusRepository{statuses: map[string]models.SourceStatus{
		"error":     {Status: basecatalog.SourceStatusError, Error: "stale load failure"},
		"available": {Status: basecatalog.SourceStatusAvailable},
	}}
	router := servingRuntimeSourceStatusRouter(t, repo)
	const statusPath = "/api/model_catalog/v1/sources/error/status"
	requestServingRuntimeSourceStatus(t, router, http.MethodDelete, statusPath, http.StatusNoContent, nil)
	assert.NotContains(t, repo.statuses, "error")
	assert.Contains(t, repo.statuses, "available")
	var status model.SourceStatus
	requestServingRuntimeSourceStatus(t, router, http.MethodGet, statusPath, http.StatusOK, &status)
	assert.False(t, status.HasStatus())
	assert.False(t, status.HasError())

	var list model.CatalogSourceList
	requestServingRuntimeSourceStatus(t, router, http.MethodGet, servingRuntimeSourcesRoute, http.StatusOK, &list)
	require.Len(t, list.Items, 5, "clearing status must preserve configured sources")
	var cleared *model.CatalogSource
	for i := range list.Items {
		if list.Items[i].Id == "error" {
			cleared = &list.Items[i]
		}
	}
	require.NotNil(t, cleared)
	assert.Equal(t, "Failed runtime", cleared.Name)
	assert.True(t, cleared.GetEnabled())
	assert.False(t, cleared.HasStatus())
	assert.False(t, cleared.HasError())
	// Repeating a clear and clearing an unknown source are both supported no-ops.
	requestServingRuntimeSourceStatus(t, router, http.MethodDelete, statusPath, http.StatusNoContent, nil)
	requestServingRuntimeSourceStatus(t, router, http.MethodDelete,
		"/api/model_catalog/v1/sources/unknown/status", http.StatusNoContent, nil)
	var unknown model.SourceStatus
	requestServingRuntimeSourceStatus(t, router, http.MethodGet,
		"/api/model_catalog/v1/sources/unknown/status", http.StatusOK, &unknown)
	assert.False(t, unknown.HasStatus())
	assert.False(t, unknown.HasError())
}

type failingRuntimeSourceStatusRepository struct {
	*fakeSourceStatusRepository
	err error
}

func (r *failingRuntimeSourceStatusRepository) GetAllStatuses() (map[string]models.SourceStatus, error) {
	return nil, r.err
}

func (r *failingRuntimeSourceStatusRepository) GetStatus(string) (models.SourceStatus, error) {
	return models.SourceStatus{}, r.err
}

func (r *failingRuntimeSourceStatusRepository) Delete(string) error { return r.err }

func TestServingRuntimeSourcesRouterStatusRepositoryErrors(t *testing.T) {
	repo := &failingRuntimeSourceStatusRepository{
		fakeSourceStatusRepository: &fakeSourceStatusRepository{statuses: map[string]models.SourceStatus{
			"error": {Status: basecatalog.SourceStatusError, Error: "previous load failure"},
		}},
		err: errors.New("status database unavailable"),
	}
	router := servingRuntimeSourceStatusRouter(t, repo)
	var list model.CatalogSourceList
	requestServingRuntimeSourceStatus(t, router, http.MethodGet, servingRuntimeSourcesRoute, http.StatusOK, &list)
	require.Len(t, list.Items, 5, "optional status failure must not prevent listing configured sources")
	for _, source := range list.Items {
		assert.False(t, source.HasStatus())
		assert.False(t, source.HasError())
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			var body model.Error
			requestServingRuntimeSourceStatus(t, router, method,
				"/api/model_catalog/v1/sources/error/status", http.StatusInternalServerError, &body)
			assert.Equal(t, "Internal Server Error", body.Code)
			assert.Contains(t, body.Message, repo.err.Error())
		})
	}
	assert.Contains(t, repo.statuses, "error", "a failed clear must preserve persisted status")
}
