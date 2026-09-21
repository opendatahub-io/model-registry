package serving_runtimecatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	openapi "github.com/kubeflow/hub/catalog/pkg/openapi"
	"github.com/kubeflow/hub/internal/platform/apiutils"
	"github.com/kubeflow/hub/pkg/api"
)

// DBServingRuntimeCatalog is a database-backed provider for serving_runtime assets.
type DBServingRuntimeCatalog struct {
	servingRuntimeRepo        models.ServingRuntimeRepository
	servingRuntimeVersionRepo models.ServingRuntimeVersionRepository
	sources                   *ServingRuntimeSourceCollection
}

// NewDBServingRuntimeCatalog creates a new database-backed serving_runtime provider.
func NewDBServingRuntimeCatalog(services Services, sources *ServingRuntimeSourceCollection) *DBServingRuntimeCatalog {
	return &DBServingRuntimeCatalog{
		servingRuntimeRepo:        services.ServingRuntimeRepository,
		servingRuntimeVersionRepo: services.ServingRuntimeVersionRepository,
		sources:                   sources,
	}
}

// ListServingRuntimesParams holds the parameters for listing serving_runtimes.
type ListServingRuntimesParams struct {
	Name          string
	SourceIDs     []string
	FilterQuery   string
	OrderBy       openapi.OrderByField
	SortOrder     openapi.SortOrder
	NextPageToken string
	PageSize      int32
}

// ListServingRuntimeVersionsParams holds the parameters for listing versions of a serving_runtime.
type ListServingRuntimeVersionsParams struct {
	FilterQuery   string
	OrderBy       openapi.OrderByField
	SortOrder     openapi.SortOrder
	NextPageToken string
	PageSize      int32
}

// GetFilterOptions returns the fields and values usable in filterQuery.
func (d *DBServingRuntimeCatalog) GetFilterOptions(ctx context.Context) (*openapi.FilterOptionsList, error) {
	_ = ctx
	options := make(map[string]openapi.FilterOption)
	return &openapi.FilterOptionsList{
		Filters: &options,
	}, nil
}

// ListServingRuntimes returns a paginated list of serving_runtimes.
func (d *DBServingRuntimeCatalog) ListServingRuntimes(ctx context.Context, params ListServingRuntimesParams) (openapi.ServingRuntimeList, error) {
	_ = ctx

	filterQuery := params.FilterQuery
	listOptions := &models.ServingRuntimeListOptions{
		FilterQuery: &filterQuery,
	}

	if len(params.SourceIDs) > 0 {
		listOptions.SourceIDs = &params.SourceIDs
	}

	orderBy := strings.ToUpper(string(params.OrderBy))
	sortOrder := strings.ToUpper(string(params.SortOrder))
	listOptions.Pagination.PageSize = &params.PageSize
	if orderBy != "" {
		listOptions.Pagination.OrderBy = &orderBy
	}
	if sortOrder != "" {
		listOptions.Pagination.SortOrder = &sortOrder
	}
	if params.NextPageToken != "" {
		listOptions.Pagination.NextPageToken = &params.NextPageToken
	}

	runtimesList, err := d.servingRuntimeRepo.List(listOptions)
	if err != nil {
		return openapi.ServingRuntimeList{}, err
	}

	items := make([]openapi.ServingRuntime, 0, len(runtimesList.Items))
	for _, dbRuntime := range runtimesList.Items {
		apiRuntime, err := mapDBServingRuntimeToAPI(dbRuntime)
		if err != nil {
			return openapi.ServingRuntimeList{}, err
		}
		items = append(items, apiRuntime)
	}

	return openapi.ServingRuntimeList{
		Items:         items,
		Size:          int32(len(items)),
		PageSize:      params.PageSize,
		NextPageToken: runtimesList.NextPageToken,
	}, nil
}

// GetServingRuntime returns a single serving_runtime by its ID.
func (d *DBServingRuntimeCatalog) GetServingRuntime(ctx context.Context, id string) (*openapi.ServingRuntime, error) {
	_ = ctx

	intID, err := apiutils.ValidateIDAsInt32(id, "serving_runtime")
	if err != nil {
		return nil, fmt.Errorf("invalid serving_runtime ID '%s': %w", id, api.ErrBadRequest)
	}

	dbRuntime, err := d.servingRuntimeRepo.GetByID(intID)
	if err != nil {
		return nil, fmt.Errorf("serving_runtime not found with ID %s: %w", id, api.ErrNotFound)
	}

	apiRuntime, err := mapDBServingRuntimeToAPI(dbRuntime)
	if err != nil {
		return nil, err
	}
	return &apiRuntime, nil
}

