# API contract checks

Pull requests targeting `main` are checked for OpenAPI compatibility by the
shared checks in
[`red-hat-data-services/api-contract-tests`](https://github.com/red-hat-data-services/api-contract-tests).
The checks need no cluster and start in **report mode**: findings are shown on
the pull request but do not fail it. They are pull-request jobs, not promotion
gates.

## What is covered

The contracts are declared in [`.rhoai/contracts.yaml`](../.rhoai/contracts.yaml).
Only the v1 APIs are contracts:

| Contract | Document | Paths compared |
|---|---|---|
| `model-registry-v1` | `api/openapi/model-registry-v1.yaml` | `/api/model_registry/v1/...` |
| `catalog-v1` | `api/openapi/catalog-v1.yaml` | `/api/<name>_catalog/v1/...` (model, MCP, agent, skill and serving runtime catalogs) |

For each contract, the `api-contract` job regenerates the bundled document from
`api/openapi/src` with the repository's own bundling script
(`scripts/merge_openapi.sh` or `scripts/merge_catalog_specs.sh`), and compares
it with the same document at the baseline commit using the pinned `oasdiff`. It
reports breaking changes such as removed operations or response fields, newly
required parameters or request fields, type changes and enum changes. It does
not replace the provider's behavior tests.

The `api-contract-coverage` job fails (once enforced) when an OpenAPI document
is neither a contract, an input of a contract's generator, nor listed under
`spec.exclusions` with a reason, so a new or moved document cannot silently
leave the check. These documents are excluded:

- the alpha APIs, `api/openapi/model-registry.yaml` (v1alpha3) and
  `api/openapi/catalog.yaml` (v1alpha1), with their sources;
- `clients/ui/api/openapi/mod-arch.yaml`, the internal API between the Model
  Registry UI and its BFF;
- `clients/ui/bff/openapi/*`, the BFF health-check Swagger.

Both jobs run on every pull request targeting `main`, with no path filter.

## Baseline

The baseline is the commit named in `.github/workflows/api-contract.yml`
(`baseline-refs`), currently `main` at `8912d58` (October 30, 2026, #1926),
which has no release tag. It replaced the `v0.3.17` release so that the enum
values added to catalog responses since then (`serving_runtimes` in
`assetType`, `boolean` in the filter-option `type`, `evaluation-metrics` in
`metricsType`) are not reported on every pull request. Against `v0.3.17` they
were the only findings. Everything in the v1 APIs at the baseline, including
the serving runtime catalog, is protected from then on.

There should be no breaking API change within a major version, so moving the
baseline forward is an exception that needs reviewer approval. It must be a
full commit SHA; branch names, tags and `HEAD` are rejected. An API whose paths
do not exist at the baseline is reported as an addition and is not yet
protected; it becomes protected once the baseline moves to a commit that
contains it.

## Run it locally

Use the same api-contract-tests ref as the workflow (the `latest` tag), so local
results match CI; fetch the tags again before each run, since `latest` moves to
each new release. The checks need the `yq` and `oasdiff` versions pinned in
`.rhoai/contracts.yaml`.

```sh
git clone git@github.com:red-hat-data-services/api-contract-tests.git ../api-contract-tests
git -C ../api-contract-tests fetch --tags --force
git -C ../api-contract-tests checkout --detach latest
python3 -m pip install PyYAML==6.0.3
GOBIN="$PWD/bin" go install github.com/mikefarah/yq/v4@v4.45.1
GOBIN="$PWD/bin" go install github.com/oasdiff/oasdiff@v1.33.0
export YQ="$PWD/bin/yq" PATH="$PWD/bin:$PATH"

baseline=8912d5886ed1a38cd7039848d235b4d8fb953641   # as in the workflow
for id in model-registry-v1 catalog-v1; do
  python3 ../api-contract-tests/cmd/contract-tests/cli.py check \
    --repo . --contract-set .rhoai/contracts.yaml --contract-id "$id" \
    --baseline-ref "$baseline" --output-dir "/tmp/contract-report-$id"
done

python3 ../api-contract-tests/cmd/contract-tests/cli.py coverage \
  --repo . --contract-set .rhoai/contracts.yaml --report-only
```

For `check`, read `report.json.status` in each output directory, not only the
exit code: report mode exits `0` even when breaking findings are recorded.
`coverage` prints each OpenAPI document's status and ends with
`Result: complete` or a list of gaps. With `--report-only` it exits `0` even
when there are gaps.
