package serving_runtimecatalog

import (
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	openapi "github.com/kubeflow/hub/catalog/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyServingRuntimeDefaults(t *testing.T) {
	t.Run("sparse source", func(t *testing.T) {
		got := applyServingRuntimeDefaults(basecatalog.PluginSource{ID: "community"})
		require.NotNil(t, got.Enabled, "community source must receive an enabled default")
		assert.True(t, *got.Enabled, "sparse community source should be enabled")
		assert.Equal(t, []string{}, got.Labels, "community source should receive an empty, non-nil label list")
		require.NotNil(t, got.AssetType, "community source must receive an asset type")
		assert.Equal(t, openapi.CATALOGASSETTYPE_SERVING_RUNTIMES, *got.AssetType, "community source must target serving runtimes")
	})
	t.Run("explicit values preserved", func(t *testing.T) {
		source := basecatalog.PluginSource{ID: "disabled", Enabled: new(false), Labels: []string{"Enterprise"}, AssetType: openapi.CATALOGASSETTYPE_MODELS.Ptr()}
		assert.Equal(t, source, applyServingRuntimeDefaults(source), "defaults must preserve explicit disabled source values")
	})
}

func TestMergeServingRuntimeSources(t *testing.T) {
	base := basecatalog.PluginSource{
		ID: "community", Name: "Community", Enabled: new(true), Type: "yaml",
		Labels: []string{"Community"}, AssetType: openapi.CATALOGASSETTYPE_SERVING_RUNTIMES.Ptr(),
		Properties: map[string]any{"yamlCatalogPath": "community.yaml", "baseOnly": true}, Origin: "/community/sources.yaml",
	}
	tests := []struct {
		name     string
		override basecatalog.PluginSource
		want     basecatalog.PluginSource
	}{
		{name: "unset fields inherit base", override: basecatalog.PluginSource{}, want: base},
		{
			name: "explicit fields override and properties replace",
			override: basecatalog.PluginSource{Name: "Enterprise", Enabled: new(false), Type: "remote", Labels: []string{"Enterprise"},
				Properties: map[string]any{"yamlCatalogPath": "enterprise.yaml"}, Origin: "/enterprise/sources.yaml", AssetType: openapi.CATALOGASSETTYPE_MODELS.Ptr()},
			want: basecatalog.PluginSource{ID: "community", Name: "Enterprise", Enabled: new(false), Type: "remote", Labels: []string{"Enterprise"},
				Properties: map[string]any{"yamlCatalogPath": "enterprise.yaml"}, Origin: "/enterprise/sources.yaml", AssetType: openapi.CATALOGASSETTYPE_MODELS.Ptr()},
		},
		{
			name:     "origin follows inherited relative path",
			override: basecatalog.PluginSource{Name: "Renamed", Origin: "/enterprise/sources.yaml"},
			want: basecatalog.PluginSource{ID: "community", Name: "Renamed", Enabled: new(true), Type: "yaml", Labels: []string{"Community"},
				Properties: map[string]any{"yamlCatalogPath": "community.yaml", "baseOnly": true}, Origin: "/community/sources.yaml", AssetType: openapi.CATALOGASSETTYPE_SERVING_RUNTIMES.Ptr()},
		},
		{
			name:     "explicit empty collections clear inherited values",
			override: basecatalog.PluginSource{Labels: []string{}, Properties: map[string]any{}, Origin: "/enterprise/sources.yaml"},
			want: basecatalog.PluginSource{ID: "community", Name: "Community", Enabled: new(true), Type: "yaml", Labels: []string{},
				Properties: map[string]any{}, Origin: "/enterprise/sources.yaml", AssetType: openapi.CATALOGASSETTYPE_SERVING_RUNTIMES.Ptr()},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mergeServingRuntimeSources(base, tt.override), "community source merge: %s", tt.name)
		})
	}
}

func TestServingRuntimeSourceCollectionOriginPrecedence(t *testing.T) {
	sources := NewServingRuntimeSourceCollection("community", "enterprise")
	// Load the higher priority origin first: precedence must follow origin order.
	require.NoError(t, sources.Merge("enterprise", map[string]basecatalog.PluginSource{
		"shared":          {ID: "shared", Name: "Enterprise", Enabled: new(false)},
		"enterprise-only": {ID: "enterprise-only"},
	}), "merge enterprise origin")
	require.NoError(t, sources.Merge("community", map[string]basecatalog.PluginSource{
		"shared":         {ID: "shared", Name: "Community", Type: "yaml", Labels: []string{"Community"}},
		"community-only": {ID: "community-only"},
	}), "merge community origin")
	all := sources.AllSources()
	require.Len(t, all, 3, "origin merging must retain independent community and enterprise sources")
	assert.Equal(t, "Enterprise", all["shared"].Name, "enterprise origin must win despite being loaded first")
	assert.False(t, all["shared"].IsEnabled(), "enterprise explicit false must override community default")
	assert.Equal(t, "yaml", all["shared"].Type, "enterprise sparse override must inherit community type")
	assert.Equal(t, []string{"Community"}, all["shared"].Labels, "enterprise sparse override must inherit community labels")
	require.NoError(t, sources.Merge("team", map[string]basecatalog.PluginSource{"shared": {ID: "shared", Name: "Team"}}), "merge previously unknown team origin")
	assert.Equal(t, "Team", sources.AllSources()["shared"].Name, "new origin should append at highest precedence")
	require.NoError(t, sources.Merge("enterprise", nil), "replace enterprise origin with no sources")
	all = sources.AllSources()
	assert.NotContains(t, all, "enterprise-only", "replacing an origin must remove its previously contributed sources")
	assert.Contains(t, all, "community-only", "replacing enterprise origin must retain community sources")
	assert.Equal(t, "Team", all["shared"].Name, "replacing enterprise origin must retain team precedence")
}

