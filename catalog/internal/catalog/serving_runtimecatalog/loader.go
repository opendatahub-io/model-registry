package serving_runtimecatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/golang/glog"
	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	servingRuntimemodels "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	mrmodels "github.com/kubeflow/hub/internal/platform/db/entity"
)

// ServingRuntimeLoader handles loading serving_runtime data from YAML configuration files.
type ServingRuntimeLoader struct {
	state basecatalog.LoaderState

	Sources  *ServingRuntimeSourceCollection
	services Services

	closerMu sync.Mutex
	closer   func()
}

func (l *ServingRuntimeLoader) setCloser(closer func()) {
	l.closerMu.Lock()
	defer l.closerMu.Unlock()
	if l.closer != nil {
		l.closer()
	}
	l.closer = closer
}

func NewServingRuntimeLoader(services Services, state basecatalog.LoaderState) *ServingRuntimeLoader {
	paths := state.Paths()
	return &ServingRuntimeLoader{
		state:    state,
		Sources:  NewServingRuntimeSourceCollection(paths...),
		services: services,
	}
}

func (l *ServingRuntimeLoader) ParseAllConfigs() error {
	glog.Infof("Initializing %s loader - parsing configs", "serving_runtime")

	for _, path := range l.state.Paths() {
		if err := l.parseAndMerge(path); err != nil {
			return fmt.Errorf("failed to parse serving_runtime config %s: %w", path, err)
		}
	}

	glog.Infof("%s loader config parsing complete", "serving_runtime")
	return nil
}

func (l *ServingRuntimeLoader) PerformLeaderOperations(ctx context.Context, allKnownSourceIDs mapset.Set[string]) error {
	glog.Infof("%s loader performing leader operations", "serving_runtime")

	ctx, cancel := context.WithCancel(ctx)
	l.setCloser(cancel)

	allSources := l.Sources.AllSources()

	for id, source := range allSources {
		if !source.IsEnabled() {
			basecatalog.SaveSourceStatus(l.services.CatalogSourceRepository, id, basecatalog.SourceStatusDisabled, "")
			continue
		}

		if source.Type != "yaml" {
			glog.Warningf("unknown %s provider type: %s", "serving_runtime", source.Type)
			basecatalog.SaveSourceStatus(l.services.CatalogSourceRepository, id, basecatalog.SourceStatusError, "unknown provider type: "+source.Type)
			continue
		}

		if err := l.loadFromYAML(ctx, id, source); err != nil {
			glog.Errorf("error loading %s from source %s: %v", "serving_runtime", id, err)
			basecatalog.SaveSourceStatus(l.services.CatalogSourceRepository, id, basecatalog.SourceStatusError, err.Error())
			continue
		}

		basecatalog.SaveSourceStatus(l.services.CatalogSourceRepository, id, basecatalog.SourceStatusAvailable, "")
	}

	glog.Infof("%s loader leader operations complete", "serving_runtime")
	return nil
}

// loadFromYAML reads a serving_runtime YAML data file for the given source and persists
// each entry to the database.
func (l *ServingRuntimeLoader) loadFromYAML(ctx context.Context, sourceID string, source basecatalog.PluginSource) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	yamlPath, ok := source.Properties[yamlServingRuntimeCatalogPathKey].(string)
	if !ok || yamlPath == "" {
		return fmt.Errorf("%s property is required for YAML serving_runtime provider", yamlServingRuntimeCatalogPathKey)
	}

	if !filepath.IsAbs(yamlPath) {
		yamlPath = filepath.Join(filepath.Dir(source.Origin), yamlPath)
	}

	entries, err := loadServingRuntimesFromYAML(yamlPath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		entity := l.buildServingRuntimeEntity(sourceID, entry)
		saved, err := l.services.ServingRuntimeRepository.Save(entity)
		if err != nil {
			return fmt.Errorf("failed to save serving_runtime %q: %w", entry.Name, err)
		}

		// Persist each version as a child artifact attributed to the runtime context.
		parentID := saved.GetID()
		for _, version := range entry.Versions {
			versionEntity := l.buildServingRuntimeVersionEntity(sourceID, entry.Name, version)
			if _, err := l.services.ServingRuntimeVersionRepository.Save(versionEntity, parentID); err != nil {
				return fmt.Errorf("failed to save serving_runtime version %q for %q: %w", version.Version, entry.Name, err)
			}
		}
	}

	return nil
}

