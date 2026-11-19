package mlflow

// ObjectKind enumerates the logical Kubeflow resources the compatibility
// store persists. Internal kinds must never leak into REST responses.
type ObjectKind string

const (
	KindRegisteredModel ObjectKind = "registered-model"
	KindModelVersion    ObjectKind = "model-version"
	KindModelArtifact   ObjectKind = "model-artifact"
)

// parentNone is the sentinel parent_id for parentless objects. It is used
// instead of NULL so that (kind, parent, name) uniqueness is enforceable
// on MySQL, which does not treat NULL values as distinct in unique keys.
const parentNone int32 = 0

// ObjectVisibility controls whether a row is publicly readable. Rows
// reserved for native-backed creates stay invisible until publication, so
// a rejected native create never appears as a successful resource.
type ObjectVisibility string

const (
	VisibilityReserved  ObjectVisibility = "RESERVED"
	VisibilityCommitted ObjectVisibility = "COMMITTED"
)

// Object is one committed logical resource row in MLflowObject. The
// typed JSON snapshot preserves metadata discriminators and original
// values; it is only ever replaced by publishing an operation.
type Object struct {
	ID                       int32            `gorm:"column:id;primaryKey;autoIncrement:true"`
	Kind                     ObjectKind       `gorm:"column:kind;not null"`
	ParentID                 int32            `gorm:"column:parent_id;not null"`
	ArtifactScopeID          int32            `gorm:"column:artifact_scope_id;not null"`
	Name                     string           `gorm:"column:name;not null"`
	ExternalID               *string          `gorm:"column:external_id"`
	CreateTimeSinceEpoch     int64            `gorm:"column:create_time_since_epoch;not null"`
	LastUpdateTimeSinceEpoch int64            `gorm:"column:last_update_time_since_epoch;not null"`
	CommittedJSON            string           `gorm:"column:committed_json;not null"`
	Revision                 int64            `gorm:"column:revision;not null"`
	Deleted                  bool             `gorm:"column:deleted;not null"`
	NativeModelName          *string          `gorm:"column:native_model_name"`
	Visibility               ObjectVisibility `gorm:"column:visibility;not null"`
}

// TableName implements the GORM naming interface.
func (*Object) TableName() string { return "MLflowObject" }

// MaxKubeflowID is the largest REST resource ID the compatibility store
// may allocate, matching the existing REST validation range.
const MaxKubeflowID = 999999999

// VersionBindingPhase is the internal materialization phase of a logical
// model version. Phases are internal state, not public ModelVersionState
// values.
type VersionBindingPhase string

const (
	PhasePendingSource VersionBindingPhase = "PENDING_SOURCE"
	PhaseMaterializing VersionBindingPhase = "MATERIALIZING"
	PhaseCreateInDoubt VersionBindingPhase = "CREATE_IN_DOUBT"
	PhaseReady         VersionBindingPhase = "READY"
	PhaseReplacing     VersionBindingPhase = "REPLACING"
)

// VersionBinding is the materialization state of a logical model version.
// NativeModelName and NativeVersionNumber are nil while the version is
// pending; once bound, the native identity is unique.
type VersionBinding struct {
	ID                  int32               `gorm:"column:id;primaryKey;autoIncrement:true"`
	VersionID           int32               `gorm:"column:version_id;not null;unique"`
	ModelID             int32               `gorm:"column:model_id;not null"`
	PrimaryArtifactID   *int32              `gorm:"column:primary_artifact_id"`
	Phase               VersionBindingPhase `gorm:"column:phase;not null"`
	NativeModelName     *string             `gorm:"column:native_model_name"`
	NativeVersionNumber *string             `gorm:"column:native_version_number"`
	ActiveGeneration    int64               `gorm:"column:active_generation;not null"`
	DesiredGeneration   int64               `gorm:"column:desired_generation;not null"`
	Alias               string              `gorm:"column:alias;not null"`
	Revision            int64               `gorm:"column:revision;not null"`
}

// TableName implements the GORM naming interface.
func (*VersionBinding) TableName() string { return "MLflowVersionBinding" }

// VersionHistory is an immutable record of one native generation bound to
// a logical version. Superseded rows are retained so imports never
// recreate replacement history as new logical versions.
type VersionHistory struct {
	ID                  int64  `gorm:"column:id;primaryKey;autoIncrement:true"`
	VersionID           int32  `gorm:"column:version_id;not null"`
	Generation          int64  `gorm:"column:generation;not null"`
	NativeModelName     string `gorm:"column:native_model_name;not null"`
	NativeVersionNumber string `gorm:"column:native_version_number;not null"`
	Source              string `gorm:"column:source;not null"`
	OperationUUID       string `gorm:"column:operation_uuid;not null"`
	Superseded          bool   `gorm:"column:superseded;not null"`
	SupersededTime      *int64 `gorm:"column:superseded_time"`
}

// TableName implements the GORM naming interface.
func (*VersionHistory) TableName() string { return "MLflowVersionHistory" }

// ArtifactLink is one artifact-to-version relationship. An artifact may
// link to multiple versions.
type ArtifactLink struct {
	ArtifactID int32 `gorm:"column:artifact_id;primaryKey"`
	VersionID  int32 `gorm:"column:version_id;primaryKey"`
}

// TableName implements the GORM naming interface.
func (*ArtifactLink) TableName() string { return "MLflowArtifactLink" }

// Adapter is the singleton row binding a compatibility database to one
// MLflow registry target. LastRefreshTime is the millisecond timestamp of
// the last successful native refresh, matching the BIGINT column.
type Adapter struct {
	ID                int32  `gorm:"column:id;primaryKey;autoIncrement:true"`
	AdapterUUID       string `gorm:"column:adapter_uuid;not null"`
	TargetFingerprint string `gorm:"column:target_fingerprint;not null"`
	SchemaVersion     int    `gorm:"column:schema_version;not null"`
	CodecVersion      int    `gorm:"column:codec_version;not null"`
	LastRefreshTime   *int64 `gorm:"column:last_refresh_time"`
}

// TableName implements the GORM naming interface.
func (*Adapter) TableName() string { return "MLflowAdapter" }

// IntentResource reserves a consumed or changed object until its graph publishes.
// The reservation is durable ownership, never a lock held over native HTTP.
type IntentResource struct {
	ResourceID        int32  `gorm:"column:resource_id;primaryKey"`
	RootOperationUUID string `gorm:"column:root_operation_uuid;not null"`
	ExpectedRevision  int64  `gorm:"column:expected_revision;not null"`
}

func (*IntentResource) TableName() string { return "MLflowIntentResource" }
