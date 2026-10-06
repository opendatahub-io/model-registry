package openapi_test

import (
	"encoding/json"
	"testing"

	model "github.com/kubeflow/hub/catalog/pkg/openapi"
	"github.com/stretchr/testify/require"
)

func TestServingRuntimePreviewResponseDeserialization(t *testing.T) {
	data := []byte(`{"assetType":"serving_runtimes","items":[{"name":"vllm","included":true}],"pageSize":100,"size":1,"nextPageToken":"","summary":{"totalAssets":1,"includedAssets":1,"excludedAssets":0}}`)
	var response model.PreviewCatalogSourceResponse
	require.NoError(t, json.Unmarshal(data, &response))
	require.NotNil(t, response.AssetSourcePreviewResponse)
	require.Nil(t, response.CatalogSourcePreviewResponse)
	require.Equal(t, model.CATALOGASSETTYPE_SERVING_RUNTIMES, response.AssetSourcePreviewResponse.AssetType)
	require.Equal(t, "vllm", response.AssetSourcePreviewResponse.Items[0].Name)
}
