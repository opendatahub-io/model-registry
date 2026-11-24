package mlflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"

	"github.com/google/uuid"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
)

// Operation kinds driven by ApplyOperation.
const (
	OperationKindModelCreate   = "model-create"
	OperationKindModelUpdate   = "model-update"
	OperationKindVersionCreate = "version-create"
	OperationKindVersionUpdate = "version-update"
)

// Creation tags stamped on every native object the adapter creates.
// They let an uncertain create reconcile by exact name and ownership:
// a native model carrying this adapter's UUID and logical ID is ours;
// anything else is an unrelated model the adapter never overwrites or
// adopts.
const (
	tagAdapterUUID = "rhoai.adapter"
	tagLogicalID   = "rhoai.logicalId"
	tagOperation   = "rhoai.operation"
)

// creationTagKeys lists the adapter-owned creation tag keys.
var creationTagKeys = []string{tagAdapterUUID, tagLogicalID, tagOperation}

// adapterIdentity loads the adapter singleton the service acts for.
func (s *Service) adapterIdentity(ctx context.Context) (Adapter, error) {
	adapter, err := s.store.GetAdapter(ctx)
	if err != nil {
		return Adapter{}, fmt.Errorf("mlflow adapter identity is not initialized: %w", err)
	}
	return adapter, nil
}

// payloadHash returns the content hash of an immutable journal payload.
func payloadHash(candidateJSON string) string {
	digest := sha256.Sum256([]byte(candidateJSON))
	return hex.EncodeToString(digest[:])
}

// customTagKeys maps original custom-property keys to their projected
// tag keys so the SQL snapshot retains the mapping for later refresh.
func customTagKeys(properties map[string]openapi.MetadataValue) map[string]string {
	if len(properties) == 0 {
		return nil
	}
	keys := make(map[string]string, len(properties))
	for key := range properties {
		keys[key] = EscapeTagKey(key)
	}
	return keys
}

// carryCreationTags preserves the creation tags of a previous owned-tag
// projection through a metadata update, so later diffs never delete the
// adapter's ownership markers.
func carryCreationTags(owned, previous map[string]string) map[string]string {
	for _, key := range creationTagKeys {
		if value, ok := previous[key]; ok {
			if owned == nil {
				owned = map[string]string{}
			}
			owned[key] = value
		}
	}
	return owned
}

// marshalResource renders the public resource with server-owned fields
// cleared, ready to become a stored snapshot's Resource payload.
func marshalResource(resource any) (json.RawMessage, error) {
	encoded, err := json.Marshal(resource)
	if err != nil {
		return nil, fmt.Errorf("serializing resource snapshot: %w: %w", err, api.ErrBadRequest)
	}
	return encoded, nil
}

// modelResource clears server-owned fields from a registered model.
func modelResource(model *openapi.RegisteredModel) *openapi.RegisteredModel {
	stored := *model
	stored.Id = nil
	stored.CreateTimeSinceEpoch = nil
	stored.LastUpdateTimeSinceEpoch = nil
	return &stored
}

// versionResource clears server-owned fields from a model version.
func versionResource(version *openapi.ModelVersion) *openapi.ModelVersion {
	stored := *version
	stored.Id = nil
	stored.CreateTimeSinceEpoch = nil
	stored.LastUpdateTimeSinceEpoch = nil
	return &stored
}

// modelView reconstructs the public registered model from a committed
// object row. Only the snapshot's resource JSON is exposed; private
// projection fields never enter the response.
func modelView(object Object) (*openapi.RegisteredModel, error) {
	if object.Kind != KindRegisteredModel {
		return nil, fmt.Errorf("object %d is a %s, not a registered model: %w",
			object.ID, object.Kind, api.ErrBadRequest)
	}
	public, err := DecodePublicSnapshot(object.CommittedJSON)
	if err != nil {
		return nil, err
	}
	var model openapi.RegisteredModel
	if err := json.Unmarshal(public, &model); err != nil {
		return nil, fmt.Errorf("decoding registered model %d snapshot: %w", object.ID, api.ErrBadRequest)
	}
	model.Id = new(strconv.FormatInt(int64(object.ID), 10))
	model.CreateTimeSinceEpoch = new(strconv.FormatInt(object.CreateTimeSinceEpoch, 10))
	model.LastUpdateTimeSinceEpoch = new(strconv.FormatInt(object.LastUpdateTimeSinceEpoch, 10))
	return &model, nil
}

// versionView reconstructs the public model version from a committed
// object row.
func versionView(object Object) (*openapi.ModelVersion, error) {
	if object.Kind != KindModelVersion {
		return nil, fmt.Errorf("object %d is a %s, not a model version: %w",
			object.ID, object.Kind, api.ErrBadRequest)
	}
	public, err := DecodePublicSnapshot(object.CommittedJSON)
	if err != nil {
		return nil, err
	}
	var version openapi.ModelVersion
	if err := json.Unmarshal(public, &version); err != nil {
		return nil, fmt.Errorf("decoding model version %d snapshot: %w", object.ID, api.ErrBadRequest)
	}
	version.Id = new(strconv.FormatInt(int64(object.ID), 10))
	version.RegisteredModelId = strconv.FormatInt(int64(object.ParentID), 10)
	version.CreateTimeSinceEpoch = new(strconv.FormatInt(object.CreateTimeSinceEpoch, 10))
	version.LastUpdateTimeSinceEpoch = new(strconv.FormatInt(object.LastUpdateTimeSinceEpoch, 10))
	return &version, nil
}

