"""Verify manifest-loaded runtimes and their versions through the public API."""

from typing import Any

import pytest

from model_catalog import CatalogAPIClient


def _assert_runtime_metadata(actual: dict[str, Any], expected: dict[str, Any], source_id: str) -> None:
    assert actual["name"] == expected["name"]
    assert actual["sourceId"] == source_id
    assert actual["versionCount"] == len(expected["versions"])
    for field in ("displayName", "provider", "description"):
        assert actual[field] == expected[field], f"{expected['name']}: {field}"
    assert {fmt["name"]: fmt for fmt in actual["supportedModelFormats"]} == {
        fmt["name"]: fmt for fmt in expected["supportedModelFormats"]
    }
    capabilities = actual["capabilities"]
    assert capabilities["requiresGPU"] is expected["capabilities"]["requiresGPU"]
    assert capabilities["multiModel"] is expected["capabilities"]["multiModel"]
    assert set(capabilities["supportedAccelerators"]) == set(expected["capabilities"]["supportedAccelerators"])


def test_configured_source_loads(serving_runtime_source: dict, serving_runtime_data: dict, ready_runtimes: dict):
    expected = {runtime["name"]: runtime for runtime in serving_runtime_data["serving_runtimes"]}
    assert set(ready_runtimes) == set(expected)
    ids = [runtime["id"] for runtime in ready_runtimes.values()]
    assert all(isinstance(runtime_id, str) and runtime_id.strip() for runtime_id in ids)
    assert len(set(ids)) == len(ids)
    for name, runtime in ready_runtimes.items():
        _assert_runtime_metadata(runtime, expected[name], serving_runtime_source["id"])


@pytest.mark.parametrize("runtime_name", ["e2e-vllm", "e2e-ovms"])
def test_runtime_detail(
    runtime_name: str,
    api_client: CatalogAPIClient,
    serving_runtime_source: dict,
    serving_runtime_data: dict,
    ready_runtimes: dict,
):
    listed = ready_runtimes[runtime_name]
    expected = next(runtime for runtime in serving_runtime_data["serving_runtimes"] if runtime["name"] == runtime_name)
    detail = api_client.get_serving_runtime(listed["id"])
    assert detail["id"] == listed["id"]
    _assert_runtime_metadata(detail, expected, serving_runtime_source["id"])


@pytest.mark.parametrize("runtime_name", ["e2e-vllm", "e2e-ovms"])
def test_versions_belong_to_runtime(
    runtime_name: str,
    api_client: CatalogAPIClient,
    serving_runtime_data: dict,
    ready_runtimes: dict,
):
    expected_runtime = next(
        runtime for runtime in serving_runtime_data["serving_runtimes"] if runtime["name"] == runtime_name
    )
    expected = {version["version"]: version for version in expected_runtime["versions"]}
    response = api_client.get_serving_runtime_versions(ready_runtimes[runtime_name]["id"])
    items = response["items"]
    assert {(version["version"], version["image"]) for version in items} == {
        (version["version"], version["image"]) for version in expected.values()
    }
    assert len(items) == len(expected)
    ids = [version["id"] for version in items]
    assert all(isinstance(version_id, str) and version_id.strip() for version_id in ids)
    assert len(set(ids)) == len(ids)
    sibling_images = {
        version["image"]
        for runtime in serving_runtime_data["serving_runtimes"]
        if runtime["name"] != runtime_name
        for version in runtime["versions"]
    }
    assert not sibling_images.intersection(version["image"] for version in items)
    for name, sibling in ready_runtimes.items():
        if name != runtime_name:
            sibling_versions = api_client.get_serving_runtime_versions(sibling["id"])["items"]
            assert set(ids).isdisjoint(version["id"] for version in sibling_versions)
    for version in items:
        assert version["artifactType"] == "serving-runtime-version"
        assert version["supportLevel"] == expected[version["version"]]["supportLevel"]
        assert set(version["protocolVersions"]) == set(expected[version["version"]]["protocolVersions"])
