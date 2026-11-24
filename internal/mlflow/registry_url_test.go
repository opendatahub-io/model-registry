package mlflow_test

import (
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/require"
)

func TestRegistryURLRedaction(t *testing.T) {
	server := httptest.NewServer(nil)
	t.Cleanup(server.Close)
	native, err := client.New(server.URL, server.Client())
	require.NoError(t, err)
	for _, raw := range []string{
		"https://dummy-user:dummy-password@registry.example/prefix",
		"https://dummy-user:dummy-password%zz@registry.example",
		"https://registry.example/prefix?token=dummy-token",
		"https://registry.example/prefix?",
		"https://registry.example/#dummy-fragment",
		"dummy-secret://registry.example",
		"https://dummy-secret%zz.example",
		"https://[dummy-secret",
		"https://[dummy-secret]/",
		"https://registry.example:dummy-secret/",
		"https://:443/dummy-secret",
		"https:///dummy-secret",
	} {
		t.Run(raw, func(t *testing.T) {
			_, fingerprintErr := mlflow.RegistryTargetFingerprint(raw)
			_, serviceErr := mlflow.NewService(mlflow.Config{RegistryURI: raw}, &mlflow.Store{}, native)
			_, clientErr := client.New(raw, nil)
			for _, err := range []error{fingerprintErr, serviceErr, clientErr} {
				require.ErrorIs(t, err, api.ErrBadRequest)
				require.NotContains(t, err.Error(), raw)
				for _, secret := range []string{"dummy-user", "dummy-password", "dummy-token", "dummy-fragment", "dummy-secret", "registry.example"} {
					require.NotContains(t, err.Error(), secret)
				}
			}
		})
	}
}

func TestRegistryURLPreservesPrefixAndFingerprint(t *testing.T) {
	canonical, err := mlflow.RegistryTargetFingerprint("https://registry.example/team/registry")
	require.NoError(t, err)
	for _, raw := range []string{"https://REGISTRY.example:443/team/registry/", "https://registry.example/team/./registry"} {
		fingerprint, err := mlflow.RegistryTargetFingerprint(raw)
		require.NoError(t, err)
		require.Equal(t, canonical, fingerprint)
	}
}

func TestRegistryURLRedactionSourceInvariants(t *testing.T) {
	const original = "https://storage.example/original?signature=dummy-original-secret"
	const other = "https://storage.example/other?signature=dummy-other-secret"
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			t.Run("attachment", func(t *testing.T) {
				backend := newBackendFixture(t, fixture)
				model := mustModel(t, backend, "redacted-source")
				one := mustVersion(t, backend, model, "one")
				artifact, err := attachWeights(t, backend.service, one, "weights", original)
				require.NoError(t, err)
				two := mustVersion(t, backend, model, "two")
				_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: artifact.ModelArtifact.Id, Uri: new(other)}}, *two.Id)
				require.ErrorIs(t, err, api.ErrConflict)
				for _, secret := range []string{original, other, "dummy-original-secret", "dummy-other-secret"} {
					require.NotContains(t, err.Error(), secret)
				}
			})
			t.Run("operator", func(t *testing.T) {
				backend := newBackendFixture(t, fixture)
				model := mustModel(t, backend, "redacted-operator")
				version := mustVersion(t, backend, model, "one")
				backend.stub.commitThenFail = 1
				_, err := attachWeights(t, backend.service, version, "weights", original)
				require.ErrorIs(t, err, api.ErrUnavailable)
				expireLeases(t, backend)
				var operation mlflow.Operation
				require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionMaterialize).First(&operation).Error)
				backend.stub.versionRecord(model.Name, "1")["source"] = other
				err = backend.service.BindNativeIdentity(t.Context(), operation.OperationUUID, model.Name, "1")
				require.ErrorIs(t, err, api.ErrConflict)
				for _, secret := range []string{original, other, "dummy-original-secret", "dummy-other-secret"} {
					require.NotContains(t, err.Error(), secret)
				}
				require.Contains(t, err.Error(), operation.OperationUUID)
				require.Contains(t, err.Error(), strconv.FormatInt(int64(operation.ResourceID), 10))
			})
			t.Run("publication", func(t *testing.T) {
				fixture.cleanup(t)
				pieces := newAggregateFixture(t, fixture)
				result := seedSucceededResultOperation(t, fixture, pieces.version,
					"27589fc6-ab04-42cc-9f8f-5f6a2c0e0f01", mlflow.NativeBindingResult{ModelName: "native-model", VersionNumber: "5", Source: other, Generation: 1, Status: "READY"})
				candidate := mlflow.MutationCandidate{CodecVersion: 2, History: []mlflow.HistoryChange{{
					Value: mlflow.VersionHistory{VersionID: pieces.version.ID, Generation: 1, NativeModelName: "native-model", NativeVersionNumber: "5", Source: original, OperationUUID: "27589fc6-ab04-42cc-9f8f-5f6a2c0e0f02"}, ResultOperationUUID: result.OperationUUID,
				}}}
				operation, err := fixture.store.PrepareOperation(t.Context(), mlflow.Operation{OperationUUID: "27589fc6-ab04-42cc-9f8f-5f6a2c0e0f02", ResourceID: pieces.version.ID, Kind: "version-materialize", ExpectedRevision: pieces.version.Revision, CandidateJSON: mustJSON(t, candidate), PayloadHash: "redaction-test", Generation: 1})
				require.NoError(t, err)
				claimed, err := fixture.store.ClaimOperation(t.Context(), operation.OperationUUID, "redaction-worker", time.Minute)
				require.NoError(t, err)
				err = fixture.store.PublishOperation(t.Context(), operation.OperationUUID, claimed.FencingToken)
				require.ErrorIs(t, err, api.ErrConflict)
				for _, secret := range []string{original, other, "dummy-original-secret", "dummy-other-secret"} {
					require.NotContains(t, err.Error(), secret)
				}
			})
		})
	}
}
