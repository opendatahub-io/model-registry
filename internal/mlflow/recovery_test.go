package mlflow_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// materializeOperationOf loads the version-materialize operation of a
// still-unmaterialized attach.
func materializeOperationOf(t *testing.T, backend *backendFixture) mlflow.Operation {
	t.Helper()
	var operation mlflow.Operation
	require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionMaterialize).
		First(&operation).Error)
	return operation
}

// expireLeases forces every running operation's lease into the past,
// simulating the passage of time after the holding replica died: a
// restarted worker may then reclaim the work.
func expireLeases(t *testing.T, backend *backendFixture) {
	t.Helper()
	require.NoError(t, backend.db.Model(&mlflow.Operation{}).
		Where("state = ?", mlflow.OperationRunning).
		Update("lease_expires_since_epoch", 0).Error)
}

// attachWeights attaches the given source-bearing artifact to a version.
func attachWeights(t *testing.T, service *mlflow.Service, version *openapi.ModelVersion,
	name, uri string) (*openapi.Artifact, error) {
	t.Helper()
	return service.UpsertModelVersionArtifact(&openapi.Artifact{
		ModelArtifact: &openapi.ModelArtifact{Name: new(name), Uri: new(uri)},
	}, *version.Id)
}

// requireInvisible asserts nothing committed and the binding is still
// pending.
func requireInvisible(t *testing.T, backend *backendFixture, version *openapi.ModelVersion) {
	t.Helper()
	var committed int64
	require.NoError(t, backend.db.Model(&mlflow.Object{}).
		Where("kind = ? AND visibility = ?", mlflow.KindModelArtifact, mlflow.VisibilityCommitted).
		Count(&committed).Error)
	assert.Zero(t, committed)
	assert.Equal(t, mlflow.PhasePendingSource, bindingOf(t, backend, *version.Id).Phase)
}

// TestRecoveryAtEveryBoundary proves deterministic failures at each
// journal boundary — the dispatched create whose outcome was dropped,
// the tag projection after the native-result checkpoint, and the alias
// move after tags — retain the intent invisibly, and a restarted
// service instance drives the same operation to completion with
// exactly one native version.
func TestRecoveryAtEveryBoundary(t *testing.T) {
	cases := []struct {
		name string
		fail func(backend *backendFixture)
	}{
		{
			name: "dispatch checkpoint with dropped create response",
			fail: func(backend *backendFixture) {
				backend.stub.mu.Lock()
				backend.stub.commitThenFail = 1
				backend.stub.mu.Unlock()
			},
		},
		{
			name: "native result checkpoint at tag projection",
			fail: func(backend *backendFixture) {
				backend.stub.failNextRequests("POST model-versions/set-tag", 1)
			},
		},
		{
			name: "tags applied at alias move",
			fail: func(backend *backendFixture) {
				backend.stub.failNextRequests("POST registered-models/alias", 1)
			},
		},
	}
	for engine, fixture := range newStoreFixtures(t) {
		for _, testCase := range cases {
			t.Run(engine+"/"+testCase.name, func(t *testing.T) {
				backend := newBackendFixture(t, fixture)
				model := mustModel(t, backend, "recover")
				version := mustVersion(t, backend, model, "1.0.0")
				testCase.fail(backend)

				_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/weights.tar.gz")
				require.ErrorIs(t, err, api.ErrUnavailable)
				requireInvisible(t, backend, version)

				// A restarted service instance recovers the recorded
				// intent and completes it.
				restarted := backend.newBackendService(t)
				expireLeases(t, backend)
				operation := materializeOperationOf(t, backend)
				require.NoError(t, restarted.ApplyOperation(t.Context(), operation.OperationUUID))

				var committed mlflow.Object
				require.NoError(t, backend.db.Where("kind = ? AND visibility = ?",
					mlflow.KindModelArtifact, mlflow.VisibilityCommitted).First(&committed).Error)
				attached, err := restarted.GetModelArtifactById(strconv.Itoa(int(committed.ID)))
				require.NoError(t, err)
				require.NotNil(t, attached.Id)
				assert.Equal(t, "s3://bucket/weights.tar.gz", *attached.Uri)
				assert.Equal(t, int64(1), bindingOf(t, backend, *version.Id).ActiveGeneration)
				assert.Equal(t, mlflow.PhaseReady, bindingOf(t, backend, *version.Id).Phase)
				assert.Equal(t, 1, len(backend.stub.versions["recover"]),
					"recovery creates no second native version")
			})
		}
	}
}

