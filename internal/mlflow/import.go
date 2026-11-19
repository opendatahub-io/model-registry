package mlflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strconv"

	"github.com/google/uuid"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// nativeIndex is the complete native view fetched before any SQL write.
// Every page of both searches is collected first: a refresh that cannot
// see the whole registry never publishes a partial observation.
type nativeIndex struct {
	models   map[string]client.RegisteredModel
	versions map[string][]client.ModelVersion
}

// fetchNativeIndex reads every page of the native registry. Any page
// failure aborts the refresh before SQL is touched.
func (s *Service) fetchNativeIndex(ctx context.Context) (*nativeIndex, error) {
	index := &nativeIndex{
		models:   map[string]client.RegisteredModel{},
		versions: map[string][]client.ModelVersion{},
	}

	modelsRequest := client.SearchRequest{MaxResults: 100}
	for {
		page, err := s.native.SearchRegisteredModels(ctx, modelsRequest)
		if err != nil {
			return nil, MapClientError(err)
		}
		for i := range page.RegisteredModels {
			index.models[page.RegisteredModels[i].Name] = page.RegisteredModels[i]
		}
		if page.NextPageToken == "" {
			break
		}
		modelsRequest.PageToken = page.NextPageToken
	}

	versionsRequest := client.SearchRequest{MaxResults: 100}
	for {
		page, err := s.native.SearchModelVersions(ctx, versionsRequest)
		if err != nil {
			return nil, MapClientError(err)
		}
		for i := range page.ModelVersions {
			index.versions[page.ModelVersions[i].Name] =
				append(index.versions[page.ModelVersions[i].Name], page.ModelVersions[i])
		}
		if page.NextPageToken == "" {
			break
		}
		versionsRequest.PageToken = page.NextPageToken
	}
	return index, nil
}

// tagMapOf renders native tags as a map.
func tagMapOf(tags []client.Tag) map[string]string {
	rendered := make(map[string]string, len(tags))
	for _, tag := range tags {
		rendered[tag.Key] = tag.Value
	}
	return rendered
}

