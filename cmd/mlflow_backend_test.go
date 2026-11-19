package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/core"
	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/platform/db/mysql"
	"github.com/kubeflow/hub/internal/platform/db/postgres"
	"github.com/kubeflow/hub/internal/platform/tls"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	if err := testutils.SetupSharedMySQL(); err != nil {
		fmt.Printf("Failed to setup shared MySQL container: %v", err)
		os.Exit(1)
	}
	if err := testutils.SetupSharedPostgres(); err != nil {
		fmt.Printf("Failed to setup shared PostgreSQL container: %v", err)
		os.Exit(1)
	}
	code := m.Run()
	testutils.CleanupSharedMySQL()
	testutils.CleanupSharedPostgres()
	os.Exit(code)
}

// openCompatibilityDB opens one throwaway gorm connection to the
// compatibility database for test setup.
func openCompatibilityDB(t *testing.T, dbType, dsn string) *gorm.DB {
	t.Helper()
	var connector interface {
		Connect() (*gorm.DB, error)
	}
	if dbType == "mysql" {
		connector = mysql.NewMySQLDBConnector(dsn, &tls.TLSConfig{})
	} else {
		connector = postgres.NewPostgresDBConnector(dsn, &tls.TLSConfig{})
	}
	db, err := connector.Connect()
	require.NoError(t, err)
	return db
}

// nativeRegistryStub is a minimal MLflow registry answering the two
// paged search endpoints the startup refresh reads. An optional failure
// switch forces the initial refresh to fail.
type nativeRegistryStub struct {
	url        string
	failSearch atomic.Bool
}

func newNativeRegistryStub(t *testing.T) *nativeRegistryStub {
	t.Helper()
	stub := &nativeRegistryStub{}
	mux := http.NewServeMux()
	search := func(w http.ResponseWriter, r *http.Request, payload any) {
		w.Header().Set("Content-Type", "application/json")
		if stub.failSearch.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error_code": "INTERNAL_ERROR"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(payload)
	}
	mux.HandleFunc("/api/2.0/mlflow/registered-models/search", func(w http.ResponseWriter, r *http.Request) {
		search(w, r, map[string]any{"registered_models": []any{}})
	})
	mux.HandleFunc("/api/2.0/mlflow/model-versions/search", func(w http.ResponseWriter, r *http.Request) {
		search(w, r, map[string]any{"model_versions": []any{}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	stub.url = server.URL
	return stub
}

// resetMLflowCompatibilityStore removes the adapter-owned rows from the
// shared compatibility database so each startup test initializes a
// fresh identity.
func resetMLflowCompatibilityStore(t *testing.T, dbType, dsn string) {
	t.Helper()
	db := openCompatibilityDB(t, dbType, dsn)
	for _, model := range []any{
		&mlflow.IntentResource{}, &mlflow.Operation{}, &mlflow.VersionHistory{}, &mlflow.VersionBinding{},
		&mlflow.ArtifactLink{}, &mlflow.Object{}, &mlflow.Adapter{},
	} {
		// The first startup test migrates the schema, so the tables may
		// not exist yet.
		if db.Migrator().HasTable(model) {
			require.NoError(t, db.Where("1 = 1").Delete(model).Error, "failed to clean table for %T", model)
		}
	}
}

// startupConfig returns a valid backend configuration for one engine.
func startupConfig(t *testing.T, engine string, registryURI string) MLflowBackendConfig {
	t.Helper()
	var dsn string
	if engine == "mysql" {
		dsn = testutils.GetSharedMySQLDSN(t)
	} else {
		dsn = testutils.GetSharedPostgresDSN(t)
	}
	resetMLflowCompatibilityStore(t, engine, dsn)
	return MLflowBackendConfig{
		RegistryURI:     registryURI,
		DatabaseType:    engine,
		DatabaseDSN:     dsn,
		RequestTimeout:  5 * time.Second,
		RefreshInterval: 30 * time.Second,
	}
}

// TestMLflowBackendConfigValidation proves invalid or missing settings
// fail before any resource opens, and that database TLS verification is
// enabled by default with a one-sided client pair rejected.
func TestMLflowBackendConfigValidation(t *testing.T) {
	valid := MLflowBackendConfig{
		RegistryURI:  "https://registry.example.com",
		DatabaseType: "mysql",
		DatabaseDSN:  "user:pass@tcp(localhost:3306)/mlflow",
	}

	tlsConfig, err := valid.validate()
	require.NoError(t, err)
	assert.False(t, tlsConfig.VerifyServerCert, "a configuration with no TLS material is a plain connection")

	withCA := valid
	withCA.DatabaseTLS.CAPath = "/tmp/ca.crt"
	tlsConfig, err = withCA.validate()
	require.NoError(t, err)
	assert.True(t, tlsConfig.VerifyServerCert,
		"any configured TLS material must force server-certificate verification on")

	for name, mutate := range map[string]func(*MLflowBackendConfig){
		"missing registry uri":  func(c *MLflowBackendConfig) { c.RegistryURI = "" },
		"missing database type": func(c *MLflowBackendConfig) { c.DatabaseType = "" },
		"unknown database type": func(c *MLflowBackendConfig) { c.DatabaseType = "sqlite" },
		"missing dsn":           func(c *MLflowBackendConfig) { c.DatabaseDSN = "" },
		"token and basic auth": func(c *MLflowBackendConfig) {
			c.TokenFile, c.Username, c.PasswordFile = "/tmp/token", "user", "/tmp/pass"
		},
		"username without password file": func(c *MLflowBackendConfig) { c.Username = "user" },
		"password file without username": func(c *MLflowBackendConfig) { c.PasswordFile = "/tmp/pass" },
		"cert without key": func(c *MLflowBackendConfig) {
			c.DatabaseTLS.CertPath = "/tmp/client.crt"
		},
		"key without cert": func(c *MLflowBackendConfig) {
			c.DatabaseTLS.KeyPath = "/tmp/client.key"
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			_, err := config.validate()
			require.Error(t, err)
		})
	}
}

// TestOpenMLflowResourcesRejectsInvalidSettings proves invalid settings
// fail without opening resources.
func TestOpenMLflowResourcesRejectsInvalidSettings(t *testing.T) {
	_, _, cleanup, err := openMLflowResources(t.Context(), MLflowBackendConfig{
		RegistryURI: "not a url", DatabaseType: "mysql", DatabaseDSN: "x",
	})
	require.Error(t, err)
	require.Nil(t, cleanup)
}

// TestNewRegistryBackendStartupAndShutdown proves the full startup
// sequence — migration, identity initialization, initial refresh, and
// the run loop — publishes a healthy service that is not the EmbedMD
// core service, and that the cleanup joins workers, closes the SQL
// connection, and is idempotent.
func TestNewRegistryBackendStartupAndShutdown(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			stub := newNativeRegistryStub(t)
			config := startupConfig(t, engine, stub.url)

			service, cleanup, err := newRegistryBackend(t.Context(), config)
			require.NoError(t, err)
			require.NotNil(t, cleanup)

			// The MLflow backend bypasses the EmbedMD core repositories.
			_, isCoreService := service.(*core.ModelRegistryService)
			assert.False(t, isCoreService, "the mlflow backend must not construct the EmbedMD core service")

			// Startup completed: identity, refresh, and reconciliation.
			healthChecker, ok := service.(interface {
				CheckHealth(context.Context) error
			})
			require.True(t, ok, "the mlflow service must expose the health capability")
			require.NoError(t, healthChecker.CheckHealth(t.Context()))

			// Shutdown is idempotent and leaves the store closed.
			require.NoError(t, cleanup())
			require.NoError(t, cleanup())
			require.Error(t, healthChecker.CheckHealth(t.Context()),
				"the compatibility store must be closed after cleanup")
		})
	}
}

