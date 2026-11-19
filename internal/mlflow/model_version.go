package mlflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/google/uuid"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"gorm.io/gorm"
)

// UpsertModelVersion creates a pending logical model version, or
// updates the metadata of an existing one. A new version is committed
// together with its PENDING_SOURCE binding in one SQL transaction
// through the typed-candidate publisher: no placeholder native source
// is supplied and no native version-create request is issued. Pending
// metadata edits stay in SQL; native synchronization begins when the
// version is materialized.
func (s *Service) UpsertModelVersion(modelVersion *openapi.ModelVersion,
	registeredModelId *string) (*openapi.ModelVersion, error) {
	ctx := s.ctx

	// An update identifies the resource by id: the version's stored
	// parent is authoritative and any supplied registered model id is
	// advisory, matching the platform contract.
	if modelVersion.Id != nil {
		id, err := parseResourceID(*modelVersion.Id)
		if err != nil {
			return nil, fmt.Errorf("model version id %q is not a valid id: %w",
				*modelVersion.Id, api.ErrBadRequest)
		}
		existing, err := s.store.GetObject(ctx, id)
		if err != nil {
			return nil, err
		}
		if existing.Kind != KindModelVersion {
			return nil, fmt.Errorf("object %d is a %s, not a model version: %w",
				existing.ID, existing.Kind, api.ErrBadRequest)
		}
		parent, err := s.store.GetObject(ctx, existing.ParentID)
		if err != nil {
			return nil, err
		}
		// An absent name keeps the stored one.
		if modelVersion.Name == "" {
			updated := *modelVersion
			updated.Name = existing.Name
			modelVersion = &updated
		}
		if _, err := EncodeVersion(modelVersion); err != nil {
			return nil, err
		}
		return s.updateModelVersion(ctx, parent, existing, modelVersion)
	}

	if registeredModelId == nil || *registeredModelId == "" {
		return nil, fmt.Errorf("a registered model id is required: %w", api.ErrBadRequest)
	}
	parentID, err := parseResourceID(*registeredModelId)
	if err != nil {
		return nil, fmt.Errorf("registered model id %q is not a valid id: %w",
			*registeredModelId, api.ErrBadRequest)
	}
	parent, err := s.store.GetObject(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if parent.Kind != KindRegisteredModel {
		return nil, fmt.Errorf("object %d is a %s, not a registered model: %w",
			parent.ID, parent.Kind, api.ErrBadRequest)
	}

	name := modelVersion.Name
	existing, err := s.store.FindObject(ctx, KindModelVersion, &name, &parentID, nil)
	if err == nil {
		return nil, fmt.Errorf("model version %q already exists under model %d as id %d: %w",
			name, parent.ID, existing.ID, api.ErrConflict)
	}
	if err != nil && !errors.Is(err, api.ErrNotFound) {
		return nil, err
	}
	if _, err := EncodeVersion(modelVersion); err != nil {
		return nil, err
	}
	return s.createPendingVersion(ctx, parent, modelVersion)
}

// createPendingVersion commits the logical version and its
// PENDING_SOURCE binding atomically, with no native interaction.
func (s *Service) createPendingVersion(ctx context.Context, parent Object,
	version *openapi.ModelVersion) (*openapi.ModelVersion, error) {
	reserved, err := s.reserveResource(ctx, Object{
		Kind: KindModelVersion, ParentID: parent.ID, Name: version.Name,
		ExternalID: version.ExternalId,
	})
	if err != nil {
		return nil, err
	}

	tags, err := EncodeVersion(version)
	if err != nil {
		return nil, err
	}
	resource, err := marshalResource(versionResource(version))
	if err != nil {
		return nil, err
	}
	candidate := MutationCandidate{
		CodecVersion: candidateCodecVersion,
		Objects: []ObjectChange{{
			ID: reserved.ID, ExpectedRevision: reserved.Revision,
			Snapshot: StoredSnapshot{
				CodecVersion: candidateCodecVersion, Kind: KindModelVersion,
				Resource: resource, TagKeys: customTagKeys(version.CustomProperties),
				OwnedTags: tags,
			},
		}},
		Bindings: []BindingChange{{
			// A pending version binds to its parent with no native
			// identity and no result operation.
			Value: VersionBinding{
				VersionID: reserved.ID, ModelID: parent.ID,
				Phase: PhasePendingSource,
				Alias: NormalizedAlias(version.Name),
			},
		}},
	}
	published, err := s.driveCreate(ctx, reserved, func() (Operation, error) {
		return prepareCandidate(ctx, s.store, uuid.NewString(), reserved.ID,
			OperationKindVersionCreate, reserved.Revision, 1, candidate)
	})
	if err != nil {
		return nil, err
	}
	return versionView(published)
}

// updateModelVersion publishes a pending version's metadata edit in
// SQL. A bound version's description and tag edits synchronize native
// metadata without creating a new generation once materialization
// exists; a pending version's edits never touch the native registry.
func (s *Service) updateModelVersion(ctx context.Context, parent, existing Object,
	version *openapi.ModelVersion) (*openapi.ModelVersion, error) {
	if existing.Kind != KindModelVersion {
		return nil, fmt.Errorf("object %d is a %s, not a model version: %w",
			existing.ID, existing.Kind, api.ErrBadRequest)
	}
	if existing.ParentID != parent.ID {
		return nil, fmt.Errorf("model version %d belongs to model %d, not %d: %w",
			existing.ID, existing.ParentID, parent.ID, api.ErrBadRequest)
	}
	// An update is partial: fields the request omits keep their prior
	// committed values, and the name is a protected identity that never
	// changes.
	previous, err := decodeStoredSnapshot(existing.CommittedJSON)
	if err != nil {
		return nil, err
	}
	var prior openapi.ModelVersion
	if len(previous.Resource) > 0 {
		if err := decodeInto(previous.Resource, &prior); err != nil {
			return nil, err
		}
	}
	prior.Name = existing.Name
	version = mergeModelVersionUpdate(&prior, version)

	tags, err := EncodeVersion(version)
	if err != nil {
		return nil, err
	}
	owned := carryCreationTags(tags, previous.OwnedTags)

	resource, err := marshalResource(versionResource(version))
	if err != nil {
		return nil, err
	}
	candidate := MutationCandidate{
		CodecVersion: candidateCodecVersion,
		Objects: []ObjectChange{{
			ID: existing.ID, ExpectedRevision: existing.Revision,
			Snapshot: StoredSnapshot{
				CodecVersion: candidateCodecVersion, Kind: KindModelVersion,
				Resource: resource, TagKeys: customTagKeys(version.CustomProperties),
				OwnedTags: owned,
			},
		}},
	}

	// A bound version's description and tag edits synchronize native
	// metadata without creating a new generation ( see applyVersionUpdate );
	// a pending version's edits never touch the native registry.
	operation, err := prepareCandidate(ctx, s.store, uuid.NewString(), existing.ID,
		OperationKindVersionUpdate, existing.Revision, 1, candidate)
	if err != nil {
		return nil, err
	}
	if err := s.ApplyOperation(ctx, operation.OperationUUID); err != nil {
		return nil, err
	}
	updated, err := s.store.GetObject(ctx, existing.ID)
	if err != nil {
		return nil, err
	}
	return versionView(updated)
}

// mergeModelVersionUpdate overlays one partial update onto the prior
// committed resource. Nil pointer fields and absent maps keep their
// prior values; a supplied map replaces the prior map wholesale.
func mergeModelVersionUpdate(prior, update *openapi.ModelVersion) *openapi.ModelVersion {
	merged := *prior
	merged.Name = prior.Name
	if update.Description != nil {
		merged.Description = update.Description
	}
	if update.ExternalId != nil {
		merged.ExternalId = update.ExternalId
	}
	if update.State != nil {
		merged.State = update.State
	}
	if update.Author != nil {
		merged.Author = update.Author
	}
	if update.CustomProperties != nil {
		merged.CustomProperties = update.CustomProperties
	}
	return &merged
}

// nativeDeletionOutcome reports how a direct-read native deletion
// observation was arbitrated against committed SQL state.
type nativeDeletionOutcome int

const (
	// deletionPublished means the tombstone was committed under the
	// observation's revision fence.
	deletionPublished nativeDeletionOutcome = iota
	// deletionDeferred means the observation lost arbitration — the
	// object or binding changed, or a reservation is active — and no
	// committed state was revised.
	deletionDeferred
	// deletionAlreadyDeleted means the row is already tombstoned.
	deletionAlreadyDeleted
)

// GetModelVersionById retrieves a model version by its REST id. A bound
// version also checks its native counterpart exists: a confirmed native
// deletion is marked locally and reads as not found, never as a stale
// copy of an older refresh. The native 404 observation is fenced: only
// a still-current, unreserved object and binding can be tombstoned, so
// a stale 404 from a superseded native generation cannot delete a
// version a replacement has since re-bound.
func (s *Service) GetModelVersionById(id string) (*openapi.ModelVersion, error) {
	parsed, err := parseResourceID(id)
	if err != nil {
		return nil, fmt.Errorf("model version id %q is not a valid id: %w", id, api.ErrBadRequest)
	}
	object, err := s.store.GetObject(s.ctx, parsed)
	if err != nil {
		return nil, err
	}
	var binding VersionBinding
	if s.dbBinding(s.ctx, parsed, &binding) &&
		binding.NativeModelName != nil && binding.NativeVersionNumber != nil {
		if _, err := s.native.GetModelVersion(s.ctx,
			*binding.NativeModelName, *binding.NativeVersionNumber); err != nil {
			if errors.Is(MapClientError(err), api.ErrNotFound) {
				outcome, observeErr := s.observeNativeDeletion(s.ctx, object, binding)
				if observeErr != nil {
					return nil, observeErr
				}
				if outcome == deletionDeferred {
					// The observation lost arbitration: return the
					// current committed view, or its not-found result if
					// the version was deleted in the meantime. A stale
					// 404 never hides a healthy re-bound version.
					current, currentErr := s.store.GetObject(s.ctx, parsed)
					if currentErr != nil {
						return nil, currentErr
					}
					return versionView(current)
				}
				return nil, fmt.Errorf("model version id %q is not found natively: %w", id, api.ErrNotFound)
			}
			// A native read that failed for any other reason does not
			// hide the committed local observation.
			_ = err
		}
	}
	return versionView(object)
}

// observeNativeDeletion arbitrates a direct-read native 404 observation
// inside one short READ COMMITTED transaction, matching refresh. The
// version and its parent model rows are locked in ascending ID order —
// the global resource lock order — then the object, binding, and
// reservations are reread under those locks. Only a committed object at
// the observed revision, whose binding still carries the exact native
// identity that disappeared and holds no reservation, may be tombstoned
// through markObservedObjectDeleted, which performs the revision CAS
// and advances the revision exactly once. The native request itself
// stays outside the transaction.
func (s *Service) observeNativeDeletion(ctx context.Context, object Object,
	binding VersionBinding) (nativeDeletionOutcome, error) {
	outcome := deletionDeferred
	err := s.store.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ids := []int32{object.ID}
		if binding.ModelID != 0 && binding.ModelID != object.ID {
			ids = append(ids, binding.ModelID)
		}
		slices.Sort(ids)
		var current Object
		for _, id := range ids {
			row, err := lockResource(tx, id)
			if err != nil {
				if id == object.ID && errors.Is(err, api.ErrNotFound) {
					outcome = deletionAlreadyDeleted
					return nil
				}
				return err
			}
			if id == object.ID {
				current = row
			}
		}
		if current.Deleted {
			outcome = deletionAlreadyDeleted
			return nil
		}
		if current.Visibility != VisibilityCommitted || current.Revision != object.Revision {
			return nil // a newer revision invalidates the observation
		}
		var currentBinding VersionBinding
		bindingErr := tx.Where("version_id = ?", object.ID).First(&currentBinding).Error
		if errors.Is(bindingErr, gorm.ErrRecordNotFound) {
			return nil // the binding vanished; identity cannot be confirmed
		}
		if bindingErr != nil {
			return mapDBError(bindingErr)
		}
		if currentBinding.Revision != binding.Revision ||
			currentBinding.NativeModelName == nil || currentBinding.NativeVersionNumber == nil ||
			*currentBinding.NativeModelName != *binding.NativeModelName ||
			*currentBinding.NativeVersionNumber != *binding.NativeVersionNumber {
			return nil // the version re-bound to a newer native generation
		}
		var reserved int64
		if err := tx.Model(&IntentResource{}).
			Where("resource_id = ?", object.ID).Count(&reserved).Error; err != nil {
			return mapDBError(err)
		}
		if reserved > 0 {
			return nil // an in-flight operation still owns this version
		}
		if err := markObservedObjectDeleted(tx, current, nowMillis()); err != nil {
			return err
		}
		outcome = deletionPublished
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return deletionDeferred, err
	}
	return outcome, nil
}

