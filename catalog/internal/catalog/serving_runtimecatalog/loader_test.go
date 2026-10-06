package serving_runtimecatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
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
	t.Helper()
	db, cleanup := testutils.SetupPostgresWithMigrations(t, testhelpers.MustDatastoreSpec(t))
	t.Cleanup(cleanup)
	testutils.CleanupPostgresTestData(t, db)
	runtimeType := schema.Type{Name: "kf.ServingRuntime", TypeKind: 1}
	versionType := schema.Type{Name: "kf.ServingRuntimeVersion", TypeKind: 2}
	require.NoError(t, db.Where("name = ?", runtimeType.Name).FirstOrCreate(&runtimeType).Error)
	require.NoError(t, db.Where("name = ?", versionType.Name).FirstOrCreate(&versionType).Error)
	return db, Services{
		ServingRuntimeRepository:        runtimeservice.NewServingRuntimeRepository(db, runtimeType.ID),
		ServingRuntimeVersionRepository: runtimeservice.NewServingRuntimeVersionRepository(db, versionType.ID),
		CatalogSourceRepository:         service.NewCatalogSourceRepository(db, testhelpers.GetCatalogSourceTypeIDForDBTest(t, db)),
		PropertyOptionsRepository:       service.NewPropertyOptionsRepository(db),
	}.WithTransactions(db)
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
	assert.Equal(t, before.GetAttributes().CreateTimeSinceEpoch, after.GetAttributes().CreateTimeSinceEpoch)
	versionAfter, err := services.ServingRuntimeVersionRepository.GetByName("first:vllm:1")
	require.NoError(t, err)
	assert.Equal(t, versionBefore.GetID(), versionAfter.GetID())
	assert.Equal(t, versionBefore.GetAttributes().CreateTimeSinceEpoch, versionAfter.GetAttributes().CreateTimeSinceEpoch)
	apiRuntime, err = NewDBServingRuntimeCatalog(services, loader.Sources).GetServingRuntime(t.Context(), strconv.FormatInt(int64(*after.GetID()), 10))
	require.NoError(t, err)
	assert.Nil(t, apiRuntime.DisplayName)
	assert.Empty(t, apiRuntime.CustomProperties)
	versionAPI, err = NewDBServingRuntimeCatalog(services, loader.Sources).ListServingRuntimeVersions(t.Context(), strconv.FormatInt(int64(*after.GetID()), 10), ListServingRuntimeVersionsParams{})
	require.NoError(t, err)
	require.Len(t, versionAPI.Items, 1)
	assert.Equal(t, "example:new", versionAPI.Items[0].Image)
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

func TestServingRuntimeVersionTemplatesRoundTrip(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "runtimes.yaml")
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - {id: first, type: yaml, properties: {yamlCatalogPath: runtimes.yaml}}\n")
	writeRuntimeFile(t, dataPath, "serving_runtimes:\n  - name: vllm\n    versions:\n      - version: '1'\n        image: example:v1\n        servingRuntimeTemplate: '{\"kind\":\"ServingRuntime\"}'\n        llmInferenceServiceConfig: '{\"apiVersion\":\"serving.kserve.io/v1alpha1\"}'\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"]))
	runtime, err := services.ServingRuntimeRepository.GetByName("first:vllm")
	require.NoError(t, err)

	versions, err := NewDBServingRuntimeCatalog(services, loader.Sources).ListServingRuntimeVersions(t.Context(), strconv.FormatInt(int64(*runtime.GetID()), 10), ListServingRuntimeVersionsParams{})
	require.NoError(t, err)
	require.Len(t, versions.Items, 1)
	require.NotNil(t, versions.Items[0].LlmInferenceServiceConfig)
	assert.Equal(t, `{"apiVersion":"serving.kserve.io/v1alpha1"}`, *versions.Items[0].LlmInferenceServiceConfig)
	encoded, err := json.Marshal(versions.Items[0])
	require.NoError(t, err)
	var response map[string]any
	require.NoError(t, json.Unmarshal(encoded, &response))
	assert.Equal(t, `{"kind":"ServingRuntime"}`, response["servingRuntimeTemplate"])
	assert.Equal(t, `{"apiVersion":"serving.kserve.io/v1alpha1"}`, response["llmInferenceServiceConfig"])
	assert.NotContains(t, response, "template")
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

// TestServingRuntimeLoaderDoLoadIgnoresLeadershipLoss verifies that losing
// leadership mid-reload does not overwrite a healthy source status with
// SourceStatusError. A node that just lost leadership is no longer
// authoritative over the source's status - the new leader is.
func TestServingRuntimeLoaderDoLoadIgnoresLeadershipLoss(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "runtimes.yaml")
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - {id: first, type: yaml, properties: {yamlCatalogPath: runtimes.yaml}}\n")
	writeRuntimeFile(t, dataPath, "serving_runtimes:\n  - name: vllm\n    versions: [{version: '1', image: example:v1}]\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	source := loader.Sources.AllSources()["first"]

	state.SetLeader(true)
	loader.doLoad(t.Context(), "first", source)
	status, err := services.CatalogSourceRepository.GetStatus("first")
	require.NoError(t, err)
	assert.Equal(t, basecatalog.SourceStatusAvailable, status.Status)

	state.SetLeader(false)
	loader.doLoad(t.Context(), "first", source)
	status, err = services.CatalogSourceRepository.GetStatus("first")
	require.NoError(t, err)
	assert.Equal(t, basecatalog.SourceStatusAvailable, status.Status, "losing leadership should not overwrite a healthy source status with an error")
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
		return loader.removeOrphanedRuntimes(t.Context(), sourceID, valid)
	})

	list, err := services.ServingRuntimeRepository.List(&models.ServingRuntimeListOptions{SourceIDs: &[]string{sourceID}})
	require.NoError(t, err)
	assert.Len(t, list.Items, total, "no valid runtimes should have been removed")
}

