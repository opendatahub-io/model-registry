package mlflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const OperationKindAliasRepair = "alias-repair"

// AliasRepairPlan records the desired committed tuple, without any snapshot
// mutation. Its parent is identity-locked, not reserved for unrelated metadata.
type AliasRepairPlan struct {
	VersionID int32               `json:"versionId"`
	ModelID   int32               `json:"modelId"`
	Alias     string              `json:"alias"`
	Desired   NativeBindingResult `json:"desired"`
}

func verifyAliasRepairDependencies(tx *gorm.DB, plan *AliasRepairPlan, objects map[int32]Object) error {
	version, model := objects[plan.VersionID], objects[plan.ModelID]
	if version.Kind != KindModelVersion || version.ParentID != plan.ModelID || model.Kind != KindRegisteredModel || model.Name != plan.Desired.ModelName || version.Deleted || model.Deleted || version.Visibility != VisibilityCommitted || model.Visibility != VisibilityCommitted {
		return fmt.Errorf("alias repair parent identity changed: %w", api.ErrConflict)
	}
	var binding VersionBinding
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("version_id = ?", plan.VersionID).First(&binding).Error; err != nil {
		return mapDBError(err)
	}
	return verifyAliasRepairBinding(plan, binding)
}

func verifyAliasRepairBinding(plan *AliasRepairPlan, binding VersionBinding) error {
	if binding.VersionID != plan.VersionID || binding.ModelID != plan.ModelID || binding.Phase != PhaseReady || binding.Alias != plan.Alias || binding.ActiveGeneration != plan.Desired.Generation || binding.NativeModelName == nil || *binding.NativeModelName != plan.Desired.ModelName || binding.NativeVersionNumber == nil || *binding.NativeVersionNumber != plan.Desired.VersionNumber {
		return fmt.Errorf("alias repair committed binding changed: %w", api.ErrConflict)
	}
	return nil
}

// PrepareAliasRepair fences the committed version and its binding revisions.
// The empty mutation has no public effects. Normal graph preparation allocates
// a fresh request sequence even if successive repairs see the same revision.
func (s *Store) PrepareAliasRepair(ctx context.Context, versionID int32) (Operation, error) {
	version, err := s.GetObject(ctx, versionID)
	if err != nil {
		return Operation{}, err
	}
	var binding VersionBinding
	if err := s.db.WithContext(ctx).Where("version_id = ?", versionID).First(&binding).Error; err != nil {
		return Operation{}, mapDBError(err)
	}
	if version.Kind != KindModelVersion || version.ParentID != binding.ModelID || binding.Phase != PhaseReady || binding.NativeModelName == nil || binding.NativeVersionNumber == nil || binding.Alias == "" {
		return Operation{}, fmt.Errorf("version %d has no committed adapter alias: %w", versionID, api.ErrConflict)
	}
	model, err := s.GetObject(ctx, version.ParentID)
	if err != nil {
		return Operation{}, err
	}
	if model.Kind != KindRegisteredModel || model.Name != *binding.NativeModelName {
		return Operation{}, fmt.Errorf("alias repair parent identity changed: %w", api.ErrConflict)
	}
	var history VersionHistory
	if err := s.db.WithContext(ctx).Where("version_id = ? AND generation = ? AND native_model_name = ? AND native_version_number = ?", binding.VersionID, binding.ActiveGeneration, *binding.NativeModelName, *binding.NativeVersionNumber).First(&history).Error; err != nil {
		return Operation{}, mapDBError(err)
	}
	candidate := MutationCandidate{CodecVersion: candidateCodecVersion,
		AliasRepair: &AliasRepairPlan{VersionID: versionID, ModelID: model.ID, Alias: binding.Alias,
			Desired: NativeBindingResult{ModelName: history.NativeModelName, VersionNumber: history.NativeVersionNumber, Source: history.Source, Generation: history.Generation, Status: "READY"}},
		ReadRevisions:        map[int32]int64{versionID: version.Revision},
		BindingReadRevisions: map[int32]int64{versionID: binding.Revision}}
	payload, err := json.Marshal(candidate)
	if err != nil {
		return Operation{}, err
	}
	return s.PrepareOperation(ctx, Operation{OperationUUID: uuid.NewString(), ResourceID: versionID,
		Kind: OperationKindAliasRepair, ExpectedRevision: version.Revision, Generation: binding.ActiveGeneration,
		CandidateJSON: string(payload), PayloadHash: payloadHash(string(payload))})
}