// ListServingRuntimeVersions returns a paginated list of versions for a serving_runtime.
func (d *DBServingRuntimeCatalog) ListServingRuntimeVersions(ctx context.Context, runtimeID string, params ListServingRuntimeVersionsParams) (openapi.ServingRuntimeVersionList, error) {
	_ = ctx

	intID, err := apiutils.ValidateIDAsInt32(runtimeID, "serving_runtime")
	if err != nil {
		return openapi.ServingRuntimeVersionList{}, fmt.Errorf("invalid serving_runtime ID '%s': %w", runtimeID, api.ErrBadRequest)
	}

	// Ensure the parent serving_runtime exists before listing its versions.
	if _, err := d.servingRuntimeRepo.GetByID(intID); err != nil {
		return openapi.ServingRuntimeVersionList{}, fmt.Errorf("serving_runtime not found with ID %s: %w", runtimeID, api.ErrNotFound)
	}

	filterQuery := params.FilterQuery
	listOptions := &models.ServingRuntimeVersionListOptions{
		FilterQuery:      &filterQuery,
		ParentResourceID: &intID,
	}

	orderBy := strings.ToUpper(string(params.OrderBy))
	sortOrder := strings.ToUpper(string(params.SortOrder))
	listOptions.Pagination.PageSize = &params.PageSize
	if orderBy != "" {
		listOptions.Pagination.OrderBy = &orderBy
	}
	if sortOrder != "" {
		listOptions.Pagination.SortOrder = &sortOrder
	}
	if params.NextPageToken != "" {
		listOptions.Pagination.NextPageToken = &params.NextPageToken
	}

	versionsList, err := d.servingRuntimeVersionRepo.List(listOptions)
	if err != nil {
		return openapi.ServingRuntimeVersionList{}, err
	}

	items := make([]openapi.ServingRuntimeVersion, 0, len(versionsList.Items))
	for _, dbVersion := range versionsList.Items {
		apiVersion, err := mapDBServingRuntimeVersionToAPI(dbVersion)
		if err != nil {
			return openapi.ServingRuntimeVersionList{}, err
		}
		items = append(items, apiVersion)
	}

	return openapi.ServingRuntimeVersionList{
		Items:         items,
		Size:          int32(len(items)),
		PageSize:      params.PageSize,
		NextPageToken: versionsList.NextPageToken,
	}, nil
}

// mapDBServingRuntimeToAPI maps a database serving_runtime entity to its OpenAPI representation.
func mapDBServingRuntimeToAPI(m models.ServingRuntime) (openapi.ServingRuntime, error) {
	res := openapi.ServingRuntime{}

	if m.GetID() != nil {
		id := strconv.FormatInt(int64(*m.GetID()), 10)
		res.Id = &id
	}

	if attrs := m.GetAttributes(); attrs != nil {
		res.Name = attrs.Name
		res.ExternalId = attrs.ExternalID
		if attrs.CreateTimeSinceEpoch != nil {
			createTime := strconv.FormatInt(*attrs.CreateTimeSinceEpoch, 10)
			res.CreateTimeSinceEpoch = &createTime
		}
		if attrs.LastUpdateTimeSinceEpoch != nil {
			lastUpdateTime := strconv.FormatInt(*attrs.LastUpdateTimeSinceEpoch, 10)
			res.LastUpdateTimeSinceEpoch = &lastUpdateTime
		}
	}

	if m.GetProperties() != nil {
		for _, prop := range *m.GetProperties() {
			switch prop.Name {
			case "source_id":
				res.SourceId = prop.StringValue
			case "displayName":
				res.DisplayName = prop.StringValue
			case "description":
				res.Description = prop.StringValue
			case "provider":
				res.Provider = prop.StringValue
			case "readme":
				res.Readme = prop.StringValue
			case "logo":
				res.Logo = prop.StringValue
			case "license":
				res.License = prop.StringValue
			case "licenseLink":
				res.LicenseLink = prop.StringValue
			case "documentationUrl":
				res.DocumentationUrl = prop.StringValue
			case "repositoryUrl":
				res.RepositoryUrl = prop.StringValue
			case "tags":
				if prop.StringValue != nil {
					var tags []string
					if err := json.Unmarshal([]byte(*prop.StringValue), &tags); err == nil {
						res.Tags = tags
					}
				}
			case "supportedModelFormats":
				if prop.StringValue != nil {
					var formats []openapi.SupportedModelFormat
					if err := json.Unmarshal([]byte(*prop.StringValue), &formats); err == nil {
						res.SupportedModelFormats = formats
					}
				}
			case "capabilities":
				if prop.StringValue != nil {
					var caps openapi.ServingRuntimeCapabilities
					if err := json.Unmarshal([]byte(*prop.StringValue), &caps); err == nil {
						res.Capabilities = &caps
					}
				}
			case "versionCount":
				if prop.IntValue != nil {
					res.VersionCount = prop.IntValue
				}
			case "publishedDate":
				res.PublishedDate = parseTimePtr(prop.StringValue)
			case "lastUpdated":
				res.LastUpdated = parseTimePtr(prop.StringValue)
			}
		}
	}

	return res, nil
}

