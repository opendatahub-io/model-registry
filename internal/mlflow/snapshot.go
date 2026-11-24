package mlflow

import (
	"encoding/json"
	"fmt"

	"github.com/kubeflow/hub/pkg/api"
)

// candidateCodecVersion is the journal payload version introduced with
// typed multi-resource candidates. Version 1 ( implicit ) is the legacy
// flat single-object candidate. Version 4 makes the additional-artifact
// tag qualification injective: mirrored fields, custom properties, and
// direct field keys keep their full sub-namespace under
// "rhoai.artifacts.<id>." instead of being flattened onto bare
// suffixes, where same-named fields and properties collided.
const candidateCodecVersion = 4

// supportedSnapshotCodec reports whether a stored journal or snapshot
// version can still be decoded. Versions 2 and 3 predate the injective
// additional-artifact namespace; their recorded projections replay
// verbatim under their own encoding.
func supportedSnapshotCodec(version int) bool {
	return version >= 2 && version <= candidateCodecVersion
}

// StoredSnapshot is the private committed snapshot format for one
// resource. Resource is the public API JSON and the only part that ever
// enters a response; TagKeys retains the original-key mapping for
// escaped custom properties and OwnedTags records the adapter-owned tag
// projection, so later updates can delete exactly the tags they own.
type StoredSnapshot struct {
	CodecVersion int               `json:"codecVersion"`
	Kind         ObjectKind        `json:"kind"`
	Resource     json.RawMessage   `json:"resource"`
	TagKeys      map[string]string `json:"tagKeys,omitempty"`
	OwnedTags    map[string]string `json:"ownedTags,omitempty"`
}

// ObjectChange is one resource update inside a mutation candidate.
type ObjectChange struct {
	ID               int32            `json:"id"`
	ExpectedRevision int64            `json:"expectedRevision"`
	Snapshot         StoredSnapshot   `json:"snapshot"`
	Visibility       ObjectVisibility `json:"visibility,omitempty"`
}

// NativeBindingResult is the verified native identity checkpointed in
// an operation's NativeResult field after a native create succeeds.
// Publication resolves the final native identity from it rather than
// from the immutable candidate template.
type NativeBindingResult struct {
	ModelName     string `json:"modelName"`
	VersionNumber string `json:"versionNumber"`
	Source        string `json:"source"`
	Generation    int64  `json:"generation"`
	Status        string `json:"status"`
	// ExplicitSelection is durable operator evidence, independent of mutable
	// scheduling failures. Every subsequent native checkpoint preserves it.
	ExplicitSelection bool `json:"explicitSelection,omitempty"`
}

// BindingChange inserts or updates one version binding. Value is an
// immutable template; when it carries a native identity, the final
// identity is resolved from the operation named by ResultOperationUUID.
type BindingChange struct {
	ExpectedRevision    int64          `json:"expectedRevision"`
	Value               VersionBinding `json:"value"`
	ResultOperationUUID string         `json:"resultOperationUUID,omitempty"`
}

// HistoryChange records one native generation in the version history,
// optionally superseding previous generations of the same version.
type HistoryChange struct {
	Value               VersionHistory `json:"value"`
	SupersedePrevious   bool           `json:"supersedePrevious,omitempty"`
	ResultOperationUUID string         `json:"resultOperationUUID,omitempty"`
}

// MutationCandidate is the versioned journal payload describing an
// aggregate mutation: object snapshots, artifact links, version
// bindings, and history entries published atomically.
type NativeVersionPlan struct {
	LogicalVersionID int32             `json:"logicalVersionId"`
	ModelName        string            `json:"modelName"`
	Source           string            `json:"source"`
	Description      string            `json:"description"`
	Alias            string            `json:"alias"`
	Generation       int64             `json:"generation"`
	OwnedTags        map[string]string `json:"ownedTags"`
	RetainedTags     map[string]string `json:"retainedTags"`
}

// MutationCandidate keeps the native work and dependency revisions immutable.
type MutationCandidate struct {
	AliasRepair          *AliasRepairPlan   `json:"aliasRepair,omitempty"`
	NativeWork           *NativeVersionPlan `json:"nativeWork,omitempty"`
	ReadRevisions        map[int32]int64    `json:"readRevisions,omitempty"`
	BindingReadRevisions map[int32]int64    `json:"bindingReadRevisions,omitempty"`
	CodecVersion         int                `json:"codecVersion"`
	Objects              []ObjectChange     `json:"objects,omitempty"`
	Links                []ArtifactLink     `json:"links,omitempty"`
	Bindings             []BindingChange    `json:"bindings,omitempty"`
	History              []HistoryChange    `json:"history,omitempty"`
}

