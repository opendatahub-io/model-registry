package mlflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"gorm.io/gorm"
)

const (
	DispositionUnselectedDuplicate = "UNSELECTED_DUPLICATE"
	DispositionOrphanRetained      = "ORPHAN_RETAINED"
	failureCodeNativeSelected      = "OPERATOR_SELECTED_NATIVE"
)

// NativeDisposition retains the operator's decision without deleting native
// data or inventing committed logical metadata. Native names compare exactly.
type NativeDisposition struct {
	NativeModelName        string `gorm:"column:native_model_name;primaryKey"`
	NativeVersionNumber    string `gorm:"column:native_version_number;primaryKey"`
	AdapterUUID            string `gorm:"column:adapter_uuid"`
	VersionID              int32  `gorm:"column:version_id"`
	OperationUUID          string `gorm:"column:operation_uuid"`
	Generation             int64  `gorm:"column:generation"`
	Disposition            string `gorm:"column:disposition"`
	ResolvingOperationUUID string `gorm:"column:resolving_operation_uuid"`
	ResolvedTimeSinceEpoch int64  `gorm:"column:resolved_time_since_epoch"`
}

func (*NativeDisposition) TableName() string { return "MLflowNativeDisposition" }

// ResolveAttempt follows only verified, durable supersession edges. Parent
// candidate references stay immutable, including across multiple retries.
func (s *Store) ResolveAttempt(ctx context.Context, operationID string) (Operation, error) {
	return resolveAttempt(s.db.WithContext(ctx), operationID)
}

func resolveAttempt(db *gorm.DB, operationID string) (Operation, error) {
	seen := map[string]bool{}
	var previous *Operation
	for {
		if seen[operationID] {
			return Operation{}, fmt.Errorf("cyclic attempt chain: %w", api.ErrConflict)
		}
		seen[operationID] = true
		var operation Operation
		if err := db.Where("operation_uuid = ?", operationID).First(&operation).Error; err != nil {
			return Operation{}, mapDBError(err)
		}
		if previous != nil {
			correctKind := operation.Kind == OperationKindVersionCreateRetry
			if previous.Kind == OperationKindVersionReplaceChild {
				correctKind = operation.Kind == OperationKindVersionReplaceChild
			}
			if !correctKind || operation.ParentOperationUUID == nil || *operation.ParentOperationUUID != previous.OperationUUID || graphRoot(operation) != graphRoot(*previous) || operation.ResourceID != previous.ResourceID || operation.ExpectedRevision != previous.ExpectedRevision || operation.Generation != previous.Generation || operation.RequestHash != previous.RequestHash {
				return Operation{}, fmt.Errorf("invalid attempt edge from %s to %s: %w", previous.OperationUUID, operationID, api.ErrConflict)
			}
		}
		if operation.SupersededByOperationUUID == nil {
			return operation, nil
		}
		if operation.State != OperationFailed || operation.FailureCode == nil || *operation.FailureCode != FailureCodeOperatorRetry {
			return Operation{}, fmt.Errorf("attempt %s was not superseded: %w", operationID, api.ErrConflict)
		}
		previous = &operation
		operationID = *operation.SupersededByOperationUUID
	}
}

