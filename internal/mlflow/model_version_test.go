package mlflow_test

import (
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func versionState() *openapi.ModelVersionState {
	state := openapi.MODELVERSIONSTATE_LIVE
	return &state
}

// mustModel creates a committed registered model for version tests.
func mustModel(t *testing.T, backend *backendFixture, name string) *openapi.RegisteredModel {
	t.Helper()
	model, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: name})
	require.NoError(t, err)
	return model
}

// Logical names travel in tags and safe aliases; registered model names
// retain the native slash/colon restriction.
func TestLogicalVersionName(t *testing.T) {
	check := func(t *testing.T, service api.ModelRegistryApi) {
		t.Helper()
		model, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "logical-names"})
		require.NoError(t, err)
		for _, name := range []string{"release/2026", "release:2026"} {
			version, err := service.UpsertModelVersion(&openapi.ModelVersion{Name: name}, model.Id)
			require.NoError(t, err)
			_, err = service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Name: new(name), Uri: new("s3://bucket/" + name),
			}}, *version.Id)
			require.NoError(t, err)
			got, err := service.GetModelVersionById(*version.Id)
			require.NoError(t, err)
			assert.Equal(t, name, got.Name)
		}
	}
	t.Run("EmbedMD", func(t *testing.T) {
		service, cleanup := setupEmbedMDService(t)
		defer cleanup()
		check(t, service)
	})
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			check(t, backend.service)
			require.NoError(t, backend.service.Refresh(t.Context()))
			for _, name := range []string{"release/2026", "release:2026"} {
				_, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: name})
				require.ErrorIs(t, err, api.ErrBadRequest)
			}
			aliases := backend.stub.aliases("logical-names")
			require.Len(t, aliases, 2)
			for alias := range aliases {
				assert.NotContains(t, alias, "/")
				assert.NotContains(t, alias, ":")
			}
			assert.Equal(t, "release/2026", backend.stub.versionTags("logical-names", "1")["rhoai.versionName"])
			assert.Equal(t, "release:2026", backend.stub.versionTags("logical-names", "2")["rhoai.versionName"])
		})
	}
}

// TestPendingVersionCreate proves a new version commits its logical row
// and PENDING_SOURCE binding atomically in SQL: no native
// version-create request is issued, no placeholder source is supplied,
// and the version survives a restart with a new service and store over
// the same database.
func TestPendingVersionCreate(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "served-model")
			nativeVersionCreates := backend.stub.countRequests("POST model-versions")
			require.Zero(t, nativeVersionCreates)

			pending, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name:        "1.0.0",
				Author:      new("author@example.com"),
				ExternalId:  new("version-ext"),
				State:       versionState(),
				Description: new("first version"),
				CustomProperties: map[string]openapi.MetadataValue{
					"build": intValue("42"),
				},
			}, model.Id)
			require.NoError(t, err)
			require.NotNil(t, pending.Id)
			require.Equal(t, "1.0.0", pending.Name)
			require.Equal(t, *model.Id, pending.RegisteredModelId)
			require.Equal(t, "author@example.com", *pending.Author)
			require.Equal(t, "version-ext", *pending.ExternalId)
			require.Equal(t, string(openapi.MODELVERSIONSTATE_LIVE), string(*pending.State))
			require.Equal(t, "42", pending.CustomProperties["build"].MetadataIntValue.IntValue)

			// No native version create was dispatched for a pending
			// version, and no tracking endpoint was touched.
			require.Zero(t, backend.stub.countRequests("model-versions"),
				"a pending version issues no native version requests")

			// The binding is committed as PENDING_SOURCE with the
			// deterministic alias and no native identity.
			var binding mlflow.VersionBinding
			require.NoError(t, backend.db.Where("version_id = ?", mustObjectID(t, pending.Id)).
				First(&binding).Error)
			require.Equal(t, mlflow.PhasePendingSource, binding.Phase)
			require.Nil(t, binding.NativeModelName)
			require.Nil(t, binding.NativeVersionNumber)
			require.Equal(t, int64(1), binding.Revision)
			assert.NotEmpty(t, binding.Alias)

			// A restart with a new service and store over the same
			// database returns the same version.
			other := backend.newBackendService(t)
			restarted, err := other.GetModelVersionById(*pending.Id)
			require.NoError(t, err)
			require.Equal(t, pending, restarted)

			_, err = other.GetModelVersionById("not-an-id")
			require.ErrorIs(t, err, api.ErrBadRequest)
			_, err = other.GetModelVersionById("888888888")
			require.ErrorIs(t, err, api.ErrNotFound)
		})
	}
}

