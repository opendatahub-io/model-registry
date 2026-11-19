package mlflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
)

// pollInterval is the pause between native model-version status checks
// while a created version finishes registering.
const pollInterval = 50 * time.Millisecond

// materializeVersion prepares and drives the operation that turns a
// pending logical version into a native model version. The primary
// artifact's real source becomes the native version's source; the
// artifact, its link, the primary binding, and the first history
// generation commit atomically only after the native create succeeds
// and the journal records its result. Concurrent primary attachments
// serialize through the binding revision compare-and-swap inside the
// publication transaction: the first one wins and the loser conflicts.
func (s *Service) materializeVersion(ctx context.Context, model, version Object,
	reserved Object, artifact *openapi.ModelArtifact, binding VersionBinding) (*openapi.Artifact, error) {
	operationUUID := uuid.NewString()
	generation := binding.ActiveGeneration + 1
	nativeModelName := model.Name

	owned, err := EncodeArtifact(artifact, true)
	if err != nil {
		return nil, err
	}

	alias := binding.Alias
	if alias == "" {
		alias = NormalizedAlias(version.Name)
	}

	source := derefString(artifact.Uri)
	newBinding := binding
	newBinding.ModelID = model.ID
	newBinding.PrimaryArtifactID = new(reserved.ID)
	newBinding.Phase = PhaseReady
	newBinding.NativeModelName = new(nativeModelName)
	newBinding.ActiveGeneration = generation
	newBinding.Alias = alias

	candidate := MutationCandidate{
		CodecVersion: candidateCodecVersion,
		Objects: []ObjectChange{{
			ID: reserved.ID, ExpectedRevision: reserved.Revision,
			Snapshot: StoredSnapshot{
				CodecVersion: candidateCodecVersion, Kind: KindModelArtifact,
				Resource:  mustMarshal(artifactResource(artifact)),
				TagKeys:   customTagKeys(artifact.CustomProperties),
				OwnedTags: owned,
			},
		}},
		Links: []ArtifactLink{{ArtifactID: reserved.ID, VersionID: version.ID}},
		Bindings: []BindingChange{{
			ExpectedRevision: binding.Revision,
			Value:            newBinding,
			// The final native identity — the server-generated version
			// number — resolves from this operation's own checkpoint at
			// publication.
			ResultOperationUUID: operationUUID,
		}},
		History: []HistoryChange{{
			Value: VersionHistory{
				VersionID: version.ID, Generation: generation,
				NativeModelName: nativeModelName,
				Source:          source, OperationUUID: operationUUID,
			},
			ResultOperationUUID: operationUUID,
		}},
	}

	additional, err := s.additionalArtifactSnapshots(ctx, version.ID, reserved.ID)
	if err != nil {
		return nil, err
	}
	plan, revisions, err := s.buildNativeVersionPlan(ctx, version, projectionPrimary(reserved, candidate.Objects[0]), additional, binding, operationUUID, generation, false)
	if err != nil {
		return nil, err
	}
	candidate.NativeWork = &plan
	candidate.ReadRevisions = revisions
	candidate.BindingReadRevisions = map[int32]int64{version.ID: binding.Revision}

	published, err := s.driveCreate(ctx, reserved, func() (Operation, error) {
		return prepareCandidate(ctx, s.store, operationUUID, reserved.ID,
			OperationKindVersionMaterialize, reserved.Revision, generation, candidate)
	})
	if err != nil {
		return nil, err
	}
	view, err := artifactView(published)
	if err != nil {
		return nil, err
	}
	return &openapi.Artifact{ModelArtifact: view}, nil
}

