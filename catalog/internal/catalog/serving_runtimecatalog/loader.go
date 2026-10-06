package serving_runtimecatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/golang/glog"
	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	servingRuntimemodels "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	servingRuntimeservice "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/service"
	"github.com/kubeflow/hub/catalog/internal/db/models"
	openapi "github.com/kubeflow/hub/catalog/pkg/openapi"
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
	l.state.WaitForInflightWrites(30 * time.Second)
	if err := l.removeRuntimesFromMissingSources(allKnownSourceIDs); err != nil {
		glog.Errorf("error removing serving runtimes from missing sources: %v", err)
		return err
	}

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

		l.state.TrackWrite()
		go l.watchAndLoadFromYAML(ctx, id, source)
	}

	glog.Infof("%s loader leader operations complete", "serving_runtime")
	return nil
}

// servingRuntimeLoadOutcome distinguishes individual runtime failures from
// source-wide read, decoding, interruption, and cleanup failures.
type servingRuntimeLoadOutcome struct {
	SuccessfulRuntimes int
	FailedRuntimeIDs   []string
	Diagnostics        []error
	Changed            bool
}

func (o *servingRuntimeLoadOutcome) reject(id string, err error) {
	o.FailedRuntimeIDs = append(o.FailedRuntimeIDs, id)
	o.Diagnostics = append(o.Diagnostics, err)
}

func (o servingRuntimeLoadOutcome) status(err error) string {
	if err != nil || (len(o.FailedRuntimeIDs) > 0 && o.SuccessfulRuntimes == 0) {
		return basecatalog.SourceStatusError
	}
	if len(o.FailedRuntimeIDs) > 0 {
		return basecatalog.SourceStatusPartiallyAvailable
	}
	return basecatalog.SourceStatusAvailable
}

var errServingRuntimeLeadershipLost = errors.New("no longer leader, stopping serving runtime database writes")

func (l *ServingRuntimeLoader) checkWrite(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !l.state.ShouldWriteDatabase() {
		return errServingRuntimeLeadershipLost
	}
	return nil
}

// loadFromYAML retains the error-only interface for direct callers. The watcher
// uses the structured outcome to report partial availability.
func (l *ServingRuntimeLoader) loadFromYAML(ctx context.Context, sourceID string, source basecatalog.PluginSource) error {
	outcome, err := l.loadYAML(ctx, sourceID, source)
	return errors.Join(append(outcome.Diagnostics, err)...)
}

