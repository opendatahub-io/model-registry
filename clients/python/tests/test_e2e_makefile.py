import os
import shlex
import shutil
import subprocess
from pathlib import Path

import pytest


@pytest.mark.parametrize("database", ["mysql", "postgres"])
def test_mlflow_acceptance_target_selects_acceptance_tests(tmp_path, database):
    """The test-mlflow recipe must run the MLflow acceptance tests.

    tests/mlflow carries both scaffolding unit tests and the e2e-marked
    compatibility/recovery acceptance tests. Without --e2e the collection
    hook deselects the acceptance tests, so the target would silently run
    only the unit scaffolding.
    """
    package_dir = Path(__file__).resolve().parents[1]
    workdir = tmp_path / "clients" / "python"
    workdir.mkdir(parents=True)
    for name in ("Makefile", "odh_rules.mk"):
        shutil.copyfile(package_dir / name, workdir / name)

    # Stub the repository root so `$(MAKE) -C ../.. build/compile` is cheap.
    root_makefile = tmp_path / "Makefile"
    root_makefile.write_text("build/compile:\n\t@echo stub-build\n")

    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    poetry_args = tmp_path / "poetry-args"
    for name, body in (
        (
            "poetry",
            f'#!/bin/sh\nprintf "%s\\n" "$*" > {shlex.quote(str(poetry_args))}\nexit 0\n',
        ),
        ("mlflow", "#!/bin/sh\nexit 0\n"),
    ):
        script = bin_dir / name
        script.write_text(body)
        script.chmod(0o755)

    env = {
        key: value
        for key, value in os.environ.items()
        if key not in ("LOCAL", "CLUSTER_NAME", "MAKEFLAGS", "MAKELEVEL", "MFLAGS")
    }
    env["PATH"] = f"{bin_dir}{os.pathsep}{env['PATH']}"
    make = shutil.which("make")
    assert make is not None
    result = subprocess.run(  # noqa: S603
        [
            make,
            "test-mlflow",
            f"MLFLOW_TEST_BINARY={bin_dir / 'mlflow'}",
            f"MLFLOW_COMPAT_DATABASE={database}",
        ],
        cwd=workdir,
        env=env,
        capture_output=True,
        text=True,
        timeout=60,
        check=False,
    )

    assert result.returncode == 0, result.stdout + result.stderr
    args = poetry_args.read_text().split()
    assert "run" in args
    assert "pytest" in args
    assert "tests/mlflow" in args, "the target must select the MLflow acceptance suite"
    assert "--e2e" in args, "the target must not deselect the e2e-marked acceptance tests"
    assert "--mlflow-backend" in args
    assert "--mlflow-compat-database" in args
    assert database in args


