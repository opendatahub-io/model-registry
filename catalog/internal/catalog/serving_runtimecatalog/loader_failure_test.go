package serving_runtimecatalog

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	"github.com/kubeflow/hub/internal/platform/db/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Save hooks inject deterministic interruptions after real repository writes.
type runtimeSaveHook struct {
	models.ServingRuntimeRepository
	before func(models.ServingRuntime) error
	after  func(models.ServingRuntime)
}

func (r runtimeSaveHook) Save(entity models.ServingRuntime) (models.ServingRuntime, error) {
	if r.before != nil {
		if err := r.before(entity); err != nil {
			return nil, err
		}
	}
	saved, err := r.ServingRuntimeRepository.Save(entity)
	if err == nil && r.after != nil {
		r.after(saved)
	}
	return saved, err
}

type versionSaveHook struct {
	models.ServingRuntimeVersionRepository
	after func(models.ServingRuntimeVersion)
}

func (r versionSaveHook) Save(entity models.ServingRuntimeVersion, parentID *int32) (models.ServingRuntimeVersion, error) {
	saved, err := r.ServingRuntimeVersionRepository.Save(entity, parentID)
	if err == nil {
		r.after(saved)
	}
	return saved, err
}

type runtimeDeleteHook struct {
	models.ServingRuntimeRepository
	after func() error
}

func (r runtimeDeleteHook) DeleteByID(id int32) error {
	if err := r.ServingRuntimeRepository.DeleteByID(id); err != nil {
		return err
	}
	return r.after()
}

type versionDeleteHook struct {
	models.ServingRuntimeVersionRepository
	after func() error
}

func (r versionDeleteHook) DeleteByID(id int32) error {
	if err := r.ServingRuntimeVersionRepository.DeleteByID(id); err != nil {
		return err
	}
	return r.after()
}

type persistedRuntime struct {
	Row        schema.Context
	Properties []schema.ContextProperty
}

type persistedVersion struct {
	Row          schema.Artifact
	Properties   []schema.ArtifactProperty
	Attributions []schema.Attribution
}

type runtimeSourceSnapshot struct {
	Runtimes map[string]persistedRuntime
	Versions map[string]persistedVersion
}

// Read artifacts by qualified name and type, independently of properties and
// parent-scoped List: a broken save must not hide an unlinked child from checks.
func snapshotRuntimeSource(t *testing.T, db *gorm.DB, services Services, sourceID string) runtimeSourceSnapshot {
	t.Helper()
	snapshot := runtimeSourceSnapshot{
		Runtimes: make(map[string]persistedRuntime),
		Versions: make(map[string]persistedVersion),
	}
	var runtimes []schema.Context
	require.NoError(t, db.Where("type_id = ? AND name LIKE ?", services.ServingRuntimeRepository.GetTypeID(), sourceID+":%").Find(&runtimes).Error)
	for _, row := range runtimes {
		entry := persistedRuntime{Row: row}
		require.NoError(t, db.Where("context_id = ?", row.ID).Order("name, is_custom_property").Find(&entry.Properties).Error)
		snapshot.Runtimes[row.Name] = entry
	}
	var versions []schema.Artifact
	require.NoError(t, db.Where("type_id = ? AND name LIKE ?", services.ServingRuntimeVersionRepository.GetTypeID(), sourceID+":%").Find(&versions).Error)
	for _, row := range versions {
		require.NotNil(t, row.Name)
		entry := persistedVersion{Row: row}
		require.NoError(t, db.Where("artifact_id = ?", row.ID).Order("name, is_custom_property").Find(&entry.Properties).Error)
		require.NoError(t, db.Where("artifact_id = ?", row.ID).Order("id").Find(&entry.Attributions).Error)
		snapshot.Versions[*row.Name] = entry
	}
	return snapshot
}

