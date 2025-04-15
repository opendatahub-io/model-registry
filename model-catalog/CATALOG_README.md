# Model Catalog stub implementation resources

This directory contains metadata for the initial stub iteration of the model catalog in the RHOAI dashboard. This stub implementation allows us to develop and demonstrate model catalog UI functionality without a real backend service. The model source files and scripts here will become obsolete once the upcoming model catalog backend service is implemented.

For the initial stub implementation, each of the YAML files in `./models` represents a model catalog source object. To include these models in the model catalog UI, this data is stored in a ConfigMap on a cluster. The YAML for each source is converted to JSON and inserted as an element in the `sources` array within the JSON blob `data.modelCatalogSources` in the ConfigMap. Multiple sources will appear in the UI under section headers.

There are two such ConfigMaps automatically created by the [manifests in the odh-dashboard repository](https://github.com/opendatahub-io/odh-dashboard/blob/main/manifests/rhoai/shared/apps/model-catalog), both in the application namespace (`opendatahub` for ODH):

- `model-catalog-sources`

  This is a managed resource containing the model sources shipped in product releases. [The manifests](https://github.com/opendatahub-io/odh-dashboard/blob/main/manifests/rhoai/shared/apps/model-catalog/model-catalog-configmap.yaml) contain the metadata content for these sources and their models. Edits to this ConfigMap on a cluster will not persist, and upgrading the platform will replace its contents if they have changed.

- `model-catalog-unmanaged-sources`

  This is an unmanaged resource which by default contains an empty `sources` array in its JSON. Sources can be added to the model catalog on a cluster by editing this ConfigMap. Its contents will be combined with the managed ConfigMap (the lists of sources are appended together).

## Updating unmanaged sources in a cluster

The `./scripts/update-unmanaged-sources-configmap.sh` script here will take one of these `models/*.yaml` files, convert it to JSON and replace the contents of the `model-catalog-unmanaged-sources` ConfigMap with its source. This can be used for demonstration purposes.

Usage:

```sh
cd model-registry/model-catalog
scripts/update-unmanaged-sources-configmap.sh models/neural-magic-models.yaml
```