// TestRecoveryAfterPublicationIsNoop proves a succeeded operation is
// inert: a restarted service re-applies it without any native request
// or SQL change.
func TestRecoveryAfterPublicationIsNoop(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "published")
			version := mustVersion(t, backend, model, "1.0.0")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/weights.tar.gz")
			operation := materializeOperationOf(t, backend)
			require.Equal(t, mlflow.OperationSucceeded, operation.State)

			creates := backend.stub.countRequests("POST model-versions/create")
			tags := backend.stub.countRequests("POST model-versions/set-tag")
			aliases := backend.stub.countRequests("POST registered-models/alias")

			restarted := backend.newBackendService(t)
			require.NoError(t, restarted.ApplyOperation(t.Context(), operation.OperationUUID))
			assert.Equal(t, creates, backend.stub.countRequests("POST model-versions/create"))
			assert.Equal(t, tags, backend.stub.countRequests("POST model-versions/set-tag"))
			assert.Equal(t, aliases, backend.stub.countRequests("POST registered-models/alias"))
			assert.Equal(t, int64(1), bindingOf(t, backend, *version.Id).ActiveGeneration)
			_ = primary
		})
	}
}

// TestRecoveryAfterCallerDeath proves a create whose caller died
// mid-request still lands server-side and is adopted by the restarted
// service: the identity tag reconciles exactly one native version, and
// no blind re-create follows.
func TestRecoveryAfterCallerDeath(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "crashed")
			version := mustVersion(t, backend, model, "1.0.0")

			// Hold the native create; crash the caller while it is in
			// flight; then let the dropped request land.
			release := backend.stub.gateRequests("POST model-versions/create")
			result := make(chan error, 1)
			go func() {
				_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/weights.tar.gz")
				result <- err
			}()
			require.Eventually(t, func() bool {
				return backend.stub.countRequests("POST model-versions/create") > 0
			}, 5*time.Second, 10*time.Millisecond)
			require.NoError(t, backend.service.Close())
			require.Error(t, <-result)
			release()

			restarted := backend.newBackendService(t)
			expireLeases(t, backend)
			operation := materializeOperationOf(t, backend)
			require.NoError(t, restarted.ApplyOperation(t.Context(), operation.OperationUUID))
			assert.Equal(t, mlflow.PhaseReady, bindingOf(t, backend, *version.Id).Phase)
			assert.Equal(t, 1, len(backend.stub.versions["crashed"]),
				"the landed create is adopted, not duplicated")
			assert.Equal(t, 1, backend.stub.countRequests("POST model-versions/create"))
		})
	}
}

// TestRunResumesInterruptedWork proves the Run loop recovers retained
// intents automatically — with capped, jittered, persisted retry
// backoff — and returns cleanly when canceled.
func TestRunResumesInterruptedWork(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "resumed")
			version := mustVersion(t, backend, model, "1.0.0")
			backend.stub.failNextRequests("POST registered-models/alias", 2)

			_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/weights.tar.gz")
			require.ErrorIs(t, err, api.ErrUnavailable)

			restarted := backend.newBackendService(t)
			expireLeases(t, backend)
			ctx, cancel := context.WithCancel(t.Context())
			runResult := make(chan error, 1)
			go func() { runResult <- restarted.Run(ctx) }()

			// The run loop's own failed attempt persists the next retry
			// time before the retry succeeds.
			require.Eventually(t, func() bool {
				operation := materializeOperationOf(t, backend)
				return operation.NextRetrySinceEpoch != nil
			}, 15*time.Second, 50*time.Millisecond, "next retry is persisted")
			require.Eventually(t, func() bool {
				return bindingOf(t, backend, *version.Id).Phase == mlflow.PhaseReady
			}, 15*time.Second, 50*time.Millisecond, "Run recovers the retained intent")

			cancel()
			select {
			case err := <-runResult:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return after cancellation")
			}
			assert.Equal(t, 1, len(backend.stub.versions["resumed"]))
		})
	}
}

// TestRunStopsDefinitiveFailures proves a definitive input failure
// records a failure code and stops automatic retries until an operator
// re-drives the operation.
func TestRunStopsDefinitiveFailures(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "definitive")
			version := mustVersion(t, backend, model, "1.0.0")
			backend.stub.failNextBadRequests("POST model-versions/set-tag", 5)

			_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/weights.tar.gz")
			require.ErrorIs(t, err, api.ErrBadRequest)

			// The run loop records the definitive failure code and stops
			// retrying automatically.
			restarted := backend.newBackendService(t)
			expireLeases(t, backend)
			ctx, cancel := context.WithCancel(t.Context())
			go func() { _ = restarted.Run(ctx) }()
			require.Eventually(t, func() bool {
				operation := materializeOperationOf(t, backend)
				return operation.FailureCode != nil
			}, 15*time.Second, 50*time.Millisecond)
			assert.Equal(t, "DEFINITIVE_INPUT", *materializeOperationOf(t, backend).FailureCode)
			time.Sleep(time.Second)
			cancel()
			assert.Equal(t, 2, backend.stub.countRequests("POST model-versions/set-tag"),
				"automatic retries stopped")

			// The operator corrects the cause and re-drives the work.
			backend.stub.mu.Lock()
			backend.stub.badNext["POST model-versions/set-tag"] = 0
			backend.stub.mu.Unlock()
			operation := materializeOperationOf(t, backend)
			require.NoError(t, restarted.ApplyOperation(t.Context(), operation.OperationUUID))
			assert.Equal(t, mlflow.PhaseReady, bindingOf(t, backend, *version.Id).Phase)
		})
	}
}