func assertRuntimeSourceContents(t *testing.T, snapshot runtimeSourceSnapshot, sourceID string, displays, images map[string]string) {
	t.Helper()
	require.Len(t, snapshot.Runtimes, len(displays), "source %s runtime membership", sourceID)
	for name, display := range displays {
		runtime, ok := snapshot.Runtimes[sourceID+":"+name]
		require.True(t, ok, "source %s runtime %s is missing", sourceID, name)
		properties := make(map[string]schema.ContextProperty)
		for _, property := range runtime.Properties {
			if !property.IsCustomProperty {
				properties[property.Name] = property
			}
		}
		assert.Equal(t, &display, properties["displayName"].StringValue, "runtime %s displayName", name)
		assert.Equal(t, &sourceID, properties["source_id"].StringValue, "runtime %s ownership", name)
	}
	require.Len(t, snapshot.Versions, len(images), "source %s version membership, including unlinked artifacts", sourceID)
	for name, image := range images {
		version, ok := snapshot.Versions[sourceID+":"+name]
		require.True(t, ok, "source %s version %s is missing", sourceID, name)
		properties := make(map[string]schema.ArtifactProperty)
		for _, property := range version.Properties {
			if !property.IsCustomProperty {
				properties[property.Name] = property
			}
		}
		assert.Equal(t, &image, properties["image"].StringValue, "version %s image", name)
		assert.Equal(t, &sourceID, properties["source_id"].StringValue, "version %s ownership", name)
		require.Len(t, version.Attributions, 1, "version %s must have exactly one parent", name)
		runtimeName, _, found := strings.Cut(name, ":")
		require.True(t, found, "version %s must include the runtime name", name)
		parentName := sourceID + ":" + runtimeName
		parent, ok := snapshot.Runtimes[parentName]
		require.True(t, ok, "version %s parent %s is missing", name, parentName)
		assert.Equal(t, parent.Row.ID, version.Attributions[0].ContextID, "version %s parent attribution", name)
	}
}

