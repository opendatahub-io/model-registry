package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/mlflow/client"
	platformdb "github.com/kubeflow/hub/internal/platform/db"
	platformproxy "github.com/kubeflow/hub/internal/platform/proxy"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMLflowLifecycleCanceledInitialization(t *testing.T) {
	for _, scenario := range []struct{ engine, phase string }{{"mysql", "refresh"}, {"mysql", "graph-hydration"}, {"postgres", "refresh"}, {"postgres", "graph-hydration"}} {
		phase, engine := scenario.phase, scenario.engine
		t.Run(engine+"/"+phase, func(t *testing.T) {
			entered := make(chan struct{})
			var once sync.Once
			registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				once.Do(func() { close(entered) })
				<-r.Context().Done()
			}))
			defer registry.Close()
			config := startupConfig(t, engine, registry.URL)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var sqlStore *mlflow.Store
			var cleanupCount atomic.Int32
			resources := func(ctx context.Context, c MLflowBackendConfig) (*mlflow.Store, *client.Client, func() error, error) {
				store, native, cleanup, err := openMLflowResources(ctx, c)
				if err != nil {
					return store, native, cleanup, err
				}
				sqlStore = store
				if phase == "graph-hydration" {
					require.NoError(t, store.DB().Callback().Query().Before("gorm:query").Register("lifecycle-barrier", func(tx *gorm.DB) {
						if strings.Contains(tx.Statement.Table, "MLflowOperation") {
							once.Do(func() { close(entered) })
							<-ctx.Done()
						}
					}))
				}
				return store, native, func() error { cleanupCount.Add(1); return cleanup() }, nil
			}
			done := make(chan error, 1)
			go func() {
				_, _, err := newRegistryBackendWithFactories(ctx, config, resources, platformdb.NewDBMigrator)
				done <- err
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("startup returned before barrier: %v", err)
			case <-time.After(60 * time.Second):
				t.Fatal("startup did not enter barrier")
			}
			cancel()
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(60 * time.Second):
				t.Fatal("canceled startup did not join")
			}
			require.Equal(t, int32(1), cleanupCount.Load())
			pool, err := sqlStore.DB().DB()
			require.NoError(t, err)
			require.Error(t, pool.Ping())
		})
	}
}

type failingLifecycleMigrator struct {
	platformdb.DBMigrator
	closed atomic.Int32
}

func (m *failingLifecycleMigrator) Migrate() error { return errors.New("migration failed") }
func (m *failingLifecycleMigrator) Close() error {
	m.closed.Add(1)
	return m.DBMigrator.(interface{ Close() error }).Close()
}

func TestMigratorCleanupFailure(t *testing.T) {
	stub := newNativeRegistryStub(t)
	config := startupConfig(t, "mysql", stub.url)
	var migrator *failingLifecycleMigrator
	var appStore *mlflow.Store
	resources := func(ctx context.Context, c MLflowBackendConfig) (*mlflow.Store, *client.Client, func() error, error) {
		store, native, cleanup, err := openMLflowResources(ctx, c)
		appStore = store
		return store, native, cleanup, err
	}
	migration := func(db *gorm.DB) (platformdb.DBMigrator, error) {
		inner, err := platformdb.NewDBMigrator(db)
		if err != nil {
			return nil, err
		}
		migrator = &failingLifecycleMigrator{DBMigrator: inner}
		return migrator, nil
	}
	_, _, err := newRegistryBackendWithFactories(t.Context(), config, resources, migration)
	require.ErrorContains(t, err, "migration failed")
	require.Equal(t, int32(1), migrator.closed.Load())
	pool, err := appStore.DB().DB()
	require.NoError(t, err)
	require.Error(t, pool.Ping())
}

func TestCheckHealthRunningReadiness(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			stub := newNativeRegistryStub(t)
			config := startupConfig(t, engine, stub.url)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			service, cleanup, err := newRegistryBackend(ctx, config)
			require.NoError(t, err)
			defer func() { require.NoError(t, cleanup()) }()
			holder := &ModelRegistryServiceHolder{}
			holder.Set(service)
			handler := platformproxy.GeneralReadinessHandler(&ConditionalModelRegistryHealthChecker{holder: holder})
			check := func(path string) int {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest("GET", path, nil))
				return response.Code
			}
			for _, path := range []string{"/readyz/health", "/readyz/isDirty"} {
				require.Equal(t, http.StatusOK, check(path))
			}
			cancel()
			for _, path := range []string{"/readyz/health", "/readyz/isDirty"} {
				require.Eventually(t, func() bool { return check(path) == http.StatusServiceUnavailable }, 5*time.Second, time.Millisecond)
			}
			require.NoError(t, service.(*mlflow.Service).Close())
			require.NoError(t, service.(*mlflow.Service).Close())

		})
	}
}
