"""Real-client compatibility tests against the MLflow backend.

Every test drives the unchanged production Python client through the
adapter and then verifies the native registry directly over REST —
never through the adapter — fetching all pages. The fixtures own the
adapter, the isolated compatibility database, and only the native
models they create.
"""

import asyncio
import hashlib
import time

import pytest
import requests  # type: ignore[import-untyped,unused-ignore]

from model_registry import ModelRegistry
from model_registry.exceptions import StoreError
from model_registry.types import ModelArtifact, ModelVersion, RegisteredModel
from mr_openapi.exceptions import BadRequestException

pytestmark = pytest.mark.e2e


def expected_alias(version_name: str) -> str:
    """The adapter's deterministic alias for a logical version name."""
    slug = "".join(
        ch if ch in "abcdefghijklmnopqrstuvwxyz0123456789_-_" else "-"
        for ch in version_name.lower()
    )
    while "--" in slug:
        slug = slug.replace("--", "-")
    slug = slug.strip("-")[:160].strip("-") or "version"
    digest = hashlib.sha256(version_name.encode()).hexdigest()
    return f"rhoai-{slug}-{digest}"


def escaped_key(key: str) -> str:
    """The adapter's escape for a reserved custom-property key."""
    return "rhoai.custom." + hashlib.sha256(key.encode()).hexdigest()


def test_uri_replacement_preserves_identity(client, mlflow_model_name):
    name = mlflow_model_name
    client.register_model(name, "s3://bucket/first.onnx", version="1.0.0",
                          model_format_name="onnx", model_format_version="1")
    version_before = client.get_model_version(name, "1.0.0")
    artifact = client.get_model_artifact(name, "1.0.0")
    assert version_before is not None and artifact is not None  # noqa: PT018
    artifact_id = artifact.id
    artifact.uri = "s3://bucket/second.onnx"
    updated = client.update(artifact)
    version_after = client.get_model_version(name, "1.0.0")
    assert updated.id == artifact_id
    assert updated.uri == "s3://bucket/second.onnx"
    assert version_after is not None and version_after.id == version_before.id  # noqa: PT018
    version_before.description = "updated through pre-replacement reference"
    assert client.update(version_before).id == version_after.id
    assert client.get_model_artifact(name, "1.0.0").uri == "s3://bucket/second.onnx"


def test_registration_projection_is_verifiable_natively(client, native_verifier, mlflow_model_name):
    """Typed metadata, descriptions, owner/author, source, connection and
    source tags, escaped properties, and the adapter alias are all
    projected to the native registry exactly once."""
    name = mlflow_model_name
    metadata = {
        "an-int": 7,
        "a-bool": True,
        "a-double": 1.25,
        "a-string": "value",
        "rhoai.reserved": "escaped-value",
    }
    client.register_model(
        name,
        "s3://bucket/model.onnx",
        model_format_name="onnx",
        model_format_version="1",
        version="1.0.0",
        owner="the-owner",
        author="the-author",
        metadata=metadata,
    )

    native_model = native_verifier.native_registered_model(name)
    assert native_model["name"] == name
    model_tags = {tag["key"]: tag["value"] for tag in native_model.get("tags", [])}
    assert model_tags["rhoai.owner"] == "the-owner"

    versions = native_verifier.native_model_versions(name)
    assert len(versions) == 1
    native_version = versions[0]
    assert native_version["source"] == "s3://bucket/model.onnx"
    initial_tags = {tag["key"]: tag["value"] for tag in native_version.get("tags", [])}
    assert initial_tags["rhoai.versionName"] == "1.0.0"
    assert initial_tags["rhoai.author"] == "the-author"
    for key, value in {"an-int": "7", "a-bool": "true", "a-double": "1.25", "a-string": "value",
                       escaped_key("rhoai.reserved"): "escaped-value"}.items():
        assert initial_tags[key] == value

    # Descriptions and the version tag projection synchronize to the
    # native registry on update.
    rm = client.get_registered_model(name)
    rm.description = "model description after update"
    client.update(rm)
    mv = client.get_model_version(name, "1.0.0")
    mv.description = "version description after update"
    client.update(mv)

    native_model = native_verifier.native_registered_model(name)
    assert native_model.get("description") == "model description after update"
    versions = native_verifier.native_model_versions(name)
    assert len(versions) == 1
    assert versions[0].get("description") == "version description after update"
    version_tags = {tag["key"]: tag["value"] for tag in versions[0].get("tags", [])}
    assert version_tags["rhoai.versionName"] == "1.0.0"
    assert version_tags["rhoai.author"] == "the-author"
    # register_model metadata lands on the version as typed custom
    # properties, projected to native tags with exact round-trip values.
    assert version_tags["an-int"] == "7"
    assert version_tags["a-bool"] == "true"
    assert version_tags["a-double"] == "1.25"
    assert version_tags["a-string"] == "value"
    assert version_tags[escaped_key("rhoai.reserved")] == "escaped-value"

    # The adapter-owned alias points at the one native version.
    alias = expected_alias("1.0.0")
    assert native_verifier.native_alias(name, alias) == native_version["version"]


