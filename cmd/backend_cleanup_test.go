package cmd

import (
	"context"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackendCleanupLateInstall(t *testing.T) {
	var owner backendCleanupOwner
	var calls atomic.Int32
	released := make(chan struct{})
	done := make(chan struct{})
	go func() { <-released; owner.Install(func() error { calls.Add(1); return nil }); close(done) }()
	require.NoError(t, owner.Close())
	close(released)
	<-done
	require.NoError(t, owner.Close())
	require.Equal(t, int32(1), calls.Load())
}

func TestBackendCleanupInstalled(t *testing.T) {
	var owner backendCleanupOwner
	var calls atomic.Int32
	owner.Install(func() error { calls.Add(1); return nil })
	require.NoError(t, owner.Close())
	require.NoError(t, owner.Close())
	require.Equal(t, int32(1), calls.Load())
}

// The factory deliberately ignores cancellation until released: cleanup must
// still transfer to shutdown after the listener function has returned.
func TestBackendCleanupProxyLateFactory(t *testing.T) {
	for _, listenerFails := range []bool{true, false} {
		t.Run(map[bool]string{true: "listener-error", false: "shutdown"}[listenerFails], func(t *testing.T) {
			previousProxy, previousCfg := proxyCfg, cfg
			defer func() { proxyCfg, cfg = previousProxy, previousCfg }()
			proxyCfg.Backend = "mlflow"
			cfg.Hostname = "127.0.0.1"
			cfg.Port = 0
			var listener net.Listener
			if listenerFails {
				var err error
				listener, err = net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				defer func() { require.NoError(t, listener.Close()) }()
				cfg.Port = listener.Addr().(*net.TCPAddr).Port
			}
			entered, release, cleaned := make(chan context.Context, 1), make(chan struct{}), make(chan struct{})
			factoryDone := make(chan struct{})
			var calls atomic.Int32
			factory := func(ctx context.Context, _ MLflowBackendConfig) (api.ModelRegistryApi, func() error, error) {
				entered <- ctx
				<-release
				return nil, func() error { calls.Add(1); close(cleaned); return nil }, nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			command := &cobra.Command{}
			command.SetContext(ctx)
			result := make(chan error, 1)
			go func() {
				result <- runProxyServerWithFactory(command, nil, func(ctx context.Context, c MLflowBackendConfig) (api.ModelRegistryApi, func() error, error) {
					defer close(factoryDone)
					return factory(ctx, c)
				})
			}()
			startupCtx := <-entered
			if !listenerFails {
				cancel()
			}
			select {
			case err := <-result:
				if listenerFails {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("proxy shutdown blocked on factory")
			}
			require.Error(t, startupCtx.Err())
			close(release)
			<-factoryDone
			select {
			case <-cleaned:
			case <-time.After(5 * time.Second):
				t.Fatal("late factory leaked resources")
			}
			require.Equal(t, int32(1), calls.Load())
		})
	}
}