// importNativeIndex reconciles the complete native view with the local
// store inside one transaction. Owned objects resolve through their
// recorded identities — never by guessing; unowned native objects are
// imported with stable names; objects whose native counterpart is
// confirmed missing are marked deleted. Reserved rows, active intents,
// and committed binding history are preserved untouched.
func importNativeIndex(ctx context.Context, tx *Store, index *nativeIndex,
	baseline ObservationBaseline) error {
	db := tx.DB().WithContext(ctx)

	// Serialize concurrent refresh transactions across replicas on the
	// adapter singleton.
	var adapter Adapter
	if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).
		Order("id").First(&adapter).Error; err != nil {
		return mapDBError(err)
	}
	adapterUUID := adapter.AdapterUUID

	// Local state maps.
	var modelRows []Object
	if err := db.Where("kind = ?", KindRegisteredModel).
		Find(&modelRows).Error; err != nil {
		return mapDBError(err)
	}
	modelsByName := map[string]Object{}
	modelsByID := map[int32]Object{}
	var versionRows []Object
	if err := db.Where("kind = ?", KindModelVersion).
		Find(&versionRows).Error; err != nil {
		return mapDBError(err)
	}
	versionsByID := map[int32]Object{}

	// Prefilter legacy correction by import provenance before taking locks.
	// Histories are reread after arbitration for authoritative identity checks.
	var histories []VersionHistory
	if err := db.Find(&histories).Error; err != nil {
		return mapDBError(err)
	}

	now := nowMillis()
	for _, history := range histories {
		if history.OperationUUID == "import" {
			if err := repairLegacyImportArtifacts(db, now); err != nil {
				return err
			}
			break
		}
	}
	// The legacy correction, when necessary, takes the complete ascending lock
	// set first. Ordinary observations lock only their model/version resources.
	ids := map[int32]bool{}
	for _, row := range modelRows {
		ids[row.ID] = true
	}
	for _, row := range versionRows {
		ids[row.ID] = true
	}
	for _, id := range sortedResourceIDs(ids) {
		row, err := lockResource(db, id)
		if err != nil {
			return err
		}
		if row.Kind == KindRegisteredModel {
			modelsByID[id] = row
			modelsByName[row.Name] = row
		} else {
			versionsByID[id] = row
		}
	}
	// These are current reads under the object locks. Graph preparation and
	// publication use those same locks before changing bindings/reservations.
	var bindings []VersionBinding
	if err := db.Find(&bindings).Error; err != nil {
		return mapDBError(err)
	}
	bindingByVersion := map[int32]VersionBinding{}
	// An unresolved binding still owns any untagged next native result.
	pendingMaterialization := map[int32]bool{}
	for _, binding := range bindings {
		bindingByVersion[binding.VersionID] = binding
		if binding.NativeVersionNumber == nil {
			pendingMaterialization[binding.ModelID] = true
		}
	}
	var reservations []IntentResource
	if err := db.Find(&reservations).Error; err != nil {
		return mapDBError(err)
	}
	reserved := map[int32]bool{}
	for _, reservation := range reservations {
		reserved[reservation.ResourceID] = true
	}
	eligible := func(row Object) bool {
		revision, observed := baseline.ObjectRevisions[row.ID]
		if !observed || revision != row.Revision || reserved[row.ID] || row.Deleted || row.Visibility != VisibilityCommitted {
			return false
		}
		binding, bound := bindingByVersion[row.ID]
		bindingRevision, wasBound := baseline.BindingRevisions[row.ID]
		return bound == wasBound && (!bound || binding.Revision == bindingRevision)
	}
	if err := db.Find(&histories).Error; err != nil {
		return mapDBError(err)
	}
	historyByNative := map[string]VersionHistory{}
	for _, history := range histories {
		historyByNative[history.NativeModelName+"/"+history.NativeVersionNumber] = history
	}
	resolvedModels := map[string]Object{}
	nativeModelSeen := map[string]bool{}

	importModel := func(native client.RegisteredModel) (Object, error) {
		tags := tagMapOf(native.Tags)
		if err := validateObservedAdapter(tags, adapterUUID); err != nil {
			return Object{}, err
		}
		local, hasLocal := modelsByName[native.Name]
		if _, tagged := tags[tagLogicalID]; tagged {
			// Ours: resolve through the recorded identity.
			logicalID, err := parseResourceID(tags[tagLogicalID])
			if err != nil {
				return Object{}, fmt.Errorf("native model %q carries an invalid logical id tag: %w",
					native.Name, api.ErrConflict)
			}
			owned, exists := modelsByID[logicalID]
			if !exists || owned.Kind != KindRegisteredModel {
				return Object{}, fmt.Errorf("native model %q claims logical id %d, which is not a local registered model: %w",
					native.Name, logicalID, api.ErrConflict)
			}
			if owned.Name != native.Name || (hasLocal && local.ID != logicalID) {
				return Object{}, fmt.Errorf("native model %q claims logical id %d, but the name belongs to model %d: %w",
					native.Name, logicalID, local.ID, api.ErrConflict)
			}
			if owned.Deleted || owned.Visibility == VisibilityReserved {
				return Object{}, nil
			}
			if eligible(owned) {
				if err := refreshModelSnapshot(db, owned, native, tags, now); err != nil {
					return Object{}, err
				}
			}
			return owned, nil
		}
		if hasLocal {
			// A local model owns this name; the native tags may have
			// been edited or removed externally. Refresh the exported
			// fields without guessing ownership.
			if local.Visibility == VisibilityReserved || local.Deleted {
				return Object{}, nil // preserve the pending create
			}
			if err := validateObservedIdentity(tags, adapterUUID, local.ID, nil); err != nil {
				return Object{}, err
			}
			if eligible(local) {
				if err := refreshModelSnapshot(db, local, native, tags, now); err != nil {
					return Object{}, err
				}
			}
			return local, nil
		}
		// An unowned native model with no local namesake: import it.
		if _, claimed := tags[tagAdapterUUID]; claimed {
			return Object{}, fmt.Errorf("native model %q has no recorded logical identity: %w", native.Name, api.ErrConflict)
		}
		imported := decodeImportedModel(native, tags)
		if err := applyModelReservedTags(imported, tags); err != nil {
			return Object{}, err
		}
		resource, err := marshalImportedResource(imported)
		if err != nil {
			return Object{}, err
		}
		projection, err := EncodeModel(imported)
		if err != nil {
			return Object{}, err
		}
		snapshot := StoredSnapshot{
			CodecVersion: candidateCodecVersion, Kind: KindRegisteredModel,
			Resource: resource, TagKeys: customTagKeys(imported.CustomProperties),
			OwnedTags: observedMetadataProjection(projection, tags, nil),
		}
		encoded, err := marshalImportedResource(snapshot)
		if err != nil {
			return Object{}, err
		}
		row := Object{
			Kind: KindRegisteredModel, ParentID: parentNone, Name: native.Name,
			ExternalID: imported.ExternalId, CreateTimeSinceEpoch: now,
			LastUpdateTimeSinceEpoch: now,
			CommittedJSON:            string(encoded),
			Visibility:               VisibilityCommitted,
		}
		if err := db.Create(&row).Error; err != nil {
			return Object{}, mapDBError(fmt.Errorf("importing native model %q: %w", native.Name, err))
		}
		return row, nil
	}

	for _, name := range sortedNativeNames(index.models) {
		nativeModelSeen[name] = true
		row, err := importModel(index.models[name])
		if err != nil {
			return err
		}
		if row.ID != 0 {
			resolvedModels[name] = row
		}
	}

	// Track which logical versions are actively claimed natively, to
	// detect duplicate claims.
	activeClaims := map[int32]string{}

	importVersion := func(model Object, native client.ModelVersion) error {
		tags := tagMapOf(native.Tags)
		if err := validateObservedAdapter(tags, adapterUUID); err != nil {
			return err
		}
		// Imported generations can remain untagged forever. Exact history is
		// durable identity proof regardless of which optional tags remain.
		history, recorded := historyByNative[native.Name+"/"+native.Version]
		historical := false
		if recorded {
			if history.Source != native.Source {
				return fmt.Errorf("native source disagrees with recorded history for version %d generation %d: %w", history.VersionID, history.Generation, api.ErrConflict)
			}
			if err := validateObservedIdentity(tags, adapterUUID, history.VersionID, &history.Generation); err != nil {
				return err
			}
			version, exists := versionsByID[history.VersionID]
			if !exists || version.ParentID != model.ID {
				return fmt.Errorf("native history belongs to another logical model: %w", api.ErrConflict)
			}
			binding, bound := bindingByVersion[history.VersionID]
			if !bound || binding.NativeModelName == nil || binding.NativeVersionNumber == nil || *binding.NativeModelName != native.Name || *binding.NativeVersionNumber != native.Version {
				historical = true
			}
		}
		operationUUID := tags[tagOperation]
		if recorded, err := verifyRecordedDisposition(db, adapterUUID, 0, &native); err != nil || recorded {
			return err
		}
		if operationUUID != "" {
			var operation Operation
			err := db.Where("operation_uuid = ?", operationUUID).First(&operation).Error
			if err != nil && err != gorm.ErrRecordNotFound {
				return mapDBError(err)
			}
			if err == nil {
				plan, legacy, err := nativeObservationPlan(operation)
				if err != nil {
					return err
				}
				if plan != nil {
					version, exists := versionsByID[plan.LogicalVersionID]
					if !exists || version.ParentID != model.ID || model.Name != plan.ModelName {
						return fmt.Errorf("native operation result belongs to another logical parent: %w", api.ErrConflict)
					}
					binding := bindingByVersion[plan.LogicalVersionID]
					established := recorded && history.VersionID == plan.LogicalVersionID && history.Generation == plan.Generation
					if !recorded && matchesNativeBinding(binding, native) && binding.ActiveGeneration == plan.Generation {
						established = true
					}
					if recorded && !established {
						return fmt.Errorf("native operation contradicts recorded history: %w", api.ErrConflict)
					}
					if established {
						if err := verifyNativeWorkFields(plan, &native); err != nil {
							return err
						}
						if err := validateObservedIdentity(tags, adapterUUID, plan.LogicalVersionID, &plan.Generation); err != nil {
							return err
						}
					} else if err := verifyNativeWorkIdentity(adapterUUID, operation, plan, &native, legacy); err != nil {
						return err
					}
				} else {
					version, exists := versionsByID[operation.ResourceID]
					binding := bindingByVersion[operation.ResourceID]
					if !exists || version.ParentID != model.ID {
						return fmt.Errorf("native metadata operation belongs to another logical parent: %w", api.ErrConflict)
					}
					generation := binding.ActiveGeneration
					if recorded {
						if history.VersionID != version.ID || history.Source != native.Source {
							return fmt.Errorf("native metadata operation contradicts recorded history: %w", api.ErrConflict)
						}
						generation = history.Generation
					} else if !matchesNativeBinding(binding, native) {
						return fmt.Errorf("native metadata operation does not own this binding: %w", api.ErrConflict)
					}
					if err := validateObservedIdentity(tags, adapterUUID, version.ID, &generation); err != nil {
						return err
					}
				}
				selected, explicit := explicitNativeSelection(operation)
				explicitAlternative := explicit && !selectedIdentity(operation, native.Name, native.Version)
				if explicitAlternative || operation.SupersededByOperationUUID != nil || (operation.FailureCode != nil && *operation.FailureCode == DispositionOrphanRetained) {
					plan, _, err := operatorNativePlan(operation)
					if err != nil {
						return err
					}
					disposition, resolver := DispositionOrphanRetained, operation.OperationUUID
					if explicitAlternative {
						// A recorded explicit choice remains authoritative after its
						// publication. Later exact alternatives inherit that decision;
						// ordinary completed operations confer no such permission.
						if selected.ModelName != plan.ModelName || selected.Source != plan.Source || selected.Generation != plan.Generation {
							return fmt.Errorf("explicit selection disagrees with recorded work: %w", api.ErrConflict)
						}
						disposition = DispositionUnselectedDuplicate
					}
					if operation.SupersededByOperationUUID != nil {
						selected, err := resolveAttempt(db, operation.OperationUUID)
						if err != nil {
							return err
						}
						disposition, resolver = DispositionUnselectedDuplicate, selected.OperationUUID
					}
					return recordNativeDisposition(db, adapterUUID, operation, plan, native, disposition, resolver)
				}
				if operation.State == OperationPending || operation.State == OperationRunning {
					return nil
				}
				if rootID := graphRoot(operation); rootID != operation.OperationUUID {
					root, err := resolveAttempt(db, rootID)
					if err != nil {
						return err
					}
					if root.State == OperationPending || root.State == OperationRunning {
						return nil
					}
				}
			}
		}
		if historical {
			return nil
		}

		if _, tagged := tags[tagLogicalID]; tagged {
			logicalID, err := parseResourceID(tags[tagLogicalID])
			if err != nil {
				return fmt.Errorf("native version %s/%s carries an invalid logical id tag: %w",
					native.Name, native.Version, api.ErrConflict)
			}
			version, exists := versionsByID[logicalID]
			if !exists || version.Kind != KindModelVersion {
				return fmt.Errorf("native version %s/%s claims logical id %d, which is not a local model version: %w",
					native.Name, native.Version, logicalID, api.ErrConflict)
			}
			if version.ParentID != model.ID {
				return fmt.Errorf("native version %s/%s claims another model's version: %w", native.Name, native.Version, api.ErrConflict)
			}
			binding, bound := bindingByVersion[logicalID]
			if !bound || binding.NativeModelName == nil || binding.NativeVersionNumber == nil ||
				*binding.NativeModelName != native.Name || *binding.NativeVersionNumber != native.Version {
				// Not the active binding: legitimate only as a superseded
				// replacement generation retained in history. Any other
				// owned claim is an unknown identity, never a guess.
				return fmt.Errorf("native version %s/%s claims logical version %d, which its binding and history do not record: %w",
					native.Name, native.Version, logicalID, api.ErrConflict)
			}
			if other, claimedBy := activeClaims[logicalID]; claimedBy && other != native.Version {
				return fmt.Errorf("native versions %s and %s both actively claim logical version %d: %w",
					other, native.Version, logicalID, api.ErrConflict)
			}
			activeClaims[logicalID] = native.Version
			if err := validateObservedIdentity(tags, adapterUUID, logicalID, &binding.ActiveGeneration); err != nil {
				return err
			}
			if !eligible(version) {
				return nil
			}
			return refreshVersionSnapshot(db, version, native, tags, now)
		}
		// Resolve existing imports by their established native binding, before
		// looking at an externally editable name tag.
		for _, binding := range bindings {
			if binding.NativeModelName != nil && *binding.NativeModelName == native.Name && binding.NativeVersionNumber != nil && *binding.NativeVersionNumber == native.Version {
				version := versionsByID[binding.VersionID]
				if err := validateObservedIdentity(tags, adapterUUID, binding.VersionID, &binding.ActiveGeneration); err != nil {
					return err
				}
				if !eligible(version) {
					return nil
				}
				return refreshVersionSnapshot(db, version, native, tags, now)
			}
		}
		if _, claimed := tags[tagAdapterUUID]; claimed {
			return fmt.Errorf("native version %s/%s has no recorded logical identity: %w", native.Name, native.Version, api.ErrConflict)
		}

		// An unowned native version: import it under its model with a
		// stable name — the explicit versionName tag when present, the
		// decimal version number otherwise. A model with an unfinished
		// materialization in flight owns its next native result, so its
		// untagged versions are excluded until the operation completes.
		if pendingMaterialization[model.ID] {
			return nil
		}
		name := tags["rhoai.versionName"]
		if name == "" {
			name = native.Version
		}
		var conflict Object
		err := db.Where("kind = ? AND parent_id = ? AND name = ? AND deleted = ?",
			KindModelVersion, model.ID, name, false).First(&conflict).Error
		if err == nil {
			if conflict.Visibility == VisibilityReserved {
				return nil // preserve the pending create
			}
			// A reserved or committed local version already holds the
			// name: the import cannot guess which identity this is.
			binding, bound := bindingByVersion[conflict.ID]
			if bound && binding.NativeModelName != nil && *binding.NativeModelName == native.Name &&
				binding.NativeVersionNumber != nil && *binding.NativeVersionNumber == native.Version {
				return nil // already ours, tags stripped externally
			}
			return fmt.Errorf("native version %s/%s imports as name %q, which a local version already holds: %w",
				native.Name, native.Version, name, api.ErrConflict)
		}
		if err != gorm.ErrRecordNotFound {
			return mapDBError(err)
		}

		initialGeneration := int64(1)
		if err := validateObservedIdentity(tags, adapterUUID, 0, &initialGeneration); err != nil {
			return err
		}
		imported := decodeImportedVersion(model.ID, name, native, tags)
		if err := applyVersionReservedTags(imported, tags); err != nil {
			return err
		}
		resource, err := marshalImportedResource(imported)
		if err != nil {
			return err
		}
		projection, err := EncodeVersion(imported)
		if err != nil {
			return err
		}
		versionSnapshot := StoredSnapshot{
			CodecVersion: candidateCodecVersion, Kind: KindModelVersion,
			Resource: resource, TagKeys: customTagKeys(imported.CustomProperties),
			OwnedTags: observedMetadataProjection(projection, tags, nil),
		}
		encoded, err := marshalImportedResource(versionSnapshot)
		if err != nil {
			return err
		}
		versionRow := Object{
			Kind: KindModelVersion, ParentID: model.ID, Name: name,
			ExternalID: imported.ExternalId, CreateTimeSinceEpoch: now,
			LastUpdateTimeSinceEpoch: now,
			CommittedJSON:            string(encoded),
			Visibility:               VisibilityCommitted,
		}
		if err := db.Create(&versionRow).Error; err != nil {
			return mapDBError(fmt.Errorf("importing native version %s/%s: %w", native.Name, native.Version, err))
		}

		// A source-bearing native version synthesizes its primary
		// artifact, binding, link, and first history generation.
		primaryArtifactID := (*int32)(nil)
		if native.Source != "" {
			artifact := &openapi.ModelArtifact{
				Name: new(native.Name), Uri: new(native.Source),
				Description: new(native.Description),
			}
			resource, err := marshalImportedResource(artifact)
			if err != nil {
				return err
			}
			artifactSnapshot := StoredSnapshot{
				CodecVersion: candidateCodecVersion, Kind: KindModelArtifact,
				Resource: resource, TagKeys: nil,
			}
			encoded, err := marshalImportedResource(artifactSnapshot)
			if err != nil {
				return err
			}
			artifactRow := Object{
				Kind: KindModelArtifact, ParentID: parentNone,
				Name:                     native.Name,
				ArtifactScopeID:          versionRow.ID,
				CreateTimeSinceEpoch:     now,
				LastUpdateTimeSinceEpoch: now,
				CommittedJSON:            string(encoded),
				Visibility:               VisibilityCommitted,
			}
			if err := db.Create(&artifactRow).Error; err != nil {
				return mapDBError(fmt.Errorf("synthesizing the primary artifact of %s/%s: %w",
					native.Name, native.Version, err))
			}
			if err := db.Create(&ArtifactLink{
				ArtifactID: artifactRow.ID, VersionID: versionRow.ID,
			}).Error; err != nil {
				return mapDBError(err)
			}
			primaryArtifactID = &artifactRow.ID
		}
		if err := db.Create(&VersionBinding{
			VersionID: versionRow.ID, ModelID: model.ID,
			PrimaryArtifactID: primaryArtifactID, Phase: PhaseReady,
			NativeModelName: new(native.Name), NativeVersionNumber: new(native.Version),
			ActiveGeneration: 1, DesiredGeneration: 1,
		}).Error; err != nil {
			return mapDBError(err)
		}
		return db.Create(&VersionHistory{
			VersionID: versionRow.ID, Generation: 1,
			NativeModelName: native.Name, NativeVersionNumber: native.Version,
			Source: native.Source, OperationUUID: "import",
		}).Error
	}

	for _, modelName := range sortedNativeNames(index.versions) {
		model, resolved := resolvedModels[modelName]
		if !resolved {
			continue
		}
		versions := index.versions[modelName]
		sort.Slice(versions, func(i, j int) bool {
			a, _ := strconv.Atoi(versions[i].Version)
			b, _ := strconv.Atoi(versions[j].Version)
			return a < b
		})
		for _, native := range versions {
			if err := importVersion(model, native); err != nil {
				return err
			}
		}
	}

	// Confirmed deletions: a committed local model whose native
	// counterpart is absent from the complete observation is marked
	// deleted, as is a bound local version whose native counterpart
	// vanished. Nothing is ever recreated from an older refresh.
	for _, row := range modelsByID {
		if !nativeModelSeen[row.Name] && eligible(row) {
			if err := markObservedObjectDeleted(db, row, now); err != nil {
				return err
			}
		}
	}
	for _, binding := range bindings {
		if binding.NativeModelName == nil || binding.NativeVersionNumber == nil {
			continue
		}
		nativeVersions, exists := index.versions[*binding.NativeModelName]
		if exists {
			found := false
			for _, native := range nativeVersions {
				if native.Version == *binding.NativeVersionNumber {
					found = true
					break
				}
			}
			if found {
				continue
			}
		}
		if row := versionsByID[binding.VersionID]; eligible(row) {
			if err := markObservedObjectDeleted(db, row, now); err != nil {
				return err
			}
		}
	}

	// The successful refresh timestamp persists in the same
	// transaction as the observation it certifies.
	return db.Model(&Adapter{}).Where("id = ?", adapter.ID).
		Update("last_refresh_time", now).Error
}

