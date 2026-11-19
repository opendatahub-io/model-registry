package mlflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"

	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const tagGeneration = "rhoai.generation"

// buildNativeVersionPlan consumes explicit SQL snapshots, never a mutable
// execution-time view. The returned revisions fence both metadata and artifact
// membership (new links advance the logical version's hidden revision).
// legacyAdditional selects the pre-v4 additional-artifact projection when
// reconstructing recorded legacy work, so replay stays verbatim.
func (s *Service) buildNativeVersionPlan(ctx context.Context, version Object, primary Object,
	additional []Object, binding VersionBinding, operationUUID string, generation int64,
	legacyAdditional bool) (NativeVersionPlan, map[int32]int64, error) {
	plan := NativeVersionPlan{LogicalVersionID: version.ID, Generation: generation, Alias: binding.Alias, RetainedTags: map[string]string{}}
	revisions := map[int32]int64{version.ID: version.Revision, primary.ID: primary.Revision}
	logical, err := versionView(version)
	if err != nil {
		return plan, nil, err
	}
	artifact, err := artifactView(primary)
	if err != nil {
		return plan, nil, err
	}
	model, err := s.store.GetObject(ctx, version.ParentID)
	if err != nil {
		return plan, nil, err
	}
	if model.Kind != KindRegisteredModel || (binding.VersionID != 0 && binding.VersionID != version.ID) || (binding.ModelID != 0 && binding.ModelID != model.ID) {
		return plan, nil, fmt.Errorf("version %d has inconsistent parent or binding: %w", version.ID, api.ErrConflict)
	}
	plan.ModelName = model.Name
	plan.Source = derefString(artifact.Uri)
	plan.Description = derefString(logical.Description)
	if plan.Alias == "" {
		plan.Alias = NormalizedAlias(version.Name)
	}
	plan.OwnedTags, err = EncodeVersion(logical)
	if err != nil {
		return plan, nil, err
	}
	primaryTags, err := EncodeArtifact(artifact, true)
	if err != nil {
		return plan, nil, err
	}
	maps.Copy(plan.OwnedTags, primaryTags)
	for _, object := range additional {
		if object.ID == primary.ID {
			continue
		}
		view, err := artifactView(object)
		if err != nil {
			return plan, nil, err
		}
		if _, err := EncodeArtifact(view, false); err != nil {
			return plan, nil, err
		}
		if legacyAdditional {
			maps.Copy(plan.OwnedTags, legacyQualifiedArtifactTags(object.ID, view))
		} else {
			maps.Copy(plan.OwnedTags, qualifiedArtifactTags(object.ID, view))
		}
		revisions[object.ID] = object.Revision
	}
	adapter, err := s.adapterIdentity(ctx)
	if err != nil {
		return plan, nil, err
	}
	plan.OwnedTags[tagAdapterUUID] = adapter.AdapterUUID
	plan.OwnedTags[tagLogicalID] = strconv.Itoa(int(version.ID))
	plan.OwnedTags[tagOperation] = operationUUID
	plan.OwnedTags[tagGeneration] = strconv.FormatInt(generation, 10)
	if binding.NativeModelName != nil && binding.NativeVersionNumber != nil {
		prior, err := s.native.GetModelVersion(ctx, *binding.NativeModelName, *binding.NativeVersionNumber)
		if err != nil {
			return plan, nil, MapClientError(err)
		}
		for _, tag := range prior.Tags {
			if _, owned := plan.OwnedTags[tag.Key]; !owned && !isAdapterOwnedTagKey(tag.Key) {
				plan.RetainedTags[tag.Key] = tag.Value
			}
		}
	}
	return plan, revisions, nil
}