// decodeMutationCandidate decodes a versioned journal payload. A legacy
// flat candidate — no codecVersion field — returns ( nil, nil ) so the
// caller applies the legacy single-object path. An unknown version
// fails without mutating anything.
func decodeMutationCandidate(payload string) (*MutationCandidate, error) {
	var probe struct {
		CodecVersion *int `json:"codecVersion"`
	}
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		return nil, fmt.Errorf("decoding candidate payload: %w", api.ErrBadRequest)
	}
	if probe.CodecVersion == nil {
		return nil, nil // legacy flat candidate
	}
	if !supportedSnapshotCodec(*probe.CodecVersion) {
		return nil, fmt.Errorf("candidate codec version %d is not supported ( expected <= %d ): %w",
			*probe.CodecVersion, candidateCodecVersion, api.ErrBadRequest)
	}

	var candidate MutationCandidate
	if err := json.Unmarshal([]byte(payload), &candidate); err != nil {
		return nil, fmt.Errorf("decoding candidate payload: %w", api.ErrBadRequest)
	}
	if err := candidate.validate(); err != nil {
		return nil, err
	}
	return &candidate, nil
}

// validate checks the candidate's structure before any SQL state is
// touched. Relationship kind checks against persisted rows happen in
// the publication transaction, where the rows are locked.
func (c *MutationCandidate) validate() error {
	if !supportedSnapshotCodec(c.CodecVersion) {
		return fmt.Errorf("candidate codec version %d is not supported: %w", c.CodecVersion, api.ErrBadRequest)
	}
	if plan := c.AliasRepair; plan != nil {
		_, objectFence := c.ReadRevisions[plan.VersionID]
		_, bindingFence := c.BindingReadRevisions[plan.VersionID]
		if c.CodecVersion < 3 || plan.VersionID <= 0 || plan.ModelID <= 0 || plan.Alias == "" || plan.Desired.ModelName == "" || plan.Desired.VersionNumber == "" || plan.Desired.Generation <= 0 || !objectFence || !bindingFence || c.NativeWork != nil || len(c.Objects)+len(c.Bindings)+len(c.History)+len(c.Links) != 0 {
			return fmt.Errorf("invalid alias repair plan: %w", api.ErrBadRequest)
		}
	}

	if c.NativeWork != nil {
		if c.CodecVersion < 3 || c.NativeWork.LogicalVersionID <= 0 || c.NativeWork.ModelName == "" || c.NativeWork.Source == "" || c.NativeWork.Generation <= 0 {
			return fmt.Errorf("invalid native work plan: %w", api.ErrBadRequest)
		}
	}
	for _, revisions := range []map[int32]int64{c.ReadRevisions, c.BindingReadRevisions} {
		for id, revision := range revisions {
			if id <= 0 || revision < 0 {
				return fmt.Errorf("invalid read revision: %w", api.ErrBadRequest)
			}
		}
	}
	seenObjects := map[int32]bool{}
	for _, change := range c.Objects {
		if change.ID <= 0 {
			return fmt.Errorf("object change id %d must be positive: %w", change.ID, api.ErrBadRequest)
		}
		if seenObjects[change.ID] {
			return fmt.Errorf("candidate contains duplicate object entry %d: %w", change.ID, api.ErrConflict)
		}
		seenObjects[change.ID] = true
		if change.ExpectedRevision < 0 {
			return fmt.Errorf("object %d has negative expected revision %d: %w",
				change.ID, change.ExpectedRevision, api.ErrBadRequest)
		}
		if !supportedSnapshotCodec(change.Snapshot.CodecVersion) {
			return fmt.Errorf("object %d snapshot codec %d is unsupported: %w", change.ID, change.Snapshot.CodecVersion, api.ErrBadRequest)
		}
		switch change.Snapshot.Kind {
		case KindRegisteredModel, KindModelVersion, KindModelArtifact:
		default:
			return fmt.Errorf("object %d snapshot has unknown kind %q: %w",
				change.ID, change.Snapshot.Kind, api.ErrBadRequest)
		}
		if len(change.Snapshot.Resource) == 0 {
			return fmt.Errorf("object %d snapshot has no resource payload: %w", change.ID, api.ErrBadRequest)
		}
		if change.Visibility != "" && change.Visibility != VisibilityReserved && change.Visibility != VisibilityCommitted {
			return fmt.Errorf("object %d has unknown visibility %q: %w", change.ID, change.Visibility, api.ErrBadRequest)
		}
	}

	seenLinks := map[ArtifactLink]bool{}
	for _, link := range c.Links {
		if link.ArtifactID <= 0 || link.VersionID <= 0 {
			return fmt.Errorf("artifact link %d/%d must reference positive ids: %w",
				link.ArtifactID, link.VersionID, api.ErrBadRequest)
		}
		if seenLinks[link] {
			return fmt.Errorf("candidate contains duplicate artifact link %d/%d: %w",
				link.ArtifactID, link.VersionID, api.ErrBadRequest)
		}
		seenLinks[link] = true
	}

	for _, binding := range c.Bindings {
		if binding.Value.VersionID <= 0 || binding.Value.ModelID <= 0 {
			return fmt.Errorf("binding change must reference positive version and model ids: %w", api.ErrBadRequest)
		}
		if binding.ExpectedRevision < 0 {
			return fmt.Errorf("binding for version %d has negative expected revision %d: %w",
				binding.Value.VersionID, binding.ExpectedRevision, api.ErrBadRequest)
		}
		switch binding.Value.Phase {
		case PhasePendingSource, PhaseMaterializing, PhaseCreateInDoubt, PhaseReady, PhaseReplacing:
		default:
			return fmt.Errorf("binding for version %d has unknown phase %q: %w",
				binding.Value.VersionID, binding.Value.Phase, api.ErrBadRequest)
		}
		hasNativeIdentity := binding.Value.NativeModelName != nil || binding.Value.NativeVersionNumber != nil
		if hasNativeIdentity && binding.ResultOperationUUID == "" {
			return fmt.Errorf("binding for version %d carries a native identity without a result operation: %w",
				binding.Value.VersionID, api.ErrBadRequest)
		}
	}

	for _, history := range c.History {
		if history.Value.VersionID <= 0 {
			return fmt.Errorf("history entry must reference a positive version id: %w", api.ErrBadRequest)
		}
		if history.Value.NativeModelName == "" || history.Value.OperationUUID == "" {
			return fmt.Errorf("history entry for version %d is incomplete: %w",
				history.Value.VersionID, api.ErrBadRequest)
		}
		// A newly created native generation cannot know its
		// server-generated version number at prepare time; it resolves
		// from the result operation at publication.
		if history.Value.NativeVersionNumber == "" && history.ResultOperationUUID == "" {
			return fmt.Errorf("history entry for version %d has no native version number and no result operation: %w",
				history.Value.VersionID, api.ErrBadRequest)
		}
	}
	return nil
}

