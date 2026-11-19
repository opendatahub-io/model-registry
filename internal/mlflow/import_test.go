package mlflow_test

import (
	"strconv"
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedNativeModel inserts a native registered model directly,
// simulating a model created outside the adapter.
func seedNativeModel(t *testing.T, backend *backendFixture, name, description string,
	tags map[string]string) {
	t.Helper()
	backend.stub.mu.Lock()
	defer backend.stub.mu.Unlock()
	backend.stub.models[name] = map[string]any{
		"name": name, "description": description,
		"tags": tags, "aliases": map[string]string{},
	}
}

// seedNativeVersion inserts a native model version directly,
// simulating a version created outside the adapter.
func seedNativeVersion(t *testing.T, backend *backendFixture, model, number,
	source, description string, tags map[string]string) {
	t.Helper()
	backend.stub.mu.Lock()
	defer backend.stub.mu.Unlock()
	if backend.stub.versions[model] == nil {
		backend.stub.versions[model] = map[string]any{}
	}
	if number != "" {
		if parsed, err := strconv.Atoi(number); err == nil && parsed > backend.stub.nextVersion[model] {
			backend.stub.nextVersion[model] = parsed
		}
	}
	backend.stub.versions[model][number] = map[string]any{
		"name": model, "version": number, "source": source,
		"description": description, "status": "READY", "tags": tags,
	}
}

// adapterOf loads the adapter singleton.
func adapterOf(t *testing.T, backend *backendFixture) mlflow.Adapter {
	t.Helper()
	var adapter mlflow.Adapter
	require.NoError(t, backend.db.Order("id").First(&adapter).Error)
	return adapter
}

// committedObjects returns the committed, non-deleted object rows of
// one kind.
func committedObjects(t *testing.T, backend *backendFixture, kind mlflow.ObjectKind) []mlflow.Object {
	t.Helper()
	var rows []mlflow.Object
	require.NoError(t, backend.db.Where("kind = ? AND deleted = ? AND visibility = ?",
		kind, false, mlflow.VisibilityCommitted).Order("id").Find(&rows).Error)
	return rows
}

// versionByName finds the committed logical version of one model by
// name.
func versionByName(t *testing.T, backend *backendFixture, modelID int32,
	name string) (mlflow.Object, bool) {
	t.Helper()
	var row mlflow.Object
	err := backend.db.Where("kind = ? AND parent_id = ? AND name = ? AND deleted = ? AND visibility = ?",
		mlflow.KindModelVersion, modelID, name, false, mlflow.VisibilityCommitted).
		First(&row).Error
	return row, err == nil
}

// TestImportUntaggedNativeObjects proves a complete refresh imports
// untagged native models and versions as committed local rows with
// stable identities: decimal fallback names, explicit versionName tags,
// synthesized primary artifacts, ready bindings, first history
// generations, and unrelated tags rendered as custom properties. A
// second refresh imports nothing again.
func TestImportUntaggedNativeObjects(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "external", "created elsewhere",
				map[string]string{"team": "platform", "rhoai.externalId": "ext-1"})
			seedNativeVersion(t, backend, "external", "3",
				"s3://bucket/external-3.tar.gz", "third version", map[string]string{"stage": "prod"})
			seedNativeVersion(t, backend, "external", "4",
				"s3://bucket/external-4.tar.gz", "", map[string]string{"rhoai.versionName": "4.0.0"})

			require.NoError(t, backend.service.Refresh(t.Context()))

			models := committedObjects(t, backend, mlflow.KindRegisteredModel)
			require.Len(t, models, 1)
			model := models[0]
			assert.Equal(t, "external", model.Name)
			require.NotNil(t, model.ExternalID)
			assert.Equal(t, "ext-1", *model.ExternalID)
			view, err := backend.service.GetRegisteredModelById(strconv.Itoa(int(model.ID)))
			require.NoError(t, err)
			assert.Equal(t, "created elsewhere", *view.Description)
			assert.Equal(t, "ext-1", *view.ExternalId)
			require.Contains(t, view.CustomProperties, "team")
			assert.Equal(t, "platform",
				view.CustomProperties["team"].MetadataStringValue.StringValue)

			// The decimal fallback name and the explicit versionName tag.
			byFallback, ok := versionByName(t, backend, model.ID, "3")
			require.True(t, ok, "decimal fallback name imported")
			byTag, ok := versionByName(t, backend, model.ID, "4.0.0")
			require.True(t, ok, "explicit versionName tag honored")

			for _, row := range []mlflow.Object{byFallback, byTag} {
				binding := bindingOf(t, backend, strconv.Itoa(int(row.ID)))
				assert.Equal(t, mlflow.PhaseReady, binding.Phase)
				require.NotNil(t, binding.NativeVersionNumber)
				require.NotNil(t, binding.PrimaryArtifactID)
				histories := historiesOf(t, backend, strconv.Itoa(int(row.ID)))
				require.Len(t, histories, 1)
				assert.Equal(t, int64(1), histories[0].Generation)
				assert.False(t, histories[0].Superseded)
			}
			assert.Equal(t, "3", *bindingOf(t, backend, strconv.Itoa(int(byFallback.ID))).NativeVersionNumber)
			assert.Equal(t, "4", *bindingOf(t, backend, strconv.Itoa(int(byTag.ID))).NativeVersionNumber)

			// The synthesized primary artifact carries the native source.
			fallbackBinding := bindingOf(t, backend, strconv.Itoa(int(byFallback.ID)))
			primary, err := backend.service.GetModelArtifactById(
				strconv.Itoa(int(*fallbackBinding.PrimaryArtifactID)))
			require.NoError(t, err)
			assert.Equal(t, "s3://bucket/external-3.tar.gz", *primary.Uri)

			// A repeated refresh is a no-op: identities are stable.
			require.NoError(t, backend.service.Refresh(t.Context()))
			assert.Len(t, committedObjects(t, backend, mlflow.KindRegisteredModel), 1)
			assert.Len(t, committedObjects(t, backend, mlflow.KindModelVersion), 2)
			assert.Len(t, committedObjects(t, backend, mlflow.KindModelArtifact), 2)
		})
	}
}

