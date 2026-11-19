"""Recovery tests: the owned adapter restarts around uncertain writes.

The fixtures own the adapter subprocess and the compatibility database,
so these tests kill and restart only fixture-owned processes. The
session-owned forwarding proxy between the adapter and the native
server gates one native create response after MLflow has committed it —
no production failpoints are involved.
"""

import asyncio
import subprocess
import threading
import time
import uuid

import pytest

from model_registry.types import ModelArtifact, ModelVersion, RegisteredModel

from .adapter import AdapterError

pytestmark = pytest.mark.e2e

RECOVERY_TIMEOUT = 90.0


def _async(coro):
    return asyncio.run(coro)


def test_adapter_restart_between_pending_creation_and_attachment(
    client, native_verifier, managed_adapter, mlflow_model_name
):
    """A durable pending version survives an adapter restart and
    materializes when the artifact is attached afterwards."""
    name = mlflow_model_name

    rm = _async(client._api.upsert_registered_model(RegisteredModel(name=name)))
    assert rm.id
    mv = _async(
        client._api.upsert_model_version(ModelVersion(name="1.0.0"), registered_model_id=rm.id)
    )
    assert mv.id
    pending_version_id = mv.id

    # Crash and restart only the fixture-owned adapter; the
    # compatibility database keeps the durable pending version.
    managed_adapter.adapter.kill()
    managed_adapter.adapter.start()

    resumed = client.get_model_version(name, "1.0.0")
    assert resumed and resumed.id == pending_version_id, "the logical version ID must be stable across restart"  # noqa: PT018

    # The production client names the primary artifact after the model;
    # get_model_artifact looks it up by that name.
    artifact = ModelArtifact(
        uri="s3://bucket/recovered.onnx",
        name=name,
        model_format_name="onnx",
        model_format_version="1",
    )
    created = _async(client._api.upsert_model_version_artifact(artifact, pending_version_id))
    assert created.id

    versions = native_verifier.native_model_versions(name)
    assert len(versions) == 1, "exactly one native version must be materialized"
    assert versions[0]["source"] == "s3://bucket/recovered.onnx"

    artifact_after = client.get_model_artifact(name, "1.0.0")
    assert artifact_after and artifact_after.id == created.id  # noqa: PT018
    assert artifact_after.uri == "s3://bucket/recovered.onnx"


@pytest.mark.parametrize("name_suffix", ["", "-O'Reilly"])
def test_uncertain_create_recovers_after_post_commit_interruption(
    client, native_verifier, managed_adapter, mlflow_model_name, name_suffix
):
    """A native create that committed while the adapter died mid-flight is
    adopted, not duplicated: stable IDs, one native version, one
    published binding. The apostrophe suffix proves recovery never
    interpolates the model name into a native search filter."""
    name = mlflow_model_name + name_suffix
    if name_suffix:
        native_verifier.record_owned_model(name)
    proxy = managed_adapter.proxy
    proxy.arm()
    try:
        rm = _async(client._api.upsert_registered_model(RegisteredModel(name=name)))
        assert rm.id
        mv = _async(
            client._api.upsert_model_version(ModelVersion(name="1.0.0"), registered_model_id=rm.id)
        )
        assert mv.id
        version_id = mv.id

        # Attach the primary artifact in the background; the native
        # create commits upstream while its response is held at the gate.
        attach_errors: list[Exception] = []

        def attach() -> None:
            try:
                artifact = ModelArtifact(
                    uri="s3://bucket/uncertain.onnx",
                    name=name,
                    model_format_name="onnx",
                    model_format_version="1",
                )
                _async(client._api.upsert_model_version_artifact(artifact, version_id))
            except Exception as err:  # noqa: BLE001 - the crash is the point
                attach_errors.append(err)

        attacher = threading.Thread(target=attach, daemon=True)
        attacher.start()

        assert proxy.holding.wait(timeout=60.0), "the gated native create never arrived"
        # MLflow has committed the version by now ( the proxy holds the
        # response of a completed upstream request ). Kill the adapter
        # before it can record the native result.
        managed_adapter.adapter.kill()
        proxy.release.set()
        attacher.join(timeout=30.0)

        # The native version exists but was never acknowledged.
        committed = native_verifier.native_model_versions(name)
        assert len(committed) == 1

        # Restart and let the sweep reconcile the uncertain create by
        # its operation identity tag.
        managed_adapter.adapter.restart()

        deadline = time.monotonic() + RECOVERY_TIMEOUT
        artifact = None
        while time.monotonic() < deadline:
            artifact = client.get_model_artifact(name, "1.0.0")
            if artifact:
                break
            time.sleep(1.0)
        assert artifact is not None, "the interrupted materialization never recovered"
        assert artifact.uri == "s3://bucket/uncertain.onnx"

        # Conservative recovery: the committed native version was
        # adopted, not duplicated.
        versions = native_verifier.native_model_versions(name)
        assert len(versions) == 1
        assert versions[0]["source"] == "s3://bucket/uncertain.onnx"

        resumed = client.get_model_version(name, "1.0.0")
        assert resumed and resumed.id == version_id, "the logical version ID must be stable"  # noqa: PT018
    finally:
        proxy.disarm()