// operatorNativePlan decodes each executor's typed candidate. A fan-out child
// intentionally has no binding/history writes; those belong to its root.
func operatorNativePlan(operation Operation) (*NativeVersionPlan, bool, error) {
	plan, legacy, err := nativeObservationPlan(operation)
	if err != nil {
		return nil, legacy, err
	}
	if plan == nil || plan.ModelName == "" || plan.Source == "" || plan.LogicalVersionID <= 0 || plan.Generation != operation.Generation {
		return nil, legacy, fmt.Errorf("operation %s has no matching native identity or generation: %w", operation.OperationUUID, api.ErrConflict)
	}
	candidate, err := decodeMutationCandidate(operation.CandidateJSON)
	if err != nil {
		return nil, legacy, err
	}
	if operation.Kind == OperationKindVersionReplaceChild {
		if plan.LogicalVersionID != operation.ResourceID {
			return nil, legacy, fmt.Errorf("child logical identity disagrees: %w", api.ErrConflict)
		}
	} else if candidate == nil || len(candidate.Bindings) != 1 || len(candidate.History) != 1 {
		return nil, legacy, fmt.Errorf("operation has no materialization candidate: %w", api.ErrConflict)
	} else {
		binding, history := candidate.Bindings[0].Value, candidate.History[0].Value
		if binding.VersionID != plan.LogicalVersionID || history.VersionID != plan.LogicalVersionID || binding.NativeModelName == nil || *binding.NativeModelName != plan.ModelName || history.NativeModelName != plan.ModelName || history.Source != plan.Source || binding.ActiveGeneration != plan.Generation || history.Generation != plan.Generation {
			return nil, legacy, fmt.Errorf("operation binding/history generation or identity disagrees: %w", api.ErrConflict)
		}
	}
	return plan, legacy, nil
}

func operatorLeaseAvailable(operation Operation, workerID string) error {
	if operation.LeaseOwner != nil && *operation.LeaseOwner != workerID && operation.LeaseExpiresSinceEpoch != nil && *operation.LeaseExpiresSinceEpoch > nowMillis() {
		return fmt.Errorf("operation %s is leased by %s: %w", operation.OperationUUID, *operation.LeaseOwner, api.ErrConflict)
	}
	return nil
}

// Claim and decision are one transaction. Rejected verification, a foreign
// lease, or any failed insert leaves every SQL row unchanged.
func claimOperatorDecision(tx *gorm.DB, operation Operation, workerID string, lease time.Duration) error {
	if err := operatorLeaseAvailable(operation, workerID); err != nil {
		return err
	}
	return mapDBError(tx.Model(&Operation{}).Where("operation_uuid = ?", operation.OperationUUID).Updates(map[string]any{
		"state": OperationRunning, "lease_owner": workerID, "lease_expires_since_epoch": nowMillis() + lease.Milliseconds(),
		"fencing_token": operation.FencingToken + 1, "attempts": operation.Attempts + 1, "last_update_time_since_epoch": nowMillis(),
	}).Error)
}

func selectedIdentity(operation Operation, modelName, number string) bool {
	var result NativeBindingResult
	return operation.NativeResult != nil && json.Unmarshal([]byte(*operation.NativeResult), &result) == nil && result.ModelName == modelName && result.VersionNumber == number
}

func explicitNativeSelection(operation Operation) (NativeBindingResult, bool) {
	var result NativeBindingResult
	if operation.NativeResult == nil || json.Unmarshal([]byte(*operation.NativeResult), &result) != nil {
		return result, false
	}
	legacyMarker := operation.FailureCode != nil && *operation.FailureCode == failureCodeNativeSelected
	return result, (result.ExplicitSelection || legacyMarker) && result.ModelName != "" && result.VersionNumber != "" && result.Source != "" && result.Generation > 0
}

// Retain the explicit choice under the same fence as every later checkpoint or
// scheduling update. This also promotes the legacy failure-code marker before
// a definitive authorization/input failure can replace that mutable field.
func retainExplicitNativeSelection(operation Operation, checkpoint *string) (*string, error) {
	selected, explicit := explicitNativeSelection(operation)
	if !explicit {
		return checkpoint, nil
	}
	result := selected
	if checkpoint != nil {
		if err := json.Unmarshal([]byte(*checkpoint), &result); err != nil {
			return nil, fmt.Errorf("explicit selection requires a complete native checkpoint: %w", api.ErrConflict)
		}
		if result.ModelName != selected.ModelName || result.VersionNumber != selected.VersionNumber || result.Source != selected.Source || result.Generation != selected.Generation {
			return nil, fmt.Errorf("native checkpoint changes explicit selection: %w", api.ErrConflict)
		}
	}
	result.ExplicitSelection = true
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return new(string(encoded)), nil
}

