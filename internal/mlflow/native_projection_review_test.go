package mlflow_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Reconciliation and checkpoint replay must reject a conflicting native
// identity before any projection can overwrite the evidence of that conflict.
func TestNativeProjectionModernReplayRejectsIdentityMismatch(t *testing.T) {
	cases := []struct {
		mode, field string
		// A version whose native model (parent) was renamed is foreign to
		// the exact-name recovery lookup: it is never adopted, the create's
		// outcome stays unknown, and the operation remains in doubt. Every
		// other mismatch is still recognized as a conflicting identity.
		wantErr error
	}{
		{mode: "dispatched", field: "adapter", wantErr: api.ErrConflict},
		{mode: "dispatched", field: "logicalId", wantErr: api.ErrConflict},
		{mode: "dispatched", field: "source", wantErr: api.ErrConflict},
		{mode: "dispatched", field: "generation", wantErr: api.ErrConflict},
		{mode: "dispatched", field: "missing-generation", wantErr: api.ErrConflict},
		{mode: "dispatched", field: "parent", wantErr: api.ErrUnavailable},
		{mode: "checkpointed", field: "source", wantErr: api.ErrConflict},
		{mode: "checkpointed", field: "logicalId", wantErr: api.ErrConflict},
		{mode: "fanout", field: "source", wantErr: api.ErrConflict},
		{mode: "fanout", field: "generation", wantErr: api.ErrConflict},
	}
	for engine, fixture := range newStoreFixtures(t) {
		for _, testCase := range cases {
			t.Run(engine+"/"+testCase.mode+"/"+testCase.field, func(t *testing.T) {
				backend := newBackendFixture(t, fixture)
				model := mustModel(t, backend, "modern-identity")
				version := mustVersion(t, backend, model, "v1")
				var primary *openapi.Artifact
				if testCase.mode == "fanout" {
					second := mustVersion(t, backend, model, "v2")
					primary = sharedPrimary(t, backend, model, version, second, "s3://bucket/original")
				}
				if testCase.mode == "checkpointed" {
					backend.stub.failNextRequests("POST model-versions/set-tag", 1)
				} else {
					backend.stub.mu.Lock()
					backend.stub.commitThenFail = 1
					backend.stub.mu.Unlock()
				}
				var err error
				if primary == nil {
					_, err = attachWeights(t, backend.service, version, "weights", "s3://bucket/original")
				} else {
					_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/replacement")}}, *version.Id)
				}
				require.ErrorIs(t, err, api.ErrUnavailable)
				var operation mlflow.Operation
				if primary == nil {
					operation = materializeOperationOf(t, backend)
				} else {
					require.NoError(t, backend.db.Where("kind = ? AND step = ?", mlflow.OperationKindVersionReplaceChild, mlflow.StepCreateDispatched).First(&operation).Error)
				}
				backend.stub.mu.Lock()
				for _, raw := range backend.stub.versions[model.Name] {
					record := raw.(map[string]any)
					tags := record["tags"].(map[string]string)
					if tags["rhoai.operation"] != operation.OperationUUID {
						continue
					}
					switch testCase.field {
					case "source":
						record["source"] = "s3://foreign/source"
					case "parent":
						record["name"] = "foreign-parent"
					case "missing-generation":
						delete(tags, "rhoai.generation")
					default:
						tags["rhoai."+testCase.field] = "999999999"
					}
				}
				backend.stub.mu.Unlock()
				creates := backend.stub.countRequests("POST model-versions/create")
				writes := backend.stub.countRequests("POST model-versions/set-tag")
				beforeBinding := bindingOf(t, backend, *version.Id)
				expireLeases(t, backend)
				err = backend.newBackendService(t).ApplyOperation(t.Context(), operation.OperationUUID)
				require.ErrorIs(t, err, testCase.wantErr)
				assert.Equal(t, creates, backend.stub.countRequests("POST model-versions/create"))
				assert.Equal(t, writes, backend.stub.countRequests("POST model-versions/set-tag"))
				assert.Equal(t, beforeBinding, bindingOf(t, backend, *version.Id))
				after, err := fixture.store.GetOperation(t.Context(), operation.OperationUUID)
				require.NoError(t, err)
				assert.NotEqual(t, mlflow.OperationSucceeded, after.State)
				assert.Equal(t, operation.CandidateJSON, after.CandidateJSON)
				assert.Equal(t, operation.NativeResult, after.NativeResult)
			})
		}
	}
}

func TestNativeProjectionLegacyReplacementRepairsRetainedTags(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "legacy-retained")
			version := mustVersion(t, backend, model, "v1")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/original")
			backend.stub.mu.Lock()
			backend.stub.versions[model.Name]["1"].(map[string]any)["tags"].(map[string]string)["external-note"] = "retained from original"
			backend.stub.commitThenFail = 1
			backend.stub.mu.Unlock()
			_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/replacement")}}, *version.Id)
			require.ErrorIs(t, err, api.ErrUnavailable)
			var operation mlflow.Operation
			require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionReplace).First(&operation).Error)
			var candidate mlflow.MutationCandidate
			require.NoError(t, json.Unmarshal([]byte(operation.CandidateJSON), &candidate))
			candidate.CodecVersion, candidate.NativeWork = 2, nil
			candidate.ReadRevisions, candidate.BindingReadRevisions = nil, nil
			for i := range candidate.Objects {
				candidate.Objects[i].Snapshot.CodecVersion = 2
			}
			payload, err := json.Marshal(candidate)
			require.NoError(t, err)
			require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", operation.OperationUUID).Updates(map[string]any{"candidate_json": string(payload), "payload_hash": "legacy-original", "request_hash": "", "execution_plan_json": nil}).Error)
			require.NoError(t, backend.db.Where("root_operation_uuid = ?", operation.OperationUUID).Delete(&mlflow.IntentResource{}).Error)
			backend.stub.mu.Lock()
			tags := backend.stub.versions[model.Name]["2"].(map[string]any)["tags"].(map[string]string)
			delete(tags, "external-note") // Old executor silently ignored a failed prior read.
			delete(tags, "rhoai.generation")
			backend.stub.mu.Unlock()
			adapter, err := fixture.store.GetAdapter(t.Context())
			require.NoError(t, err)
			_, err = fixture.store.InitializeAdapter(t.Context(), adapter.TargetFingerprint)
			require.NoError(t, err)
			expireLeases(t, backend)
			require.NoError(t, backend.newBackendService(t).ApplyOperation(t.Context(), operation.OperationUUID))
			assert.Equal(t, "retained from original", backend.stub.versionTags(model.Name, "2")["external-note"])
			after, err := fixture.store.GetOperation(t.Context(), operation.OperationUUID)
			require.NoError(t, err)
			require.NotNil(t, after.ExecutionPlanJSON)
			var plan mlflow.NativeVersionPlan
			require.NoError(t, json.Unmarshal([]byte(*after.ExecutionPlanJSON), &plan))
			assert.Equal(t, "retained from original", plan.RetainedTags["external-note"])
			assert.Equal(t, string(payload), after.CandidateJSON)
			assert.Equal(t, "legacy-original", after.PayloadHash)
			assert.Equal(t, fmt.Sprint(2), backend.stub.versionTags(model.Name, "2")["rhoai.generation"])
			assert.Equal(t, 2, backend.stub.countRequests("POST model-versions/create"))
		})
	}
}