func (s *Service) additionalArtifactSnapshots(ctx context.Context, versionID, primaryID int32) ([]Object, error) {
	var links []ArtifactLink
	if err := s.store.DB().WithContext(ctx).Where("version_id = ? AND artifact_id <> ?", versionID, primaryID).Order("artifact_id").Find(&links).Error; err != nil {
		return nil, mapDBError(err)
	}
	objects := make([]Object, 0, len(links))
	for _, link := range links {
		object, err := s.store.GetObject(ctx, link.ArtifactID)
		if err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, nil
}

// Freeze the desired primary snapshot with its original expected revision.
func projectionPrimary(object Object, change ObjectChange) Object {
	object.CommittedJSON = string(mustMarshal(change.Snapshot))
	return object
}

func nativePlanTags(plan *NativeVersionPlan) []client.Tag {
	tags := maps.Clone(plan.RetainedTags)
	if tags == nil {
		tags = map[string]string{}
	}
	maps.Copy(tags, plan.OwnedTags)
	return tagSlice(tags)
}

// Legacy candidates remain immutable. This separate plan records the committed
// SQL projection selected at upgrade, and its complete dependency reservations.
// Earlier graph hydration wrote only source/identity; the marker distinguishes
// that incomplete work order from a fully frozen projection.
type nativeExecutionPlan struct {
	NativeVersionPlan
	ProjectionFrozen     bool            `json:"projectionFrozen"`
	ReadRevisions        map[int32]int64 `json:"readRevisions"`
	BindingReadRevisions map[int32]int64 `json:"bindingReadRevisions"`
}

func (s *Service) nativeVersionPlan(ctx context.Context, operation, authority Operation) (*NativeVersionPlan, error) {
	candidate, err := decodeMutationCandidate(operation.CandidateJSON)
	if err != nil {
		return nil, err
	}
	if candidate != nil && candidate.NativeWork != nil {
		return candidate.NativeWork, s.verifyNativePlanResult(ctx, operation, candidate.NativeWork, false)
	}
	if operation.ExecutionPlanJSON != nil {
		var recorded nativeExecutionPlan
		if err := json.Unmarshal([]byte(*operation.ExecutionPlanJSON), &recorded); err != nil {
			return nil, err
		}
		if recorded.ProjectionFrozen {
			err := s.verifyNativePlanResult(ctx, operation, &recorded.NativeVersionPlan, true)
			if errors.Is(err, api.ErrConflict) || errors.Is(err, api.ErrBadRequest) || errors.Is(err, api.ErrNotFound) {
				if quarantineErr := s.quarantineLegacyProjection(ctx, authority); quarantineErr != nil {
					return nil, quarantineErr
				}
			}
			return &recorded.NativeVersionPlan, err
		}
	}
	plan, err := s.freezeLegacyNativePlan(ctx, operation, authority, candidate)
	if errors.Is(err, api.ErrConflict) || errors.Is(err, api.ErrBadRequest) || errors.Is(err, api.ErrNotFound) {
		if quarantineErr := s.quarantineLegacyProjection(ctx, authority); quarantineErr != nil {
			return nil, quarantineErr
		}
	}
	return plan, err
}

func (s *Service) freezeLegacyNativePlan(ctx context.Context, operation, authority Operation, candidate *MutationCandidate) (*NativeVersionPlan, error) {
	if candidate == nil {
		return nil, fmt.Errorf("operation %s has no verifiable legacy projection: %w", operation.OperationUUID, api.ErrConflict)
	}
	rootCandidate := candidate
	versionID := int32(0)
	source, modelName := "", ""
	generation := operation.Generation
	if operation.Kind == OperationKindVersionReplaceChild {
		recorded, err := legacyNativePlan(operation)
		if err != nil {
			return nil, err
		}
		if recorded == nil || operation.AggregateOperationUUID == nil {
			return nil, fmt.Errorf("legacy child has no recorded aggregate: %w", api.ErrConflict)
		}
		versionID, source, modelName = recorded.LogicalVersionID, recorded.Source, recorded.ModelName
		parent, err := s.store.GetOperation(ctx, graphRoot(operation))
		if err != nil {
			return nil, err
		}
		rootCandidate, err = decodeMutationCandidate(parent.CandidateJSON)
		if err != nil {
			return nil, err
		}
	} else if len(candidate.Bindings) == 1 && len(candidate.History) == 1 {
		versionID = candidate.Bindings[0].Value.VersionID
		history := candidate.History[0].Value
		if history.VersionID != versionID || history.Generation != generation {
			return nil, fmt.Errorf("legacy binding/history mismatch: %w", api.ErrConflict)
		}
		source, modelName = history.Source, history.NativeModelName
	}
	if rootCandidate == nil || versionID == 0 {
		return nil, fmt.Errorf("legacy native identity cannot be verified: %w", api.ErrConflict)
	}
	var desired *BindingChange
	for i := range rootCandidate.Bindings {
		change := &rootCandidate.Bindings[i]
		if change.Value.VersionID == versionID && change.ResultOperationUUID != "" {
			selected, err := s.store.ResolveAttempt(ctx, change.ResultOperationUUID)
			if err != nil {
				return nil, err
			}
			if selected.OperationUUID == operation.OperationUUID {
				desired = change
				break
			}
		}
	}
	if desired == nil || desired.Value.PrimaryArtifactID == nil || desired.Value.NativeModelName == nil || *desired.Value.NativeModelName != modelName || desired.Value.ActiveGeneration != generation {
		return nil, fmt.Errorf("legacy projection has no matching recorded binding: %w", api.ErrConflict)
	}
	var primaryChange *ObjectChange
	for i := range rootCandidate.Objects {
		if rootCandidate.Objects[i].ID == *desired.Value.PrimaryArtifactID {
			primaryChange = &rootCandidate.Objects[i]
			break
		}
	}
	if primaryChange == nil || primaryChange.Snapshot.Kind != KindModelArtifact {
		return nil, fmt.Errorf("legacy primary snapshot missing: %w", api.ErrConflict)
	}
	version, err := s.store.GetObject(ctx, versionID)
	if err != nil {
		return nil, err
	}
	if version.ParentID != desired.Value.ModelID {
		return nil, fmt.Errorf("legacy version parent changed: %w", api.ErrConflict)
	}
	primary, err := s.loadObjectRow(ctx, primaryChange.ID)
	if err != nil {
		return nil, err
	}
	if primary.Revision != primaryChange.ExpectedRevision {
		return nil, fmt.Errorf("legacy primary revision changed: %w", api.ErrConflict)
	}
	primary = projectionPrimary(primary, *primaryChange)
	artifact, err := artifactView(primary)
	if err != nil {
		return nil, err
	}
	if derefString(artifact.Uri) != source {
		return nil, fmt.Errorf("legacy primary source differs from work order: %w", api.ErrConflict)
	}
	var binding VersionBinding
	if err := s.store.DB().WithContext(ctx).Where("version_id = ?", versionID).First(&binding).Error; err != nil {
		return nil, mapDBError(err)
	}
	if binding.Revision != desired.ExpectedRevision {
		return nil, fmt.Errorf("legacy binding revision changed: %w", api.ErrConflict)
	}
	if err := s.verifyNativePlanResult(ctx, operation, &NativeVersionPlan{ModelName: modelName, Source: source, LogicalVersionID: versionID, Generation: generation}, true); err != nil {
		return nil, err
	}
	additional, err := s.additionalArtifactSnapshots(ctx, version.ID, primary.ID)
	if err != nil {
		return nil, err
	}
	plan, revisions, err := s.buildNativeVersionPlan(ctx, version, primary, additional, binding, operation.OperationUUID, generation, true)
	if err != nil {
		return nil, err
	}
	if plan.ModelName != modelName {
		return nil, fmt.Errorf("legacy model identity changed: %w", api.ErrConflict)
	}
	frozen := nativeExecutionPlan{NativeVersionPlan: plan, ProjectionFrozen: true, ReadRevisions: revisions, BindingReadRevisions: map[int32]int64{versionID: binding.Revision}}
	if err := s.store.freezeExecutionPlan(ctx, operation, authority, frozen); err != nil {
		return nil, err
	}
	return &plan, nil
}

// Native reads above happen outside this transaction. Lock the union of old and
// new dependencies in global order, verify the lease, then atomically reserve
// the consumed snapshots and persist the plan without touching the work order.
func (s *Store) freezeExecutionPlan(ctx context.Context, operation, authority Operation, plan nativeExecutionPlan) error {
	encoded, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		rootID := graphRoot(operation)
		var nodes []Operation
		if err := tx.Where("operation_uuid = ? OR aggregate_operation_uuid = ?", rootID, rootID).Find(&nodes).Error; err != nil {
			return mapDBError(err)
		}
		for i := range nodes {
			if nodes[i].OperationUUID == operation.OperationUUID {
				nodes[i].ExecutionPlanJSON = new(string(encoded))
			}
		}
		deps, err := operationDependencies(nodes)
		if err != nil {
			return err
		}
		objects, err := lockGraphObjects(tx, deps)
		if err != nil {
			return err
		}
		locked, _, err := lockOperationGraph(tx, authority.OperationUUID)
		if err != nil {
			return err
		}
		if locked.State != OperationRunning || locked.FencingToken != authority.FencingToken || locked.LeaseExpiresSinceEpoch == nil || *locked.LeaseExpiresSinceEpoch <= nowMillis() {
			return fmt.Errorf("legacy projection lease expired: %w", api.ErrConflict)
		}
		for _, id := range sortedResourceIDs(deps.reserve) {
			var reservation IntentResource
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("resource_id = ?", id).First(&reservation).Error
			if err == gorm.ErrRecordNotFound {
				if err := tx.Create(&IntentResource{ResourceID: id, RootOperationUUID: rootID, ExpectedRevision: objects[id].Revision}).Error; err != nil {
					return mapDBError(err)
				}
			} else if err != nil {
				return mapDBError(err)
			} else if reservation.RootOperationUUID != rootID || reservation.ExpectedRevision != objects[id].Revision {
				return fmt.Errorf("legacy projection dependency %d is owned elsewhere: %w", id, api.ErrConflict)
			}
		}
		current, err := lockOperationForPublication(tx, operation.OperationUUID)
		if err != nil {
			return err
		}
		if current.ExecutionPlanJSON != nil {
			var existing nativeExecutionPlan
			if err := json.Unmarshal([]byte(*current.ExecutionPlanJSON), &existing); err != nil {
				return err
			}
			if existing.ProjectionFrozen && *current.ExecutionPlanJSON != string(encoded) {
				return fmt.Errorf("legacy projection already frozen differently: %w", api.ErrConflict)
			}
		}
		return mapDBError(tx.Model(&Operation{}).Where("operation_uuid = ?", operation.OperationUUID).Update("execution_plan_json", string(encoded)).Error)
	})
}

