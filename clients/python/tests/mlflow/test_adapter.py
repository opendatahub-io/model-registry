"""Pure tests for acceptance resource ownership and independent observations."""

import http.client
import signal
import subprocess
import threading
from pathlib import Path
from typing import Any
from unittest.mock import Mock, call

import pytest
import requests

from ..conftest import cleanup
from . import adapter as scaffolding
from .adapter import AdapterError, MLflowStack


def test_unset_adapter_address_allocates_port(monkeypatch):
    monkeypatch.delenv("MR_URL", raising=False)
    allocate = Mock(return_value=53123)
    monkeypatch.setattr(scaffolding, "_free_port", allocate)
    assert bare_stack()._resolve_adapter_address() == ("http://127.0.0.1", 53123)
    allocate.assert_called_once_with()


def test_explicit_adapter_port_collision_fails(monkeypatch):
    import socket

    with socket.socket() as occupied:
        occupied.bind(("127.0.0.1", 0))
        port = occupied.getsockname()[1]
        monkeypatch.setenv("MR_URL", f"http://127.0.0.1:{port}")
        with pytest.raises(AdapterError, match="explicit MR_URL port .* is unavailable"):
            bare_stack()._resolve_adapter_address()


@pytest.mark.parametrize("follower", [False, True])
def test_expired_before_forward_gate_never_dispatches_native_request(monkeypatch, follower):
    proxy = scaffolding.GatedNativeProxy.__new__(scaffolding.GatedNativeProxy)
    monkeypatch.setattr(proxy, "select_gate", Mock(return_value=(not follower, follower)))
    proxy.gate_before = True
    proxy.holding = threading.Event()
    proxy.followers_holding = threading.Event()
    proxy.release = Mock()
    proxy.release.wait.return_value = False
    upstream = Mock()
    monkeypatch.setattr(requests, "request", upstream)
    handler = Mock(command="POST", path="/registered-models/alias", headers={})
    proxy.forward(handler)
    assert (proxy.followers_holding if follower else proxy.holding).is_set()
    upstream.assert_not_called()
    handler.send_error.assert_called_once_with(504, "owned native gate expired without release")


@pytest.mark.parametrize("before", [False, True])
def test_proxy_gates_only_selected_response_and_releases_on_disarm(monkeypatch, before):
    response = Mock(status_code=200, headers={}, content=b"{}")
    upstream = Mock(return_value=response)
    monkeypatch.setattr(requests, "request", upstream)
    proxy = scaffolding.GatedNativeProxy("http://unused.invalid")
    proxy.arm(path="/registered-models/alias", ordinal=2, before=before)

    def post(path):
        connection = http.client.HTTPConnection("127.0.0.1", proxy.port, timeout=5)
        try:
            connection.request("POST", path, body=b"{}")
            result = connection.getresponse()
            assert result.status == 200
            assert result.read() == b"{}"
        finally:
            connection.close()

    worker = threading.Thread(target=post, args=("/registered-models/alias",))
    try:
        post("/model-versions/create")
        post("/registered-models/alias")
        assert not proxy.holding.is_set()
        worker.start()
        assert proxy.holding.wait(5)
        assert worker.is_alive()
        assert upstream.call_count == (2 if before else 3)
        proxy.disarm()
        worker.join(5)
        assert not worker.is_alive()
        assert proxy.forwarded.is_set()
    finally:
        proxy.stop()
        if worker.ident is not None:
            worker.join(5)


def bare_stack() -> Any:
    stack: Any = MLflowStack.__new__(MLflowStack)
    stack.owned_model_names = set()
    return stack


def test_cleanup_deletes_only_recorded_model():
    stack = bare_stack()
    stack.owned_model_names = {"owned-model"}
    stack.native_model_names = Mock(return_value={"owned-model", "concurrent-unrelated"})
    stack.native_delete_model = Mock()
    stack.delete_test_owned_native_models()
    stack.native_delete_model.assert_called_once_with("owned-model")


