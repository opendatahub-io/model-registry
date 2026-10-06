package service

import (
	"context"

	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	"gorm.io/gorm"
)

// NewRuntimeFamilyTransaction creates a transaction runner for a runtime and its versions.
func NewRuntimeFamilyTransaction(db *gorm.DB, runtimeTypeID, versionTypeID int32) func(context.Context, func(models.ServingRuntimeRepository, models.ServingRuntimeVersionRepository) error) error {
	return func(ctx context.Context, operation func(models.ServingRuntimeRepository, models.ServingRuntimeVersionRepository) error) error {
		return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return operation(NewServingRuntimeRepository(tx, runtimeTypeID), NewServingRuntimeVersionRepository(tx, versionTypeID))
		})
	}
}