// ReconcileAliases compares native aliases with committed bindings. Imports
// have no adapter alias and remain read-only. Each application has a bounded
// native budget; failures retain journal evidence and use the normal backoff.
// No native request occurs inside a SQL transaction.
func (s *Service) ReconcileAliases(ctx context.Context) error {
	var bindings []VersionBinding
	if err := s.store.DB().WithContext(ctx).Where("phase = ? AND alias <> ? AND native_model_name IS NOT NULL AND native_version_number IS NOT NULL", PhaseReady, "").Order("version_id").Find(&bindings).Error; err != nil {
		return mapDBError(err)
	}
	var failures error
	for _, binding := range bindings {
		if ctx.Err() != nil {
			return errors.Join(failures, ctx.Err())
		}
		// Active publication owns even an expired lease until recovery completes.
		// This early check avoids unnecessary reads; preparation arbitrates races.
		var count int64
		if err := s.store.DB().WithContext(ctx).Model(&IntentResource{}).Where("resource_id = ?", binding.VersionID).Count(&count).Error; err != nil {
			return mapDBError(err)
		}
		if count != 0 {
			continue
		}
		err := s.reconcileAlias(ctx, binding)
		if err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

func (s *Service) reconcileAlias(ctx context.Context, binding VersionBinding) error {
	ctx, cancel := context.WithTimeout(ctx, 3*s.config.RequestTimeout)
	defer cancel()
	current, err := s.native.GetRegisteredModelAlias(ctx, *binding.NativeModelName, binding.Alias)
	if err != nil {
		if typed, ok := client.AsError(err); !ok || !typed.IsNotFound() {
			return MapClientError(err)
		}
	} else {
		if err := s.verifyAliasTarget(ctx, binding.VersionID, *binding.NativeModelName, current); err != nil {
			return err
		}
		if matchesNativeBinding(binding, *current) {
			return nil
		}
	}
	operation, err := s.store.PrepareAliasRepair(ctx, binding.VersionID)
	if errors.Is(err, api.ErrConflict) {
		return nil
	} // another graph won preparation
	if err != nil {
		return err
	}
	if err := s.ApplyOperation(ctx, operation.OperationUUID); err != nil {
		if errors.Is(err, api.ErrConflict) {
			current, lookupErr := s.store.GetOperation(ctx, operation.OperationUUID)
			if lookupErr == nil && current.LeaseOwner != nil && *current.LeaseOwner != s.workerID && current.LeaseExpiresSinceEpoch != nil && *current.LeaseExpiresSinceEpoch > nowMillis() {
				return nil
			}
		}
		s.scheduleRetry(ctx, operation.OperationUUID, err)
		return err
	}
	return nil
}

func (s *Service) applyAliasRepair(ctx context.Context, claimed Operation) error {
	ctx, cancel := context.WithTimeout(ctx, 3*s.config.RequestTimeout)
	defer cancel()
	candidate, err := decodeMutationCandidate(claimed.CandidateJSON)
	if err != nil {
		return err
	}
	if candidate == nil || candidate.AliasRepair == nil {
		return fmt.Errorf("alias repair cannot publish snapshots: %w", api.ErrConflict)
	}
	// Re-read after claiming, using the recorded dependency revision rather
	// than the sweep's older observation. Claim and publication both fence it.
	var binding VersionBinding
	if err := s.store.DB().WithContext(ctx).Where("version_id = ?", claimed.ResourceID).First(&binding).Error; err != nil {
		return mapDBError(err)
	}
	revision, recorded := candidate.BindingReadRevisions[claimed.ResourceID]
	if !recorded || revision != binding.Revision || binding.ActiveGeneration != claimed.Generation || binding.NativeModelName == nil || binding.NativeVersionNumber == nil || binding.Alias == "" {
		return fmt.Errorf("alias repair binding changed: %w", api.ErrConflict)
	}
	if err := verifyAliasRepairBinding(candidate.AliasRepair, binding); err != nil {
		return err
	}
	desired := candidate.AliasRepair.Desired
	if err := s.moveAdapterAlias(ctx, binding.VersionID, desired, binding.Alias); err != nil {
		return err
	}
	result, err := json.Marshal(desired)
	if err != nil {
		return err
	}
	if err := s.store.CheckpointOperation(ctx, claimed.OperationUUID, claimed.FencingToken, StepAliasMoved, new(string(result))); err != nil {
		return err
	}
	// Empty candidate publication verifies every dependency and atomically
	// completes the root and releases its reservations; child completion keeps
	// the aggregate's reservations through its existing separate path.
	return s.store.PublishOperation(ctx, claimed.OperationUUID, claimed.FencingToken)
}

// moveAdapterAlias only moves the recorded alias of this logical version.
// A reserved-looking name is not ownership: both endpoints must have recorded
// identities. A native write already in flight may outlive its SQL lease; the
// periodic repair sweep restores any drift that lands after publication.
func (s *Service) moveAdapterAlias(ctx context.Context, versionID int32, desired NativeBindingResult, alias string) error {
	version, err := s.store.GetObject(ctx, versionID)
	if err != nil {
		return err
	}
	var binding VersionBinding
	if err := s.store.DB().WithContext(ctx).Where("version_id = ?", versionID).First(&binding).Error; err != nil {
		return mapDBError(err)
	}
	expectedAlias := binding.Alias
	if expectedAlias == "" {
		expectedAlias = NormalizedAlias(version.Name)
	}
	if version.Kind != KindModelVersion || version.ParentID != binding.ModelID || alias != expectedAlias || alias == "" {
		return fmt.Errorf("alias does not belong to logical version %d: %w", versionID, api.ErrConflict)
	}
	if err := s.verifyAliasParent(ctx, version.ParentID, desired.ModelName); err != nil {
		return err
	}
	native, err := s.native.GetModelVersion(ctx, desired.ModelName, desired.VersionNumber)
	if err != nil {
		return MapClientError(err)
	}
	if native.Name != desired.ModelName || native.Version != desired.VersionNumber || native.Source != desired.Source || (native.Status != "" && native.Status != "READY") {
		return fmt.Errorf("desired alias target differs from recorded native result: %w", api.ErrConflict)
	}
	if err := s.verifyAliasTarget(ctx, versionID, desired.ModelName, native); err != nil {
		return err
	}
	current, err := s.native.GetRegisteredModelAlias(ctx, desired.ModelName, alias)
	if err != nil {
		if typed, ok := client.AsError(err); !ok || !typed.IsNotFound() {
			return MapClientError(err)
		}
	} else {
		if err := s.verifyAliasTarget(ctx, versionID, desired.ModelName, current); err != nil {
			return err
		}
		if current.Version == desired.VersionNumber {
			return nil
		}
	}
	if err := s.native.SetRegisteredModelAlias(ctx, desired.ModelName, alias, desired.VersionNumber); err != nil {
		return MapClientError(err)
	}
	observed, err := s.native.GetRegisteredModelAlias(ctx, desired.ModelName, alias)
	if err != nil {
		return MapClientError(err)
	}
	if observed.Version != desired.VersionNumber {
		return fmt.Errorf("alias changed during its move: %w", api.ErrConflict)
	}
	return s.verifyAliasTarget(ctx, versionID, desired.ModelName, observed)
}

// Imported parents have a committed SQL identity even before they carry adapter
// tags. Present foreign claims are never overwritten by an explicit write.
func (s *Service) verifyAliasParent(ctx context.Context, modelID int32, modelName string) error {
	model, err := s.store.GetObject(ctx, modelID)
	if err != nil {
		return err
	}
	if model.Kind != KindRegisteredModel || model.Name != modelName {
		return fmt.Errorf("native alias parent differs from stored logical parent: %w", api.ErrConflict)
	}
	native, err := s.native.GetRegisteredModel(ctx, modelName)
	if err != nil {
		return MapClientError(err)
	}
	if native.Name != model.Name {
		return fmt.Errorf("native registered-model identity changed: %w", api.ErrConflict)
	}
	adapter, err := s.adapterIdentity(ctx)
	if err != nil {
		return err
	}
	return validateObservedIdentity(nativeTagMap(native.Tags), adapter.AdapterUUID, modelID, nil)
}

// Exact history and active bindings prove established identities even when old
// optional identity tags have been removed. New results need the strict frozen
// work identity, exact checkpoint, and a live graph reservation.
func (s *Service) verifyAliasTarget(ctx context.Context, versionID int32, modelName string, native *client.ModelVersion) error {
	if native == nil || native.Name != modelName || native.Version == "" {
		return fmt.Errorf("alias points outside its registered model: %w", api.ErrConflict)
	}
	adapter, err := s.adapterIdentity(ctx)
	if err != nil {
		return err
	}
	tags := nativeTagMap(native.Tags)
	var history VersionHistory
	err = s.store.DB().WithContext(ctx).Where("version_id = ? AND native_model_name = ? AND native_version_number = ?", versionID, modelName, native.Version).First(&history).Error
	if err == nil {
		if history.Source != native.Source {
			return fmt.Errorf("alias target source differs from recorded history: %w", api.ErrConflict)
		}
		return validateObservedIdentity(tags, adapter.AdapterUUID, versionID, &history.Generation)
	}
	if err != gorm.ErrRecordNotFound {
		return mapDBError(err)
	}
	var binding VersionBinding
	if err := s.store.DB().WithContext(ctx).Where("version_id = ?", versionID).First(&binding).Error; err != nil {
		return mapDBError(err)
	}
	if matchesNativeBinding(binding, *native) {
		return validateObservedIdentity(tags, adapter.AdapterUUID, versionID, &binding.ActiveGeneration)
	}
	if recorded, err := verifyRecordedDisposition(s.store.DB().WithContext(ctx), adapter.AdapterUUID, versionID, native); err != nil || recorded {
		return err
	}
	operationID := tags[tagOperation]
	operation, err := s.store.GetOperation(ctx, operationID)
	if err != nil {
		if !errors.Is(err, api.ErrNotFound) {
			return err
		}
		return fmt.Errorf("alias target has no recorded native identity: %w", api.ErrConflict)
	}
	plan, legacy, err := nativeObservationPlan(operation)
	if err != nil {
		return err
	}
	if plan == nil || plan.LogicalVersionID != versionID {
		return fmt.Errorf("alias target belongs to another logical version: %w", api.ErrConflict)
	}
	if err := verifyNativeWorkIdentity(adapter.AdapterUUID, operation, plan, native, legacy); err != nil {
		return err
	}
	var result NativeBindingResult
	if operation.NativeResult == nil || json.Unmarshal([]byte(*operation.NativeResult), &result) != nil || result.ModelName != modelName || result.VersionNumber != native.Version || result.Source != native.Source || result.Generation != plan.Generation || operation.SupersededByOperationUUID != nil || operation.State == OperationFailed {
		return fmt.Errorf("alias target has no matching native checkpoint: %w", api.ErrConflict)
	}
	root, err := s.store.GetOperation(ctx, graphRoot(operation))
	if err != nil {
		return err
	}
	if (root.State == OperationFailed || root.State == OperationSucceeded) && root.SupersededByOperationUUID == nil {
		return fmt.Errorf("alias target graph is no longer active: %w", api.ErrConflict)
	}
	var reservation IntentResource
	if err := s.store.DB().WithContext(ctx).Where("resource_id = ? AND root_operation_uuid = ?", versionID, root.OperationUUID).First(&reservation).Error; err != nil {
		if err != gorm.ErrRecordNotFound {
			return mapDBError(err)
		}
		return fmt.Errorf("alias target graph does not reserve the logical version: %w", api.ErrConflict)
	}
	return nil
}

func nativeTagMap(tags []client.Tag) map[string]string {
	values := make(map[string]string, len(tags))
	for _, tag := range tags {
		values[tag.Key] = tag.Value
	}
	return values
}