func markObservedObjectDeleted(db *gorm.DB, row Object, now int64) error {
	result := db.Model(&Object{}).Where("id = ? AND revision = ?", row.ID, row.Revision).
		Updates(map[string]any{"deleted": true, "revision": row.Revision + 1, "last_update_time_since_epoch": now})
	if result.Error != nil {
		return mapDBError(result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("object %d changed during deletion observation: %w", row.ID, api.ErrConflict)
	}
	return nil
}

// sortedNativeNames renders deterministic import order.
func sortedNativeNames[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// refreshModelSnapshot applies externally visible changes to a local
// model's exported fields, preserving the private key mapping and type
// discriminators in the stored snapshot. A refresh that observes no
// change writes nothing, so it never invalidates a concurrent writer's
// expected revision.
func refreshModelSnapshot(db *gorm.DB, row Object, native client.RegisteredModel,
	tags map[string]string, now int64) error {
	snapshot, err := decodeStoredSnapshot(row.CommittedJSON)
	if err != nil {
		return err
	}
	var model openapi.RegisteredModel
	if err := json.Unmarshal(snapshot.Resource, &model); err != nil {
		return fmt.Errorf("decoding model %d snapshot: %w", row.ID, api.ErrBadRequest)
	}
	if derefString(model.Description) != native.Description {
		model.Description = new(native.Description)
	}
	if err := applyModelReservedTags(&model, tags); err != nil {
		return err
	}
	if err := applyExternalTags(&model.CustomProperties, tags, snapshot.TagKeys); err != nil {
		return err
	}
	encoded, err := marshalImportedResource(&model)
	if err != nil {
		return err
	}
	projection, err := EncodeModel(&model)
	if err != nil {
		return err
	}
	observedTags := observedMetadataProjection(projection, tags, snapshot.OwnedTags)
	if string(encoded) == string(snapshot.Resource) && maps.Equal(observedTags, snapshot.OwnedTags) {
		return nil
	}
	snapshot.Resource = encoded
	snapshot.OwnedTags = observedTags
	return updateImportedSnapshot(db, row, snapshot, model.ExternalId, now)
}

// refreshVersionSnapshot applies externally visible changes to a local
// version's exported fields. The logical name never changes: it is the
// local identity, and an external edit of the projection tag is not a
// rename. A refresh that observes no change writes nothing.
func refreshVersionSnapshot(db *gorm.DB, row Object, native client.ModelVersion,
	tags map[string]string, now int64) error {
	snapshot, err := decodeStoredSnapshot(row.CommittedJSON)
	if err != nil {
		return err
	}
	var version openapi.ModelVersion
	if err := json.Unmarshal(snapshot.Resource, &version); err != nil {
		return fmt.Errorf("decoding version %d snapshot: %w", row.ID, api.ErrBadRequest)
	}
	if derefString(version.Description) != native.Description {
		version.Description = new(native.Description)
	}
	if err := applyVersionReservedTags(&version, tags); err != nil {
		return err
	}
	if err := applyExternalTags(&version.CustomProperties, tags, snapshot.TagKeys); err != nil {
		return err
	}
	encoded, err := marshalImportedResource(&version)
	if err != nil {
		return err
	}
	projection, err := EncodeVersion(&version)
	if err != nil {
		return err
	}
	observedTags := observedMetadataProjection(projection, tags, snapshot.OwnedTags)
	if string(encoded) == string(snapshot.Resource) && maps.Equal(observedTags, snapshot.OwnedTags) {
		return nil
	}
	snapshot.Resource = encoded
	snapshot.OwnedTags = observedTags
	return updateImportedSnapshot(db, row, snapshot, version.ExternalId, now)
}

func updateImportedSnapshot(db *gorm.DB, row Object, snapshot StoredSnapshot, externalID *string, now int64) error {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("serializing snapshot for object %d: %w", row.ID, err)
	}
	result := db.Model(&Object{}).
		Where("id = ? AND revision = ?", row.ID, row.Revision).
		Where("NOT EXISTS (?)", db.Model(&IntentResource{}).Select("1").Where("resource_id = ?", row.ID)).
		Updates(map[string]any{
			"committed_json":               string(encoded),
			"external_id":                  externalID,
			"revision":                     row.Revision + 1,
			"last_update_time_since_epoch": now,
		})
	if result.Error != nil {
		return mapDBError(result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("object %d changed during metadata observation: %w", row.ID, api.ErrConflict)
	}
	return nil
}

func observedStringTag(tags map[string]string, key string) *string {
	if value, exists := tags[key]; exists {
		return new(value)
	}
	return nil
}

// Record only the resource's exported metadata as the next native diff's
// baseline. Creation provenance remains local; other reserved namespaces
// (including artifact metadata on a version) are never adopted or deleted.
func observedMetadataProjection(projected, native, prior map[string]string) map[string]string {
	observed := map[string]string{}
	for key := range projected {
		if value, present := native[key]; present {
			observed[key] = value
		}
	}
	return carryCreationTags(observed, prior)
}

func validateObservedAdapter(tags map[string]string, adapterUUID string) error {
	if adapter, present := tags[tagAdapterUUID]; present && adapter != adapterUUID {
		return fmt.Errorf("native object carries a foreign adapter identity: %w", api.ErrConflict)
	}
	return nil
}

// Missing legacy tags can use exact stored identity proof. Present tags must
// agree with it; stripping one tag never authorizes a conflicting remaining tag.
func validateObservedIdentity(tags map[string]string, adapterUUID string, logicalID int32, generation *int64) error {
	if err := validateObservedAdapter(tags, adapterUUID); err != nil {
		return err
	}
	if value, present := tags[tagLogicalID]; present {
		id, err := parseResourceID(value)
		if err != nil || id != logicalID {
			return fmt.Errorf("native logical identity does not match recorded object %d: %w", logicalID, api.ErrConflict)
		}
	}
	if value, present := tags[tagGeneration]; present && generation != nil {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed != *generation {
			return fmt.Errorf("native generation does not match recorded generation %d: %w", *generation, api.ErrConflict)
		}
	}
	return nil
}

func applyModelReservedTags(model *openapi.RegisteredModel, tags map[string]string) error {
	model.Owner = observedStringTag(tags, "rhoai.owner")
	model.ExternalId = observedStringTag(tags, "rhoai.externalId")
	model.State = nil
	if value, exists := tags["rhoai.state"]; exists {
		state := openapi.RegisteredModelState(value)
		if !state.IsValid() {
			return fmt.Errorf("native model %q has an invalid state tag: %w", model.Name, api.ErrConflict)
		}
		model.State = &state
	}
	return nil
}

func applyVersionReservedTags(version *openapi.ModelVersion, tags map[string]string) error {
	if name, exists := tags["rhoai.versionName"]; exists && name != version.Name {
		return fmt.Errorf("native version name tag disagrees with logical identity %q: %w", version.Name, api.ErrConflict)
	}
	version.Author = observedStringTag(tags, "rhoai.author")
	version.ExternalId = observedStringTag(tags, "rhoai.externalId")
	version.State = nil
	if value, exists := tags["rhoai.state"]; exists {
		state := openapi.ModelVersionState(value)
		if !state.IsValid() {
			return fmt.Errorf("native version %q has an invalid state tag: %w", version.Name, api.ErrConflict)
		}
		version.State = &state
	}
	return nil
}

// applyExternalTags merges unrelated native tags into a resource's
// custom properties. Local values keep their types: an external change
// is parsed only when it is valid for the prior type, and an invalid
// changed value becomes the exact external string. Properties
// projected under escaped tag keys are recovered through the stored
// original-key mapping; adapter-owned tags are never merged.
func applyExternalTags(properties *map[string]openapi.MetadataValue,
	tags map[string]string, tagKeys map[string]string) error {
	merged := map[string]openapi.MetadataValue{}
	maps.Copy(merged, *properties)
	reverse := map[string]string{}
	for original, projected := range tagKeys {
		reverse[projected] = original
	}
	for key, value := range tags {
		propertyKey := key
		if original, mapped := reverse[key]; mapped {
			propertyKey = original
		} else if isAdapterOwnedTagKey(key) {
			continue
		}
		if prior, exists := merged[propertyKey]; exists {
			parsed, err := mergeExternalValue(prior, value)
			if err != nil {
				return fmt.Errorf("merging custom property %q: %w", propertyKey, err)
			}
			merged[propertyKey] = parsed
			continue
		}
		merged[propertyKey] = openapi.MetadataValue{
			MetadataStringValue: &openapi.MetadataStringValue{
				StringValue: value, MetadataType: "MetadataStringValue",
			},
		}
	}
	// Properties the native view no longer carries are removed only
	// now, after the complete refresh observed their absence. A
	// property survives when the tags carry its projected — possibly
	// escaped — tag key.
	for key := range merged {
		projected := key
		if escaped, mapped := tagKeys[key]; mapped {
			projected = escaped
		}
		if _, carried := tags[projected]; carried {
			continue
		}
		delete(merged, key)
	}
	if len(merged) == 0 {
		*properties = nil // match the omitted-field encoding of a fresh resource
		return nil
	}
	*properties = merged
	return nil
}

// decodeImportedModel builds the local resource of an unowned native
// model from its exported fields.
func decodeImportedModel(native client.RegisteredModel, tags map[string]string) *openapi.RegisteredModel {
	model := &openapi.RegisteredModel{Name: native.Name}
	if native.Description != "" {
		model.Description = new(native.Description)
	}
	if owner, exists := tags["rhoai.owner"]; exists {
		model.Owner = new(owner)
	}
	if externalID, exists := tags["rhoai.externalId"]; exists {
		model.ExternalId = new(externalID)
	}
	model.CustomProperties = stringProperties(tags)
	return model
}

// decodeImportedVersion builds the local resource of an unowned native
// version from its exported fields.
func decodeImportedVersion(parentID int32, name string, native client.ModelVersion,
	tags map[string]string) *openapi.ModelVersion {
	version := &openapi.ModelVersion{
		Name: name, RegisteredModelId: strconv.FormatInt(int64(parentID), 10),
	}
	if native.Description != "" {
		version.Description = new(native.Description)
	}
	if author, exists := tags["rhoai.author"]; exists {
		version.Author = new(author)
	}
	if externalID, exists := tags["rhoai.externalId"]; exists {
		version.ExternalId = new(externalID)
	}
	version.CustomProperties = stringProperties(tags)
	return version
}

// stringProperties renders unrelated native tags as string properties.
func stringProperties(tags map[string]string) map[string]openapi.MetadataValue {
	properties := map[string]openapi.MetadataValue{}
	for key, value := range tags {
		if isAdapterOwnedTagKey(key) {
			continue
		}
		properties[key] = openapi.MetadataValue{
			MetadataStringValue: &openapi.MetadataStringValue{
				StringValue: value, MetadataType: "MetadataStringValue",
			},
		}
	}
	return properties
}

// repairLegacyImportArtifacts corrects only the old import representation
// proven by the immutable import history, active binding, link and source.
// A name prefix is never provenance. Active candidates and explicit sharing
// defer correction so immutable operation evidence and relationships survive.

func repairLegacyImportArtifacts(db *gorm.DB, now int64) error {
	var bindings []VersionBinding
	if err := db.Where("primary_artifact_id IS NOT NULL").Order("version_id").Find(&bindings).Error; err != nil {
		return mapDBError(err)
	}
	var candidates []VersionBinding
	for _, binding := range bindings {
		_, _, eligible, err := legacyImportEvidence(db, binding)
		if err != nil {
			return err
		}
		if eligible {
			candidates = append(candidates, binding)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	// This one-time upgrade correction runs before any observation writes.
	// Lock the complete existing resource set in one ascending order: later
	// import/model/deletion writes then reuse these locks instead of taking a
	// lower ID after an artifact. Normal refresh never takes these broad locks.
	var ids []int32
	if err := db.Model(&Object{}).Order("id").Pluck("id", &ids).Error; err != nil {
		return mapDBError(err)
	}
	locked := map[int32]Object{}
	for _, id := range ids {
		row, err := lockResource(db, id)
		if err != nil {
			return err
		}
		locked[id] = row
	}
	for _, candidate := range candidates {
		var binding VersionBinding
		if err := db.Where("version_id = ?", candidate.VersionID).First(&binding).Error; err != nil {
			return mapDBError(err)
		}
		// A changed identity is deferred; never extend the lock set after
		// arbitration, even if a newly selected primary would sort higher.
		if binding.PrimaryArtifactID == nil || *binding.PrimaryArtifactID != *candidate.PrimaryArtifactID ||
			binding.NativeModelName == nil || binding.NativeVersionNumber == nil ||
			*binding.NativeModelName != *candidate.NativeModelName || *binding.NativeVersionNumber != *candidate.NativeVersionNumber ||
			binding.ActiveGeneration != candidate.ActiveGeneration || binding.Revision != candidate.Revision || binding.ModelID != candidate.ModelID {
			continue
		}
		if _, ok := locked[*binding.PrimaryArtifactID]; !ok {
			continue
		}
		if _, ok := locked[binding.VersionID]; !ok {
			continue
		}
		row, snapshot, eligible, err := legacyImportEvidence(db, binding)
		if err != nil {
			return err
		}
		if !eligible {
			continue
		}
		// Current READ COMMITTED guard reads happen after all resource locks.
		// Artifact edits/shares prepare against the artifact resource; version
		// and replacement-child intents prepare against the version resource.
		// Those prepares and FK-backed link inserts cannot cross these locks.
		// No operation-row locks invert the publication resource/operation order.
		var collisions int64
		if err := db.Model(&Object{}).Where("kind = ? AND parent_id = ? AND artifact_scope_id = ? AND name = ? AND id <> ?", KindModelArtifact, row.ParentID, binding.VersionID, *binding.NativeModelName, row.ID).Count(&collisions).Error; err != nil {
			return mapDBError(err)
		}
		if collisions != 0 {
			continue
		}
		var artifact openapi.ModelArtifact
		if err := json.Unmarshal(snapshot.Resource, &artifact); err != nil {
			return err
		}
		artifact.Name = new(*binding.NativeModelName)
		snapshot.Resource, err = marshalImportedResource(&artifact)
		if err != nil {
			return err
		}
		encoded, err := marshalImportedResource(snapshot)
		if err != nil {
			return err
		}
		result := db.Model(&Object{}).Where("id = ? AND revision = ?", row.ID, row.Revision).Updates(map[string]any{"name": *binding.NativeModelName, "artifact_scope_id": binding.VersionID, "committed_json": string(encoded), "revision": row.Revision + 1, "last_update_time_since_epoch": now})
		if result.Error != nil {
			return mapDBError(result.Error)
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("legacy imported artifact %d changed during correction: %w", row.ID, api.ErrConflict)
		}
		audit := struct {
			MutationCandidate
			PreviousName string `json:"previousName"`
		}{MutationCandidate: MutationCandidate{CodecVersion: candidateCodecVersion, Objects: []ObjectChange{{ID: row.ID, ExpectedRevision: row.Revision, Snapshot: snapshot}}}, PreviousName: row.Name}
		payload, err := marshalImportedResource(audit)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(payload)
		if err := db.Create(&Operation{OperationUUID: uuid.NewString(), ResourceID: row.ID, Kind: "import-artifact-name-correction", ExpectedRevision: row.Revision, CandidateJSON: string(payload), PayloadHash: hex.EncodeToString(digest[:]), State: OperationSucceeded, Step: StepPublished, CreateTimeSinceEpoch: now, LastUpdateTimeSinceEpoch: now}).Error; err != nil {
			return mapDBError(err)
		}
	}
	return nil
}

// legacyImportEvidence is a nonauthoritative prefilter before arbitration and
// a current authoritative guard when rerun under Refresh's resource locks.
// Every read uses the Refresh transaction's READ COMMITTED isolation.
func legacyImportEvidence(db *gorm.DB, binding VersionBinding) (Object, StoredSnapshot, bool, error) {
	skip := func() (Object, StoredSnapshot, bool, error) { return Object{}, StoredSnapshot{}, false, nil }
	failure := func(err error) (Object, StoredSnapshot, bool, error) {
		return Object{}, StoredSnapshot{}, false, mapDBError(err)
	}
	if binding.PrimaryArtifactID == nil || binding.NativeModelName == nil || binding.NativeVersionNumber == nil || binding.Phase != PhaseReady {
		return skip()
	}
	var row Object
	if err := db.Where("id = ?", *binding.PrimaryArtifactID).First(&row).Error; err != nil {
		return failure(err)
	}
	if row.Kind != KindModelArtifact || row.Deleted || row.Visibility != VisibilityCommitted || row.ArtifactScopeID != 0 ||
		row.Name != fmt.Sprintf("native-%s-%s", *binding.NativeModelName, *binding.NativeVersionNumber) {
		return skip()
	}
	var version, model Object
	if err := db.Where("id = ?", binding.VersionID).First(&version).Error; err != nil {
		return failure(err)
	}
	if version.Kind != KindModelVersion || version.Deleted || version.Visibility != VisibilityCommitted || version.ParentID != binding.ModelID {
		return skip()
	}
	if err := db.Where("id = ?", version.ParentID).First(&model).Error; err != nil {
		return failure(err)
	}
	if model.Kind != KindRegisteredModel || model.Name != *binding.NativeModelName || model.Deleted || model.Visibility != VisibilityCommitted {
		return skip()
	}
	var history VersionHistory
	if err := db.Where("version_id = ? AND generation = ? AND native_model_name = ? AND native_version_number = ? AND operation_uuid = ? AND superseded = ?", version.ID, binding.ActiveGeneration, *binding.NativeModelName, *binding.NativeVersionNumber, "import", false).First(&history).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return skip()
		}
		return failure(err)
	}
	snapshot, err := decodeStoredSnapshot(row.CommittedJSON)
	if err != nil {
		return failure(err)
	}
	var artifact openapi.ModelArtifact
	if err := json.Unmarshal(snapshot.Resource, &artifact); err != nil {
		return failure(err)
	}
	if derefString(artifact.Name) != "primary" || derefString(artifact.Uri) != history.Source {
		return skip()
	}
	var links []ArtifactLink
	if err := db.Where("artifact_id = ?", row.ID).Find(&links).Error; err != nil {
		return failure(err)
	}
	if len(links) != 1 || links[0].VersionID != version.ID {
		return skip()
	}
	var reserved int64
	if err := db.Model(&IntentResource{}).Where("resource_id IN ?", []int32{row.ID, version.ID, version.ParentID}).Count(&reserved).Error; err != nil {
		return failure(err)
	}
	if reserved > 0 {
		return skip()
	}
	var active []Operation
	if err := db.Where("state IN ?", []OperationState{OperationPending, OperationRunning}).Find(&active).Error; err != nil {
		return failure(err)
	}
	for _, operation := range active {
		if operation.ResourceID == row.ID || operation.ResourceID == version.ID || operation.ResourceID == version.ParentID {
			return skip()
		}
		candidate, err := decodeMutationCandidate(operation.CandidateJSON)
		if err != nil {
			return skip()
		}
		for _, id := range candidateResourceIDs(operation.ResourceID, candidate) {
			if id == row.ID || id == version.ID {
				return skip()
			}
		}
	}
	return row, snapshot, true, nil
}