// deleteNativeVersion removes one native model-version record directly,
// simulating an external deletion the adapter can only observe later.
func deleteNativeVersion(t *testing.T, backend *backendFixture, model, number string) {
	t.Helper()
	backend.stub.mu.Lock()
	defer backend.stub.mu.Unlock()
	_, exists := backend.stub.versions[model][number]
	require.True(t, exists, "native version %s/%s must exist", model, number)
	delete(backend.stub.versions[model], number)
}

// objectRow loads the raw object row, including deleted and reserved
// rows, for direct SQL assertions.
func objectRow(t *testing.T, backend *backendFixture, id int32) mlflow.Object {
	t.Helper()
	var row mlflow.Object
	require.NoError(t, backend.db.First(&row, id).Error)
	return row
}

// TestModelVersionDeletionObservationFenced proves a direct-read native
// 404 tombstones a logical version only when SQL arbitration confirms
// the observation is still current: the object is committed at the
// observed revision, the binding still carries the exact native
// identity that disappeared, and no operation holds a reservation on
// the resource. A stale 404 from an old native generation must never
// tombstone a version a replacement has since re-bound.
func TestModelVersionDeletionObservationFenced(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			// A replacement completing while the old generation's 404
			// response is held must keep the version healthy: the read
			// loses arbitration against the new binding.
			t.Run("replacement completes during held stale 404", func(t *testing.T) {
				backend := newBackendFixture(t, fixture)
				model := mustModel(t, backend, "fenced-replacement")
				version := mustVersion(t, backend, model, "1.0.0")
				primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/original")
				binding := bindingOf(t, backend, *version.Id)
				versionID := mustObjectID(t, version.Id)

				entered, release := backend.stub.requestBarrier("GET model-versions/get")
				defer release()
				read := make(chan error, 1)
				go func() {
					_, err := backend.service.GetModelVersionById(*version.Id)
					read <- err
				}()
				waitRefreshBarrier(t, entered)
				// Later requests pass freely; the held handler keeps its
				// captured gate and its response stays a stale 404.
				backend.stub.mu.Lock()
				delete(backend.stub.gates, "GET model-versions/get")
				backend.stub.mu.Unlock()

				other := backend.newBackendService(t)
				_, err := other.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
					Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/replacement"),
				}}, *version.Id)
				require.NoError(t, err, "the replacement must complete while the stale 404 is held")
				rebound := bindingOf(t, backend, *version.Id)
				require.Equal(t, binding.ActiveGeneration+1, rebound.ActiveGeneration)
				require.Equal(t, "2", *rebound.NativeVersionNumber)
				afterReplacement := objectRow(t, backend, versionID)

				// The old native generation disappears only now: the held
				// reader still checked the binding it loaded before the
				// replacement, so its response is a stale 404.
				deleteNativeVersion(t, backend, model.Name, *binding.NativeVersionNumber)
				release()
				require.NoError(t, <-read, "a stale 404 must not fail a healthy re-bound version")

				row := objectRow(t, backend, versionID)
				require.False(t, row.Deleted, "a stale 404 must not tombstone a re-bound version")
				require.Equal(t, afterReplacement, row, "the observation must not revise committed state")
				require.Equal(t, rebound, bindingOf(t, backend, *version.Id),
					"the new binding must survive the stale observation")

				require.NoError(t, backend.service.Refresh(t.Context()), "refresh must not lose the re-bound version")
				require.Equal(t, rebound, bindingOf(t, backend, *version.Id))
				view, err := backend.service.GetModelVersionById(*version.Id)
				require.NoError(t, err)
				require.Equal(t, *version.Id, *view.Id)
			})

			// A replacement prepared but not published holds a
			// reservation: the observation defers instead of tombstoning
			// the version the pending operation still owns.
			t.Run("replacement prepared but not published", func(t *testing.T) {
				backend := newBackendFixture(t, fixture)
				model := mustModel(t, backend, "fenced-pending")
				version := mustVersion(t, backend, model, "1.0.0")
				primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/original")
				binding := bindingOf(t, backend, *version.Id)
				versionID := mustObjectID(t, version.Id)

				entered, release := backend.stub.requestBarrier("GET model-versions/get")
				defer release()
				read := make(chan error, 1)
				go func() {
					_, err := backend.service.GetModelVersionById(*version.Id)
					read <- err
				}()
				waitRefreshBarrier(t, entered)
				backend.stub.mu.Lock()
				delete(backend.stub.gates, "GET model-versions/get")
				backend.stub.commitThenFail = 1
				backend.stub.mu.Unlock()

				other := backend.newBackendService(t)
				_, err := other.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
					Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/pending"),
				}}, *version.Id)
				require.ErrorIs(t, err, api.ErrUnavailable, "the replacement is left recoverable")

				// The old generation disappears after the pending
				// operation recorded its reservation but before the held
				// response is released.
				deleteNativeVersion(t, backend, model.Name, *binding.NativeVersionNumber)
				release()
				require.NoError(t, <-read, "an observation against a reserved version must defer, not delete")
				row := objectRow(t, backend, versionID)
				require.False(t, row.Deleted)

				// The deferred observation left the pending replacement
				// recoverable and the version readable: every later read
				// keeps deferring while the reservation holds.
				var operation mlflow.Operation
				require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionReplace).
					First(&operation).Error)
				require.Equal(t, mlflow.OperationRunning, operation.State,
					"the interrupted replacement stays recoverable")
				view, err := backend.service.GetModelVersionById(*version.Id)
				require.NoError(t, err, "a read against the reserved version keeps deferring")
				require.Equal(t, *version.Id, *view.Id)
				require.False(t, objectRow(t, backend, versionID).Deleted)
			})

			// A metadata revision advancing during the read invalidates
			// the observation: the read returns the current committed
			// view without revising anything.
			t.Run("metadata revision advances during read", func(t *testing.T) {
				backend := newBackendFixture(t, fixture)
				model := mustModel(t, backend, "fenced-metadata")
				version := mustVersion(t, backend, model, "1.0.0")
				mustPrimaryArtifact(t, backend, version, "s3://bucket/model")
				binding := bindingOf(t, backend, *version.Id)
				versionID := mustObjectID(t, version.Id)

				entered, release := backend.stub.requestBarrier("GET model-versions/get")
				defer release()
				type readResult struct {
					view *openapi.ModelVersion
					err  error
				}
				read := make(chan readResult, 1)
				go func() {
					view, err := backend.service.GetModelVersionById(*version.Id)
					read <- readResult{view: view, err: err}
				}()
				waitRefreshBarrier(t, entered)
				backend.stub.mu.Lock()
				delete(backend.stub.gates, "GET model-versions/get")
				backend.stub.mu.Unlock()

				other := backend.newBackendService(t)
				_, err := other.UpsertModelVersion(&openapi.ModelVersion{
					Id: version.Id, Name: version.Name, Author: new("concurrent-editor"),
				}, model.Id)
				require.NoError(t, err)
				edited := objectRow(t, backend, versionID)
				require.Greater(t, edited.Revision, binding.Revision)

				deleteNativeVersion(t, backend, model.Name, *binding.NativeVersionNumber)
				release()
				result := <-read
				require.NoError(t, result.err, "an invalidated observation must return the current view")
				require.Equal(t, "concurrent-editor", *result.view.Author)
				row := objectRow(t, backend, versionID)
				require.False(t, row.Deleted)
				require.Equal(t, edited, row, "the observation must not revise the concurrent edit")
			})

			// A genuine, unchanged native deletion tombstones the version
			// exactly once, and later reads stay not-found.
			t.Run("genuine native deletion", func(t *testing.T) {
				backend := newBackendFixture(t, fixture)
				model := mustModel(t, backend, "fenced-genuine")
				version := mustVersion(t, backend, model, "1.0.0")
				mustPrimaryArtifact(t, backend, version, "s3://bucket/model")
				binding := bindingOf(t, backend, *version.Id)
				versionID := mustObjectID(t, version.Id)
				before := objectRow(t, backend, versionID)

				deleteNativeVersion(t, backend, model.Name, *binding.NativeVersionNumber)
				_, err := backend.service.GetModelVersionById(*version.Id)
				require.ErrorIs(t, err, api.ErrNotFound)

				row := objectRow(t, backend, versionID)
				require.True(t, row.Deleted)
				require.Equal(t, before.Revision+1, row.Revision, "deletion advances the revision exactly once")

				_, err = backend.service.GetModelVersionById(*version.Id)
				require.ErrorIs(t, err, api.ErrNotFound)
				require.NoError(t, backend.service.Refresh(t.Context()))
				require.Empty(t, committedObjects(t, backend, mlflow.KindModelVersion))
			})

			// A native read that fails for any other reason still hides
			// behind the committed local observation.
			t.Run("non-404 native error preserves the view", func(t *testing.T) {
				backend := newBackendFixture(t, fixture)
				model := mustModel(t, backend, "fenced-unavailable")
				version := mustVersion(t, backend, model, "1.0.0")
				mustPrimaryArtifact(t, backend, version, "s3://bucket/model")

				backend.stub.failNextRequests("GET model-versions/get", 1)
				view, err := backend.service.GetModelVersionById(*version.Id)
				require.NoError(t, err)
				require.Equal(t, *version.Id, *view.Id)
			})
		})
	}
}

