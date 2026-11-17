# PR Build Validation

This repository runs a Konflux build simulator on pull requests. The simulator mirrors the two container builds that Konflux runs from `.tekton/` and validates the dependency and manifest inputs that most often fail in Konflux or downstream hermetic builds.

The workflow is `.github/workflows/pr-build-validation.yml`; the reusable implementation is `scripts/validate-build.sh`.

## Konflux build targets

| Image | PipelineRun | Build context | Dockerfile |
|-------|-------------|---------------|------------|
| `quay.io/opendatahub/model-registry` | `.tekton/odh-model-registry-pull-request.yaml` | `.` | `Dockerfile` |
| `quay.io/opendatahub/model-registry-job-async-upload` | `.tekton/odh-model-registry-job-async-upload-pull-request.yaml` | `jobs/async-upload` | `Dockerfile` |

The controller, CSI, testops, and UI images are out of scope here: Konflux does not build them in these PipelineRuns, and their existing GitHub workflows cover them.

## What the workflow validates

### Hermetic Build Preflight

- Populates an isolated Go module cache with network access, then compiles with `GOPROXY=off` so a module missing from the prefetched set fails immediately instead of in Konflux.
- Downloads the async-upload Python distributions into a local wheelhouse, then resolves `requirements.txt` with `pip --no-index`, which is the Python equivalent of a Hermeto prefetch.
- Mirrors the root Dockerfile by building with `CGO_ENABLED=0 GOOS=linux` and `-buildvcs=false`.

### Static Validation Checks

- Parses every Konflux pull-request PipelineRun and verifies that its `path-context` and `dockerfile` point at a real file.
- Checks every `go.mod` for a matching `go.sum` when external modules are required, verifies required `go.sum` entries, and checks that local `replace` targets exist and stay inside the repository.
- Runs `go mod download` and `go mod verify` for the root module used by the Konflux server build.
- Checks that `jobs/async-upload/requirements.txt` uses exact `==` pins, has no VCS/local/URL dependencies, and matches `poetry.lock`.
- Checks that the root Dockerfile `COPY` instructions include every local Go `replace` target. This is the Go equivalent of the missing workspace-package `COPY` failure.
- Warns about FIPS release blockers (`CGO_ENABLED=0` and a missing `-tags strictfipsruntime`), floating `:latest` base images, and RPM installs that would need prefetching for a hermetic build. These are warnings, not blockers, because they describe downstream RHOAI release requirements rather than a broken build.

### Build and Validate Server Image

- Builds the root `Dockerfile` for `linux/amd64`.
- Starts PostgreSQL 16 and launches the built image with `proxy --datastore-type=embedmd --embedmd-database-type=postgres`.
- Waits for `/readyz/health` and `/readyz/isDirty` to return HTTP 200.
- Calls `GET /api/model_registry/v1/registered_models` and checks for the expected `items` array.
- Asserts the container runs as UID `65532`, stays running, and logs no `{{ALERT}}` datastore error, panic, or fatal error.

### Build and Validate Async Upload Image

- Builds `jobs/async-upload/Dockerfile` for `linux/amd64`.
- Imports `job`, `job.entrypoint`, `job.healthcheck`, and `model_registry` inside the image.
- Runs `pip check` and the job healthcheck module.
- Verifies `cosign` and `skopeo` are present, the image runs as UID `1000`, and the image config contains a `HEALTHCHECK`.

### Operator Integration (KinD)

- Loads the image built by the server phase into Kind, so the operator reconciles the same bytes that were just tested rather than a registry tag.
- Deploys `opendatahub-io/model-registry-operator` at a pinned commit from `config/default`.
- Pins `RELATED_IMAGE_ODH_MODEL_REGISTRY_IMAGE` to the PR image. This matters: the operator resolves workload images from `RELATED_IMAGE_*`, so patching the Deployment spec alone leaves `:latest` in play.
- Applies `config/samples/mysql` and waits for `modelregistry-sample` to become `Available`.
- Asserts the reconcile actually happened: `modelregistry.opendatahub.io/finalizer` is present, the rendered server Deployment uses the PR image, and no container restarts after the Deployment reports `Available`. A restart during startup is reported with the previous container logs but does not fail the phase, because the server can legitimately start before MySQL accepts connections.
- Dumps operator logs, pod state, the `ModelRegistry` resource, and recent events inside the failing step.

This is the layer container checks cannot reach: CRD installation, RBAC, image wiring, and controller reconcile behaviour.

### Validate Manifests

- Renders every kustomization under `manifests/` with kustomize.
- Runs kubeconform in strict mode against Kubernetes 1.33 schemas.
- Fails on invalid resources and on built-in kinds that were skipped for a missing schema, which catches `Deploymnet`-style typos that `-ignore-missing-schemas` would otherwise hide.
- Reports CRD-backed kinds that are expected to skip (for example `ServiceMonitor`, `ClusterStorageContainer`, Istio resources).

