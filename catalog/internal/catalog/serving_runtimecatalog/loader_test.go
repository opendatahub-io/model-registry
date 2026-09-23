package serving_runtimecatalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	runtimeservice "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/service"
	dbmodels "github.com/kubeflow/hub/catalog/internal/db/models"
	"github.com/kubeflow/hub/catalog/internal/db/service"
	"github.com/kubeflow/hub/catalog/internal/testhelpers"
	openapi "github.com/kubeflow/hub/catalog/pkg/openapi"
	mrmodels "github.com/kubeflow/hub/internal/platform/db/entity"
	"github.com/kubeflow/hub/internal/platform/db/schema"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) { os.Exit(testutils.TestMainPostgresHelper(m)) }

func setupServingRuntimeLoader(t *testing.T) (*gorm.DB, Services) {
	db, cleanup := testutils.SetupPostgresWithMigrations(t, testhelpers.MustDatastoreSpec(t))
	t.Cleanup(cleanup)
	runtimeType := schema.Type{Name: "kf.ServingRuntime", TypeKind: 1}
	versionType := schema.Type{Name: "kf.ServingRuntimeVersion", TypeKind: 2}
	require.NoError(t, db.Create(&runtimeType).Error)
	require.NoError(t, db.Create(&versionType).Error)
	return db, Services{
		ServingRuntimeRepository:        runtimeservice.NewServingRuntimeRepository(db, runtimeType.ID),
		ServingRuntimeVersionRepository: runtimeservice.NewServingRuntimeVersionRepository(db, versionType.ID),
		CatalogSourceRepository:         service.NewCatalogSourceRepository(db, testhelpers.GetCatalogSourceTypeIDForDBTest(t, db)),
		PropertyOptionsRepository:       service.NewPropertyOptionsRepository(db),
	}
}

func writeRuntimeFile(t *testing.T, path, data string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
}

func runtimeVersions(t *testing.T, services Services, parentID int32) []models.ServingRuntimeVersion {
	t.Helper()
	list, err := services.ServingRuntimeVersionRepository.List(&models.ServingRuntimeVersionListOptions{ParentResourceID: &parentID})
	require.NoError(t, err)
	return list.Items
}

func TestServingRuntimeLoaderReloadAndValidation(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "runtimes.yaml")
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    name: First\n    type: yaml\n    properties:\n      yamlCatalogPath: runtimes.yaml\n")
	writeRuntimeFile(t, dataPath, "serving_runtimes:\n  - name: vllm\n    displayName: vLLM\n    customProperties:\n      owner: {metadataType: MetadataStringValue, string_value: test}\n      priority: {metadataType: MetadataIntValue, int_value: '5'}\n    versions:\n      - version: '1'\n        image: example:v1\n        supportLevel: supported\n      - version: '2'\n        image: example:v2\n  - name: ovms\n    versions:\n      - version: '1'\n        image: ovms:v1\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"]))
	before, err := services.ServingRuntimeRepository.GetByName("first:vllm")
	require.NoError(t, err)
	require.Len(t, runtimeVersions(t, services, *before.GetID()), 2)
	versionBefore, err := services.ServingRuntimeVersionRepository.GetByName("first:vllm:1")
	require.NoError(t, err)
	assert.NotEmpty(t, before.GetCustomProperties())
	assert.Equal(t, "first:vllm:1", *versionBefore.GetAttributes().Name)
	versionAPI, err := NewDBServingRuntimeCatalog(services, loader.Sources).ListServingRuntimeVersions(t.Context(), strconv.FormatInt(int64(*before.GetID()), 10), ListServingRuntimeVersionsParams{})
	require.NoError(t, err)
	require.Len(t, versionAPI.Items, 2)
	assert.Equal(t, "example:v1", versionAPI.Items[0].Image)
	// The API response must not leak the internal "sourceID:" qualifier stored on the entity name.
	assert.Equal(t, "vllm:1", *versionAPI.Items[0].Name)
	apiRuntime, err := NewDBServingRuntimeCatalog(services, loader.Sources).GetServingRuntime(t.Context(), strconv.FormatInt(int64(*before.GetID()), 10))
	require.NoError(t, err)
	assert.Equal(t, "vllm", *apiRuntime.Name)
	assert.Contains(t, apiRuntime.CustomProperties, "owner")
	assert.Equal(t, "test", apiRuntime.CustomProperties["owner"].MetadataStringValue.StringValue)
	require.NotNil(t, apiRuntime.CustomProperties["priority"].MetadataIntValue)
	assert.Equal(t, "5", apiRuntime.CustomProperties["priority"].MetadataIntValue.IntValue)

	writeRuntimeFile(t, dataPath, "serving_runtimes:\n  - name: vllm\n    versions:\n      - version: '1'\n        image: example:new\n")
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"]))
	after, err := services.ServingRuntimeRepository.GetByName("first:vllm")
	require.NoError(t, err)
	assert.Equal(t, before.GetID(), after.GetID())
	versionAfter, err := services.ServingRuntimeVersionRepository.GetByName("first:vllm:1")
	require.NoError(t, err)
	assert.Equal(t, versionBefore.GetID(), versionAfter.GetID())
	apiRuntime, err = NewDBServingRuntimeCatalog(services, loader.Sources).GetServingRuntime(t.Context(), strconv.FormatInt(int64(*after.GetID()), 10))
	require.NoError(t, err)
	assert.Nil(t, apiRuntime.DisplayName)
	assert.Empty(t, apiRuntime.CustomProperties)
	versionAPI, err = NewDBServingRuntimeCatalog(services, loader.Sources).ListServingRuntimeVersions(t.Context(), strconv.FormatInt(int64(*after.GetID()), 10), ListServingRuntimeVersionsParams{})
	require.NoError(t, err)
	require.Len(t, versionAPI.Items, 1)
	assert.Nil(t, versionAPI.Items[0].SupportLevel)
	require.Len(t, runtimeVersions(t, services, *after.GetID()), 1)
	_, err = services.ServingRuntimeRepository.GetByName("first:ovms")
	require.Error(t, err)

	writeRuntimeFile(t, dataPath, "serving_runtimes:\n  - name: vllm\n    versions:\n      - version: '1'\n        image: example:v1\n  - name: vllm\n")
	require.Error(t, loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"]))
	retained, err := services.ServingRuntimeRepository.GetByName("first:vllm")
	require.NoError(t, err)
	assert.Equal(t, after.GetID(), retained.GetID())
}

