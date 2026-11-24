package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/golang/glog"
	"github.com/kubeflow/hub/internal/core"
	"github.com/kubeflow/hub/internal/datastore/embedmd"
	"github.com/kubeflow/hub/internal/db/models"
	"github.com/kubeflow/hub/internal/db/service"
	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/platform/datastore"
	platformproxy "github.com/kubeflow/hub/internal/platform/proxy"
	platformmw "github.com/kubeflow/hub/internal/platform/server/middleware"
	"github.com/kubeflow/hub/internal/platform/tls"
	"github.com/kubeflow/hub/internal/proxy"
	"github.com/kubeflow/hub/internal/server/middleware"
	v1 "github.com/kubeflow/hub/internal/server/openapi/v1"
	"github.com/kubeflow/hub/internal/server/openapi/v1alpha3"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/spf13/cobra"
)

type ProxyConfig struct {
	EmbedMD       embedmd.EmbedMDConfig
	DatastoreType string
	Backend       string
	MLflow        MLflowBackendConfig
}

const (
	// datastoreUnavailableMessage is the message returned when the datastore service is down or unavailable.
	datastoreUnavailableMessage = "Datastore service is down or unavailable. Please check that the database is reachable and try again later."
)

var (
	proxyCfg = ProxyConfig{
		DatastoreType: "embedmd",
		Backend:       "embedmd",
		EmbedMD: embedmd.EmbedMDConfig{
			TLSConfig: &tls.TLSConfig{},
		},
		MLflow: MLflowBackendConfig{
			RequestTimeout:  mlflow.DefaultRequestTimeout,
			RefreshInterval: mlflow.DefaultRefreshInterval,
		},
	}

	// proxyCmd represents the proxy command
	proxyCmd = &cobra.Command{
		Use:   "proxy",
		Short: "Starts the go OpenAPI proxy server to connect to a metadata store",
		Long: `This command launches the go OpenAPI proxy server.

The server connects to a metadata store, currently only the internal store is supported. It supports options to customize the
hostname and port where it listens.`,
		RunE: runProxyServer,
	}
)

// ModelRegistryServiceHolder safely holds the model registry service
type ModelRegistryServiceHolder struct {
	mu      sync.RWMutex
	service api.ModelRegistryApi
}

func (h *ModelRegistryServiceHolder) Set(service api.ModelRegistryApi) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.service = service
}

func (h *ModelRegistryServiceHolder) Get() api.ModelRegistryApi {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.service
}

// ConditionalModelRegistryHealthChecker checks model registry health only if service is available
type ConditionalModelRegistryHealthChecker struct {
	holder *ModelRegistryServiceHolder
}

func (c *ConditionalModelRegistryHealthChecker) Check() platformproxy.HealthCheck {
	service := c.holder.Get()
	if service == nil {
		return platformproxy.HealthCheck{
			Name:    proxy.HealthCheckModelRegistry,
			Status:  platformproxy.StatusFail,
			Message: "model registry service not yet initialized",
			Details: map[string]any{
				"service_ready": false,
			},
		}
	}

	checker := proxy.NewModelRegistryHealthChecker(service)
	return checker.Check()
}

func runProxyServer(cmd *cobra.Command, args []string) error {
	return runProxyServerWithFactory(cmd, args, newRegistryBackend)
}

