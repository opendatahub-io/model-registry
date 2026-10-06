package serving_runtimecatalog

import (
	"context"

	servingRuntimemodels "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	servingRuntimeservice "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/service"
	sharedmodels "github.com/kubeflow/hub/catalog/internal/db/models"
	"gorm.io/gorm"
)

type Services struct {
	ServingRuntimeRepository        servingRuntimemodels.ServingRuntimeRepository
	ServingRuntimeVersionRepository servingRuntimemodels.ServingRuntimeVersionRepository
	CatalogSourceRepository         sharedmodels.CatalogSourceRepository
	PropertyOptionsRepository       sharedmodels.PropertyOptionsRepository
	// WithRuntimeFamilyTransaction supplies repositories sharing one transaction.
	WithRuntimeFamilyTransaction func(context.Context, func(servingRuntimemodels.ServingRuntimeRepository, servingRuntimemodels.ServingRuntimeVersionRepository) error) error
}

// WithTransactions enables atomic runtime family writes without changing the
// repositories used by API reads or other loaders.
func (s Services) WithTransactions(db *gorm.DB) Services {
	s.WithRuntimeFamilyTransaction = servingRuntimeservice.NewRuntimeFamilyTransaction(db,
		s.ServingRuntimeRepository.GetTypeID(), s.ServingRuntimeVersionRepository.GetTypeID())
	return s
}