// BindNativeIdentity selects one verified result even when the registry holds
// other exact matches. All observed alternatives remain native and audited.
func (s *Service) BindNativeIdentity(ctx context.Context, operationID, modelName, versionNumber string) error {
	operation, err := s.store.GetOperation(ctx, operationID)
	if err != nil {
		return err
	}
	if _, selected := explicitNativeSelection(operation); selected && selectedIdentity(operation, modelName, versionNumber) {
		return s.ApplyOperation(ctx, operationID)
	}
	if err := requireResolvableOperation(operation, "bind"); err != nil {
		return err
	}
	if operation.Step == StepPrepared || operation.Step == StepClaimed {
		return fmt.Errorf("operation has not dispatched a native create: %w", api.ErrConflict)
	}
	plan, legacy, err := operatorNativePlan(operation)
	if err != nil {
		return err
	}
	if modelName != plan.ModelName {
		return fmt.Errorf("native model differs from logical parent: %w", api.ErrConflict)
	}
	native, err := s.native.GetModelVersion(ctx, modelName, versionNumber)
	if err != nil {
		if typed, ok := client.AsError(err); ok && typed.IsNotFound() {
			return fmt.Errorf("no native model version %s/%s exists: %w", modelName, versionNumber, api.ErrNotFound)
		}
		return MapClientError(err)
	}
	if native.Version != versionNumber {
		return fmt.Errorf("native identity differs from selection: %w", api.ErrConflict)
	}
	if err := s.verifyNativeVersionIdentity(ctx, operation, plan, native, legacy); err != nil {
		return fmt.Errorf("operation %s for resource %d (logical version %d) cannot bind the selected native identity: %w", operation.OperationUUID, operation.ResourceID, plan.LogicalVersionID, err)
	}
	version, err := s.store.GetObject(ctx, plan.LogicalVersionID)
	if err != nil {
		return err
	}
	if err := s.verifyAliasParent(ctx, version.ParentID, modelName); err != nil {
		return err
	}
	matches, err := s.lookupOwnedVersions(ctx, modelName, operation.OperationUUID)
	if err != nil {
		return err
	}
	// Only exact matches can become retained alternatives. Contradictory owned
	// evidence still conflicts, even when the chosen identity is valid.
	for i := range matches {
		if err := s.verifyNativeVersionIdentity(ctx, operation, plan, &matches[i], legacy); err != nil {
			return err
		}
	}
	adapter, err := s.adapterIdentity(ctx)
	if err != nil {
		return err
	}
	result, err := json.Marshal(NativeBindingResult{ModelName: modelName, VersionNumber: versionNumber, Source: plan.Source, Generation: plan.Generation, Status: native.Status, ExplicitSelection: true})
	if err != nil {
		return err
	}
	err = s.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, _, err := lockOperationGraph(tx, operationID)
		if err != nil {
			return err
		}
		if err := requireResolvableOperation(locked, "bind"); err != nil {
			return err
		}
		if locked.NativeResult != nil && !selectedIdentity(locked, modelName, versionNumber) {
			return fmt.Errorf("operation already checkpointed another native identity: %w", api.ErrConflict)
		}
		var bound int64
		if err := tx.Model(&VersionHistory{}).Where("native_model_name = ? AND native_version_number = ?", modelName, versionNumber).Count(&bound).Error; err != nil {
			return mapDBError(err)
		}
		if bound != 0 {
			return fmt.Errorf("native identity already bound: %w", api.ErrConflict)
		}
		if err := tx.Model(&VersionBinding{}).Where("native_model_name = ? AND native_version_number = ?", modelName, versionNumber).Count(&bound).Error; err != nil {
			return mapDBError(err)
		}
		if bound != 0 {
			return fmt.Errorf("native identity already bound: %w", api.ErrConflict)
		}
		if err := tx.Model(&NativeDisposition{}).Where("native_model_name = ? AND native_version_number = ?", modelName, versionNumber).Count(&bound).Error; err != nil {
			return mapDBError(err)
		}
		if bound != 0 {
			return fmt.Errorf("native identity was retained by another selection: %w", api.ErrConflict)
		}
		if err := claimOperatorDecision(tx, locked, s.workerID, 3*s.config.RequestTimeout); err != nil {
			return err
		}
		for _, alternative := range matches {
			if alternative.Version == versionNumber {
				continue
			}
			if err := recordNativeDisposition(tx, adapter.AdapterUUID, locked, plan, alternative, DispositionUnselectedDuplicate, operationID); err != nil {
				return err
			}
		}
		return mapDBError(tx.Model(&Operation{}).Where("operation_uuid = ? AND fencing_token = ?", operationID, locked.FencingToken+1).Updates(map[string]any{"native_result": string(result), "step": StepNativeCreated, "failure_code": failureCodeNativeSelected}).Error)
	})
	if err != nil {
		return err
	}
	return s.ApplyOperation(ctx, operationID)
}

