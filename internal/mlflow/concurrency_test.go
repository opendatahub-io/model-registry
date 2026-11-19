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

// launchTwo starts the given calls concurrently and returns a channel
// delivering both results. The caller can release a held native gate
// while the calls are in flight and then collect the results.
func launchTwo(first, second func() error) <-chan error {
	results := make(chan error, 2)
	go func() { results <- first() }()
	go func() { results <- second() }()
	return results
}

// TestConcurrencyRacingAttaches proves two replicas racing the same
// attachment converge: exactly one materializes the version through
// the journal, the loser conflicts on the lease or the active intent,
// and the final state holds one native version and one committed
// artifact.
func TestConcurrencyRacingAttaches(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			replica := backend.newBackendService(t)
			model := mustModel(t, backend, "racing")
			version := mustVersion(t, backend, model, "1.0.0")

			// A barrier at the native create holds the winner while the
			// loser reaches the journal.
			release := backend.stub.gateRequests("POST model-versions/create")
			results := launchTwo(
				func() error {
					_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/weights.tar.gz")
					return err
				},
				func() error {
					_, err := attachWeights(t, replica, version, "weights", "s3://bucket/weights.tar.gz")
					return err
				})
			require.Eventually(t, func() bool {
				return backend.stub.countRequests("POST model-versions/create") > 0
			}, 5*time.Second, 10*time.Millisecond)
			release()
			first, second := <-results, <-results

			errors := []error{first, second}
			succeeded := 0
			for _, err := range errors {
				if err == nil {
					succeeded++
				}
			}
			assert.Equal(t, 1, succeeded, "exactly one replica wins: %v", errors)

			assert.Equal(t, mlflow.PhaseReady, bindingOf(t, backend, *version.Id).Phase)
			assert.Equal(t, 1, len(backend.stub.versions["racing"]), "one native version")
			assert.Equal(t, 1, backend.stub.countRequests("POST model-versions/create"))
			var committed int64
			require.NoError(t, backend.db.Model(&mlflow.Object{}).
				Where("kind = ? AND visibility = ?", mlflow.KindModelArtifact, mlflow.VisibilityCommitted).
				Count(&committed).Error)
			assert.Equal(t, int64(1), committed)
		})
	}
}

// Materialization consumes version metadata, so its reservation must reject a
// concurrent metadata mutation until the complete generation publishes.
func TestConcurrencyMetadataVersusMaterialization(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			replica := backend.newBackendService(t)
			model := mustModel(t, backend, "mixed")
			version := mustVersion(t, backend, model, "1.0.0")
			entered, release := backend.stub.requestBarrier("POST model-versions/create")
			t.Cleanup(release)
			result := make(chan error, 1)
			go func() {
				_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/weights.tar.gz")
				result <- err
			}()
			select {
			case <-entered:
			case err := <-result:
				t.Fatalf("materialization ended before dispatch: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("no create reached barrier")
			}
			_, err := replica.UpsertModelVersion(&openapi.ModelVersion{Id: version.Id, Name: "1.0.0", Description: new("annotated")}, model.Id)
			assert.ErrorIs(t, err, api.ErrConflict)
			release()
			require.NoError(t, <-result)
			edited, err := replica.UpsertModelVersion(&openapi.ModelVersion{Id: version.Id, Name: "1.0.0", Description: new("annotated")}, model.Id)
			require.NoError(t, err)
			assert.Equal(t, "annotated", *edited.Description)
			assert.Equal(t, mlflow.PhaseReady, bindingOf(t, backend, *version.Id).Phase)
			assert.Len(t, backend.stub.versions["mixed"], 1)
		})
	}
}

// TestConcurrencySimultaneousReplacements proves two simultaneous URI
// edits produce exactly one replacement: the journal's resource
// revision lets one candidate win and rejects the other, and no second
// native generation appears.
func TestConcurrencySimultaneousReplacements(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			replica := backend.newBackendService(t)
			model := mustModel(t, backend, "editors")
			version := mustVersion(t, backend, model, "1.0.0")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/old.tar.gz")

			replace := func(service *mlflow.Service, uri string) error {
				_, err := service.UpsertModelVersionArtifact(&openapi.Artifact{
					ModelArtifact: &openapi.ModelArtifact{
						Id: primary.ModelArtifact.Id, Name: new("primary"), Uri: new(uri),
					},
				}, *version.Id)
				return err
			}
			results := launchTwo(
				func() error { return replace(backend.service, "s3://bucket/left.tar.gz") },
				func() error { return replace(replica, "s3://bucket/right.tar.gz") })
			first, second := <-results, <-results

			errors := []error{first, second}
			succeeded := 0
			for _, err := range errors {
				if err == nil {
					succeeded++
				}
			}
			assert.Equal(t, 1, succeeded, "exactly one replacement wins: %v", errors)

			// One new generation, one new native number, and the
			// committed URI is exactly one of the two candidates.
			binding := bindingOf(t, backend, *version.Id)
			assert.Equal(t, int64(2), binding.ActiveGeneration)
			assert.Equal(t, 2, len(backend.stub.versions["editors"]))
			visible, err := backend.service.GetModelArtifactById(*primary.ModelArtifact.Id)
			require.NoError(t, err)
			assert.Contains(t, []string{"s3://bucket/left.tar.gz", "s3://bucket/right.tar.gz"}, *visible.Uri)
		})
	}
}

// TestConcurrencyStaleCompletion proves a stale lease holder cannot
// complete after a faster replica re-claimed and published the
// operation: the stale fence rejects its checkpoint, and its delayed
// alias write cannot corrupt the committed state.
func TestConcurrencyStaleCompletion(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "stale")
			version := mustVersion(t, backend, model, "1.0.0")

			// A short-lease replica claims the work and stalls at the
			// alias move; its lease expires while it is blocked.
			stale, err := mlflow.NewService(mlflow.Config{
				RegistryURI: backend.stub.url, RequestTimeout: 200 * time.Millisecond,
			}, mlflow.NewStore(backend.db), backend.native)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, stale.Close()) })

			release := backend.stub.gateRequests("POST registered-models/alias")
			staleResult := make(chan error, 1)
			go func() {
				_, err := attachWeights(t, stale, version, "weights", "s3://bucket/weights.tar.gz")
				staleResult <- err
			}()
			require.Eventually(t, func() bool {
				return backend.stub.countRequests("POST registered-models/alias") > 0
			}, 5*time.Second, 10*time.Millisecond)

			// The lease ( 3 × request timeout ) expires; a fresh replica
			// re-claims and drives the same intent to publication.
			expireLeases(t, backend)
			fresh := backend.newBackendService(t)
			freshResult := make(chan error, 1)
			go func() {
				operation := materializeOperationOf(t, backend)
				freshResult <- fresh.ApplyOperation(t.Context(), operation.OperationUUID)
			}()
			require.Eventually(t, func() bool {
				return backend.stub.countRequests("POST registered-models/alias") > 1
			}, 5*time.Second, 10*time.Millisecond)
			release()

			require.NoError(t, <-freshResult)
			require.Error(t, <-staleResult, "the stale lease holder must not complete")

			// The committed state is the fresh replica's publication.
			assert.Equal(t, mlflow.PhaseReady, bindingOf(t, backend, *version.Id).Phase)
			assert.Equal(t, 1, len(backend.stub.versions["stale"]))
			alias := bindingOf(t, backend, *version.Id).Alias
			assert.Equal(t, "1", backend.stub.aliases("stale")[alias])
		})
	}
}
