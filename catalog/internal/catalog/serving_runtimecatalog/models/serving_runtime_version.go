package models

import (
	dbmodels "github.com/kubeflow/hub/internal/platform/db/entity"
	"github.com/kubeflow/hub/internal/platform/db/filter"
)

// ServingRuntimeVersionListOptions holds the options for listing ServingRuntimeVersion entities.
type ServingRuntimeVersionListOptions struct {
	dbmodels.Pagination
	SourceIDs   *[]string
	FilterQuery *string
	// ParentResourceID scopes the list to versions attributed to a specific
	// ServingRuntime (context) ID via the Attribution table.
	ParentResourceID *int32
}

// GetRestEntityType implements the FilterApplier interface.
func (o *ServingRuntimeVersionListOptions) GetRestEntityType() filter.RestEntityType {
	return filter.RestEntityType("serving_runtime_version")
}

// GetFilterQuery returns the filter query string for advanced filtering.
func (o *ServingRuntimeVersionListOptions) GetFilterQuery() string {
	if o.FilterQuery == nil {
		return ""
	}
	return *o.FilterQuery
}

// ServingRuntimeVersionAttributes holds the attributes for a ServingRuntimeVersion record.
type ServingRuntimeVersionAttributes struct {
	Name                     *string
	ExternalID               *string
	CreateTimeSinceEpoch     *int64
	LastUpdateTimeSinceEpoch *int64
}

// ServingRuntimeVersion represents a ServingRuntimeVersion stored in the database.
type ServingRuntimeVersion interface {
	dbmodels.Entity[ServingRuntimeVersionAttributes]
}

// ServingRuntimeVersionImpl is the concrete implementation of ServingRuntimeVersion.
type ServingRuntimeVersionImpl = dbmodels.BaseEntity[ServingRuntimeVersionAttributes]

// ServingRuntimeVersionRepository defines the interface for ServingRuntimeVersion persistence.
type ServingRuntimeVersionRepository interface {
	GetByID(id int32) (ServingRuntimeVersion, error)
	GetByName(name string) (ServingRuntimeVersion, error)
	List(listOptions *ServingRuntimeVersionListOptions) (*dbmodels.ListWrapper[ServingRuntimeVersion], error)
	Save(entity ServingRuntimeVersion, parentResourceID *int32) (ServingRuntimeVersion, error)
	DeleteBySource(sourceID string) error
	DeleteByParentID(parentID int32) error
	DeleteByID(id int32) error
	GetDistinctSourceIDs() ([]string, error)
	GetTypeID() int32
}