def test_missing_prerequisites_fail_with_instructions():
    """The stack reports setup instructions rather than skipping when the
    native target is unreachable."""
    import os

    from .adapter import MLflowStack

    saved = os.environ.get("MLFLOW_TEST_BINARY")
    os.environ["MLFLOW_TEST_BINARY"] = "/missing/mlflow"
    try:
        with pytest.raises(AdapterError, match="MLFLOW_TEST_BINARY"):
            MLflowStack()
    finally:
        if saved is None:
            del os.environ["MLFLOW_TEST_BINARY"]
        else:
            os.environ["MLFLOW_TEST_BINARY"] = saved


@pytest.mark.parametrize(("interrupted_child", "resolution"), [(1, "automatic"), (2, "automatic"), (2, "bind"), (2, "retry"), (0, "automatic")])
def test_shared_source_fanout_recovers_through_binary_startup(
    client, native_verifier, managed_adapter, mlflow_model_name, interrupted_child, resolution,
):
    """Restart with a partially completed aggregate and with all native creates committed."""
    name = mlflow_model_name
    client.register_model(name, "s3://bucket/shared-old.onnx", version="one", model_format_name="onnx", model_format_version="1")
    model = client.get_registered_model(name)
    original = client.get_model_artifact(name, "one")
    second = _async(client._api.upsert_model_version(ModelVersion(name="two"), model.id))
    _async(client._api.upsert_model_version_artifact(ModelArtifact(id=original.id), second.id))
    first = client.get_model_version(name, "one")
    for item in [first, second]:
        item.description = f"description {item.name}"
        item.author = f"author {item.name}"
        item.custom_properties = {"rank": 7, "label": "", "flag": True}
        client.update(item)
    proxy = managed_adapter.proxy
    managed_adapter.adapter.kill()
    managed_adapter.adapter.start(request_timeout="15s")
    if interrupted_child:
        proxy.arm(ordinal=interrupted_child, hold_followers=interrupted_child == 1)
    else:
        proxy.arm(path="/registered-models/alias")
    errors: list[Exception] = []

    def replace() -> None:
        try:
            changed = ModelArtifact(id=original.id, uri="s3://bucket/shared-new.onnx")
            client.update(changed)
        except Exception as error:  # noqa: BLE001 - interrupted HTTP is expected
            errors.append(error)

    updater = threading.Thread(target=replace, daemon=True)
    try:
        updater.start()
        assert proxy.holding.wait(60), "replacement never reached the selected native child"
        # Every response held by the proxy has already committed upstream.
        assert len(native_verifier.native_model_versions(name)) == 2 + (interrupted_child or 2)
        _assert_children_ready(managed_adapter, interrupted_child)
        # Partial aggregate never publishes the new shared URI.
        assert client.get_model_artifact(name, "one").uri == original.uri
        assert client.get_model_artifact(name, "two").uri == original.uri
        managed_adapter.adapter.kill()
        proxy.disarm()
        updater.join(30)
        assert not updater.is_alive()
        if resolution != "automatic":
            _resolve_child(managed_adapter, native_verifier, name, resolution)
        managed_adapter.adapter.start()
        _assert_recovered_fanout(client, name, first, second, original)
        assert {item.id for item in client.get_model_versions(name)} == {first.id, second.id}
        versions = native_verifier.native_model_versions(name)
        assert len(versions) == (4 if resolution == "automatic" else 5)
        assert sum(item["source"] == original.uri for item in versions) == 2
        assert sum(item["source"] == "s3://bucket/shared-new.onnx" for item in versions) == (2 if resolution == "automatic" else 3)
        _assert_fanout_projection(versions)
        from .test_compatibility import expected_alias

        for logical_name in ["one", "two"]:
            selected = native_verifier.native_alias(name, expected_alias(logical_name))
            assert next(item for item in versions if item["version"] == selected)["source"] == "s3://bucket/shared-new.onnx"
    finally:
        proxy.disarm()
        updater.join(30)


