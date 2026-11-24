package mlflow

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
)

// Operation kinds for source replacement.
const (
	OperationKindVersionReplace       = "version-replace"
	OperationKindVersionReplaceFanOut = "version-replace-fanout"
	OperationKindVersionReplaceChild  = "version-replace-child"
)

// replaceChildPayload is the immutable work order a fan-out parent
// records in each child operation: the native model to version under
// and the real source to register.
type replaceChildPayload struct {
	ModelName string `json:"modelName"`
	Source    string `json:"source"`
}

// replacePrimarySource replaces the primary source of a materialized
// version. A single affected version replaces inline through the
// materialization journal; a primary shared by several versions fans
// out one durable child operation per version whose SQL effects commit
// atomically with the parent's publication. Clearing the URI is
// rejected without changing either committed state.
func (s *Service) replacePrimarySource(ctx context.Context, version, existing Object,
	artifact *openapi.ModelArtifact, binding VersionBinding) (*openapi.Artifact, error) {
	source := derefString(artifact.Uri)
	if source == "" {
		return nil, fmt.Errorf("model artifact %d is the materialized primary of version %d; the source cannot be cleared, only replaced: %w",
			existing.ID, version.ID, api.ErrConflict)
	}
	model, err := s.store.GetObject(ctx, version.ParentID)
	if err != nil {
		return nil, err
	}

	// Every version bound to this artifact as its materialized primary
	// is affected by the replacement.
	var affected []VersionBinding
	if err := s.store.DB().WithContext(ctx).
		Where("primary_artifact_id = ? AND native_version_number IS NOT NULL",
			existing.ID).
		Order("version_id ASC").Find(&affected).Error; err != nil {
		return nil, mapDBError(err)
	}
	if len(affected) == 0 {
		// The binding passed in is the only authority; use it.
		affected = []VersionBinding{binding}
	}

	operationUUID := uuid.NewString()
	if len(affected) == 1 {
		return s.replaceSingle(ctx, model, version, existing, artifact,
			affected[0], source, operationUUID)
	}
	return s.replaceFanOut(ctx, model, existing, artifact, affected, source, operationUUID)
}

// replaceSingle drives a one-version replacement through the
// materialization journal: a new native generation is created from the
// new source, the previous history is superseded ( never deleted ),
// and the adapter-owned alias moves only after the new generation is
// ready.
func (s *Service) replaceSingle(ctx context.Context, model, version, existing Object,
	artifact *openapi.ModelArtifact, binding VersionBinding, source,
	operationUUID string) (*openapi.Artifact, error) {
	generation := binding.ActiveGeneration + 1

	owned, err := EncodeArtifact(artifact, true)
	if err != nil {
		return nil, err
	}

	newBinding := binding
	newBinding.ModelID = model.ID
	newBinding.PrimaryArtifactID = new(existing.ID)
	newBinding.Phase = PhaseReady
	newBinding.NativeModelName = new(model.Name)
	newBinding.ActiveGeneration = generation
	// The new generation's native number resolves from the verified
	// result, not the superseded binding.
	newBinding.NativeVersionNumber = nil
	if newBinding.Alias == "" {
		newBinding.Alias = NormalizedAlias(version.Name)
	}

	candidate := MutationCandidate{
		CodecVersion: candidateCodecVersion,
		Objects: []ObjectChange{{
			ID: existing.ID, ExpectedRevision: existing.Revision,
			Snapshot: StoredSnapshot{
				CodecVersion: candidateCodecVersion, Kind: KindModelArtifact,
				Resource:  mustMarshal(artifactResource(artifact)),
				TagKeys:   customTagKeys(artifact.CustomProperties),
				OwnedTags: owned,
			},
		}},
		Links: []ArtifactLink{{ArtifactID: existing.ID, VersionID: version.ID}},
		Bindings: []BindingChange{{
			ExpectedRevision:    binding.Revision,
			Value:               newBinding,
			ResultOperationUUID: operationUUID,
		}},
		History: []HistoryChange{{
			Value: VersionHistory{
				VersionID: version.ID, Generation: generation,
				NativeModelName: model.Name, Source: source,
				OperationUUID: operationUUID,
			},
			SupersedePrevious:   true,
			ResultOperationUUID: operationUUID,
		}},
	}
	additional, err := s.additionalArtifactSnapshots(ctx, version.ID, existing.ID)
	if err != nil {
		return nil, err
	}
	plan, revisions, err := s.buildNativeVersionPlan(ctx, version, projectionPrimary(existing, candidate.Objects[0]), additional, binding, operationUUID, generation, false)
	if err != nil {
		return nil, err
	}
	candidate.NativeWork = &plan
	candidate.ReadRevisions = revisions
	candidate.BindingReadRevisions = map[int32]int64{version.ID: binding.Revision}
	// A retried replacement resumes its still-active intent rather than
	// deadlocking on the journal entry.
	updated, err := s.driveCreate(ctx, existing, func() (Operation, error) {
		return prepareCandidate(ctx, s.store, operationUUID, existing.ID,
			OperationKindVersionReplace, existing.Revision, generation, candidate)
	})
	if err != nil {
		return nil, err
	}
	view, err := artifactView(updated)
	if err != nil {
		return nil, err
	}
	return &openapi.Artifact{ModelArtifact: view}, nil
}