// TestNewRegistryBackendFailurePaths proves a canceled startup and a
// failed initial refresh fail loudly and release their resources.
func TestNewRegistryBackendFailurePaths(t *testing.T) {
	t.Run("canceled startup", func(t *testing.T) {
		stub := newNativeRegistryStub(t)
		config := startupConfig(t, "mysql", stub.url)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, cleanup, err := newRegistryBackend(ctx, config)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "startup canceled")
		require.Nil(t, cleanup)
	})

	t.Run("failed initial refresh", func(t *testing.T) {
		stub := newNativeRegistryStub(t)
		stub.failSearch.Store(true)
		config := startupConfig(t, "mysql", stub.url)

		_, cleanup, err := newRegistryBackend(t.Context(), config)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "initial mlflow refresh")
		require.Nil(t, cleanup)
	})

	t.Run("unreachable registry", func(t *testing.T) {
		config := startupConfig(t, "mysql", "http://127.0.0.1:1")

		_, cleanup, err := newRegistryBackend(t.Context(), config)
		require.Error(t, err)
		require.Nil(t, cleanup)
	})
}

// TestProxyBackendSelection proves the default backend is EmbedMD and
// that an unknown backend is rejected before the server starts.
func TestProxyBackendSelection(t *testing.T) {
	assert.Equal(t, "embedmd", proxyCfg.Backend, "the default backend must be embedmd")

	previous := proxyCfg.Backend
	defer func() { proxyCfg.Backend = previous }()

	proxyCfg.Backend = "bogus"
	err := runProxyServer(&cobra.Command{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --backend")
}

// TestProxyExplicitMLflowSelection proves an explicit mlflow selection
// routes startup through the MLflow backend: a registry failure
// surfaces the MLflow startup error, not an EmbedMD datastore error.
func TestProxyExplicitMLflowSelection(t *testing.T) {
	previousBackend, previousMLflow := proxyCfg.Backend, proxyCfg.MLflow
	previousServer := cfg
	defer func() {
		proxyCfg.Backend, proxyCfg.MLflow = previousBackend, previousMLflow
		cfg = previousServer
	}()
	// This test owns an ephemeral listener; a developer's server on the
	// default port must not race the backend failure being exercised.
	cfg.Hostname, cfg.Port = "127.0.0.1", 0

	stub := newNativeRegistryStub(t)
	stub.failSearch.Store(true)
	proxyCfg.Backend = "mlflow"
	proxyCfg.MLflow = startupConfig(t, "mysql", stub.url)

	err := runProxyServer(&cobra.Command{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mlflow backend")
}
