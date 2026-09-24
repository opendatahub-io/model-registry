package v1

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	model "github.com/kubeflow/hub/catalog/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failingServingRuntimeRepository struct {
	models.ServingRuntimeRepository
	err error
}

func (r failingServingRuntimeRepository) GetByID(int32) (models.ServingRuntime, error) {
	return nil, r.err
}

func TestFindServingRuntimesRejectsCombinedSourceFilters(t *testing.T) {
	service := NewServingRuntimeCatalogServiceAPIService(nil, nil)
	response, err := service.FindServingRuntimes(context.Background(), "", "", []string{"first"}, []string{"official"}, "", "", "", "", "")
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, response.Code)
}

func TestResolveServingRuntimeSourceLabels(t *testing.T) {
	collection := serving_runtimecatalog.NewServingRuntimeSourceCollection()
	require.NoError(t, collection.Merge("test", map[string]basecatalog.PluginSource{
		"labeled":   {ID: "labeled", Labels: []string{"Official"}},
		"unlabeled": {ID: "unlabeled"},
		"disabled":  {ID: "disabled", Labels: []string{"Official"}, Enabled: new(false)},
	}))

	ids, empty := resolveServingRuntimeSourceIDs(collection, nil, []string{"official"})
	require.False(t, empty)
	assert.Equal(t, []string{"labeled"}, ids)
	ids, empty = resolveServingRuntimeSourceIDs(collection, nil, []string{"null"})
	require.False(t, empty)
	assert.Equal(t, []string{"unlabeled"}, ids)
	ids, empty = resolveServingRuntimeSourceIDs(collection, nil, []string{"missing"})
	assert.True(t, empty)
	assert.Empty(t, ids)
}

func TestGetServingRuntimeReportsDatastoreFailure(t *testing.T) {
	provider := serving_runtimecatalog.NewDBServingRuntimeCatalog(serving_runtimecatalog.Services{
		ServingRuntimeRepository: failingServingRuntimeRepository{err: errors.New("database unavailable")},
	}, nil)
	service := NewServingRuntimeCatalogServiceAPIService(provider, nil)
	response, err := service.GetServingRuntime(context.Background(), "12")
	require.Error(t, err)
	assert.Equal(t, http.StatusInternalServerError, response.Code)
}

func TestServingRuntimeListsRejectInvalidSortParameters(t *testing.T) {
	service := NewServingRuntimeCatalogServiceAPIService(nil, nil)
	response, err := service.FindServingRuntimes(context.Background(), "", "", nil, nil, "", "", model.OrderByField("unknown"), "", "")
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, response.Code)

	response, err = service.GetServingRuntimeVersions(context.Background(), "1", "", "", "", model.SortOrder("unknown"), "")
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, response.Code)
}
