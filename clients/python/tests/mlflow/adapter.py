"""Test-owned MLflow backend stack.

Destructive acceptance runs own a native MLflow subprocess and its SQLite
database, a compatibility database container, and the adapter and proxy.
Cleanup uses explicitly recorded model names. External native targets are
available only through the non-destructive verifier.
"""

from __future__ import annotations

import contextlib
import json
import os
import shlex
import signal
import socket
import subprocess
import tempfile
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from threading import Event
from typing import IO
from urllib.parse import urlparse

import requests

PINNED_MLFLOW_VERSION = "3.18.0"
TEST_MODEL_PREFIX = "mr-e2e-"
COMPAT_DATABASE = "mr_mlflow_compat"
STARTUP_TIMEOUT = 90.0
HTTP_TIMEOUT = 30.0


class AdapterError(RuntimeError):
    """A required test prerequisite or owned resource failed."""


def _run(cmd: list[str], *, timeout: float = 60.0, check: bool = True) -> subprocess.CompletedProcess:
    proc = subprocess.run(  # noqa: S603
        cmd,
        capture_output=True,
        text=True,
        timeout=timeout,
        check=False,
    )
    if check and proc.returncode != 0:
        command = " ".join(shlex.quote(c) for c in cmd)
        raise AdapterError(f"command failed ({command}):\n{proc.stdout}\n{proc.stderr}")  # noqa: EM102
    return proc


def _container_runtime() -> str:
    for runtime in ("podman", "docker"):
        if _run(["which", runtime], check=False).returncode == 0:
            return runtime
    message = (
        "no container runtime found: install podman (or docker) "
        "so the tests can own a disposable compatibility database"
    )
    raise AdapterError(message)


def _free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


@dataclass
class CompatDatabase:
    """A fixture-owned compatibility database container."""

    runtime: str
    container_id: str
    engine: str  # "mysql" or "postgres"
    port: int
    password: str = field(default_factory=lambda: f"mr-{uuid.uuid4().hex[:12]}")

    @property
    def dsn(self) -> str:
        if self.engine == "mysql":
            return f"root:{self.password}@tcp(127.0.0.1:{self.port})/{COMPAT_DATABASE}?parseTime=true"
        return f"postgres://postgres:{self.password}@127.0.0.1:{self.port}/{COMPAT_DATABASE}?sslmode=disable"

    def exec(self, sql_cmd: str) -> None:
        if self.engine == "mysql":
            _run([self.runtime, "exec", self.container_id, "mysql", "-uroot", f"-p{self.password}", "-e", sql_cmd])
        else:
            _run([self.runtime, "exec", self.container_id, "psql", "-U", "postgres", "-c", sql_cmd])

    def query(self, sql_cmd: str) -> str:
        """Read fixture-owned journal state without changing application state."""
        if self.engine == "mysql":
            command = ["mysql", "-uroot", f"-p{self.password}", "-NB", COMPAT_DATABASE, "-e", sql_cmd]
        else:
            command = ["psql", "-U", "postgres", "-d", COMPAT_DATABASE, "-At", "-c", sql_cmd]
        return _run([self.runtime, "exec", self.container_id, *command]).stdout.strip()

    def wait_ready(self) -> None:
        deadline = time.monotonic() + STARTUP_TIMEOUT
        probe = (
            ["mysqladmin", "ping", "-uroot", f"-p{self.password}", "--silent"]
            if self.engine == "mysql"
            else ["pg_isready", "-U", "postgres"]
        )
        while time.monotonic() < deadline:
            if _run([self.runtime, "exec", self.container_id, *probe], check=False).returncode == 0:
                return
            time.sleep(1.0)
        message = f"{self.engine} compatibility database did not become ready within {STARTUP_TIMEOUT}s"
        raise AdapterError(message)

    def reset(self) -> None:
        """Drop and recreate the isolated compatibility database."""
        if self.engine == "mysql":
            self.exec(f"DROP DATABASE IF EXISTS {COMPAT_DATABASE}; CREATE DATABASE {COMPAT_DATABASE};")
        else:
            # postgres cannot drop the database it is connected to
            self.exec(
                "SELECT pg_terminate_backend(pid) FROM pg_stat_activity "  # noqa: S608
                "WHERE datname = '" + COMPAT_DATABASE + "' AND pid <> pg_backend_pid();"
            )
            self.exec(f"DROP DATABASE IF EXISTS {COMPAT_DATABASE};")
            self.exec(f"CREATE DATABASE {COMPAT_DATABASE};")

    def stop(self) -> None:
        _run([self.runtime, "rm", "-f", self.container_id], check=False, timeout=60.0)