// TestServingRuntimeUnnamedEntryDoesNotBlockOrphanCleanup verifies that a single
// unnamed (and thus rejected) entry doesn't suppress orphan cleanup for the rest of
// a successfully-loaded source: a runtime dropped from the file must still be
// removed even though an unrelated entry in the same file has a blank name.
func TestServingRuntimeUnnamedEntryDoesNotBlockOrphanCleanup(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	path := filepath.Join(t.TempDir(), "runtimes.yaml")
	state := basecatalog.NewBaseLoader(nil)
	state.SetLeader(true)
	loader := NewServingRuntimeLoader(services, state)
	source := basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}}

	writeRuntimeFile(t, path, "serving_runtimes:\n  - name: keep\n  - name: removed\n")
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", source))
	_, err := services.ServingRuntimeRepository.GetByName("first:removed")
	require.NoError(t, err)

	// Reload with "removed" dropped from the file and an unnamed entry present.
	writeRuntimeFile(t, path, "serving_runtimes:\n  - name: keep\n  - name: ''\n")
	require.Error(t, loader.loadFromYAML(t.Context(), "first", source), "the unnamed entry is rejected")

	_, err = services.ServingRuntimeRepository.GetByName("first:keep")
	require.NoError(t, err, "successfully-loaded runtime must remain")
	_, err = services.ServingRuntimeRepository.GetByName("first:removed")
	require.ErrorIs(t, err, runtimeservice.ErrServingRuntimeNotFound,
		"runtime dropped from the file must be cleaned up despite the unrelated unnamed entry")
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

func TestServingRuntimeListSearchesMetadata(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    type: yaml\n    properties: {yamlCatalogPath: runtimes.yaml}\n")
	writeRuntimeFile(t, filepath.Join(dir, "runtimes.yaml"), "serving_runtimes:\n  - name: vllm\n    displayName: Fast Engine\n    provider: Acme\n    description: Runs large language models\n  - name: ovms\n    description: CPU inference\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"]))
	provider := NewDBServingRuntimeCatalog(services, loader.Sources)

	for _, query := range []string{"VLLM", "FAST", "ACME", "LANGUAGE"} {
		result, err := provider.ListServingRuntimes(t.Context(), ListServingRuntimesParams{Query: query})
		require.NoError(t, err)
		require.Len(t, result.Items, 1, "query %q", query)
		assert.Equal(t, "vllm", *result.Items[0].Name)
	}
}

func TestServingRuntimeFilterOptionsIncludePropertiesAndNamedQueries(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    type: yaml\n    properties: {yamlCatalogPath: runtimes.yaml}\nnamedQueries:\n  acme_only:\n    assetType: serving_runtimes\n    filters:\n      provider: {operator: '=', value: Acme}\n")
	writeRuntimeFile(t, filepath.Join(dir, "runtimes.yaml"), "serving_runtimes:\n  - name: vllm\n    provider: Acme\n    tags: [llm]\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"]))
	require.NoError(t, services.PropertyOptionsRepository.Refresh(dbmodels.ContextPropertyOptionType))

	options, err := NewDBServingRuntimeCatalog(services, loader.Sources).GetFilterOptions(t.Context())
	require.NoError(t, err)
	require.NotNil(t, options.Filters)
	assert.Contains(t, *options.Filters, "provider")
	assert.NotContains(t, *options.Filters, "source_id")
	require.NotNil(t, options.NamedQueries)
	assert.Contains(t, *options.NamedQueries, "acme_only")
}

func TestServingRuntimeStructuredFieldsFilterByValues(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    type: yaml\n    properties: {yamlCatalogPath: runtimes.yaml}\n")
	writeRuntimeFile(t, filepath.Join(dir, "runtimes.yaml"), `serving_runtimes:
  - name: ovms
    supportedModelFormats:
      - {name: onnx, autoSelect: true, priority: 2}
    capabilities:
      requiresGPU: false
      multiModel: true
      supportedAccelerators: [intel.com/gaudi]
    versions:
      - version: "1"
        image: example:1
        supportedModelFormats: [{name: onnx, version: "2"}]
        env: [{name: MODEL_PATH, required: true}]
  - name: vllm
    supportedModelFormats: [{name: safetensors}]
    capabilities: {requiresGPU: true, multiModel: false}
    versions:
      - version: "1"
        image: example:2
`)
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"]))
	provider := NewDBServingRuntimeCatalog(services, loader.Sources)

	for _, query := range []string{
		"supportedModelFormats = 'onnx'",
		"capabilities.requiresGPU = false",
		"capabilities.multiModel = true",
		"capabilities.supportedAccelerators = 'intel.com/gaudi'",
	} {
		result, err := provider.ListServingRuntimes(t.Context(), ListServingRuntimesParams{FilterQuery: query})
		require.NoError(t, err, query)
		require.Len(t, result.Items, 1, query)
		assert.Equal(t, "ovms", *result.Items[0].Name, query)
		assert.Equal(t, "onnx", result.Items[0].SupportedModelFormats[0].Name, query)
		assert.Equal(t, int32(2), *result.Items[0].SupportedModelFormats[0].Priority, query)
	}

	require.NoError(t, services.PropertyOptionsRepository.Refresh(dbmodels.ContextPropertyOptionType))
	options, err := provider.GetFilterOptions(t.Context())
	require.NoError(t, err)
	require.NotNil(t, options.Filters)
	assert.Equal(t, []any{"onnx", "safetensors"}, (*options.Filters)["supportedModelFormats"].Values)
	assert.Equal(t, []any{"intel.com/gaudi"}, (*options.Filters)["capabilities.supportedAccelerators"].Values)
	assert.Equal(t, "boolean", (*options.Filters)["capabilities.requiresGPU"].Type)
	assert.Equal(t, []any{false, true}, (*options.Filters)["capabilities.requiresGPU"].Values)
	assert.NotContains(t, *options.Filters, "supportedModelFormatsDetails")
	assert.NotContains(t, *options.Filters, "capabilities")

	ovms, err := services.ServingRuntimeRepository.GetByName("first:ovms")
	require.NoError(t, err)
	for _, query := range []string{"supportedModelFormats = 'onnx'", "env = 'MODEL_PATH'"} {
		versions, err := provider.ListServingRuntimeVersions(t.Context(), strconv.FormatInt(int64(*ovms.GetID()), 10), ListServingRuntimeVersionsParams{FilterQuery: query})
		require.NoError(t, err, query)
		require.Len(t, versions.Items, 1, query)
		assert.Equal(t, "onnx", versions.Items[0].SupportedModelFormats[0].Name, query)
		assert.Equal(t, "2", *versions.Items[0].SupportedModelFormats[0].Version, query)
		assert.Equal(t, "MODEL_PATH", versions.Items[0].Env[0].Name, query)
		assert.True(t, *versions.Items[0].Env[0].Required, query)
	}
}