func TestServingRuntimeLoaderSourceCleanup(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "runtimes.yaml")
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, dataPath, "serving_runtimes:\n  - name: same\n    versions: [{version: '1', image: example:v1}]\n")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - {id: first, type: yaml, properties: {yamlCatalogPath: runtimes.yaml}}\n  - {id: second, type: yaml, properties: {yamlCatalogPath: runtimes.yaml}}\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)
	for id, source := range loader.Sources.AllSources() {
		require.NoError(t, loader.loadFromYAML(t.Context(), id, source))
	}
	first, err := services.ServingRuntimeRepository.GetByName("first:same")
	require.NoError(t, err)
	second, err := services.ServingRuntimeRepository.GetByName("second:same")
	require.NoError(t, err)
	assert.NotEqual(t, first.GetID(), second.GetID())

	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - {id: second, type: yaml, enabled: false, properties: {yamlCatalogPath: runtimes.yaml}}\n")
	require.NoError(t, loader.ReloadParsing())
	basecatalog.SaveSourceStatus(services.CatalogSourceRepository, "first", basecatalog.SourceStatusAvailable, "")
	require.NoError(t, loader.removeRuntimesFromMissingSources(mapset.NewSet("first")))
	_, err = services.ServingRuntimeRepository.GetByName("first:same")
	require.Error(t, err)
	_, err = services.ServingRuntimeRepository.GetByName("second:same")
	require.Error(t, err)
	assert.Empty(t, runtimeVersions(t, services, *first.GetID()))
	assert.Empty(t, runtimeVersions(t, services, *second.GetID()))
	status, err := services.CatalogSourceRepository.GetStatus("first")
	require.NoError(t, err)
	assert.Equal(t, basecatalog.SourceStatusAvailable, status.Status)
	require.NoError(t, loader.removeRuntimesFromMissingSources(mapset.NewSet[string]()))
	status, err = services.CatalogSourceRepository.GetStatus("first")
	require.NoError(t, err)
	assert.Empty(t, status.Status)
}

