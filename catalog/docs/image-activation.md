# Catalog image activation

Catalog readiness requires image resolution, a matching runtime activation report,
and a healthy rollout. Deploy this operator together with the runtime implementing
activation protocol `v1` (model-registry PR #1914). Build that runtime revision and
set `RELATED_IMAGE_ODH_MODEL_REGISTRY_IMAGE` to its immutable image reference in
the same release. An older runtime cannot establish activation readiness: startup
failure or the ten-minute activation deadline is reported instead of accepting a
Ready pod as evidence. Upgrade the runtime and operator together before enabling
scheduled data-image updates.

## Candidate and ownership

The two selectors introduced in #722 remain independent. Empty selects each
product release image, `stable` follows its managed ImageStream, and a SHA-256
selection pins the configured import repository. Both selected datasets must
activate together. An import failure never authorizes historical data.

`Catalog.status.activation` persists the Catalog UID, opaque attempt ID, request
fingerprint, requested references, resolved image pair, state, stage, reason,
message, start time, observed report content hash, and last successful
activation. Selection, pair, relevant import outcome, runtime image, or explicit
retry changes create a new attempt. Resyncs and unchanged successful imports do
not. Unrelated streams cannot invalidate pinned selections.

The operator publishes `model-catalog-activation` with an `intent.json` document
before changing the workload. It creates a separate
`model-catalog-activation-<attemptID>` ConfigMap for the runtime's `report.json`.
Transport version `v1` includes Catalog UID, attempt ID, both image references,
state, stage, reason, and message. Success includes `activatedAt`. Intent also
contains Catalog name, generation, and the explicit retry token.

Only the operator updates intent and Catalog status. The runtime service account
can GET the named Catalog and intent, and GET/UPDATE only its current report
ConfigMap. It cannot update Catalog status or serving authorization. Reports must
match the current UID, attempt, protocol version, and both immutable references.
A success already present when a failure was observed cannot clear that failure;
a new runtime completion can. Failure survives pod disappearance and controller
restart. Last-success metadata is retained until another activation succeeds.

Tagged product defaults use a pull-only bootstrap pod. Completed init containers
must expose pullable `repository@sha256:...` image IDs; then the operator replaces
that workload with both references pinned. Bare CRI config/image IDs are not
assumed to be pullable registry manifest digests. Where the runtime does not
provide pullable IDs, configure immutable release references. Bootstrap is
subject to the same activation deadline and cannot report success with tags.

## Transaction and serving

The runtime validates all required YAML and benchmark content before initializing
ingestion. It stages each attempt in a separate PostgreSQL schema derived from
its UID, attempt, and image pair. One database transaction imports the required
model, MCP, agent, serving-runtime, artifact, tool, and benchmark records, refreshes
property options, and writes a completion marker. An advisory lock serializes
replicas. Write errors, cancellation, or superseded intent roll back that whole
transaction. A failed replacement never modifies the last successful schema.

Normal loaders skip required origins already imported transactionally. Source
include/exclude filters are honored. Administrator source ConfigMaps remain
managed by the ordinary source workflow; optional user sources retain their
existing availability policy. Required data is reimported after database loss,
and startup/recovery reports success only after the marker commits.

Every runtime data request checks live operator intent, live Catalog UID,
generation and retry token, its own attempt/image pair, and its database marker.
Communication errors fail closed. Catalog edits or deletion invalidate old intent
before the controller next reconciles. Previous replicas and previous schemas
cannot serve as automatic fallback. Standalone runtimes without activation
environment variables retain the existing behavior.

Unavailable data requests return `503`, code `CatalogUnavailable`, stage, reason,
message and attempt ID with `Cache-Control: no-store`. `/healthz` remains a process
health check and `/statusz` exposes the gate state on a running pod. `/readyz`
closes with the gate, so a Service with no ready endpoints may produce an
infrastructure 503 instead of the runtime JSON. The UI backend translates that
catalog outage into an actionable unavailable message and preserves structured
runtime diagnostics when present. Import failures additionally request deployment
scale-down as containment; runtime authorization remains the data-access guard.

## Status, recovery and operations

One reducer sets Catalog `Ready`/`Available` from current selection,
`DataActivationReady`, and workload health. `DataImageActivationFailed` remains
latched during a replacement's progress and clears only on matching success.
Failure reasons flow through the existing Catalog -> AIHub -> DSC status chain.
Unrelated component conditions are preserved by the existing parent reducers.

Inspect `status.activation`, Catalog conditions and Warning Events to distinguish
Selection, Import, Pull, Initialization, Loading, and Activation failures. Correct
the image/source, pin a known-good digest, clear to a valid release default, or
explicitly request another attempt:

```sh
kubectl annotate catalog <name> -n <namespace> \
  aihub.opendatahub.io/retry-activation="$(date -u +%s)" --overwrite
```

A retry closes authorization and revalidates the whole pair. Pinning a previous
pair is a new activation, not permission to serve the old schema. Pending
activation times out after ten minutes. Report-only failures also emit Events.
There is no automatic schema garbage collection: retained schemas are recovery
material, consume database space, and share the database's durability. Do not
manually drop a schema used by a running attempt. This mechanism is not a database
backup, and emptyDir database replacement loses all retained schemas.

## Validation

Tests cover independent pair transitions, stale UID/attempt/pair/protocol reports,
old-success fencing, pod loss, explicit retries, deadlines, tagged release pinning,
unchanged resyncs, restricted report RBAC, Catalog/AIHub failure propagation, live
runtime authorization, and actual PostgreSQL rollback across all four shipped
source types and benchmarks. Cluster acceptance must additionally exercise the
product's parent DSC operator, registry credentials, image importer, rollout and
route behavior using compatible built images; local unit/envtest runs do not
establish those cluster results.