func TestServingRuntimeLoaderInterruptedReload(t *testing.T) {
	initial := `serving_runtimes:
  - name: same
    displayName: Original
    customProperties:
      owner: {metadataType: MetadataStringValue, string_value: original-owner}
    versions:
      - {version: '1', image: 'same:old'}
      - {version: '9', image: 'same:stale'}
  - name: later
    displayName: Later original
    versions: [{version: '1', image: 'later:old'}]
  - name: removed
    displayName: Removed original
    versions: [{version: '1', image: 'removed:old'}]
`
	replacement := `serving_runtimes:
  - name: same
    displayName: Updated
    versions:
      - {version: '1', image: 'same:new'}
      - {version: '2', image: 'same:added'}
  - name: added
    displayName: Added
    versions: [{version: '1', image: 'added:new'}]
  - name: later
    displayName: Later updated
    versions: [{version: '1', image: 'later:new'}]
`
	tests := []struct{ name, interruption string }{
		{"failed/pre_canceled_preserves_both_sources", "before_load"},
		{"failed/cancel_after_runtime_preserves_snapshot", "after_runtime"},
		{"failed/cancel_after_version_preserves_snapshot", "after_version"},
		{"failed/later_runtime_error_preserves_snapshot", "runtime_error"},
		{"failed/version_attribution_error_preserves_snapshot", "version_error"},
		{"failed/cancel_during_version_cleanup_preserves_snapshot", "version_cleanup"},
		{"failed/cancel_during_runtime_cleanup_preserves_snapshot", "runtime_cleanup"},
		{"failed/error_during_runtime_cleanup_preserves_snapshot", "cleanup_error"},
		{"failed/leadership_loss_preserves_snapshot", "leadership_loss"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, services := setupServingRuntimeLoader(t)
			dir := t.TempDir()
			dataPath := filepath.Join(dir, "failed.yaml")
			configPath := filepath.Join(dir, "sources.yaml")
			writeRuntimeFile(t, dataPath, initial)
			writeRuntimeFile(t, filepath.Join(dir, "healthy.yaml"), `serving_runtimes:
  - name: same
    displayName: Healthy
    versions:
      - {version: '1', image: 'healthy:stable'}
      - {version: '9', image: 'healthy:retained'}
  - name: removed
    displayName: Healthy retained
    versions: [{version: '1', image: 'healthy:removed-retained'}]
`)
			writeRuntimeFile(t, configPath, `serving_runtime_catalogs:
  - {id: failed, type: yaml, properties: {yamlCatalogPath: failed.yaml}}
  - {id: healthy, type: yaml, properties: {yamlCatalogPath: healthy.yaml}}
`)
			state := basecatalog.NewBaseLoader([]string{configPath})
			state.SetLeader(true)
			loader := NewServingRuntimeLoader(services, state)
			require.NoError(t, loader.ParseAllConfigs())
			for _, id := range []string{"failed", "healthy"} {
				require.NoError(t, loader.loadFromYAML(t.Context(), id, loader.Sources.AllSources()[id]), "seed source %s", id)
			}
			before := snapshotRuntimeSource(t, db, services, "failed")
			owner := "original-owner"
			require.Contains(t, before.Runtimes["failed:same"].Properties, schema.ContextProperty{
				ContextID: before.Runtimes["failed:same"].Row.ID, Name: "owner", IsCustomProperty: true, StringValue: &owner,
			}, "initial load must persist the custom property before testing its deletion")
			healthy := snapshotRuntimeSource(t, db, services, "healthy")
			assertRuntimeSourceContents(t, healthy, "healthy",
				map[string]string{"same": "Healthy", "removed": "Healthy retained"},
				map[string]string{"same:1": "healthy:stable", "same:9": "healthy:retained", "removed:1": "healthy:removed-retained"})
			writeRuntimeFile(t, dataPath, replacement)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			injected := errors.New("injected serving runtime write failure")
			wantErr := context.Canceled
			fired := false
			failVersionWrites := true
			var failedArtifactID int32
			switch tt.interruption {
			case "before_load":
				cancel()
				fired = true
			case "after_runtime", "leadership_loss":
				loader.services.ServingRuntimeRepository = runtimeSaveHook{
					ServingRuntimeRepository: services.ServingRuntimeRepository,
					after: func(saved models.ServingRuntime) {
						if *saved.GetAttributes().Name == "failed:same" {
							fired = true
							assert.Equal(t, before, snapshotRuntimeSource(t, db, services, "failed"), "readers see the complete old dataset during a reload")
							if tt.interruption == "leadership_loss" {
								state.SetLeader(false)
							} else {
								cancel()
							}
						}
					},
				}
			case "after_version":
				loader.services.ServingRuntimeVersionRepository = versionSaveHook{
					ServingRuntimeVersionRepository: services.ServingRuntimeVersionRepository,
					after: func(saved models.ServingRuntimeVersion) {
						if *saved.GetAttributes().Name == "failed:same:1" {
							fired = true
							cancel()
						}
					},
				}
			case "version_cleanup", "runtime_cleanup", "cleanup_error":
				if tt.interruption == "cleanup_error" {
					wantErr = injected
				}
			case "runtime_error":
				wantErr = injected
				loader.services.ServingRuntimeRepository = runtimeSaveHook{
					ServingRuntimeRepository: services.ServingRuntimeRepository,
					before: func(entity models.ServingRuntime) error {
						if *entity.GetAttributes().Name == "failed:added" {
							fired = true
							return injected
						}
						return nil
					},
				}
			case "version_error":
				wantErr = injected
				var targetParentID int32
				loader.services.ServingRuntimeRepository = runtimeSaveHook{
					ServingRuntimeRepository: services.ServingRuntimeRepository,
					after: func(saved models.ServingRuntime) {
						if *saved.GetAttributes().Name == "failed:added" {
							targetParentID = *saved.GetID()
						}
					},
				}
				const callbackName = "test:fail_serving_runtime_attribution"
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
					attribution, ok := tx.Statement.Dest.(*schema.Attribution)
					if ok && failVersionWrites && targetParentID != 0 && attribution.ContextID == targetParentID {
						fired = true
						failedArtifactID = attribution.ArtifactID
						_ = tx.AddError(injected) // AddError stores the failure on tx for Save to return.
					}
				}))
				t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove(callbackName)) })
			}

			transact := services.Transact
			loader.services.Transact = func(ctx context.Context, fn func(Services) error) error {
				return transact(ctx, func(txServices Services) error {
					if hook, ok := loader.services.ServingRuntimeRepository.(runtimeSaveHook); ok {
						hook.ServingRuntimeRepository = txServices.ServingRuntimeRepository
						txServices.ServingRuntimeRepository = hook
					}
					if hook, ok := loader.services.ServingRuntimeVersionRepository.(versionSaveHook); ok {
						hook.ServingRuntimeVersionRepository = txServices.ServingRuntimeVersionRepository
						txServices.ServingRuntimeVersionRepository = hook
					}
					switch tt.interruption {
					case "version_cleanup":
						txServices.ServingRuntimeVersionRepository = versionDeleteHook{txServices.ServingRuntimeVersionRepository, func() error { fired = true; cancel(); return nil }}
					case "runtime_cleanup", "cleanup_error":
						txServices.ServingRuntimeRepository = runtimeDeleteHook{txServices.ServingRuntimeRepository, func() error {
							fired = true
							assert.Equal(t, before, snapshotRuntimeSource(t, db, services, "failed"), "cleanup changes remain invisible until commit")
							if tt.interruption == "cleanup_error" {
								return injected
							}
							cancel()
							return nil
						}}
					}
					return fn(txServices)
				})
			}
			if tt.interruption == "leadership_loss" {
				wantErr = errServingRuntimeLeadershipLost
			}

			err := loader.loadFromYAML(ctx, "failed", loader.Sources.AllSources()["failed"])
			require.True(t, fired, "interruption %s must fire during source failed reload", tt.interruption)
			require.ErrorIs(t, err, wantErr)
			if tt.interruption == "runtime_error" {
				assert.Contains(t, err.Error(), "failed to save serving_runtime \"added\"")
			}
			if tt.interruption == "version_error" {
				assert.Contains(t, err.Error(), "failed to save serving_runtime version \"1\" for \"added\"")
				require.Positive(t, failedArtifactID, "artifact insertion precedes the failed attribution")
				for _, model := range []any{&schema.Artifact{}, &schema.ArtifactProperty{}, &schema.Attribution{}} {
					column := "artifact_id"
					if _, ok := model.(*schema.Artifact); ok {
						column = "id"
					}
					var count int64
					require.NoError(t, db.Model(model).Where(column+" = ?", failedArtifactID).Count(&count).Error)
					assert.Zero(t, count, "failed version must leave no %T rows", model)
				}
			}
			after := snapshotRuntimeSource(t, db, services, "failed")
			assert.Equal(t, before, after, "interrupted source reload must preserve the complete previous snapshot")
			assert.Equal(t, healthy, snapshotRuntimeSource(t, db, services, "healthy"), "failed reload must preserve all healthy source rows")

			// Disable injection and retry with a fresh context. Recovery must clean
			// stale records and finish the previously incomplete parent/version set.
			loader.services = services
			failVersionWrites = false
			state.SetLeader(true)
			require.NoError(t, loader.loadFromYAML(t.Context(), "failed", loader.Sources.AllSources()["failed"]))
			recovered := snapshotRuntimeSource(t, db, services, "failed")
			assertRuntimeSourceContents(t, recovered, "failed",
				map[string]string{"same": "Updated", "added": "Added", "later": "Later updated"},
				map[string]string{"same:1": "same:new", "same:2": "same:added", "added:1": "added:new", "later:1": "later:new"})
			for _, property := range recovered.Runtimes["failed:same"].Properties {
				assert.False(t, property.IsCustomProperty, "retry removes the old custom property %s", property.Name)
			}
			assert.Equal(t, before.Runtimes["failed:same"].Row.ID, recovered.Runtimes["failed:same"].Row.ID, "retry retains the original runtime ID")
			assert.Equal(t, before.Versions["failed:same:1"].Row.ID, recovered.Versions["failed:same:1"].Row.ID, "retry retains the original version ID")
			var staleAttributions int64
			require.NoError(t, db.Model(&schema.Attribution{}).
				Where("context_id = ? OR artifact_id = ?", before.Runtimes["failed:removed"].Row.ID, before.Versions["failed:same:9"].Row.ID).
				Count(&staleAttributions).Error)
			assert.Zero(t, staleAttributions, "retry removes stale parent/version attributions")
			assert.Equal(t, healthy, snapshotRuntimeSource(t, db, services, "healthy"), "successful retry must preserve healthy source rows")
		})
	}
}