func TestServingRuntimeCapabilityDefaultsToFalse(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    type: yaml\n    properties: {yamlCatalogPath: runtimes.yaml}\n")
	writeRuntimeFile(t, filepath.Join(dir, "runtimes.yaml"), `serving_runtimes:
  - name: cpu-only
    versions:
      - version: "1"
        image: example:1
  - name: partial-caps
    capabilities:
      multiModel: true
    versions:
      - version: "1"
        image: example:2
`)
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"]))
	provider := NewDBServingRuntimeCatalog(services, loader.Sources)

	// Both runtimes omit capabilities.requiresGPU (or capabilities entirely), so it
	// must default to false rather than being absent from the property set.
	result, err := provider.ListServingRuntimes(t.Context(), ListServingRuntimesParams{FilterQuery: "capabilities.requiresGPU = false"})
	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	names := []string{*result.Items[0].Name, *result.Items[1].Name}
	assert.ElementsMatch(t, []string{"cpu-only", "partial-caps"}, names)

	// cpu-only also omits multiModel entirely, so it must default to false.
	result, err = provider.ListServingRuntimes(t.Context(), ListServingRuntimesParams{FilterQuery: "capabilities.multiModel = false"})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "cpu-only", *result.Items[0].Name)
}

func TestServingRuntimeSourceFilterIgnoresCustomSourceID(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    type: yaml\n    properties: {yamlCatalogPath: runtimes.yaml}\n")
	writeRuntimeFile(t, filepath.Join(dir, "runtimes.yaml"), "serving_runtimes: []\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)

	// A "source_id" customProperties key is rejected at YAML ingestion (see
	// TestServingRuntimeLoaderRejectsReservedCustomProperties), so save the
	// runtime directly through the repository to exercise the SourceIDs list
	// filter's handling of a same-named custom property.
	name := "first:vllm"
	_, err := services.ServingRuntimeRepository.Save(&models.ServingRuntimeImpl{
		Attributes: &models.ServingRuntimeAttributes{Name: &name},
		Properties: &[]mrmodels.Properties{
			mrmodels.NewStringProperty("source_id", "first", false),
		},
		CustomProperties: &[]mrmodels.Properties{
			{Name: "source_id", IsCustomProperty: true, StringValue: new("second")},
		},
	})
	require.NoError(t, err)

	result, err := NewDBServingRuntimeCatalog(services, loader.Sources).ListServingRuntimes(t.Context(), ListServingRuntimesParams{SourceIDs: []string{"second"}})
	require.NoError(t, err)
	assert.Empty(t, result.Items)
}

// TestServingRuntimeLoaderRejectsReservedCustomProperties ensures that
// "source_id" and "base_name" cannot be set via customProperties. Both are
// internal bookkeeping properties written by the loader as non-custom
// properties; allowing a source to override them via a custom property of
// the same name would let it impersonate another source_id or base_name,
// corrupting source-ownership queries (DeleteBySource, GetDistinctSourceIDs).
func TestServingRuntimeLoaderRejectsReservedCustomProperties(t *testing.T) {
	for _, key := range []string{"source_id", "base_name"} {
		t.Run(key, func(t *testing.T) {
			_, services := setupServingRuntimeLoader(t)
			dir := t.TempDir()
			dataPath := filepath.Join(dir, "runtimes.yaml")
			configPath := filepath.Join(dir, "sources.yaml")
			writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    type: yaml\n    properties:\n      yamlCatalogPath: runtimes.yaml\n")
			writeRuntimeFile(t, dataPath, fmt.Sprintf("serving_runtimes:\n  - name: vllm\n    customProperties:\n      %s: {metadataType: MetadataStringValue, string_value: other}\n", key))
			state := basecatalog.NewBaseLoader([]string{configPath})
			loader := NewServingRuntimeLoader(services, state)
			require.NoError(t, loader.ParseAllConfigs())
			state.SetLeader(true)

			err := loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"])
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
			_, err = services.ServingRuntimeRepository.GetByName("first:vllm")
			require.Error(t, err, "the runtime must not be persisted when a reserved custom property is rejected")
		})
	}
}

// TestServingRuntimeRemoveRuntimesFromMissingSourcesIgnoresCustomSourceID
// reproduces the phantom-source scenario reported against DeleteBySource /
// GetDistinctSourceIDs directly at the repository layer (bypassing the
// loader's reserved-property validation, which blocks this from happening via
// YAML ingestion — see TestServingRuntimeLoaderRejectsReservedCustomProperties).
// A runtime that has a *custom* property named "source_id" must not be
// treated as belonging to that value when the leader reconciles sources.
// Before source-ownership queries required is_custom_property = false, such a
// runtime made GetDistinctSourceIDs report a phantom source not present in
// the enabled set, and removeRuntimesFromMissingSources would then call
// DeleteBySource("phantom"), deleting the runtime out from under its real
// source on every leader reconcile pass.
func TestServingRuntimeRemoveRuntimesFromMissingSourcesIgnoresCustomSourceID(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    type: yaml\n    properties: {yamlCatalogPath: runtimes.yaml}\n")
	writeRuntimeFile(t, filepath.Join(dir, "runtimes.yaml"), "serving_runtimes: []\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)

	// Save the runtime directly through the repository (bypassing loadFromYAML)
	// with a real, non-custom source_id of "first" plus a custom property that
	// also happens to be named "source_id" pointing at a phantom source.
	name := "first:vllm"
	_, err := services.ServingRuntimeRepository.Save(&models.ServingRuntimeImpl{
		Attributes: &models.ServingRuntimeAttributes{Name: &name},
		Properties: &[]mrmodels.Properties{
			mrmodels.NewStringProperty("source_id", "first", false),
		},
		CustomProperties: &[]mrmodels.Properties{
			{Name: "source_id", IsCustomProperty: true, StringValue: new("phantom")},
		},
	})
	require.NoError(t, err)

	require.NoError(t, loader.removeRuntimesFromMissingSources(mapset.NewSet("first")))

	_, err = services.ServingRuntimeRepository.GetByName(name)
	require.NoError(t, err, "runtime must survive: DeleteBySource must not match a custom source_id property")
}

func TestServingRuntimeListsFilterAndPaginate(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    type: yaml\n    properties: {yamlCatalogPath: runtimes.yaml}\n")
	writeRuntimeFile(t, filepath.Join(dir, "runtimes.yaml"), "serving_runtimes:\n  - name: vllm\n    provider: Acme\n    versions: [{version: '1', image: vllm:1}, {version: '2', image: vllm:2}]\n  - name: ovms\n    provider: Acme\n    versions: [{version: '2', image: ovms:2}]\n  - name: mlserver\n    provider: Other\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"]))
	provider := NewDBServingRuntimeCatalog(services, loader.Sources)

	params := ListServingRuntimesParams{SourceIDs: []string{"first"}, FilterQuery: "provider = 'Acme'", PageSize: 1, OrderBy: openapi.ORDERBYFIELD_NAME, SortOrder: openapi.SORTORDER_ASC}
	first, err := provider.ListServingRuntimes(t.Context(), params)
	require.NoError(t, err)
	require.Len(t, first.Items, 1)
	assert.Equal(t, "ovms", *first.Items[0].Name)
	require.NotEmpty(t, first.NextPageToken)
	params.NextPageToken = first.NextPageToken
	second, err := provider.ListServingRuntimes(t.Context(), params)
	require.NoError(t, err)
	require.Len(t, second.Items, 1)
	assert.Equal(t, "vllm", *second.Items[0].Name)
	assert.Empty(t, second.NextPageToken)

	versions, err := provider.ListServingRuntimeVersions(t.Context(), *second.Items[0].Id, ListServingRuntimeVersionsParams{FilterQuery: "version = '2'", PageSize: 1})
	require.NoError(t, err)
	require.Len(t, versions.Items, 1)
	assert.Equal(t, "vllm:2", *versions.Items[0].Name)
	assert.Equal(t, "vllm:2", versions.Items[0].Image)
}

