package mlflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/kubeflow/hub/internal/converter"
	"github.com/kubeflow/hub/internal/converter/generated"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"gorm.io/gorm"
)

// Operation kinds for artifact work.
const (
	OperationKindVersionMaterialize = "version-materialize"
	OperationKindArtifactAttach     = "artifact-attach"
	OperationKindArtifactUpdate     = "artifact-update"
)

// artifactResource clears server-owned fields from a model artifact.
func artifactResource(artifact *openapi.ModelArtifact) *openapi.ModelArtifact {
	stored := *artifact
	stored.Id = nil
	stored.CreateTimeSinceEpoch = nil
	stored.LastUpdateTimeSinceEpoch = nil
	// The create/update converters deliberately ignore ArtifactType (the
	// union variant carries it), so the stored record names its own type.
	// Clients also need it as the response discriminator: only model
	// artifacts exist in this backend, so the default is unambiguous.
	if stored.ArtifactType == nil {
		stored.ArtifactType = new("model-artifact")
	}
	return &stored
}

// artifactView reconstructs the public model artifact from a committed
// object row.
func artifactView(object Object) (*openapi.ModelArtifact, error) {
	if object.Kind != KindModelArtifact {
		return nil, fmt.Errorf("object %d is a %s, not a model artifact: %w",
			object.ID, object.Kind, api.ErrBadRequest)
	}
	public, err := DecodePublicSnapshot(object.CommittedJSON)
	if err != nil {
		return nil, err
	}
	var artifact openapi.ModelArtifact
	if err := json.Unmarshal(public, &artifact); err != nil {
		return nil, fmt.Errorf("decoding model artifact %d snapshot: %w", object.ID, api.ErrBadRequest)
	}
	artifact.Id = new(strconv.FormatInt(int64(object.ID), 10))
	artifact.CreateTimeSinceEpoch = new(strconv.FormatInt(object.CreateTimeSinceEpoch, 10))
	artifact.LastUpdateTimeSinceEpoch = new(strconv.FormatInt(object.LastUpdateTimeSinceEpoch, 10))
	return &artifact, nil
}

// deterministicArtifactName synthesizes a stable name for a nameless
// artifact from its content, so identical retries converge on one
// identity instead of accumulating rows.
func deterministicArtifactName(artifact *openapi.ModelArtifact) string {
	digest := sha256.Sum256(fmt.Appendf(nil, "%s|%s", derefString(artifact.Name), derefString(artifact.Uri)))
	return "artifact-" + hex.EncodeToString(digest[:8])
}

// UpsertModelArtifact creates or updates a standalone model artifact.
// A standalone artifact has no version attachment and no native
// projection; it commits in SQL only.
func (s *Service) UpsertModelArtifact(modelArtifact *openapi.ModelArtifact) (*openapi.ModelArtifact, error) {
	ctx := s.ctx
	if modelArtifact == nil {
		return nil, fmt.Errorf("model artifact is required: %w", api.ErrBadRequest)
	}
	if _, err := EncodeArtifact(modelArtifact, true); err != nil {
		return nil, err
	}
	if modelArtifact.Id != nil {
		id, err := parseResourceID(*modelArtifact.Id)
		if err != nil {
			return nil, fmt.Errorf("model artifact id %q is not a valid id: %w",
				*modelArtifact.Id, api.ErrBadRequest)
		}
		existing, err := s.store.GetObject(ctx, id)
		if err != nil {
			return nil, err
		}
		return s.updateArtifact(ctx, existing, modelArtifact)
	}

	name := derefString(modelArtifact.Name)
	if name == "" {
		name = *converter.GenerateNewName()
		named := *modelArtifact
		named.Name = new(name)
		modelArtifact = &named
	}
	if _, err := s.store.FindStandaloneArtifact(ctx, name); err == nil {
		return nil, fmt.Errorf("model artifact %q already exists: %w", name, api.ErrConflict)
	} else if !errors.Is(err, api.ErrNotFound) {
		return nil, err
	}

	reserved, err := s.reserveArtifact(ctx, Object{
		Kind: KindModelArtifact, Name: name, ExternalID: modelArtifact.ExternalId,
	}, modelArtifact)
	if err != nil {
		return nil, err
	}
	published, err := s.driveCreate(ctx, reserved, func() (Operation, error) {
		return prepareCandidate(ctx, s.store, uuid.NewString(), reserved.ID,
			OperationKindArtifactAttach, reserved.Revision, 1, s.artifactCandidate(reserved, modelArtifact, nil, nil))
	})
	if err != nil {
		return nil, err
	}
	return artifactView(published)
}