// Cancellation inside a SQL callback verifies that repository writes inherit the
// reload context. Checks between saves alone cannot catch a missing WithContext.
func TestServingRuntimeLoaderCancellationReachesDatabase(t *testing.T) {
	db, services := setupServingRuntimeLoader(t)
	path := filepath.Join(t.TempDir(), "runtimes.yaml")
	writeRuntimeFile(t, path, "serving_runtimes:\n  - name: vllm\n    versions: [{version: '1', image: 'example:v1'}]\n")
	state := basecatalog.NewBaseLoader(nil)
	state.SetLeader(true)
	loader := NewServingRuntimeLoader(services, state)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const callback = "test:cancel_serving_runtime_sql"
	fired := false
	var writeError error
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if row, ok := tx.Statement.Dest.(*schema.Context); ok && row.Name == "canceled:vllm" {
			fired = true
			cancel()
		}
	}))
	require.NoError(t, db.Callback().Create().After("gorm:create").Register(callback+":result", func(tx *gorm.DB) {
		if row, ok := tx.Statement.Dest.(*schema.Context); ok && row.Name == "canceled:vllm" {
			writeError = tx.Error
		}
	}))
	t.Cleanup(func() {
		require.NoError(t, db.Callback().Create().Remove(callback))
		require.NoError(t, db.Callback().Create().Remove(callback+":result"))
	})
	err := loader.loadFromYAML(ctx, "canceled", basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}})
	require.True(t, fired, "source canceled runtime vllm must reach the database write")
	require.ErrorIs(t, writeError, context.Canceled, "runtime vllm SQL must fail when its reload context is canceled")
	require.ErrorIs(t, err, context.Canceled, "source canceled must preserve the SQL cancellation cause")
	snapshot := snapshotRuntimeSource(t, db, services, "canceled")
	assert.Empty(t, snapshot.Runtimes, "canceled first load must leave no parent rows")
	assert.Empty(t, snapshot.Versions, "canceled first load must leave no child rows")
}
