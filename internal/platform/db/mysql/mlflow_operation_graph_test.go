package mysql_test

import (
	"encoding/json"
	"fmt"
	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/platform/db/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMLflowOperationGraphMigration(t *testing.T) {
	for _, scenario := range []string{"safe", "reservations", "sequences", "candidate_v3", "snapshot_v3", "aggregate", "execution_plan"} {
		t.Run(scenario, func(t *testing.T) {
			db := newSecondaryMySQLDB(t, "graph_upgrade_"+scenario)
			migrator, err := mysql.NewMySQLMigrator(db)
			require.NoError(t, err)
			require.NoError(t, migrator.Migrate())
			step := -2
			require.NoError(t, migrator.Down(&step))
			var oldVersion int
			require.NoError(t, db.Raw("SELECT version FROM schema_migrations").Scan(&oldVersion).Error)
			require.Equal(t, 37, oldVersion)
			require.NoError(t, db.Exec("INSERT INTO `MLflowObject` (id,kind,parent_id,artifact_scope_id,name,create_time_since_epoch,last_update_time_since_epoch,committed_json,revision,deleted,visibility) VALUES (1,'registered-model',0,0,'upgrade',10,20,'{}',0,FALSE,'RESERVED')").Error)
			require.NoError(t, db.Exec("INSERT INTO `MLflowOperation` (operation_uuid,resource_id,kind,expected_revision,candidate_json,payload_hash,generation,step,state,create_time_since_epoch,last_update_time_since_epoch) VALUES ('legacy',1,'model-create',0,'{\"codecVersion\":2}','immutable',1,'CREATE_DISPATCHED','RUNNING',10,20)").Error)
			require.NoError(t, db.Exec("INSERT INTO `MLflowAdapter` (id,adapter_uuid,target_fingerprint,schema_version,codec_version) VALUES (1,'adapter','target',1,2)").Error)
			require.NoError(t, migrator.Migrate())
			var sequence int64
			require.NoError(t, db.Raw("SELECT request_sequence FROM `MLflowOperation` WHERE operation_uuid='legacy' ").Scan(&sequence).Error)
			assert.Zero(t, sequence)
			var candidate string
			require.NoError(t, db.Raw("SELECT candidate_json FROM `MLflowOperation` WHERE operation_uuid='legacy' ").Scan(&candidate).Error)
			assert.Equal(t, `{"codecVersion":2}`, candidate)
			var schema, codec int
			require.NoError(t, db.Raw("SELECT schema_version,codec_version FROM `MLflowAdapter`").Row().Scan(&schema, &codec))
			assert.Equal(t, 2, schema)
			assert.Equal(t, 3, codec)
			switch scenario {
			case "reservations":
				require.NoError(t, db.Exec("INSERT INTO `MLflowIntentResource` (resource_id,root_operation_uuid,expected_revision) VALUES (1,'legacy',0)").Error)
			case "sequences":
				require.NoError(t, db.Exec("INSERT INTO `MLflowOperation` (operation_uuid,resource_id,kind,expected_revision,candidate_json,payload_hash,generation,step,state,create_time_since_epoch,last_update_time_since_epoch,request_sequence) VALUES ('second',1,'model-create',0,'{}','retained',1,'PREPARED','PENDING',10,20,1)").Error)
			case "candidate_v3":
				require.NoError(t, db.Exec("UPDATE `MLflowOperation` SET candidate_json='{\"codecVersion\":3}' WHERE operation_uuid='legacy' ").Error)
			case "snapshot_v3":
				require.NoError(t, db.Exec("UPDATE `MLflowObject` SET committed_json='{\"codecVersion\":3,\"resource\":{}}' WHERE id=1").Error)
			case "aggregate":
				require.NoError(t, db.Exec("UPDATE `MLflowOperation` SET aggregate_operation_uuid='legacy' WHERE operation_uuid='legacy' ").Error)
			case "execution_plan":
				require.NoError(t, db.Exec("UPDATE `MLflowOperation` SET execution_plan_json='{}' WHERE operation_uuid='legacy' ").Error)
			}
			err = migrator.Down(&step)
			if scenario == "safe" {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "unsafe downgrade must refuse before dropping graph evidence")
				assert.True(t, db.Migrator().HasTable("MLflowIntentResource"))
				assert.True(t, db.Migrator().HasColumn("MLflowOperation", "request_sequence"))
			}
		})
	}
}