// loadYAML decodes the complete document before making any changes, then
// validates and saves each runtime independently.
func (l *ServingRuntimeLoader) loadYAML(ctx context.Context, sourceID string, source basecatalog.PluginSource) (servingRuntimeLoadOutcome, error) {
	var outcome servingRuntimeLoadOutcome
	if err := l.checkWrite(ctx); err != nil {
		return outcome, err
	}
	if l.services.WithRuntimeFamilyTransaction == nil {
		return outcome, errors.New("serving runtime family transaction support is required")
	}
	yamlPath, ok := source.Properties[yamlServingRuntimeCatalogPathKey].(string)
	if !ok || yamlPath == "" {
		return outcome, fmt.Errorf("%s property is required for YAML serving_runtime provider", yamlServingRuntimeCatalogPathKey)
	}
	if !filepath.IsAbs(yamlPath) {
		yamlPath = filepath.Join(filepath.Dir(source.Origin), yamlPath)
	}
	glog.Infof("Loading serving runtimes from source %s (%s)", sourceID, yamlPath)
	entries, present, err := loadServingRuntimesFromYAML(yamlPath)
	if err != nil {
		return outcome, err
	}
	if !present {
		// A data file with no top-level 'serving_runtimes' key is almost certainly a
		// mistake (typo, wrong indentation, truncated file) rather than an intentional
		// empty catalog. Treat it as an error so existing runtimes are preserved and the
		// source is reported as errored, instead of silently wiping everything below.
		// Use `serving_runtimes: []` to intentionally empty a source.
		return outcome, fmt.Errorf("serving_runtime catalog %q has no top-level 'serving_runtimes' key; refusing to treat it as an empty catalog", yamlPath)
	}
	names := make(map[string]int, len(entries))
	for _, entry := range entries {
		names[entry.Name]++
	}
	retainedNames := mapset.NewSet[string]()
	for index, entry := range entries {
		if err := l.checkWrite(ctx); err != nil {
			return outcome, err
		}
		qualifiedName := sourceID + ":" + entry.Name
		identifier := qualifiedName
		if strings.TrimSpace(entry.Name) == "" {
			identifier = fmt.Sprintf("entry %d", index+1)
		} else {
			// Retain both successful and rejected/failed named entries.
			retainedNames.Add(qualifiedName)
		}
		if err := validateServingRuntime(entry, names[entry.Name] > 1); err != nil {
			outcome.reject(identifier, err)
			continue
		}
		entity, err := l.buildServingRuntimeEntity(sourceID, entry)
		if err != nil {
			outcome.reject(identifier, fmt.Errorf("failed to build serving_runtime %q: %w", entry.Name, err))
			continue
		}
		glog.Infof("Loading serving runtime %s from source %s with %d version(s)", entry.Name, sourceID, len(entry.Versions))
		err = l.services.WithRuntimeFamilyTransaction(ctx, func(runtimeRepo servingRuntimemodels.ServingRuntimeRepository, versionRepo servingRuntimemodels.ServingRuntimeVersionRepository) error {
			return l.saveRuntimeFamily(ctx, sourceID, entry, entity, runtimeRepo, versionRepo)
		})
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, errServingRuntimeLeadershipLost) {
				return outcome, err
			}
			outcome.reject(identifier, err)
			continue
		}
		outcome.Changed = true
		outcome.SuccessfulRuntimes++
	}
	if err := l.checkWrite(ctx); err != nil {
		return outcome, err
	}
	// An empty catalog intentionally removes all entries; a catalog with zero
	// successful runtimes cannot safely identify orphans (e.g. every entry failed
	// to parse or save, which could otherwise wipe the source's entire contents).
	if len(entries) == 0 || outcome.SuccessfulRuntimes > 0 {
		if err := l.removeOrphanedRuntimes(ctx, sourceID, retainedNames); err != nil {
			return outcome, err
		}
		outcome.Changed = true
	}
	if err := l.checkWrite(ctx); err != nil {
		return outcome, err
	}
	glog.Infof("%s: loaded %d serving runtimes, rejected or failed %d", sourceID, outcome.SuccessfulRuntimes, len(outcome.FailedRuntimeIDs))
	return outcome, nil
}

// saveRuntimeFamily performs every family database operation through transaction-bound repositories.
func (l *ServingRuntimeLoader) saveRuntimeFamily(ctx context.Context, sourceID string, entry yamlServingRuntime, entity servingRuntimemodels.ServingRuntime, runtimeRepo servingRuntimemodels.ServingRuntimeRepository, versionRepo servingRuntimemodels.ServingRuntimeVersionRepository) error {
	qualifiedName := sourceID + ":" + entry.Name
	if existing, err := runtimeRepo.GetByName(qualifiedName); err == nil {
		entity.SetID(*existing.GetID())
		entity.GetAttributes().CreateTimeSinceEpoch = existing.GetAttributes().CreateTimeSinceEpoch
	} else if !errors.Is(err, servingRuntimeservice.ErrServingRuntimeNotFound) {
		return fmt.Errorf("failed to look up serving_runtime %q: %w", entry.Name, err)
	}
	if err := l.checkWrite(ctx); err != nil {
		return err
	}
	saved, err := runtimeRepo.Save(entity)
	if err != nil {
		return fmt.Errorf("failed to save serving_runtime %q: %w", entry.Name, err)
	}
	parentID := saved.GetID()
	validVersions := mapset.NewSet[string]()
	for _, version := range entry.Versions {
		if err := l.checkWrite(ctx); err != nil {
			return err
		}
		versionEntity := l.buildServingRuntimeVersionEntity(sourceID, entry.Name, version)
		versionName := *versionEntity.GetAttributes().Name
		validVersions.Add(versionName)
		if existing, err := versionRepo.GetByName(versionName); err == nil {
			versionEntity.SetID(*existing.GetID())
			versionEntity.GetAttributes().CreateTimeSinceEpoch = existing.GetAttributes().CreateTimeSinceEpoch
		} else if !errors.Is(err, servingRuntimeservice.ErrServingRuntimeVersionNotFound) {
			return fmt.Errorf("failed to look up serving_runtime version %q: %w", versionName, err)
		}
		if err := l.checkWrite(ctx); err != nil {
			return err
		}
		if _, err := versionRepo.Save(versionEntity, parentID); err != nil {
			return fmt.Errorf("failed to save serving_runtime version %q for %q: %w", version.Version, entry.Name, err)
		}
	}
	if err := l.removeOrphanedVersions(ctx, versionRepo, *parentID, validVersions); err != nil {
		return err
	}
	return l.checkWrite(ctx)
}

