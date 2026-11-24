package mlflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/golang/glog"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
)

// reserveResource reserves a logical identity invisibly. A reserved row
// left behind by a crashed attempt — between reservation and intent
// preparation — is adopted rather than duplicated, so a retry of the
// same create converges on one identity.
func (s *Service) reserveResource(ctx context.Context, object Object) (Object, error) {
	object.Visibility = VisibilityReserved
	if object.CommittedJSON == "" {
		object.CommittedJSON = "{}"
	}
	reserved, err := s.store.ReserveObject(ctx, object)
	if err == nil {
		return reserved, nil
	}
	if !errors.Is(err, api.ErrConflict) {
		return Object{}, err
	}

	// The name ( or external ID ) is taken. If the conflicting row is
	// our own invisible reservation of the same identity, adopt it.
	var existing Object
	lookup := s.store.DB().WithContext(ctx).
		Where("kind = ? AND name = ? AND artifact_scope_id = ?", object.Kind, object.Name, object.ArtifactScopeID)
	if object.ParentID != 0 {
		lookup = lookup.Where("parent_id = ?", object.ParentID)
	}
	if err := lookup.First(&existing).Error; err != nil {
		// The conflicting identity is not this name — for example the
		// external ID belongs to another row. The conflict stands.
		return Object{}, fmt.Errorf("%s %q conflicts with an existing identity: %w",
			object.Kind, object.Name, api.ErrConflict)
	}
	if existing.Visibility != VisibilityReserved {
		return Object{}, fmt.Errorf("%s %q already exists: %w", object.Kind, object.Name, api.ErrConflict)
	}
	return existing, nil
}

// driveCreate prepares and applies a create intent on a reserved row.
// A retry whose earlier intent is still active drives that intent to
// completion instead of failing on the conflicting journal entry.
func (s *Service) driveCreate(ctx context.Context, reserved Object,
	prepare func() (Operation, error)) (Object, error) {
	operation, err := prepare()
	if err != nil {
		return Object{}, err
	}
	if err := s.ApplyOperation(ctx, operation.OperationUUID); err != nil {
		return Object{}, err
	}
	return s.store.GetObject(ctx, reserved.ID)
}

// ApplyOperation drives one journaled operation to completion: it
// claims the lease, performs the operation kind's native work with
// checkpointed in-doubt handling, and publishes the committed state in
// the final SQL transaction. A succeeded operation is a no-op, so
// retries are safe. Unknown outcomes retain the intent and surface as
// unavailable; the next application reconciles.
//
// Only one goroutine per replica drives an operation at a time: a
// second caller — an inline request racing this replica's own sweep —
// waits bounded for the first application, then reports the observed
// outcome instead of re-claiming the lease and invalidating the
// in-flight fencing token.
func (s *Service) ApplyOperation(ctx context.Context, operationID string) (applyErr error) {
	operation, err := s.store.ResolveAttempt(ctx, operationID)
	if err != nil {
		return err
	}
	operationID = operation.OperationUUID
	if s.beginApplication(operationID) {
		return s.awaitAppliedOperation(ctx, operationID)
	}
	defer s.endApplication(operationID)

	if operation.State == OperationSucceeded {
		return nil
	}
	defer func() {
		if applyErr == nil && operation.Kind != OperationKindAliasRepair && operation.Kind != OperationKindVersionReplaceChild {
			if err := s.ReconcileAliases(ctx); err != nil {
				glog.Warningf("MLflow alias repair after publication: %v", err)
			}
		}
	}()

	// The lease covers the whole application — native create, bounded
	// registration polling, tag and alias writes — not one request. The
	// worker identity is per replica, so twin replicas contend for the
	// lease instead of silently sharing it.
	claimed, err := s.store.ClaimOperation(ctx, operationID,
		s.workerID, 3*s.config.RequestTimeout)
	if err != nil {
		return err
	}

	switch operation.Kind {
	case OperationKindAliasRepair:
		return s.applyAliasRepair(ctx, claimed)
	case OperationKindModelCreate:
		return s.applyModelCreate(ctx, claimed)
	case OperationKindModelUpdate:
		return s.applyModelUpdate(ctx, claimed)
	case OperationKindVersionCreate:
		// SQL-only publication: the candidate is already immutable in
		// the journal.
		return s.store.PublishOperation(ctx, operationID, claimed.FencingToken)
	case OperationKindVersionUpdate:
		return s.applyVersionUpdate(ctx, claimed)
	case OperationKindVersionMaterialize, OperationKindVersionReplace, OperationKindVersionCreateRetry:
		return s.Materialize(ctx, operationID)
	case OperationKindVersionReplaceFanOut:
		return s.applyVersionReplaceFanOut(ctx, claimed)
	case OperationKindVersionReplaceChild:
		return s.applyReplaceChild(ctx, claimed)
	case OperationKindArtifactAttach, OperationKindArtifactUpdate:
		return s.applyArtifactProjection(ctx, claimed)
	default:
		return fmt.Errorf("operation %s has unknown kind %q: %w",
			operationID, operation.Kind, api.ErrNotImplemented)
	}
}