// This upgrade fixture is inserted before the graph migration. It includes a
// partially completed replacement, a pending create, an operator retry, and
// orphan children, preserving all prior operation and binding evidence.
func TestMLflowOperationGraphUpgradeRecovery(t *testing.T) {
	db := newSecondaryMySQLDB(t, "graph_recovery_upgrade")
	migrator, err := mysql.NewMySQLMigrator(db)
	require.NoError(t, err)
	require.NoError(t, migrator.Migrate())
	step := -2
	require.NoError(t, migrator.Down(&step))
	model := mlflow.Object{ID: 10, Kind: mlflow.KindRegisteredModel, Name: "upgrade", CommittedJSON: `{"name":"upgrade"}`, Revision: 1, Visibility: mlflow.VisibilityCommitted}
	require.NoError(t, db.Create(&model).Error)
	for _, id := range []int32{11, 12, 14, 15, 16} {
		require.NoError(t, db.Create(&mlflow.Object{ID: id, Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: fmt.Sprint(id), CommittedJSON: `{"name":"version"}`, Revision: 1, Visibility: mlflow.VisibilityCommitted}).Error)
	}
	require.NoError(t, db.Create(&mlflow.Object{ID: 13, Kind: mlflow.KindModelArtifact, Name: "primary", CommittedJSON: `{"name":"primary","uri":"s3://old"}`, Revision: 1, Visibility: mlflow.VisibilityCommitted}).Error)
	for _, id := range []int32{11, 12} {
		require.NoError(t, db.Create(&mlflow.ArtifactLink{ArtifactID: 13, VersionID: id}).Error)
		require.NoError(t, db.Create(&mlflow.VersionBinding{VersionID: id, ModelID: 10, PrimaryArtifactID: new(int32(13)), Phase: mlflow.PhaseReady, NativeModelName: new("upgrade"), NativeVersionNumber: new(fmt.Sprint(id)), Revision: 1, ActiveGeneration: 1}).Error)
		require.NoError(t, db.Create(&mlflow.VersionHistory{VersionID: id, Generation: 1, NativeModelName: "upgrade", NativeVersionNumber: fmt.Sprint(id), Source: "s3://old", OperationUUID: "original-materialization"}).Error)
	}
	rootCandidate := mlflow.MutationCandidate{CodecVersion: 2, Objects: []mlflow.ObjectChange{{ID: 13, ExpectedRevision: 1, Snapshot: mlflow.StoredSnapshot{CodecVersion: 2, Kind: mlflow.KindModelArtifact, Resource: json.RawMessage(`{"name":"primary","uri":"s3://new"}`)}}}}
	ops := []mlflow.Operation{}
	for _, id := range []int32{11, 12} {
		childID := fmt.Sprintf("child-%d", id)
		rootCandidate.Bindings = append(rootCandidate.Bindings, mlflow.BindingChange{ExpectedRevision: 1, Value: mlflow.VersionBinding{VersionID: id, ModelID: 10, PrimaryArtifactID: new(int32(13)), Phase: mlflow.PhaseReady, NativeModelName: new("upgrade"), ActiveGeneration: 2}, ResultOperationUUID: childID})
		candidate := mlflow.MutationCandidate{CodecVersion: 2, Objects: []mlflow.ObjectChange{{ID: id, ExpectedRevision: 1, Snapshot: mlflow.StoredSnapshot{CodecVersion: 2, Kind: mlflow.KindModelVersion, Resource: json.RawMessage(`{"modelName":"upgrade","source":"s3://new"}`)}}}}
		payload, err := json.Marshal(candidate)
		require.NoError(t, err)
		op := mlflow.Operation{OperationUUID: childID, ResourceID: id, Kind: mlflow.OperationKindVersionReplaceChild, ExpectedRevision: 1, CandidateJSON: string(payload), PayloadHash: "retained-child", Generation: 2, State: mlflow.OperationRunning, Step: mlflow.StepCreateDispatched, Attempts: 2, FencingToken: 3}
		if id == 11 {
			op.State = mlflow.OperationSucceeded
			op.Step = mlflow.StepNativeCreated
			op.NativeResult = new(`{"modelName":"upgrade","versionNumber":"21","generation":2,"status":"READY"}`)
		}
		ops = append(ops, op)
	}
	payload, err := json.Marshal(rootCandidate)
	require.NoError(t, err)
	ops = append(ops, mlflow.Operation{OperationUUID: "root", ResourceID: 13, Kind: mlflow.OperationKindVersionReplaceFanOut, ExpectedRevision: 1, CandidateJSON: string(payload), PayloadHash: "retained-root", State: mlflow.OperationRunning, Step: mlflow.StepClaimed})
	ops = append(ops, mlflow.Operation{OperationUUID: "pending-create", ResourceID: 14, Kind: mlflow.OperationKindVersionCreate, ExpectedRevision: 1, CandidateJSON: `{"name":"pending"}`, PayloadHash: "pending", State: mlflow.OperationPending, Step: mlflow.StepPrepared})
	ops = append(ops, mlflow.Operation{OperationUUID: "superseded", ResourceID: 15, Kind: mlflow.OperationKindVersionMaterialize, ExpectedRevision: 1, CandidateJSON: `{}`, PayloadHash: "superseded", State: mlflow.OperationFailed, FailureCode: new(mlflow.FailureCodeOperatorRetry), Step: mlflow.StepCreateDispatched})
	ops = append(ops, mlflow.Operation{OperationUUID: "retry", ResourceID: 15, Kind: mlflow.OperationKindVersionCreateRetry, ExpectedRevision: 1, CandidateJSON: `{}`, PayloadHash: "retry", State: mlflow.OperationPending, Step: mlflow.StepPrepared, ParentOperationUUID: new("superseded")})
	ops = append(ops, mlflow.Operation{OperationUUID: "orphan", ResourceID: 16, Kind: mlflow.OperationKindVersionReplaceChild, ExpectedRevision: 1, CandidateJSON: `{}`, PayloadHash: "orphan", State: mlflow.OperationPending, Step: mlflow.StepCreateDispatched})
	completedVersion := mlflow.Object{ID: 17, Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: "completed", CommittedJSON: `{"name":"completed"}`, Revision: 2, Visibility: mlflow.VisibilityCommitted}
	require.NoError(t, db.Create(&completedVersion).Error)
	completedBinding := mlflow.VersionBinding{VersionID: 17, ModelID: 10, Phase: mlflow.PhaseReady, NativeModelName: new("upgrade"), NativeVersionNumber: new("31"), Revision: 2, ActiveGeneration: 1}
	require.NoError(t, db.Create(&completedBinding).Error)
	completedHistory := mlflow.VersionHistory{VersionID: 17, Generation: 1, NativeModelName: "upgrade", NativeVersionNumber: "31", Source: "s3://completed", OperationUUID: "completed-retry"}
	require.NoError(t, db.Create(&completedHistory).Error)
	for _, id := range []string{"completed-original", "completed-retry"} {
		candidate := mlflow.MutationCandidate{CodecVersion: 2,
			Bindings: []mlflow.BindingChange{{ExpectedRevision: 1, Value: completedBinding, ResultOperationUUID: id}},
			History:  []mlflow.HistoryChange{{Value: mlflow.VersionHistory{VersionID: 17, Generation: 1, NativeModelName: "upgrade", Source: "s3://completed", OperationUUID: id}, ResultOperationUUID: id}}}
		payload, err := json.Marshal(candidate)
		require.NoError(t, err)
		op := mlflow.Operation{OperationUUID: id, ResourceID: 17, Kind: mlflow.OperationKindVersionMaterialize, ExpectedRevision: 1, CandidateJSON: string(payload), PayloadHash: "immutable-" + id, Generation: 1, State: mlflow.OperationFailed, FailureCode: new(mlflow.FailureCodeOperatorRetry), Step: mlflow.StepCreateDispatched, Attempts: 2, FencingToken: 3}
		if id == "completed-retry" {
			op.Kind, op.State, op.Step = mlflow.OperationKindVersionCreateRetry, mlflow.OperationSucceeded, mlflow.StepPublished
			op.ParentOperationUUID, op.FailureCode = new("completed-original"), nil
			op.NativeResult = new(`{"modelName":"upgrade","versionNumber":"31","source":"s3://completed","generation":1,"status":"READY"}`)
		}
		ops = append(ops, op)
	}
	for i := range ops {
		ops[i].CreateTimeSinceEpoch = 10
		ops[i].LastUpdateTimeSinceEpoch = 20
		require.NoError(t, db.Omit("RequestHash", "RequestSequence", "AggregateOperationUUID", "SupersededByOperationUUID", "ExecutionPlanJSON").Create(&ops[i]).Error)
	}
	require.NoError(t, migrator.Migrate())
	store := mlflow.NewStore(db)
	_, err = store.InitializeAdapter(t.Context(), "upgrade-registry")
	require.NoError(t, err)
	for _, before := range ops {
		after, err := store.GetOperation(t.Context(), before.OperationUUID)
		require.NoError(t, err)
		assert.Equal(t, before.CandidateJSON, after.CandidateJSON)
		assert.Equal(t, before.PayloadHash, after.PayloadHash)
		assert.Equal(t, before.NativeResult, after.NativeResult)
		assert.Equal(t, before.Step, after.Step)
		assert.Equal(t, before.Attempts, after.Attempts)
		assert.Equal(t, before.FencingToken, after.FencingToken)
		assert.Equal(t, int64(10), after.CreateTimeSinceEpoch)
		assert.Zero(t, after.RequestSequence)
	}
	var reservations []mlflow.IntentResource
	require.NoError(t, db.Find(&reservations).Error)
	assert.Len(t, reservations, 6)
	var binding mlflow.VersionBinding
	require.NoError(t, db.Where("version_id = ?", 11).First(&binding).Error)
	assert.Equal(t, "11", *binding.NativeVersionNumber)
	assert.Equal(t, int64(1), binding.ActiveGeneration)
	var history int64
	require.NoError(t, db.Model(&mlflow.VersionHistory{}).Count(&history).Error)
	assert.Equal(t, int64(3), history)
	completed, err := store.ResolveAttempt(t.Context(), "completed-original")
	require.NoError(t, err)
	assert.Equal(t, "completed-retry", completed.OperationUUID)
	assert.Equal(t, mlflow.OperationSucceeded, completed.State)
	assert.NotEmpty(t, completed.RequestHash)
	var preservedVersion mlflow.Object
	require.NoError(t, db.Where("id = ?", 17).First(&preservedVersion).Error)
	assert.Equal(t, completedVersion, preservedVersion)
	var preservedBinding mlflow.VersionBinding
	require.NoError(t, db.Where("version_id = ?", 17).First(&preservedBinding).Error)
	assert.Equal(t, completedBinding, preservedBinding)
	var preservedHistory mlflow.VersionHistory
	require.NoError(t, db.Where("version_id = ?", 17).First(&preservedHistory).Error)
	assert.Equal(t, completedHistory, preservedHistory)
	child, err := store.GetOperation(t.Context(), "child-11")
	require.NoError(t, err)
	assert.Equal(t, mlflow.OperationSucceeded, child.State)
	assert.Equal(t, "root", *child.AggregateOperationUUID)
	retry, err := store.GetOperation(t.Context(), "retry")
	require.NoError(t, err)
	assert.Equal(t, "superseded", *retry.AggregateOperationUUID)
	selected, err := store.ResolveAttempt(t.Context(), "superseded")
	require.NoError(t, err)
	assert.Equal(t, "retry", selected.OperationUUID)
	original, err := store.GetOperation(t.Context(), "superseded")
	require.NoError(t, err)
	assert.NotEmpty(t, original.RequestHash)
	assert.Equal(t, original.RequestHash, selected.RequestHash)
	orphan, err := store.GetOperation(t.Context(), "orphan")
	require.NoError(t, err)
	assert.Equal(t, "LEGACY_GRAPH_QUARANTINED", *orphan.FailureCode)
	require.Error(t, migrator.Down(&step), "hydrated graph ownership must prohibit downgrade")
}
