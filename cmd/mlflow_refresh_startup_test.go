package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/require"
)

// Exercise the real factory: initial import precedes the recovery worker.
// Successful children and uncertain creates both still belong to the root.
func TestStartupPartialFanOut(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			for _, uncertain := range []bool{false, true} {
				t.Run(map[bool]string{false: "children-ready", true: "create-uncertain"}[uncertain], func(t *testing.T) {
					f := newOperationsFixture(t, engine)
					model := f.mustModel(t, "startup-fanout")
					first := f.mustVersion(t, model, "first")
					second := f.mustVersion(t, model, "second")
					primary, err := f.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Name: new("shared"), Uri: new("s3://bucket/old")}}, *first.Id)
					require.NoError(t, err)
					_, err = f.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id}}, *second.Id)
					require.NoError(t, err)
					f.stub.mu.Lock()
					f.stub.failAliases = !uncertain
					if uncertain {
						f.stub.failVersionCreates = 1
					}
					f.stub.mu.Unlock()
					_, err = f.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/new")}}, *first.Id)
					require.ErrorIs(t, err, api.ErrUnavailable)
					var root mlflow.Operation
					f.firstOperation(t, mlflow.OperationKindVersionReplaceFanOut, &root)
					beforeFirst := f.bindingOfVersion(t, *first.Id)
					beforeSecond := f.bindingOfVersion(t, *second.Id)
					creates := f.stub.countRequests("POST model-versions/create")
					if !uncertain {
						var children []mlflow.Operation
						require.NoError(t, f.store.DB().Where("kind = ?", mlflow.OperationKindVersionReplaceChild).Find(&children).Error)
						require.Len(t, children, 2)
						for _, child := range children {
							require.Equal(t, mlflow.OperationSucceeded, child.State)
						}
					}
					dsn := testutils.GetSharedMySQLDSN(t)
					if engine == "postgres" {
						dsn = testutils.GetSharedPostgresDSN(t)
					}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					backend, shutdown, err := newRegistryBackend(ctx, MLflowBackendConfig{RegistryURI: f.stub.url, DatabaseType: engine, DatabaseDSN: dsn, RequestTimeout: 5 * time.Second, RefreshInterval: time.Hour})
					require.NoError(t, err, "startup must serve the prior committed view before recovery")
					defer func() { require.NoError(t, shutdown()) }()
					view, err := backend.GetModelArtifactById(*primary.ModelArtifact.Id)
					require.NoError(t, err)
					require.Equal(t, "s3://bucket/old", *view.Uri)
					require.Equal(t, beforeFirst, f.bindingOfVersion(t, *first.Id))
					require.Equal(t, beforeSecond, f.bindingOfVersion(t, *second.Id))
					if uncertain {
						require.NoError(t, shutdown())
						f.expireOperationLeases(t)
						// A direct recovery sweep is allowed to search, never to create again.
						err = f.service.ApplyOperation(t.Context(), root.OperationUUID)
						require.ErrorIs(t, err, api.ErrUnavailable)
						require.Equal(t, creates, f.stub.countRequests("POST model-versions/create"))
						retained, err := f.store.GetOperation(t.Context(), root.OperationUUID)
						require.NoError(t, err)
						require.Equal(t, root.CandidateJSON, retained.CandidateJSON)
						require.Equal(t, mlflow.OperationRunning, retained.State)
					} else {
						f.stub.mu.Lock()
						f.stub.failAliases = false
						f.stub.mu.Unlock()
						f.expireOperationLeases(t)
						require.Eventually(t, func() bool {
							op, err := f.store.GetOperation(t.Context(), root.OperationUUID)
							return err == nil && op.State == mlflow.OperationSucceeded
						}, 5*time.Second, 10*time.Millisecond)
						view, err = backend.GetModelArtifactById(*primary.ModelArtifact.Id)
						require.NoError(t, err)
						require.Equal(t, "s3://bucket/new", *view.Uri)
						require.Equal(t, creates, f.stub.countRequests("POST model-versions/create"))
					}
				})
			}
		})
	}
}