func recordNativeDisposition(db *gorm.DB, adapter string, operation Operation, plan *NativeVersionPlan, native client.ModelVersion, disposition, resolver string) error {
	var existing NativeDisposition
	err := db.Where("native_model_name = ? AND native_version_number = ?", native.Name, native.Version).First(&existing).Error
	if err == nil {
		if existing.AdapterUUID != adapter || existing.OperationUUID != operation.OperationUUID || existing.VersionID != plan.LogicalVersionID || existing.Generation != plan.Generation || existing.Disposition != disposition || existing.ResolvingOperationUUID != resolver {
			return fmt.Errorf("native disposition disagrees with resolution: %w", api.ErrConflict)
		}
		return nil
	}
	if err != gorm.ErrRecordNotFound {
		return mapDBError(err)
	}
	return mapDBError(db.Create(&NativeDisposition{NativeModelName: native.Name, NativeVersionNumber: native.Version, AdapterUUID: adapter, VersionID: plan.LogicalVersionID, OperationUUID: operation.OperationUUID, Generation: plan.Generation, Disposition: disposition, ResolvingOperationUUID: resolver, ResolvedTimeSinceEpoch: nowMillis()}).Error)
}

// A disposition identifies retained data, not a blanket exception for a reused
// native number. Refresh and alias repair require the same immutable proof.
func verifyRecordedDisposition(db *gorm.DB, adapter string, versionID int32, native *client.ModelVersion) (bool, error) {
	var disposition NativeDisposition
	err := db.Where("native_model_name = ? AND native_version_number = ?", native.Name, native.Version).First(&disposition).Error
	if err == gorm.ErrRecordNotFound {
		return false, nil
	}
	if err != nil {
		return false, mapDBError(err)
	}
	var origin Operation
	if err := db.Where("operation_uuid = ?", disposition.OperationUUID).First(&origin).Error; err != nil {
		return true, mapDBError(err)
	}
	plan, legacy, err := operatorNativePlan(origin)
	if err != nil {
		return true, err
	}
	if disposition.AdapterUUID != adapter || disposition.VersionID != plan.LogicalVersionID || disposition.Generation != plan.Generation || (versionID != 0 && disposition.VersionID != versionID) {
		return true, fmt.Errorf("native disposition identity disagrees: %w", api.ErrConflict)
	}
	var version, model Object
	if err := db.Where("id = ?", plan.LogicalVersionID).First(&version).Error; err != nil {
		return true, mapDBError(err)
	}
	if err := db.Where("id = ?", version.ParentID).First(&model).Error; err != nil {
		return true, mapDBError(err)
	}
	if version.Kind != KindModelVersion || model.Kind != KindRegisteredModel || model.Name != plan.ModelName {
		return true, fmt.Errorf("native disposition parent disagrees: %w", api.ErrConflict)
	}
	return true, verifyNativeWorkIdentity(adapter, origin, plan, native, legacy)
}

