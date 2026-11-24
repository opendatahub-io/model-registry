package mlflow_test

import (
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// historiesOf loads the version-history rows of one logical version in
// generation order.
func historiesOf(t *testing.T, backend *backendFixture, versionID string) []mlflow.VersionHistory {
	t.Helper()
	var histories []mlflow.VersionHistory
	require.NoError(t, backend.db.Where("version_id = ?", mustObjectID(t, new(versionID))).
		Order("generation").Find(&histories).Error)
	return histories
}

// TestReplacePrimarySource proves a changed primary source is a
// replacement: the public artifact and version identities stay stable,
// exactly one new native version number is created, the previous
// history generation is superseded but retained, and the persisted
// adapter alias moves to the new native number only once it is ready.
// Repeating the committed URI or editing metadata creates no
// generation, and clearing the URI is rejected without changing
// committed state.
func TestReplacePrimarySource(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "replaced")
			version := mustVersion(t, backend, model, "1.0.0")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/old.tar.gz")
			artifactID := *primary.ModelArtifact.Id
			binding := bindingOf(t, backend, *version.Id)
			assert.Equal(t, int64(1), binding.ActiveGeneration)
			assert.Equal(t, 1, len(backend.stub.versions["replaced"]))

			replaced, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Id: new(artifactID), Name: new("primary"),
					Uri:         new("s3://bucket/new.tar.gz"),
					Description: new("second generation"),
				},
			}, *version.Id)
			require.NoError(t, err)

			// Stable public identities, new source.
			assert.Equal(t, artifactID, *replaced.ModelArtifact.Id)
			assert.Equal(t, "s3://bucket/new.tar.gz", *replaced.ModelArtifact.Uri)
			after := bindingOf(t, backend, *version.Id)
			assert.Equal(t, int64(2), after.ActiveGeneration)
			require.NotNil(t, after.NativeVersionNumber)
			assert.Equal(t, "2", *after.NativeVersionNumber, "one new native number")

			// Exactly one native create beyond the first materialization.
			assert.Equal(t, 2, backend.stub.countRequests("POST model-versions/create"))
			assert.Equal(t, 2, len(backend.stub.versions["replaced"]))

			// The old generation is superseded but retained.
			histories := historiesOf(t, backend, *version.Id)
			require.Len(t, histories, 2)
			assert.True(t, histories[0].Superseded, "old history retained, superseded")
			assert.Equal(t, "s3://bucket/old.tar.gz", histories[0].Source)
			assert.False(t, histories[1].Superseded)
			assert.Equal(t, "s3://bucket/new.tar.gz", histories[1].Source)

			// The persisted adapter alias points at the new number.
			assert.Equal(t, "2", backend.stub.aliases("replaced")[binding.Alias])

			// Repeating the committed URI with edited metadata creates no
			// generation.
			edited, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Id: new(artifactID), Name: new("primary"),
					Uri:         new("s3://bucket/new.tar.gz"),
					Description: new("annotated"),
				},
			}, *version.Id)
			require.NoError(t, err)
			assert.Equal(t, "annotated", *edited.ModelArtifact.Description)
			assert.Equal(t, int64(2), bindingOf(t, backend, *version.Id).ActiveGeneration)
			assert.Equal(t, 2, backend.stub.countRequests("POST model-versions/create"))
			assert.Len(t, historiesOf(t, backend, *version.Id), 2)

			// An omitted URI retains the committed source and generation.
			retained, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{Id: new(artifactID)},
			}, *version.Id)
			require.NoError(t, err)
			assert.Equal(t, "s3://bucket/new.tar.gz", *retained.ModelArtifact.Uri)
			assert.Equal(t, int64(2), bindingOf(t, backend, *version.Id).ActiveGeneration)
			assert.Equal(t, 2, backend.stub.countRequests("POST model-versions/create"))

			// Clearing the materialized primary URI is rejected without
			// changing committed state.
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Id: new(artifactID), Name: new("primary"), Uri: new(""),
				},
			}, *version.Id)
			require.ErrorIs(t, err, api.ErrConflict)
			cleared, err := backend.service.GetModelArtifactById(artifactID)
			require.NoError(t, err)
			assert.Equal(t, "s3://bucket/new.tar.gz", *cleared.Uri)
			assert.Equal(t, int64(2), bindingOf(t, backend, *version.Id).ActiveGeneration)
		})
	}
}

// TestReplaceStandaloneURIChange proves a standalone artifact's source
// changes in place with no parent argument and no native work: the
// artifact has no version link, so the edit is SQL-only.
func TestReplaceStandaloneURIChange(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			created, err := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{
				Name: new("standalone"), Uri: new("s3://bucket/first.tar.gz"),
			})
			require.NoError(t, err)

			updated, err := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{
				Id: created.Id, Name: new("standalone"),
				Uri: new("s3://bucket/second.tar.gz"),
			})
			require.NoError(t, err)
			assert.Equal(t, "s3://bucket/second.tar.gz", *updated.Uri)
			assert.Equal(t, *created.Id, *updated.Id)

			// No native version was ever created or searched.
			assert.Zero(t, backend.stub.countRequests("POST model-versions/create"))
			assert.Zero(t, backend.stub.countRequests("GET model-versions/search"))
		})
	}
}

// TestReplaceWaitsForReady proves the alias moves only after the new
// generation is ready: a replacement whose native create stays in
// PENDING_REGISTRATION is not published, and the alias still points at
// the old number until registration completes.
func TestReplaceWaitsForReady(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "pending-replace")
			version := mustVersion(t, backend, model, "1.0.0")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/old.tar.gz")
			binding := bindingOf(t, backend, *version.Id)

			backend.stub.mu.Lock()
			backend.stub.createStatus = "PENDING_REGISTRATION"
			backend.stub.mu.Unlock()

			// The replacement create stays pending: awaitReady polls until
			// its deadline and the write stays unavailable.
			_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Id: primary.ModelArtifact.Id, Name: new("primary"),
					Uri: new("s3://bucket/new.tar.gz"),
				},
			}, *version.Id)
			require.ErrorIs(t, err, api.ErrUnavailable)

			// Nothing switched: the public URI, binding, and alias still
			// describe the first generation.
			visible, err := backend.service.GetModelArtifactById(*primary.ModelArtifact.Id)
			require.NoError(t, err)
			assert.Equal(t, "s3://bucket/old.tar.gz", *visible.Uri)
			stillFirst := bindingOf(t, backend, *version.Id)
			assert.Equal(t, int64(1), stillFirst.ActiveGeneration)
			assert.Equal(t, "1", backend.stub.aliases("pending-replace")[binding.Alias])

			// Registration completes and a restart drives the recorded
			// intent to publication; only then does the alias move.
			backend.stub.mu.Lock()
			backend.stub.flipStatus = true
			backend.stub.mu.Unlock()
			restarted := backend.newBackendService(t)
			expireLeases(t, backend)
			var operation mlflow.Operation
			require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionReplace).
				First(&operation).Error)
			require.NoError(t, restarted.ApplyOperation(t.Context(), operation.OperationUUID))
			moved := bindingOf(t, backend, *version.Id)
			assert.Equal(t, int64(2), moved.ActiveGeneration)
			assert.Equal(t, "2", backend.stub.aliases("pending-replace")[binding.Alias])
		})
	}
}
