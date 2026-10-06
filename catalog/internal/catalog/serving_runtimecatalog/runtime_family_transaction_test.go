package serving_runtimecatalog

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	runtimeservice "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/service"
	"github.com/kubeflow/hub/internal/platform/db/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type runtimeFamilySnapshot struct {
	Parent            schema.Context
	ParentProperties  []schema.ContextProperty
	Versions          []schema.Artifact
	VersionProperties []schema.ArtifactProperty
	Relationships     []schema.Attribution
}

// Capture database rows, including timestamps and properties, independently of entity mapping.
func snapshotRuntimeFamily(t *testing.T, db *gorm.DB, parentID int32) runtimeFamilySnapshot {
	t.Helper()
	var snapshot runtimeFamilySnapshot
	require.NoError(t, db.First(&snapshot.Parent, parentID).Error)
	require.NoError(t, db.Where("context_id = ?", parentID).Order("name, is_custom_property").Find(&snapshot.ParentProperties).Error)
	require.NoError(t, db.Where("context_id = ?", parentID).Order("id").Find(&snapshot.Relationships).Error)
	ids := db.Model(&schema.Attribution{}).Select("artifact_id").Where("context_id = ?", parentID)
	require.NoError(t, db.Where("id IN (?)", ids).Order("id").Find(&snapshot.Versions).Error)
	require.NoError(t, db.Where("artifact_id IN (?)", ids).Order("artifact_id, name, is_custom_property").Find(&snapshot.VersionProperties).Error)
	return snapshot
}

func TestServingRuntimeNewFamilyFailureLeavesNoRows(t *testing.T) {
	db, services := setupServingRuntimeLoader(t)
	path := filepath.Join(t.TempDir(), "runtimes.yaml")
	state := basecatalog.NewBaseLoader(nil)
	state.SetLeader(true)
	loader := NewServingRuntimeLoader(services, state)
	source := basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}}
	writeRuntimeFile(t, path, "serving_runtimes:\n  - name: retained\n    versions: [{version: '1', image: original:1}]\n")
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", source))
	retained, err := services.ServingRuntimeRepository.GetByName("first:retained")
	require.NoError(t, err)
	before := snapshotRuntimeFamily(t, db, *retained.GetID())
	loader.services.ServingRuntimeVersionRepository = failingVersionRepository{
		ServingRuntimeVersionRepository: services.ServingRuntimeVersionRepository,
		saveName:                        "first:failed:broken",
	}
	loader.services = bindRuntimeTransactionRepositories(loader.services)
	writeRuntimeFile(t, path, "serving_runtimes:\n  - name: failed\n    versions: [{version: 'new', image: new:1}, {version: 'broken', image: new:2}]\n")
	outcome, err := loader.loadYAML(t.Context(), "first", source)
	require.NoError(t, err)
	assert.Equal(t, basecatalog.SourceStatusError, outcome.status(err))
	assert.Zero(t, outcome.SuccessfulRuntimes)
	assert.False(t, outcome.Changed)
	assert.Equal(t, []string{"first:failed"}, outcome.FailedRuntimeIDs)
	assert.Equal(t, before, snapshotRuntimeFamily(t, db, *retained.GetID()))
	_, err = services.ServingRuntimeRepository.GetByName("first:failed")
	require.ErrorIs(t, err, runtimeservice.ErrServingRuntimeNotFound)
	for _, table := range []struct {
		model any
		count int64
	}{
		{&schema.Context{}, 1},
		{&schema.ContextProperty{}, int64(len(before.ParentProperties))},
		{&schema.Artifact{}, 1},
		{&schema.ArtifactProperty{}, int64(len(before.VersionProperties))},
		{&schema.Attribution{}, 1},
	} {
		var count int64
		require.NoError(t, db.Model(table.model).Count(&count).Error)
		assert.Equal(t, table.count, count, "failed family must leave no rows in %T", table.model)
	}
}

func TestServingRuntimeFamilyInterruptionRollsBack(t *testing.T) {
	for _, timing := range []string{"after parent save", "before commit"} {
		for _, interruption := range []string{"cancellation", "leadership loss"} {
			t.Run(timing+"/"+interruption, func(t *testing.T) {
				db, services := setupServingRuntimeLoader(t)
				path := filepath.Join(t.TempDir(), "runtimes.yaml")
				state := basecatalog.NewBaseLoader(nil)
				state.SetLeader(true)
				loader := NewServingRuntimeLoader(services, state)
				source := basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}}
				writeRuntimeFile(t, path, "serving_runtimes:\n  - name: current\n    displayName: Original\n    versions: [{version: '1', image: original:1}]\n  - name: absent\n")
				require.NoError(t, loader.loadFromYAML(t.Context(), "first", source))
				current, err := services.ServingRuntimeRepository.GetByName("first:current")
				require.NoError(t, err)
				before := snapshotRuntimeFamily(t, db, *current.GetID())
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				interrupt := func() {
					if interruption == "cancellation" {
						cancel()
					} else {
						state.SetLeader(false)
					}
				}
				if timing == "after parent save" {
					loader.services.ServingRuntimeRepository = failingRuntimeRepository{ServingRuntimeRepository: services.ServingRuntimeRepository, afterSave: interrupt}
				} else {
					loader.services.ServingRuntimeVersionRepository = failingVersionRepository{ServingRuntimeVersionRepository: services.ServingRuntimeVersionRepository, afterList: interrupt}
				}
				loader.services = bindRuntimeTransactionRepositories(loader.services)
				writeRuntimeFile(t, path, "serving_runtimes:\n  - name: before\n  - name: current\n    displayName: Updated\n    versions: [{version: '1', image: updated:1}, {version: '2', image: new:2}]\n  - name: after\n")
				// Interrupt only the current family, after the preceding family committed.
				run := loader.services.WithRuntimeFamilyTransaction
				loader.services.WithRuntimeFamilyTransaction = func(ctx context.Context, operation func(models.ServingRuntimeRepository, models.ServingRuntimeVersionRepository) error) error {
					if _, err := services.ServingRuntimeRepository.GetByName("first:before"); err != nil {
						return services.WithRuntimeFamilyTransaction(ctx, operation)
					}
					return run(ctx, operation)
				}
				outcome, err := loader.loadYAML(ctx, "first", source)
				if interruption == "cancellation" {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.ErrorIs(t, err, errServingRuntimeLeadershipLost)
				}
				assert.Equal(t, 1, outcome.SuccessfulRuntimes)
				assert.True(t, outcome.Changed, "preceding family remains committed")
				assert.Equal(t, before, snapshotRuntimeFamily(t, db, *current.GetID()))
				_, err = services.ServingRuntimeRepository.GetByName("first:before")
				require.NoError(t, err)
				_, err = services.ServingRuntimeRepository.GetByName("first:after")
				require.ErrorIs(t, err, runtimeservice.ErrServingRuntimeNotFound)
				_, err = services.ServingRuntimeRepository.GetByName("first:absent")
				require.NoError(t, err, "interruption skips source cleanup")
			})
		}
	}
}

func TestServingRuntimeLoaderRequiresFamilyTransaction(t *testing.T) {
	state := basecatalog.NewBaseLoader(nil)
	state.SetLeader(true)
	loader := NewServingRuntimeLoader(Services{}, state)
	outcome, err := loader.loadYAML(t.Context(), "first", basecatalog.PluginSource{})
	require.ErrorContains(t, err, "transaction support is required")
	assert.False(t, outcome.Changed)
	assert.Zero(t, outcome.SuccessfulRuntimes)
}