def start_compat_database(engine: str) -> CompatDatabase:
    runtime = _container_runtime()
    port = _free_port()
    password = f"mr-{uuid.uuid4().hex[:16]}"
    if engine == "mysql":
        image, args = "mysql:8.3", [
            "-e", f"MYSQL_ROOT_PASSWORD={password}",
            "-e", f"MYSQL_DATABASE={COMPAT_DATABASE}",
        ]
    elif engine == "postgres":
        image, args = "postgres:16", [
            "-e", f"POSTGRES_PASSWORD={password}",
            "-e", f"POSTGRES_DB={COMPAT_DATABASE}",
        ]
    else:
        message = f"unsupported compatibility database engine {engine!r}: use mysql or postgres"
        raise AdapterError(message)
    container_port = "3306" if engine == "mysql" else "5432"
    proc = _run(
        [runtime, "run", "-d", "--rm", "-p", f"127.0.0.1:{port}:{container_port}", *args, image],
        timeout=300.0,
    )
    database = CompatDatabase(
        runtime=runtime,
        container_id=proc.stdout.strip(),
        engine=engine,
        port=port,
        password=password,
    )
    try:
        database.wait_ready()
    except BaseException:
        database.stop()
        raise
    return database


class GatedNativeProxy:
    """A forwarding proxy between the adapter and the native server.

    The adapter always addresses the native registry through this
    proxy, so the compatibility store's registry-target fingerprint
    stays constant for the whole session. The gate, when armed, holds
    the response to one native model-version create after MLflow has
    committed it — reproducing a post-commit, pre-publication
    interruption without any production failpoint.
    """

    def __init__(self, upstream: str) -> None:
        import threading
        from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

        self.upstream = upstream.rstrip("/")
        self.port = _free_port()
        self.armed = False
        self.gate_path = "/model-versions/create"
        self.gate_ordinal = 1
        self.gate_seen = 0
        self.held_response: dict = {}
        self.held_request: dict = {}
        self.held_status: int | None = None
        self.gate_before = False
        self.hold_followers = False
        self.followers_holding = threading.Event()
        self.followers_forwarded = threading.Event()
        self.forwarded = threading.Event()
        self.gate_lock = threading.Lock()
        self.holding = threading.Event()
        self.release = threading.Event()
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def _forward(self) -> None:
                outer.forward(self)

            do_GET = _forward
            do_POST = _forward
            do_PUT = _forward
            do_PATCH = _forward
            do_DELETE = _forward

            def log_message(self, *args) -> None:  # silence
                return

        self.server = ThreadingHTTPServer(("127.0.0.1", self.port), Handler)
        try:
            self.server.daemon_threads = True
            self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
            self.thread.start()
        except BaseException:
            self.server.server_close()
            raise

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    def forward(self, handler) -> None:
        import requests as requests_lib

        length = int(handler.headers.get("Content-Length", 0))
        body = handler.rfile.read(length) if length else None
        gated, follower = self.select_gate(handler.command, handler.path)
        if gated:
            self.held_request = json.loads(body or "{}")
        if not self.wait_before_forward(handler, gated, follower):
            return
        response = requests_lib.request(
            handler.command,
            self.upstream + handler.path,
            data=body,
            headers={
                key: value
                for key, value in handler.headers.items()
                if key.lower() not in ("host", "content-length")
            },
            timeout=HTTP_TIMEOUT,
        )
        if response.status_code >= 400:
            print(f"owned native {handler.path}: {response.status_code} {response.text}", flush=True)
        if not self.record_gate_response(handler, response, gated, follower):
            return
        self.send_native_response(handler, response)

    def wait_before_forward(self, handler, gated: bool, follower: bool) -> bool:
        if gated and self.gate_before:
            self.holding.set()
            if not self.wait_gate(handler):
                return False
        if follower:
            self.followers_holding.set()
            if not self.wait_gate(handler):
                return False
        return True

    def record_gate_response(self, handler, response, gated: bool, follower: bool) -> bool:
        if follower:
            self.followers_forwarded.set()
        if gated:
            self.held_status = response.status_code
            self.held_response = response.json()
            if not self.gate_before:
                self.holding.set()
                if not self.wait_gate(handler):
                    return False
            self.forwarded.set()
        return True

    @staticmethod
    def send_native_response(handler, response) -> None:
        handler.send_response(response.status_code)
        for key, value in response.headers.items():
            if key.lower() not in (
                "content-length",
                "transfer-encoding",
                "content-encoding",
                "connection",
            ):
                handler.send_header(key, value)
        handler.send_header("Content-Length", str(len(response.content)))
        handler.end_headers()
        handler.wfile.write(response.content)

    def wait_gate(self, handler) -> bool:
        if self.release.wait(timeout=120.0):
            return True
        handler.send_error(504, "owned native gate expired without release")
        return False

    def select_gate(self, method: str, path: str) -> tuple[bool, bool]:
        with self.gate_lock:
            matching = self.armed and method == "POST" and path.endswith(self.gate_path)
            if matching:
                self.gate_seen += 1
            return (matching and self.gate_seen == self.gate_ordinal,
                    matching and self.hold_followers and self.gate_seen > self.gate_ordinal)

    def arm(self, *, path: str = "/model-versions/create", ordinal: int = 1,
            before: bool = False, hold_followers: bool = False) -> None:
        """Hold one selected native response after the upstream commit."""
        if ordinal < 1:
            message = "gate ordinal must be positive"
            raise ValueError(message)
        self.holding.clear()
        self.release.clear()
        self.gate_path = path
        self.gate_ordinal = ordinal
        self.gate_seen = 0
        self.held_response = {}
        self.held_request = {}
        self.held_status = None
        self.gate_before = before
        self.hold_followers = hold_followers
        self.followers_holding.clear()
        self.followers_forwarded.clear()
        self.forwarded.clear()
        self.armed = True

    def disarm(self) -> None:
        self.release.set()
        self.armed = False

    def stop(self) -> None:
        self.disarm()
        self.server.shutdown()
        self.server.server_close()