func runProxyServerWithFactory(cmd *cobra.Command, args []string, factory func(context.Context, MLflowBackendConfig) (api.ModelRegistryApi, func() error, error)) error {
	if proxyCfg.Backend != "embedmd" && proxyCfg.Backend != "mlflow" {
		return fmt.Errorf("invalid --backend %q: supported backends: embedmd, mlflow", proxyCfg.Backend)
	}

	var (
		ds           datastore.Connector
		wg           sync.WaitGroup
		cleanupOwner backendCleanupOwner
	)

	// shutdownBackend joins the selected backend's workers and closes its
	// owned resources. It is idempotent and safe to call on every exit
	// path; the EmbedMD backend owns no per-command resources.
	shutdownBackend := func() { _ = cleanupOwner.Close() }

	serviceHolder := &ModelRegistryServiceHolder{}

	router := platformproxy.NewDynamicRouter()

	router.SetRouter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, datastoreUnavailableMessage, http.StatusServiceUnavailable)
	}))

	readyChecks := []platformproxy.HealthChecker{}
	generalChecks := []platformproxy.HealthChecker{
		&ConditionalModelRegistryHealthChecker{holder: serviceHolder},
	}

	switch proxyCfg.Backend {
	case "embedmd":
		dbHealthChecker := platformproxy.NewDatabaseHealthChecker()
		readyChecks = append(readyChecks, dbHealthChecker)
		generalChecks = append(generalChecks, dbHealthChecker)
	case "mlflow":
		// The MLflow backend's optional health capability drives both
		// readiness endpoints: SQL, registry, and initialization state
		// are checked through the service, not the global database.
		readyChecks = append(readyChecks, &ConditionalModelRegistryHealthChecker{holder: serviceHolder})
	}

	generalReadinessHandler := platformproxy.GeneralReadinessHandler(generalChecks...)
	readinessHandler := platformproxy.GeneralReadinessHandler(readyChecks...)

	// route health endpoints appropriately
	mainHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if strings.HasSuffix(r.URL.Path, "/readyz/isDirty") {
			readinessHandler.ServeHTTP(w, r)
			return
		}

		if strings.HasSuffix(r.URL.Path, "/readyz/health") {
			generalReadinessHandler.ServeHTTP(w, r)
			return
		}

		router.ServeHTTP(w, r)
	})

	// The command's lifetime is the process's: cancellation on SIGINT or
	// SIGTERM stops the HTTP server and the backend workers in order.
	baseCtx := cmd.Context()
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, stopSignals := signal.NotifyContext(baseCtx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	errChan := make(chan error, 2)

	wg.Add(2)

	go func() {
		defer close(errChan)
		wg.Wait()
	}()

	// Start the connection to the backend in a separate goroutine, so that
	// we can start the proxy server and start serving requests while we wait
	// for the connection to be established.
	go func() {
		defer wg.Done()

		var conn api.ModelRegistryApi

		if proxyCfg.Backend == "mlflow" {
			// The MLflow backend opens its own compatibility database and
			// native client; it never constructs the EmbedMD core
			// repositories.
			service, cleanup, err := factory(ctx, proxyCfg.MLflow)
			if err != nil {
				errChan <- fmt.Errorf("{{ALERT}} error starting mlflow backend: %w", err)
				return
			}
			cleanupOwner.Install(cleanup)
			if ctx.Err() != nil {
				return
			}
			conn = service
		} else {
			var err error
			ds, err = datastore.NewConnector(proxyCfg.DatastoreType, &proxyCfg.EmbedMD)
			if err != nil {
				errChan <- fmt.Errorf("error creating datastore: %w", err)
				return
			}

			conn, err = newModelRegistryService(ds)
			if err != nil {
				// {{ALERT}} is used to identify this error in pod logs, DO NOT REMOVE
				errChan <- fmt.Errorf("{{ALERT}} error connecting to datastore: %w", err)
				return
			}
		}

		mux, err := buildRegistryRouter(conn)
		if err != nil {
			errChan <- err
			return
		}

		router.SetRouter(middleware.WrapWithValidation(cfg.CORSAllowedOrigins, mux))

		// Set the model registry service in the holder for health checks AFTER router is ready
		// This ensures the readiness probe only passes when the router can serve actual requests
		serviceHolder.Set(conn)
	}()

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.Hostname, cfg.Port),
		Handler: mainHandler,
	}

	// Start the proxy server in a separate goroutine so that we can handle
	// errors from both the proxy server and the connection to the backend.
	go func() {
		defer wg.Done()

		glog.Infof("Proxy server started at %s:%v", cfg.Hostname, cfg.Port)

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("error starting proxy server: %w", err)
		}
	}()

	// Bound the shutdown so a stuck request cannot block exit; workers
	// stop before their SQL connection and HTTP transport close.
	shutdownServer := func() {
		stopSignals()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		shutdownBackend()
	}

	select {
	case err := <-errChan:
		shutdownServer()
		return err
	case <-ctx.Done():
		shutdownServer()
		glog.Info("Proxy server shut down")
		return nil
	}
}

// buildRegistryRouter wires both generated REST controllers over one
// registry service, applying the configured v1alpha3 deprecation
// headers.
func buildRegistryRouter(conn api.ModelRegistryApi) (*chi.Mux, error) {
	v1alpha3Service := v1alpha3.NewModelRegistryServiceAPIService(conn)
	v1alpha3Controller := v1alpha3.NewModelRegistryServiceAPIController(v1alpha3Service)

	v1Service := v1.NewModelRegistryServiceAPIService(conn)
	v1Controller := v1.NewModelRegistryServiceAPIController(v1Service)

	mux := chi.NewRouter()
	mux.Use(chimiddleware.Logger)

	if cfg.AlphaSunsetDate != "" {
		sunsetDate, err := time.Parse("2006-01-02", cfg.AlphaSunsetDate)
		if err != nil {
			return nil, fmt.Errorf("invalid --alpha-sunset-date %q: must be YYYY-MM-DD: %w", cfg.AlphaSunsetDate, err)
		}
		mux.Use(platformmw.DeprecationMiddleware(platformmw.DeprecationConfig{
			SunsetDate: sunsetDate,
			Paths: []platformmw.DeprecatedPath{
				{Prefix: "/api/model_registry/v1alpha3/", Successor: "/api/model_registry/v1/"},
			},
		}))
		glog.Infof("Alpha API (v1alpha3) deprecation headers enabled; sunset date: %s", cfg.AlphaSunsetDate)
	}

	for _, route := range v1alpha3Controller.OrderedRoutes() {
		mux.Method(route.Method, route.Pattern, route.HandlerFunc)
	}
	for _, route := range v1Controller.OrderedRoutes() {
		mux.Method(route.Method, route.Pattern, route.HandlerFunc)
	}

	return mux, nil
}