func TestServingRuntimeNameOrderingUsesUnqualifiedNameAcrossSources(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    type: yaml\n    properties: {yamlCatalogPath: first.yaml}\n  - id: second\n    type: yaml\n    properties: {yamlCatalogPath: second.yaml}\n")
	writeRuntimeFile(t, filepath.Join(dir, "first.yaml"), "serving_runtimes:\n  - name: zulu\n  - name: alpha\n")
	writeRuntimeFile(t, filepath.Join(dir, "second.yaml"), "serving_runtimes:\n  - name: bravo\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)
	for id, source := range loader.Sources.AllSources() {
		require.NoError(t, loader.loadFromYAML(t.Context(), id, source))
	}
	provider := NewDBServingRuntimeCatalog(services, loader.Sources)

	params := ListServingRuntimesParams{PageSize: 2, OrderBy: openapi.ORDERBYFIELD_NAME, SortOrder: openapi.SORTORDER_ASC}
	first, err := provider.ListServingRuntimes(t.Context(), params)
	require.NoError(t, err)
	require.Len(t, first.Items, 2)
	assert.Equal(t, "alpha", *first.Items[0].Name)
	assert.Equal(t, "bravo", *first.Items[1].Name)
	require.NotEmpty(t, first.NextPageToken)

	params.NextPageToken = first.NextPageToken
	second, err := provider.ListServingRuntimes(t.Context(), params)
	require.NoError(t, err)
	require.Len(t, second.Items, 1)
	assert.Equal(t, "zulu", *second.Items[0].Name)
	assert.Empty(t, second.NextPageToken)
}

func TestServingRuntimeVersionDefaultsDeprecatedToFalse(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - id: first\n    type: yaml\n    properties: {yamlCatalogPath: runtimes.yaml}\n")
	writeRuntimeFile(t, filepath.Join(dir, "runtimes.yaml"), "serving_runtimes:\n  - name: vllm\n    versions:\n      - {version: '1', image: vllm:1}\n      - {version: '2', image: vllm:2, deprecated: true}\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	require.NoError(t, loader.ParseAllConfigs())
	state.SetLeader(true)
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", loader.Sources.AllSources()["first"]))
	runtime, err := services.ServingRuntimeRepository.GetByName("first:vllm")
	require.NoError(t, err)

	versions, err := NewDBServingRuntimeCatalog(services, loader.Sources).ListServingRuntimeVersions(
		t.Context(), strconv.FormatInt(int64(*runtime.GetID()), 10), ListServingRuntimeVersionsParams{FilterQuery: "deprecated = false"},
	)
	require.NoError(t, err)
	require.Len(t, versions.Items, 1)
	assert.Equal(t, "1", versions.Items[0].Version)
	require.NotNil(t, versions.Items[0].Deprecated)
	assert.False(t, *versions.Items[0].Deprecated)
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
		return loader.removeOrphanedVersions(t.Context(), services.ServingRuntimeVersionRepository, *runtime.GetID(), valid)
	})

	assert.Len(t, runtimeVersions(t, services, *runtime.GetID()), total, "no valid versions should have been removed")
}

func TestServingRuntimeLoaderRejectsColonInSourceID(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "runtimes.yaml")
	configPath := filepath.Join(dir, "sources.yaml")
	writeRuntimeFile(t, dataPath, "serving_runtimes:\n  - name: vllm\n    versions: [{version: '1', image: example:v1}]\n")
	writeRuntimeFile(t, configPath, "serving_runtime_catalogs:\n  - {id: 'rh:prod', type: yaml, properties: {yamlCatalogPath: runtimes.yaml}}\n")
	state := basecatalog.NewBaseLoader([]string{configPath})
	loader := NewServingRuntimeLoader(services, state)
	err := loader.ParseAllConfigs()
	require.Error(t, err, "a source id containing ':' must be rejected, since stored names are qualified as sourceID:name")
	assert.Contains(t, err.Error(), "rh:prod")
}