// buildServingRuntimeEntity converts a YAML entry into a persistable domain entity.
func (l *ServingRuntimeLoader) buildServingRuntimeEntity(sourceID string, entry yamlServingRuntime) (servingRuntimemodels.ServingRuntime, error) {
	name := sourceID + ":" + entry.Name
	attrs := &servingRuntimemodels.ServingRuntimeAttributes{
		Name:       &name,
		ExternalID: entry.ExternalID,
	}

	properties := []mrmodels.Properties{
		mrmodels.NewStringProperty("source_id", sourceID, false),
		mrmodels.NewStringProperty("base_name", entry.Name, false),
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
		addJSON("supportedModelFormats", supportedModelFormatNames(entry.SupportedModelFormats))
		addJSON("supportedModelFormatsDetails", entry.SupportedModelFormats)
	}
	requiresGPU := false
	multiModel := false
	if entry.Capabilities != nil {
		addJSON("capabilities", entry.Capabilities)
		if entry.Capabilities.RequiresGPU != nil {
			requiresGPU = *entry.Capabilities.RequiresGPU
		}
		if entry.Capabilities.MultiModel != nil {
			multiModel = *entry.Capabilities.MultiModel
		}
		if len(entry.Capabilities.SupportedAccelerators) > 0 {
			addJSON("capabilities.supportedAccelerators", entry.Capabilities.SupportedAccelerators)
		}
	}
	properties = append(properties,
		mrmodels.NewBoolProperty("capabilities.requiresGPU", requiresGPU, false),
		mrmodels.NewBoolProperty("capabilities.multiModel", multiModel, false),
	)

	properties = append(properties, mrmodels.NewIntProperty("versionCount", int32(len(entry.Versions)), false))

	entity := &servingRuntimemodels.ServingRuntimeImpl{
		Attributes: attrs,
		Properties: &properties,
	}
	if entry.CustomProperties != nil {
		custom := make([]mrmodels.Properties, 0, len(*entry.CustomProperties))
		for key, value := range *entry.CustomProperties {
			prop, err := servingRuntimeCustomProperty(key, value)
			if err != nil {
				return nil, fmt.Errorf("serving_runtime %q: %w", entry.Name, err)
			}
			custom = append(custom, prop)
		}
		entity.CustomProperties = &custom
	}
	return entity, nil
}

// reservedServingRuntimeProperties are internal bookkeeping property names that
// must not be overridden by a YAML source's customProperties. Allowing them
// through would let a source impersonate another source_id or base_name,
// corrupting source-ownership queries (e.g. DeleteBySource, GetDistinctSourceIDs).
var reservedServingRuntimeProperties = map[string]bool{
	"source_id": true,
	"base_name": true,
}