def test_cleanup_preserves_preexisting_prefixed_model():
    stack = bare_stack()
    stack.native_model_names = Mock(return_value={"mr-e2e-preexisting"})
    stack.native_delete_model = Mock()
    stack.delete_test_owned_native_models()
    stack.native_delete_model.assert_not_called()


def test_recording_precedes_create_and_cleanup_consumes_ownership():
    stack = bare_stack()
    stack.native_delete_model = Mock()
    stack.native_model_names = Mock(return_value={"owned-model"})
    stack.record_owned_model("owned-model")
    stack.delete_test_owned_native_models()
    stack.delete_test_owned_native_models()
    stack.native_delete_model.assert_called_once_with("owned-model")
    assert stack.owned_model_names == set()


@pytest.fixture
def resources(monkeypatch):
    native, database, proxy, managed = Mock(), Mock(), Mock(), Mock()
    native.url = "http://127.0.0.1:5001"
    monkeypatch.setenv("MLFLOW_REGISTRY_URL", "http://external.invalid:5000")
    monkeypatch.setattr(MLflowStack, "_verify_native_server", Mock())
    monkeypatch.setattr(MLflowStack, "_resolve_binary", Mock(return_value=Path("/mr")))
    monkeypatch.setattr(MLflowStack, "_resolve_native_binary", Mock(return_value=Path("/mlflow")), raising=False)
    monkeypatch.setattr(MLflowStack, "_resolve_adapter_address", Mock(return_value=("http://127.0.0.1", 8080)))
    monkeypatch.setattr(MLflowStack, "native_model_names", Mock(return_value={"mr-e2e-preexisting"}))
    delete = Mock()
    monkeypatch.setattr(MLflowStack, "native_delete_model", delete)
    monkeypatch.setattr(scaffolding, "NativeRegistryProcess", Mock(start=Mock(return_value=native)), raising=False)
    monkeypatch.setattr(scaffolding, "start_compat_database", Mock(return_value=database))
    monkeypatch.setattr(scaffolding, "GatedNativeProxy", Mock(return_value=proxy))
    monkeypatch.setattr(scaffolding, "ManagedAdapter", Mock(return_value=managed))
    return native, database, proxy, managed, delete


def test_startup_never_claims_existing_model_by_prefix(resources):
    *_, delete = resources
    stack: Any = MLflowStack()
    try:
        delete.assert_not_called()
        assert stack.registry_uri == "http://127.0.0.1:5001"
    finally:
        stack.close()


@pytest.mark.parametrize("failure", ["database", "proxy", "adapter", "start"])
def test_failed_constructor_closes_allocated_resources(resources, failure):
    native, database, proxy, managed, _ = resources
    target = {
        "database": scaffolding.start_compat_database,
        "proxy": scaffolding.GatedNativeProxy,
        "adapter": scaffolding.ManagedAdapter,
        "start": managed.start,
    }[failure]
    target.side_effect = RuntimeError("allocation failed")
    with pytest.raises(RuntimeError, match="allocation failed"):
        MLflowStack()
    native.stop.assert_called_once_with()
    if failure != "database":
        database.stop.assert_called_once_with()
    if failure in ("adapter", "start"):
        proxy.stop.assert_called_once_with()
    if failure == "start":
        managed.stop.assert_called_once_with()


def test_reset_preserves_concurrent_unrelated_model(resources):
    native, database, proxy, managed, delete = resources
    stack: Any = MLflowStack()
    try:
        stack.native_model_names = Mock(return_value={"mr-e2e-preexisting", "concurrent-unrelated", "owned-model"})
        stack.owned_model_names = {"owned-model"}
        delete.reset_mock()
        order = Mock()
        order.attach_mock(managed.kill, "kill")
        order.attach_mock(delete, "delete")
        order.attach_mock(database.reset, "reset")
        order.attach_mock(managed.start, "start")
        stack.reset_test_state()
        assert order.mock_calls == [call.kill(), call.delete("owned-model"), call.reset(), call.start()]
        native.stop.assert_not_called()
        proxy.disarm.assert_called_once_with()
    finally:
        stack.close()