// artifactCandidate builds the mutation candidate committing one
// artifact, optionally linked to a version and optionally carrying a
// binding and history change for materialization.
func (s *Service) artifactCandidate(reserved Object, artifact *openapi.ModelArtifact,
	link *ArtifactLink, materialization *BindingChange) MutationCandidate {
	tags, _ := EncodeArtifact(artifact, link != nil && materialization == nil)
	// The owned-tag projection for a standalone artifact is empty: no
	// native object carries its tags.
	if link == nil {
		tags = nil
	}
	candidate := MutationCandidate{
		CodecVersion: candidateCodecVersion,
		Objects: []ObjectChange{{
			ID: reserved.ID, ExpectedRevision: reserved.Revision,
			Snapshot: StoredSnapshot{
				CodecVersion: candidateCodecVersion, Kind: KindModelArtifact,
				Resource:  mustMarshal(artifactResource(artifact)),
				TagKeys:   customTagKeys(artifact.CustomProperties),
				OwnedTags: tags,
			},
		}},
	}
	if link != nil {
		candidate.Links = []ArtifactLink{*link}
	}
	if materialization != nil {
		candidate.Bindings = []BindingChange{*materialization}
	}
	return candidate
}

func mustMarshal(resource any) []byte {
	encoded, err := json.Marshal(resource)
	if err != nil {
		panic(fmt.Sprintf("serializing resource snapshot: %v", err))
	}
	return encoded
}

// GetModelArtifactById retrieves a model artifact by its REST id.
func (s *Service) GetModelArtifactById(id string) (*openapi.ModelArtifact, error) {
	parsed, err := parseResourceID(id)
	if err != nil {
		return nil, fmt.Errorf("model artifact id %q is not a valid id: %w", id, api.ErrBadRequest)
	}
	object, err := s.store.GetObject(s.ctx, parsed)
	if err != nil {
		return nil, err
	}
	return artifactView(object)
}

// GetArtifactById retrieves an artifact by its REST id. Only model
// artifacts exist in this backend; other artifact unions are
// unsupported.
func (s *Service) GetArtifactById(id string) (*openapi.Artifact, error) {
	artifact, err := s.GetModelArtifactById(id)
	if err != nil {
		return nil, err
	}
	return &openapi.Artifact{ModelArtifact: artifact}, nil
}

// UpsertArtifact creates or updates a standalone artifact. Only model
// artifacts are supported by this backend.
func (s *Service) UpsertArtifact(artifact *openapi.Artifact) (*openapi.Artifact, error) {
	if artifact == nil || artifact.ModelArtifact == nil {
		return nil, fmt.Errorf("only model artifacts are supported by the mlflow backend: %w",
			errUnsupportedResource(artifactKind(artifact)))
	}
	created, err := s.UpsertModelArtifact(artifact.ModelArtifact)
	if err != nil {
		return nil, err
	}
	return &openapi.Artifact{ModelArtifact: created}, nil
}

func artifactKind(artifact *openapi.Artifact) string {
	switch {
	case artifact == nil:
		return "artifact"
	case artifact.DataSet != nil:
		return "data-set"
	case artifact.DocArtifact != nil:
		return "doc-artifact"
	case artifact.Metric != nil:
		return "metric"
	case artifact.Parameter != nil:
		return "parameter"
	default:
		return "model-artifact"
	}
}

