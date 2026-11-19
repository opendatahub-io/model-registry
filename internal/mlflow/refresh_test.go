package mlflow_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lastRefreshTime reads the adapter's persisted refresh timestamp.
func lastRefreshTime(t *testing.T, backend *backendFixture) *int64 {
	t.Helper()
	stamp := adapterOf(t, backend).LastRefreshTime
	return stamp
}

// TestRefreshLaterPageFailurePreservesObservation proves a refresh
// that cannot read every page publishes nothing: the earlier
// observation, its timestamp, and the failed page's would-be imports
// stay untouched, and a later complete refresh imports them.
func TestRefreshLaterPageFailurePreservesObservation(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "paged", "", nil)
			seedNativeVersion(t, backend, "paged", "1", "s3://bucket/one.tar.gz", "", nil)
			seedNativeVersion(t, backend, "paged", "2", "s3://bucket/two.tar.gz", "", nil)
			require.NoError(t, backend.service.Refresh(t.Context()))
			firstStamp := lastRefreshTime(t, backend)
			require.NotNil(t, firstStamp)

			// A new native version and a page size that splits the
			// search: the second page fails.
			seedNativeVersion(t, backend, "paged", "3", "s3://bucket/three.tar.gz", "", nil)
			backend.stub.mu.Lock()
			backend.stub.searchPageSize = 1
			backend.stub.mu.Unlock()
			backend.stub.failNextRequests("GET model-versions/search", 2)

			err := backend.service.Refresh(t.Context())
			require.Error(t, err)
			assert.Len(t, committedObjects(t, backend, mlflow.KindModelVersion), 2,
				"no partial observation is published")
			assert.Equal(t, firstStamp, lastRefreshTime(t, backend),
				"the successful-refresh timestamp is intact")

			// A complete refresh imports the third version.
			backend.stub.mu.Lock()
			backend.stub.failNext["GET model-versions/search"] = 0
			backend.stub.mu.Unlock()
			require.NoError(t, backend.service.Refresh(t.Context()))
			assert.Len(t, committedObjects(t, backend, mlflow.KindModelVersion), 3)
			assert.Greater(t, *lastRefreshTime(t, backend), *firstStamp,
				"the timestamp advances in milliseconds")
		})
	}
}

// TestRefreshCanceledAbortsCleanly proves a canceled refresh context
// aborts before publishing and leaves the prior observation committed.
func TestRefreshCanceledAbortsCleanly(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "canceled", "", nil)
			require.NoError(t, backend.service.Refresh(t.Context()))

			ctx, cancel := context.WithCancel(t.Context())
			backend.stub.mu.Lock()
			backend.stub.searchPageSize = 1
			backend.stub.mu.Unlock()
			seedNativeVersion(t, backend, "canceled", "1", "s3://bucket/one.tar.gz", "", nil)
			release := backend.stub.gateRequests("GET registered-models/search")
			result := make(chan error, 1)
			go func() { result <- backend.service.Refresh(ctx) }()
			require.Eventually(t, func() bool {
				return backend.stub.countRequests("GET registered-models/search") > 0
			}, 5*time.Second, 10*time.Millisecond)
			cancel()
			release()
			require.Error(t, <-result)
			assert.Empty(t, committedObjects(t, backend, mlflow.KindModelVersion))
		})
	}
}

// TestRefreshCoalescesConcurrentCalls proves concurrent refreshes share
// one observation: exactly one registered-models search sweep runs and
// every caller receives the same result.
func TestRefreshCoalescesConcurrentCalls(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "coalesced", "", nil)

			release := backend.stub.gateRequests("GET registered-models/search")
			results := make(chan error, 5)
			var launch sync.WaitGroup
			launch.Add(5)
			for range 5 {
				go func() {
					launch.Done()
					results <- backend.service.Refresh(t.Context())
				}()
			}
			launch.Wait()
			require.Eventually(t, func() bool {
				return backend.stub.countRequests("GET registered-models/search") > 0
			}, 5*time.Second, 10*time.Millisecond)
			release()
			for range 5 {
				require.NoError(t, <-results)
			}
			assert.Equal(t, 1, backend.stub.countRequests("GET registered-models/search"),
				"one fetch serves every concurrent caller")
		})
	}
}