func servingRuntimeCustomProperty(key string, value openapi.MetadataValue) (mrmodels.Properties, error) {
	if reservedServingRuntimeProperties[key] {
		return mrmodels.Properties{}, fmt.Errorf("custom property %q is reserved and cannot be set", key)
	}
	if value.MetadataStringValue != nil {
		return mrmodels.NewStringProperty(key, value.MetadataStringValue.StringValue, true), nil
	}
	if value.MetadataBoolValue != nil {
		return mrmodels.NewBoolProperty(key, value.MetadataBoolValue.BoolValue, true), nil
	}
	if value.MetadataIntValue != nil {
		n, err := strconv.ParseInt(value.MetadataIntValue.IntValue, 10, 32)
		if err != nil {
			return mrmodels.Properties{}, fmt.Errorf("custom property %q value %q is not a valid int32: %w", key, value.MetadataIntValue.IntValue, err)
		}
		return mrmodels.NewIntProperty(key, int32(n), true), nil
	}
	if value.MetadataDoubleValue != nil {
		return mrmodels.NewDoubleProperty(key, value.MetadataDoubleValue.DoubleValue, true), nil
	}
	if encoded, err := json.Marshal(value); err == nil {
		return mrmodels.NewStringProperty(key, string(encoded), true), nil
	}
	return mrmodels.NewStringProperty(key, "", true), nil
}

// buildServingRuntimeVersionEntity converts a YAML version entry into a persistable
// child artifact for the given serving_runtime.
func (l *ServingRuntimeLoader) buildServingRuntimeVersionEntity(sourceID, runtimeName string, version yamlServingRuntimeVersion) servingRuntimemodels.ServingRuntimeVersion {
	name := fmt.Sprintf("%s:%s:%s", sourceID, runtimeName, version.Version)
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
	addString("servingRuntimeTemplate", version.ServingRuntimeTemplate)
	addString("llmInferenceServiceConfig", version.LlmInferenceServiceConfig)
	addString("publishedDate", version.PublishedDate)
	deprecated := false
	if version.Deprecated != nil {
		deprecated = *version.Deprecated
	}
	properties = append(properties, mrmodels.NewBoolProperty("deprecated", deprecated, false))
	if len(version.SupportedModelFormats) > 0 {
		addJSON("supportedModelFormats", supportedModelFormatNames(version.SupportedModelFormats))
		addJSON("supportedModelFormatsDetails", version.SupportedModelFormats)
	}
	if len(version.ProtocolVersions) > 0 {
		addJSON("protocolVersions", version.ProtocolVersions)
	}
	if len(version.DefaultArgs) > 0 {
		addJSON("defaultArgs", version.DefaultArgs)
	}
	if len(version.Env) > 0 {
		names := make([]string, 0, len(version.Env))
		for _, variable := range version.Env {
			names = append(names, variable.Name)
		}
		addJSON("env", names)
		addJSON("envDetails", version.Env)
	}
	if version.RecommendedResources != nil {
		addJSON("recommendedResources", version.RecommendedResources)
	}

	return &servingRuntimemodels.ServingRuntimeVersionImpl{
		Attributes: attrs,
		Properties: &properties,
	}
}

func supportedModelFormatNames(formats []openapi.SupportedModelFormat) []string {
	names := make([]string, 0, len(formats))
	for _, format := range formats {
		names = append(names, format.Name)
	}
	return names
}

