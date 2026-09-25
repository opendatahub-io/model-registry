package service

import (
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	dbmodels "github.com/kubeflow/hub/internal/platform/db/entity"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServingRuntimeRepository_SourceOwnershipIgnoresCustomProperties reproduces
// the phantom-source scenario reported against DeleteBySource/GetDistinctSourceIDs:
// a runtime that declares a *custom* property named "source_id" must not be
// mistaken for belonging to that source, since "source_id" is a reserved,
// non-custom bookkeeping property written by the loader (see loader.go). If the
// source-ownership queries matched custom properties too, a runtime in source
// "real-source" with customProperties.source_id = "other-source" would make
// GetDistinctSourceIDs report a phantom "other-source", and DeleteBySource on
// that phantom source would delete this runtime out from under its real source.
func TestServingRuntimeRepository_SourceOwnershipIgnoresCustomProperties(t *testing.T) {
	sharedDB, cleanup := testutils.SetupPostgresWithMigrations(t, testDatastoreSpec())
	defer cleanup()

	typeID := getServingRuntimeTypeID(t, sharedDB)
	repo := NewServingRuntimeRepository(sharedDB, typeID)

	const realSourceID = "source-ownership-real"
	const phantomSourceID = "source-ownership-phantom"

	runtime := &models.ServingRuntimeImpl{
		Attributes: &models.ServingRuntimeAttributes{
			Name: new("source-ownership-runtime"),
		},
		Properties: &[]dbmodels.Properties{
			{
				Name:        "source_id",
				StringValue: new(realSourceID),
			},
		},
		CustomProperties: &[]dbmodels.Properties{
			{
				Name:             "source_id",
				IsCustomProperty: true,
				StringValue:      new(phantomSourceID),
			},
		},
	}

	saved, err := repo.Save(runtime)
	require.NoError(t, err)
	require.NotNil(t, saved.GetID())

	t.Run("GetDistinctSourceIDs excludes the custom property value", func(t *testing.T) {
		sourceIDs, err := repo.GetDistinctSourceIDs()
		require.NoError(t, err)
		assert.Contains(t, sourceIDs, realSourceID)
		assert.NotContains(t, sourceIDs, phantomSourceID)
	})

	t.Run("DeleteBySource on the phantom source is a no-op", func(t *testing.T) {
		err := repo.DeleteBySource(phantomSourceID)
		require.NoError(t, err)

		_, err = repo.GetByID(*saved.GetID())
		require.NoError(t, err, "runtime must still exist: DeleteBySource matched a custom property")
	})

	t.Run("DeleteBySource on the real source removes it", func(t *testing.T) {
		err := repo.DeleteBySource(realSourceID)
		require.NoError(t, err)

		_, err = repo.GetByID(*saved.GetID())
		assert.ErrorIs(t, err, ErrServingRuntimeNotFound)
	})
}
