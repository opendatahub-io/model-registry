package mlflow_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startHealthyBackend completes the runtime startup sequence the health
// probe observes: one successful refresh ( recording the adapter's
// refresh timestamp ) and the reconciliation loop. The returned stop
// function halts the loop so later fault injection is deterministic.
func startHealthyBackend(t *testing.T, backend *backendFixture) (stop func()) {
	t.Helper()
	require.NoError(t, backend.service.Refresh(t.Context()))
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = backend.service.Run(runCtx)
	}()
	require.Eventually(t, func() bool {
		return backend.service.CheckHealth(t.Context()) == nil
	}, 10*time.Second, 50*time.Millisecond, "the run loop must start")
	return func() {
		cancel()
		<-done
	}
}

// TestCheckHealthProbesDependencies proves the health capability fails
// for each broken dependency in order — an uninitialized adapter, a
// missing initial refresh, a stopped reconciliation loop, an
// unreachable SQL store, an unreachable native registry — and passes
// only when all are healthy, without calling any unsupported API
// family.
func TestCheckHealthProbesDependencies(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			// Before startup: no adapter identity, no refresh, no run.
			fixture.cleanup(t)
			bareStub, bareNative := newNativeStub(t)
			bare, err := mlflow.NewService(mlflow.Config{RegistryURI: bareStub.url},
				mlflow.NewStore(fixture.db), bareNative)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, bare.Close()) })
			err = bare.CheckHealth(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "adapter identity is not initialized")

			backend := newBackendFixture(t, fixture)

			// After adapter initialization but before the first refresh.
			err = backend.service.CheckHealth(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "initial mlflow refresh has not completed")

			// After the initial refresh but before the run loop starts.
			require.NoError(t, backend.service.Refresh(t.Context()))
			err = backend.service.CheckHealth(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "reconciliation loop is not running")

			// An unreachable SQL store fails the probe.
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			err = backend.service.CheckHealth(canceled)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "compatibility store is unreachable")

			// Fully started: healthy. Keep the loop alive during native fault injection.
			stopLoop := startHealthyBackend(t, backend)
			assert.NoError(t, backend.service.CheckHealth(t.Context()))
			defer stopLoop()

			// A failed native read fails the probe without side effects.
			// The failure stays queued for several requests: a refresh
			// already in flight when the loop stopped may consume one,
			// and the probe must still observe a failure.
			backend.stub.failNextRequests("GET registered-models/search", 5)
			err = backend.service.CheckHealth(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "registry is unreachable")

			// The probe recovers once the registry answers again.
			backend.stub.mu.Lock()
			backend.stub.failNext["GET registered-models/search"] = 0
			backend.stub.mu.Unlock()
			assert.NoError(t, backend.service.CheckHealth(t.Context()))
			stopLoop()
			require.Eventually(t, func() bool {
				err := backend.service.CheckHealth(t.Context())
				return err != nil && strings.Contains(err.Error(), "reconciliation loop is not running")
			}, 5*time.Second, time.Millisecond)
		})
	}
}