func (l *ServingRuntimeLoader) ReloadParsing() error {
	var errs []error
	for _, path := range l.state.Paths() {
		if err := l.parseAndMerge(path); err != nil {
			errs = append(errs, fmt.Errorf("unable to reload serving_runtime sources from %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
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
		if strings.Contains(source.GetId(), ":") {
			return fmt.Errorf("invalid serving_runtime source: id %q must not contain ':'", source.GetId())
		}
		if _, exists := sources[source.GetId()]; exists {
			return fmt.Errorf("invalid serving_runtime source: duplicate id %s", source.GetId())
		}

		source.Origin = path
		sources[source.GetId()] = source
		glog.Infof("loaded serving_runtime source %s of type %s", source.GetId(), source.Type)
	}

	if config.NamedQueries != nil {
		filtered := basecatalog.FilterNamedQueriesByAssetType(config.NamedQueries, basecatalog.AssetTypeServingRuntimes)
		if len(filtered) > 0 {
			return l.Sources.MergeWithNamedQueries(path, sources, filtered)
		}
	}
	return l.Sources.Merge(path, sources)
}

func (l *ServingRuntimeLoader) watchAndLoadFromYAML(ctx context.Context, sourceID string, source basecatalog.PluginSource) {
	releaseInitial := sync.OnceFunc(l.state.WriteComplete)
	defer releaseInitial()
	yamlPath, ok := source.Properties[yamlServingRuntimeCatalogPathKey].(string)
	if !ok || yamlPath == "" {
		glog.Errorf("serving runtime source %s has no yamlCatalogPath", sourceID)
		basecatalog.SaveSourceStatus(l.services.CatalogSourceRepository, sourceID, basecatalog.SourceStatusError, "yamlCatalogPath is required")
		return
	}
	if !filepath.IsAbs(yamlPath) {
		yamlPath = filepath.Join(filepath.Dir(source.Origin), yamlPath)
	}
	ch, err := basecatalog.GetMonitor().Path(ctx, yamlPath)
	if err != nil {
		glog.Errorf("unable to watch serving_runtime catalog file %s: %v", yamlPath, err)
	}
	l.doLoad(ctx, sourceID, source)
	releaseInitial()
	if ch == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			glog.Infof("Reloading serving runtime catalog from %s (file changed)", yamlPath)
			l.state.TrackWrite()
			l.doLoad(ctx, sourceID, source)
			l.state.WriteComplete()
		}
	}
}

// doLoad performs a single load/reload of sourceID and persists the resulting
// source status. Leadership loss and context cancellation are benign
// transitions during failover/shutdown - like the modelcatalog and mcpcatalog
// loaders, we stop silently without overwriting the source status, since
// another node (or a future reload) owns reporting its health.
func (l *ServingRuntimeLoader) doLoad(ctx context.Context, sourceID string, source basecatalog.PluginSource) {
	outcome, err := l.loadYAML(ctx, sourceID, source)
	// A load that failed only because leadership was briefly lost must not publish
	// a status: if leadership has since been regained, l.state.ShouldWriteDatabase()
	// below would pass and overwrite a healthy status with a spurious "no longer
	// leader" error, even though nothing is actually wrong.
	if ctx.Err() != nil || !l.state.ShouldWriteDatabase() || errors.Is(err, errServingRuntimeLeadershipLost) {
		return
	}
	diagnostics := errors.Join(append(outcome.Diagnostics, err)...)
	message := ""
	if diagnostics != nil {
		message = diagnostics.Error()
		glog.Errorf("error loading serving_runtime from source %s: %v", sourceID, diagnostics)
	}
	basecatalog.SaveSourceStatus(l.services.CatalogSourceRepository, sourceID, outcome.status(err), message)
	if !outcome.Changed {
		return
	}
	if err := l.services.PropertyOptionsRepository.Refresh(models.ContextPropertyOptionType); err != nil {
		glog.Errorf("error refreshing property options after serving_runtime load: %v", err)
	}
	if err := l.services.PropertyOptionsRepository.Refresh(models.ArtifactPropertyOptionType); err != nil {
		glog.Errorf("error refreshing version property options after serving_runtime load: %v", err)
	}
}

func (l *ServingRuntimeLoader) removeRuntimesFromMissingSources(allKnownSourceIDs mapset.Set[string]) error {
	configured := mapset.NewSet[string]()
	enabled := mapset.NewSet[string]()
	for id, source := range l.Sources.AllSources() {
		configured.Add(id)
		if source.IsEnabled() {
			enabled.Add(id)
		}
	}
	runtimeIDs, err := l.services.ServingRuntimeRepository.GetDistinctSourceIDs()
	if err != nil {
		return fmt.Errorf("listing serving_runtime source IDs: %w", err)
	}
	versionIDs, err := l.services.ServingRuntimeVersionRepository.GetDistinctSourceIDs()
	if err != nil {
		return fmt.Errorf("listing serving_runtime version source IDs: %w", err)
	}
	for oldSource := range mapset.NewSet(append(runtimeIDs, versionIDs...)...).Difference(enabled).Iter() {
		if !l.state.ShouldWriteDatabase() {
			glog.Info("No longer leader, stopping serving runtime database writes")
			return nil
		}
		glog.Infof("Removing serving runtimes from source %s", oldSource)
		l.state.TrackWrite()
		err = l.services.ServingRuntimeVersionRepository.DeleteBySource(oldSource)
		if err == nil {
			err = l.services.ServingRuntimeRepository.DeleteBySource(oldSource)
		}
		l.state.WriteComplete()
		if err != nil {
			return fmt.Errorf("removing serving_runtimes from source %q: %w", oldSource, err)
		}
		if !configured.Contains(oldSource) && !allKnownSourceIDs.Contains(oldSource) {
			glog.Infof("Removing status for serving runtime source %s (no longer in any config)", oldSource)
			if err := l.services.CatalogSourceRepository.Delete(oldSource); err != nil {
				glog.Errorf("failed to delete status for serving_runtime source %s: %v", oldSource, err)
			}
		}
	}
	return basecatalog.CleanupOrphanedCatalogSources(l.services.CatalogSourceRepository, configured.Union(allKnownSourceIDs))
}

func (l *ServingRuntimeLoader) removeOrphanedVersions(ctx context.Context, versionRepo servingRuntimemodels.ServingRuntimeVersionRepository, parentID int32, valid mapset.Set[string]) error {
	pageSize := int32(100)
	options := &servingRuntimemodels.ServingRuntimeVersionListOptions{ParentResourceID: &parentID}
	options.PageSize = &pageSize
	for {
		if err := l.checkWrite(ctx); err != nil {
			return err
		}
		list, err := versionRepo.List(options)
		if err != nil {
			return fmt.Errorf("listing versions for serving_runtime %d: %w", parentID, err)
		}
		for _, version := range list.Items {
			if err := l.checkWrite(ctx); err != nil {
				return err
			}
			if attrs := version.GetAttributes(); attrs != nil && attrs.Name != nil && version.GetID() != nil && !valid.Contains(*attrs.Name) {
				if err := l.checkWrite(ctx); err != nil {
					return err
				}
				if err := versionRepo.DeleteByID(*version.GetID()); err != nil {
					return err
				}
				glog.Infof("Removed orphaned serving runtime version %s", *attrs.Name)
			}
		}
		// Repository.List mutates options in place to advance the pagination
		// cursor, but advance it explicitly too (and stop on an empty page) so
		// this loop doesn't depend on that side effect to avoid looping forever.
		if list.NextPageToken == "" || len(list.Items) == 0 {
			return l.checkWrite(ctx)
		}
		options.NextPageToken = &list.NextPageToken
	}
}

func (l *ServingRuntimeLoader) removeOrphanedRuntimes(ctx context.Context, sourceID string, valid mapset.Set[string]) error {
	pageSize := int32(100)
	options := &servingRuntimemodels.ServingRuntimeListOptions{SourceIDs: &[]string{sourceID}}
	options.PageSize = &pageSize
	for {
		if err := l.checkWrite(ctx); err != nil {
			return err
		}
		list, err := l.services.ServingRuntimeRepository.List(options)
		if err != nil {
			return fmt.Errorf("listing serving_runtimes from source %q: %w", sourceID, err)
		}
		for _, runtime := range list.Items {
			if err := l.checkWrite(ctx); err != nil {
				return err
			}
			if attrs := runtime.GetAttributes(); attrs != nil && attrs.Name != nil && runtime.GetID() != nil && !valid.Contains(*attrs.Name) {
				if err := l.checkWrite(ctx); err != nil {
					return err
				}
				if err := l.services.ServingRuntimeVersionRepository.DeleteByParentID(*runtime.GetID()); err != nil {
					return err
				}
				if err := l.checkWrite(ctx); err != nil {
					return err
				}
				if err := l.services.ServingRuntimeRepository.DeleteByID(*runtime.GetID()); err != nil {
					return err
				}
				glog.Infof("Removed orphaned serving runtime %s from source %s", *attrs.Name, sourceID)
			}
		}
		// Repository.List mutates options in place to advance the pagination
		// cursor, but advance it explicitly too (and stop on an empty page) so
		// this loop doesn't depend on that side effect to avoid looping forever.
		if list.NextPageToken == "" || len(list.Items) == 0 {
			return l.checkWrite(ctx)
		}
		options.NextPageToken = &list.NextPageToken
	}
}