// mapDBServingRuntimeVersionToAPI maps a database serving_runtime_version artifact to its OpenAPI representation.
func mapDBServingRuntimeVersionToAPI(m models.ServingRuntimeVersion) (openapi.ServingRuntimeVersion, error) {
	res := openapi.ServingRuntimeVersion{
		ArtifactType: "serving-runtime-version",
	}

	if m.GetID() != nil {
		id := strconv.FormatInt(int64(*m.GetID()), 10)
		res.Id = &id
	}

	if attrs := m.GetAttributes(); attrs != nil {
		res.Name = attrs.Name
		res.ExternalId = attrs.ExternalID
		if attrs.CreateTimeSinceEpoch != nil {
			createTime := strconv.FormatInt(*attrs.CreateTimeSinceEpoch, 10)
			res.CreateTimeSinceEpoch = &createTime
		}
		if attrs.LastUpdateTimeSinceEpoch != nil {
			lastUpdateTime := strconv.FormatInt(*attrs.LastUpdateTimeSinceEpoch, 10)
			res.LastUpdateTimeSinceEpoch = &lastUpdateTime
		}
	}

	if m.GetProperties() != nil {
		for _, prop := range *m.GetProperties() {
			switch prop.Name {
			case "artifactType":
				if prop.StringValue != nil && *prop.StringValue != "" {
					res.ArtifactType = *prop.StringValue
				}
			case "version":
				if prop.StringValue != nil {
					res.Version = *prop.StringValue
				}
			case "image":
				if prop.StringValue != nil {
					res.Image = *prop.StringValue
				}
			case "supportLevel":
				if prop.StringValue != nil {
					level := openapi.ServingRuntimeSupportLevel(*prop.StringValue)
					res.SupportLevel = &level
				}
			case "supportedModelFormats":
				if prop.StringValue != nil {
					var formats []openapi.SupportedModelFormat
					if err := json.Unmarshal([]byte(*prop.StringValue), &formats); err == nil {
						res.SupportedModelFormats = formats
					}
				}
			case "protocolVersions":
				if prop.StringValue != nil {
					var protocols []string
					if err := json.Unmarshal([]byte(*prop.StringValue), &protocols); err == nil {
						res.ProtocolVersions = protocols
					}
				}
			case "recommendedResources":
				if prop.StringValue != nil {
					var rec openapi.ServingRuntimeResourceRecommendation
					if err := json.Unmarshal([]byte(*prop.StringValue), &rec); err == nil {
						res.RecommendedResources = &rec
					}
				}
			case "defaultArgs":
				if prop.StringValue != nil {
					var args []string
					if err := json.Unmarshal([]byte(*prop.StringValue), &args); err == nil {
						res.DefaultArgs = args
					}
				}
			case "env":
				if prop.StringValue != nil {
					var env []openapi.ServingRuntimeEnvVar
					if err := json.Unmarshal([]byte(*prop.StringValue), &env); err == nil {
						res.Env = env
					}
				}
			case "template":
				res.Template = prop.StringValue
			case "deprecated":
				if prop.BoolValue != nil {
					res.Deprecated = prop.BoolValue
				}
			case "publishedDate":
				res.PublishedDate = parseTimePtr(prop.StringValue)
			}
		}
	}

	return res, nil
}

// parseTimePtr parses an RFC3339 timestamp string into a *time.Time, returning nil on failure.
func parseTimePtr(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil
	}
	return &t
}
