package serving_runtimecatalog

import (
	"slices"
	"strings"
	"sync"

	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	model "github.com/kubeflow/hub/catalog/pkg/openapi"
)

type servingRuntimeOriginEntry struct {
	origin  string
	sources map[string]basecatalog.PluginSource
}

// ServingRuntimeSourceCollection manages serving_runtime catalog sources from multiple origins with priority-based merging.
type ServingRuntimeSourceCollection struct {
	mu                sync.RWMutex
	entries           []servingRuntimeOriginEntry
	namedQueryEntries map[string]map[string]map[string]basecatalog.FieldFilter
}

func NewServingRuntimeSourceCollection(originOrder ...string) *ServingRuntimeSourceCollection {
	entries := make([]servingRuntimeOriginEntry, len(originOrder))
	for i, origin := range originOrder {
		entries[i] = servingRuntimeOriginEntry{origin: origin, sources: nil}
	}
	return &ServingRuntimeSourceCollection{
		entries:           entries,
		namedQueryEntries: make(map[string]map[string]map[string]basecatalog.FieldFilter),
	}
}

func (sc *ServingRuntimeSourceCollection) Merge(origin string, sources map[string]basecatalog.PluginSource) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	delete(sc.namedQueryEntries, origin)
	return sc.mergeSources(origin, sources)
}

func (sc *ServingRuntimeSourceCollection) MergeWithNamedQueries(origin string, sources map[string]basecatalog.PluginSource, queries map[string]map[string]basecatalog.FieldFilter) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if err := sc.mergeSources(origin, sources); err != nil {
		return err
	}
	sc.namedQueryEntries[origin] = basecatalog.CloneNamedQueries(queries)
	return nil
}

func (sc *ServingRuntimeSourceCollection) mergeSources(origin string, sources map[string]basecatalog.PluginSource) error {

	for i := range sc.entries {
		if sc.entries[i].origin == origin {
			sc.entries[i].sources = sources
			return nil
		}
	}

	sc.entries = append(sc.entries, servingRuntimeOriginEntry{origin: origin, sources: sources})
	return nil
}

func (sc *ServingRuntimeSourceCollection) GetNamedQueries() map[string]map[string]basecatalog.FieldFilter {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	order := make([]string, len(sc.entries))
	for i, entry := range sc.entries {
		order[i] = entry.origin
	}
	return basecatalog.CloneNamedQueries(basecatalog.MergeNamedQueriesInOrder(order, sc.namedQueryEntries))
}

func (sc *ServingRuntimeSourceCollection) merged() map[string]basecatalog.PluginSource {
	result := map[string]basecatalog.PluginSource{}

	for _, entry := range sc.entries {
		for id, source := range entry.sources {
			if existing, ok := result[id]; ok {
				result[id] = mergeServingRuntimeSources(existing, source)
			} else {
				result[id] = source
			}
		}
	}

	for id, source := range result {
		result[id] = applyServingRuntimeDefaults(source)
	}

	return result
}

func mergeServingRuntimeSources(base, override basecatalog.PluginSource) basecatalog.PluginSource {
	result := base

	common := basecatalog.MergeCommonSourceFields(
		basecatalog.CommonSourceFields{Name: base.Name, Enabled: base.Enabled, Labels: base.Labels, Type: base.Type, Properties: base.Properties, Origin: base.Origin, AssetType: base.AssetType},
		basecatalog.CommonSourceFields{Name: override.Name, Enabled: override.Enabled, Labels: override.Labels, Type: override.Type, Properties: override.Properties, Origin: override.Origin, AssetType: override.AssetType},
	)
	result.Name = common.Name
	result.Enabled = common.Enabled
	result.Labels = common.Labels
	result.Type = common.Type
	result.Properties = common.Properties
	result.Origin = common.Origin
	result.AssetType = common.AssetType

	return result
}

func applyServingRuntimeDefaults(source basecatalog.PluginSource) basecatalog.PluginSource {
	if source.Enabled == nil {
		source.Enabled = new(true)
	}
	if source.Labels == nil {
		source.Labels = []string{}
	}
	if source.AssetType == nil {
		source.AssetType = model.CATALOGASSETTYPE_SERVING_RUNTIMES.Ptr()
	}
	return source
}

func (sc *ServingRuntimeSourceCollection) AllSources() map[string]basecatalog.PluginSource {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	return sc.merged()
}

// ByLabel returns enabled sources matching any requested label. "null" matches
// sources without labels.
func (sc *ServingRuntimeSourceCollection) ByLabel(labels []string) []basecatalog.PluginSource {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	wanted := make(map[string]bool, len(labels))
	for _, label := range labels {
		wanted[strings.ToLower(label)] = true
	}
	var matches []basecatalog.PluginSource
	for _, source := range sc.merged() {
		if !source.IsEnabled() {
			continue
		}
		if len(source.Labels) == 0 && wanted["null"] {
			matches = append(matches, source)
			continue
		}
		for _, label := range source.Labels {
			if wanted[strings.ToLower(label)] {
				matches = append(matches, source)
				break
			}
		}
	}
	slices.SortFunc(matches, func(a, b basecatalog.PluginSource) int { return strings.Compare(a.ID, b.ID) })
	return matches
}
