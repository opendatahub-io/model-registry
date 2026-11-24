package mlflow

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kubeflow/hub/pkg/api"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// candidateResourceIDs returns every resource ID a publication touches
// — the operation's own resource plus all candidate references —
// deduplicated and sorted ascending, so every publication acquires
// resource locks in the same global order.
func candidateResourceIDs(operationResourceID int32, candidate *MutationCandidate) []int32 {
	ids := map[int32]bool{operationResourceID: true}
	add := func(id int32) { ids[id] = true }
	if candidate != nil {
		if candidate.AliasRepair != nil {
			add(candidate.AliasRepair.ModelID)
		}
		for id := range candidate.ReadRevisions {
			add(id)
		}
		for id := range candidate.BindingReadRevisions {
			add(id)
		}
		if candidate.NativeWork != nil {
			add(candidate.NativeWork.LogicalVersionID)
		}
		for _, change := range candidate.Objects {
			add(change.ID)
		}
		for _, link := range candidate.Links {
			add(link.ArtifactID)
			add(link.VersionID)
		}
		for _, binding := range candidate.Bindings {
			add(binding.Value.VersionID)
			add(binding.Value.ModelID)
			if binding.Value.PrimaryArtifactID != nil {
				add(*binding.Value.PrimaryArtifactID)
			}
		}
		for _, history := range candidate.History {
			add(history.Value.VersionID)
		}
	}
	sorted := make([]int32, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	slices.Sort(sorted)
	return sorted
}

// publishMutationCandidate applies a versioned multi-resource
// candidate inside the caller's transaction. Every referenced resource
// row has already been locked in ascending ID order by the caller; an
// error at any step rolls back the whole publication, leaving every
// prior row, revision, timestamp, and visibility unchanged. Operation
// completion is the caller's final step, never a signal sent before
// commit.
func publishMutationCandidate(tx *gorm.DB, operation Operation, candidate *MutationCandidate, objects map[int32]Object) error {
	// Validate relationship kinds against the locked rows, in addition
	// to the database's foreign keys.
	for _, link := range candidate.Links {
		if artifact := objects[link.ArtifactID]; artifact.Kind != KindModelArtifact {
			return fmt.Errorf("artifact link %d/%d references %s %d, not a model artifact: %w",
				link.ArtifactID, link.VersionID, artifact.Kind, link.ArtifactID, api.ErrBadRequest)
		}
		if version := objects[link.VersionID]; version.Kind != KindModelVersion {
			return fmt.Errorf("artifact link %d/%d references %s %d, not a model version: %w",
				link.ArtifactID, link.VersionID, version.Kind, link.VersionID, api.ErrBadRequest)
		}
	}
	for _, binding := range candidate.Bindings {
		if version := objects[binding.Value.VersionID]; version.Kind != KindModelVersion {
			return fmt.Errorf("binding references %s %d, not a model version: %w",
				version.Kind, binding.Value.VersionID, api.ErrBadRequest)
		}
		if model := objects[binding.Value.ModelID]; model.Kind != KindRegisteredModel {
			return fmt.Errorf("binding references %s %d, not a registered model: %w",
				model.Kind, binding.Value.ModelID, api.ErrBadRequest)
		}
		if version := objects[binding.Value.VersionID]; version.ParentID != binding.Value.ModelID {
			return fmt.Errorf("binding version %d does not belong to model %d: %w",
				binding.Value.VersionID, binding.Value.ModelID, api.ErrBadRequest)
		}
	}
	for _, history := range candidate.History {
		if version := objects[history.Value.VersionID]; version.Kind != KindModelVersion {
			return fmt.Errorf("history entry references %s %d, not a model version: %w",
				version.Kind, history.Value.VersionID, api.ErrBadRequest)
		}
	}

	// Resolve native identities from the verified operation graph
	// before writing anything. The publishing operation may reference
	// its own checkpoint: it was written under the fencing token this
	// publication holds, so it is authoritative without being
	// SUCCEEDED yet.
	resolved := make(map[string]NativeBindingResult, len(candidate.Bindings)+len(candidate.History))
	resolve := func(operationUUID string) (NativeBindingResult, error) {
		if operationUUID == "" {
			return NativeBindingResult{}, nil
		}
		if result, ok := resolved[operationUUID]; ok {
			return result, nil
		}
		result, err := loadVerifiedNativeResult(tx, operationUUID, operation.OperationUUID)
		if err != nil {
			return NativeBindingResult{}, err
		}
		resolved[operationUUID] = result
		return result, nil
	}
	for i := range candidate.Bindings {
		binding := &candidate.Bindings[i]
		result, err := resolve(binding.ResultOperationUUID)
		if err != nil {
			return err
		}
		if err := verifyBindingResult(*binding, result); err != nil {
			return err
		}
		// The final native identity comes from the checkpoint, never
		// from the immutable template.
		if result.VersionNumber != "" {
			binding.Value.NativeModelName = new(result.ModelName)
			binding.Value.NativeVersionNumber = new(result.VersionNumber)
		}
	}
	for i := range candidate.History {
		history := &candidate.History[i]
		if history.ResultOperationUUID == "" {
			continue
		}
		result, err := resolve(history.ResultOperationUUID)
		if err != nil {
			return err
		}
		// The immutable template asserts what it knows; the final
		// native identity — the server-generated version number, and
		// any field the template left unset — resolves from the
		// verified result.
		if history.Value.NativeModelName != "" && result.ModelName != history.Value.NativeModelName {
			return fmt.Errorf("history result operation %s names model %q, not %q: %w",
				history.ResultOperationUUID, result.ModelName, history.Value.NativeModelName, api.ErrConflict)
		}
		if history.Value.NativeVersionNumber != "" {
			if result.VersionNumber != history.Value.NativeVersionNumber {
				return fmt.Errorf("history result operation %s names version %q, not %q: %w",
					history.ResultOperationUUID, result.VersionNumber, history.Value.NativeVersionNumber, api.ErrConflict)
			}
		} else if result.VersionNumber != "" {
			history.Value.NativeVersionNumber = result.VersionNumber
		}
		if history.Value.Source != "" && result.Source != "" && result.Source != history.Value.Source {
			return fmt.Errorf("history result operation %s records a different source for version %d: %w",
				history.ResultOperationUUID, history.Value.VersionID, api.ErrConflict)
		}
		if history.Value.Generation != 0 && result.Generation != 0 && result.Generation != history.Value.Generation {
			return fmt.Errorf("history result operation %s records generation %d, not %d: %w",
				history.ResultOperationUUID, result.Generation, history.Value.Generation, api.ErrConflict)
		}
	}

	// Object snapshots: creation times are preserved; update times and
	// revisions advance only on success ( an error rolls back ).
	for _, change := range candidate.Objects {
		// Persist the full stored snapshot so tag-key mappings and
		// owned tags survive; public decoding extracts Resource only.
		snapshotJSON, err := json.Marshal(change.Snapshot)
		if err != nil {
			return fmt.Errorf("serializing snapshot for object %d: %w", change.ID, err)
		}
		externalID, err := snapshotExternalID(change.Snapshot)
		if err != nil {
			return err
		}
		visibility := change.Visibility
		if visibility == "" {
			visibility = VisibilityCommitted
		}
		result := tx.Model(&Object{}).
			Where("id = ? AND revision = ?", change.ID, change.ExpectedRevision).
			Updates(map[string]any{
				"committed_json":               string(snapshotJSON),
				"revision":                     change.ExpectedRevision + 1,
				"external_id":                  externalID,
				"visibility":                   visibility,
				"last_update_time_since_epoch": nowMillis(),
			})
		if result.Error != nil {
			return mapDBError(fmt.Errorf("publishing object %d: %w", change.ID, result.Error))
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("object %d moved past revision %d: %w",
				change.ID, change.ExpectedRevision, api.ErrConflict)
		}
	}

	// A version's hidden revision also guards membership of the artifact
	// projection. Snapshot changes already advance it; otherwise advance once
	// for actual new links without changing public snapshots or timestamps.
	advancedVersions := map[int32]bool{}
	for _, change := range candidate.Objects {
		advancedVersions[change.ID] = true
	}
	// Artifact links: identical links are deduplicated by validation
	// and idempotent against already-committed rows.
	for _, link := range candidate.Links {
		var count int64
		if err := tx.Model(&ArtifactLink{}).
			Where("artifact_id = ? AND version_id = ?", link.ArtifactID, link.VersionID).
			Count(&count).Error; err != nil {
			return mapDBError(err)
		}
		if count > 0 {
			continue
		}
		if err := tx.Create(&link).Error; err != nil {
			return mapDBError(fmt.Errorf("publishing artifact link %d/%d: %w",
				link.ArtifactID, link.VersionID, err))
		}
		if !advancedVersions[link.VersionID] {
			version := objects[link.VersionID]
			result := tx.Model(&Object{}).Where("id = ? AND revision = ?", version.ID, version.Revision).Update("revision", version.Revision+1)
			if result.Error != nil {
				return mapDBError(result.Error)
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("version %d artifact membership changed: %w", version.ID, api.ErrConflict)
			}
			advancedVersions[version.ID] = true
		}
	}

	// Bindings: an absent binding is inserted only with expected
	// revision zero; an existing binding requires the matching revision.
	for _, binding := range candidate.Bindings {
		var existing VersionBinding
		err := tx.Where("version_id = ?", binding.Value.VersionID).First(&existing).Error
		if err == nil {
			if existing.Revision != binding.ExpectedRevision {
				return fmt.Errorf("binding for version %d is at revision %d, not %d: %w",
					binding.Value.VersionID, existing.Revision, binding.ExpectedRevision, api.ErrConflict)
			}
			value := binding.Value
			value.ID = existing.ID
			value.Revision = existing.Revision + 1
			if err := tx.Model(&VersionBinding{}).
				Where("id = ? AND revision = ?", existing.ID, existing.Revision).
				Updates(map[string]any{
					"primary_artifact_id":   value.PrimaryArtifactID,
					"phase":                 value.Phase,
					"native_model_name":     value.NativeModelName,
					"native_version_number": value.NativeVersionNumber,
					"active_generation":     value.ActiveGeneration,
					"desired_generation":    value.DesiredGeneration,
					"alias":                 value.Alias,
					"revision":              value.Revision,
				}).Error; err != nil {
				return mapDBError(fmt.Errorf("updating binding for version %d: %w",
					binding.Value.VersionID, err))
			}
			continue
		}
		if err != gorm.ErrRecordNotFound {
			return mapDBError(err)
		}
		if binding.ExpectedRevision != 0 {
			return fmt.Errorf("binding for version %d does not exist but expects revision %d: %w",
				binding.Value.VersionID, binding.ExpectedRevision, api.ErrConflict)
		}
		value := binding.Value
		value.Revision = 1
		if err := tx.Create(&value).Error; err != nil {
			return mapDBError(fmt.Errorf("inserting binding for version %d: %w",
				binding.Value.VersionID, err))
		}
	}

	// History: new generations are recorded and, when requested,
	// previous generations of the same version are superseded.
	now := nowMillis()
	for _, history := range candidate.History {
		if history.SupersedePrevious {
			if err := tx.Model(&VersionHistory{}).
				Where("version_id = ? AND superseded = ?", history.Value.VersionID, false).
				Updates(map[string]any{
					"superseded":      true,
					"superseded_time": now,
				}).Error; err != nil {
				return mapDBError(fmt.Errorf("superseding history for version %d: %w",
					history.Value.VersionID, err))
			}
		}
		value := history.Value
		if value.ID == 0 {
			if err := tx.Create(&value).Error; err != nil {
				return mapDBError(fmt.Errorf("recording history for version %d: %w",
					history.Value.VersionID, err))
			}
		}
	}
	return nil
}

// loadVerifiedNativeResult loads and verifies the native identity an
// operation checkpointed. The referenced operation must have completed
// and carry a native result — unless it is the operation currently
// publishing, whose checkpoint was written under the fencing token
// this publication holds. A legacy plain-number checkpoint decodes as
// a version number only.
func loadVerifiedNativeResult(tx *gorm.DB, operationUUID, publishingOperationUUID string) (NativeBindingResult, error) {
	operation, err := resolveAttempt(tx, operationUUID)
	if err != nil {
		return NativeBindingResult{}, err
	}
	operationUUID = operation.OperationUUID
	if operation.State != OperationSucceeded && operationUUID != publishingOperationUUID {
		return NativeBindingResult{}, fmt.Errorf("result operation %s is %s, not succeeded: %w",
			operationUUID, operation.State, api.ErrConflict)
	}
	if operation.NativeResult == nil || *operation.NativeResult == "" {
		return NativeBindingResult{}, fmt.Errorf("result operation %s has no native result: %w",
			operationUUID, api.ErrConflict)
	}

	var result NativeBindingResult
	if err := json.Unmarshal([]byte(*operation.NativeResult), &result); err != nil || result.VersionNumber == "" {
		// Legacy plain-number checkpoint.
		result = NativeBindingResult{VersionNumber: *operation.NativeResult}
	}
	if result.Status != "" && result.Status != "READY" {
		return NativeBindingResult{}, fmt.Errorf("result operation %s native version is %q, not READY: %w",
			operationUUID, result.Status, api.ErrConflict)
	}
	return result, nil
}

// verifyBindingResult checks the resolved native identity against the
// binding template's immutable expectations.
func verifyBindingResult(binding BindingChange, result NativeBindingResult) error {
	if binding.ResultOperationUUID == "" {
		return nil
	}
	if result.VersionNumber == "" {
		return fmt.Errorf("result operation %s resolved no native version: %w",
			binding.ResultOperationUUID, api.ErrConflict)
	}
	if binding.Value.NativeModelName != nil && result.ModelName != *binding.Value.NativeModelName {
		return fmt.Errorf("result operation %s names model %q, not %q: %w",
			binding.ResultOperationUUID, result.ModelName, *binding.Value.NativeModelName, api.ErrConflict)
	}
	if binding.Value.NativeVersionNumber != nil && result.VersionNumber != *binding.Value.NativeVersionNumber {
		return fmt.Errorf("result operation %s names version %q, not %q: %w",
			binding.ResultOperationUUID, result.VersionNumber, *binding.Value.NativeVersionNumber, api.ErrConflict)
	}
	if result.Generation != 0 && result.Generation != binding.Value.ActiveGeneration {
		return fmt.Errorf("result operation %s records generation %d, not %d: %w",
			binding.ResultOperationUUID, result.Generation, binding.Value.ActiveGeneration, api.ErrConflict)
	}
	return nil
}

// lockOperationForPublication locks the operation row after every
// resource row, preserving the global lock order.
func lockOperationForPublication(tx *gorm.DB, operationID string) (Operation, error) {
	var operation Operation
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("operation_uuid = ?", operationID).First(&operation).Error
	if err != nil {
		return Operation{}, mapDBError(err)
	}
	return operation, nil
}