class ManagedAdapter:
    """The model-registry proxy running against the MLflow backend."""

    def __init__(self, binary: Path, port: int, registry_uri: str, database: CompatDatabase) -> None:
        self.binary = binary
        self.port = port
        self.registry_uri = registry_uri
        self.database = database
        self.process: subprocess.Popen | None = None
        self._log: IO[bytes] | None = None
        self._default_registry_uri: str | None = None

    @property
    def log_path(self) -> str | None:
        """The adapter's log file, for failure diagnostics."""
        return self._log.name if self._log else None

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    def start(self, registry_uri: str | None = None, request_timeout: str = "5s") -> None:
        if self.process is not None:
            raise AdapterError("adapter process is already running")  # noqa: EM101
        if registry_uri is None:
            registry_uri = self._default_registry_uri or self.registry_uri
        self._default_registry_uri = registry_uri
        cmd = [
            str(self.binary), "proxy",
            "--port", str(self.port),
            "--backend", "mlflow",
            "--mlflow-registry-uri", registry_uri,
            "--mlflow-database-type", self.database.engine,
            "--mlflow-database-dsn", self.database.dsn,
            "--mlflow-refresh-interval", "1s",
            "--mlflow-request-timeout", request_timeout,
        ]
        # Drain adapter output to a file: an undrained pipe would fill
        # its buffer and wedge the process mid-suite.
        self._log = tempfile.NamedTemporaryFile(  # noqa: SIM115
            prefix="mr-mlflow-adapter-", suffix=".log", delete=False
        )
        try:
            self.process = subprocess.Popen(  # noqa: S603
                cmd, stdout=self._log, stderr=subprocess.STDOUT, text=True
            )
            if not self._wait_ready():
                tail = self._log_tail()
                exit_code = self.process.poll() if self.process is not None else None
                message = (
                    f"adapter did not become ready within {STARTUP_TIMEOUT}s "
                    f"(process exit code: {exit_code}); "
                    "check that the model-registry binary supports --backend=mlflow; "
                    f"adapter log tail: {tail}"
                )
                raise AdapterError(message)
        except BaseException:
            self.stop()
            raise

    def _log_tail(self) -> str:
        if not self._log:
            return "(no log)"
        try:
            with open(self._log.name) as handle:
                return handle.read()[-4000:]
        except OSError:
            return "(log unreadable)"

    def _wait_ready(self) -> bool:
        deadline = time.monotonic() + STARTUP_TIMEOUT
        while time.monotonic() < deadline:
            if self.process is not None and self.process.poll() is not None:
                return False
            try:
                response = requests.get(f"{self.url}/readyz/health", timeout=5.0)
                if response.status_code == 200:
                    return True
            except requests.exceptions.RequestException:
                pass
            time.sleep(0.5)
        return False

    def _close_log(self) -> None:
        if self._log is not None:
            with contextlib.suppress(OSError):
                self._log.close()
            self._log = None

    def stop(self) -> None:
        if self.process is None:
            self._close_log()
            return
        process, self.process = self.process, None
        process.terminate()
        try:
            process.wait(timeout=15.0)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=15.0)
        self._close_log()

    def kill(self) -> None:
        """Stop without graceful shutdown, as a crash would."""
        if self.process is None:
            return
        process, self.process = self.process, None
        process.kill()
        process.wait(timeout=15.0)
        self._close_log()

    def restart(self, registry_uri: str | None = None) -> None:
        """Stop ( however it died ) and start again against the same compatibility database."""
        if self.process is not None:
            self.kill()
        self.start(registry_uri=registry_uri)