// TestRecoveryRepairsAliasDrift proves a stale remote alias write —
// for example one that landed after lease loss — is repaired by the
// next alias move, which always ends pointing at the current
// generation.
func TestRecoveryRepairsAliasDrift(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "drift")
			version := mustVersion(t, backend, model, "1.0.0")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/old.tar.gz")
			alias := bindingOf(t, backend, *version.Id).Alias
			assert.Equal(t, "1", backend.stub.aliases("drift")[alias])

			// A stale write points the alias at a number that was never
			// this version's generation.
			backend.stub.mu.Lock()
			backend.stub.models["drift"]["aliases"].(map[string]string)[alias] = "99"
			backend.stub.mu.Unlock()

			_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Id: primary.ModelArtifact.Id, Name: new("primary"),
					Uri: new("s3://bucket/new.tar.gz"),
				},
			}, *version.Id)
			require.NoError(t, err)
			assert.Equal(t, "2", backend.stub.aliases("drift")[alias],
				"the replacement repairs the drifted alias")
		})
	}
}

// TestRecoveryQuotedModelName proves recovery and operator resolution
// locate a model's owned versions for every accepted model name —
// including apostrophes, double quotes, and filter-like text — without
// interpolating the name into a native search filter, and that exact
// model-name filtering happens before match counting so a foreign
// version carrying the same operation tag is never adopted.
func TestRecoveryQuotedModelName(t *testing.T) {
	quotedNames := []string{
		"apo'strophe",
		`quo"ted`,
		`bo'th"quotes`,
		`filter'like = "text"`,
	}
	for engine, fixture := range newStoreFixtures(t) {
		for _, modelName := range quotedNames {
			t.Run(engine+"/"+modelName, func(t *testing.T) {
				backend := newBackendFixture(t, fixture)

				// An uncertain create under a quoted model name.
				model := mustModel(t, backend, modelName)
				version := mustVersion(t, backend, model, "1.0.0")
				backend.stub.mu.Lock()
				backend.stub.commitThenFail = 1
				backend.stub.searchPageSize = 1
				backend.stub.mu.Unlock()
				_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/weights.tar.gz")
				require.ErrorIs(t, err, api.ErrUnavailable)
				operation := materializeOperationOf(t, backend)

				// Unrelated models fill other search pages, one carrying the
				// same operation and adapter identity tags.
				unrelated := mustModel(t, backend, "unrelated-"+strconv.Itoa(len(modelName)))
				unrelatedVersion := mustVersion(t, backend, unrelated, "1.0.0")
				mustPrimaryArtifact(t, backend, unrelatedVersion, "s3://bucket/unrelated")
				backend.stub.forgeVersion(unrelated.Name, "9", map[string]string{
					"rhoai.operation": operation.OperationUUID,
					"rhoai.adapter":   adapterUUIDOf(t, backend),
				})

				expireLeases(t, backend)
				require.NoError(t, backend.newBackendService(t).ApplyOperation(t.Context(), operation.OperationUUID))
				creates := backend.stub.countRequests("POST model-versions/create")
				assert.Equal(t, 1, len(backend.stub.versions[modelName]),
					"recovery adopts the committed native version and never issues another create")
				assert.Equal(t, 2, creates, "only the unrelated model's materialization and the adopted create occurred")
				assert.Equal(t, mlflow.PhaseReady, bindingOf(t, backend, *version.Id).Phase)

				// Operator resolution resolves the same lookup helper under a
				// quoted model name: an uncertain replacement child adopts its
				// committed native version.
				fanoutModel := mustModel(t, backend, modelName+"-fanout")
				first := mustVersion(t, backend, fanoutModel, "one")
				second := mustVersion(t, backend, fanoutModel, "two")
				primary := sharedPrimary(t, backend, fanoutModel, first, second, "s3://old")
				backend.stub.mu.Lock()
				backend.stub.commitThenFail = 1
				backend.stub.mu.Unlock()
				_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
					Id: primary.ModelArtifact.Id, Name: new("primary"), Uri: new("s3://new"),
				}}, *first.Id)
				require.ErrorIs(t, err, api.ErrUnavailable)
				var root mlflow.Operation
				require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionReplaceFanOut).First(&root).Error)
				expireLeases(t, backend)
				creates = backend.stub.countRequests("POST model-versions/create")
				require.NoError(t, backend.newBackendService(t).ApplyOperation(t.Context(), root.OperationUUID))
				assert.Equal(t, creates+1, backend.stub.countRequests("POST model-versions/create"),
					"recovery adopts the uncertain child through the shared lookup and creates only the remaining generation")
				assert.Equal(t, int64(2), bindingOf(t, backend, *first.Id).ActiveGeneration)
				assert.Equal(t, int64(2), bindingOf(t, backend, *second.Id).ActiveGeneration)
				assert.Equal(t, 4, len(backend.stub.versions[fanoutModel.Name]),
					"two materialized generations plus one new native generation per version")
			})
		}
	}
}
