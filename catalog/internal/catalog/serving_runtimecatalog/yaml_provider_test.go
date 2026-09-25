package serving_runtimecatalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	"github.com/stretchr/testify/require"
)

func TestLoadServingRuntimesRejectsInvalidEntries(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"missing name", "serving_runtimes:\n  - versions: [{version: '1', image: example:v1}]\n"},
		{"duplicate runtime", "serving_runtimes:\n  - name: one\n  - name: one\n"},
		{"missing version", "serving_runtimes:\n  - name: one\n    versions: [{image: example:v1}]\n"},
		{"missing image", "serving_runtimes:\n  - name: one\n    versions: [{version: '1'}]\n"},
		{"duplicate version", "serving_runtimes:\n  - name: one\n    versions: [{version: '1', image: example:v1}, {version: '1', image: example:v2}]\n"},
		{"colon in name", "serving_runtimes:\n  - name: one:two\n    versions: [{version: '1', image: example:v1}]\n"},
		{"colon in version", "serving_runtimes:\n  - name: one\n    versions: [{version: '1:2', image: example:v1}]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runtimes.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0600))
			_, err := loadServingRuntimesFromYAML(path)
			require.Error(t, err)
		})
	}
}

func TestDemoServingRuntimeSource(t *testing.T) {
	demo := filepath.Join("../../../../manifests/kustomize/options/catalog/overlays/demo")
	config, err := basecatalog.ReadSourceConfig(filepath.Join(demo, "dev-sources.yaml"))
	require.NoError(t, err)
	require.Len(t, config.ServingRuntimeCatalogs, 1)
	source := config.ServingRuntimeCatalogs[0]
	require.Equal(t, "rh_serving_runtimes", source.GetId())
	path, ok := source.Properties[yamlServingRuntimeCatalogPathKey].(string)
	require.True(t, ok)
	entries, err := loadServingRuntimesFromYAML(filepath.Join(demo, path))
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "vllm", entries[0].Name)
	require.Equal(t, "ovms", entries[1].Name)
	require.Equal(t, "registry.redhat.io/rhoai/vllm-rhel9:0.5.0", entries[0].Versions[0].Image)
	require.Equal(t, "registry.redhat.io/rhoai/openvino-rhel9:2024.4", entries[1].Versions[0].Image)
}