// TestImportVersionWithoutSource proves a sourceless native version
// imports without a synthesized primary artifact: the binding is ready
// with no primary, and later attach flows drive it.
func TestImportVersionWithoutSource(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "bare", "", nil)
			seedNativeVersion(t, backend, "bare", "1", "", "", nil)

			require.NoError(t, backend.service.Refresh(t.Context()))
			models := committedObjects(t, backend, mlflow.KindRegisteredModel)
			require.Len(t, models, 1)
			versions := committedObjects(t, backend, mlflow.KindModelVersion)
			require.Len(t, versions, 1)
			assert.Empty(t, committedObjects(t, backend, mlflow.KindModelArtifact))
			binding := bindingOf(t, backend, strconv.Itoa(int(versions[0].ID)))
			assert.Equal(t, mlflow.PhaseReady, binding.Phase)
			assert.Nil(t, binding.PrimaryArtifactID)
		})
	}
}

// TestImportDuplicateNameIsConflict proves an untagged native version
// that would import under a name a different local version already
// holds is a conflict, never a silent second identity.
func TestImportDuplicateNameIsConflict(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "duplicated")
			version := mustVersion(t, backend, model, "1.0.0")
			mustPrimaryArtifact(t, backend, version, "s3://bucket/primary.tar.gz")

			// An untagged native version projecting the same logical
			// name under a different native number.
			seedNativeVersion(t, backend, "duplicated", "9", "s3://bucket/other.tar.gz",
				"", map[string]string{"rhoai.versionName": "1.0.0"})

			err := backend.service.Refresh(t.Context())
			require.ErrorIs(t, err, api.ErrConflict)
			_, exists := versionByName(t, backend, mustObjectID(t, model.Id), "1.0.0")
			assert.True(t, exists, "the local version is untouched")
		})
	}
}

// TestImportOwnedIdentityConflicts proves owned identity records
// resolve strictly: a missing local record and duplicate active claims
// are conflicts, and an old replacement generation is skipped, never
// re-imported.
func TestImportOwnedIdentityConflicts(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			adapter := adapterOf(t, backend)
			model := mustModel(t, backend, "owned")
			version := mustVersion(t, backend, model, "1.0.0")
			mustPrimaryArtifact(t, backend, version, "s3://bucket/old.tar.gz")
			versionID := strconv.Itoa(int(mustObjectID(t, version.Id)))

			// Old replacement generations are retained natively but
			// superseded locally: the refresh skips them.
			seedNativeVersion(t, backend, "owned", "1", "s3://bucket/old.tar.gz", "",
				map[string]string{
					"rhoai.adapter": adapter.AdapterUUID, "rhoai.logicalId": versionID,
				})
			require.NoError(t, backend.service.Refresh(t.Context()))
			assert.Len(t, committedObjects(t, backend, mlflow.KindModelVersion), 1,
				"the superseded generation is not re-imported")

			// A forged second active claim on the same logical version
			// is a conflict.
			seedNativeVersion(t, backend, "owned", "7", "s3://bucket/forged.tar.gz", "",
				map[string]string{
					"rhoai.adapter": adapter.AdapterUUID, "rhoai.logicalId": versionID,
				})
			err := backend.service.Refresh(t.Context())
			require.ErrorIs(t, err, api.ErrConflict)

			// A claim on a logical id with no local record is a
			// conflict.
			backend.stub.mu.Lock()
			delete(backend.stub.versions["owned"], "7")
			backend.stub.mu.Unlock()
			seedNativeVersion(t, backend, "owned", "8", "s3://bucket/ghost.tar.gz", "",
				map[string]string{
					"rhoai.adapter": adapter.AdapterUUID, "rhoai.logicalId": "99999",
				})
			require.ErrorIs(t, backend.service.Refresh(t.Context()), api.ErrConflict)
		})
	}
}