// A rejected entry must not prevent either neighboring runtime from loading,
// mutate the rejected runtime, or protect genuinely absent runtimes from cleanup.
func TestServingRuntimePartialLoadPreservesRejectedEntries(t *testing.T) {
	for _, tc := range []struct {
		name, rejected string
	}{
		{"invalid version", "  - name: rejected\n    displayName: changed\n    versions: [{version: 'new', image: new:1}, {version: 'bad'}]\n"},
		{"duplicate runtime", "  - name: rejected\n    displayName: changed\n  - name: rejected\n"},
		{"duplicate version", "  - name: rejected\n    versions: [{version: '1', image: new:1}, {version: '1', image: new:2}]\n"},
		{"integer overflow", "  - name: rejected\n    customProperties: {tokens: {metadataType: MetadataIntValue, int_value: '3000000000'}}\n"},
		{"reserved property", "  - name: rejected\n    customProperties: {source_id: {metadataType: MetadataStringValue, string_value: other}}\n"},
		// An entry with no name at all ("unidentified") isn't covered here: unlike
		// these cases it leaves no "rejected"-named entry in the reloaded file, so
		// "first:rejected" is correctly cleaned up as a genuine orphan rather than
		// retained. See TestServingRuntimeUnnamedEntryDoesNotBlockOrphanCleanup.
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, services := setupServingRuntimeLoader(t)
			path := filepath.Join(t.TempDir(), "runtimes.yaml")
			state := basecatalog.NewBaseLoader(nil)
			state.SetLeader(true)
			loader := NewServingRuntimeLoader(services, state)
			source := basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}}
			writeRuntimeFile(t, path, "serving_runtimes:\n  - name: rejected\n    displayName: original\n    versions: [{version: '1', image: old:1}, {version: '2', image: old:2}]\n  - name: absent\n")
			require.NoError(t, loader.loadFromYAML(t.Context(), "first", source))
			before, err := services.ServingRuntimeRepository.GetByName("first:rejected")
			require.NoError(t, err)
			writeRuntimeFile(t, path, "serving_runtimes:\n  - name: before\n"+tc.rejected+"  - name: after\n")
			outcome, err := loader.loadYAML(t.Context(), "first", source)
			require.NoError(t, err, "individual rejections are reported in the load outcome")
			assert.Equal(t, 2, outcome.SuccessfulRuntimes)
			assert.True(t, outcome.Changed)
			assert.NotEmpty(t, outcome.Diagnostics)
			if tc.name == "duplicate runtime" {
				assert.Equal(t, []string{"first:rejected", "first:rejected"}, outcome.FailedRuntimeIDs)
			} else {
				assert.Equal(t, []string{"first:rejected"}, outcome.FailedRuntimeIDs)
			}
			for _, name := range []string{"before", "after"} {
				_, err := services.ServingRuntimeRepository.GetByName("first:" + name)
				require.NoError(t, err, "valid runtime %s must load", name)
			}
			retained, err := services.ServingRuntimeRepository.GetByName("first:rejected")
			require.NoError(t, err)
			assert.Equal(t, before.GetID(), retained.GetID())
			assert.Equal(t, before.GetProperties(), retained.GetProperties())
			assert.Len(t, runtimeVersions(t, services, *retained.GetID()), 2)
			_, err = services.ServingRuntimeRepository.GetByName("first:absent")
			require.ErrorIs(t, err, runtimeservice.ErrServingRuntimeNotFound)
		})
	}
}

func TestServingRuntimeMalformedAndEmptyCatalogCleanup(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	path := filepath.Join(t.TempDir(), "runtimes.yaml")
	state := basecatalog.NewBaseLoader(nil)
	state.SetLeader(true)
	loader := NewServingRuntimeLoader(services, state)
	source := basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}}
	writeRuntimeFile(t, path, "serving_runtimes:\n  - name: retained\n    versions: [{version: '1', image: old:1}]\n")
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", source))
	for _, bad := range []string{"serving_runtimes: [", "serving_runtimes:\n  - name: invalid\n    versions: [{version: '1'}]\n"} {
		writeRuntimeFile(t, path, bad)
		require.Error(t, loader.loadFromYAML(t.Context(), "first", source))
		_, err := services.ServingRuntimeRepository.GetByName("first:retained")
		require.NoError(t, err)
	}
	missingSource := basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: filepath.Join(t.TempDir(), "missing.yaml")}}
	require.Error(t, loader.loadFromYAML(t.Context(), "first", missingSource))
	_, err := services.ServingRuntimeRepository.GetByName("first:retained")
	require.NoError(t, err)
	writeRuntimeFile(t, path, "serving_runtimes: []\n")
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", source))
	_, err = services.ServingRuntimeRepository.GetByName("first:retained")
	require.ErrorIs(t, err, runtimeservice.ErrServingRuntimeNotFound)
}

// TestServingRuntimeMistypedKeyDoesNotWipe guards against a non-strict YAML decode
// silently treating a missing/mistyped top-level key as an intentional empty catalog,
// which would otherwise delete every runtime for the source (see loadYAML's cleanup gate).
func TestServingRuntimeMistypedKeyDoesNotWipe(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	path := filepath.Join(t.TempDir(), "runtimes.yaml")
	state := basecatalog.NewBaseLoader(nil)
	state.SetLeader(true)
	loader := NewServingRuntimeLoader(services, state)
	source := basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}}
	writeRuntimeFile(t, path, "serving_runtimes:\n  - name: retained\n    versions: [{version: '1', image: old:1}]\n")
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", source))

	for _, bad := range []string{
		"servingRuntimes:\n  - name: vllm\n    versions: [{version: '1', image: example:v1}]\n", // mistyped/mis-cased key
		"",              // empty file
		"source: foo\n", // key entirely absent
	} {
		writeRuntimeFile(t, path, bad)
		require.Error(t, loader.loadFromYAML(t.Context(), "first", source))
		_, err := services.ServingRuntimeRepository.GetByName("first:retained")
		require.NoError(t, err, "a missing/mistyped top-level key must not wipe existing runtimes")
	}

	// An explicit empty list is still an intentional wipe.
	writeRuntimeFile(t, path, "serving_runtimes: []\n")
	require.NoError(t, loader.loadFromYAML(t.Context(), "first", source))
	_, err := services.ServingRuntimeRepository.GetByName("first:retained")
	require.ErrorIs(t, err, runtimeservice.ErrServingRuntimeNotFound)
}

