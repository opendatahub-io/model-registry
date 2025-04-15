#!/bin/bash

# Check if required tools are installed
for cmd in yq jq; do
    if ! command -v $cmd >/dev/null 2>&1; then
        echo "Error: $cmd is not installed. Please install it first."
        exit 1
    fi
done

# Check if logged into a cluster
if ! oc whoami >/dev/null 2>&1; then
    echo "Error: Not logged into a cluster. Please run 'oc login' first."
    exit 1
fi

# Check if input file is provided
if [ $# -ne 1 ]; then
    echo "Usage: $0 <input-yaml-file>"
    exit 1
fi

INPUT_YAML=$1

# Check if input file exists
if [ ! -f "$INPUT_YAML" ]; then
    echo "Error: Input file $INPUT_YAML does not exist"
    exit 1
fi

# Convert input YAML to JSON and wrap it in a sources array
export WRAPPED_JSON=$(yq -o=json "$INPUT_YAML" | jq -c '{sources: [.]}')

# Grab the existing configmap and update the modelCatalogSources field with the new content
mkdir tmp
oc get configmap model-catalog-unmanaged-sources -n opendatahub -o yaml > tmp/model-catalog-unmanaged-sources.yaml
yq -i '.data.modelCatalogSources = strenv(WRAPPED_JSON)' tmp/model-catalog-unmanaged-sources.yaml

# Update the configmap with the new content
oc apply -f tmp/model-catalog-unmanaged-sources.yaml -n opendatahub

# Clean up
rm tmp/model-catalog-unmanaged-sources.yaml
rmdir tmp

echo "Success"