def test_external_probe_refuses_reset_before_mutation():
    stack = bare_stack()
    stack.native = None
    stack.adapter, stack.database, stack.proxy = Mock(), Mock(), Mock()
    with pytest.raises(AdapterError, match="owned native"):
        stack.reset_test_state()
    stack.adapter.kill.assert_not_called()
    stack.database.reset.assert_not_called()
    stack.proxy.disarm.assert_not_called()


def test_experiments_search_reads_more_than_one_hundred():
    stack = bare_stack()
    first = [{"experiment_id": str(i), "name": f"experiment-{i}"} for i in range(100)]
    last = {"experiment_id": "100", "name": "experiment-100"}
    stack._native_post = Mock(side_effect=[{"experiments": first, "next_page_token": "next"}, {"experiments": [last]}])
    assert stack.native_experiments() == [*first, last]
    assert stack._native_post.call_args_list[1].args[1] == {"max_results": 100, "view_type": "ALL", "page_token": "next"}


def test_runs_search_reads_all_experiments_and_run_pages():
    stack = bare_stack()
    first = [{"info": {"run_id": f"run-{i}", "experiment_id": "0"}} for i in range(100)]
    last = {"info": {"run_id": "run-100", "experiment_id": "0"}}
    later = {"info": {"run_id": "later", "experiment_id": "100"}}
    experiments = [{"experiment_id": str(i)} for i in range(100)]
    stack._native_post = Mock(side_effect=[
        {"experiments": experiments, "next_page_token": "experiments-next"},
        {"experiments": [{"experiment_id": "100"}]},
        {"runs": first, "next_page_token": "runs-next"},
        {"runs": [last]},
        *({"runs": []} for _ in range(99)),
        {"runs": [later]},
    ])
    assert stack.native_runs() == [*first, last, later]
    assert stack._native_post.call_args_list[3].args[1] == {
        "experiment_ids": ["0"], "max_results": 100, "run_view_type": "ALL", "page_token": "runs-next",
    }


@pytest.mark.parametrize(("method", "key"), [("native_experiments", "experiments"), ("native_runs", "runs")])
def test_tracking_search_rejects_repeated_page_token(method, key):
    stack = bare_stack()
    if key == "runs":
        stack.native_experiments = Mock(return_value=[{"experiment_id": "0"}])
    stack._native_post = Mock(side_effect=[{key: [], "next_page_token": "same"}, {key: [], "next_page_token": "same"}])
    with pytest.raises(AdapterError, match="repeated page token"):
        getattr(stack, method)()
    assert stack._native_post.call_count == 2


@pytest.mark.parametrize("method", ["native_experiments", "native_runs"])
@pytest.mark.parametrize("error", [requests.HTTPError("HTTP failure"), requests.Timeout("request timed out")])
def test_tracking_search_propagates_http_and_transport_errors(method, error):
    stack = bare_stack()
    stack._native_post = Mock(side_effect=error)
    with pytest.raises(type(error)):
        getattr(stack, method)()


@pytest.mark.parametrize(("method", "key"), [("native_experiments", "experiments"), ("native_runs", "runs")])
def test_later_page_failure_never_returns_partial_tracking_observation(method, key):
    stack = bare_stack()
    prefix = [{"experiments": [{"experiment_id": "0"}]}] if key == "runs" else []
    stack._native_post = Mock(side_effect=[
        *prefix, {key: [{"experiment_id": "0"}], "next_page_token": "next"}, requests.HTTPError("later page failed"),
    ])
    with pytest.raises(requests.HTTPError, match="later page failed"):
        getattr(stack, method)()


@pytest.mark.parametrize("method", ["native_experiments", "native_runs"])
def test_tracking_search_cancellation_prevents_requests(method):
    stack = bare_stack()
    stack._native_post = Mock()
    cancelled = threading.Event()
    cancelled.set()
    with pytest.raises(AdapterError, match="cancelled"):
        getattr(stack, method)(cancel=cancelled)
    stack._native_post.assert_not_called()


