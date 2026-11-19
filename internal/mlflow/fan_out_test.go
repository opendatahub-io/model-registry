package mlflow_test

import (
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sharedPrimary materializes a second version from an existing
// artifact, producing a primary shared by two versions of one model.
func sharedPrimary(t *testing.T, backend *backendFixture, model *openapi.RegisteredModel,
	first, second *openapi.ModelVersion, uri string) *openapi.Artifact {
	t.Helper()
	primary := mustPrimaryArtifact(t, backend, first, uri)
	shared, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
		ModelArtifact: &openapi.ModelArtifact{
			Id: primary.ModelArtifact.Id,
		},
	}, *second.Id)
	require.NoError(t, err)
	assert.Equal(t, *primary.ModelArtifact.Id, *shared.ModelArtifact.Id)
	return primary
}

// childOperations loads the fan-out child operations in creation order.
func childOperations(t *testing.T, backend *backendFixture) []mlflow.Operation {
	t.Helper()
	var operations []mlflow.Operation
	require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionReplaceChild).
		Order("create_time_since_epoch").Find(&operations).Error)
	return operations
}

// adapterUUIDOf reads the adapter's identity UUID.
func adapterUUIDOf(t *testing.T, backend *backendFixture) string {
	t.Helper()
	var adapter mlflow.Adapter
	require.NoError(t, backend.db.First(&adapter).Error)
	return adapter.AdapterUUID
}

// TestFanOutReplacesSharedPrimary proves a primary shared by two
// versions is replaced by a durable fan-out: one child operation per
// version creates exactly one new native generation each, every
// binding, snapshot, and history row switches atomically with the
// parent's publication, and each version's adapter alias moves to its
// own new native number. Native aliases cannot switch atomically across
// versions — each moves only after its own child is ready.
func TestFanOutReplacesSharedPrimary(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "fanout")
			first := mustVersion(t, backend, model, "1.0.0")
			second := mustVersion(t, backend, model, "2.0.0")
			primary := sharedPrimary(t, backend, model, first, second, "s3://bucket/old.tar.gz")
			assert.Equal(t, 2, len(backend.stub.versions["fanout"]))
			firstAlias := bindingOf(t, backend, *first.Id).Alias
			secondAlias := bindingOf(t, backend, *second.Id).Alias

			replaced, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Id: primary.ModelArtifact.Id, Name: new("primary"),
					Uri: new("s3://bucket/new.tar.gz"),
				},
			}, *first.Id)
			require.NoError(t, err)
			assert.Equal(t, "s3://bucket/new.tar.gz", *replaced.ModelArtifact.Uri)
			assert.Equal(t, *primary.ModelArtifact.Id, *replaced.ModelArtifact.Id)

			// Both children completed with exactly one native create each.
			children := childOperations(t, backend)
			require.Len(t, children, 2)
			numbers := map[string]bool{}
			for _, child := range children {
				assert.Equal(t, mlflow.OperationSucceeded, child.State)
				require.NotNil(t, child.NativeResult)
				numbers[*child.NativeResult] = true
			}
			assert.Equal(t, 2, len(numbers), "each child resolved its own native number")

			// Every binding switched to generation 2 with a distinct new
			// native number, and the history of both versions retains the
			// superseded first generation.
			bindings := []mlflow.VersionBinding{
				bindingOf(t, backend, *first.Id), bindingOf(t, backend, *second.Id),
			}
			seen := map[string]bool{}
			for _, binding := range bindings {
				assert.Equal(t, int64(2), binding.ActiveGeneration)
				require.NotNil(t, binding.NativeVersionNumber)
				assert.False(t, seen[*binding.NativeVersionNumber])
				seen[*binding.NativeVersionNumber] = true
			}
			for _, versionID := range []string{*first.Id, *second.Id} {
				histories := historiesOf(t, backend, versionID)
				require.Len(t, histories, 2)
				assert.True(t, histories[0].Superseded)
				assert.Equal(t, "s3://bucket/new.tar.gz", histories[1].Source)
			}

			// Each version's alias moved to its own new number.
			aliases := backend.stub.aliases("fanout")
			assert.Equal(t, *bindings[0].NativeVersionNumber, aliases[firstAlias])
			assert.Equal(t, *bindings[1].NativeVersionNumber, aliases[secondAlias])
		})
	}
}

