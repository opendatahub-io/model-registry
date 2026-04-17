#!/usr/bin/env bash
# Standalone script to run async-upload e2e tests against an ODH cluster.
# Usage: scripts/odh_env.sh
#
# Expects to be run from the jobs/async-upload/ directory, or via:
#   make -C jobs/async-upload test-e2e-odh

set -euo pipefail

SCRIPT_DIR="$(dirname "$(realpath "${BASH_SOURCE[0]}")")"
REPO_ROOT="$(realpath "${SCRIPT_DIR}/..")"

trap 'rm -f "${REPO_ROOT}/scripts/manifests/minio/.env"' EXIT

set -a; . "${REPO_ROOT}/scripts/manifests/minio/.env"; set +a
mkdir -p "${REPO_ROOT}/results"

AUTH_TOKEN=$(kubectl config view --raw -o jsonpath="{.users[?(@.name==\"$(kubectl config view -o jsonpath="{.contexts[?(@.name==\"$(kubectl config current-context)\")].context.user}")\")].user.token}")
export AUTH_TOKEN

export VERIFY_SSL=False

MR_NAMESPACE=$(kubectl get datasciencecluster default-dsc -o jsonpath='{.spec.components.modelregistry.registriesNamespace}')
export MR_NAMESPACE

MR_ENDPOINT=$(kubectl get service -n "${MR_NAMESPACE}" model-registry -o jsonpath='{.metadata.annotations.routing\.opendatahub\.io\/external-address-rest}')
export MR_HOST_URL="https://${MR_ENDPOINT}"
MR_ENDPOINT="${MR_ENDPOINT%%:*}"
export MR_ENDPOINT

export MODEL_SYNC_REGISTRY_SERVER_ADDRESS="https://${MR_ENDPOINT}"
export MODEL_SYNC_REGISTRY_PORT="443"
export MODEL_SYNC_REGISTRY_IS_SECURE="false"
export MODEL_SYNC_REGISTRY_USER_TOKEN="${AUTH_TOKEN}"
CONTAINER_IMAGE_URI=$("${SCRIPT_DIR}/get_async_upload_image.sh")
export CONTAINER_IMAGE_URI

poetry install --all-extras --with integration
poetry run pytest --e2e tests/integration/ -svvv -rA \
  --html="${REPO_ROOT}/results/report.html" \
  --junit-xml="${REPO_ROOT}/results/xunit_report.xml" \
  --self-contained-html \
  -o junit_suite_name=odh-async-upload