@pytest.mark.parametrize("method", ["native_experiments", "native_runs"])
def test_tracking_search_timeout_prevents_requests(method):
    stack = bare_stack()
    stack._native_post = Mock()
    with pytest.raises(AdapterError, match="timed out"):
        getattr(stack, method)(timeout=0)
    stack._native_post.assert_not_called()


def test_owned_native_process_uses_isolated_paths_and_environment(monkeypatch, tmp_path):
    monkeypatch.setenv("MLFLOW_REGISTRY_STORE_URI", "postgresql://external.invalid/registry")
    process = Mock()
    process.poll.return_value = None
    popen = Mock(return_value=process)
    signals = Mock()
    monkeypatch.setattr(scaffolding.os, "killpg", signals)
    monkeypatch.setattr(scaffolding.subprocess, "Popen", popen)
    monkeypatch.setattr(scaffolding, "_free_port", Mock(return_value=5111))
    monkeypatch.setattr(scaffolding.requests, "get", Mock(side_effect=[Mock(status_code=200), Mock(status_code=200, text="3.18.0")]))
    native = scaffolding.NativeRegistryProcess.start(Path("/supplied/mlflow"), tmp_path)
    try:
        argv = popen.call_args.args[0]
        assert argv[:2] == ["/supplied/mlflow", "server"]
        assert argv[argv.index("--backend-store-uri") + 1] == f"sqlite:///{tmp_path / 'mlflow.db'}"
        assert argv[argv.index("--registry-store-uri") + 1] == f"sqlite:///{tmp_path / 'mlflow.db'}"
        assert argv[argv.index("--default-artifact-root") + 1] == (tmp_path / "artifacts").as_uri()
        assert argv[argv.index("--host") + 1] == "127.0.0.1"
        assert argv[argv.index("--port") + 1] == "5111"
        assert popen.call_args.kwargs["env"]["MLFLOW_ALLOWED_HOST_ADDRESSED_ARTIFACT_SCHEMES"] == "https"
        assert native.url == "http://127.0.0.1:5111"
    finally:
        native.stop()
    signals.assert_called_once_with(process.pid, signal.SIGTERM)
    assert popen.call_args.kwargs["stdout"].closed


@pytest.mark.parametrize("response", [Mock(status_code=200, text="3.17.0"), Mock(status_code=503, text="3.18.0")])
def test_wrong_native_version_or_http_failure_closes_started_process(monkeypatch, tmp_path, response):
    process = Mock()
    process.poll.return_value = None
    popen = Mock(return_value=process)
    signals = Mock()
    monkeypatch.setattr(scaffolding.os, "killpg", signals)
    monkeypatch.setattr(scaffolding.subprocess, "Popen", popen)
    monkeypatch.setattr(scaffolding, "_free_port", Mock(return_value=5111))
    monkeypatch.setattr(scaffolding.requests, "get", Mock(side_effect=[Mock(status_code=200), response]))
    with pytest.raises(AdapterError, match="3.18.0|version"):
        scaffolding.NativeRegistryProcess.start(Path("/mlflow"), tmp_path)
    signals.assert_called_once_with(process.pid, signal.SIGTERM)
    assert popen.call_args.kwargs["stdout"].closed


def test_native_spawn_failure_closes_log(monkeypatch, tmp_path):
    popen = Mock(side_effect=OSError("cannot execute"))
    monkeypatch.setattr(scaffolding.subprocess, "Popen", popen)
    monkeypatch.setattr(scaffolding, "_free_port", Mock(return_value=5111))
    with pytest.raises(OSError, match="cannot execute"):
        scaffolding.NativeRegistryProcess.start(Path("/mlflow"), tmp_path)
    assert popen.call_args.kwargs["stdout"].closed


def test_missing_native_binary_fails_with_setup_instructions(monkeypatch):
    monkeypatch.delenv("MLFLOW_TEST_BINARY", raising=False)
    monkeypatch.setenv("MLFLOW_REGISTRY_URL", "http://external.invalid")
    stack = bare_stack()
    with pytest.raises(AdapterError, match="MLFLOW_TEST_BINARY.*3.18.0"):
        stack._resolve_native_binary()


