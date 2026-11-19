package client

// DTOs for the MLflow Model Registry REST API ( /api/2.0/mlflow/ ).
// Only the fields the adapter reads or writes are modeled.

// Tag is a model or model-version registry tag.
type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// RegisteredModelAlias points an alias at a native version.
type RegisteredModelAlias struct {
	Alias   string `json:"alias"`
	Version string `json:"version"`
}

// RegisteredModel is a native registered model.
type RegisteredModel struct {
	Name                 string                 `json:"name"`
	Description          string                 `json:"description,omitempty"`
	CreationTimestamp    int64                  `json:"creation_timestamp,omitempty"`
	LastUpdatedTimestamp int64                  `json:"last_updated_timestamp,omitempty"`
	Tags                 []Tag                  `json:"tags,omitempty"`
	Aliases              []RegisteredModelAlias `json:"aliases,omitempty"`
}

// ModelVersion is a native model version. Version is the server-generated
// decimal version number, carried as a string to preserve exactness.
type ModelVersion struct {
	Name                 string `json:"name"`
	Version              string `json:"version"`
	Source               string `json:"source,omitempty"`
	Status               string `json:"status,omitempty"`
	Description          string `json:"description,omitempty"`
	CreationTimestamp    int64  `json:"creation_timestamp,omitempty"`
	LastUpdatedTimestamp int64  `json:"last_updated_timestamp,omitempty"`
	Tags                 []Tag  `json:"tags,omitempty"`
}

// CreateRegisteredModelRequest creates a native registered model.
type CreateRegisteredModelRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Tags        []Tag  `json:"tags,omitempty"`
}

// UpdateRegisteredModelRequest changes a registered model's description.
type UpdateRegisteredModelRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CreateVersionRequest creates a native model version. Source is required
// and nonempty by upstream validation; no tracking run or experiment is
// ever referenced.
type CreateVersionRequest struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Description string `json:"description,omitempty"`
	Tags        []Tag  `json:"tags,omitempty"`
}

// UpdateVersionRequest changes a model version's description only.
type UpdateVersionRequest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// SetTagRequest sets one tag on a model or model version.
type SetTagRequest struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Key     string `json:"key"`
	Value   string `json:"value"`
}

// DeleteTagRequest deletes one tag from a model or model version.
type DeleteTagRequest struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Key     string `json:"key"`
}

// SetAliasRequest points a registered-model alias at a version.
type SetAliasRequest struct {
	Name    string `json:"name"`
	Alias   string `json:"alias"`
	Version string `json:"version"`
}

// SearchRequest is the shared paged search body for registered models and
// model versions.
type SearchRequest struct {
	Filter     string   `json:"filter,omitempty"`
	MaxResults int32    `json:"max_results,omitempty"`
	OrderBy    []string `json:"order_by,omitempty"`
	PageToken  string   `json:"page_token,omitempty"`
}

// SearchRegisteredModelsResponse is a paged registered-model search.
type SearchRegisteredModelsResponse struct {
	RegisteredModels []RegisteredModel `json:"registered_models"`
	NextPageToken    string            `json:"next_page_token,omitempty"`
}

// SearchVersionsResponse is a paged model-version search.
type SearchVersionsResponse struct {
	ModelVersions []ModelVersion `json:"model_versions"`
	NextPageToken string         `json:"next_page_token,omitempty"`
}

// GetAliasResponse returns the version an alias points at.
type GetAliasResponse struct {
	ModelVersion ModelVersion `json:"model_version"`
}