// RetryCreate atomically replaces one attempt, keeping its original root and
// every reservation. Children remain children and never publish root metadata.
func (s *Service) RetryCreate(ctx context.Context, operationID string) error {
	operation, err := s.store.GetOperation(ctx, operationID)
	if err != nil {
		return err
	}
	if err := requireResolvableOperation(operation, "retry-create"); err != nil {
		return err
	}
	if operation.Step != StepCreateDispatched || operation.NativeResult != nil {
		return fmt.Errorf("retry-create requires an uncertain dispatched create: %w", api.ErrConflict)
	}
	if _, _, err := operatorNativePlan(operation); err != nil {
		return err
	}
	candidate, err := decodeMutationCandidate(operation.CandidateJSON)
	if err != nil {
		return err
	}
	retryID := uuid.NewString()
	rewritten, err := retryCandidateJSON(candidate, retryID)
	if err != nil {
		return err
	}
	retry := Operation{OperationUUID: retryID, ResourceID: operation.ResourceID, Kind: OperationKindVersionCreateRetry, ExpectedRevision: operation.ExpectedRevision, CandidateJSON: rewritten, PayloadHash: payloadHash(rewritten), Generation: operation.Generation, ParentOperationUUID: new(operationID), AggregateOperationUUID: new(graphRoot(operation)), RequestHash: operation.RequestHash, State: OperationPending, Step: StepPrepared, CreateTimeSinceEpoch: nowMillis(), LastUpdateTimeSinceEpoch: nowMillis()}
	if operation.Kind == OperationKindVersionReplaceChild {
		retry.Kind = operation.Kind
	}
	if operation.ExecutionPlanJSON != nil {
		var plan nativeExecutionPlan
		if err := json.Unmarshal([]byte(*operation.ExecutionPlanJSON), &plan); err != nil {
			return err
		}
		if plan.OwnedTags == nil {
			plan.OwnedTags = map[string]string{}
		}
		plan.OwnedTags[tagOperation] = retryID
		encoded, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		retry.ExecutionPlanJSON = new(string(encoded))
	}
	err = s.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, _, err := lockOperationGraph(tx, operationID)
		if err != nil {
			return err
		}
		if err := requireResolvableOperation(locked, "retry-create"); err != nil {
			return err
		}
		if locked.Step != StepCreateDispatched || locked.NativeResult != nil || locked.CandidateJSON != operation.CandidateJSON {
			return fmt.Errorf("attempt changed before retry: %w", api.ErrConflict)
		}
		if err := operatorLeaseAvailable(locked, s.workerID); err != nil {
			return err
		}
		if err := tx.Model(&Operation{}).Where("operation_uuid = ?", operationID).Updates(map[string]any{"state": OperationFailed, "failure_code": FailureCodeOperatorRetry, "superseded_by_operation_uuid": retryID, "lease_owner": nil, "lease_expires_since_epoch": nil, "fencing_token": locked.FencingToken + 1, "last_update_time_since_epoch": nowMillis()}).Error; err != nil {
			return mapDBError(err)
		}
		var sequence int64
		if err := tx.Model(&Operation{}).Select("COALESCE(MAX(request_sequence), 0)").Where("resource_id = ? AND expected_revision = ? AND kind = ?", retry.ResourceID, retry.ExpectedRevision, retry.Kind).Scan(&sequence).Error; err != nil {
			return mapDBError(err)
		}
		retry.RequestSequence = sequence + 1
		return mapDBError(tx.Create(&retry).Error)
	})
	if err != nil {
		return err
	}
	return s.ApplyOperation(ctx, retry.OperationUUID)
}

