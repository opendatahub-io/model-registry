package mlflow

// OperationState is the lifecycle state of a journaled operation.
type OperationState string

const (
	OperationPending   OperationState = "PENDING"
	OperationRunning   OperationState = "RUNNING"
	OperationSucceeded OperationState = "SUCCEEDED"
	OperationFailed    OperationState = "FAILED"
)

// Operation steps, persisted as execution checkpoints.
const (
	StepPrepared         = "PREPARED"
	StepClaimed          = "CLAIMED"
	StepCreateDispatched = "CREATE_DISPATCHED"
	StepNativeCreated    = "NATIVE_CREATED"
	StepTagsApplied      = "TAGS_APPLIED"
	StepAliasMoved       = "ALIAS_MOVED"
	StepPublished        = "PUBLISHED"
)

// Operation is one journaled mutation intent. CandidateJSON is an
// immutable snapshot of the intended post-operation object state; it is
// only applied to MLflowObject by PublishOperation, so a rejected native
// write never changes visible state.
type Operation struct {
	RequestHash               string         `gorm:"column:request_hash;not null"`
	RequestSequence           int64          `gorm:"column:request_sequence;not null"`
	AggregateOperationUUID    *string        `gorm:"column:aggregate_operation_uuid"`
	SupersededByOperationUUID *string        `gorm:"column:superseded_by_operation_uuid"`
	ExecutionPlanJSON         *string        `gorm:"column:execution_plan_json"`
	OperationUUID             string         `gorm:"column:operation_uuid;primaryKey"`
	ResourceID                int32          `gorm:"column:resource_id;not null"`
	Kind                      string         `gorm:"column:kind;not null"`
	ExpectedRevision          int64          `gorm:"column:expected_revision;not null"`
	CandidateJSON             string         `gorm:"column:candidate_json;not null"`
	PayloadHash               string         `gorm:"column:payload_hash;not null"`
	Generation                int64          `gorm:"column:generation;not null"`
	Step                      string         `gorm:"column:step;not null"`
	State                     OperationState `gorm:"column:state;not null"`
	NativeResult              *string        `gorm:"column:native_result"`
	ParentOperationUUID       *string        `gorm:"column:parent_operation_uuid"`
	LeaseOwner                *string        `gorm:"column:lease_owner"`
	LeaseExpiresSinceEpoch    *int64         `gorm:"column:lease_expires_since_epoch"`
	FencingToken              int64          `gorm:"column:fencing_token;not null"`
	Attempts                  int            `gorm:"column:attempts;not null"`
	NextRetrySinceEpoch       *int64         `gorm:"column:next_retry_since_epoch"`
	FailureCode               *string        `gorm:"column:failure_code"`
	CreateTimeSinceEpoch      int64          `gorm:"column:create_time_since_epoch;not null"`
	LastUpdateTimeSinceEpoch  int64          `gorm:"column:last_update_time_since_epoch;not null"`
}

// TableName implements the GORM naming interface.
func (*Operation) TableName() string { return "MLflowOperation" }
