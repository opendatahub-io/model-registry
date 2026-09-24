#!/usr/bin/env bash

set -e

SEAWEEDFS_NAMESPACE="seaweedfs"
unset KF_MR_TEST_ACCESS_KEY_ID KF_MR_TEST_SECRET_ACCESS_KEY KF_MR_TEST_S3_ENDPOINT KF_MR_TEST_BUCKET_NAME

echo 'Delete SeaweedFS namespace if exists'
if kubectl get namespace "$SEAWEEDFS_NAMESPACE" >/dev/null 2>&1; then
    kubectl delete namespace "$SEAWEEDFS_NAMESPACE" --ignore-not-found --wait=False
    echo "Waiting for namespace $SEAWEEDFS_NAMESPACE to be deleted..."
    kubectl wait --for=delete namespace/"$SEAWEEDFS_NAMESPACE" --timeout=300s
else
    echo "Namespace $SEAWEEDFS_NAMESPACE does not exist; nothing to delete."
fi