def test_artifact_fields_and_connection_project_natively(client, native_verifier, mlflow_model_name):
    name = mlflow_model_name
    client.register_model(
        name,
        "s3://bucket/model.onnx",
        model_format_name="onnx",
        model_format_version="1",
        version="1.0.0",
        storage_key="my-connection",
        model_source_kind="hf",
        model_source_id="bert-base",
    )
    versions = native_verifier.native_model_versions(name)
    assert len(versions) == 1
    tags = {tag["key"]: tag["value"] for tag in versions[0].get("tags", [])}
    assert tags["rhoai.sourceUri"] == "s3://bucket/model.onnx"
    assert tags["rhoai.modelFormat"] == "onnx"
    assert tags["rhoai.modelFormatVersion"] == "1"
    assert tags["rhoai.connection"] == "my-connection"
    assert tags["rhoai.modelSourceKind"] == "hf"
    assert tags["rhoai.modelSourceId"] == "bert-base"


def test_additional_artifact_is_qualified_natively(client, native_verifier, mlflow_model_name):
    """A second artifact attaches without disturbing the primary projection."""
    name = mlflow_model_name
    client.register_model(name, "s3://bucket/primary.onnx", version="1.0.0",
                          model_format_name="onnx", model_format_version="1")
    version = client.get_model_version(name, "1.0.0")
    assert version and version.id  # noqa: PT018

    import asyncio

    additional = ModelArtifact(uri="s3://bucket/extra.bin", name="extra")
    created = asyncio.run(
        client._api.upsert_model_version_artifact(additional, version.id)
    )
    assert created.id

    versions = native_verifier.native_model_versions(name)
    assert len(versions) == 1
    tags = {tag["key"]: tag["value"] for tag in versions[0].get("tags", [])}
    prefix = f"rhoai.artifacts.{created.id}."
    assert tags[prefix + "sourceUri"] == "s3://bucket/extra.bin"
    # the primary projection is untouched
    assert tags["rhoai.sourceUri"] == "s3://bucket/primary.onnx"


def test_metadata_only_update_creates_no_native_version(client, native_verifier, mlflow_model_name):
    name = mlflow_model_name
    client.register_model(name, "s3://bucket/model.onnx", version="1.0.0",
                          model_format_name="onnx", model_format_version="1")
    assert len(native_verifier.native_model_versions(name)) == 1

    version = client.get_model_version(name, "1.0.0")
    version.custom_properties = {"only-metadata": "yes"}
    client.update(version)

    versions = native_verifier.native_model_versions(name)
    assert len(versions) == 1
    tags = {tag["key"]: tag["value"] for tag in versions[0].get("tags", [])}
    assert tags["only-metadata"] == "yes"


def test_multiple_generations_retain_history_and_move_alias(client, native_verifier, mlflow_model_name):
    name = mlflow_model_name
    client.register_model(name, "s3://bucket/first.onnx", version="1.0.0",
                          model_format_name="onnx", model_format_version="1")
    artifact = client.get_model_artifact(name, "1.0.0")
    artifact.uri = "s3://bucket/second.onnx"
    client.update(artifact)

    versions = native_verifier.native_model_versions(name)
    assert len(versions) == 2
    sources = {version["source"] for version in versions}
    assert sources == {"s3://bucket/first.onnx", "s3://bucket/second.onnx"}

    # The logical identity is stable and the alias follows the new generation.
    logical = client.get_model_version(name, "1.0.0")
    assert logical
    alias = expected_alias("1.0.0")
    current = native_verifier.native_alias(name, alias)
    new_generation = [v for v in versions if v["source"] == "s3://bucket/second.onnx"]
    assert len(new_generation) == 1
    assert current == new_generation[0]["version"]


