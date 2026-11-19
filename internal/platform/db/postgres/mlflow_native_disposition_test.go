package postgres_test

import (
	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/platform/db/postgres"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestMLflowNativeCheckpointCapacityMigration(t *testing.T) {
	db := newSecondaryPostgresDB(t, "native_checkpoint_capacity")
	migrator, err := postgres.NewPostgresMigrator(db)
	require.NoError(t, err)
	require.NoError(t, migrator.Migrate())
	step := -1
	require.NoError(t, migrator.Down(&step))
	require.NoError(t, db.Create(&mlflow.Object{ID: 1, Kind: mlflow.KindModelVersion, Name: "capacity", CommittedJSON: "{}", Visibility: mlflow.VisibilityCommitted}).Error)
	legacy := strings.Repeat("x", 512)
	require.NoError(t, db.Create(&mlflow.Operation{OperationUUID: "capacity", ResourceID: 1, Kind: mlflow.OperationKindVersionMaterialize, CandidateJSON: "{}", PayloadHash: "immutable", State: mlflow.OperationRunning, Step: mlflow.StepNativeCreated, NativeResult: &legacy}).Error)
	require.NoError(t, migrator.Migrate())
	var upgraded mlflow.Operation
	require.NoError(t, db.Where("operation_uuid = ?", "capacity").First(&upgraded).Error)
	require.Equal(t, &legacy, upgraded.NativeResult)
	require.NoError(t, db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", "capacity").Update("native_result", legacy+"x").Error, "durable operator evidence must not reduce the old checkpoint capacity")
	require.Error(t, migrator.Down(&step), "downgrade must refuse truncating retained checkpoint evidence")
	require.True(t, db.Migrator().HasTable("MLflowNativeDisposition"))
}

func TestMLflowSelectionCheckpointDowngradeGuard(t *testing.T) {
	db := newSecondaryPostgresDB(t, "native_selection_downgrade")
	migrator, err := postgres.NewPostgresMigrator(db)
	require.NoError(t, err)
	require.NoError(t, migrator.Migrate())
	require.NoError(t, db.Create(&mlflow.Object{ID: 1, Kind: mlflow.KindModelVersion, Name: "selected", CommittedJSON: "{}", Visibility: mlflow.VisibilityCommitted}).Error)
	require.NoError(t, db.Create(&mlflow.Operation{OperationUUID: "selection", ResourceID: 1, Kind: mlflow.OperationKindVersionMaterialize,
		CandidateJSON: "{}", PayloadHash: "immutable", State: mlflow.OperationRunning, Step: mlflow.StepNativeCreated, FailureCode: new("DEFINITIVE_AUTH"),
		NativeResult: new(`{"modelName":"model","versionNumber":"1","source":"s3://source","generation":1,"status":"READY","explicitSelection":true}`)}).Error)
	step := -1
	require.Error(t, migrator.Down(&step), "selection evidence survives scheduling failures and must prohibit downgrade")
	require.True(t, db.Migrator().HasTable("MLflowNativeDisposition"))
}

func TestMLflowNativeDispositionMigration(t *testing.T) {
	db := newSecondaryPostgresDB(t, "native_disposition_upgrade")
	migrator, err := postgres.NewPostgresMigrator(db)
	require.NoError(t, err)
	require.NoError(t, migrator.Migrate())
	require.True(t, db.Migrator().HasTable("MLflowNativeDisposition"))
	step := -1
	require.NoError(t, migrator.Down(&step))
	require.False(t, db.Migrator().HasTable("MLflowNativeDisposition"))
	require.NoError(t, migrator.Migrate())
	for _, name := range []string{"Model", "model", "Model "} {
		require.NoError(t, db.Table("MLflowNativeDisposition").Create(map[string]any{"native_model_name": name, "native_version_number": "1", "adapter_uuid": "adapter", "version_id": 1, "operation_uuid": "origin", "generation": 2, "disposition": "UNSELECTED_DUPLICATE", "resolving_operation_uuid": "selection", "resolved_time_since_epoch": 123}).Error)
	}
	require.Error(t, migrator.Down(&step), "retained audit evidence prohibits downgrade")
	var count int64
	require.NoError(t, db.Table("MLflowNativeDisposition").Count(&count).Error)
	require.Equal(t, int64(3), count)
}