func TestServingRuntimeLoaderWatchesDataFile(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "runtimes.yaml")
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, dataPath, "serving_runtimes:\n  - name: old\n    versions: [{version: '1', image: example:v1}]\n")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - {id: watched, type: yaml, properties: {yamlCatalogPath: runtimes.yaml}}\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, loader.PerformLeaderOperations(ctx, mapset.NewSet[string]()))
	state.WaitForInflightWrites(5 * time.Second)
	_, err := services.ServingRuntimeRepository.GetByName("watched:old")
	require.NoError(t, err)
	options, err := services.PropertyOptionsRepository.List(dbmodels.ArtifactPropertyOptionType, services.ServingRuntimeVersionRepository.GetTypeID())
	require.NoError(t, err)
	assert.Condition(t, func() bool {
		for _, option := range options {
			if option.Name == "image" && len(option.StringValue) == 1 && option.StringValue[0] == "example:v1" {
				return true
			}
		}
		return false
	})
	writeRuntimeFile(t, dataPath, "serving_runtimes:\n  - name: new\n")
	assert.Eventually(t, func() bool {
		_, err := services.ServingRuntimeRepository.GetByName("watched:new")
		return err == nil
	}, 15*time.Second, 100*time.Millisecond)
}

func TestServingRuntimeVersionDeleteByParentIDPreservesOtherArtifactTypes(t *testing.T) {
	db, services := setupServingRuntimeLoader(t)
	name := "source:runtime"
	runtime, err := services.ServingRuntimeRepository.Save(&models.ServingRuntimeImpl{
		Attributes: &models.ServingRuntimeAttributes{Name: &name},
	})
	require.NoError(t, err)
	versionName := name + ":1"
	_, err = services.ServingRuntimeVersionRepository.Save(&models.ServingRuntimeVersionImpl{
		Attributes: &models.ServingRuntimeVersionAttributes{Name: &versionName},
	}, runtime.GetID())
	require.NoError(t, err)
	otherName := "unrelated"
	other := schema.Artifact{
		TypeID:                   testhelpers.GetCatalogModelArtifactTypeIDForDBTest(t, db),
		Name:                     &otherName,
		CreateTimeSinceEpoch:     1,
		LastUpdateTimeSinceEpoch: 1,
	}
	require.NoError(t, db.Create(&other).Error)
	require.NoError(t, db.Create(&schema.Attribution{ContextID: *runtime.GetID(), ArtifactID: other.ID}).Error)

	require.NoError(t, services.ServingRuntimeVersionRepository.DeleteByParentID(*runtime.GetID()))
	assert.Empty(t, runtimeVersions(t, services, *runtime.GetID()))
	var retained schema.Artifact
	require.NoError(t, db.First(&retained, other.ID).Error)
	assert.Equal(t, otherName, *retained.Name)
}

// runWithTimeout runs fn in a goroutine and fails the test if it doesn't return
// within timeout, rather than hanging the whole test run if fn regresses into
// an infinite loop.
func runWithTimeout(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(timeout):
		t.Fatal("function did not return in time; it is likely stuck re-fetching the same page")
	}
}

func TestRemoveOrphanedRuntimesPaginatesAcrossPages(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	const total = 150 // more than the loader's hardcoded 100-item page size
	sourceID := "paginated"
	valid := mapset.NewSet[string]()
	for i := range total {
		name := fmt.Sprintf("%s:runtime-%03d", sourceID, i)
		properties := []mrmodels.Properties{mrmodels.NewStringProperty("source_id", sourceID, false)}
		_, err := services.ServingRuntimeRepository.Save(&models.ServingRuntimeImpl{
			Attributes: &models.ServingRuntimeAttributes{Name: &name},
			Properties: &properties,
		})
		require.NoError(t, err)
		valid.Add(name)
	}

	state := basecatalog.NewBaseLoader(nil)
	state.SetLeader(true)
	loader := NewServingRuntimeLoader(services, state)

	runWithTimeout(t, 15*time.Second, func() error {
		return loader.removeOrphanedRuntimes(sourceID, valid)
	})

	list, err := services.ServingRuntimeRepository.List(&models.ServingRuntimeListOptions{SourceIDs: &[]string{sourceID}})
	require.NoError(t, err)
	assert.Len(t, list.Items, total, "no valid runtimes should have been removed")
}