func mustObjectID(t *testing.T, id *string) int32 {
	t.Helper()
	parsed, err := parseID(*id)
	require.NoError(t, err)
	return parsed
}

// TestPendingVersionNaming proves name uniqueness is scoped to the
// parent model: the same name under different parents succeeds, a
// duplicate under one parent conflicts, and an invalid parent fails
// cleanly.
func TestPendingVersionNaming(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			first := mustModel(t, backend, "model-one")
			second := mustModel(t, backend, "model-two")

			version, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name: "1.0.0",
			}, first.Id)
			require.NoError(t, err)

			// The same name under a different parent is a distinct
			// version.
			other, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name: "1.0.0",
			}, second.Id)
			require.NoError(t, err)
			require.NotEqual(t, *version.Id, *other.Id)

			// A duplicate name under one parent conflicts rather than
			// creating a second row, matching the platform contract.
			_, err = backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name: "1.0.0", Author: new("editor@example.com"),
			}, first.Id)
			require.Error(t, err)
			assert.Equal(t, httpConflict, api.ErrToStatus(err))

			// Invalid parents fail cleanly without creating rows.
			_, err = backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name: "2.0.0",
			}, new("not-an-id"))
			require.ErrorIs(t, err, api.ErrBadRequest)
			_, err = backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name: "2.0.0",
			}, new("777777777"))
			require.ErrorIs(t, err, api.ErrNotFound)
			_, err = backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name: "2.0.0",
			}, nil)
			require.ErrorIs(t, err, api.ErrBadRequest)
			// A version id is not a model id.
			_, err = backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name: "2.0.0",
			}, version.Id)
			require.ErrorIs(t, err, api.ErrBadRequest)

			var versions int64
			require.NoError(t, backend.db.Model(&mlflow.Object{}).
				Where("kind = ?", mlflow.KindModelVersion).Count(&versions).Error)
			require.Equal(t, int64(2), versions)
		})
	}
}

