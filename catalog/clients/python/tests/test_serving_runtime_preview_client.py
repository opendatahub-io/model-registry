"""Serving runtime preview deserialization needs no running catalog."""

from catalog_openapi.models.asset_source_preview_response import AssetSourcePreviewResponse
from catalog_openapi.models.preview_catalog_source_response import PreviewCatalogSourceResponse


def test_serving_runtime_preview_deserialization():
    response = PreviewCatalogSourceResponse.from_dict(
        {
            "assetType": "serving_runtimes",
            "items": [{"name": "vllm", "included": True}],
            "pageSize": 100,
            "size": 1,
            "nextPageToken": "",
            "summary": {"totalAssets": 1, "includedAssets": 1, "excludedAssets": 0},
        }
    )
    preview = response.actual_instance
    assert isinstance(preview, AssetSourcePreviewResponse)
    assert preview.asset_type == "serving_runtimes"
    assert preview.items[0].name == "vllm"
    assert preview.items[0].included is True
    assert preview.summary.total_assets == 1
    assert preview.summary.included_assets == 1
    assert preview.summary.excluded_assets == 0