def test_standalone_low_level_artifact_update(client, native_verifier, mlflow_model_name):
    name = mlflow_model_name
    client.register_model(name, "s3://bucket/model.onnx", version="1.0.0",
                          model_format_name="onnx", model_format_version="1")
    artifact = client.get_model_artifact(name, "1.0.0")
    assert artifact and artifact.id  # noqa: PT018

    import asyncio

    artifact.description = "directly updated"
    updated = asyncio.run(client._api.upsert_model_artifact(artifact))
    assert updated.description == "directly updated"
    assert updated.id == artifact.id

    versions = native_verifier.native_model_versions(name)
    assert len(versions) == 1
    tags = {tag["key"]: tag["value"] for tag in versions[0].get("tags", [])}
    assert tags["rhoai.artifactRecord.description"] == "directly updated"


def test_native_model_imported_after_refresh(client, native_verifier, mlflow_model_name):
    """A model created directly in the native registry appears through the
    adapter after one refresh interval."""
    name = mlflow_model_name
    native_verifier._native_post(
        "/api/2.0/mlflow/registered-models/create", {"name": name}
    )
    native_verifier._native_post(
        "/api/2.0/mlflow/model-versions/create",
        {"name": name, "source": "s3://bucket/external.onnx"},
    )

    deadline = time.monotonic() + 30.0
    imported = None
    while time.monotonic() < deadline:
        imported = client.get_registered_model(name)
        if imported:
            break
        time.sleep(1.0)
    assert imported and imported.id  # noqa: PT018
    versions = list(client.get_model_versions(name))
    assert len(versions) == 1
    assert versions[0].name == "1" or versions[0].name, "imported version is addressable"
    artifact = client.get_model_artifact(name, versions[0].name)
    assert artifact is not None
    assert artifact.name == name
    assert artifact.uri == "s3://bucket/external.onnx"
    identities = (imported.id, versions[0].id, artifact.id)
    native_update = requests.patch(
        native_verifier.registry_uri + "/api/2.0/mlflow/model-versions/update",
        json={"name": name, "version": "1", "description": "external description"}, timeout=30,
    )
    native_update.raise_for_status()
    native_verifier._native_post("/api/2.0/mlflow/model-versions/set-tag", {
        "name": name, "version": "1", "key": "external-label", "value": "changed",
    })
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        changed = client.get_model_version(name, versions[0].name)
        if changed.description == "external description" and (changed.custom_properties or {}).get("external-label") == "changed":
            break
        time.sleep(0.2)
    assert changed.description == "external description"
    assert changed.custom_properties["external-label"] == "changed"
    assert (client.get_registered_model(name).id, changed.id, client.get_model_artifact(name, changed.name).id) == identities


def test_unsupported_families_are_501_on_both_versions(client, managed_adapter):
    base = managed_adapter.adapter.url
    for path in [
        "/api/model_registry/v1/serving_environments",
        "/api/model_registry/v1/inference_services",
        "/api/model_registry/v1alpha3/serving_environments",
        "/api/model_registry/v1alpha3/experiments",
    ]:
        response = requests.get(base + path, timeout=30.0)
        assert response.status_code == 501, f"{path}: {response.status_code} {response.text}"
        assert response.text.strip(), f"{path}: expected a non-empty unsupported-family body"


def test_normal_initialization_against_empty_registry(managed_adapter):
    """A plain ModelRegistry constructor works against the empty registry."""
    registry = ModelRegistry(
        managed_adapter.adapter_host,
        managed_adapter.adapter_port,
        author="author",
        is_secure=False,
    )
    models = list(registry.get_registered_models())
    assert models == []


def test_registration_creates_no_experiments_or_runs(client, native_verifier, mlflow_model_name):
    """Registration must use registry APIs only, never tracking."""
    experiments_before = native_verifier.native_experiments()
    runs_before = native_verifier.native_runs()

    client.register_model(mlflow_model_name, "s3://bucket/model.onnx", version="1.0.0",
                          model_format_name="onnx", model_format_version="1")

    assert native_verifier.native_experiments() == experiments_before
    assert native_verifier.native_runs() == runs_before


@pytest.mark.parametrize("second_uri", ["s3://bucket/first.onnx", "s3://bucket/second.onnx"])
def test_two_versions_have_independent_artifacts(client, native_verifier, mlflow_model_name, second_uri):
    name = mlflow_model_name
    for version, uri in [("one", "s3://bucket/first.onnx"), ("two", second_uri)]:
        client.register_model(name, uri, version=version, model_format_name="onnx", model_format_version="1")
    first = client.get_model_artifact(name, "one")
    second = client.get_model_artifact(name, "two")
    assert first.id != second.id
    assert first.uri == "s3://bucket/first.onnx"
    assert second.uri == second_uri
    assert len(native_verifier.native_model_versions(name)) == 2
    second.description = "independent"
    client.update(second)
    assert client.get_model_artifact(name, "one").description != "independent"