@pytest.mark.parametrize(
    ("make_args", "expected_context"),
    [([], "kind-mr-e2e-mlflow"), (["CLUSTER_NAME=custom-mlflow"], "kind-custom-mlflow")],
)
def test_mlflow_infra_uses_dedicated_kind_cluster(tmp_path, make_args, expected_context):
    package_dir = Path(__file__).resolve().parents[1]
    workdir = tmp_path / "clients" / "python"
    workdir.mkdir(parents=True)
    for name in ("Makefile", "odh_rules.mk"):
        shutil.copyfile(package_dir / name, workdir / name)

    scripts = tmp_path / "scripts"
    scripts.mkdir()
    context = tmp_path / "context"
    context.write_text("kind-kind")
    deployments = tmp_path / "deployments"
    # Simulate the shared scripts' context switch when LOCAL is enabled.
    for name, service in (
        ("deploy_seaweedfs_on_kind.sh", "seaweedfs"),
        ("deploy_local_kind_registry.sh", "registry"),
    ):
        script = scripts / name
        script.write_text(
            '#!/bin/sh\n'
            'if [ -n "$LOCAL" ]; then echo "kind-${CLUSTER_NAME:-kind}" > "$TEST_CONTEXT"; fi\n'
            f'printf "{service} %s\\n" "$(cat "$TEST_CONTEXT")" >> "$TEST_DEPLOYMENTS"\n'
        )
        script.chmod(0o755)

    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    for name in ("kubectl", "curl"):
        script = bin_dir / name
        script.write_text("#!/bin/sh\nexit 0\n")
        script.chmod(0o755)

    env = {
        key: value
        for key, value in os.environ.items()
        if key not in ("LOCAL", "CLUSTER_NAME", "MAKEFLAGS", "MAKELEVEL", "MFLAGS")
    }
    env.update(
        PATH=f"{bin_dir}{os.pathsep}{env['PATH']}",
        TEST_CONTEXT=str(context),
        TEST_DEPLOYMENTS=str(deployments),
    )
    make = shutil.which("make")
    assert make is not None
    result = subprocess.run(  # noqa: S603
        [make, "deploy-e2e-mlflow-infra", *make_args],
        cwd=workdir,
        env=env,
        capture_output=True,
        text=True,
        timeout=15,
        check=False,
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert context.read_text().strip() == expected_context
    assert deployments.read_text().splitlines() == [f"seaweedfs {expected_context}", f"registry {expected_context}"]


@pytest.mark.parametrize(
    ("run_status", "cleanup_status"),
    [(0, 0), (7, 0), (0, 9), (7, 9)],
)
def test_e2e_mlflow_preserves_test_and_cleanup_status(tmp_path, run_status, cleanup_status):
    """A failed test run must fail the target even when cleanup succeeds,
    and a failed cleanup must fail an otherwise successful run.

    The wrapper recipe runs both recursive targets in one shell, so the
    test status survives the cleanup invocation. Cleanup runs exactly
    once, after the test, in every case.
    """
    package_dir = Path(__file__).resolve().parents[1]
    workdir = tmp_path / "clients" / "python"
    workdir.mkdir(parents=True)
    for name in ("Makefile", "odh_rules.mk"):
        shutil.copyfile(package_dir / name, workdir / name)

    # Stub the repository root so `$(MAKE) -C ../.. build/compile` is cheap.
    root_makefile = tmp_path / "Makefile"
    root_makefile.write_text("build/compile:\n\t@echo stub-build\n")

    # Override the recursive targets with recording stubs so the real
    # wrapper recipe is exercised without a cluster or native services.
    # The overrides are appended to the copied Makefile because -f flags
    # do not propagate to sub-makes: every recursive $(MAKE) must see them.
    record = tmp_path / "invocations"
    with (workdir / "Makefile").open("a") as makefile:
        makefile.write(
            "\n"
            "deploy-e2e-mlflow-infra:\n"
            f"\t@echo infra >> {record}\n"
            "test-e2e-mlflow-run:\n"
            f"\t@echo run >> {record}\n"
            f"\t@exit {run_status}\n"
            "test-e2e-mlflow-cleanup:\n"
            f"\t@echo cleanup >> {record}\n"
            f"\t@exit {cleanup_status}\n"
        )

    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    mlflow_stub = bin_dir / "mlflow"
    mlflow_stub.write_text("#!/bin/sh\nexit 0\n")
    mlflow_stub.chmod(0o755)

    env = {
        key: value
        for key, value in os.environ.items()
        if key not in ("LOCAL", "CLUSTER_NAME", "MAKEFLAGS", "MAKELEVEL", "MFLAGS")
    }
    env["PATH"] = f"{bin_dir}{os.pathsep}{env['PATH']}"
    make = shutil.which("make")
    assert make is not None
    result = subprocess.run(  # noqa: S603
        [
            make,
            "test-e2e-mlflow",
            f"MLFLOW_TEST_BINARY={mlflow_stub}",
        ],
        cwd=workdir,
        env=env,
        capture_output=True,
        text=True,
        timeout=60,
        check=False,
    )

    invocations = record.read_text().splitlines()
    assert invocations == ["infra", "run", "cleanup"], (
        "cleanup must run exactly once, after the test, in every case"
    )
    # GNU Make converts a recipe failure into its own exit code 2 at each
    # recursive boundary, so assert nonzero rather than the original status.
    if run_status == 0 and cleanup_status == 0:
        assert result.returncode == 0, result.stdout + result.stderr
    else:
        assert result.returncode != 0, (
            "a failed test or cleanup must fail the top-level target"
        )