def _resolve_child(managed_adapter, native_verifier, name, resolution):
    proxy = managed_adapter.proxy
    versions = native_verifier.native_model_versions(name)
    selected = next(item for item in versions if item["version"] == proxy.held_response["model_version"]["version"])
    tags = {item["key"]: item["value"] for item in selected["tags"]}
    operation = str(uuid.UUID(tags["rhoai.operation"]))
    # Query only the owned SQL journal and wait for the real lease boundary.
    table = "`MLflowOperation`" if managed_adapter.database.engine == "mysql" else '"MLflowOperation"'
    deadline = time.monotonic() + RECOVERY_TIMEOUT
    while time.monotonic() < deadline:
        expiry = managed_adapter.database.query(
            f"SELECT lease_expires_since_epoch FROM {table} WHERE operation_uuid = '{operation}'",  # noqa: S608 - UUID validated above
        )
        if not expiry or int(expiry) <= int(time.time() * 1000):
            break
        time.sleep(0.2)
    command = [str(managed_adapter.adapter.binary), "mlflow-operations",
               "--mlflow-registry-uri", proxy.url,
               "--mlflow-database-type", managed_adapter.database.engine,
               "--mlflow-database-dsn", managed_adapter.database.dsn]
    if resolution == "bind":
        native_verifier._native_post("/api/2.0/mlflow/model-versions/create", {
            "name": name, "source": selected["source"], "tags": selected["tags"],
            "description": selected.get("description", ""),
        })
        command += ["bind", "--operation", operation, "--model", name, "--version", selected["version"]]
    else:
        command += ["retry-create", "--operation", operation, "--accept-duplicate-risk"]
    result = subprocess.run(command, capture_output=True, text=True, timeout=60, check=False)  # noqa: S603
    assert result.returncode == 0, result.stdout + result.stderr


