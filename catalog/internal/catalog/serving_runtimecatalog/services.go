package serving_runtimecatalog

import (
	"context"

	servingRuntimemodels "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	servingRuntimeservice "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/service"
	sharedmodels "github.com/kubeflow/hub/catalog/internal/db/models"
	"gorm.io/gorm"
)

type Services struct {
	// Transact runs a source reload with repositories bound to one transaction.
	Transact                        func(context.Context, func(Services) error) error
	ServingRuntimeRepository        servingRuntimemodels.ServingRuntimeRepository
	ServingRuntimeVersionRepository servingRuntimemodels.ServingRuntimeVersionRepository
	CatalogSourceRepository         sharedmodels.CatalogSourceRepository
	PropertyOptionsRepository       sharedmodels.PropertyOptionsRepository
}

// WithTransactions enables atomic source reloads without changing the repositories
// used by API reads or other loaders. Transaction repositories carry the request
// context through every lookup, save, and cleanup operation.
func (s Services) WithTransactions(db *gorm.DB) Services {
	s.Transact = func(ctx context.Context, fn func(Services) error) error {
		return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			transactionServices := s
			transactionServices.ServingRuntimeRepository = servingRuntimeservice.NewServingRuntimeRepository(tx, s.ServingRuntimeRepository.GetTypeID())
			transactionServices.ServingRuntimeVersionRepository = servingRuntimeservice.NewServingRuntimeVersionRepository(tx, s.ServingRuntimeVersionRepository.GetTypeID())
			return fn(transactionServices)
		})
	}
	return s
}