// TestImportSkipsUnfinishedOperations proves native results owned by
// unfinished operations are excluded from the import: a version
// carrying an operation tag whose journal entry has not succeeded, and
// the untagged next result of a model with a pending materialization.
func TestImportSkipsUnfinishedOperations(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "inflight")
			version := mustVersion(t, backend, model, "1.0.0")

			// A materialization whose native create committed but whose
			// response was dropped leaves the next native result
			// untagged and the operation unfinished.
			backend.stub.mu.Lock()
			backend.stub.commitThenFail = 1
			backend.stub.mu.Unlock()
			_, err := attachWeights(t, backend.service, version, "weights",
				"s3://bucket/weights.tar.gz")
			require.ErrorIs(t, err, api.ErrUnavailable)
			require.Len(t, backend.stub.versions["inflight"], 1,
				"the native create landed server-side")

			// An operation UUID alone cannot exempt an unrelated native result.
			operation := materializeOperationOf(t, backend)
			seedNativeVersion(t, backend, "elsewhere", "1", "s3://bucket/tagged.tar.gz", "",
				map[string]string{"rhoai.operation": operation.OperationUUID})
			seedNativeModel(t, backend, "elsewhere", "", nil)

			require.ErrorIs(t, backend.service.Refresh(t.Context()), api.ErrConflict)
			backend.stub.mu.Lock()
			delete(backend.stub.versions["elsewhere"], "1")
			backend.stub.mu.Unlock()
			require.NoError(t, backend.service.Refresh(t.Context()))
			assert.Empty(t, committedObjects(t, backend, mlflow.KindModelArtifact),
				"the in-flight result is not imported")
			_, imported := versionByName(t, backend, mustObjectID(t, model.Id), "1")
			assert.False(t, imported, "the untagged in-flight native result is not imported")

			// Completing the operation and refreshing again leaves one
			// committed identity after the forged debris was rejected.
			restarted := backend.newBackendService(t)
			expireLeases(t, backend)
			require.NoError(t, restarted.ApplyOperation(t.Context(), operation.OperationUUID))
			require.NoError(t, backend.service.Refresh(t.Context()))
			assert.Len(t, committedObjects(t, backend, mlflow.KindModelArtifact), 1)
		})
	}
}

// TestImportPreservesLocalTypes proves external metadata edits parse
// into the prior property type when valid and leave the local value
// intact when they do not, and that properties the complete native
// view no longer carries are removed.
func TestImportPreservesLocalTypes(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "typed", "edited externally",
				map[string]string{"team": "platform", "rank": "42", "score": "9.5"})

			require.NoError(t, backend.service.Refresh(t.Context()))
			models := committedObjects(t, backend, mlflow.KindRegisteredModel)
			require.Len(t, models, 1)
			view, err := backend.service.GetRegisteredModelById(strconv.Itoa(int(models[0].ID)))
			require.NoError(t, err)
			assert.Equal(t, "edited externally", *view.Description)
			require.Contains(t, view.CustomProperties, "team")
			require.Contains(t, view.CustomProperties, "rank")
			require.Contains(t, view.CustomProperties, "score")

			// Unknown native properties start as strings, so subsequent
			// edits retain their exact external text.
			seedNativeModel(t, backend, "typed", "edited again",
				map[string]string{"team": "sre", "rank": "43", "score": "not-a-number"})
			require.NoError(t, backend.service.Refresh(t.Context()))
			view, err = backend.service.GetRegisteredModelById(strconv.Itoa(int(models[0].ID)))
			require.NoError(t, err)
			assert.Equal(t, "edited again", *view.Description)
			assert.Equal(t, "sre", view.CustomProperties["team"].MetadataStringValue.StringValue)
			assert.Equal(t, "43", view.CustomProperties["rank"].MetadataStringValue.StringValue,
				"unknown imports start as strings")
			require.NotNil(t, view.CustomProperties["score"].MetadataStringValue)
			assert.Equal(t, "not-a-number", view.CustomProperties["score"].MetadataStringValue.StringValue,
				"an unparsable edit does not invent a typed value")

			// A property the complete view no longer carries is removed
			// after the refresh observes its absence.
			seedNativeModel(t, backend, "typed", "edited again", map[string]string{"team": "sre"})
			require.NoError(t, backend.service.Refresh(t.Context()))
			view, err = backend.service.GetRegisteredModelById(strconv.Itoa(int(models[0].ID)))
			require.NoError(t, err)
			assert.NotContains(t, view.CustomProperties, "rank")
			assert.NotContains(t, view.CustomProperties, "score")
		})
	}
}