func TestServingRuntimeWatcherPartialFailureAndRecovery(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	path := filepath.Join(t.TempDir(), "runtimes.yaml")
	state := basecatalog.NewBaseLoader(nil)
	state.SetLeader(true)
	loader := NewServingRuntimeLoader(services, state)
	source := basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}}
	writeRuntimeFile(t, path, "serving_runtimes:\n  - name: good\n    versions: [{version: '1', image: partial:1}]\n  - name: bad\n    versions: [{version: '1'}]\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	state.TrackWrite()
	done := make(chan struct{})
	go func() { defer close(done); loader.watchAndLoadFromYAML(ctx, "watched", source) }()
	t.Cleanup(func() { cancel(); <-done })
	waitStatus := func(want string) {
		t.Helper()
		require.Eventually(t, func() bool {
			status, err := services.CatalogSourceRepository.GetStatus("watched")
			return err == nil && status.Status == want
		}, 15*time.Second, 50*time.Millisecond)
	}
	waitStatus(basecatalog.SourceStatusPartiallyAvailable)
	require.Eventually(t, func() bool {
		options, err := services.PropertyOptionsRepository.List(dbmodels.ArtifactPropertyOptionType, services.ServingRuntimeVersionRepository.GetTypeID())
		if err != nil {
			return false
		}
		for _, option := range options {
			if option.Name == "image" && len(option.StringValue) == 1 && option.StringValue[0] == "partial:1" {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond)
	writeRuntimeFile(t, path, "serving_runtimes:\n  - name: bad\n    versions: [{version: '1'}]\n")
	waitStatus(basecatalog.SourceStatusError)
	_, err := services.ServingRuntimeRepository.GetByName("watched:good")
	require.NoError(t, err, "complete failure must suppress source cleanup")
	writeRuntimeFile(t, path, "serving_runtimes:\n  - name: good\n  - name: bad\n")
	waitStatus(basecatalog.SourceStatusAvailable)
	status, err := services.CatalogSourceRepository.GetStatus("watched")
	require.NoError(t, err)
	assert.Empty(t, status.Error)
	writeRuntimeFile(t, path, "serving_runtimes: []\n")
	require.Eventually(t, func() bool {
		_, err := services.ServingRuntimeRepository.GetByName("watched:good")
		return errors.Is(err, runtimeservice.ErrServingRuntimeNotFound)
	}, 15*time.Second, 50*time.Millisecond)
	waitStatus(basecatalog.SourceStatusAvailable)
}

// bindRuntimeTransactionRepositories keeps failure injection inside real database transactions.
func bindRuntimeTransactionRepositories(services Services) Services {
	run := services.WithRuntimeFamilyTransaction
	services.WithRuntimeFamilyTransaction = func(ctx context.Context, operation func(models.ServingRuntimeRepository, models.ServingRuntimeVersionRepository) error) error {
		return run(ctx, func(runtimeRepo models.ServingRuntimeRepository, versionRepo models.ServingRuntimeVersionRepository) error {
			if failing, ok := services.ServingRuntimeRepository.(failingRuntimeRepository); ok {
				failing.ServingRuntimeRepository = runtimeRepo
				runtimeRepo = failing
			}
			if failing, ok := services.ServingRuntimeVersionRepository.(failingVersionRepository); ok {
				failing.ServingRuntimeVersionRepository = versionRepo
				versionRepo = failing
			}
			return operation(runtimeRepo, versionRepo)
		})
	}
	return services
}

type failingRuntimeRepository struct {
	models.ServingRuntimeRepository
	lookupName string
	saveName   string
	afterSave  func()
	failList   bool
}

func (r failingRuntimeRepository) GetByName(name string) (models.ServingRuntime, error) {
	if name == r.lookupName {
		return nil, fmt.Errorf("injected runtime lookup failure")
	}
	return r.ServingRuntimeRepository.GetByName(name)
}
func (r failingRuntimeRepository) Save(entity models.ServingRuntime) (models.ServingRuntime, error) {
	if *entity.GetAttributes().Name == r.saveName {
		return nil, fmt.Errorf("injected runtime save failure")
	}
	saved, err := r.ServingRuntimeRepository.Save(entity)
	if r.afterSave != nil {
		r.afterSave()
	}
	return saved, err
}
func (r failingRuntimeRepository) List(options *models.ServingRuntimeListOptions) (*mrmodels.ListWrapper[models.ServingRuntime], error) {
	if r.failList {
		return nil, fmt.Errorf("injected cleanup failure")
	}
	return r.ServingRuntimeRepository.List(options)
}

type failingVersionRepository struct {
	models.ServingRuntimeVersionRepository
	lookupName   string
	saveName     string
	failParentID int32
	failDeleteID int32
	afterList    func()
}

func (r failingVersionRepository) GetByName(name string) (models.ServingRuntimeVersion, error) {
	if name == r.lookupName {
		return nil, fmt.Errorf("injected version lookup failure")
	}
	return r.ServingRuntimeVersionRepository.GetByName(name)
}
func (r failingVersionRepository) Save(entity models.ServingRuntimeVersion, parentID *int32) (models.ServingRuntimeVersion, error) {
	if *entity.GetAttributes().Name == r.saveName {
		return nil, fmt.Errorf("injected version save failure")
	}
	return r.ServingRuntimeVersionRepository.Save(entity, parentID)
}

func (r failingVersionRepository) List(options *models.ServingRuntimeVersionListOptions) (*mrmodels.ListWrapper[models.ServingRuntimeVersion], error) {
	if options.ParentResourceID != nil && *options.ParentResourceID == r.failParentID {
		return nil, errors.New("injected version cleanup list failure")
	}
	list, err := r.ServingRuntimeVersionRepository.List(options)
	if r.afterList != nil {
		r.afterList()
	}
	return list, err
}

func (r failingVersionRepository) DeleteByID(id int32) error {
	if id == r.failDeleteID {
		return errors.New("injected version cleanup delete failure")
	}
	return r.ServingRuntimeVersionRepository.DeleteByID(id)
}

func TestServingRuntimePersistenceFailuresContinueAndPreserveFailedEntries(t *testing.T) {
	for _, failure := range []string{"runtime lookup", "runtime save", "version lookup", "version save", "version cleanup list", "version cleanup delete"} {
		t.Run(failure, func(t *testing.T) {
			db, services := setupServingRuntimeLoader(t)
			path := filepath.Join(t.TempDir(), "runtimes.yaml")
			state := basecatalog.NewBaseLoader(nil)
			state.SetLeader(true)
			loader := NewServingRuntimeLoader(services, state)
			source := basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}}
			writeRuntimeFile(t, path, "serving_runtimes:\n  - name: failed\n    displayName: Original\n    customProperties: {owner: {metadataType: MetadataStringValue, string_value: original}}\n    versions: [{version: 'old', image: old:1}, {version: 'stale', image: old:2}, {version: 'stale2', image: old:3}]\n  - name: absent\n")
			require.NoError(t, loader.loadFromYAML(t.Context(), "first", source))
			failed, err := services.ServingRuntimeRepository.GetByName("first:failed")
			require.NoError(t, err)
			before := snapshotRuntimeFamily(t, db, *failed.GetID())
			runtimeRepo := failingRuntimeRepository{ServingRuntimeRepository: services.ServingRuntimeRepository}
			versionRepo := failingVersionRepository{ServingRuntimeVersionRepository: services.ServingRuntimeVersionRepository}
			switch failure {
			case "runtime lookup":
				runtimeRepo.lookupName = "first:failed"
			case "runtime save":
				runtimeRepo.saveName = "first:failed"
			case "version lookup":
				versionRepo.lookupName = "first:failed:broken"
			case "version save":
				versionRepo.saveName = "first:failed:broken"
			case "version cleanup list":
				versionRepo.failParentID = *failed.GetID()
			case "version cleanup delete":
				version, err := services.ServingRuntimeVersionRepository.GetByName("first:failed:stale2")
				require.NoError(t, err)
				versionRepo.failDeleteID = *version.GetID()
			}
			loader.services.ServingRuntimeRepository = runtimeRepo
			loader.services.ServingRuntimeVersionRepository = versionRepo
			loader.services = bindRuntimeTransactionRepositories(loader.services)
			writeRuntimeFile(t, path, "serving_runtimes:\n  - name: before\n  - name: failed\n    displayName: Updated\n    versions: [{version: 'old', image: updated:1}, {version: 'new', image: new:1}, {version: 'broken', image: new:2}]\n  - name: after\n")
			outcome, err := loader.loadYAML(t.Context(), "first", source)
			require.NoError(t, err)
			assert.Equal(t, 2, outcome.SuccessfulRuntimes)
			assert.Equal(t, []string{"first:failed"}, outcome.FailedRuntimeIDs)
			assert.Equal(t, basecatalog.SourceStatusPartiallyAvailable, outcome.status(err))
			assert.Equal(t, before, snapshotRuntimeFamily(t, db, *failed.GetID()))
			_, err = services.ServingRuntimeRepository.GetByName("first:before")
			require.NoError(t, err)
			_, err = services.ServingRuntimeRepository.GetByName("first:after")
			require.NoError(t, err, "persistence errors must allow later runtimes to load")
			_, err = services.ServingRuntimeRepository.GetByName("first:failed")
			require.NoError(t, err)
			for _, name := range []string{"old", "stale", "stale2"} {
				_, err = services.ServingRuntimeVersionRepository.GetByName("first:failed:" + name)
				require.NoError(t, err, "failed runtimes must not clean up their existing versions")
			}
			versions := runtimeVersions(t, services, *failed.GetID())
			assert.Len(t, versions, 3, "failed families must roll back earlier version saves")
			_, err = services.ServingRuntimeVersionRepository.GetByName("first:failed:new")
			require.ErrorIs(t, err, runtimeservice.ErrServingRuntimeVersionNotFound)
			_, err = services.ServingRuntimeRepository.GetByName("first:absent")
			require.ErrorIs(t, err, runtimeservice.ErrServingRuntimeNotFound)
		})
	}
}

func TestServingRuntimeInterruptedLoadSkipsCleanup(t *testing.T) {
	for _, interruption := range []string{"cancellation", "leadership loss"} {
		t.Run(interruption, func(t *testing.T) {
			_, services := setupServingRuntimeLoader(t)
			path := filepath.Join(t.TempDir(), "runtimes.yaml")
			state := basecatalog.NewBaseLoader(nil)
			state.SetLeader(true)
			loader := NewServingRuntimeLoader(services, state)
			source := basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}}
			writeRuntimeFile(t, path, "serving_runtimes:\n  - name: absent\n    versions: [{version: '1', image: old:1}]\n")
			require.NoError(t, loader.loadFromYAML(t.Context(), "first", source))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			loader.services.ServingRuntimeRepository = failingRuntimeRepository{
				ServingRuntimeRepository: services.ServingRuntimeRepository,
				afterSave: func() {
					if interruption == "cancellation" {
						cancel()
					} else {
						state.SetLeader(false)
					}
				},
			}
			loader.services = bindRuntimeTransactionRepositories(loader.services)
			writeRuntimeFile(t, path, "serving_runtimes:\n  - name: current\n")
			require.Error(t, loader.loadFromYAML(ctx, "first", source), "interrupted loading must not report success")
			_, err := services.ServingRuntimeRepository.GetByName("first:absent")
			require.NoError(t, err)
			_, err = services.ServingRuntimeVersionRepository.GetByName("first:absent:1")
			require.NoError(t, err)
			_, err = services.ServingRuntimeRepository.GetByName("first:current")
			require.ErrorIs(t, err, runtimeservice.ErrServingRuntimeNotFound, "interruption must roll back a new family")
		})
	}
}