// replaceFanOut prepares one child operation per affected version and
// a parent whose publication switches every affected binding,
// snapshot, and history entry atomically. The children create the
// native generations and checkpoint their results; the parent applies
// the projection tags and moves each adapter-owned alias only after
// every child is ready, then publishes. A partial remote fan-out keeps
// the public artifact URI unchanged and returns unavailable; the
// durable operations recover it on the next application.
func (s *Service) replaceFanOut(ctx context.Context, model, existing Object,
	artifact *openapi.ModelArtifact, affected []VersionBinding, source,
	parentUUID string) (*openapi.Artifact, error) {
	if err := s.preflightFanOutReservations(ctx, existing.ID, affected); err != nil {
		return nil, err
	}
	projection, err := EncodeArtifact(artifact, true)
	if err != nil {
		return nil, err
	}

	var childOperations []Operation
	candidate := MutationCandidate{
		CodecVersion: candidateCodecVersion,
		Objects: []ObjectChange{{
			ID: existing.ID, ExpectedRevision: existing.Revision,
			Snapshot: StoredSnapshot{
				CodecVersion: candidateCodecVersion, Kind: KindModelArtifact,
				Resource:  mustMarshal(artifactResource(artifact)),
				TagKeys:   customTagKeys(artifact.CustomProperties),
				OwnedTags: projection,
			},
		}},
	}
	for _, binding := range affected {
		version, err := s.store.GetObject(ctx, binding.VersionID)
		if err != nil {
			return nil, err
		}
		if version.ParentID != model.ID {
			return nil, fmt.Errorf("version %d does not belong to model %d: %w",
				version.ID, model.ID, api.ErrBadRequest)
		}
		childUUID := uuid.NewString()
		additional, err := s.additionalArtifactSnapshots(ctx, version.ID, existing.ID)
		if err != nil {
			return nil, err
		}
		plan, revisions, err := s.buildNativeVersionPlan(ctx, version, projectionPrimary(existing, candidate.Objects[0]), additional, binding, childUUID, binding.ActiveGeneration+1, false)
		if err != nil {
			return nil, err
		}
		childCandidate := MutationCandidate{CodecVersion: candidateCodecVersion, NativeWork: &plan, ReadRevisions: revisions, BindingReadRevisions: map[int32]int64{version.ID: binding.Revision}}

		payload, err := json.Marshal(childCandidate)
		if err != nil {
			return nil, err
		}
		childOperations = append(childOperations, Operation{OperationUUID: childUUID, ResourceID: version.ID,
			Kind: OperationKindVersionReplaceChild, ExpectedRevision: version.Revision,
			Generation: binding.ActiveGeneration + 1, CandidateJSON: string(payload), PayloadHash: payloadHash(string(payload))})

		generation := binding.ActiveGeneration + 1
		newBinding := binding
		newBinding.ModelID = model.ID
		newBinding.Phase = PhaseReady
		newBinding.NativeModelName = new(model.Name)
		newBinding.ActiveGeneration = generation
		// The child's verified result names the new native version.
		newBinding.NativeVersionNumber = nil
		if newBinding.Alias == "" {
			newBinding.Alias = NormalizedAlias(version.Name)
		}
		candidate.Bindings = append(candidate.Bindings, BindingChange{
			ExpectedRevision:    binding.Revision,
			Value:               newBinding,
			ResultOperationUUID: childUUID,
		})
		candidate.History = append(candidate.History, HistoryChange{
			Value: VersionHistory{
				VersionID: binding.VersionID, Generation: generation,
				NativeModelName: model.Name, Source: source,
				OperationUUID: childUUID,
			},
			SupersedePrevious:   true,
			ResultOperationUUID: childUUID,
		})
	}

	// A retried fan-out resumes its still-active parent intent.
	updated, err := s.driveCreate(ctx, existing, func() (Operation, error) {
		if err := s.store.captureCandidateReads(ctx, &candidate); err != nil {
			return Operation{}, err
		}
		payload, err := json.Marshal(candidate)
		if err != nil {
			return Operation{}, err
		}
		root, _, err := s.store.PrepareOperationGraph(ctx, Operation{OperationUUID: parentUUID,
			ResourceID: existing.ID, Kind: OperationKindVersionReplaceFanOut, ExpectedRevision: existing.Revision,
			Generation: 1, CandidateJSON: string(payload), PayloadHash: payloadHash(string(payload))}, childOperations)
		return root, err
	})
	if err != nil {
		return nil, err
	}
	view, err := artifactView(updated)
	if err != nil {
		return nil, err
	}
	return &openapi.Artifact{ModelArtifact: view}, nil
}