// Materialize drives a claimed version-materialize operation to
// completion: it dispatches the native model-version create with the
// primary artifact's real source, checkpoints the native identity,
// waits for registration to finish, applies the owned tags and the
// adapter-owned alias, and publishes the committed state in one SQL
// transaction. An unknown native outcome retains the intent and
// surfaces as unavailable; the next application reconciles by the
// operation tag.
func (s *Service) Materialize(ctx context.Context, operationID string) error {
	operation, err := s.store.GetOperation(ctx, operationID)
	if err != nil {
		return err
	}
	if operation.State == OperationSucceeded {
		return nil
	}

	candidate, err := decodeMutationCandidate(operation.CandidateJSON)
	if err != nil {
		return err
	}
	if candidate == nil || len(candidate.Bindings) != 1 || len(candidate.History) != 1 {
		return fmt.Errorf("operation %s does not carry a materialization candidate: %w",
			operationID, api.ErrBadRequest)
	}
	binding := candidate.Bindings[0].Value
	history := candidate.History[0].Value
	if binding.NativeModelName == nil {
		return fmt.Errorf("operation %s has no native model name: %w", operationID, api.ErrBadRequest)
	}
	// A checkpoint resumes projection without dispatching create again. Repeat
	// the parent identity check before any legacy description or tag replay.
	if err := s.verifyAliasParent(ctx, binding.ModelID, *binding.NativeModelName); err != nil {
		return err
	}
	plan, err := s.nativeVersionPlan(ctx, operation, operation)
	if err != nil {
		return err
	}
	modelName, source := plan.ModelName, plan.Source
	createTags := nativePlanTags(plan)
	fence := operation.FencingToken

	var native *client.ModelVersion
	if operation.NativeResult != nil {
		// An earlier application — or a verified operator bind — already
		// checkpointed the native identity under a fencing token. Resume
		// from the checkpoint instead of dispatching or searching again.
		var adopted NativeBindingResult
		if err := json.Unmarshal([]byte(*operation.NativeResult), &adopted); err != nil {
			return fmt.Errorf("decoding the native result of operation %s: %w: %w",
				operationID, err, api.ErrBadRequest)
		}
		if adopted.VersionNumber == "" || adopted.ModelName != modelName ||
			(adopted.Generation != 0 && adopted.Generation != history.Generation) {
			return fmt.Errorf("operation %s recorded a native result that does not match its candidate: %w",
				operationID, api.ErrConflict)
		}
		native = &client.ModelVersion{
			Name: adopted.ModelName, Version: adopted.VersionNumber, Status: adopted.Status,
		}
	} else {
		native, err = s.dispatchVersionCreate(ctx, operation, plan)
		if err != nil {
			return err
		}
	}

	// Wait for registration to finish before publishing: the committed
	// version must be usable, not merely accepted.
	native, err = s.awaitReady(ctx, modelName, native)
	if err != nil {
		return err
	}

	result, err := json.Marshal(NativeBindingResult{
		ModelName: modelName, VersionNumber: native.Version,
		Source: source, Generation: history.Generation, Status: native.Status,
	})
	if err != nil {
		return fmt.Errorf("serializing native result for operation %s: %w", operationID, err)
	}
	if err := s.store.CheckpointOperation(ctx, operationID,
		fence, StepNativeCreated, new(string(result))); err != nil {
		return err
	}

	if candidate.NativeWork == nil {
		if err := s.native.UpdateModelVersion(ctx, client.UpdateVersionRequest{Name: modelName, Version: native.Version, Description: plan.Description}); err != nil {
			return MapClientError(err)
		}
	}

	// Apply the complete frozen tag projection and adapter-owned alias. Both
	// are idempotent, so an interrupted materialization re-runs them
	// safely.
	for _, tag := range createTags {
		if err := s.native.SetModelVersionTag(ctx, modelName, native.Version, tag.Key, tag.Value); err != nil {
			return MapClientError(err)
		}
	}
	if err := s.store.CheckpointOperation(ctx, operationID,
		fence, StepTagsApplied, nil); err != nil {
		return err
	}
	if err := s.moveAdapterAlias(ctx, binding.VersionID, NativeBindingResult{ModelName: modelName,
		VersionNumber: native.Version, Source: source, Generation: history.Generation}, binding.Alias); err != nil {
		return err
	}
	if err := s.store.CheckpointOperation(ctx, operationID,
		fence, StepAliasMoved, nil); err != nil {
		return err
	}
	return s.store.PublishOperation(ctx, operationID, fence)
}

// dispatchVersionCreate creates the native model version for an
// operation, or reconciles an earlier dispatch whose outcome was never
// recorded. The CREATE_DISPATCHED checkpoint is persisted before the
// request leaves; once it exists, the create is never blindly retried:
// reconciliation searches every page for the operation's identity tag,
// adopts exactly one verified match, treats multiple matches as a
// conflict, and keeps the operation in doubt on zero matches — even
// after a gateway error or a crash before send.
func (s *Service) dispatchVersionCreate(ctx context.Context, operation Operation, plan *NativeVersionPlan) (*client.ModelVersion, error) {
	version, err := s.store.GetObject(ctx, plan.LogicalVersionID)
	if err != nil {
		return nil, err
	}
	if err := s.verifyAliasParent(ctx, version.ParentID, plan.ModelName); err != nil {
		return nil, err
	}
	candidate, err := decodeMutationCandidate(operation.CandidateJSON)
	if err != nil {
		return nil, err
	}
	legacy := candidate == nil || candidate.NativeWork == nil
	if operation.NativeResult != nil {
		var result NativeBindingResult
		if err := json.Unmarshal([]byte(*operation.NativeResult), &result); err != nil {
			return nil, err
		}
		if result.ModelName != plan.ModelName || result.VersionNumber == "" || result.Source != plan.Source || result.Generation != plan.Generation {
			return nil, fmt.Errorf("native checkpoint differs from work order: %w", api.ErrConflict)
		}
		native, err := s.native.GetModelVersion(ctx, result.ModelName, result.VersionNumber)
		if err != nil {
			return nil, MapClientError(err)
		}
		if native.Version != result.VersionNumber {
			return nil, fmt.Errorf("native version differs from checkpoint: %w", api.ErrConflict)
		}
		return native, s.verifyNativeVersionIdentity(ctx, operation, plan, native, legacy)
	}
	reconcile := func() (*client.ModelVersion, error) {
		native, err := s.reconcileDispatchedCreate(ctx, operation, plan.ModelName)
		if err != nil {
			return nil, err
		}
		if err := s.verifyNativeVersionIdentity(ctx, operation, plan, native, legacy); err != nil {
			return nil, err
		}
		return native, nil
	}
	// Once dispatch is recorded, only exact reconciliation can select a result.
	if operation.Step != StepPrepared && operation.Step != StepClaimed {
		return reconcile()
	}
	if err := s.store.CheckpointOperation(ctx, operation.OperationUUID, operation.FencingToken, StepCreateDispatched, nil); err != nil {
		return nil, err
	}
	created, err := s.native.CreateModelVersion(ctx, client.CreateVersionRequest{Name: plan.ModelName, Source: plan.Source, Description: plan.Description, Tags: nativePlanTags(plan)})
	if err != nil {
		if typed, ok := client.AsError(err); ok && typed.IsDuplicate() {
			return reconcile()
		}
		return nil, MapClientError(err)
	}
	return created, nil
}