// prepareCandidate serializes a mutation candidate into an operation
// intent. The payload is immutable once prepared.
func prepareCandidate(ctx context.Context, store *Store, operationUUID string,
	resourceID int32, kind string, revision int64, generation int64, candidate MutationCandidate) (Operation, error) {
	if err := store.captureCandidateReads(ctx, &candidate); err != nil {
		return Operation{}, err
	}
	candidateJSON, err := json.Marshal(candidate)
	if err != nil {
		return Operation{}, fmt.Errorf("serializing %s candidate: %w: %w", kind, err, api.ErrBadRequest)
	}
	return store.PrepareOperation(ctx, Operation{
		OperationUUID:    operationUUID,
		ResourceID:       resourceID,
		Kind:             kind,
		ExpectedRevision: revision,
		CandidateJSON:    string(candidateJSON),
		PayloadHash:      payloadHash(string(candidateJSON)),
		Generation:       generation,
	})
}

// UpsertRegisteredModel creates or updates a registered model. Creation
// follows reserved identity → immutable intent → native operation →
// committed publication: the logical row is reserved invisibly, the
// native create runs under the journal with creation tags identifying
// adapter, operation, and logical ID, and the committed view appears
// only after SQL commit. Updates synchronize native description and
// tags, then publish the new snapshot; the prior committed view is
// preserved on any rejected write.
func (s *Service) UpsertRegisteredModel(registeredModel *openapi.RegisteredModel) (*openapi.RegisteredModel, error) {
	ctx := s.ctx

	if registeredModel.Id != nil {
		id, err := parseResourceID(*registeredModel.Id)
		if err != nil {
			return nil, fmt.Errorf("registered model id %q is not a valid id: %w",
				*registeredModel.Id, api.ErrBadRequest)
		}
		existing, err := s.store.GetObject(ctx, id)
		if err != nil {
			return nil, err
		}
		// An update identifies the resource by id; an absent name keeps
		// the stored one, and a supplied name is advisory only.
		if registeredModel.Name == "" {
			updated := *registeredModel
			updated.Name = existing.Name
			registeredModel = &updated
		}
		return s.updateRegisteredModel(ctx, existing, registeredModel)
	}

	if existing, err := s.store.FindObject(ctx, KindRegisteredModel,
		&registeredModel.Name, nil, nil); err == nil {
		return nil, fmt.Errorf("registered model %q already exists as id %d: %w",
			registeredModel.Name, existing.ID, api.ErrConflict)
	} else if !errors.Is(err, api.ErrNotFound) {
		return nil, err
	}

	tags, err := EncodeModel(registeredModel)
	if err != nil {
		return nil, err
	}
	return s.createRegisteredModel(ctx, registeredModel, tags)
}

// createRegisteredModel reserves the logical identity invisibly and
// drives the native create through the journal.
func (s *Service) createRegisteredModel(ctx context.Context, model *openapi.RegisteredModel,
	tags map[string]string) (*openapi.RegisteredModel, error) {
	adapter, err := s.adapterIdentity(ctx)
	if err != nil {
		return nil, err
	}

	// The row stays invisible until publication, so a rejected native
	// create never appears as a successful resource.
	reserved, err := s.reserveResource(ctx, Object{
		Kind: KindRegisteredModel, Name: model.Name, ExternalID: model.ExternalId,
	})
	if err != nil {
		return nil, err
	}

	operationUUID := uuid.NewString()
	owned := maps.Clone(tags)
	if owned == nil {
		owned = map[string]string{}
	}
	owned[tagAdapterUUID] = adapter.AdapterUUID
	owned[tagLogicalID] = strconv.FormatInt(int64(reserved.ID), 10)
	owned[tagOperation] = operationUUID

	resource, err := marshalResource(modelResource(model))
	if err != nil {
		return nil, err
	}
	candidate := MutationCandidate{
		CodecVersion: candidateCodecVersion,
		Objects: []ObjectChange{{
			ID: reserved.ID, ExpectedRevision: reserved.Revision,
			Snapshot: StoredSnapshot{
				CodecVersion: candidateCodecVersion, Kind: KindRegisteredModel,
				Resource: resource, TagKeys: customTagKeys(model.CustomProperties),
				OwnedTags: owned,
			},
		}},
	}
	published, err := s.driveCreate(ctx, reserved, func() (Operation, error) {
		return prepareCandidate(ctx, s.store, operationUUID, reserved.ID,
			OperationKindModelCreate, reserved.Revision, 1, candidate)
	})
	if err != nil {
		return nil, err
	}
	return modelView(published)
}