// parseResourceID converts a decimal REST resource ID string to its
// numeric value.
func parseResourceID(id string) (int32, error) {
	parsed, err := strconv.ParseInt(id, 10, 32)
	if err != nil {
		return 0, err
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("id must be positive")
	}
	return int32(parsed), nil
}

// dbBinding loads the version binding for one logical version, if any.
func (s *Service) dbBinding(ctx context.Context, versionID int32, binding *VersionBinding) bool {
	err := s.store.DB().WithContext(ctx).Where("version_id = ?", versionID).First(binding).Error
	return err == nil
}

// applyVersionUpdate publishes a version metadata edit. A bound
// version's description and tag edits synchronize native metadata
// without creating a new generation; a pending version has no native
// projection and publishes in SQL only.
func (s *Service) applyVersionUpdate(ctx context.Context, claimed Operation) error {
	var binding VersionBinding
	if !s.dbBinding(ctx, claimed.ResourceID, &binding) ||
		binding.NativeModelName == nil || binding.NativeVersionNumber == nil {
		return s.store.PublishOperation(ctx, claimed.OperationUUID, claimed.FencingToken)
	}
	if err := s.verifyAliasParent(ctx, binding.ModelID, *binding.NativeModelName); err != nil {
		return err
	}

	candidate, resource, err := decodeOperationResource(claimed)
	if err != nil {
		return err
	}
	var version openapi.ModelVersion
	if err := decodeInto(resource, &version); err != nil {
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

	name, number := *binding.NativeModelName, *binding.NativeVersionNumber
	if derefString(version.Description) != priorSnapshotDescription(priorSnapshot) {
		if err := s.native.UpdateModelVersion(ctx, client.UpdateVersionRequest{
			Name: name, Version: number, Description: derefString(version.Description),
		}); err != nil {
			return MapClientError(err)
		}
	}
	owned := change.Snapshot.OwnedTags
	for _, key := range sortedKeys(priorSnapshot.OwnedTags) {
		if _, kept := owned[key]; !kept {
			if err := s.native.DeleteModelVersionTag(ctx, name, number, key); err != nil {
				return MapClientError(err)
			}
		}
	}
	for _, key := range sortedKeys(owned) {
		if prior, exists := priorSnapshot.OwnedTags[key]; !exists || prior != owned[key] {
			if err := s.native.SetModelVersionTag(ctx, name, number, key, owned[key]); err != nil {
				return MapClientError(err)
			}
		}
	}
	return s.store.PublishOperation(ctx, claimed.OperationUUID, claimed.FencingToken)
}