func TestServingRuntimeWatcherCleanupFailureReportsError(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	path := filepath.Join(t.TempDir(), "runtimes.yaml")
	writeRuntimeFile(t, path, "serving_runtimes:\n  - name: good\n")
	state := basecatalog.NewBaseLoader(nil)
	state.SetLeader(true)
	services.ServingRuntimeRepository = failingRuntimeRepository{ServingRuntimeRepository: services.ServingRuntimeRepository, failList: true}
	loader := NewServingRuntimeLoader(bindRuntimeTransactionRepositories(services), state)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	state.TrackWrite()
	done := make(chan struct{})
	go func() {
		defer close(done)
		loader.watchAndLoadFromYAML(ctx, "cleanup", basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}})
	}()
	t.Cleanup(func() { cancel(); <-done })
	state.WaitForInflightWrites(5 * time.Second)
	status, err := services.CatalogSourceRepository.GetStatus("cleanup")
	require.NoError(t, err)
	assert.Equal(t, basecatalog.SourceStatusError, status.Status)
	assert.Contains(t, status.Error, "cleanup failure")
	_, err = services.ServingRuntimeRepository.GetByName("cleanup:good")
	require.NoError(t, err, "cleanup failure must not roll back a committed runtime save")
}

func TestServingRuntimeWatcherInterruptionDoesNotPublishSuccess(t *testing.T) {
	for _, interruption := range []string{"cancellation", "leadership loss"} {
		t.Run(interruption, func(t *testing.T) {
			_, services := setupServingRuntimeLoader(t)
			path := filepath.Join(t.TempDir(), "runtimes.yaml")
			writeRuntimeFile(t, path, "serving_runtimes:\n  - name: good\n")
			state := basecatalog.NewBaseLoader(nil)
			state.SetLeader(true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			services.ServingRuntimeRepository = failingRuntimeRepository{
				ServingRuntimeRepository: services.ServingRuntimeRepository,
				afterSave: func() {
					if interruption == "cancellation" {
						cancel()
					} else {
						state.SetLeader(false)
					}
				},
			}
			basecatalog.SaveSourceStatus(services.CatalogSourceRepository, "interrupted", basecatalog.SourceStatusError, "previous failure")
			loader := NewServingRuntimeLoader(bindRuntimeTransactionRepositories(services), state)
			state.TrackWrite()
			done := make(chan struct{})
			go func() {
				defer close(done)
				loader.watchAndLoadFromYAML(ctx, "interrupted", basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}})
			}()
			t.Cleanup(func() { cancel(); <-done })
			state.WaitForInflightWrites(5 * time.Second)
			status, err := services.CatalogSourceRepository.GetStatus("interrupted")
			require.NoError(t, err)
			assert.Equal(t, basecatalog.SourceStatusError, status.Status)
			assert.Equal(t, "previous failure", status.Error)
			_, err = services.ServingRuntimeRepository.GetByName("interrupted:good")
			require.ErrorIs(t, err, runtimeservice.ErrServingRuntimeNotFound)
		})
	}
}

