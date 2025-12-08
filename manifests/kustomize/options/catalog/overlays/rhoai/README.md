# RHOAI Dev Overlay

These manifests deploy model catalog with the performance data from RHOAI. These are intended for development.

The manifests expect a pull secret called `registry-redhat-io-credentials` to pull from `registry.redhat.io`. If you're authenticated to that registry with Docker locally, you can create the secret with:

```sh
jq '{"auths": .auths | with_entries(select(.key == "registry.redhat.io"))}' ~/.docker/config.json | kubectl create secret generic registry-redhat-io-credentials --type=kubernetes.io/dockerconfigjson -n kubeflow --from-file=.dockerconfigjson=/dev/stdin
```

## Tilt

To use these manifests with Tilt, follow the [instructions](../odh/README.md) for configuring the ODH overlay, but replace `../odh` with `../rhoai`.