@pytest.mark.parametrize("source", ["s3", "some://uri"])
def test_incompatible_original_sources_are_rejected(client, native_verifier, mlflow_model_name, source):
    with pytest.raises(BadRequestException):
        client.register_model(mlflow_model_name, source, version="one",
                              model_format_name="onnx", model_format_version="1")
    assert native_verifier.native_model_versions(mlflow_model_name) == []


def test_supported_source_duplicate_version(client, native_verifier, mlflow_model_name):
    params = {"name": mlflow_model_name, "uri": "s3://bucket/model.onnx", "version": "one",
              "model_format_name": "test_format", "model_format_version": "test_version"}
    client.register_model(**params, metadata=None)
    with pytest.raises(StoreError):
        client.register_model(**params, metadata=None)
    assert len(native_verifier.native_model_versions(mlflow_model_name)) == 1


def test_explicit_artifact_id_sharing(client, native_verifier, mlflow_model_name):
    name = mlflow_model_name
    client.register_model(name, "s3://bucket/shared.onnx", version="one", model_format_name="onnx", model_format_version="1")
    model = client.get_registered_model(name)
    artifact = client.get_model_artifact(name, "one")
    second = asyncio.run(client._api.upsert_model_version(ModelVersion(name="two"), model.id))
    shared = asyncio.run(client._api.upsert_model_version_artifact(ModelArtifact(id=artifact.id), second.id))
    assert shared.id == artifact.id
    assert client.get_model_artifact(name, "two").id == artifact.id
    artifact.uri = "s3://bucket/shared-replaced.onnx"
    client.update(artifact)
    assert client.get_model_artifact(name, "one").uri == artifact.uri
    assert client.get_model_artifact(name, "two").uri == artifact.uri
    assert len(native_verifier.native_model_versions(name)) == 4


@pytest.mark.parametrize("logical_name", ["release/one", "release:one"])
def test_initial_description_and_logical_name_projection(client, native_verifier, mlflow_model_name, logical_name):
    model = asyncio.run(client._api.upsert_registered_model(RegisteredModel(name=mlflow_model_name)))
    version = asyncio.run(client._api.upsert_model_version(
        ModelVersion(name=logical_name, description="initial description"), model.id,
    ))
    asyncio.run(client._api.upsert_model_version_artifact(
        ModelArtifact(name=mlflow_model_name, uri="s3://bucket/initial.onnx"), version.id,
    ))
    native = native_verifier.native_model_versions(mlflow_model_name)
    assert len(native) == 1
    assert native[0]["description"] == "initial description"
    assert native_verifier.native_alias(mlflow_model_name, expected_alias(logical_name)) == native[0]["version"]
    assert client.get_model_version(mlflow_model_name, logical_name).id == version.id