def test_late_alias_request_is_repaired_after_later_generation(
    client, native_verifier, managed_adapter, mlflow_model_name,
):
    from .test_compatibility import expected_alias

    name = mlflow_model_name
    client.register_model(name, "s3://bucket/one.onnx", version="one",
                          model_format_name="onnx", model_format_version="1")
    artifact = client.get_model_artifact(name, "one")
    proxy = managed_adapter.proxy
    proxy.arm(path="/registered-models/alias", before=True)
    errors: list[Exception] = []

    def replace():
        try:
            client.update(ModelArtifact(id=artifact.id, uri="s3://bucket/two.onnx"))
        except Exception as error:  # noqa: BLE001 - killed request is expected
            errors.append(error)

    worker = threading.Thread(target=replace, daemon=True)
    try:
        worker.start()
        assert proxy.holding.wait(60)
        managed_adapter.adapter.kill()
        worker.join(30)
        assert not worker.is_alive()
        managed_adapter.adapter.start()
        _wait_for_uri(client, name, "s3://bucket/two.onnx")
        client.update(ModelArtifact(id=artifact.id, uri="s3://bucket/three.onnx"))
        versions = native_verifier.native_model_versions(name)
        assert len(versions) == 3
        newest = next(item for item in versions if item["source"] == "s3://bucket/three.onnx")
        delayed = next(item for item in versions if item["source"] == "s3://bucket/two.onnx")
        assert str(proxy.held_request["version"]) == delayed["version"]
        alias = expected_alias("one")
        assert native_verifier.native_alias(name, alias) == newest["version"]
        assert proxy.holding.is_set()
        assert not proxy.forwarded.is_set()
        assert proxy.held_status is None
        # Only now send the old alias HTTP mutation upstream, after generation three.
        proxy.disarm()
        assert proxy.forwarded.wait(30)
        assert proxy.held_status == 200
        deadline = time.monotonic() + RECOVERY_TIMEOUT
        while time.monotonic() < deadline:
            if native_verifier.native_alias(name, alias) == newest["version"]:
                break
            time.sleep(0.2)
        assert native_verifier.native_alias(name, alias) == newest["version"]
        assert client.get_model_artifact(name, "one").id == artifact.id
        assert client.get_model_artifact(name, "one").uri == "s3://bucket/three.onnx"
        assert len(native_verifier.native_model_versions(name)) == 3
    finally:
        proxy.disarm()
        worker.join(30)


def _wait_for_uri(client, name, uri):
    deadline = time.monotonic() + RECOVERY_TIMEOUT
    while time.monotonic() < deadline:
        if client.get_model_artifact(name, "one").uri == uri:
            return
        time.sleep(0.2)
    pytest.fail(f"committed URI did not recover to {uri}")


def _assert_fanout_projection(versions):
    for item in versions:
        if item["source"] != "s3://bucket/shared-new.onnx":
            continue
        tags = {tag["key"]: tag["value"] for tag in item["tags"]}
        logical_name = tags["rhoai.versionName"]
        assert item["description"] == f"description {logical_name}"
        assert tags["rhoai.author"] == f"author {logical_name}"
        assert {key: tags[key] for key in ["rank", "label", "flag"]} == {"rank": "7", "label": "", "flag": "true"}


def _assert_children_ready(managed_adapter, interrupted_child):
    table = "`MLflowOperation`" if managed_adapter.database.engine == "mysql" else '"MLflowOperation"'
    states = managed_adapter.database.query(
        f"SELECT state FROM {table} WHERE aggregate_operation_uuid IS NOT NULL AND generation = 2",  # noqa: S608 - fixture table constant
    ).splitlines()
    assert len(states) == 2
    assert states.count("SUCCEEDED") == ({0: 2, 1: 0, 2: 1}[interrupted_child])
    if interrupted_child:
        tags = managed_adapter.proxy.held_response["model_version"]["tags"]
        operation = str(uuid.UUID(next(tag["value"] for tag in tags if tag["key"] == "rhoai.operation")))
        step = managed_adapter.database.query(
            f"SELECT step FROM {table} WHERE operation_uuid = '{operation}'",  # noqa: S608 - validated UUID
        )
        assert step == "CREATE_DISPATCHED"


def _assert_recovered_fanout(client, name, first, second, original):
    deadline = time.monotonic() + RECOVERY_TIMEOUT
    while time.monotonic() < deadline:
        if client.get_model_artifact(name, "one").uri == "s3://bucket/shared-new.onnx":
            break
        time.sleep(0.2)
    for logical_name, version_id in [("one", first.id), ("two", second.id)]:
        assert client.get_model_version(name, logical_name).id == version_id
        artifact = client.get_model_artifact(name, logical_name)
        assert artifact.id == original.id
        assert artifact.uri == "s3://bucket/shared-new.onnx"