func (s *Service) quarantineLegacyProjection(ctx context.Context, authority Operation) error {
	return s.store.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		root, err := lockOperationForPublication(tx, graphRoot(authority))
		if err != nil {
			return err
		}
		locked := root
		if authority.OperationUUID != root.OperationUUID {
			locked, err = lockOperationForPublication(tx, authority.OperationUUID)
			if err != nil {
				return err
			}
		}
		if locked.State != OperationRunning || locked.FencingToken != authority.FencingToken || locked.LeaseExpiresSinceEpoch == nil || *locked.LeaseExpiresSinceEpoch <= nowMillis() {
			return fmt.Errorf("legacy projection lease lost: %w", api.ErrConflict)
		}
		return mapDBError(tx.Model(&Operation{}).Where("operation_uuid = ?", root.OperationUUID).Update("failure_code", "LEGACY_GRAPH_QUARANTINED").Error)
	})
}

// verifyNativePlanResult checks a possibly dispatched or checkpointed result
// before any projection can overwrite contradictory native identity evidence.
func (s *Service) verifyNativePlanResult(ctx context.Context, operation Operation, plan *NativeVersionPlan, legacy bool) error {
	if operation.NativeResult == nil && (operation.Step == StepPrepared || operation.Step == StepClaimed) {
		return nil
	}
	var native *client.ModelVersion
	var err error
	if operation.NativeResult != nil {
		var result NativeBindingResult
		if err := json.Unmarshal([]byte(*operation.NativeResult), &result); err != nil {
			return err
		}
		if result.ModelName != plan.ModelName || result.VersionNumber == "" || (result.Source != "" && result.Source != plan.Source) || (result.Generation != plan.Generation && (!legacy || result.Generation != 0)) {
			return fmt.Errorf("native result mismatches frozen work order: %w", api.ErrConflict)
		}
		native, err = s.native.GetModelVersion(ctx, result.ModelName, result.VersionNumber)
		if err != nil {
			return MapClientError(err)
		}
		if native.Version != result.VersionNumber {
			return fmt.Errorf("native result version differs from checkpoint: %w", api.ErrConflict)
		}
	} else {
		native, err = s.reconcileDispatchedCreate(ctx, operation, plan.ModelName)
		if err != nil {
			return err
		}
	}
	return s.verifyNativeVersionIdentity(ctx, operation, plan, native, legacy)
}