def test_native_stop_kills_process_after_grace_period(monkeypatch, tmp_path):
    process = Mock()
    process.poll.return_value = None
    process.wait.side_effect = [subprocess.TimeoutExpired("mlflow", 15), 0]
    signals = Mock()
    monkeypatch.setattr(scaffolding.os, "killpg", signals)
    monkeypatch.setattr(scaffolding.subprocess, "Popen", Mock(return_value=process))
    monkeypatch.setattr(scaffolding, "_free_port", Mock(return_value=5111))
    monkeypatch.setattr(scaffolding.requests, "get", Mock(side_effect=[Mock(status_code=200), Mock(status_code=200, text="3.18.0")]))
    native = scaffolding.NativeRegistryProcess.start(Path("/mlflow"), tmp_path)
    native.stop()
    native.stop()
    assert signals.call_args_list == [call(process.pid, signal.SIGTERM), call(process.pid, signal.SIGKILL)]


async def test_client_fixture_cleanup_runs_after_test_failure():
    request = Mock()
    request.config.getoption.return_value = True
    stack = Mock()
    request.getfixturevalue.return_value = stack
    request.node.path = Path("/tests/mlflow/test_example.py")
    fixture = cleanup(lambda: "client")(Path("/repo"), request)
    assert await anext(fixture) == "client"
    with pytest.raises(RuntimeError, match="test failed"):
        await fixture.athrow(RuntimeError("test failed"))
    stack.reset_test_state.assert_called_once_with(recreate_native=False)


def test_reused_fixed_names_reset_only_owned_native_database(resources, monkeypatch):
    native, database, proxy, _, _ = resources
    stack = MLflowStack()
    fresh = Mock(url="http://127.0.0.1:5002")
    monkeypatch.setattr(scaffolding.NativeRegistryProcess, "start", Mock(return_value=fresh))
    try:
        stack.reset_test_state(recreate_native=True)
        native.stop.assert_called_once_with()
        assert stack.registry_uri == fresh.url
        assert proxy.upstream == fresh.url
        database.reset.assert_called_once_with()
    finally:
        stack.close()
    fresh.stop.assert_called_once_with()


@pytest.mark.parametrize("final_page", [True, False])
def test_cancellation_during_page_never_returns_partial_observation(final_page):
    stack = bare_stack()
    cancelled = threading.Event()

    def respond(*args, **kwargs):
        cancelled.set()
        return {"experiments": [{"experiment_id": "0"}], **({} if final_page else {"next_page_token": "next"})}

    stack._native_post = Mock(side_effect=respond)
    with pytest.raises(AdapterError, match="cancelled"):
        stack.native_experiments(cancel=cancelled)
    assert stack._native_post.call_count == 1


def test_timeout_during_last_page_never_returns_partial_observation(monkeypatch):
    stack = bare_stack()
    stack._native_post = Mock(return_value={"experiments": [{"experiment_id": "0"}]})
    monkeypatch.setattr(scaffolding.time, "monotonic", Mock(side_effect=[0.0, 0.1, 2.0]))
    with pytest.raises(AdapterError, match="timed out"):
        stack.native_experiments(timeout=1.0)


@pytest.mark.parametrize("operation", ["record", "delete", "create", "reset", "recreate"])
def test_external_verifier_never_mutates_external_target(monkeypatch, operation):
    monkeypatch.setenv("MLFLOW_REGISTRY_URL", "http://external.invalid")
    monkeypatch.setattr(MLflowStack, "_verify_native_server", Mock())
    http_delete, http_post = Mock(), Mock()
    monkeypatch.setattr(scaffolding.requests, "delete", http_delete)
    monkeypatch.setattr(scaffolding.requests, "post", http_post)
    verifier = MLflowStack.external_verifier()
    operations = {
        "record": lambda: verifier.record_owned_model("external-model"),
        "delete": lambda: verifier.native_delete_model("external-model"),
        "create": lambda: verifier._native_post("/api/2.0/mlflow/registered-models/create", {"name": "external-model"}),
        "reset": lambda: verifier.reset_test_state(),
        "recreate": lambda: verifier.reset_test_state(recreate_native=True),
    }
    with pytest.raises(AdapterError, match="owned native|read-only"):
        operations[operation]()
    http_delete.assert_not_called()
    http_post.assert_not_called()


