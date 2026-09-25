package models

import (
	dbmodels "github.com/kubeflow/hub/internal/platform/db/entity"
	"github.com/kubeflow/hub/internal/platform/db/filter"
)

// ServingRuntimeListOptions holds the options for listing ServingRuntime entities.
type ServingRuntimeListOptions struct {
	dbmodels.Pagination
	SourceIDs   *[]string
	FilterQuery *string
}

// GetRestEntityType implements the FilterApplier interface.
func (o *ServingRuntimeListOptions) GetRestEntityType() filter.RestEntityType {
	return filter.RestEntityType("serving_runtime")
}

// GetFilterQuery returns the filter query string for advanced filtering.
func (o *ServingRuntimeListOptions) GetFilterQuery() string {
	if o.FilterQuery == nil {
		return ""
	}
	return *o.FilterQuery
}

// ServingRuntimeAttributes holds the attributes for a ServingRuntime record.
type ServingRuntimeAttributes struct {
	Name                     *string
	ExternalID               *string
	CreateTimeSinceEpoch     *int64
	LastUpdateTimeSinceEpoch *int64
}

// ServingRuntime represents a ServingRuntime stored in the database.
type ServingRuntime interface {
	dbmodels.Entity[ServingRuntimeAttributes]
}

// ServingRuntimeImpl is the concrete implementation of ServingRuntime.
type ServingRuntimeImpl = dbmodels.BaseEntity[ServingRuntimeAttributes]

// ServingRuntimeRepository defines the interface for ServingRuntime persistence.
type ServingRuntimeRepository interface {
	GetByID(id int32) (ServingRuntime, error)
	GetByName(name string) (ServingRuntime, error)
	List(listOptions *ServingRuntimeListOptions) (*dbmodels.ListWrapper[ServingRuntime], error)
	Save(entity ServingRuntime) (ServingRuntime, error)
	DeleteBySource(sourceID string) error
	DeleteByID(id int32) error
	GetDistinctSourceIDs() ([]string, error)
	GetTypeID() int32
}
