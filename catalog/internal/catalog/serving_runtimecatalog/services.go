package serving_runtimecatalog

import (
	servingRuntimeVersionmodels "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	servingRuntimemodels "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	sharedmodels "github.com/kubeflow/hub/catalog/internal/db/models"
)

type Services struct {
	ServingRuntimeRepository        servingRuntimemodels.ServingRuntimeRepository
	ServingRuntimeVersionRepository servingRuntimeVersionmodels.ServingRuntimeVersionRepository
	CatalogSourceRepository         sharedmodels.CatalogSourceRepository
	PropertyOptionsRepository       sharedmodels.PropertyOptionsRepository
}