// TestFanOutPartialFailureRecovers proves a partial remote fan-out is
// recoverable and invisible: while one child cannot complete, the
// public artifact URI stays unchanged and the write answers
// unavailable. A restarted service drives the durable parent to
// completion, and the recovered child created exactly one native
// version. The reconciliation searches across search pages.
func TestFanOutPartialFailureRecovers(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "partial")
			first := mustVersion(t, backend, model, "1.0.0")
			second := mustVersion(t, backend, model, "2.0.0")
			primary := sharedPrimary(t, backend, model, first, second, "s3://bucket/old.tar.gz")

			// The first child's native create commits server-side but
			// its response is dropped; the search that reconciles it
			// must cross pages.
			backend.stub.mu.Lock()
			backend.stub.commitThenFail = 1
			backend.stub.searchPageSize = 1
			backend.stub.mu.Unlock()

			_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Id: primary.ModelArtifact.Id, Name: new("primary"),
					Uri: new("s3://bucket/new.tar.gz"),
				},
			}, *first.Id)
			require.ErrorIs(t, err, api.ErrUnavailable)

			// The public artifact URI is unchanged and no binding moved.
			beforeRecovery, err := backend.service.GetModelArtifactById(*primary.ModelArtifact.Id)
			require.NoError(t, err)
			assert.Equal(t, "s3://bucket/old.tar.gz", *beforeRecovery.Uri)
			assert.Equal(t, int64(1), bindingOf(t, backend, *first.Id).ActiveGeneration)
			assert.Equal(t, int64(1), bindingOf(t, backend, *second.Id).ActiveGeneration)

			// A restarted service completes the durable fan-out.
			restarted := backend.newBackendService(t)
			expireLeases(t, backend)
			var parent mlflow.Operation
			require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionReplaceFanOut).
				First(&parent).Error)
			require.NoError(t, restarted.ApplyOperation(t.Context(), parent.OperationUUID))

			afterRecovery, err := restarted.GetModelArtifactById(*primary.ModelArtifact.Id)
			require.NoError(t, err)
			assert.Equal(t, "s3://bucket/new.tar.gz", *afterRecovery.Uri)
			assert.Equal(t, int64(2), bindingOf(t, backend, *first.Id).ActiveGeneration)
			assert.Equal(t, int64(2), bindingOf(t, backend, *second.Id).ActiveGeneration)

			// The failed child recovered with exactly one native version:
			// two initial materializations plus one per child, no
			// duplicates.
			assert.Equal(t, 4, len(backend.stub.versions["partial"]))
			for _, child := range childOperations(t, backend) {
				assert.Equal(t, mlflow.OperationSucceeded, child.State)
			}
		})
	}
}

// TestFanOutDuplicateTaggedResults proves a duplicate native version
// carrying a child's operation identity tag is a conflict, never an
// adoption: the fan-out stays unswitched rather than guessing.
func TestFanOutDuplicateTaggedResults(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "duplicates")
			first := mustVersion(t, backend, model, "1.0.0")
			second := mustVersion(t, backend, model, "2.0.0")
			primary := sharedPrimary(t, backend, model, first, second, "s3://bucket/old.tar.gz")

			backend.stub.mu.Lock()
			backend.stub.commitThenFail = 1
			backend.stub.mu.Unlock()
			_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Id: primary.ModelArtifact.Id, Name: new("primary"),
					Uri: new("s3://bucket/new.tar.gz"),
				},
			}, *first.Id)
			require.ErrorIs(t, err, api.ErrUnavailable)

			// Forge a second native version carrying the committed child's
			// identity tag: reconciliation must refuse to guess.
			children := childOperations(t, backend)
			require.Len(t, children, 2)
			var failed string
			for _, child := range children {
				// The committed-then-dropped child sits at the dispatch
				// checkpoint; the never-attempted sibling is still
				// prepared.
				if child.Step == mlflow.StepCreateDispatched {
					failed = child.OperationUUID
				}
			}
			require.NotEmpty(t, failed)
			backend.stub.forgeVersion("duplicates", "99", map[string]string{
				"rhoai.operation": failed,
				"rhoai.adapter":   adapterUUIDOf(t, backend),
			})

			restarted := backend.newBackendService(t)
			expireLeases(t, backend)
			var parent mlflow.Operation
			require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionReplaceFanOut).
				First(&parent).Error)
			err = restarted.ApplyOperation(t.Context(), parent.OperationUUID)
			require.ErrorIs(t, err, api.ErrConflict)
			assert.Contains(t, err.Error(), "identity tag",
				"the conflict is the duplicate identity, not a lease")

			// Nothing switched.
			visible, err := restarted.GetModelArtifactById(*primary.ModelArtifact.Id)
			require.NoError(t, err)
			assert.Equal(t, "s3://bucket/old.tar.gz", *visible.Uri)
			assert.Equal(t, int64(1), bindingOf(t, backend, *first.Id).ActiveGeneration)
		})
	}
}