// applyVersionReplaceFanOut drives a fan-out parent: it completes
// every child, applies each new generation's projection tags, repairs
// alias drift, and publishes the atomic switch. A child that cannot
// complete yet returns its error with the parent retained.
func (s *Service) applyVersionReplaceFanOut(ctx context.Context, claimed Operation) error {
	candidate, err := decodeMutationCandidate(claimed.CandidateJSON)
	if err != nil {
		return err
	}
	if candidate == nil {
		return fmt.Errorf("operation %s does not carry a fan-out candidate: %w",
			claimed.OperationUUID, api.ErrBadRequest)
	}
	// Succeeded children skip their executor on recovery, including upgraded
	// legacy children. Validate every stored parent before replaying any work.
	for _, binding := range candidate.Bindings {
		if binding.Value.NativeModelName == nil {
			return fmt.Errorf("replacement binding has no native parent: %w", api.ErrConflict)
		}
		if err := s.verifyAliasParent(ctx, binding.Value.ModelID, *binding.Value.NativeModelName); err != nil {
			return err
		}
	}

	// Complete every child first; each application is idempotent.
	for _, binding := range candidate.Bindings {
		if err := s.ApplyOperation(ctx, binding.ResultOperationUUID); err != nil {
			return err
		}
	}

	// All children are ready: apply the projection tags and move each
	// adapter-owned alias before the atomic SQL publication.
	for _, binding := range candidate.Bindings {
		child, err := s.store.ResolveAttempt(ctx, binding.ResultOperationUUID)
		if err != nil {
			return err
		}
		if child.State != OperationSucceeded || child.NativeResult == nil {
			return fmt.Errorf("replacement child %s is %s without a native result: %w",
				child.OperationUUID, child.State, api.ErrConflict)
		}
		var result NativeBindingResult
		if err := json.Unmarshal([]byte(*child.NativeResult), &result); err != nil || result.VersionNumber == "" {
			return fmt.Errorf("replacement child %s has no usable native result: %w",
				child.OperationUUID, api.ErrConflict)
		}
		plan, err := s.nativeVersionPlan(ctx, child, claimed)
		if err != nil {
			return err
		}
		if child.ExecutionPlanJSON != nil {
			if err := s.native.UpdateModelVersion(ctx, client.UpdateVersionRequest{Name: result.ModelName, Version: result.VersionNumber, Description: plan.Description}); err != nil {
				return MapClientError(err)
			}
		}
		for _, tag := range nativePlanTags(plan) {
			if err := s.native.SetModelVersionTag(ctx, result.ModelName,
				result.VersionNumber, tag.Key, tag.Value); err != nil {
				return MapClientError(err)
			}
		}
		if err := s.moveAdapterAlias(ctx, binding.Value.VersionID, result, binding.Value.Alias); err != nil {
			return err
		}
	}
	return s.store.PublishOperation(ctx, claimed.OperationUUID, claimed.FencingToken)
}

// applyReplaceChild creates one native generation for a fan-out child
// and completes with its verified result. It never touches SQL
// committed state: the parent's publication owns the switch.
func (s *Service) applyReplaceChild(ctx context.Context, claimed Operation) error {
	plan, err := s.nativeVersionPlan(ctx, claimed, claimed)
	if err != nil {
		return err
	}
	if plan == nil {
		return fmt.Errorf("child %s has no native plan: %w", claimed.OperationUUID, api.ErrConflict)
	}

	native, err := s.dispatchVersionCreate(ctx, claimed, plan)
	if err != nil {
		return err
	}
	native, err = s.awaitReady(ctx, plan.ModelName, native)
	if err != nil {
		return err
	}
	result, err := json.Marshal(NativeBindingResult{
		ModelName: plan.ModelName, VersionNumber: native.Version,
		Source: plan.Source, Generation: claimed.Generation, Status: native.Status,
	})
	if err != nil {
		return fmt.Errorf("serializing native result for operation %s: %w",
			claimed.OperationUUID, err)
	}
	return s.store.CompleteOperation(ctx, claimed.OperationUUID,
		claimed.FencingToken, new(string(result)))
}

// isAdapterOwnedTagKey reports whether a native tag key belongs to the
// adapter's reserved namespace.
func isAdapterOwnedTagKey(key string) bool {
	return len(key) >= len(reservedTagPrefix) && key[:len(reservedTagPrefix)] == reservedTagPrefix
}

// Reject already-known foreign owners before native projection reads. This is
// only an early read-only check: races and same-root semantic request matching
// are always arbitrated again by atomic PrepareOperationGraph.
func (s *Service) preflightFanOutReservations(ctx context.Context, artifactID int32, affected []VersionBinding) error {
	ids := []int32{artifactID}
	for _, binding := range affected {
		ids = append(ids, binding.VersionID)
	}
	var reservations []IntentResource
	if err := s.store.DB().WithContext(ctx).Where("resource_id IN ?", ids).Find(&reservations).Error; err != nil {
		return mapDBError(err)
	}
	for _, reservation := range reservations {
		owner, err := s.store.GetOperation(ctx, reservation.RootOperationUUID)
		if err != nil {
			return err
		}
		if owner.ResourceID != artifactID || owner.Kind != OperationKindVersionReplaceFanOut {
			return fmt.Errorf("resource %d is reserved by foreign graph %s: %w", reservation.ResourceID, reservation.RootOperationUUID, api.ErrConflict)
		}
	}
	return nil
}