class NativeRegistryProcess:
    """A session-owned native server with isolated SQL and artifact paths."""

    port: int
    process: subprocess.Popen | None
    _log: IO[bytes] | None

    @classmethod
    def start(cls, binary: Path, workdir: Path) -> NativeRegistryProcess:
        native = cls()
        native.port = _free_port()
        native.process = None
        native._log = None
        workdir.mkdir(parents=True, exist_ok=True)
        artifacts = workdir / "artifacts"
        artifacts.mkdir()
        try:
            native._log = (workdir / "native.log").open("wb")
            environment = os.environ.copy()
            environment["MLFLOW_ALLOWED_HOST_ADDRESSED_ARTIFACT_SCHEMES"] = "https"
            native.process = subprocess.Popen(  # noqa: S603
                [
                    str(binary), "server",
                    "--backend-store-uri", f"sqlite:///{workdir / 'mlflow.db'}",
                    "--registry-store-uri", f"sqlite:///{workdir / 'mlflow.db'}",
                    "--default-artifact-root", artifacts.as_uri(),
                    "--artifacts-destination", str(artifacts),
                    "--host", "127.0.0.1", "--port", str(native.port),
                    "--workers", "1",
                ],
                cwd=workdir, env=environment, stdout=native._log,
                stderr=subprocess.STDOUT, start_new_session=True,
            )
            native._wait_ready()
        except BaseException:
            native.stop()
            raise
        return native

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    def _wait_ready(self) -> None:
        deadline = time.monotonic() + STARTUP_TIMEOUT
        while time.monotonic() < deadline:
            if self.process is not None and self.process.poll() is not None:
                break
            try:
                health = requests.get(f"{self.url}/health", timeout=5.0)
                if health.status_code == 200:
                    _verify_native_version(self.url)
                    return
            except requests.exceptions.ConnectionError:
                pass
            time.sleep(0.5)
        message = f"owned MLflow server did not become ready; check MLFLOW_TEST_BINARY and {self._log.name if self._log else 'native log'}"
        raise AdapterError(message)

    def stop(self) -> None:
        try:
            if self.process is not None:
                process, self.process = self.process, None
                # MLflow can launch workers: terminate its entire owned process group.
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=15.0)
                except subprocess.TimeoutExpired:
                    with contextlib.suppress(ProcessLookupError):
                        os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=15.0)
        finally:
            if self._log is not None:
                self._log.close()
                self._log = None


