#!/usr/bin/env bash

set -e

DSC_NAME="default-dsc"

echo "Check if DataScienceCluster exists"
if kubectl get datasciencecluster "$DSC_NAME" &> /dev/null; then
  echo "DataScienceCluster '$DSC_NAME' exists."
else
  echo "DataScienceCluster '$DSC_NAME' does NOT exist."
  exit 1
fi

echo "Check if Model Registry is enabled in DSC"
MR_STATE=$(kubectl get datasciencecluster "$DSC_NAME" -o jsonpath='{.spec.components.modelregistry.managementState}' 2>/dev/null)
if [ "$MR_STATE" != "Managed" ]; then
  echo "Model Registry is not enabled (managementState='$MR_STATE'). Expected 'Managed'."
  exit 1
fi
echo "Model Registry is enabled (managementState='Managed')."

MR_NAMESPACE=$(kubectl get datasciencecluster "$DSC_NAME" -o jsonpath='{.spec.components.modelregistry.registriesNamespace}' 2>/dev/null)
if [ -z "$MR_NAMESPACE" ]; then
  echo "Could not determine registriesNamespace from DSC."
  exit 1
fi
echo "Model Registry namespace: '$MR_NAMESPACE'"

echo "Looking for catalog sources ConfigMap in namespace '$MR_NAMESPACE'"
if kubectl get configmap mcp-catalog-sources -n "$MR_NAMESPACE" &> /dev/null; then
  CATALOG_CONFIGMAP="mcp-catalog-sources"
  echo "ConfigMap 'mcp-catalog-sources' found."
elif kubectl get configmap model-catalog-sources -n "$MR_NAMESPACE" &> /dev/null; then
  CATALOG_CONFIGMAP="model-catalog-sources"
  echo "ConfigMap 'model-catalog-sources' found (fallback)."
else
  echo "Neither 'mcp-catalog-sources' nor 'model-catalog-sources' ConfigMap found in namespace '$MR_NAMESPACE'."
  exit 1
fi

SCRIPT_DIR="$(dirname "$(realpath "$BASH_SOURCE")")"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
TEST_CATALOG_FILE="${REPO_ROOT}/manifests/kustomize/options/catalog/overlays/e2e/test-catalog.yaml"

if [ ! -f "$TEST_CATALOG_FILE" ]; then
  echo "Required file not found: $TEST_CATALOG_FILE"
  exit 1
fi

echo "Fetching current sources.yaml from ConfigMap"
CURRENT_SOURCES=$(kubectl get configmap "$CATALOG_CONFIGMAP" -n "$MR_NAMESPACE" -o jsonpath='{.data.sources\.yaml}')

if echo "$CURRENT_SOURCES" | grep -q "test_catalog"; then
  echo "test_catalog already present in sources.yaml, skipping patch."
else
  echo "Patching ConfigMap to add test catalog source"

  TEST_CATALOG_CONTENT=$(cat "$TEST_CATALOG_FILE")

  TEST_CATALOG_ENTRY="
catalogs:
  - name: Test Catalog
    id: test_catalog
    type: yaml
    enabled: true
    properties:
      yamlCatalogPath: test-catalog.yaml
    labels:
      - Test Catalog
"

  if echo "$CURRENT_SOURCES" | grep -q "catalogs: \[\]"; then
    # Replace empty catalogs: [] with test catalog entry
    UPDATED_SOURCES=$(echo "$CURRENT_SOURCES" | sed 's/catalogs: \[\]//')
    UPDATED_SOURCES="${UPDATED_SOURCES}${TEST_CATALOG_ENTRY}"
  elif echo "$CURRENT_SOURCES" | grep -q "^catalogs:"; then
    # Append to existing catalogs section
    UPDATED_SOURCES=$(echo "$CURRENT_SOURCES" | sed '/^catalogs:/a\
  - name: Test Catalog\
    id: test_catalog\
    type: yaml\
    enabled: true\
    properties:\
      yamlCatalogPath: test-catalog.yaml\
    labels:\
      - Test Catalog')
  else
    # No catalogs section exists, append one
    UPDATED_SOURCES="${CURRENT_SOURCES}${TEST_CATALOG_ENTRY}"
  fi

  # Patch the existing ConfigMap: update sources.yaml and add the test catalog YAML as a data key
  kubectl patch configmap "$CATALOG_CONFIGMAP" -n "$MR_NAMESPACE" --type merge -p "$(cat <<EOF
{"data": {"sources.yaml": $(echo "$UPDATED_SOURCES" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))'), "test-catalog.yaml": $(echo "$TEST_CATALOG_CONTENT" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')}}
EOF
)"

  echo "ConfigMap '$CATALOG_CONFIGMAP' patched successfully."

  echo "Restarting model-catalog pod to pick up config changes"
  CATALOG_DEPLOYMENT=$(kubectl get deployment -n "$MR_NAMESPACE" -l app.kubernetes.io/name=model-catalog -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  if [ -z "$CATALOG_DEPLOYMENT" ]; then
    echo "Could not find model-catalog deployment in namespace '$MR_NAMESPACE'."
    exit 1
  fi
  kubectl delete pod -l app.kubernetes.io/name=model-catalog -n "$MR_NAMESPACE" --wait=true
  kubectl wait --for=condition=Available deployment/"$CATALOG_DEPLOYMENT" -n "$MR_NAMESPACE" --timeout=5m
  echo "model-catalog pod is ready."
fi