// TestVersionMetadataPendingEditsStayInSQL proves pending metadata
// edits publish in SQL only: fields round-trip through the snapshot,
// the binding phase is untouched, and no native request is issued
// beyond the parent model's create.
func TestVersionMetadataPendingEditsStayInSQL(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "editable-model")
			pending, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name:        "1.0.0",
				Description: new("before"),
				Author:      new("a@example.com"),
				CustomProperties: map[string]openapi.MetadataValue{
					"stage": stringValue("dev"),
				},
			}, model.Id)
			require.NoError(t, err)
			requestsBefore := len(backend.stub.requests())

			edited, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Id:          pending.Id,
				Name:        "1.0.0",
				Description: new("after"),
				Author:      new("b@example.com"),
				ExternalId:  new("added-ext"),
				CustomProperties: map[string]openapi.MetadataValue{
					"stage": stringValue("prod"),
					"extra": boolValue(true),
				},
			}, model.Id)
			require.NoError(t, err)
			require.Equal(t, *pending.Id, *edited.Id)
			require.Equal(t, "after", *edited.Description)
			require.Equal(t, "b@example.com", *edited.Author)
			require.Equal(t, "added-ext", *edited.ExternalId)
			require.Equal(t, "prod", edited.CustomProperties["stage"].MetadataStringValue.StringValue)
			require.Equal(t, true, edited.CustomProperties["extra"].MetadataBoolValue.BoolValue)

			// The edit issued no native requests.
			require.Equal(t, requestsBefore, len(backend.stub.requests()),
				"pending metadata edits stay in SQL")

			// The binding remains pending with no native identity.
			var binding mlflow.VersionBinding
			require.NoError(t, backend.db.Where("version_id = ?", mustObjectID(t, pending.Id)).
				First(&binding).Error)
			require.Equal(t, mlflow.PhasePendingSource, binding.Phase)
			require.Nil(t, binding.NativeModelName)

			// A restart sees the edited view.
			other := backend.newBackendService(t)
			restarted, err := other.GetModelVersionById(*pending.Id)
			require.NoError(t, err)
			require.Equal(t, "after", *restarted.Description)

			// The name is a protected identity: a supplied rename is
			// advisory and the stored name is kept.
			renamed, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name: "2.0.0", Id: pending.Id,
			}, model.Id)
			require.NoError(t, err)
			require.Equal(t, "1.0.0", renamed.Name)

			// The stored parent is authoritative: a supplied mismatched
			// registered model id is advisory on an update by id.
			otherModel := mustModel(t, backend, "other-parent")
			still, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name: "1.0.0", Id: pending.Id,
			}, otherModel.Id)
			require.NoError(t, err)
			require.Equal(t, *model.Id, still.RegisteredModelId)
		})
	}
}
