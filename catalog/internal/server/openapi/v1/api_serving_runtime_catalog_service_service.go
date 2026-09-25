package v1

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog"
	model "github.com/kubeflow/hub/catalog/pkg/openapi"
	"github.com/kubeflow/hub/pkg/api"
)

// ServingRuntimeCatalogServiceAPIService implements the ServingRuntimeCatalogServiceAPIServicer.
type ServingRuntimeCatalogServiceAPIService struct {
	provider *serving_runtimecatalog.DBServingRuntimeCatalog
	sources  *serving_runtimecatalog.ServingRuntimeSourceCollection
}

var _ ServingRuntimeCatalogServiceAPIServicer = &ServingRuntimeCatalogServiceAPIService{}

// NewServingRuntimeCatalogServiceAPIService creates a default api service.
func NewServingRuntimeCatalogServiceAPIService(provider *serving_runtimecatalog.DBServingRuntimeCatalog, sources *serving_runtimecatalog.ServingRuntimeSourceCollection) ServingRuntimeCatalogServiceAPIServicer {
	return &ServingRuntimeCatalogServiceAPIService{
		provider: provider,
		sources:  sources,
	}
}

// FindServingRuntimes - List serving_runtimes.
func (s *ServingRuntimeCatalogServiceAPIService) FindServingRuntimes(ctx context.Context, name string, q string, source []string, sourceLabel []string, filterQuery string, pageSize string, orderBy model.OrderByField, sortOrder model.SortOrder, nextPageToken string) (ImplResponse, error) {
	pageSizeInt, err := parsePaginationParams(pageSize, nextPageToken)
	if err != nil {
		return ErrorResponse(http.StatusBadRequest, err), err
	}

	sourceIDs, empty := resolveServingRuntimeSourceIDs(s.sources, source, sourceLabel)
	if empty {
		return Response(http.StatusOK, model.ServingRuntimeList{
			Items:    []model.ServingRuntime{},
			PageSize: pageSizeInt,
		}), nil
	}

	params := serving_runtimecatalog.ListServingRuntimesParams{
		Name:          name,
		SourceIDs:     sourceIDs,
		FilterQuery:   filterQuery,
		OrderBy:       orderBy,
		SortOrder:     sortOrder,
		NextPageToken: nextPageToken,
		PageSize:      pageSizeInt,
	}

	runtimes, err := s.provider.ListServingRuntimes(ctx, params)
	if err != nil {
		return ErrorResponse(api.ErrToStatus(err), err), err
	}

	return Response(http.StatusOK, runtimes), nil
}

// FindServingRuntimesFilterOptions - Lists fields and values usable in filterQuery.
func (s *ServingRuntimeCatalogServiceAPIService) FindServingRuntimesFilterOptions(ctx context.Context) (ImplResponse, error) {
	filterOptions, err := s.provider.GetFilterOptions(ctx)
	if err != nil {
		return ErrorResponse(http.StatusInternalServerError, err), err
	}
	return Response(http.StatusOK, *filterOptions), nil
}

// GetServingRuntime - Get a serving_runtime by ID.
func (s *ServingRuntimeCatalogServiceAPIService) GetServingRuntime(ctx context.Context, id string) (ImplResponse, error) {
	runtime, err := s.provider.GetServingRuntime(ctx, id)
	if err != nil {
		return ErrorResponse(api.ErrToStatus(err), err), err
	}
	if runtime == nil {
		return ErrorResponse(http.StatusNotFound, errors.New("serving_runtime not found")), nil
	}
	return Response(http.StatusOK, *runtime), nil
}

// GetServingRuntimeVersions - List versions of a `ServingRuntime`.
func (s *ServingRuntimeCatalogServiceAPIService) GetServingRuntimeVersions(ctx context.Context, id string, filterQuery string, pageSize string, orderBy model.OrderByField, sortOrder model.SortOrder, nextPageToken string) (ImplResponse, error) {
	pageSizeInt, err := parsePaginationParams(pageSize, nextPageToken)
	if err != nil {
		return ErrorResponse(http.StatusBadRequest, err), err
	}

	params := serving_runtimecatalog.ListServingRuntimeVersionsParams{
		FilterQuery:   filterQuery,
		OrderBy:       orderBy,
		SortOrder:     sortOrder,
		NextPageToken: nextPageToken,
		PageSize:      pageSizeInt,
	}

	versions, err := s.provider.ListServingRuntimeVersions(ctx, id, params)
	if err != nil {
		return ErrorResponse(api.ErrToStatus(err), err), err
	}

	return Response(http.StatusOK, versions), nil
}

// resolveServingRuntimeSourceIDs merges explicit source IDs with sources matched by label.
// It returns the resolved source IDs and a boolean indicating whether the caller requested
// a filter that matched no sources (in which case the result set is empty).
func resolveServingRuntimeSourceIDs(sources *serving_runtimecatalog.ServingRuntimeSourceCollection, source []string, sourceLabel []string) ([]string, bool) {
	if len(source) == 1 && source[0] == "" {
		source = nil
	}
	if len(sourceLabel) == 1 && sourceLabel[0] == "" {
		sourceLabel = nil
	}

	if len(sourceLabel) == 0 {
		return source, false
	}

	if sources == nil {
		return source, len(source) == 0
	}

	var matched []string
	for id, src := range sources.AllSources() {
		for _, label := range src.Labels {
			if slices.Contains(sourceLabel, label) {
				matched = append(matched, id)
				break
			}
		}
	}

	if len(source) > 0 {
		matched = append(matched, source...)
	}

	if len(matched) == 0 {
		return nil, true
	}
	return matched, false
}