// snapshotExternalID derives the queryable external ID from the typed
// resource JSON rather than a duplicate unvalidated candidate field.
func snapshotExternalID(snapshot StoredSnapshot) (*string, error) {
	var resource struct {
		ExternalID *string `json:"externalId"`
	}
	if err := json.Unmarshal(snapshot.Resource, &resource); err != nil {
		return nil, fmt.Errorf("decoding resource snapshot for object kind %s: %w",
			snapshot.Kind, api.ErrBadRequest)
	}
	return resource.ExternalID, nil
}

// DecodePublicSnapshot extracts the public API JSON from a committed
// snapshot, never exposing the private tag-key mapping or owned-tag
// projection. A legacy flat snapshot is returned unchanged.
func DecodePublicSnapshot(committedJSON string) (json.RawMessage, error) {
	var probe struct {
		CodecVersion *int `json:"codecVersion"`
	}
	if err := json.Unmarshal([]byte(committedJSON), &probe); err != nil {
		return nil, fmt.Errorf("decoding committed snapshot: %w", api.ErrBadRequest)
	}
	if probe.CodecVersion == nil {
		// Legacy flat candidate: the payload is the public JSON.
		return json.RawMessage(committedJSON), nil
	}
	if !supportedSnapshotCodec(*probe.CodecVersion) {
		return nil, fmt.Errorf("committed snapshot codec version %d is not supported: %w",
			*probe.CodecVersion, api.ErrBadRequest)
	}
	var snapshot StoredSnapshot
	if err := json.Unmarshal([]byte(committedJSON), &snapshot); err != nil {
		return nil, fmt.Errorf("decoding committed snapshot: %w", api.ErrBadRequest)
	}
	return snapshot.Resource, nil
}