// awaitAppliedOperation waits bounded for another goroutine of this
// replica to finish driving one operation. A succeeded application is
// a success; anything else — still running, failed, or retriable —
// surfaces as unavailable so the caller retries later.
func (s *Service) awaitAppliedOperation(ctx context.Context, operationID string) error {
	deadline := time.Now().Add(s.config.RequestTimeout)
	for {
		operation, err := s.store.GetOperation(ctx, operationID)
		if err != nil {
			return err
		}
		if operation.State == OperationSucceeded {
			return nil
		}
		if !s.applying(operationID) {
			// The first application finished without publishing; the
			// journal holds the retriable intent.
			return fmt.Errorf("operation %s did not complete: %w", operationID, api.ErrUnavailable)
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return fmt.Errorf("operation %s is still being applied: %w", operationID, api.ErrUnavailable)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation %s is still being applied: %w", operationID, api.ErrUnavailable)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// applyModelCreate creates the native registered model and publishes
// the reserved logical row. The create is dispatched only after the
// CREATE_DISPATCHED checkpoint, so a crash between dispatch and the
// native response reconciles by exact name and ownership instead of
// guessing.
func (s *Service) applyModelCreate(ctx context.Context, claimed Operation) error {
	candidate, resource, err := decodeOperationResource(claimed)
	if err != nil {
		return err
	}
	var model openapi.RegisteredModel
	if err := decodeInto(resource, &model); err != nil {
		return err
	}
	change := candidate.Objects[0]
	owned := change.Snapshot.OwnedTags

	// Reconcile an in-doubt create: the dispatch checkpoint exists but
	// the native outcome was never recorded.
	if claimed.Step == StepCreateDispatched || claimed.Step == StepNativeCreated {
		_, found, err := s.lookupOwnedModel(ctx, claimed, model.Name)
		if err != nil {
			return err
		}
		if found {
			// Ours by exact name and ownership: adopt the native result.
			if err := s.store.CheckpointOperation(ctx, claimed.OperationUUID,
				claimed.FencingToken, StepNativeCreated, nil); err != nil {
				return err
			}
			return s.store.PublishOperation(ctx, claimed.OperationUUID, claimed.FencingToken)
		}
		// Not found: the dispatched create evidently failed or was
		// removed; fall through and dispatch again. The dispatch
		// checkpoint is already recorded.
	}

	if claimed.Step != StepCreateDispatched && claimed.Step != StepNativeCreated {
		if err := s.store.CheckpointOperation(ctx, claimed.OperationUUID,
			claimed.FencingToken, StepCreateDispatched, nil); err != nil {
			return err
		}
	}

	_, err = s.native.CreateRegisteredModel(ctx, client.CreateRegisteredModelRequest{
		Name:        model.Name,
		Description: derefString(model.Description),
		Tags:        tagSlice(owned),
	})
	if err != nil {
		// A definitive duplicate may be our own earlier create whose
		// outcome was never checkpointed; reconcile by name and
		// ownership rather than failing the intent.
		if typed, ok := client.AsError(err); ok && typed.IsDuplicate() {
			_, found, lookupErr := s.lookupOwnedModel(ctx, claimed, model.Name)
			if lookupErr != nil {
				return lookupErr
			}
			if found {
				if err := s.store.CheckpointOperation(ctx, claimed.OperationUUID,
					claimed.FencingToken, StepNativeCreated, nil); err != nil {
					return err
				}
				return s.store.PublishOperation(ctx, claimed.OperationUUID, claimed.FencingToken)
			}
			// An unrelated native model owns the name: never overwrite
			// or adopt it.
			return fmt.Errorf("native registry already contains a model named %q: %w",
				model.Name, api.ErrConflict)
		}
		return MapClientError(err)
	}

	if err := s.store.CheckpointOperation(ctx, claimed.OperationUUID,
		claimed.FencingToken, StepNativeCreated, nil); err != nil {
		return err
	}
	return s.store.PublishOperation(ctx, claimed.OperationUUID, claimed.FencingToken)
}

// FailureCodeOperatorRetry marks an operation superseded by an
// operator-authorized create retry. The superseded row keeps its
// execution step, native result, and attempt count as evidence.
const FailureCodeOperatorRetry = "OPERATOR_AUTHORIZED_CREATE_RETRY"

// OperationKindVersionCreateRetry is the kind of an operator-authorized
// retry of an uncertain native version create. It drives the same
// materialization executor as its parent kind; the distinct kind keeps
// the retry a separate auditable journal row — linked through its
// parent UUID — where the one-intent-per-resource unique key would
// otherwise force it to join the superseded original.
const OperationKindVersionCreateRetry = "version-create-retry"

// isVersionCreateKind reports whether an operation kind dispatches a
// native model-version create whose outcome can be uncertain.
func isVersionCreateKind(kind string) bool {
	return kind == OperationKindVersionMaterialize || kind == OperationKindVersionReplace ||
		kind == OperationKindVersionCreateRetry || kind == OperationKindVersionReplaceChild
}

// requireResolvableOperation rejects operations an operator cannot
// resolve explicitly: terminal ones, and kinds that never dispatch an
// uncertain native create.
func requireResolvableOperation(operation Operation, action string) error {
	if operation.State == OperationSucceeded || operation.State == OperationFailed {
		return fmt.Errorf("operation %s is already %s; %s cannot resolve it: %w",
			operation.OperationUUID, operation.State, action, api.ErrConflict)
	}
	if !isVersionCreateKind(operation.Kind) {
		return fmt.Errorf("operation %s is a %s; %s applies to native version creates: %w",
			operation.OperationUUID, operation.Kind, action, api.ErrBadRequest)
	}
	return nil
}

// retryCandidateJSON rewrites a superseded operation's candidate for
// its retry: the operation identity tag, binding result reference, and
// history reference point at the retry, so the retry's native create
// and later reconciliations identify the retry's own work.
func retryCandidateJSON(candidate *MutationCandidate, retryID string) (string, error) {
	if candidate.NativeWork != nil {
		candidate.NativeWork.OwnedTags[tagOperation] = retryID
	}
	if len(candidate.Objects) > 0 && candidate.Objects[0].Snapshot.OwnedTags != nil {
		candidate.Objects[0].Snapshot.OwnedTags[tagOperation] = retryID
	}
	if len(candidate.Bindings) == 1 {
		candidate.Bindings[0].ResultOperationUUID = retryID
	}
	if len(candidate.History) == 1 {
		candidate.History[0].ResultOperationUUID = retryID
		candidate.History[0].Value.OperationUUID = retryID
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return "", fmt.Errorf("serializing the retried candidate: %w", err)
	}
	return string(encoded), nil
}

// lookupOwnedModel finds a native model by exact name and reports
// whether it carries this adapter's ownership tags. A same-named model
// without our tags is an unrelated model: reported as found only when
// owned, and as a conflict otherwise.
func (s *Service) lookupOwnedModel(ctx context.Context, claimed Operation,
	name string) (client.RegisteredModel, bool, error) {
	native, err := s.native.GetRegisteredModel(ctx, name)
	if err != nil {
		if typed, ok := client.AsError(err); ok && typed.IsNotFound() {
			return client.RegisteredModel{}, false, nil
		}
		return client.RegisteredModel{}, false, MapClientError(err)
	}

	adapterUUID := ""
	logicalID := ""
	for _, tag := range native.Tags {
		switch tag.Key {
		case tagAdapterUUID:
			adapterUUID = tag.Value
		case tagLogicalID:
			logicalID = tag.Value
		}
	}
	if adapterUUID == "" || logicalID == "" {
		// The registry target is shared with non-adapter models: never
		// overwrite or adopt an unowned model.
		return client.RegisteredModel{}, false, fmt.Errorf(
			"native registry contains an unrelated model named %q: %w", name, api.ErrConflict)
	}
	adapter, err := s.adapterIdentity(ctx)
	if err != nil {
		return client.RegisteredModel{}, false, err
	}
	if adapterUUID != adapter.AdapterUUID || logicalID != strconv.FormatInt(int64(claimed.ResourceID), 10) {
		return client.RegisteredModel{}, false, fmt.Errorf(
			"native registry contains a model named %q owned by another adapter or logical id: %w",
			name, api.ErrConflict)
	}
	return *native, true, nil
}

// applyModelUpdate synchronizes a registered model's native description
// and tag projection, then publishes the new snapshot. Every native
// write is idempotent, so an interrupted update is safely re-run.
func (s *Service) applyModelUpdate(ctx context.Context, claimed Operation) error {
	candidate, resource, err := decodeOperationResource(claimed)
	if err != nil {
		return err
	}
	var model openapi.RegisteredModel
	if err := decodeInto(resource, &model); err != nil {
		return err
	}
	if err := s.verifyAliasParent(ctx, claimed.ResourceID, model.Name); err != nil {
		return err
	}
	change := candidate.Objects[0]

	previous, err := s.store.GetObject(ctx, claimed.ResourceID)
	if err != nil {
		return err
	}
	priorSnapshot, err := decodeStoredSnapshot(previous.CommittedJSON)
	if err != nil {
		return err
	}

	// Description first, then the tag diff: delete removed keys, set
	// changed or new ones. Both operations are idempotent.
	if derefString(model.Description) != priorSnapshotDescription(priorSnapshot) {
		if err := s.native.UpdateRegisteredModel(ctx, client.UpdateRegisteredModelRequest{
			Name:        model.Name,
			Description: derefString(model.Description),
		}); err != nil {
			return MapClientError(err)
		}
	}

	owned := change.Snapshot.OwnedTags
	for _, key := range sortedKeys(priorSnapshot.OwnedTags) {
		if _, kept := owned[key]; !kept {
			if err := s.native.DeleteRegisteredModelTag(ctx, model.Name, key); err != nil {
				return MapClientError(err)
			}
		}
	}
	for _, key := range sortedKeys(owned) {
		if prior, exists := priorSnapshot.OwnedTags[key]; !exists || prior != owned[key] {
			if err := s.native.SetRegisteredModelTag(ctx, model.Name, key, owned[key]); err != nil {
				return MapClientError(err)
			}
		}
	}

	return s.store.PublishOperation(ctx, claimed.OperationUUID, claimed.FencingToken)
}

// decodeOperationResource decodes the candidate and returns the
// operation's single changed resource payload.
func decodeOperationResource(operation Operation) (*MutationCandidate, []byte, error) {
	candidate, err := decodeMutationCandidate(operation.CandidateJSON)
	if err != nil {
		return nil, nil, err
	}
	if candidate == nil || len(candidate.Objects) != 1 {
		return nil, nil, fmt.Errorf("operation %s does not carry a single-resource candidate: %w",
			operation.OperationUUID, api.ErrBadRequest)
	}
	return candidate, candidate.Objects[0].Snapshot.Resource, nil
}

func decodeInto(resource []byte, into any) error {
	if err := json.Unmarshal(resource, into); err != nil {
		return fmt.Errorf("decoding operation resource: %w: %w", err, api.ErrBadRequest)
	}
	return nil
}

func priorSnapshotDescription(snapshot StoredSnapshot) string {
	var prior struct {
		Description *string `json:"description"`
	}
	_ = json.Unmarshal(snapshot.Resource, &prior)
	return derefString(prior.Description)
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// tagSlice renders an owned-tag projection as deterministic native
// tags.
func tagSlice(owned map[string]string) []client.Tag {
	tags := make([]client.Tag, 0, len(owned))
	for _, key := range sortedKeys(owned) {
		tags = append(tags, client.Tag{Key: key, Value: owned[key]})
	}
	return tags
}

func sortedKeys(m map[string]string) []string {
	keys := slices.Collect(maps.Keys(m))
	slices.Sort(keys)
	return keys
}