def test_database_startup_exception_closes_created_container(monkeypatch):
    monkeypatch.setattr(scaffolding, "_container_runtime", Mock(return_value="docker"))
    monkeypatch.setattr(scaffolding, "_free_port", Mock(return_value=5333))
    run = Mock(return_value=Mock(stdout="owned-container"))
    monkeypatch.setattr(scaffolding, "_run", run)
    monkeypatch.setattr(scaffolding.CompatDatabase, "wait_ready", Mock(side_effect=RuntimeError("probe failed")))
    with pytest.raises(RuntimeError, match="probe failed"):
        scaffolding.start_compat_database("postgres")
    assert run.call_args.args[0] == ["docker", "rm", "-f", "owned-container"]


def test_adapter_spawn_failure_closes_allocated_log(monkeypatch):
    popen = Mock(side_effect=OSError("cannot execute"))
    monkeypatch.setattr(scaffolding.subprocess, "Popen", popen)
    managed = scaffolding.ManagedAdapter(Path("/mr"), 8080, "http://native.invalid", Mock())
    with pytest.raises(OSError, match="cannot execute"):
        managed.start()
    assert popen.call_args.kwargs["stdout"].closed


def test_proxy_thread_startup_failure_closes_allocated_server(monkeypatch):
    server = Mock()
    monkeypatch.setattr(scaffolding, "_free_port", Mock(return_value=5111))
    monkeypatch.setattr("http.server.ThreadingHTTPServer", Mock(return_value=server))
    monkeypatch.setattr("threading.Thread", Mock(side_effect=RuntimeError("cannot start thread")))
    with pytest.raises(RuntimeError, match="cannot start thread"):
        scaffolding.GatedNativeProxy("http://native.invalid")
    server.server_close.assert_called_once_with()


def test_close_continues_cleanup_after_native_delete_failure(resources):
    native, database, proxy, managed, delete = resources
    stack = MLflowStack()
    stack.owned_model_names = {"owned-model"}
    delete.side_effect = requests.ConnectionError("native unavailable")
    with pytest.raises(requests.ConnectionError, match="native unavailable"):
        stack.close()
    managed.stop.assert_called_once_with()
    proxy.stop.assert_called_once_with()
    database.stop.assert_called_once_with()
    native.stop.assert_called_once_with()


def test_follower_gate_blocks_later_create_but_passes_unrelated_requests(monkeypatch):
    upstream = Mock(return_value=Mock(status_code=200, headers={}, content=b"{}"))
    monkeypatch.setattr(requests, "request", upstream)
    proxy = scaffolding.GatedNativeProxy("http://unused.invalid")
    proxy.arm(hold_followers=True)

    def post(path):
        connection = http.client.HTTPConnection("127.0.0.1", proxy.port, timeout=5)
        try:
            connection.request("POST", path, body=b"{}")
            result = connection.getresponse()
            assert result.status == 200
            assert result.read() == b"{}"
        finally:
            connection.close()

    first = threading.Thread(target=post, args=("/model-versions/create",))
    follower = threading.Thread(target=post, args=("/model-versions/create",))
    try:
        first.start()
        assert proxy.holding.wait(5)
        follower.start()
        assert proxy.followers_holding.wait(5)
        post("/model-versions/set-tag")
        assert upstream.call_count == 2
        assert not proxy.followers_forwarded.is_set()
        assert first.is_alive()
        assert follower.is_alive()
        proxy.disarm()
        first.join(5)
        follower.join(5)
        assert not first.is_alive()
        assert not follower.is_alive()
        assert proxy.forwarded.is_set()
        assert proxy.followers_forwarded.is_set()
        assert upstream.call_count == 3
    finally:
        proxy.stop()
        first.join(5)
        if follower.ident is not None:
            follower.join(5)
