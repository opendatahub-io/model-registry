all: install tidy

IMG_REGISTRY ?= quay.io
IMG_VERSION ?= latest
IMG ?= ghcr.io/kubeflow/hub/server
BUILD_IMAGE ?= true # whether to build the MR server image
include odh_rules.mk

.PHONY: install
install:
	../../bin/openapi-generator-cli generate -i ../../api/openapi/model-registry-v1.yaml -g python -o src/ --package-name mr_openapi --additional-properties=library=asyncio,generateSourceCodeOnly=true,useOneOfDiscriminatorLookup=true --template-dir templates/
	mv src/mr_openapi_README.md src/mr_openapi/README.md
	git apply patches/*
	poetry install

.PHONY: clean
clean:
	rm -rf src/mr_openapi

.PHONY: deploy-latest-mr
deploy-latest-mr:
	cd ../../ && \
	$(if $(filter true,$(BUILD_IMAGE)),\
		IMG_VERSION=${IMG_VERSION} IMG=${IMG} make image/build ARGS="--load$(if ${DEV_BUILD}, --target dev-build)" && \
	) \
	LOCAL=1 IMG=$(IMG):$(IMG_VERSION) ./scripts/deploy_on_kind.sh
# TODO RHOAIENG-30453 align consistency ./scripts/deploy_on_kind.sh uses IMG with :tag, Vs, Makefile(s) and ci/GHA we use IMG without trailing :tag
	kubectl port-forward -n kubeflow services/model-registry-service 8080:8080 > /dev/null 2>&1 & echo $$! >> .port-forwards.pid
	@echo "Waiting for model-registry port-forward (8080)..."
	@for i in $$(seq 1 30); do curl -sf http://localhost:8080/api/model_registry/v1/registered_models > /dev/null 2>&1 && break || sleep 1; done
	@curl -sf http://localhost:8080/api/model_registry/v1/registered_models > /dev/null 2>&1 || \
		(echo "ERROR: model-registry not reachable on localhost:8080 after 30s"; exit 1)

.PHONY: deploy-test-seaweedfs
deploy-test-seaweedfs:
	cd ../../ && ./scripts/deploy_seaweedfs_on_kind.sh
	kubectl port-forward -n seaweedfs svc/seaweedfs 8333:8333 > /dev/null 2>&1 & echo $$! >> .port-forwards.pid
	@echo "Waiting for SeaweedFS S3 port-forward (8333)..."
	@for i in $$(seq 1 30); do curl -sf http://localhost:8333/healthz > /dev/null 2>&1 && break || sleep 1; done
	@curl -sf http://localhost:8333/healthz > /dev/null 2>&1 || \
		(echo "ERROR: SeaweedFS S3 not reachable on localhost:8333 after 30s"; exit 1)

.PHONY: deploy-local-registry
deploy-local-registry:
	cd ../../ && ./scripts/deploy_local_kind_registry.sh
	kubectl port-forward service/distribution-registry-test-service -n local-oci-registry-ns 5001:5001 > /dev/null 2>&1 & echo $$! >> .port-forwards.pid
	@echo "Waiting for local-registry port-forward (5001)..."
	@for i in $$(seq 1 30); do curl -sf http://localhost:5001/v2/ > /dev/null 2>&1 && break || sleep 1; done
	@curl -sf http://localhost:5001/v2/ > /dev/null 2>&1 || \
		(echo "ERROR: local registry not reachable on localhost:5001 after 30s"; exit 1)

.PHONY: test-e2e
test-e2e: deploy-latest-mr deploy-local-registry deploy-test-seaweedfs
	@echo "Starting test-e2e"
	-$(MAKE) test-e2e-run; STATUS=$$?
	$(MAKE) test-e2e-cleanup
	@exit $$STATUS

# Run the MLflow backend acceptance tests (tests/mlflow). The tests own their
# native MLflow server, compatibility database container (podman or docker), and
# adapter process, so no cluster is needed.
#   MLFLOW_TEST_BINARY     path to an MLflow 3.18.0 executable (default: `mlflow` on PATH)
#   MR_BINARY              model-registry server binary (default: built from the repo root)
#   MLFLOW_COMPAT_DATABASE mysql (default) or postgres
MLFLOW_TEST_BINARY ?= $(shell command -v mlflow 2>/dev/null)
MR_BINARY ?= $(abspath ../../model-registry)
MLFLOW_COMPAT_DATABASE ?= mysql

.PHONY: test-mlflow
test-mlflow:
	@if [ -z "$(MLFLOW_TEST_BINARY)" ]; then \
		echo "ERROR: set MLFLOW_TEST_BINARY to an MLflow 3.18.0 executable (e.g. pipx install mlflow==3.18.0)"; \
		exit 1; \
	fi
	$(MAKE) -C ../.. build/compile
	MLFLOW_TEST_BINARY="$(MLFLOW_TEST_BINARY)" MR_BINARY="$(MR_BINARY)" \
		poetry run pytest tests/mlflow --e2e --mlflow-backend --mlflow-compat-database $(MLFLOW_COMPAT_DATABASE) -s -rA

# Run the full e2e suite (--e2e) against the MLflow backend. Like test-mlflow,
# the fixtures own the native MLflow server, compatibility database container,
# and adapter process — that is the "deploy an mlflow instance and configure
# the backend" step — so the model-registry server is never deployed to the
# cluster. SeaweedFS and the local OCI registry are deployed to a dedicated
# mr-e2e-mlflow Kind cluster (override with CLUSTER_NAME). MR_URL pins the adapter to
# the port the raw-REST tests expect. Tests the MLflow backend cannot support
# are deselected:
#   - non-model artifact unions (doc-artifact etc.) return 501 by design
#   - registered-model names containing "/" (hf_import, the default-OCI test)
#     are rejected by native MLflow
#   - test_register_version_long_name expects the EmbedMD name-length cap
MLFLOW_E2E_DESELECTS = \
	--deselect tests/REST_bindings_test.py::test_model_artifact \
	--deselect tests/test_core.py::test_insert_model_version_artifact \
	--deselect tests/test_core.py::test_update_model_version_artifact \
	--deselect tests/test_core.py::test_page_through_model_version_artifacts \
	--deselect tests/test_core.py::test_get_model_artifact_by_id \
	--deselect tests/test_core.py::test_get_model_artifact_by_name \
	--deselect tests/test_core.py::test_get_model_artifact_by_external_id \
	--deselect tests/test_core.py::test_get_all_model_artifacts \
	--deselect tests/test_core.py::test_get_model_version_artifacts_by_mv_id \
	--deselect tests/test_client.py::test_hf_import \
	--deselect tests/test_client.py::test_hf_import_default_env \
	--deselect tests/test_client.py::test_register_version_long_name \
	--deselect tests/test_client.py::test_upload_artifact_and_register_model_with_default_oci \
	--deselect tests/test_client.py::test_as_mlops_engineer_i_would_like_to_store_a_longer_documentation_for_the_model

.PHONY: deploy-e2e-mlflow-infra
deploy-e2e-mlflow-infra: export CLUSTER_NAME ?= mr-e2e-mlflow
deploy-e2e-mlflow-infra:
	$(MAKE) deploy-test-seaweedfs LOCAL=1
	$(MAKE) deploy-local-registry

.PHONY: test-e2e-mlflow
test-e2e-mlflow:
	@if [ -z "$(MLFLOW_TEST_BINARY)" ]; then \
		echo "ERROR: set MLFLOW_TEST_BINARY to an MLflow 3.18.0 executable (e.g. pipx install mlflow==3.18.0)"; \
		exit 1; \
	fi
	$(MAKE) -C ../.. build/compile
	$(MAKE) deploy-e2e-mlflow-infra
	@echo "Starting test-e2e-mlflow"
	@TEST_STATUS=0; CLEANUP_STATUS=0; \
	$(MAKE) test-e2e-mlflow-run || TEST_STATUS=$$?; \
	$(MAKE) test-e2e-mlflow-cleanup || CLEANUP_STATUS=$$?; \
	if [ "$$TEST_STATUS" -ne 0 ]; then exit "$$TEST_STATUS"; fi; \
	exit "$$CLEANUP_STATUS"

.PHONY: test-e2e-mlflow-run
test-e2e-mlflow-run:
	@echo "Ensuring all extras are installed..."
	poetry install --all-extras
	@echo "Running tests..."
	@set -a; . ../../scripts/manifests/seaweedfs/.env; set +a; \
	MR_URL=http://localhost:8080 \
	MLFLOW_TEST_BINARY="$(MLFLOW_TEST_BINARY)" MR_BINARY="$(MR_BINARY)" \
	poetry run pytest --e2e --mlflow-backend --mlflow-compat-database $(MLFLOW_COMPAT_DATABASE) \
		$(MLFLOW_E2E_DESELECTS) -s -rA
	@rm -f ../../scripts/manifests/seaweedfs/.env

.PHONY: test-e2e-mlflow-cleanup
test-e2e-mlflow-cleanup:
	@echo "Cleaning up port-forward processes..."
	@if [ -f .port-forwards.pid ]; then \
		kill $$(cat .port-forwards.pid) || true; \
		rm -f .port-forwards.pid; \
	fi

.PHONY: test-fuzz
test-fuzz: deploy-latest-mr deploy-local-registry deploy-test-seaweedfs
	@echo "Cleaning database before fuzz tests..."
	cd ../../ && ./scripts/cleanup.sh
	poetry install --all-extras
	@echo "Starting stateless fuzz tests with parallel execution"; \
	set -a; . ../../scripts/manifests/seaweedfs/.env; set +a; \
	poetry run pytest tests/fuzz_api/model_registry/test_mr_stateless.py tests/fuzz_api/model_catalog/test_catalog_stateless.py --fuzz -n auto -v --hypothesis-show-statistics; \
	STATELESS=$$?; \
	echo "Cleaning database before stateful fuzz tests..."; \
	cd ../../ && ./scripts/cleanup.sh && cd clients/python; \
	echo "Running stateful fuzz tests sequentially"; \
	set -a; . ../../scripts/manifests/seaweedfs/.env; set +a; \
	poetry run pytest tests/fuzz_api/model_registry/test_mr_stateful.py tests/fuzz_api/model_catalog/test_catalog_stateful.py --fuzz -v --hypothesis-show-statistics; \
	STATEFUL=$$?; \
	rm -f ../../scripts/manifests/seaweedfs/.env; \
	$(MAKE) test-e2e-cleanup; \
	if [ $$STATELESS -ne 0 ] || [ $$STATEFUL -ne 0 ]; then \
		echo "Fuzz tests failed (stateless=$$STATELESS, stateful=$$STATEFUL)"; exit 1; \
	fi

.PHONY: test-e2e-run
test-e2e-run:
	@echo "Ensuring all extras are installed..."
	poetry install --all-extras
	@echo "Running tests..."
	@set -a; . ../../scripts/manifests/seaweedfs/.env; set +a; \
	poetry run pytest --e2e -s -rA --html=report.html --self-contained-html
	@rm -f ../../scripts/manifests/seaweedfs/.env

.PHONY: test-e2e-cleanup
test-e2e-cleanup:
	@echo "Cleaning up database..."
	cd ../../ && ./scripts/cleanup.sh
	@echo "Cleaning up port-forward processes..."
	@if [ -f .port-forwards.pid ]; then \
		kill $$(cat .port-forwards.pid) || true; \
		rm -f .port-forwards.pid; \
	fi

.PHONY: undeploy-seaweedfs
undeploy-seaweedfs:
	@echo "Undeploy SeaweedFS..."
	cd ../../ && ./scripts/undeploy_seaweedfs.sh

.PHONY: undeploy-local-kind-registry
undeploy-local-kind-registry:
	@echo "Undeploy Local Kind Registry..."
	cd ../../ && ./scripts/undeploy_local_registry.sh

.PHONY: test
test:
	poetry run pytest -s -rA

.PHONY: lint
lint:
	poetry run ruff check
	poetry run mypy .

.PHONY: tidy
tidy:
	# Fix code issues everywhere, including src/mr_openapi/ (excluded by default in pyproject.toml)
	poetry run ruff check --fix-only --unsafe-fixes . src/mr_openapi
	poetry run ruff format src src/mr_openapi

.PHONY: build
build: install tidy
	poetry build

.PHONY: publish
publish: build
	poetry publish --build -u ${PYPI_USERNAME} -p ${PYPI_PASSWORD}

.PHONY: update
update:
	poetry lock
