# Required Catalog image content: first implementation pass

This pass supports RHOAIENG-97414's validation and failure reporting. It does
not implement the complete activation/serving authorization handshake.

## Runtime configuration

The operator supplies:

```text
--required-catalogs-path=/data/default-sources/sources.yaml
--performance-metrics=/shared-benchmark-data
--require-performance-metrics
CATALOG_ACTIVATION_FAILURE_PATH=/dev/termination-log
```

`--required-catalogs-path` identifies shipped source configurations to validate
before datastore initialization, plugin initialization, or ingestion leadership.
It is separate from `--catalogs-path`: administrator and labeled source files
remain governed by the existing source-loading behavior, and preflight never
reads or modifies those files unless explicitly designated required.

Enabled required YAML sources are checked using the relevant model, MCP, agent,
and serving-runtime parsers. Missing files, invalid YAML, missing collection
shape, and basic entity identity errors reject the candidate. Empty explicitly
declared collections remain valid. Required remote/skill sources are not
supported by this first pass.

With required benchmarks enabled, every configured benchmark root must exist
and contain model metadata. All known metadata and NDJSON files are validated,
including files for models that are not currently selected by a Catalog source.
Malformed records reject the entire preflight rather than being skipped. Metric
files remain optional per model, but a present metric file requires sibling
metadata. Existing `config_id` performance record compatibility is preserved.

Standalone use retains the existing optional-content policy unless these flags
are supplied. Required preflight is deliberately not applied to all user sources.

## Failure-only transport

A required-content failure exits before datastore writes and, when configured,
writes a bounded JSON Kubernetes termination message:

```json
{"stage":"Loading","reason":"BenchmarkContentInvalid","message":"..."}
```

`CatalogContentInvalid` identifies required shipped YAML/configuration failures;
`BenchmarkContentInvalid` identifies required benchmark failures. Messages are
bounded to 512 runes to fit Kubernetes' 4 KiB limit even after JSON escaping.

The operator reads failed-container status and publishes Catalog availability,
degradation, a failure-specific condition, and a Warning event. This report is
not an activation-success acknowledgement. Successful preflight must never be
used as evidence that both datasets have been activated in the database.

## Remaining integration

- Consume the persisted complete-pair attempt identity from 97413.
- Stage database writes and atomically publish the complete required dataset.
- Provide genuine success/failure evidence from ingestion, including asynchronous
  provider/database failures after startup.
- Accept only current-attempt activation reports, then grant external serving.
- Enforce serving authorization across replicas, restart, revocation, and lost
  communication, keeping status endpoints available when data serving is denied.
- Complete Catalog -> AIHub -> DSC readiness propagation in the 97413 foundation.

Preflight protects previous data against the malformed-content failures it
detects by rejecting before any writes. It does not guarantee atomicity for
later database errors or prevent older replicas from serving after failure.

The operator and runtime changes must be deployed together: older runtime
images do not recognize the new command-line flags.