func newModelRegistryService(ds datastore.Connector) (api.ModelRegistryApi, error) {
	repoSet, err := ds.Connect(service.DatastoreSpec())
	if err != nil {
		return nil, err
	}

	modelRegistryService := core.NewModelRegistryService(
		getRepo[models.ArtifactRepository](repoSet),
		getRepo[models.ModelArtifactRepository](repoSet),
		getRepo[models.DocArtifactRepository](repoSet),
		getRepo[models.RegisteredModelRepository](repoSet),
		getRepo[models.ModelVersionRepository](repoSet),
		getRepo[models.ServingEnvironmentRepository](repoSet),
		getRepo[models.InferenceServiceRepository](repoSet),
		getRepo[models.ServeModelRepository](repoSet),
		getRepo[models.ExperimentRepository](repoSet),
		getRepo[models.ExperimentRunRepository](repoSet),
		getRepo[models.DataSetRepository](repoSet),
		getRepo[models.MetricRepository](repoSet),
		getRepo[models.ParameterRepository](repoSet),
		getRepo[models.MetricHistoryRepository](repoSet),
		repoSet.TypeMap(),
	)

	glog.Infof("EmbedMD service connected")

	return modelRegistryService, nil
}

func getRepo[T any](repoSet datastore.RepoSet) T {
	repo, err := repoSet.Repository(reflect.TypeFor[T]())
	if err != nil {
		panic(fmt.Sprintf("unable to get repository: %v", err))
	}

	return repo.(T)
}

func init() {
	rootCmd.AddCommand(proxyCmd)

	proxyCmd.Flags().StringVarP(&cfg.Hostname, "hostname", "n", cfg.Hostname, "Proxy server listen hostname")
	proxyCmd.Flags().IntVarP(&cfg.Port, "port", "p", cfg.Port, "Proxy server listen port")

	proxyCmd.Flags().StringVar(&proxyCfg.EmbedMD.DatabaseType, "embedmd-database-type", "mysql", "EmbedMD database type")
	proxyCmd.Flags().StringVar(&proxyCfg.EmbedMD.DatabaseDSN, "embedmd-database-dsn", "", "EmbedMD database DSN")
	proxyCmd.Flags().StringVar(&proxyCfg.EmbedMD.TLSConfig.CertPath, "embedmd-database-ssl-cert", "", "EmbedMD SSL cert path")
	proxyCmd.Flags().StringVar(&proxyCfg.EmbedMD.TLSConfig.KeyPath, "embedmd-database-ssl-key", "", "EmbedMD SSL key path")
	proxyCmd.Flags().StringVar(&proxyCfg.EmbedMD.TLSConfig.RootCertPath, "embedmd-database-ssl-root-cert", "", "EmbedMD SSL root cert path")
	proxyCmd.Flags().StringVar(&proxyCfg.EmbedMD.TLSConfig.CAPath, "embedmd-database-ssl-ca", "", "EmbedMD SSL CA path")
	proxyCmd.Flags().StringVar(&proxyCfg.EmbedMD.TLSConfig.Cipher, "embedmd-database-ssl-cipher", "", "Colon-separated list of allowed TLS ciphers for the EmbedMD database connection. Values are from the list at https://pkg.go.dev/crypto/tls#pkg-constants e.g. 'TLS_AES_128_GCM_SHA256:TLS_CHACHA20_POLY1305_SHA256'")
	proxyCmd.Flags().BoolVar(&proxyCfg.EmbedMD.TLSConfig.VerifyServerCert, "embedmd-database-ssl-verify-server-cert", false, "EmbedMD SSL verify server cert")

	proxyCmd.Flags().StringVar(&proxyCfg.DatastoreType, "datastore-type", proxyCfg.DatastoreType, "Datastore type")

	proxyCmd.Flags().StringVar(&proxyCfg.Backend, "backend", proxyCfg.Backend, "Registry backend: embedmd (default) or mlflow")

	bindMLflowResourceFlags(proxyCmd.Flags(), &proxyCfg.MLflow)
	proxyCmd.Flags().DurationVar(&proxyCfg.MLflow.RefreshInterval, "mlflow-refresh-interval", mlflow.DefaultRefreshInterval, "Interval between MLflow registry refreshes")
	// Unlike EmbedMD, MLflow database TLS verifies the server certificate
	// by default; --mlflow-database-ssl-skip-verify is intentionally not
	// offered.

	proxyCmd.Flags().StringSliceVar(&cfg.CORSAllowedOrigins, "cors-allowed-origins", nil,
		"Comma-separated list of allowed CORS origins. If empty (default), CORS is disabled.")

	proxyCmd.Flags().StringVar(&cfg.AlphaSunsetDate, "alpha-sunset-date", "",
		"Sunset date (YYYY-MM-DD) for the deprecated v1alpha3 API, per RFC 8594. "+
			"If empty (default), no deprecation headers are added to v1alpha3 responses.")
}