// flappingLeaderState wraps a BaseLoader so its ShouldWriteDatabase() reports a
// single transient "not leader" the first time it's called after arm() is set, then
// reverts to the real leader state on every subsequent call - simulating leadership
// lost and immediately regained within a single load.
type flappingLeaderState struct {
	*basecatalog.BaseLoader
	mu      sync.Mutex
	armed   bool
	tripped bool
}

func (s *flappingLeaderState) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
}

func (s *flappingLeaderState) ShouldWriteDatabase() bool {
	s.mu.Lock()
	if s.armed && !s.tripped {
		s.tripped = true
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	return s.BaseLoader.ShouldWriteDatabase()
}

// TestServingRuntimeWatcherLeadershipFlapDoesNotPublishStatus verifies that a
// transient leadership loss - regained before the watcher's post-load guard
// re-checks - doesn't overwrite a healthy status with a spurious "no longer leader"
// error, since nothing about the load actually failed.
func TestServingRuntimeWatcherLeadershipFlapDoesNotPublishStatus(t *testing.T) {
	_, services := setupServingRuntimeLoader(t)
	path := filepath.Join(t.TempDir(), "runtimes.yaml")
	writeRuntimeFile(t, path, "serving_runtimes:\n  - name: good\n")
	state := &flappingLeaderState{BaseLoader: basecatalog.NewBaseLoader(nil)}
	state.SetLeader(true)
	// Arm the flap after the runtime save, so the trip lands on the next
	// checkWrite (in the version loop) rather than before any work is done.
	services.ServingRuntimeRepository = failingRuntimeRepository{
		ServingRuntimeRepository: services.ServingRuntimeRepository,
		afterSave:                state.arm,
	}
	basecatalog.SaveSourceStatus(services.CatalogSourceRepository, "flapping", basecatalog.SourceStatusAvailable, "")
	loader := NewServingRuntimeLoader(bindRuntimeTransactionRepositories(services), state)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	state.TrackWrite()
	done := make(chan struct{})
	go func() {
		defer close(done)
		loader.watchAndLoadFromYAML(ctx, "flapping", basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}})
	}()
	t.Cleanup(func() { cancel(); <-done })
	state.WaitForInflightWrites(5 * time.Second)
	status, err := services.CatalogSourceRepository.GetStatus("flapping")
	require.NoError(t, err)
	assert.Equal(t, basecatalog.SourceStatusAvailable, status.Status, "a transient leadership flap must not overwrite the prior status")
	assert.Empty(t, status.Error, "a transient leadership flap must not record a spurious error")
	_, err = services.ServingRuntimeRepository.GetByName("flapping:good")
	require.ErrorIs(t, err, runtimeservice.ErrServingRuntimeNotFound, "leadership loss must roll back the active family")
}