// ResolveOrphan abandons only an unreferenced child's unpublished intent.
// Native reads happen before SQL arbitration; no missing aggregate payload is
// reconstructed. A dispatched zero-match result remains auditable uncertainty.
func (s *Service) ResolveOrphan(ctx context.Context, operationID string, retainNative bool) error {
	if !retainNative {
		return fmt.Errorf("resolve-orphan requires --retain-native: %w", api.ErrBadRequest)
	}
	operation, err := s.store.GetOperation(ctx, operationID)
	if err != nil {
		return err
	}
	if operation.Kind != OperationKindVersionReplaceChild {
		return fmt.Errorf("operation is not an orphan child: %w", api.ErrConflict)
	}
	if err := ensureUnreferencedOrphan(s.store.db.WithContext(ctx), operation); err != nil {
		return err
	}
	adapter, err := s.adapterIdentity(ctx)
	if err != nil {
		return err
	}
	var plan *NativeVersionPlan
	var matches []client.ModelVersion
	if operation.Step != StepPrepared && operation.Step != StepClaimed {
		var legacy bool
		plan, legacy, err = operatorNativePlan(operation)
		if err != nil {
			return err
		}
		matches, err = s.lookupOwnedVersions(ctx, plan.ModelName, operation.OperationUUID)
		if err != nil {
			return err
		}
		for i := range matches {
			if err := verifyNativeWorkIdentity(adapter.AdapterUUID, operation, plan, &matches[i], legacy); err != nil {
				return err
			}
		}
	}
	return s.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := lockResource(tx, operation.ResourceID); err != nil {
			return err
		}
		locked, err := lockOperationForPublication(tx, operationID)
		if err != nil {
			return err
		}
		if err := ensureUnreferencedOrphan(tx, locked); err != nil {
			return err
		}
		if err := operatorLeaseAvailable(locked, s.workerID); err != nil {
			return err
		}
		if locked.SupersededByOperationUUID != nil {
			return fmt.Errorf("orphan has a selected retry: %w", api.ErrConflict)
		}
		if locked.Step != operation.Step || locked.CandidateJSON != operation.CandidateJSON || locked.NativeResult != nil && operation.NativeResult == nil {
			return fmt.Errorf("orphan changed during resolution: %w", api.ErrConflict)
		}
		if locked.State == OperationFailed && (locked.FailureCode == nil || (*locked.FailureCode != DispositionOrphanRetained && *locked.FailureCode != "LEGACY_ORPHAN_BEFORE_DISPATCH")) {
			return fmt.Errorf("operation is already terminal: %w", api.ErrConflict)
		}
		for _, native := range matches {
			if err := recordNativeDisposition(tx, adapter.AdapterUUID, locked, plan, native, DispositionOrphanRetained, operationID); err != nil {
				return err
			}
		}
		if err := tx.Model(&Operation{}).Where("operation_uuid = ?", operationID).Updates(map[string]any{"state": OperationFailed, "failure_code": DispositionOrphanRetained, "lease_owner": nil, "lease_expires_since_epoch": nil, "fencing_token": locked.FencingToken + 1, "last_update_time_since_epoch": nowMillis()}).Error; err != nil {
			return mapDBError(err)
		}
		return mapDBError(tx.Where("root_operation_uuid = ? AND resource_id = ? AND expected_revision = ?", operationID, operation.ResourceID, operation.ExpectedRevision).Delete(&IntentResource{}).Error)
	})
}

func ensureUnreferencedOrphan(db *gorm.DB, operation Operation) error {
	var operations []Operation
	if err := db.Find(&operations).Error; err != nil {
		return mapDBError(err)
	}
	for _, root := range operations {
		if root.OperationUUID == operation.OperationUUID {
			continue
		}
		if operation.AggregateOperationUUID != nil && root.OperationUUID == *operation.AggregateOperationUUID {
			return fmt.Errorf("operation belongs to a recorded aggregate: %w", api.ErrConflict)
		}
		candidate, err := decodeMutationCandidate(root.CandidateJSON)
		if err != nil {
			return err
		}
		if candidate == nil {
			continue
		}
		for _, binding := range candidate.Bindings {
			if binding.ResultOperationUUID == operation.OperationUUID {
				return fmt.Errorf("operation is referenced by an aggregate: %w", api.ErrConflict)
			}
		}
		for _, history := range candidate.History {
			if history.ResultOperationUUID == operation.OperationUUID {
				return fmt.Errorf("operation is referenced by an aggregate: %w", api.ErrConflict)
			}
		}
	}
	return nil
}