// updateRegisteredModel publishes a metadata edit. The name is a
// protected identity and cannot change once set, and the edit is
// partial: omitted fields keep their committed values.
func (s *Service) updateRegisteredModel(ctx context.Context, existing Object,
	model *openapi.RegisteredModel) (*openapi.RegisteredModel, error) {
	if existing.Kind != KindRegisteredModel {
		return nil, fmt.Errorf("object %d is a %s, not a registered model: %w",
			existing.ID, existing.Kind, api.ErrBadRequest)
	}
	// An update is partial: fields the request omits keep their prior
	// committed values, and a supplied custom-property map replaces the
	// prior map. The name is a protected identity and never changes.
	previous, err := decodeStoredSnapshot(existing.CommittedJSON)
	if err != nil {
		return nil, err
	}
	var prior openapi.RegisteredModel
	if len(previous.Resource) > 0 {
		if err := decodeInto(previous.Resource, &prior); err != nil {
			return nil, err
		}
	}
	prior.Name = existing.Name
	model = mergeRegisteredModelUpdate(&prior, model)

	tags, err := EncodeModel(model)
	if err != nil {
		return nil, err
	}
	owned := carryCreationTags(tags, previous.OwnedTags)

	resource, err := marshalResource(modelResource(model))
	if err != nil {
		return nil, err
	}
	candidate := MutationCandidate{
		CodecVersion: candidateCodecVersion,
		Objects: []ObjectChange{{
			ID: existing.ID, ExpectedRevision: existing.Revision,
			Snapshot: StoredSnapshot{
				CodecVersion: candidateCodecVersion, Kind: KindRegisteredModel,
				Resource: resource, TagKeys: customTagKeys(model.CustomProperties),
				OwnedTags: owned,
			},
		}},
	}
	operation, err := prepareCandidate(ctx, s.store, uuid.NewString(), existing.ID,
		OperationKindModelUpdate, existing.Revision, 1, candidate)
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
	return modelView(updated)
}

// mergeRegisteredModelUpdate overlays one partial update onto the prior
// committed resource. Nil pointer fields and absent maps keep their
// prior values; a supplied map replaces the prior map wholesale.
func mergeRegisteredModelUpdate(prior, update *openapi.RegisteredModel) *openapi.RegisteredModel {
	merged := *prior
	merged.Name = prior.Name
	if update.Description != nil {
		merged.Description = update.Description
	}
	if update.Owner != nil {
		merged.Owner = update.Owner
	}
	if update.ExternalId != nil {
		merged.ExternalId = update.ExternalId
	}
	if update.State != nil {
		merged.State = update.State
	}
	if update.Readme != nil {
		merged.Readme = update.Readme
	}
	if update.Maturity != nil {
		merged.Maturity = update.Maturity
	}
	if update.Provider != nil {
		merged.Provider = update.Provider
	}
	if update.Logo != nil {
		merged.Logo = update.Logo
	}
	if update.License != nil {
		merged.License = update.License
	}
	if update.LicenseLink != nil {
		merged.LicenseLink = update.LicenseLink
	}
	if update.LibraryName != nil {
		merged.LibraryName = update.LibraryName
	}
	if update.Language != nil {
		merged.Language = update.Language
	}
	if update.Tasks != nil {
		merged.Tasks = update.Tasks
	}
	if update.CustomProperties != nil {
		merged.CustomProperties = update.CustomProperties
	}
	return &merged
}

// decodeStoredSnapshot decodes a committed stored snapshot, refusing
// unknown codec versions. A reservation placeholder carries no
// projection and decodes as an empty snapshot.
func decodeStoredSnapshot(committedJSON string) (StoredSnapshot, error) {
	if committedJSON == "" || committedJSON == "{}" {
		return StoredSnapshot{CodecVersion: candidateCodecVersion}, nil
	}
	var probe struct {
		CodecVersion *int `json:"codecVersion"`
	}
	if err := json.Unmarshal([]byte(committedJSON), &probe); err != nil {
		return StoredSnapshot{}, fmt.Errorf("invalid committed snapshot: %w", api.ErrBadRequest)
	}
	if probe.CodecVersion == nil {
		return StoredSnapshot{Resource: json.RawMessage(committedJSON)}, nil
	}
	if !supportedSnapshotCodec(*probe.CodecVersion) {
		return StoredSnapshot{}, fmt.Errorf("committed snapshot codec version %d is not supported: %w",
			*probe.CodecVersion, api.ErrBadRequest)
	}
	var snapshot StoredSnapshot
	if err := json.Unmarshal([]byte(committedJSON), &snapshot); err != nil {
		return StoredSnapshot{}, fmt.Errorf("decoding stored snapshot: %w: %w", err, api.ErrBadRequest)
	}
	return snapshot, nil
}

// GetRegisteredModelById retrieves a registered model by its REST id.
func (s *Service) GetRegisteredModelById(id string) (*openapi.RegisteredModel, error) {
	parsed, err := parseResourceID(id)
	if err != nil {
		return nil, fmt.Errorf("registered model id %q is not a valid id: %w", id, api.ErrBadRequest)
	}
	object, err := s.store.GetObject(s.ctx, parsed)
	if err != nil {
		return nil, err
	}
	return modelView(object)
}
