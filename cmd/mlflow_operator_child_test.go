package cmd

import (
	"encoding/json"
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOperatorChildCommands(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			f := newOperationsFixture(t, engine)
			for _, action := range []string{"bind", "retry-create"} {
				t.Run(action, func(t *testing.T) {
					model := f.mustModel(t, action)
					first := f.mustVersion(t, model, "first")
					second := f.mustVersion(t, model, "second")
					artifact, err := f.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Name: new(action), Uri: new("s3://old")}}, *first.Id)
					require.NoError(t, err)
					_, err = f.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: artifact.ModelArtifact.Id}}, *second.Id)
					require.NoError(t, err)
					f.stub.mu.Lock()
					f.stub.commitThenFail = 1
					f.stub.mu.Unlock()
					_, err = f.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: artifact.ModelArtifact.Id, Name: new(action), Uri: new("s3://new")}}, *first.Id)
					require.ErrorIs(t, err, api.ErrUnavailable)
					f.expireOperationLeases(t)
					var child mlflow.Operation
					require.NoError(t, f.store.DB().Where("kind = ? AND step = ? AND state = ?", mlflow.OperationKindVersionReplaceChild, mlflow.StepCreateDispatched, mlflow.OperationRunning).First(&child).Error)
					args := []string{action, "--operation", child.OperationUUID}
					if action == "bind" {
						args = append(args, "--model", action, "--version", "3")
					} else {
						args = append(args, "--accept-duplicate-risk")
					}
					_, err = f.runOperationCommand(t, args...)
					require.NoError(t, err)
					root := f.operation(t, *child.AggregateOperationUUID)
					assert.NotEqual(t, mlflow.OperationSucceeded, root.State)
					f.expireOperationLeases(t)
					require.NoError(t, f.service.ApplyOperation(t.Context(), root.OperationUUID))
					assert.Equal(t, int64(2), f.bindingOfVersion(t, *first.Id).ActiveGeneration)
				})
			}
		})
	}
}

func TestOrphanResolutionCommandAndInspection(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			f := newOperationsFixture(t, engine)
			op := f.uncertainOperation(t, "inspection", "https://storage.invalid/object?signature=secret", true)
			raw := `{"modelName":"inspection","versionNumber":"1","generation":1,"source":"https://storage.invalid/object?signature=secret","signedSource":"hidden credential"}`
			require.NoError(t, f.store.DB().Model(&mlflow.Operation{}).Where("operation_uuid = ?", op.OperationUUID).Update("native_result", raw).Error)
			output, err := f.runOperationCommand(t, "list")
			require.NoError(t, err)
			assert.Contains(t, output, "inspection")
			assert.NotContains(t, output, "secret")
			assert.NotContains(t, output, "credential")
			assert.NotContains(t, output, "storage.invalid")
			_, err = f.runOperationCommand(t, "resolve-orphan", "--retain-native")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "operation")
			_, err = f.runOperationCommand(t, "resolve-orphan", "--operation", op.OperationUUID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--retain-native")
			var original mlflow.MutationCandidate
			require.NoError(t, json.Unmarshal([]byte(op.CandidateJSON), &original))
			plan := original.NativeWork
			require.NotNil(t, plan)
			plan.OwnedTags["rhoai.operation"] = "command-orphan"
			encoded, err := json.Marshal(mlflow.MutationCandidate{CodecVersion: 3, NativeWork: plan})
			require.NoError(t, err)
			version, err := f.store.GetObject(t.Context(), plan.LogicalVersionID)
			require.NoError(t, err)
			orphan := mlflow.Operation{OperationUUID: "command-orphan", ResourceID: version.ID, Kind: mlflow.OperationKindVersionReplaceChild, ExpectedRevision: version.Revision, CandidateJSON: string(encoded), PayloadHash: "retained", Generation: plan.Generation, State: mlflow.OperationRunning, Step: mlflow.StepCreateDispatched}
			require.NoError(t, f.store.DB().Create(&orphan).Error)
			_, err = f.runOperationCommand(t, "resolve-orphan", "--operation", orphan.OperationUUID, "--retain-native")
			require.NoError(t, err)
			resolved := f.operation(t, orphan.OperationUUID)
			assert.Equal(t, mlflow.OperationFailed, resolved.State)
			require.NotNil(t, resolved.FailureCode)
			assert.Equal(t, "ORPHAN_RETAINED", *resolved.FailureCode)
			assert.Equal(t, orphan.CandidateJSON, resolved.CandidateJSON)
			assert.Nil(t, resolved.NativeResult)
		})
	}
}