// The exact frozen identity gate is shared by single and fan-out recovery.
// Codec-2 work proved its binding/history when its upgrade plan was frozen;
// only that legacy path may accept an absent generation tag.
func (s *Service) verifyNativeVersionIdentity(ctx context.Context, operation Operation, plan *NativeVersionPlan, native *client.ModelVersion, legacy bool) error {
	adapter, err := s.adapterIdentity(ctx)
	if err != nil {
		return err
	}
	return verifyNativeWorkIdentity(adapter.AdapterUUID, operation, plan, native, legacy)
}

// These pure checks also arbitrate observations inside the SQL publication
// transaction. They perform no native reads or database writes.
func verifyNativeWorkFields(plan *NativeVersionPlan, native *client.ModelVersion) error {
	if native.Name != plan.ModelName || native.Version == "" || native.Source != plan.Source {
		return fmt.Errorf("native model, version, or source differs from frozen work order: %w", api.ErrConflict)
	}
	return nil
}

func verifyNativeWorkIdentity(adapterUUID string, operation Operation, plan *NativeVersionPlan, native *client.ModelVersion, legacy bool) error {
	if err := verifyNativeWorkFields(plan, native); err != nil {
		return err
	}
	tags := map[string]string{}
	for _, tag := range native.Tags {
		tags[tag.Key] = tag.Value
	}
	generation := strconv.FormatInt(plan.Generation, 10)
	if tags[tagAdapterUUID] != adapterUUID || tags[tagLogicalID] != strconv.Itoa(int(plan.LogicalVersionID)) || tags[tagOperation] != operation.OperationUUID || (tags[tagGeneration] != generation && (!legacy || tags[tagGeneration] != "")) {
		return fmt.Errorf("native ownership or identity differs from frozen work order: %w", api.ErrConflict)
	}
	return nil
}