// TestSharedArtifactMetadataFailureRecovers proves a native tag failure
// on the second binding leaves SQL untouched and the operation
// recoverable: the retry converges both bindings without creating any
// new native generation.
func TestSharedArtifactMetadataFailureRecovers(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "shared-failure")
			first := mustVersion(t, backend, model, "1.0.0")
			second := mustVersion(t, backend, model, "2.0.0")
			shared := sharedPrimary(t, backend, model, first, second, "s3://bucket/shared")
			artifactID := mustObjectID(t, shared.ModelArtifact.Id)
			var baseline mlflow.Object
			require.NoError(t, backend.db.Where("id = ?", artifactID).First(&baseline).Error)

			// Pre-apply the first binding's next description natively so the
			// fan-out's only tag write belongs to the second binding.
			firstNumber := *bindingOf(t, backend, *first.Id).NativeVersionNumber
			backend.stub.mu.Lock()
			backend.stub.versions[model.Name][firstNumber].(map[string]any)["tags"].(map[string]string)["rhoai.artifactRecord.description"] = "converged"
			backend.stub.mu.Unlock()
			backend.stub.failNextRequests("POST model-versions/set-tag", 1)
			_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Id: shared.ModelArtifact.Id, Description: new("converged"),
			}}, *first.Id)
			assert.ErrorIs(t, err, api.ErrUnavailable)

			// SQL never published: the snapshot and revision are unchanged.
			var after mlflow.Object
			require.NoError(t, backend.db.Where("id = ?", artifactID).First(&after).Error)
			assert.Equal(t, baseline.Revision, after.Revision)
			assert.Equal(t, baseline.CommittedJSON, after.CommittedJSON)

			// The interrupted operation recovers and converges both bindings.
			var operation mlflow.Operation
			require.NoError(t, backend.db.Where("kind = ? AND resource_id = ?", mlflow.OperationKindArtifactUpdate, artifactID).First(&operation).Error)
			creates := backend.stub.countRequests("POST model-versions/create")
			expireLeases(t, backend)
			require.NoError(t, backend.newBackendService(t).ApplyOperation(t.Context(), operation.OperationUUID))
			assert.Equal(t, "converged", backend.stub.versionTags(model.Name, firstNumber)["rhoai.artifactRecord.description"])
			secondNumber := *bindingOf(t, backend, *second.Id).NativeVersionNumber
			assert.Equal(t, "converged", backend.stub.versionTags(model.Name, secondNumber)["rhoai.artifactRecord.description"])
			assert.Equal(t, creates, backend.stub.countRequests("POST model-versions/create"), "no new native generation is created")
			assert.Equal(t, int64(1), bindingOf(t, backend, *first.Id).ActiveGeneration)
			assert.Equal(t, int64(1), bindingOf(t, backend, *second.Id).ActiveGeneration)
		})
	}
}

// TestSharedArtifactMetadataDefersToConcurrentAttachment proves a
// concurrent attachment of the shared artifact to a new version defers
// while the metadata fan-out holds its reservations: the fan-out covers
// exactly its captured targets, and the deferred attachment projects the
// new binding once it retries.
func TestSharedArtifactMetadataDefersToConcurrentAttachment(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			replica := backend.newBackendService(t)
			model := mustModel(t, backend, "shared-barrier")
			first := mustVersion(t, backend, model, "1.0.0")
			second := mustVersion(t, backend, model, "2.0.0")
			third := mustVersion(t, backend, model, "3.0.0")
			shared := sharedArtifactRoles(t, backend, model, first, second, "additional/additional")
			mustPrimaryArtifact(t, backend, third, "s3://bucket/third")

			entered, release := backend.stub.requestBarrier("POST model-versions/set-tag")
			t.Cleanup(release)
			result := make(chan error, 1)
			go func() {
				_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
					Id: shared.ModelArtifact.Id, Description: new("fan-out-update"),
				}}, *first.Id)
				result <- err
			}()
			select {
			case <-entered:
			case err := <-result:
				t.Fatalf("update ended before the tag barrier: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("tag barrier not reached")
			}

			// The concurrent attachment conflicts with the running fan-out.
			_, err := replica.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Id: shared.ModelArtifact.Id,
			}}, *third.Id)
			assert.ErrorIs(t, err, api.ErrConflict, "a concurrent attachment defers to the running fan-out")

			release()
			require.NoError(t, <-result)
			thirdNumber := *bindingOf(t, backend, *third.Id).NativeVersionNumber
			assert.NotContains(t, backend.stub.versionTags(model.Name, thirdNumber),
				"rhoai.artifacts."+*shared.ModelArtifact.Id+".artifactRecord.description",
				"the fan-out never projects a binding outside its captured targets")

			// The deferred attachment retries and projects the new binding.
			attached, err := replica.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Id: shared.ModelArtifact.Id,
			}}, *third.Id)
			require.NoError(t, err)
			assert.Equal(t, *shared.ModelArtifact.Id, *attached.ModelArtifact.Id)
			assert.Equal(t, "fan-out-update", backend.stub.versionTags(model.Name, thirdNumber)["rhoai.artifacts."+*shared.ModelArtifact.Id+".artifactRecord.description"])
		})
	}
}
