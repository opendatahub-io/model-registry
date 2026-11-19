// Package mlflow implements an api.ModelRegistryApi backend that stores
// Kubeflow model-registry metadata in MLflow Model Registry, backed by a
// SQL compatibility store for Kubeflow identities, pending entities,
// typed metadata, and operation recovery.
//
// The implementation is introduced incrementally; see
// docs/mlflow-backend.md for the supported scope and behavior contract.
package mlflow

// Operation graph schema generation 2 uses candidate/snapshot codec 3, and
// continues to read codec 2 and legacy flat snapshots. Applying the graph
// migrations requires stopping all old adapter binaries: mixed old/new replicas
// are unsupported, because old binaries cannot honor durable graph reservations.
// Startup hydrates recorded legacy graph edges before native refresh or dispatch;
// ambiguous evidence remains quarantined for explicit operator resolution.

// Codec-2 source operations acquire a separate complete frozen execution plan
// before projection replay. The plan uses committed SQL version and additional
// artifact metadata observed at upgrade, while preserving the original desired
// primary source, candidate JSON, payload hash and dispatch evidence. Its exact
// object and binding revisions are reserved atomically and checked at every
// later checkpoint/publication. A possibly dispatched create is reconciled and
// its parent, source, adapter, logical ID and operation identity verified first;
// older generations without rhoai.generation require the matching journal and
// binding proof. Unverifiable ownership or dependencies quarantine the graph;
// unknown create outcomes remain uncertain and never authorize another create.