// UpsertModelVersionArtifact attaches a model artifact to a pending or
// materialized model version. The first artifact carrying a nonempty
// source becomes the primary artifact and materializes the version —
// a native model version is created from the real source under the
// journal, and the artifact, link, binding, and history commit
// atomically only after the native create succeeds. A source-less
// attachment leaves the version pending. Additional artifacts project
// under their ID namespace and never replace the primary.
func (s *Service) UpsertModelVersionArtifact(artifact *openapi.Artifact,
	modelVersionId string) (*openapi.Artifact, error) {
	ctx := s.ctx
	if artifact == nil || artifact.ModelArtifact == nil {
		return nil, fmt.Errorf("only model artifacts are supported by the mlflow backend: %w",
			errUnsupportedResource(artifactKind(artifact)))
	}
	modelArtifact := artifact.ModelArtifact
	if _, err := EncodeArtifact(modelArtifact, true); err != nil {
		return nil, err
	}
	versionID, err := parseResourceID(modelVersionId)
	if err != nil {
		return nil, fmt.Errorf("model version id %q is not a valid id: %w", modelVersionId, api.ErrBadRequest)
	}
	version, err := s.store.GetObject(ctx, versionID)
	if err != nil {
		return nil, err
	}
	if version.Kind != KindModelVersion {
		return nil, fmt.Errorf("object %d is a %s, not a model version: %w",
			version.ID, version.Kind, api.ErrBadRequest)
	}
	model, err := s.store.GetObject(ctx, version.ParentID)
	if err != nil {
		return nil, err
	}

	// An update to an already-attached artifact edits its metadata.
	// Artifacts are parentless; their version relationships live in the
	// artifact-link table, so a name match is validated against the
	// link before it is treated as this version's artifact.
	if modelArtifact.Id != nil {
		id, err := parseResourceID(*modelArtifact.Id)
		if err != nil {
			return nil, fmt.Errorf("model artifact id %q is not a valid id: %w",
				*modelArtifact.Id, api.ErrBadRequest)
		}
		existing, err := s.store.GetObject(ctx, id)
		if err != nil {
			return nil, err
		}
		modelArtifact, err = mergeCommittedArtifact(existing, modelArtifact)
		if err != nil {
			return nil, err
		}
		if s.artifactLinkedTo(ctx, existing.ID, version.ID) {
			return s.updateAttachedArtifact(ctx, version, existing, modelArtifact)
		}
		return s.attachExistingArtifact(ctx, model, version, existing, modelArtifact)
	}

	name := derefString(modelArtifact.Name)
	if name == "" {
		// A nameless artifact gets a stable generated name, committed
		// like any client-supplied name.
		name = deterministicArtifactName(modelArtifact)
		named := *modelArtifact
		named.Name = new(name)
		modelArtifact = &named
	}
	if existing, err := s.store.FindArtifactInVersion(ctx, name, versionID); err == nil {
		if err := s.identicalArtifactAttachment(ctx, existing, versionID, modelArtifact); err != nil {
			return nil, err
		}
		view, err := artifactView(existing)
		if err != nil {
			return nil, err
		}
		return &openapi.Artifact{ModelArtifact: view}, nil
	} else if !errors.Is(err, api.ErrNotFound) {
		return nil, err
	}

	// Artifacts are parentless: their zero-to-many version
	// relationships live in MLflowArtifactLink, committed with the
	// candidate.
	reserved, err := s.reserveArtifact(ctx, Object{
		Kind: KindModelArtifact, Name: name, ArtifactScopeID: versionID,
		ExternalID: modelArtifact.ExternalId,
	}, modelArtifact)
	if err != nil {
		return nil, err
	}

	var binding VersionBinding
	bound := s.dbBinding(ctx, versionID, &binding)
	hasSource := derefString(modelArtifact.Uri) != ""
	isPrimary := !bound || (binding.PrimaryArtifactID == nil && hasSource)

	if isPrimary && hasSource {
		if bound && binding.NativeModelName != nil {
			return nil, fmt.Errorf("model version %d is already materialized; source replacement is required to change the primary source: %w",
				versionID, api.ErrConflict)
		}
		return s.materializeVersion(ctx, model, version, reserved, modelArtifact, binding)
	}

	// An additional or source-less attachment commits in SQL, and —
	// when the version is already bound — projects its tags onto the
	// native model version under its ID namespace.
	link := ArtifactLink{ArtifactID: reserved.ID, VersionID: versionID}
	candidate := s.artifactCandidate(reserved, modelArtifact, &link, nil)
	if bound && binding.NativeModelName != nil {
		candidate.Objects[0].Snapshot.OwnedTags = qualifiedArtifactTags(reserved.ID, modelArtifact)
	}
	published, err := s.driveCreate(ctx, reserved, func() (Operation, error) {
		return prepareCandidate(ctx, s.store, uuid.NewString(), reserved.ID,
			OperationKindArtifactAttach, reserved.Revision, 1, candidate)
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

// qualifiedArtifactTags projects an additional artifact's tags under
// its ID namespace on the native model version. The codec returns the
// keys relative to the shared "rhoai.artifacts." namespace; the service
// qualifies them with the artifact ID so concurrent additional
// artifacts never collide.
func qualifiedArtifactTags(artifactID int32, artifact *openapi.ModelArtifact) map[string]string {
	relative, err := EncodeArtifact(artifact, false)
	if err != nil {
		return nil
	}
	qualified := make(map[string]string, len(relative))
	for key, value := range relative {
		trimmed := strings.TrimPrefix(key, additionalTagPrefix)
		qualified[fmt.Sprintf("%s%d.%s", additionalTagPrefix, artifactID, trimmed)] = value
	}
	return qualified
}

// legacyQualifiedArtifactTags reproduces the pre-v4 ( codec 2/3 )
// additional-artifact projection under the artifact's ID namespace,
// where mirrored fields, custom properties, and direct field keys were
// flattened onto bare suffixes. It exists only to diff and retire keys
// written by stored legacy snapshots and to replay recorded legacy
// work verbatim; new writes never produce these keys.
func legacyQualifiedArtifactTags(artifactID int32, artifact *openapi.ModelArtifact) map[string]string {
	primary, err := EncodeArtifact(artifact, true)
	if err != nil {
		return nil
	}
	qualified := make(map[string]string, len(primary))
	for key, value := range legacyQualifyAdditionalArtifactTags(primary) {
		trimmed := strings.TrimPrefix(key, additionalTagPrefix)
		qualified[fmt.Sprintf("%s%d.%s", additionalTagPrefix, artifactID, trimmed)] = value
	}
	return qualified
}

// attachExistingArtifact attaches an already-committed artifact to
// another version: as that version's materialized primary when the
// version is pending and the artifact carries its committed source, or
// as an additional artifact when the version is already materialized.
// A shared primary is the root of the replacement fan-out.
func (s *Service) attachExistingArtifact(ctx context.Context, model, version, existing Object,
	artifact *openapi.ModelArtifact) (*openapi.Artifact, error) {
	var binding VersionBinding
	bound := s.dbBinding(ctx, version.ID, &binding)
	hasSource := derefString(artifact.Uri) != ""
	if (!bound || binding.PrimaryArtifactID == nil) && hasSource && existing.Visibility == VisibilityCommitted {
		// A shared primary never crosses models: every linked version
		// must belong to the version's own model, or a replacement
		// fan-out could not switch all bindings under one native model.
		var linkedVersions []int32
		if err := s.store.DB().WithContext(ctx).Model(&ArtifactLink{}).
			Where("artifact_id = ?", existing.ID).Distinct().Pluck("version_id", &linkedVersions).Error; err != nil {
			return nil, mapDBError(err)
		}
		for _, linkedID := range linkedVersions {
			if linkedID == version.ID {
				continue
			}
			linked, err := s.store.GetObject(ctx, linkedID)
			if err != nil {
				return nil, err
			}
			if linked.ParentID != model.ID {
				return nil, fmt.Errorf("model artifact %d is the primary of a version in another model; it cannot be shared across models: %w",
					existing.ID, api.ErrConflict)
			}
		}
		prior, err := decodeStoredSnapshot(existing.CommittedJSON)
		if err != nil {
			return nil, err
		}
		var priorArtifact openapi.ModelArtifact
		if err := json.Unmarshal(prior.Resource, &priorArtifact); err != nil {
			return nil, fmt.Errorf("decoding model artifact %d snapshot: %w", existing.ID, api.ErrBadRequest)
		}
		if derefString(artifact.Uri) != derefString(priorArtifact.Uri) {
			return nil, fmt.Errorf("model artifact %d source differs from the requested attachment; replace it through a linked version: %w",
				existing.ID, api.ErrConflict)
		}
		// The version adopts the shared artifact as its primary and
		// materializes from its committed source.
		return s.materializeVersion(ctx, model, version, existing, artifact, binding)
	}

	// An additional attachment of an existing artifact links it and
	// projects its tags under its ID namespace once the version is
	// bound.
	link := ArtifactLink{ArtifactID: existing.ID, VersionID: version.ID}
	candidate := s.artifactCandidate(existing, artifact, &link, nil)
	if bound && binding.NativeModelName != nil {
		candidate.Objects[0].Snapshot.OwnedTags = qualifiedArtifactTags(existing.ID, artifact)
	}
	published, err := s.driveCreate(ctx, existing, func() (Operation, error) {
		return prepareCandidate(ctx, s.store, uuid.NewString(), existing.ID,
			OperationKindArtifactAttach, existing.Revision, 1, candidate)
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

// updateArtifact publishes a standalone artifact metadata edit in SQL.
func (s *Service) updateArtifact(ctx context.Context, existing Object,
	artifact *openapi.ModelArtifact) (*openapi.ModelArtifact, error) {
	if existing.Kind != KindModelArtifact {
		return nil, fmt.Errorf("object %d is a %s, not a model artifact: %w",
			existing.ID, existing.Kind, api.ErrBadRequest)
	}
	artifact, err := mergeCommittedArtifact(existing, artifact)
	if err != nil {
		return nil, err
	}
	if versionID, linked := s.linkedVersionID(ctx, existing.ID); linked {
		version, err := s.store.GetObject(ctx, versionID)
		if err != nil {
			return nil, err
		}
		updated, err := s.updateAttachedArtifact(ctx, version, existing, artifact)
		if err != nil {
			return nil, err
		}
		return updated.ModelArtifact, nil
	}

	tags, err := EncodeArtifact(artifact, true)
	if err != nil {
		return nil, err
	}
	_ = tags // standalone artifacts carry no native projection
	candidate := MutationCandidate{
		CodecVersion: candidateCodecVersion,
		Objects: []ObjectChange{{
			ID: existing.ID, ExpectedRevision: existing.Revision,
			Snapshot: StoredSnapshot{
				CodecVersion: candidateCodecVersion, Kind: KindModelArtifact,
				Resource:  mustMarshal(artifactResource(artifact)),
				TagKeys:   customTagKeys(artifact.CustomProperties),
				OwnedTags: nil,
			},
		}},
	}
	operation, err := prepareCandidate(ctx, s.store, uuid.NewString(), existing.ID,
		OperationKindArtifactUpdate, existing.Revision, 1, candidate)
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
	return artifactView(updated)
}

// updateAttachedArtifact edits an attached artifact's metadata. The
// primary source of a materialized version cannot be cleared or
// changed here; replacement is a separate operation.
func (s *Service) updateAttachedArtifact(ctx context.Context, version, existing Object,
	artifact *openapi.ModelArtifact) (*openapi.Artifact, error) {
	if existing.Kind != KindModelArtifact {
		return nil, fmt.Errorf("object %d is a %s, not a model artifact: %w",
			existing.ID, existing.Kind, api.ErrBadRequest)
	}
	if !s.artifactLinkedTo(ctx, existing.ID, version.ID) {
		return nil, fmt.Errorf("model artifact %d is not attached to version %d: %w",
			existing.ID, version.ID, api.ErrBadRequest)
	}
	artifact, err := mergeCommittedArtifact(existing, artifact)
	if err != nil {
		return nil, err
	}

	var binding VersionBinding
	bound := s.dbBinding(ctx, version.ID, &binding)
	isPrimary := bound && binding.PrimaryArtifactID != nil && *binding.PrimaryArtifactID == existing.ID
	if isPrimary && bound && binding.NativeModelName != nil {
		previous, err := decodeStoredSnapshot(existing.CommittedJSON)
		if err != nil {
			return nil, err
		}
		var priorArtifact openapi.ModelArtifact
		if err := json.Unmarshal(previous.Resource, &priorArtifact); err != nil {
			return nil, fmt.Errorf("decoding model artifact %d snapshot: %w", existing.ID, api.ErrBadRequest)
		}
		if derefString(artifact.Uri) != derefString(priorArtifact.Uri) {
			// A changed primary source is a replacement: a new native
			// generation under the journal, never an in-place edit.
			return s.replacePrimarySource(ctx, version, existing, artifact, binding)
		}
	}

	var owned map[string]string
	if bound && binding.NativeModelName != nil {
		if isPrimary {
			owned, _ = EncodeArtifact(artifact, true)
		} else {
			owned = qualifiedArtifactTags(existing.ID, artifact)
		}
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
	}
	operation, err := prepareCandidate(ctx, s.store, uuid.NewString(), existing.ID,
		OperationKindArtifactUpdate, existing.Revision, 1, candidate)
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
	view, err := artifactView(updated)
	if err != nil {
		return nil, err
	}
	return &openapi.Artifact{ModelArtifact: view}, nil
}

// loadObjectRow loads an object row regardless of visibility. The
// journal's internal paths read reserved rows the public store hides.
func (s *Service) loadObjectRow(ctx context.Context, id int32) (Object, error) {
	var object Object
	err := s.store.DB().WithContext(ctx).
		Where("id = ? AND deleted = ?", id, false).First(&object).Error
	if err != nil {
		return Object{}, mapDBError(err)
	}
	return object, nil
}

// artifactLinkedTo reports whether an artifact is linked to a version.
func (s *Service) artifactLinkedTo(ctx context.Context, artifactID, versionID int32) bool {
	var count int64
	err := s.store.DB().WithContext(ctx).Model(&ArtifactLink{}).
		Where("artifact_id = ? AND version_id = ?", artifactID, versionID).
		Count(&count).Error
	return err == nil && count > 0
}

// linkedVersionID returns the first version an artifact is linked to.
func (s *Service) linkedVersionID(ctx context.Context, artifactID int32) (int32, bool) {
	var link ArtifactLink
	err := s.store.DB().WithContext(ctx).
		Where("artifact_id = ?", artifactID).First(&link).Error
	if err != nil {
		return 0, false
	}
	return link.VersionID, true
}

// applyArtifactProjection publishes an artifact attach or metadata
// edit. The artifact projects onto every linked, natively bound
// version — in ascending version-ID order — because a shared artifact
// plays a role in each binding's namespace and a single recorded
// projection cannot represent them all. Each binding synchronizes its
// own tag diff first — deleted keys removed, changed keys set — and the
// SQL publication follows exactly once after every projection
// succeeds; a pending version's artifact work stays in SQL only. Every
// native write is idempotent, so an interrupted operation re-runs
// safely.
func (s *Service) applyArtifactProjection(ctx context.Context, claimed Operation) error {
	candidate, _, err := decodeOperationResource(claimed)
	if err != nil {
		return err
	}
	change := candidate.Objects[0]

	// The artifact row identifies its versions: through the candidate's
	// links on attach, plus every committed link. Deduplicate and order
	// them so the fan-out is deterministic.
	linked := map[int32]bool{}
	for _, link := range candidate.Links {
		linked[link.VersionID] = true
	}
	var links []ArtifactLink
	if err := s.store.DB().WithContext(ctx).Where("artifact_id = ?", claimed.ResourceID).
		Order("version_id").Find(&links).Error; err != nil {
		return mapDBError(err)
	}
	for _, link := range links {
		linked[link.VersionID] = true
	}
	if len(linked) == 0 {
		return s.store.PublishOperation(ctx, claimed.OperationUUID, claimed.FencingToken)
	}

	// The artifact row may still be reserved on its first publication;
	// read it without the public visibility filter.
	previous, err := s.loadObjectRow(ctx, claimed.ResourceID)
	if err != nil {
		return err
	}
	prior, err := artifactView(previous)
	if err != nil {
		return err
	}
	committed, err := decodeStoredSnapshot(previous.CommittedJSON)
	if err != nil {
		return err
	}
	var desired openapi.ModelArtifact
	if err := json.Unmarshal(change.Snapshot.Resource, &desired); err != nil {
		return fmt.Errorf("decoding candidate artifact %d snapshot: %w", claimed.ResourceID, api.ErrBadRequest)
	}

	// The recorded projection covers only the role it was prepared in.
	// Every binding derives its own role's projection from the same
	// immutable candidate resource; the recorded map is replayed
	// verbatim for the role it matches.
	recorded := change.Snapshot.OwnedTags
	recordedAdditional := false
	for key := range recorded {
		if strings.HasPrefix(key, additionalTagPrefix) {
			recordedAdditional = true
		}
	}

	// Codec 4 made the additional-artifact namespace injective. A legacy
	// candidate ( snapshot codec < 4 ) replays verbatim against the legacy
	// baseline, and a v4 write against a legacy committed snapshot also
	// diffs against the legacy baseline: the flattened keys the legacy
	// binary wrote — including a collision it already overwrote natively —
	// are retired, and every v4 key is ( re )written from the committed
	// SQL resource, which is the migration authority. Refresh never
	// diffs artifact tags, so an unmigrated legacy snapshot is never
	// treated as a deletion.
	legacyProjection := change.Snapshot.CodecVersion < 4 || committed.CodecVersion < 4

	for _, versionID := range sortedResourceIDs(linked) {
		var binding VersionBinding
		err := s.store.DB().WithContext(ctx).Where("version_id = ?", versionID).First(&binding).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue // a pending version has no native projection
		}
		if err != nil {
			return mapDBError(err)
		}
		if binding.NativeModelName == nil || binding.NativeVersionNumber == nil {
			continue
		}
		// The prepared graph captured every linked binding's revision.
		// A binding that changed since preparation — a concurrent
		// attachment, materialization, or source replacement — defers
		// to the newer operation rather than projecting onto stale
		// state or omitting it from the fan-out.
		if candidate.BindingReadRevisions != nil {
			captured, ok := candidate.BindingReadRevisions[versionID]
			if !ok || captured != binding.Revision {
				return fmt.Errorf("version %d binding changed since preparation: %w", versionID, api.ErrConflict)
			}
		}
		name, number := *binding.NativeModelName, *binding.NativeVersionNumber
		if err := s.verifyAliasParent(ctx, binding.ModelID, name); err != nil {
			return err
		}
		isPrimary := binding.PrimaryArtifactID != nil && *binding.PrimaryArtifactID == previous.ID

		// The same artifact can play different roles across versions;
		// older pending attachments also stored an unqualified
		// projection even though materialization now writes their
		// qualified tags. Diff the committed resource in this binding's
		// namespace, never delete another role's tags.
		var priorOwned map[string]string
		if s.artifactLinkedTo(ctx, previous.ID, versionID) {
			if isPrimary {
				priorOwned, err = EncodeArtifact(prior, true)
			} else if legacyProjection {
				priorOwned = legacyQualifiedArtifactTags(previous.ID, prior)
			} else {
				priorOwned = qualifiedArtifactTags(previous.ID, prior)
			}
			if err != nil {
				return err
			}
		}

		var owned map[string]string
		if isPrimary {
			if len(recorded) > 0 && !recordedAdditional {
				owned = recorded
			} else if owned, err = EncodeArtifact(&desired, true); err != nil {
				return err
			}
		} else if len(recorded) > 0 && recordedAdditional {
			owned = recorded
		} else if change.Snapshot.CodecVersion < 4 {
			owned = legacyQualifiedArtifactTags(previous.ID, &desired)
		} else {
			owned = qualifiedArtifactTags(previous.ID, &desired)
		}

		for _, key := range sortedKeys(priorOwned) {
			if _, kept := owned[key]; !kept {
				if err := s.native.DeleteModelVersionTag(ctx, name, number, key); err != nil {
					return MapClientError(err)
				}
			}
		}
		for _, key := range sortedKeys(owned) {
			if priorValue, exists := priorOwned[key]; !exists || priorValue != owned[key] {
				if err := s.native.SetModelVersionTag(ctx, name, number, key, owned[key]); err != nil {
					return MapClientError(err)
				}
			}
		}
	}
	return s.store.PublishOperation(ctx, claimed.OperationUUID, claimed.FencingToken)
}

// mergeCommittedArtifact applies the same partial-update and protected-field
// rules as the core backend before source selection or native projection.
func mergeCommittedArtifact(existing Object, update *openapi.ModelArtifact) (*openapi.ModelArtifact, error) {
	prior, err := artifactView(existing)
	if err != nil {
		return nil, err
	}
	merged, err := (&generated.OpenAPIReconcilerImpl{}).UpdateExistingModelArtifact(converter.NewOpenapiUpdateWrapper(prior, update))
	if err != nil {
		return nil, err
	}
	return &merged, nil
}

func normalizedArtifactCreate(artifact *openapi.ModelArtifact) []byte {
	normalized := artifactResource(artifact)
	if normalized.Uri == nil {
		normalized.Uri = new("")
	}
	if normalized.State == nil {
		normalized.State = new(openapi.ARTIFACTSTATE_UNKNOWN)
	}
	if normalized.ArtifactType == nil {
		normalized.ArtifactType = new("model-artifact")
	}
	return mustMarshal(normalized)
}

// identicalArtifactAttachment compares the original immutable intent, not an
// edited committed view. A name-only create must never mutate another create.
func (s *Service) identicalArtifactAttachment(ctx context.Context, existing Object, versionID int32, artifact *openapi.ModelArtifact) error {
	var operations []Operation
	if err := s.store.DB().WithContext(ctx).Where("resource_id = ? AND kind IN ?", existing.ID, []string{OperationKindArtifactAttach, OperationKindVersionMaterialize}).Order("expected_revision, operation_uuid").Find(&operations).Error; err != nil {
		return mapDBError(err)
	}
	for _, operation := range operations {
		candidate, err := decodeMutationCandidate(operation.CandidateJSON)
		if err != nil {
			return err
		}
		if candidate == nil {
			continue
		}
		for _, link := range candidate.Links {
			if link.ArtifactID != existing.ID || link.VersionID != versionID {
				continue
			}
			for _, change := range candidate.Objects {
				if change.ID != existing.ID {
					continue
				}
				var initial openapi.ModelArtifact
				if err := json.Unmarshal(change.Snapshot.Resource, &initial); err != nil {
					return err
				}
				if string(normalizedArtifactCreate(&initial)) == string(normalizedArtifactCreate(artifact)) {
					return nil
				}
				return fmt.Errorf("model artifact %q attachment differs from its initial payload: %w", existing.Name, api.ErrConflict)
			}
		}
	}
	return fmt.Errorf("model artifact %q already exists; update by ID: %w", existing.Name, api.ErrConflict)
}

// reserveArtifact retains the initial normalized request in the invisible
// reservation. An old unlinked reservation is adopted only when its journal
// proves this exact version link and payload; a global name is insufficient.
func (s *Service) reserveArtifact(ctx context.Context, object Object, artifact *openapi.ModelArtifact) (Object, error) {
	if object.ArtifactScopeID != 0 {
		var legacy []Object
		if err := s.store.DB().WithContext(ctx).Where("kind = ? AND name = ? AND artifact_scope_id = 0 AND visibility = ? AND deleted = ?", KindModelArtifact, object.Name, VisibilityReserved, false).Find(&legacy).Error; err != nil {
			return Object{}, mapDBError(err)
		}
		for _, row := range legacy {
			var operations []Operation
			if err := s.store.DB().WithContext(ctx).Where("resource_id = ? AND state IN ?", row.ID, []OperationState{OperationPending, OperationRunning}).Find(&operations).Error; err != nil {
				return Object{}, mapDBError(err)
			}
			for _, operation := range operations {
				candidate, err := decodeMutationCandidate(operation.CandidateJSON)
				if err != nil {
					return Object{}, err
				}
				if candidate == nil {
					continue
				}
				for _, link := range candidate.Links {
					if link.ArtifactID == row.ID && link.VersionID == object.ArtifactScopeID {
						if err := s.identicalArtifactAttachment(ctx, row, object.ArtifactScopeID, artifact); err != nil {
							return Object{}, err
						}
						return row, nil
					}
				}
			}
		}
	}
	object.CommittedJSON = string(mustMarshal(StoredSnapshot{CodecVersion: candidateCodecVersion, Kind: KindModelArtifact, Resource: mustMarshal(artifactResource(artifact))}))
	reserved, err := s.reserveResource(ctx, object)
	if err != nil {
		return Object{}, err
	}
	initial, err := decodeStoredSnapshot(reserved.CommittedJSON)
	if err != nil {
		return Object{}, err
	}
	var prior openapi.ModelArtifact
	if len(initial.Resource) != 0 && string(initial.Resource) != "{}" {
		if err := json.Unmarshal(initial.Resource, &prior); err != nil {
			return Object{}, err
		}
		if string(normalizedArtifactCreate(&prior)) != string(normalizedArtifactCreate(artifact)) {
			return Object{}, fmt.Errorf("model artifact %q reservation differs from its initial payload: %w", object.Name, api.ErrConflict)
		}
	} else if object.ArtifactScopeID != 0 {
		if err := s.identicalArtifactAttachment(ctx, reserved, object.ArtifactScopeID, artifact); err != nil {
			return Object{}, err
		}
	} else {
		return Object{}, fmt.Errorf("model artifact %q reservation has no initial payload: %w", object.Name, api.ErrConflict)
	}
	return reserved, nil
}