def _verify_native_version(registry_uri: str) -> None:
    response = requests.get(f"{registry_uri}/version", timeout=HTTP_TIMEOUT)
    if response.status_code != 200 or response.text.strip() != PINNED_MLFLOW_VERSION:
        message = (
            f"native /version must report MLflow {PINNED_MLFLOW_VERSION}, got "
            f"HTTP {response.status_code}: {response.text.strip()}; supply that version via MLFLOW_TEST_BINARY"
        )
        raise AdapterError(message)


class MLflowStack:
    """Everything the --mlflow-backend tests own, with owned cleanup."""

    _external = False

    def __init__(self, engine: str = "mysql") -> None:
        self.engine = engine
        self.owned_model_names: set[str] = set()
        self.native: NativeRegistryProcess | None = None
        self.binary = self._resolve_binary()
        native_binary = self._resolve_native_binary()
        self.adapter_host, self.adapter_port = self._resolve_adapter_address()
        # Register cleanup immediately after every successful allocation, including
        # constructors that precede adapter startup. ExitStack also closes all
        # remaining resources if one cleanup callback fails.
        with contextlib.ExitStack() as resources:
            workdir = resources.enter_context(tempfile.TemporaryDirectory(prefix="mr-mlflow-native-"))
            self._native_workdir = Path(workdir)
            self._native_binary = native_binary
            self._native_resources = resources.enter_context(contextlib.ExitStack())
            self.native = NativeRegistryProcess.start(native_binary, self._native_workdir / uuid.uuid4().hex)
            self._native_resources.callback(self.native.stop)
            self.registry_uri = self.native.url
            self.database = start_compat_database(engine)
            resources.callback(self.database.stop)
            self.proxy = GatedNativeProxy(self.registry_uri)
            resources.callback(self.proxy.stop)
            resources.callback(self.delete_test_owned_native_models)
            self.adapter = ManagedAdapter(self.binary, self.adapter_port, self.registry_uri, self.database)
            resources.callback(self.adapter.stop)
            self.adapter.start(registry_uri=self.proxy.url)
            self._resources = resources.pop_all()

    @classmethod
    def external_verifier(cls) -> MLflowStack:
        """Read-only probes of an externally managed MLFLOW_REGISTRY_URL."""
        verifier = cls.__new__(cls)
        verifier.native = None
        verifier._external = True
        verifier.owned_model_names = set()
        verifier.registry_uri = os.environ.get("MLFLOW_REGISTRY_URL", "").rstrip("/")
        if not verifier.registry_uri:
            message = "MLFLOW_REGISTRY_URL is required for external native contract probes"
            raise AdapterError(message)
        verifier._verify_native_server()
        return verifier

    def _resolve_native_binary(self) -> Path:
        supplied = os.environ.get("MLFLOW_TEST_BINARY", "")
        binary = Path(supplied).resolve() if supplied else None
        if binary is None or not binary.is_file() or not os.access(binary, os.X_OK):
            message = (
                f"MLFLOW_TEST_BINARY must point to an executable MLflow {PINNED_MLFLOW_VERSION} installation; "
                "prepare it separately and set MLFLOW_TEST_BINARY=<path-to-mlflow>. "
                "Acceptance tests start their own native server and never reset MLFLOW_REGISTRY_URL."
            )
            raise AdapterError(message)
        return binary

    def _resolve_binary(self) -> Path:
        binary_env = os.environ.get("MR_BINARY", "")
        if binary_env:
            binary = Path(binary_env)
            if not binary.is_absolute():
                binary = (Path.cwd() / binary).resolve()
        else:
            binary = Path(__file__).resolve().parents[3] / "model-registry"
        if not (binary.is_file() and os.access(binary, os.X_OK)):
            message = (
                f"model-registry binary not found or not executable at {binary}: build it with "
                "'make build/compile' and pass MR_BINARY=<path>"
            )
            raise AdapterError(message)
        return binary

    def _resolve_adapter_address(self) -> tuple[str, int]:
        adapter_url = os.environ.get("MR_URL")
        if not adapter_url:
            return "http://127.0.0.1", _free_port()
        parsed = urlparse(adapter_url)
        if not parsed.hostname:
            raise AdapterError(f"MR_URL must be an http URL, got {adapter_url!r}")  # noqa: EM102
        port = parsed.port or 8080
        with socket.socket() as probe:
            # SO_REUSEADDR: sockets from a just-stopped adapter linger in
            # TIME_WAIT and would fail the probe without it.
            probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            try:
                probe.bind((parsed.hostname, port))
            except OSError as error:
                message = f"explicit MR_URL port {port} is unavailable"
                raise AdapterError(message) from error
        return f"{parsed.scheme}://{parsed.hostname}", port

    def _verify_native_server(self) -> None:
        try:
            health = requests.get(f"{self.registry_uri}/health", timeout=HTTP_TIMEOUT)
        except requests.exceptions.RequestException as err:
            message = (
                f"MLFLOW_REGISTRY_URL ({self.registry_uri}) is not reachable: {err}; "
                "start a disposable MLflow server before running these tests"
            )
            raise AdapterError(message) from err
        if health.status_code != 200:
            message = (
                f"MLflow server at {self.registry_uri} failed its /health check "
                f"with {health.status_code}"
            )
            raise AdapterError(message)
        _verify_native_version(self.registry_uri)

    # -- native REST ( independent verification, never through the adapter ) --

    def _native_get(self, path: str, params: dict | None = None):
        response = requests.get(f"{self.registry_uri}{path}", params=params, timeout=HTTP_TIMEOUT)
        response.raise_for_status()
        return response.json()

    def _native_post(self, path: str, body: dict | None = None, *, timeout: float = HTTP_TIMEOUT):
        if getattr(self, "_external", False) and path not in (
            "/api/2.0/mlflow/experiments/search", "/api/2.0/mlflow/runs/search",
        ):
            raise AdapterError("external native verifier is read-only")  # noqa: EM101
        response = requests.post(
            f"{self.registry_uri}{path}", json=body or {}, timeout=timeout
        )
        response.raise_for_status()
        return response.json() if response.content else {}

    def native_model_names(self) -> set[str]:
        """Every native registered-model name, all pages."""
        names: set[str] = set()
        token = None
        while True:
            params = {"max_results": 100}
            if token:
                params["page_token"] = token
            page = self._native_get("/api/2.0/mlflow/registered-models/search", params=params)
            for model in page.get("registered_models", []):
                names.add(model["name"])
            token = page.get("next_page_token")
            if not token:
                return names

    def native_registered_model(self, name: str) -> dict:
        return self._native_get(
            "/api/2.0/mlflow/registered-models/get", params={"name": name}
        )["registered_model"]

    def native_model_versions(self, name: str) -> list[dict]:
        """Every native model version of one model, all pages.

        The name is matched exactly on each returned version rather than
        interpolated into a filter literal: accepted model names may
        contain quotes, whose server-side escaping rules are not part of
        the contract under verification.
        """
        versions: list[dict] = []
        token = None
        while True:
            params = {"max_results": 100}
            if token:
                params["page_token"] = token
            page = self._native_get("/api/2.0/mlflow/model-versions/search", params=params)
            versions.extend(v for v in page.get("model_versions", []) if v["name"] == name)
            token = page.get("next_page_token")
            if not token:
                return versions

    def native_version_tags(self, name: str, version: str) -> dict[str, str]:
        for native in self.native_model_versions(name):
            if native["version"] == version:
                return {tag["key"]: tag["value"] for tag in native.get("tags", [])}
        message = f"native model version {name}/{version} not found"
        raise AdapterError(message)

    def native_alias(self, name: str, alias: str) -> str | None:
        try:
            model = self._native_get(
                "/api/2.0/mlflow/registered-models/alias", params={"name": name, "alias": alias}
            )
        except requests.exceptions.HTTPError:
            return None
        return model["model_version"]["version"]

    def native_delete_model(self, name: str) -> None:
        if getattr(self, "_external", False):
            raise AdapterError("external native verifier is read-only")  # noqa: EM101
        response = requests.delete(
            f"{self.registry_uri}/api/2.0/mlflow/registered-models/delete",
            json={"name": name},
            timeout=HTTP_TIMEOUT,
        )
        response.raise_for_status()

    def _tracking_pages(self, path: str, key: str, body: dict, deadline: float, cancel: Event | None) -> list[dict]:
        observed: list[dict] = []
        seen_tokens: set[str] = set()
        while True:
            if cancel is not None and cancel.is_set():
                raise AdapterError("native tracking observation cancelled")  # noqa: EM101
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise AdapterError("native tracking observation timed out")  # noqa: EM101
            page = self._native_post(path, body, timeout=min(HTTP_TIMEOUT, remaining))
            if cancel is not None and cancel.is_set():
                raise AdapterError("native tracking observation cancelled")  # noqa: EM101
            if time.monotonic() >= deadline:
                raise AdapterError("native tracking observation timed out")  # noqa: EM101
            observed.extend(page.get(key, []))
            token = page.get("next_page_token")
            if not token:
                return observed
            if token in seen_tokens:
                raise AdapterError("native tracking search returned a repeated page token")  # noqa: EM101
            seen_tokens.add(token)
            body = {**body, "page_token": token}

    def native_experiments(self, *, cancel: Event | None = None, timeout: float = HTTP_TIMEOUT) -> list[dict]:
        """Complete tracking observation, bounded across all search pages."""
        return self._tracking_pages(
            "/api/2.0/mlflow/experiments/search", "experiments",
            {"max_results": 100, "view_type": "ALL"}, time.monotonic() + timeout, cancel,
        )

    def native_runs(self, *, cancel: Event | None = None, timeout: float = HTTP_TIMEOUT) -> list[dict]:
        """All runs in all experiments, including deleted tracking entities."""
        deadline = time.monotonic() + timeout
        runs: list[dict] = []
        experiments = self.native_experiments(cancel=cancel, timeout=timeout)
        for experiment in experiments:
            runs.extend(self._tracking_pages(
                "/api/2.0/mlflow/runs/search", "runs",
                {"experiment_ids": [experiment["experiment_id"]], "max_results": 100, "run_view_type": "ALL"},
                deadline, cancel,
            ))
        return runs

    # -- owned cleanup --

    def record_owned_model(self, name: str) -> None:
        """Record intent before creation; names are never inferred from searches."""
        if getattr(self, "_external", False):
            raise AdapterError("external native verifier is read-only")  # noqa: EM101
        self.owned_model_names.add(name)

    def delete_test_owned_native_models(self) -> None:
        """Delete only native models the tests created during this session."""
        for name in sorted(self.owned_model_names):
            try:
                self.native_delete_model(name)
            except requests.exceptions.HTTPError as err:
                # Ownership is registered before create, which may fail or be pending.
                if err.response is None or err.response.status_code != 404:
                    raise
            self.owned_model_names.remove(name)

    def reset_test_state(self, *, recreate_native: bool = False) -> None:
        """Isolated state for one test: empty compatibility database, no test-owned native models."""
        native: NativeRegistryProcess | None = getattr(self, "native", None)
        if (
            native is None
            or getattr(self, "_resources", None) is None
            or self.registry_uri != native.url
        ):
            raise AdapterError("reset requires an owned native server")  # noqa: EM101
        self.proxy.disarm()
        # Stop the adapter first: it holds pooled connections, and postgres
        # refuses to drop a database that is still in use.
        self.adapter.kill()
        if recreate_native:
            # Reused acceptance tests choose fixed names without ownership records.
            # Their reset gets a new database only after stopping our native process.
            native.stop()
            self.native = NativeRegistryProcess.start(
                self._native_binary, self._native_workdir / uuid.uuid4().hex
            )
            self._native_resources.callback(self.native.stop)
            self.registry_uri = self.native.url
            self.proxy.upstream = self.registry_uri
            self.owned_model_names.clear()
        else:
            self.delete_test_owned_native_models()
        self.database.reset()
        self.adapter.start()

    def close(self) -> None:
        resources = getattr(self, "_resources", None)
        if resources is not None:
            resources.close()

    # -- misc helpers --

    def __repr__(self) -> str:  # pragma: no cover - diagnostics only
        return f"MLflowStack(engine={self.engine!r}, adapter={self.adapter.url}, native={self.registry_uri})"


def dump_json(value) -> str:  # pragma: no cover - diagnostics only
    return json.dumps(value, default=str)