def test_supported_source_update_get_labels_patch_and_pagination(client, native_verifier, managed_adapter, mlflow_model_name):
    """Supported-source counterparts of the original update/get/label/page cases."""
    name = mlflow_model_name
    metadata = {"a": 1, "b": "2", "label": "", "bool": True, "double": 1.25, "integral-double": 1.0}
    for number in range(3):
        client.register_model(name, f"s3://bucket/{number}.onnx", version=str(number), metadata=metadata,
                              model_format_name="onnx", model_format_version="1")
    model = client.get_registered_model(name)
    version = client.get_model_version(name, "0")
    artifact = client.get_model_artifact(name, "0")
    identities = (model.id, version.id, artifact.id)
    expected_metadata = {}
    for index, resource in enumerate([model, version, artifact]):
        values = {**metadata, f"my-label{index * 2 + 1}": "", f"my-label{index * 2 + 2}": ""}
        expected_metadata[resource.id] = values
        resource.description = "updated description"
        resource.custom_properties = values
        assert client.update(resource).description == "updated description"
    response = requests.patch(
        f"{managed_adapter.adapter.url}/api/model_registry/v1/model_artifacts/{artifact.id}",
        json={"modelFormatName": "foo"}, timeout=30,
    )
    assert response.status_code == 200, response.text
    updated = client.update(ModelArtifact(id=artifact.id, uri=artifact.uri, description="partial update"))
    assert updated.model_format_name == "foo"
    assert updated.model_format_version == "1"
    # Restart forces initialization refresh; lists exercise persisted typed decoding.
    managed_adapter.adapter.restart()
    refreshed = [client.get_registered_model(name), client.get_model_version(name, "0"),
                 client.get_model_artifact(name, "0")]
    assert tuple(resource.id for resource in refreshed) == identities
    for resource in refreshed:
        values = expected_metadata[resource.id]
        _assert_client_metadata(resource, values)
    for family, resource in zip(["registered_models", "model_versions", "model_artifacts"], refreshed, strict=True):
        response = requests.get(f"{managed_adapter.adapter.url}/api/model_registry/v1/{family}/{resource.id}", timeout=30)
        response.raise_for_status()
        assert response.json()["customProperties"]["integral-double"]["metadataType"] == "MetadataDoubleValue"
    iterator = client.get_model_versions(name).page_size(1)
    assert {item.name for item in iterator} == {"0", "1", "2"}
    iterator.restart()
    assert {item.name for item in iterator} == {"0", "1", "2"}
    assert [item.id for item in client.get_registered_models().page_size(1)] == [model.id]
    listed_model = list(client.get_registered_models())[0]
    listed_version = next(item for item in client.get_model_versions(name) if item.name == "0")
    listed_artifacts = asyncio.run(client._api.get_model_version_artifacts(version.id))
    assert [item.id for item in listed_artifacts] == [artifact.id]
    for resource in [listed_model, listed_version, *listed_artifacts]:
        values = expected_metadata[resource.id]
        _assert_client_metadata(resource, values)
    assert len(native_verifier.native_model_versions(name)) == 3


def _assert_client_metadata(resource, values):
    assert resource.custom_properties == values
    for key, value in values.items():
        # Generated StrictFloat|StrictInt accepts integral JSON doubles as ints;
        # the REST discriminator is independently checked after refresh above.
        if key != "integral-double":
            assert type(resource.custom_properties[key]) is type(value)


@pytest.mark.parametrize("family", ["models", "versions"])
def test_supported_source_original_pagination_assertions(client, native_verifier, mlflow_model_name, family):
    """Keep the original 21-item/10-page token and restart assertions."""
    from itertools import islice

    names = []
    for number in range(21):
        name = f"{mlflow_model_name}-{number}" if family == "models" else mlflow_model_name
        native_verifier.record_owned_model(name)
        names.append(name)
        client.register_model(name, f"s3://bucket/{number}.onnx", version=f"1.0.{number}",
                              model_format_name="test_format", model_format_version="test_version")
    iterator = (client.get_registered_models() if family == "models"
                else client.get_model_versions(mlflow_model_name)).page_size(10)
    count = 0
    previous = None
    changes = 0
    with pytest.raises(StopIteration):  # noqa: PT012 - original token-transition assertions
        while count < 50 and next(iterator):
            if iterator.options.next_page_token != previous:
                previous = iterator.options.next_page_token
                changes += 1
            count += 1
    assert changes == 3
    assert count == 21
    iterator.restart()
    prefix = list(islice(iterator, 3))
    assert len(prefix) == 3
    iterator.restart()
    complete = list(iterator)
    assert len(complete) == 21
    assert complete[:3] == prefix
    if family == "models":
        assert {item.name for item in complete} == set(names)
    else:
        assert {item.name for item in complete} == {f"1.0.{number}" for number in range(21)}


@pytest.mark.parametrize("family", ["models", "versions"])
def test_supported_source_original_ordering_assertions(client, native_verifier, mlflow_model_name, family):
    resources = []
    for number in range(5):
        name = f"{mlflow_model_name}-{number}" if family == "models" else mlflow_model_name
        native_verifier.record_owned_model(name)
        model = client.register_model(name, f"s3://bucket/{number}.onnx", version=str(number),
                                      model_format_name="test_format", model_format_version="test_version")
        resources.append(model if family == "models" else client.get_model_version(name, str(number)))

    def listing():
        return client.get_registered_models() if family == "models" else client.get_model_versions(mlflow_model_name)

    expected = [item.id for item in resources]
    assert [item.id for item in listing().order_by_id()] == expected
    assert [item.id for item in listing().order_by_id().descending()] == list(reversed(expected))
    assert [item.id for item in listing().order_by_creation_time()] == expected
    assert [item.id for item in listing().order_by_update_time()] == expected
    for item in reversed(resources):
        item.description = "updated"
        client.update(item)
    assert [item.id for item in listing().order_by_update_time()] == list(reversed(expected))
    assert [item.id for item in listing().order_by_update_time().descending()] == expected