// TestRefreshRejectsWrongTargetFingerprint proves a service pointed at
// a registry other than the one its store is bound to refuses to
// import from it.
func TestRefreshRejectsWrongTargetFingerprint(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "wrong-target", "", nil)

			other, err := mlflow.NewService(mlflow.Config{
				RegistryURI: backend.stub.url + "/other", RequestTimeout: 5 * time.Second,
			}, mlflow.NewStore(backend.db), backend.native)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, other.Close()) })

			require.ErrorIs(t, other.Refresh(t.Context()), api.ErrConflict)
			assert.Empty(t, committedObjects(t, backend, mlflow.KindRegisteredModel))
		})
	}
}

// TestRefreshConcurrentLocalWriteWins proves a local write committed
// while a refresh is in flight is not clobbered by the refresh's
// snapshot update.
func TestRefreshConcurrentLocalWriteWins(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "racing")

			release := backend.stub.gateRequests("GET registered-models/search")
			refreshed := make(chan error, 1)
			go func() { refreshed <- backend.service.Refresh(t.Context()) }()
			require.Eventually(t, func() bool {
				return backend.stub.countRequests("GET registered-models/search") > 0
			}, 5*time.Second, 10*time.Millisecond)

			// The local description edit commits while the refresh is
			// gathering its observation.
			_, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Id: model.Id, Name: "racing", Description: new("local edit"),
			})
			require.NoError(t, err)
			release()
			require.NoError(t, <-refreshed)

			view, err := backend.service.GetRegisteredModelById(*model.Id)
			require.NoError(t, err)
			assert.Equal(t, "local edit", *view.Description,
				"the concurrent local write survives the refresh")
		})
	}
}

// TestRefreshMarksNativeDeletions proves a bound local version whose
// native counterpart vanished from a complete observation reads as
// deleted: the direct-ID read checks native existence, marks the
// deletion, and never recreates the version from an older refresh.
func TestRefreshMarksNativeDeletions(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "vanishing")
			version := mustVersion(t, backend, model, "1.0.0")
			mustPrimaryArtifact(t, backend, version, "s3://bucket/gone.tar.gz")
			versionID := *version.Id

			// The native version and its model are deleted outside the
			// adapter.
			backend.stub.mu.Lock()
			delete(backend.stub.versions["vanishing"], "1")
			delete(backend.stub.models, "vanishing")
			backend.stub.mu.Unlock()

			require.NoError(t, backend.service.Refresh(t.Context()))

			// The bound direct-ID read confirms the deletion natively.
			_, err := backend.service.GetModelVersionById(versionID)
			require.ErrorIs(t, err, api.ErrNotFound)
			var row mlflow.Object
			require.NoError(t, backend.db.Where("id = ?", mustObjectID(t, version.Id)).First(&row).Error)
			assert.True(t, row.Deleted, "the deletion is marked locally")

			// A later refresh does not resurrect the version from the
			// older observation.
			require.NoError(t, backend.service.Refresh(t.Context()))
			_, err = backend.service.GetModelVersionById(versionID)
			require.ErrorIs(t, err, api.ErrNotFound)
		})
	}
}

// TestRunRefreshesPeriodically proves the Run loop refreshes on its
// interval from the service's own lifetime: a native object created
// after startup appears without any manual Refresh call.
func TestRunRefreshesPeriodically(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			service, err := mlflow.NewService(mlflow.Config{
				RegistryURI: backend.stub.url, RequestTimeout: 5 * time.Second,
				RefreshInterval: 200 * time.Millisecond,
			}, mlflow.NewStore(backend.db), backend.native)
			require.NoError(t, err)

			ctx, cancel := context.WithCancel(t.Context())
			runResult := make(chan error, 1)
			go func() { runResult <- service.Run(ctx) }()

			seedNativeModel(t, backend, "periodic", "", nil)
			seedNativeVersion(t, backend, "periodic", "1", "s3://bucket/periodic.tar.gz", "", nil)
			require.Eventually(t, func() bool {
				return len(committedObjects(t, backend, mlflow.KindModelVersion)) == 1
			}, 15*time.Second, 100*time.Millisecond, "Run imports the native object")

			cancel()
			select {
			case err := <-runResult:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return after cancellation")
			}
			require.NoError(t, service.Close())
		})
	}
}