## Running locally

Prerequisites: Docker, Go 1.26+, Python 3, `curl`, `jq`, kustomize, and kubeconform.

```bash
# Fast checks only
./scripts/validate-build.sh static
./scripts/validate-build.sh go-modules
./scripts/validate-build.sh hermetic
./scripts/validate-build.sh manifests

# Container builds and runtime checks
./scripts/validate-build.sh server
./scripts/validate-build.sh async-upload

# Operator integration (needs Docker, kind, and kubectl)
./scripts/validate-build.sh operator

# Everything
./scripts/validate-build.sh all
```

Useful overrides:

```bash
PLATFORM=linux/arm64 ./scripts/validate-build.sh server
SERVER_IMAGE=localhost/model-registry:dev ./scripts/validate-build.sh server
ASYNC_IMAGE=localhost/model-registry-job-async-upload:dev ./scripts/validate-build.sh async-upload
SERVER_PORT=28080 ./scripts/validate-build.sh server
KEEP_KIND=1 ./scripts/validate-build.sh operator
OPERATOR_REF=<sha> OPERATOR_REPO=<git-url> ./scripts/validate-build.sh operator
KUSTOMIZE_BIN=/path/to/kustomize KUBECONFORM_BIN=/path/to/kubeconform \
  ./scripts/validate-build.sh manifests
```

The workflow installs kustomize and kubeconform. Locally, kustomize is required for the `manifests` check; if kubeconform or `jq` is missing, schema validation is skipped with a warning. Kubeconform downloads schemas on first use.

`hermetic` needs only Go and Python; it does not require Docker. It takes a few minutes because it downloads modules and wheels with network access before the offline phase.

## Skipping the workflow

Add `[skip konflux-sim]` to the pull request title or add the `skip-konflux-sim` label. Use this only when the change cannot affect the Konflux images, dependencies, or manifests; documentation-only changes are detected automatically.

The skip condition is evaluated once by the `Check Skip Condition` job; every other job consumes its output, so the title and label rules cannot drift between jobs. Path filtering happens inside that job rather than in the workflow trigger, so the summary check also runs for out-of-scope PRs and required status checks are never left pending.

The workflow re-evaluates the skip decision when a PR is edited or its labels change, so removing the opt-out starts validation again. Metadata-only runs are queued rather than cancelling an in-flight build, because label bots would otherwise abort image validation.

## Troubleshooting

### Go module validation fails

- Run `go mod tidy` in the directory named by the error and commit the resulting `go.mod` and `go.sum`.
- If a local `replace` target is reported as missing, check that the directory exists and is committed.
- If the Dockerfile `COPY` cross-check fails, add the local replace target to the Dockerfile before `go mod download`.

### Python dependency validation fails

- Run `cd jobs/async-upload && make install` to regenerate `requirements.txt` from `poetry.lock`.
- Do not add `git+`, `file:`, URL, or unpinned dependencies to `requirements.txt`; Hermeto cannot prefetch them reliably.

### Hermetic preflight fails

- A `GOPROXY=off` failure means a module is missing from the downloaded set; run `go mod tidy` and check for modules pulled in only by build tags or tools.
- A `--no-index` failure means a distribution is missing from the wheelhouse; check for unpinned or platform-specific dependencies that `pip download` could not resolve.

### Server image runtime fails

- The script prints container logs before exiting. Check the datastore DSN, PostgreSQL readiness, and migration errors.
- A `{{ALERT}}` log line means the server could not connect to the datastore.
- `CGO_ENABLED=0` and the missing `strictfipsruntime` tag are FIPS warnings. They do not fail this simulator, but they can block downstream RHOAI release.

### Kustomize or kubeconform fails

- Reproduce the render with `kustomize build manifests/kustomize/<overlay>`.
- A kubeconform message about a skipped built-in kind usually means an `apiVersion` or `kind` typo.
- `manifests/kustomize/options/catalog/options/istio` is a known-stale option directory upstream: it references `istio-authorization-policy.yaml`, which is not present. The simulator warns and skips it until that file is restored; the deployment docs use `manifests/kustomize/options/istio` instead.

## Making the checks required

Repository administrators can mark these jobs as required status checks in branch protection:

- `Hermetic Build Preflight`
- `Static Validation Checks`
- `Build and Validate Server Image`
- `Build and Validate Async Upload Image`
- `Validate Manifests`
- `Operator Integration (KinD)`
- `Build Validation Summary`

`Build Validation Summary` is the simplest required check: it passes only when every other job succeeds or is skipped.

The workflow is intentionally self-contained and uses only `contents: read` permissions.
