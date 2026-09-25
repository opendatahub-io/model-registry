package service

import (
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	dbmodels "github.com/kubeflow/hub/internal/platform/db/entity"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServingRuntimeVersionRepository_SourceOwnershipIgnoresCustomProperties is the
// version-repository counterpart of TestServingRuntimeRepository_SourceOwnershipIgnoresCustomProperties:
// DeleteBySource and GetDistinctSourceIDs must only ever match the non-custom
// "source_id" property written by the loader, never a same-named custom property.
func TestServingRuntimeVersionRepository_SourceOwnershipIgnoresCustomProperties(t *testing.T) {
	sharedDB, cleanup := testutils.SetupPostgresWithMigrations(t, testDatastoreSpec())
	defer cleanup()

	typeID := getServingRuntimeVersionTypeID(t, sharedDB)
	repo := NewServingRuntimeVersionRepository(sharedDB, typeID)

	const realSourceID = "version-ownership-real"
	const phantomSourceID = "version-ownership-phantom"

	version := &models.ServingRuntimeVersionImpl{
		Attributes: &models.ServingRuntimeVersionAttributes{
			Name: new("version-ownership-runtime:v1"),
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

	saved, err := repo.Save(version, nil)
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
		require.NoError(t, err, "version must still exist: DeleteBySource matched a custom property")
	})

	t.Run("DeleteBySource on the real source removes it", func(t *testing.T) {
		err := repo.DeleteBySource(realSourceID)
		require.NoError(t, err)

		_, err = repo.GetByID(*saved.GetID())
		assert.ErrorIs(t, err, ErrServingRuntimeVersionNotFound)
	})
}