func TestServingRuntimeSourceCollectionNamedQueries(t *testing.T) {
	sources := NewServingRuntimeSourceCollection("community", "enterprise")
	community := map[string]map[string]basecatalog.FieldFilter{
		"gpu":            {"provider": {Operator: "in", Value: []any{"community"}}, "capabilities.requiresGPU": {Operator: "=", Value: true}},
		"community-only": {"provider": {Operator: "=", Value: "community"}},
	}
	enterprise := map[string]map[string]basecatalog.FieldFilter{
		"gpu":             {"provider": {Operator: "in", Value: []any{"enterprise"}}},
		"enterprise-only": {"provider": {Operator: "=", Value: "enterprise"}},
	}
	require.NoError(t, sources.MergeWithNamedQueries("enterprise", nil, enterprise), "merge enterprise queries first")
	require.NoError(t, sources.MergeWithNamedQueries("community", nil, community), "merge community queries second")
	want := map[string]map[string]basecatalog.FieldFilter{
		"gpu":             {"provider": {Operator: "in", Value: []any{"enterprise"}}, "capabilities.requiresGPU": {Operator: "=", Value: true}},
		"community-only":  {"provider": {Operator: "=", Value: "community"}},
		"enterprise-only": {"provider": {Operator: "=", Value: "enterprise"}},
	}
	assert.Equal(t, want, sources.GetNamedQueries(), "query collisions must merge fields in configured origin order")
	enterprise["gpu"]["provider"].Value.([]any)[0] = "changed input slice"
	delete(community["gpu"], "capabilities.requiresGPU")
	delete(enterprise, "enterprise-only")
	assert.Equal(t, want, sources.GetNamedQueries(), "mutating caller maps and filter slices must not alter stored serving runtime queries")
	got := sources.GetNamedQueries()
	got["gpu"]["provider"].Value.([]any)[0] = "changed output slice"
	delete(got["gpu"], "capabilities.requiresGPU")
	delete(got, "community-only")
	assert.Equal(t, want, sources.GetNamedQueries(), "mutating returned maps and slices must not alter subsequent queries")
	require.NoError(t, sources.MergeWithNamedQueries("enterprise", nil, map[string]map[string]basecatalog.FieldFilter{
		"replacement": {"provider": {Operator: "=", Value: "team"}},
	}), "replace enterprise queries")
	assert.Equal(t, map[string]map[string]basecatalog.FieldFilter{
		"gpu":            {"provider": {Operator: "in", Value: []any{"community"}}, "capabilities.requiresGPU": {Operator: "=", Value: true}},
		"community-only": {"provider": {Operator: "=", Value: "community"}},
		"replacement":    {"provider": {Operator: "=", Value: "team"}},
	}, sources.GetNamedQueries(), "replacement must remove stale enterprise queries and restore lower priority collision fields")
	require.NoError(t, sources.Merge("community", nil), "reload community without named queries")
	assert.Equal(t, map[string]map[string]basecatalog.FieldFilter{
		"replacement": {"provider": {Operator: "=", Value: "team"}},
	}, sources.GetNamedQueries(), "plain merge must clear only its origin's queries")
}

func TestServingRuntimeSourceCollectionByLabel(t *testing.T) {
	sources := NewServingRuntimeSourceCollection()
	require.NoError(t, sources.Merge("community", map[string]basecatalog.PluginSource{
		"z-community":        {ID: "z-community", Labels: []string{"Community", "GPU"}},
		"a-gpu":              {ID: "a-gpu", Labels: []string{"gpu"}},
		"unlabeled":          {ID: "unlabeled"},
		"empty-labels":       {ID: "empty-labels", Labels: []string{}},
		"disabled":           {ID: "disabled", Enabled: new(false), Labels: []string{"gpu"}},
		"disabled-unlabeled": {ID: "disabled-unlabeled", Enabled: new(false)},
	}), "merge serving runtime label fixtures")
	for _, tt := range []struct {
		name   string
		labels []string
		want   []string
	}{
		{name: "case insensitive and sorted", labels: []string{"GpU"}, want: []string{"a-gpu", "z-community"}},
		{name: "multiple labels match once", labels: []string{"gpu", "COMMUNITY", "gpu"}, want: []string{"a-gpu", "z-community"}},
		{name: "null includes nil and empty labels", labels: []string{"NULL"}, want: []string{"empty-labels", "unlabeled"}},
		{name: "null and normal labels form union", labels: []string{"null", "community"}, want: []string{"empty-labels", "unlabeled", "z-community"}},
		{name: "unknown label", labels: []string{"unknown"}, want: []string{}},
		{name: "no labels", labels: nil, want: []string{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ids := []string{}
			for _, source := range sources.ByLabel(tt.labels) {
				ids = append(ids, source.ID)
			}
			assert.Equal(t, tt.want, ids, "serving runtime label request %v must exclude disabled sources and preserve sorted unique matches", tt.labels)
		})
	}
}