// buildServingRuntimeEntity converts a YAML entry into a persistable domain entity.
func (l *ServingRuntimeLoader) buildServingRuntimeEntity(sourceID string, entry yamlServingRuntime) servingRuntimemodels.ServingRuntime {
	name := entry.Name
	attrs := &servingRuntimemodels.ServingRuntimeAttributes{
		Name:       &name,
		ExternalID: entry.ExternalID,
	}

	properties := []mrmodels.Properties{
		mrmodels.NewStringProperty("source_id", sourceID, false),
	}
	addString := func(key string, val *string) {
		if val != nil {
			properties = append(properties, mrmodels.NewStringProperty(key, *val, false))
		}
	}
	addJSON := func(key string, val any) {
		if encoded, err := json.Marshal(val); err == nil {
			properties = append(properties, mrmodels.NewStringProperty(key, string(encoded), false))
		}
	}

	addString("displayName", entry.DisplayName)
	addString("description", entry.Description)
	addString("provider", entry.Provider)
	addString("readme", entry.Readme)
	addString("logo", entry.Logo)
	addString("license", entry.License)
	addString("licenseLink", entry.LicenseLink)
	addString("documentationUrl", entry.DocumentationURL)
	addString("repositoryUrl", entry.RepositoryURL)
	addString("publishedDate", entry.PublishedDate)
	addString("lastUpdated", entry.LastUpdated)

	if len(entry.Tags) > 0 {
		addJSON("tags", entry.Tags)
	}
	if len(entry.SupportedModelFormats) > 0 {
		addJSON("supportedModelFormats", entry.SupportedModelFormats)
	}
	if entry.Capabilities != nil {
		addJSON("capabilities", entry.Capabilities)
	}

	properties = append(properties, mrmodels.NewIntProperty("versionCount", int32(len(entry.Versions)), false))

	return &servingRuntimemodels.ServingRuntimeImpl{
		Attributes: attrs,
		Properties: &properties,
	}
}

// buildServingRuntimeVersionEntity converts a YAML version entry into a persistable
// child artifact for the given serving_runtime.
func (l *ServingRuntimeLoader) buildServingRuntimeVersionEntity(sourceID, runtimeName string, version yamlServingRuntimeVersion) servingRuntimemodels.ServingRuntimeVersion {
	name := fmt.Sprintf("%s:%s", runtimeName, version.Version)
	attrs := &servingRuntimemodels.ServingRuntimeVersionAttributes{
		Name:       &name,
		ExternalID: version.ExternalID,
	}

	properties := []mrmodels.Properties{
		mrmodels.NewStringProperty("source_id", sourceID, false),
		mrmodels.NewStringProperty("artifactType", "serving-runtime-version", false),
		mrmodels.NewStringProperty("version", version.Version, false),
		mrmodels.NewStringProperty("image", version.Image, false),
	}
	addString := func(key string, val *string) {
		if val != nil {
			properties = append(properties, mrmodels.NewStringProperty(key, *val, false))
		}
	}
	addJSON := func(key string, val any) {
		if encoded, err := json.Marshal(val); err == nil {
			properties = append(properties, mrmodels.NewStringProperty(key, string(encoded), false))
		}
	}

	if version.SupportLevel != nil {
		properties = append(properties, mrmodels.NewStringProperty("supportLevel", string(*version.SupportLevel), false))
	}
	addString("template", version.Template)
	addString("publishedDate", version.PublishedDate)
	if version.Deprecated != nil {
		properties = append(properties, mrmodels.NewBoolProperty("deprecated", *version.Deprecated, false))
	}
	if len(version.SupportedModelFormats) > 0 {
		addJSON("supportedModelFormats", version.SupportedModelFormats)
	}
	if len(version.ProtocolVersions) > 0 {
		addJSON("protocolVersions", version.ProtocolVersions)
	}
	if len(version.DefaultArgs) > 0 {
		addJSON("defaultArgs", version.DefaultArgs)
	}
	if len(version.Env) > 0 {
		addJSON("env", version.Env)
	}
	if version.RecommendedResources != nil {
		addJSON("recommendedResources", version.RecommendedResources)
	}

	return &servingRuntimemodels.ServingRuntimeVersionImpl{
		Attributes: attrs,
		Properties: &properties,
	}
}

func (l *ServingRuntimeLoader) ReloadParsing() error {
	for _, path := range l.state.Paths() {
		if err := l.parseAndMerge(path); err != nil {
			glog.Errorf("unable to reload serving_runtime sources from %s: %v", path, err)
			return err
		}
	}
	return nil
}

func (l *ServingRuntimeLoader) parseAndMerge(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("failed to get absolute path for %s: %v", path, err)
	}

	config, err := basecatalog.ReadSourceConfig(path)
	if err != nil {
		return err
	}

	return l.updateSources(path, config)
}

func (l *ServingRuntimeLoader) updateSources(path string, config *basecatalog.SourceConfig) error {
	sources := make(map[string]basecatalog.PluginSource, len(config.ServingRuntimeCatalogs))

	for _, source := range config.ServingRuntimeCatalogs {
		glog.Infof("reading serving_runtime catalog config type %s...", source.Type)
		if source.GetId() == "" {
			return fmt.Errorf("invalid serving_runtime source: missing id")
		}
		if _, exists := sources[source.GetId()]; exists {
			return fmt.Errorf("invalid serving_runtime source: duplicate id %s", source.GetId())
		}

		source.Origin = path
		sources[source.GetId()] = source
		glog.Infof("loaded serving_runtime source %s of type %s", source.GetId(), source.Type)
	}

	return l.Sources.Merge(path, sources)
}
