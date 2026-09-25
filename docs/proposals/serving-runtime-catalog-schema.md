# Proposal: Serving Runtime Catalog Schema

- **Status:** Draft / for discussion
- **Related:** [RHAISTRAT-1988](https://redhat.atlassian.net/browse/RHAISTRAT-1988) — *Model Runtime Catalog for Discovery and Deployment of Serving Runtimes* (RFE [RHAIRFE-2419](https://redhat.atlassian.net/browse/RHAIRFE-2419))
- **Goal:** Define a stable API contract for a serving-runtime catalog so the **dashboard team can build the Runtime Catalog UI in parallel** with backend implementation.

## Summary

This proposes a new **`serving_runtime` catalog plugin** for the federated catalog
service, following the exact conventions already used by the `model`, `mcp`,
`agent`, and `skill` plugins. It lets users discover serving runtimes (vLLM,
OVMS, MLServer for MVP; NVIDIA NIM, HF TGI later), browse their versions with
Supported/Community badges, and retrieve the metadata needed to generate a valid
KServe `ServingRuntime` manifest.

The catalog is **discovery metadata only** — it does not create Kubernetes
resources. It mirrors the MCP plugin's `runtimeMetadata` philosophy: "defaults,
recommendations, and requirements for creating deployments... not actual
deployment configuration."

> Note on the RFE: RHAISTRAT-1988 sketches a single `odh-runtime-catalog`
> ConfigMap read directly by the dashboard BFF. This proposal is the catalog-service
> alternative — it reuses the same federated plugin machinery as the Model Catalog
> (multi-source, filtering, pagination, `/sources` governance, YAML loaders) rather
> than a bespoke ConfigMap reader, giving true parity with the Model Catalog rather
> than a parallel one-off. The two are not mutually exclusive; see
> [Open Questions](#open-questions).

## How it fits the existing framework

Per [`writing-a-catalog-plugin.md`](../writing-a-catalog-plugin.md), a plugin is
defined by a name, a description, and one or more entities mapped to a datastore
kind (`context`, `artifact`, `execution`).

| Input | Value |
|---|---|
| **Name** | `serving_runtime` |
| **Description** | `"Serving runtime catalog"` |
| **Entities** | `CatalogServingRuntime:context`, `CatalogServingRuntimeVersion:artifact` |

- **`CatalogServingRuntime`** (`context`) — the top-level, independently listed
  asset: a runtime *family* (vLLM, OVMS, MLServer). This is the grid card.
- **`CatalogServingRuntimeVersion`** (`artifact`) — a specific, deployable version
  of a runtime: a container image plus the resource recommendations, args, env,
  and template needed to deploy it. This is a child artifact of the runtime,
  mirroring how `CatalogModelArtifact` is a child of `CatalogModel`.

Modeling versions as **artifacts** (rather than an inline array) matches the model
plugin, keeps the list-runtimes response small, gives versions independent
pagination/filtering, and lets version pinning be enforced by filtering artifacts
at the API layer.

New asset-type registration (Step 2 of the plugin guide):

- Add `serving_runtimes` to the `CatalogAssetType` enum in
  `api/openapi/src/catalog.yaml`.
- Add `AssetTypeServingRuntime` in `basecatalog/source_types.go` and to
  `validAssetTypes` in `basecatalog/validation.go`.

## Endpoints

Base path: `/api/serving_runtime_catalog/v1`

| Method & Path | Operation | Purpose |
|---|---|---|
| `GET /serving_runtimes` | `findServingRuntimes` | List/search runtimes across sources (grid browse). |
| `GET /serving_runtimes/filter_options` | `findServingRuntimesFilterOptions` | Fields/values/named queries usable in `filterQuery`. |
| `GET /sources/{source_id}/serving_runtimes/{runtime_name+}` | `getServingRuntime` | Get one runtime (detail view). |
| `GET /sources/{source_id}/serving_runtimes/{runtime_name}/versions` | `getServingRuntimeVersions` | List a runtime's versions (artifacts). |
| `GET /serving_runtimes/{runtime_id}/logo` | `getServingRuntimeLogo` | Serve/redirect the runtime logo (as MCP does). |

Shared query params (`q`, `source`, `sourceLabel`, `filterQuery`, `pageSize`,
`orderBy`, `sortOrder`, `nextPageToken`) and shared responses (`BadRequest`,
`Unauthorized`, `NotFound`, `InternalServerError`, `FilterOptionsResponse`) come
from `catalog.yaml` / `lib/common.yaml` during spec assembly — identical to the
other plugins.

## Proposed OpenAPI schema

This is the plugin source spec that would live at
`api/openapi/src/plugins/serving_runtime-v1.yaml`. Only the `components` block is
shown; the `paths` block follows the model/mcp plugins verbatim in shape.

```yaml
components:
  schemas:
    CatalogServingRuntime:
      description: A serving runtime family in the serving runtime catalog.
      allOf:
        - $ref: "#/components/schemas/BaseResource"
        - type: object
          required:
            - name
          properties:
            name:
              type: string
              description: Runtime identifier. Must be unique within a source.
              example: vllm
            displayName:
              type: string
              description: Human-friendly display label.
              example: vLLM
              maxLength: 255
            sourceId:
              type: string
              description: Catalog source that provides this runtime.
            provider:
              type: string
              description: Organization providing the runtime.
              example: Red Hat
            description:
              type: string
              description: Short description of the runtime.
            readme:
              type: string
              description: Full Markdown documentation for this runtime.
            logo:
              type: string
              description: >-
                Runtime logo. A data URL is recommended; a plain http(s) URL is
                also accepted (served via the /logo endpoint).
            tags:
              type: array
              description: Categorization tags.
              items:
                type: string
              example: ["llm", "generative-ai", "gpu"]
            license:
              type: string
              description: SPDX identifier for the runtime license.
              example: apache-2.0
            licenseLink:
              type: string
              format: uri
              description: URL to the full license text.
            documentationUrl:
              type: string
              format: uri
              description: URL to external documentation.
            repositoryUrl:
              type: string
              format: uri
              description: URL to the source repository.
            supportedModelFormats:
              type: array
              description: >-
                Model formats this runtime can serve, aggregated across its
                versions. Maps to ServingRuntime.spec.supportedModelFormats.
              items:
                $ref: "#/components/schemas/SupportedModelFormat"
            capabilities:
              $ref: "#/components/schemas/ServingRuntimeCapabilities"
            versionCount:
              type: integer
              format: int32
              description: Number of catalog versions available for this runtime.
              readOnly: true
              example: 3
            versions:
              type: array
              description: >-
                Runtime versions. Populated on the single-runtime detail endpoint;
                omitted (or summarized by versionCount) on the list endpoint.
              items:
                $ref: "#/components/schemas/CatalogServingRuntimeVersion"
            publishedDate:
              type: string
              format: date-time
              description: Initial publication timestamp.
            lastUpdated:
              type: string
              format: date-time
              description: Last metadata update timestamp.

    CatalogServingRuntimeVersion:
      description: >-
        A specific, deployable version of a serving runtime. Carries the
        container image and the metadata needed to generate a ServingRuntime
        manifest. Discovery metadata only — not a live deployment.
      allOf:
        - $ref: "#/components/schemas/BaseResource"
        - type: object
          required:
            - artifactType
            - version
            - image
          properties:
            artifactType:
              type: string
              default: serving-runtime-version
            version:
              type: string
              description: Version string of the runtime (typically the image tag).
              example: "0.5.0"
            image:
              type: string
              description: >-
                Fully-qualified container image reference for this version. The
                registry host may be rewritten by the dashboard for disconnected
                environments (registry mirror override).
              example: registry.redhat.io/rhoai/vllm-rhel9:0.5.0
            supportLevel:
              $ref: "#/components/schemas/ServingRuntimeSupportLevel"
            supportedModelFormats:
              type: array
              description: Model formats supported by this specific version.
              items:
                $ref: "#/components/schemas/SupportedModelFormat"
            protocolVersions:
              type: array
              description: Inference protocols supported (maps to ServingRuntime.spec.protocolVersions).
              items:
                type: string
              example: ["v2", "grpc-v2"]
            recommendedResources:
              $ref: "#/components/schemas/ServingRuntimeResourceRecommendation"
            defaultArgs:
              type: array
              description: Default container args for the generated ServingRuntime.
              items:
                type: string
              example: ["--max-model-len", "4096"]
            env:
              type: array
              description: Environment variables the runtime accepts (discovery hints; no secret values).
              items:
                $ref: "#/components/schemas/ServingRuntimeEnvVar"
            template:
              type: string
              description: >-
                Optional full ServingRuntime (KServe v1alpha1) manifest for this
                version, as a JSON-encoded string, ready for review/edit before
                creation. Mirrors AgentTemplateArtifact.content. If omitted, the
                consumer generates the manifest from the fields above.
            deprecated:
              type: boolean
              description: Whether this version is deprecated and should be de-emphasized in the UI.
              default: false
            publishedDate:
              type: string
              format: date-time
              description: Publication timestamp for this version/image.

    SupportedModelFormat:
      description: A model format a runtime can serve. Maps to ServingRuntime.spec.supportedModelFormats[].
      type: object
      required:
        - name
      properties:
        name:
          type: string
          description: Format name.
          example: safetensors
        version:
          type: string
          description: Optional format version.
        autoSelect:
          type: boolean
          description: Whether this runtime may be auto-selected for the format.
          default: false
        priority:
          type: integer
          format: int32
          description: Selection priority when multiple runtimes support the format.

    ServingRuntimeSupportLevel:
      description: >-
        Support/provenance badge for a runtime version. A display label usable
        for filtering; carries no ordering semantics.
      type: string
      enum:
        - supported          # Red Hat supported
        - techPreview
        - developerPreview
        - community

    ServingRuntimeCapabilities:
      description: High-level capability flags to aid discovery and filtering.
      type: object
      properties:
        requiresGPU:
          type: boolean
          description: Whether the runtime requires a GPU/accelerator.
          default: false
        supportedAccelerators:
          type: array
          description: Accelerator types this runtime supports.
          items:
            type: string
          example: ["nvidia.com/gpu", "amd.com/gpu"]
        multiModel:
          type: boolean
          description: Whether the runtime can serve multiple models (ModelMesh-style). Maps to ServingRuntime.spec.multiModel.
          default: false

    ServingRuntimeResourceRecommendation:
      description: >-
        Recommended resource requests/limits for deploying a runtime version.
        Users should adjust based on model size and traffic. Same tiered shape as
        the MCP plugin's MCPResourceRecommendation, with accelerators added.
      type: object
      properties:
        minimal:
          $ref: "#/components/schemas/ResourceTier"
        recommended:
          $ref: "#/components/schemas/ResourceTier"
        high:
          $ref: "#/components/schemas/ResourceTier"

    ResourceTier:
      type: object
      properties:
        cpu:
          type: string
          example: "4"
        memory:
          type: string
          example: 16Gi
        accelerator:
          type: object
          description: Accelerator request, e.g. {"nvidia.com/gpu": "1"}.
          additionalProperties:
            type: string
          example:
            nvidia.com/gpu: "1"

    ServingRuntimeEnvVar:
      description: An environment variable the runtime accepts. Discovery hint only; never carries secret values.
      type: object
      required:
        - name
      properties:
        name:
          type: string
          example: HF_TOKEN
        description:
          type: string
          example: Hugging Face token used to pull gated models.
        required:
          type: boolean
          default: false
        defaultValue:
          type: string
          description: Default value for optional, non-secret variables only.
        secret:
          type: boolean
          description: Whether the value should come from a Secret (never defaulted).
          default: false

    CatalogServingRuntimeList:
      description: List of CatalogServingRuntime entities.
      allOf:
        - type: object
          required:
            - items
          properties:
            items:
              type: array
              items:
                $ref: "#/components/schemas/CatalogServingRuntime"
        - $ref: "#/components/schemas/BaseResourceList"

    CatalogServingRuntimeVersionList:
      description: List of CatalogServingRuntimeVersion entities.
      allOf:
        - type: object
          required:
            - items
          properties:
            items:
              type: array
              items:
                $ref: "#/components/schemas/CatalogServingRuntimeVersion"
        - $ref: "#/components/schemas/BaseResourceList"
```

## Sample data + source registration

Source registration in `sources.yaml` (parallel to the `mcp_catalogs` /
`model_catalogs` blocks):

```yaml
serving_runtime_catalogs:
  - name: "Red Hat Serving Runtimes"
    id: rh_serving_runtimes
    type: yaml
    enabled: true
    properties:
      yamlCatalogPath: serving-runtimes.yaml
    labels:
      - Red Hat
```

Sample `serving-runtimes.yaml` (loader input; every field needs both `yaml` and
`json` struct tags per the plugin guide):

```yaml
serving_runtimes:
  - name: vllm
    displayName: vLLM
    provider: Red Hat
    description: High-performance generative AI serving runtime for LLMs.
    tags: ["llm", "generative-ai", "gpu"]
    license: apache-2.0
    repositoryUrl: https://github.com/vllm-project/vllm
    capabilities:
      requiresGPU: true
      supportedAccelerators: ["nvidia.com/gpu"]
      multiModel: false
    supportedModelFormats:
      - name: safetensors
        autoSelect: true
      - name: pytorch
    versions:
      - version: "0.5.0"
        image: registry.redhat.io/rhoai/vllm-rhel9:0.5.0
        supportLevel: supported
        protocolVersions: ["v2"]
        recommendedResources:
          recommended:
            cpu: "4"
            memory: 16Gi
            accelerator:
              nvidia.com/gpu: "1"
        defaultArgs: ["--max-model-len", "4096"]
        env:
          - name: HF_TOKEN
            description: Token for gated Hugging Face models.
            secret: true

  - name: ovms
    displayName: OpenVINO Model Server
    provider: Intel
    description: Serving runtime optimized for CPU inference.
    supportLevel: supported
    capabilities:
      requiresGPU: false
    supportedModelFormats:
      - name: openvino_ir
      - name: onnx
    versions:
      - version: "2024.4"
        image: registry.redhat.io/rhoai/openvino-rhel9:2024.4
        supportLevel: supported
        recommendedResources:
          recommended:
            cpu: "2"
            memory: 8Gi
```

## Admin controls mapping (RHAISTRAT-1988 P0/P1)

| Requirement | Where it lives |
|---|---|
| Enable/disable runtimes | Source-level `enabled` flag + `sourceLabel` filtering already in the framework; per-runtime toggle stays in `OdhDashboardConfig` (dashboard side). |
| Registry mirror override (disconnected) | Catalog stores canonical `image`; the dashboard rewrites the registry host at manifest-generation time. The catalog schema stays mirror-agnostic. |
| Version pinning (P1) | Enforced by filtering `CatalogServingRuntimeVersion` artifacts (e.g. `filterQuery=version LIKE "0.5.%"`) or by admin config on the dashboard. |
| Supported vs Community badge | `ServingRuntimeSupportLevel` on each version; filterable and sortable. |

## Open questions

1. **Catalog service vs ConfigMap.** RHAISTRAT-1988 assumes a dashboard-read
   ConfigMap. Do we adopt the catalog-service plugin (this proposal) for true
   Model-Catalog parity, keep the ConfigMap, or bridge (ConfigMap as a `type` of
   catalog source feeding the plugin)? *(Owner: PM/Eng — see the RFE's "Integration
   point with Daniele Zonca's proposal?" question.)*
2. **Versions as artifacts vs inline array.** This proposal models versions as
   `artifact` entities (matching the model plugin) while still surfacing them
   inline on the detail endpoint. Confirm the dashboard prefers the `/versions`
   endpoint for pagination.
3. **`template` string vs structured spec.** Ship the full ServingRuntime
   manifest as a JSON string (like `AgentTemplateArtifact.content`) or generate it
   client-side from typed fields? A string is opaque but future-proof against CRD
   drift; typed fields are queryable.
4. **Scope of v1alpha2 LLMInferenceService.** The RFE scopes MVP to
   InferenceService v1beta1. The schema above is CRD-version-agnostic; confirm
   whether `protocolVersions`/`template` need to distinguish the two paths.
5. **API version.** *Resolved:* the plugin ships **`v1` only** (matching the
   `skill` plugin), under `/api/serving_runtime_catalog/v1`. There is no
   `v1alpha1` surface for this catalog.
```
