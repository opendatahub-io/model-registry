"""Fixture expectations and bounded readiness for manifest-loaded runtimes."""

import time
from pathlib import Path
from typing import Any

import pytest
import yaml

from model_catalog import CatalogAPIClient, CatalogAPIError, CatalogConnectionError, CatalogError
from tests.constants import MAX_BACKOFF, MAX_POLL_TIME, POLL_INTERVAL

RUNTIME_PATH = "/api/serving_runtime_catalog/v1/serving_runtimes"


@pytest.fixture(scope="session", autouse=True)
def require_runtime_fixture(kind_cluster: bool) -> None:
    """Skip before connecting when the external deployment lacks our fixture."""
    if not kind_cluster:
        pytest.skip("Serving-runtime tests require the E2E overlay fixture; KIND_CLUSTER=False uses external data")


@pytest.fixture(scope="session")
def serving_runtime_source(sources_config: dict) -> dict:
    sources = sources_config["serving_runtime_catalogs"]
    source = next(source for source in sources if source["id"] == "test_serving_runtimes")
    assert source["enabled"] is True
    assert source["type"] == "yaml"
    return source


@pytest.fixture(scope="session")
def serving_runtime_data(root: Path, serving_runtime_source: dict) -> dict:
    deployed_path = Path(serving_runtime_source["properties"]["yamlCatalogPath"])
    assert deployed_path.parent == Path("/testdata")
    overlay = root / "manifests/kustomize/options/catalog/overlays/e2e"
    with (overlay / deployed_path.name).open() as stream:
        data = yaml.safe_load(stream)
    assert data["source"] == serving_runtime_source["id"]
    assert {runtime["name"] for runtime in data["serving_runtimes"]} == {"e2e-vllm", "e2e-ovms"}
    return data


def _items(response: Any, fields: tuple[str, ...], context: str) -> list[dict[str, Any]]:
    if not isinstance(response, dict) or not isinstance(response.get("items"), list):
        pytest.fail(f"Malformed response at {context}: {response!r}")
    for item in response["items"]:
        if not isinstance(item, dict) or any(
            not isinstance(item.get(field), str) or not item[field].strip() for field in fields
        ):
            pytest.fail(f"Malformed item at {context}: {item!r}; response={response!r}")
    return response["items"]


def _poll_runtimes(client: CatalogAPIClient, source_id: str, expected: dict) -> dict:
    deadline = time.monotonic() + MAX_POLL_TIME
    backoff = POLL_INTERVAL
    observed: dict[str, Any] = {}
    endpoint = f"{client.base_url}{RUNTIME_PATH}?source={source_id}"
    context = f"source={source_id}, runtimes={list(expected)}, endpoint={endpoint}"
    while True:
        remaining = int(deadline - time.monotonic())
        if remaining < 1:
            break
        try:
            client.timeout = min(client.timeout, MAX_BACKOFF, remaining)
            response = client.get_serving_runtimes(source=source_id)
            observed["runtimes"] = response
            items = _items(response, ("name", "id"), context)
            runtimes = {runtime["name"]: runtime for runtime in items}
            assert len(runtimes) == len(items), f"Duplicate runtime names at {context}: {response!r}"
            if _versions_visible(client, runtimes, expected, deadline, observed, source_id):
                return runtimes
        except CatalogConnectionError as error:
            observed["error"] = str(error)
        except CatalogAPIError as error:
            if error.status_code is None or not 500 <= error.status_code < 600:
                pytest.fail(f"Runtime readiness failed at {context}: {error}; last results={observed!r}")
            observed["error"] = str(error)
        except CatalogError as error:
            pytest.fail(f"Invalid runtime response at {context}: {error}; last results={observed!r}")
        time.sleep(max(0, min(backoff, deadline - time.monotonic())))
        backoff = min(backoff * 2, MAX_BACKOFF)
    pytest.fail(f"Runtime fixture not ready within {MAX_POLL_TIME}s at {context}; last results={observed!r}")


def _versions_visible(
    client: CatalogAPIClient,
    runtimes: dict,
    expected: dict,
    deadline: float,
    observed: dict,
    source_id: str,
) -> bool:
    if not expected.keys() <= runtimes.keys():
        return False
    complete = True
    for name, runtime in expected.items():
        runtime_id = runtimes[name]["id"]
        endpoint = f"{client.base_url}{RUNTIME_PATH}/{runtime_id}/versions"
        context = f"source={source_id}, runtime={name}, id={runtime_id}, endpoint={endpoint}"
        observed["checking"] = context
        remaining = int(deadline - time.monotonic())
        if remaining < 1:
            return False
        client.timeout = min(client.timeout, MAX_BACKOFF, remaining)
        response = client.get_serving_runtime_versions(runtime_id)
        observed[context] = response
        versions = _items(response, ("id", "version", "image"), context)
        pairs = {(version["version"], version["image"]) for version in versions}
        complete = complete and {(version["version"], version["image"]) for version in runtime["versions"]} <= pairs
    return complete


@pytest.fixture(scope="session")
def ready_runtimes(
    require_runtime_fixture: None,
    api_client: CatalogAPIClient,
    serving_runtime_source: dict,
    serving_runtime_data: dict,
) -> dict:
    """Wait for both runtime families and their versions via an isolated poller."""
    expected = {runtime["name"]: runtime for runtime in serving_runtime_data["serving_runtimes"]}
    # Dedicated client with retries bound at construction so transport retries
    # cannot exceed the poll deadline; the shared session client is left untouched.
    with CatalogAPIClient(
        api_client.base_url,
        timeout=api_client.timeout,
        verify_ssl=api_client.verify_ssl,
        access_token=api_client.access_token,
        retries=0,
    ) as poller:
        return _poll_runtimes(poller, serving_runtime_source["id"], expected)
