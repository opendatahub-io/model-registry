package serving_runtimecatalog

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServingRuntimePreviewConfig(t *testing.T) {
	for _, data := range []string{"[", "assetType: serving_runtimes"} {
		_, err := ParseServingRuntimePreviewConfig([]byte(data))
		require.Error(t, err)
	}
	config, err := ParseServingRuntimePreviewConfig([]byte("type: yaml\nassetType: serving_runtimes\nname: ignored\nincludedRuntimes: [none]\n"))
	require.NoError(t, err)
	results, err := PreviewSourceRuntimes(context.Background(), config, []byte("serving_runtimes: [{name: discovered, versions: invalid}]"))
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "discovered", results[0].Name)
	assert.True(t, results[0].Included)
}

func TestServingRuntimePreviewRejectsMissingServingRuntimesKey(t *testing.T) {
	config := &ServingRuntimePreviewConfig{Type: "yaml"}
	for _, data := range []string{
		"servingRuntimes: [{name: typo}]", // mistyped key
		"source: foo\n",                   // key entirely absent
	} {
		_, err := PreviewSourceRuntimes(context.Background(), config, []byte(data))
		require.Error(t, err, "input: %s", data)
		assert.Contains(t, err.Error(), "serving_runtimes")
	}
	// An explicit empty list is a valid, intentional empty catalog.
	results, err := PreviewSourceRuntimes(context.Background(), config, []byte("serving_runtimes: []"))
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestServingRuntimePreviewRelativePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtimes.yaml")
	require.NoError(t, os.WriteFile(path, []byte("serving_runtimes: [{name: from-path}]"), 0600))
	cwd, err := os.Getwd()
	require.NoError(t, err)
	path, err = filepath.Rel(cwd, path)
	require.NoError(t, err)
	config := &ServingRuntimePreviewConfig{Type: "yaml", Properties: map[string]any{"yamlCatalogPath": path}}
	results, err := PreviewSourceRuntimes(context.Background(), config, nil)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "from-path", results[0].Name)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = PreviewSourceRuntimes(ctx, config, nil)
	require.ErrorIs(t, err, context.Canceled)
}