// TestRefreshRestartKeepsIdentities proves a refresh after a service
// restart resolves the same stable identities: no duplicate rows, and
// the imported version keeps its logical id.
func TestRefreshRestartKeepsIdentities(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "restart", "", nil)
			seedNativeVersion(t, backend, "restart", "5", "s3://bucket/restart.tar.gz", "", nil)

			require.NoError(t, backend.service.Refresh(t.Context()))
			versions := committedObjects(t, backend, mlflow.KindModelVersion)
			require.Len(t, versions, 1)
			originalID := versions[0].ID

			restarted := backend.newBackendService(t)
			require.NoError(t, restarted.Refresh(t.Context()))
			versions = committedObjects(t, backend, mlflow.KindModelVersion)
			require.Len(t, versions, 1)
			assert.Equal(t, originalID, versions[0].ID)
			binding := bindingOf(t, backend, strconv.Itoa(int(originalID)))
			assert.Equal(t, "5", *binding.NativeVersionNumber)
		})
	}
}

// TestRefreshDoesNotDeleteConcurrentlyCreatedModel proves a refresh
// whose native observation was fetched before an inline create
// committed does not read that create's absence as a native deletion.
// The refresh fetch is gated so the observation provably predates the
// model, reproducing the race between Run's periodic refresh and
// inline writes.
func TestRefreshDoesNotDeleteConcurrentlyCreatedModel(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)

			release := backend.stub.gateRequests("GET registered-models/search")
			refreshErr := make(chan error, 1)
			go func() { refreshErr <- backend.service.Refresh(t.Context()) }()
			require.Eventually(t, func() bool {
				return backend.stub.countRequests("GET registered-models/search") > 0
			}, 5*time.Second, 10*time.Millisecond)

			// The inline create commits while the refresh's fetch is
			// still held at the gate. Removing the model from the stub
			// before the release makes the eventual observation provably
			// predate the create, exactly as a fetch that completed
			// before the native create would have.
			created, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name: "concurrent", Owner: new("team"),
			})
			require.NoError(t, err)
			require.NotNil(t, created.Id)
			backend.stub.mu.Lock()
			delete(backend.stub.models, "concurrent")
			backend.stub.mu.Unlock()

			release()
			require.NoError(t, <-refreshErr)

			// The stale observation must not mark the new model deleted.
			readBack, err := backend.service.GetRegisteredModelById(*created.Id)
			require.NoError(t, err, "a model created after the observation began must survive the refresh")
			require.Equal(t, "concurrent", readBack.Name)

			// The next refresh — now observing the native model —
			// still sees exactly one committed row.
			seedNativeModel(t, backend, "concurrent", "", nil)
			require.NoError(t, backend.service.Refresh(t.Context()))
			assert.Len(t, committedObjects(t, backend, mlflow.KindRegisteredModel), 1)
		})
	}
}

// TestRefreshDoesNotDeleteReservedRowOfFailedCreate proves a reserved
// row left by a failed native create is never marked deleted by a
// refresh that cannot yet observe its native counterpart: the retry
// must still resume, publish, and read back.
func TestRefreshDoesNotDeleteReservedRowOfFailedCreate(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			backend.stub.mu.Lock()
			backend.stub.failCreates = 1
			backend.stub.mu.Unlock()

			_, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name: "reserved", Owner: new("team"),
			})
			require.ErrorIs(t, err, api.ErrUnavailable)

			// The reserved row has no native counterpart yet; the
			// refresh must leave it untouched.
			require.NoError(t, backend.service.Refresh(t.Context()))

			retried, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name: "reserved", Owner: new("team"),
			})
			require.NoError(t, err, "the retry must resume the recorded intent after the refresh")
			require.NotNil(t, retried.Id)

			readBack, err := backend.service.GetRegisteredModelById(*retried.Id)
			require.NoError(t, err)
			require.Equal(t, "reserved", readBack.Name)
			assert.Len(t, committedObjects(t, backend, mlflow.KindRegisteredModel), 1)
		})
	}
}
