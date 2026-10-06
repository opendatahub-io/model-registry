package v1alpha1

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	model "github.com/kubeflow/hub/catalog/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServingRuntimePreview(t *testing.T) {
	const config = "assetType: serving_runtimes\ntype: yaml\n"
	// Preview discovers names without validating runtime metadata or versions.
	const data = "serving_runtimes:\n  - name: first\n    versions: invalid-metadata\n  - name: second\n  - name: third\n"
	service := NewModelCatalogServiceAPIService(nil, nil, nil, nil, nil, nil)
	preview := func(t *testing.T, cfg, catalogData, pageSize, token, filter string) (ImplResponse, error) {
		t.Helper()
		configFile := writeTempYAML(t, "config", cfg)
		var dataFile *os.File
		if catalogData != "" {
			dataFile = writeTempYAML(t, "data", catalogData)
		}
		return service.PreviewCatalogSource(context.Background(), configFile, pageSize, token, filter, dataFile)
	}
	t.Run("upload pagination and counts", func(t *testing.T) {
		response, err := preview(t, config, data, "2", "", "")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.Code)
		result, ok := response.Body.(model.AssetSourcePreviewResponse)
		require.True(t, ok, "got %T", response.Body)
		assert.Equal(t, model.CATALOGASSETTYPE_SERVING_RUNTIMES, result.AssetType)
		assert.Equal(t, int32(3), result.Summary.TotalAssets)
		assert.Equal(t, int32(3), result.Summary.IncludedAssets)
		assert.Zero(t, result.Summary.ExcludedAssets)
		require.Len(t, result.Items, 2)
		assert.Equal(t, "first", result.Items[0].Name)
		assert.True(t, result.Items[0].Included)
		require.NotEmpty(t, result.NextPageToken)
		response, err = preview(t, config, data, "2", result.NextPageToken, "included")
		require.NoError(t, err)
		result = response.Body.(model.AssetSourcePreviewResponse)
		require.Len(t, result.Items, 1)
		assert.Equal(t, "third", result.Items[0].Name)
		assert.Empty(t, result.NextPageToken)
	})
	t.Run("unnamed runtimes do not interrupt pagination", func(t *testing.T) {
		const data = `serving_runtimes: [{versions: []}, {name: first}, {name: ""}, {name: second}, {name: null}]`
		response, err := preview(t, config, data, "1", "", "")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.Code)
		result, ok := response.Body.(model.AssetSourcePreviewResponse)
		require.True(t, ok, "got %T", response.Body)
		assert.Equal(t, int32(2), result.Summary.TotalAssets)
		assert.Equal(t, int32(2), result.Summary.IncludedAssets)
		assert.Zero(t, result.Summary.ExcludedAssets)
		require.Len(t, result.Items, 1)
		assert.Equal(t, "first", result.Items[0].Name)
		require.NotEmpty(t, result.NextPageToken)

		response, err = preview(t, config, data, "1", result.NextPageToken, "")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.Code)
		result = response.Body.(model.AssetSourcePreviewResponse)
		require.Len(t, result.Items, 1)
		assert.Equal(t, "second", result.Items[0].Name)
		assert.Empty(t, result.NextPageToken)
	})
	t.Run("path and uploaded precedence", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "catalog.yaml")
		require.NoError(t, os.WriteFile(path, []byte(data), 0600))
		cfg := config + fmt.Sprintf("properties:\n  yamlCatalogPath: %q\n", path)
		response, err := preview(t, cfg, "", "100", "", "")
		require.NoError(t, err)
		assert.Len(t, response.Body.(model.AssetSourcePreviewResponse).Items, 3)
		response, err = preview(t, cfg, "serving_runtimes: [{name: uploaded}]", "100", "", "")
		require.NoError(t, err)
		result := response.Body.(model.AssetSourcePreviewResponse)
		require.Len(t, result.Items, 1)
		assert.Equal(t, "uploaded", result.Items[0].Name)
	})
	t.Run("empty and excluded", func(t *testing.T) {
		for _, input := range []struct{ data, filter string }{{"serving_runtimes: []", ""}, {data, "excluded"}} {
			response, err := preview(t, config, input.data, "100", "", input.filter)
			require.NoError(t, err)
			result, ok := response.Body.(model.AssetSourcePreviewResponse)
			require.True(t, ok)
			assert.Empty(t, result.Items)
			assert.Zero(t, result.Size)
			assert.Zero(t, result.Summary.ExcludedAssets)
		}
	})
	for _, tc := range []struct {
		name, cfg, data, size, token, filter string
		code                                 int
	}{
		{"missing type", "assetType: serving_runtimes", data, "100", "", "", 422},
		{"unsupported type", "assetType: serving_runtimes\ntype: hf", data, "100", "", "", 422},
		{"missing data", config, "", "100", "", "", 422},
		{"bad path", config + "properties: {yamlCatalogPath: /missing/runtime.yaml}", "", "100", "", "", 422},
		{"invalid YAML", config, "serving_runtimes: [", "100", "", "", 422},
		{"bad size", config, data, "bad", "", "", 400},
		{"bad token", config, data, "100", "invalid", "", 400},
		{"bad filter", config, data, "100", "", "invalid", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, err := preview(t, tc.cfg, tc.data, tc.size, tc.token, tc.filter)
			require.Error(t, err)
			assert.Equal(t, tc.code, response.Code)
		})
	}
}