func TestServingRuntimeListFiltersByName(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "runtimes.yaml")
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    type: yaml\n    properties:\n      yamlCatalogPath: runtimes.yaml\n")
	writeRuntimeFile(t, dataPath, "serving_runtimes:\n  - name: vllm\n    versions: [{version: '1', image: example:v1}]\n  - name: ovms\n    versions: [{version: '1', image: example:v1}]\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"]))

	provider := NewDBServingRuntimeCatalog(services, loader.Sources)
	filtered, err := provider.ListServingRuntimes(t.Context(), ListServingRuntimesParams{Name: "vllm"})
	require.NoError(t, err)
	require.Len(t, filtered.Items, 1)
	assert.Equal(t, "vllm", *filtered.Items[0].Name)

	all, err := provider.ListServingRuntimes(t.Context(), ListServingRuntimesParams{})
	require.NoError(t, err)
	assert.Len(t, all.Items, 2)
}

func TestServingRuntimeCustomPropertyIntOverflow(t *testing.T) {
	inRange := openapi.MetadataValue{MetadataIntValue: openapi.NewMetadataIntValue("42", "MetadataIntValue")}
	prop, err := servingRuntimeCustomProperty("priority", inRange)
	require.NoError(t, err)
	require.NotNil(t, prop.IntValue)
	assert.Equal(t, int32(42), *prop.IntValue)

	overflow := openapi.MetadataValue{MetadataIntValue: openapi.NewMetadataIntValue("3000000000", "MetadataIntValue")}
	_, err = servingRuntimeCustomProperty("maxTokens", overflow)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxTokens")
}

func TestServingRuntimeLoaderRejectsIntCustomPropertyOverflow(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "runtimes.yaml")
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    type: yaml\n    properties:\n      yamlCatalogPath: runtimes.yaml\n")
	writeRuntimeFile(t, dataPath, "serving_runtimes:\n  - name: vllm\n    customProperties:\n      maxTokens: {metadataType: MetadataIntValue, int_value: '3000000000'}\n    versions: [{version: '1', image: example:v1}]\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)

	err := loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"])
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxTokens")
	_, err = services.ServingRuntimeRepository.GetByName("first:vllm")
	require.Error(t, err, "the runtime must not be persisted when a custom property fails to build")
}

func TestServingRuntimeSaveDeletesMissingPropertiesAtomically(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	name := "source:runtime"
	initial := []mrmodels.Properties{
		mrmodels.NewStringProperty("displayName", "vLLM", false),
		mrmodels.NewStringProperty("description", "a runtime", false),
	}
	saved, err := services.ServingRuntimeRepository.Save(&models.ServingRuntimeImpl{
		Attributes: &models.ServingRuntimeAttributes{Name: &name},
		Properties: &initial,
	})
	require.NoError(t, err)
	require.Len(t, *saved.GetProperties(), 2)

	updated := []mrmodels.Properties{
		mrmodels.NewStringProperty("displayName", "vLLM", false),
	}
	_, err = services.ServingRuntimeRepository.Save(&models.ServingRuntimeImpl{
		ID:         saved.GetID(),
		Attributes: &models.ServingRuntimeAttributes{Name: &name},
		Properties: &updated,
	})
	require.NoError(t, err)

	after, err := services.ServingRuntimeRepository.GetByID(*saved.GetID())
	require.NoError(t, err)
	names := make([]string, 0, len(*after.GetProperties()))
	for _, prop := range *after.GetProperties() {
		names = append(names, prop.Name)
	}
	assert.ElementsMatch(t, []string{"displayName"}, names, "the dropped 'description' property must be deleted, not left stale")
}

func TestRemoveOrphanedVersionsPaginatesAcrossPages(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	name := "paginated:runtime"
	runtime, err := services.ServingRuntimeRepository.Save(&models.ServingRuntimeImpl{
		Attributes: &models.ServingRuntimeAttributes{Name: &name},
	})
	require.NoError(t, err)

	const total = 150 // more than the loader's hardcoded 100-item page size
	valid := mapset.NewSet[string]()
	for i := range total {
		versionName := fmt.Sprintf("%s:%03d", name, i)
		_, err := services.ServingRuntimeVersionRepository.Save(&models.ServingRuntimeVersionImpl{
			Attributes: &models.ServingRuntimeVersionAttributes{Name: &versionName},
		}, runtime.GetID())
		require.NoError(t, err)
		valid.Add(versionName)
	}

	state := basecatalog.NewBaseLoader(nil)
	state.SetLeader(true)
	loader := NewServingRuntimeLoader(services, state)

	runWithTimeout(t, 15*time.Second, func() error {
		return loader.removeOrphanedVersions(*runtime.GetID(), valid)
	})

	assert.Len(t, runtimeVersions(t, services, *runtime.GetID()), total, "no valid versions should have been removed")
}