// reconcileDispatchedCreate searches every page of the native model's
// versions for one carrying this operation's identity tag.
func (s *Service) reconcileDispatchedCreate(ctx context.Context, operation Operation,
	modelName string) (*client.ModelVersion, error) {
	matches, err := s.lookupOwnedVersions(ctx, modelName, operation.OperationUUID)
	if err != nil {
		return nil, err
	}
	switch len(matches) {
	case 1:
		return &matches[0], nil
	case 0:
		return nil, fmt.Errorf("operation %s dispatched a native create for %q whose outcome is unknown; no owned native version was found: %w",
			operation.OperationUUID, modelName, api.ErrUnavailable)
	default:
		return nil, fmt.Errorf("operation %s found %d native versions carrying its identity tag: %w",
			operation.OperationUUID, len(matches), api.ErrConflict)
	}
}

// lookupOwnedVersions scans every search page for the native model
// versions carrying this operation's identity tag. The search is left
// unfiltered and the model name is matched exactly on each returned
// version: interpolating an accepted name into a native filter string
// would require guessing the server's string-literal escaping for names
// containing quotes. The tradeoff is scanning all accessible versions
// during recovery; normal create execution is unchanged. Versions of
// other models — even carrying the same operation tag — are never
// adopted.
func (s *Service) lookupOwnedVersions(ctx context.Context, modelName,
	operationUUID string) ([]client.ModelVersion, error) {
	var matches []client.ModelVersion
	request := client.SearchRequest{
		MaxResults: 100,
	}
	for {
		page, err := s.native.SearchModelVersions(ctx, request)
		if err != nil {
			return nil, MapClientError(err)
		}
		for i := range page.ModelVersions {
			version := page.ModelVersions[i]
			if version.Name != modelName {
				continue
			}
			opUUID, adapterUUID := "", ""
			for _, tag := range version.Tags {
				switch tag.Key {
				case tagOperation:
					opUUID = tag.Value
				case tagAdapterUUID:
					adapterUUID = tag.Value
				}
			}
			if opUUID == operationUUID && adapterUUID != "" {
				matches = append(matches, version)
			}
		}
		if page.NextPageToken == "" {
			return matches, nil
		}
		request.PageToken = page.NextPageToken
	}
}

// awaitReady polls a freshly created native model version until its
// registration status is terminal, bounded by the configured request
// timeout. A failed registration is definitive; a still-pending
// version at the deadline surfaces as unavailable with the intent
// retained.
func (s *Service) awaitReady(ctx context.Context, modelName string,
	native *client.ModelVersion) (*client.ModelVersion, error) {
	deadline := time.Now().Add(s.config.RequestTimeout)
	for {
		switch native.Status {
		case "", "READY":
			return native, nil
		case "FAILED_REGISTRATION":
			return nil, fmt.Errorf("native model version %s/%s failed registration: %w",
				modelName, native.Version, api.ErrConflict)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("native model version %s/%s is still %q: %w",
				modelName, native.Version, native.Status, api.ErrUnavailable)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for native model version %s/%s: %w",
				modelName, native.Version, api.ErrUnavailable)
		case <-time.After(pollInterval):
		}
		refreshed, err := s.native.GetModelVersion(ctx, modelName, native.Version)
		if err != nil {
			return nil, MapClientError(err)
		}
		native = refreshed
	}
}
