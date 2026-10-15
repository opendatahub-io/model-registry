//	@title			Model Registry BFF REST API
//	@version		1.0.0
//	@description	REST API for Model Registry BFF
//	@license.name	Apache 2.0
//	@license.url	https://www.apache.org/licenses/LICENSE-2.0

//	@host		localhost:4000
//	@BasePath	/api/v1
//	@schemes	http https

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/kubeflow/hub/ui/bff/internal/api"
	"github.com/kubeflow/hub/ui/bff/internal/config"
	"github.com/kubeflow/hub/ui/bff/internal/tlsprofile"
	configv1 "github.com/openshift/api/config/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	restclient "k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	_ "github.com/kubeflow/hub/ui/bff/openapi" // swagger docs for Swagger UI
)

type tlsPolicyRuntime struct {
	policy *tlsprofile.Result
	done   chan error
	stop   context.CancelFunc
}

type tlsPolicyDependencies struct {
	getConfig    func() (*restclient.Config, error)
	resolve      func(*restclient.Config) (*tlsprofile.Result, error)
	startWatcher func(*restclient.Config, *tlsprofile.Result) (chan error, context.CancelFunc, error)
}

func waitForShutdown(shutdownCh <-chan os.Signal, serverDone <-chan error, policyManagerDone <-chan error) (int, error) {
	select {
	case <-shutdownCh:
		return 0, nil
	case err := <-serverDone:
		if err != nil && err != http.ErrServerClosed {
			return 1, fmt.Errorf("BFF server stopped unexpectedly: %w", err)
		}
		return 0, nil
	case err := <-policyManagerDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			return 1, fmt.Errorf("TLS policy watcher stopped: %w", err)
		}
		return 0, nil
	}
}

func initializeTLSPolicy(certFile, keyFile string, mockK8Client bool, deps tlsPolicyDependencies) (*tlsPolicyRuntime, error) {
	runtime := &tlsPolicyRuntime{}
	if certFile == "" || keyFile == "" || mockK8Client {
		return runtime, nil
	}

	kubeConfig, err := deps.getConfig()
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes config for TLS policy: %w", err)
	}
	runtime.policy, err = deps.resolve(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("resolve cluster TLS policy: %w", err)
	}
	if runtime.policy.HasOpenShiftConfigAPI {
		runtime.done, runtime.stop, err = deps.startWatcher(kubeConfig, runtime.policy)
		if err != nil {
			return nil, fmt.Errorf("start TLS policy watcher: %w", err)
		}
	}
	return runtime, nil
}

func main() {
	var cfg config.EnvConfig
	var certFile, keyFile string

	flag.IntVar(&cfg.Port, "port", getEnvAsInt("PORT", 8080), "API server port")
	flag.StringVar(&certFile, "cert-file", "", "Path to TLS certificate file")
	flag.StringVar(&keyFile, "key-file", "", "Path to TLS key file")
	flag.BoolVar(&cfg.MockK8Client, "mock-k8s-client", false, "Use mock Kubernetes client")
	flag.BoolVar(&cfg.MockMRClient, "mock-mr-client", false, "Use mock Model Registry client")
	flag.BoolVar(&cfg.MockMRCatalogClient, "mock-mr-catalog-client", false, "Use mock Model Registry Catalog client")
	flag.BoolVar(&cfg.DevMode, "dev-mode", false, "Use development mode for access to local K8s cluster")
	flag.IntVar(&cfg.DevModeModelRegistryPort, "dev-mode-model-registry-port", getEnvAsInt("DEV_MODE_MODEL_REGISTRY_PORT", 8080), "Use port when in development mode for model registry")
	flag.IntVar(&cfg.DevModeCatalogPort, "dev-mode-catalog-port", getEnvAsInt("DEV_MODE_CATALOG_PORT", 8081), "Use port when in development mode for catalog")

	// New deployment mode flag
	flag.Var(&cfg.DeploymentMode, "deployment-mode", "Deployment mode (kubeflow, federated, or standalone)")

	flag.StringVar(&cfg.StaticAssetsDir, "static-assets-dir", "./static", "Configure frontend static assets root directory")
	flag.TextVar(&cfg.LogLevel, "log-level", parseLevel(getEnvAsString("LOG_LEVEL", "INFO")), "Sets server log level, possible values: error, warn, info, debug")
	flag.Func("allowed-origins", "Sets allowed origins for CORS purposes, accepts a comma separated list of origins or * to allow all, default none", newOriginParser(&cfg.AllowedOrigins, getEnvAsString("ALLOWED_ORIGINS", "")))
	// bundle-paths accepts a comma-separated list of CA bundle file paths to trust for outbound TLS.
	// If not provided via flag, it can be set via BUNDLE_PATHS env var (comma-separated). Defaults to empty.
	defaultBundlePaths := getEnvAsString("BUNDLE_PATHS", "")
	flag.StringVar(&cfg.OCIBaseImageConfigNamespace, "oci-base-image-config-namespace", getEnvAsString("OCI_BASE_IMAGE_CONFIG_NAMESPACE", ""), "Namespace containing the OCI base-image ConfigMap (defaults to the BFF Pod namespace)")
	flag.Func("bundle-paths", "Comma-separated list of PEM CA bundle file paths to trust for outbound TLS (optional)", newOriginParser(&cfg.BundlePaths, defaultBundlePaths))
	flag.StringVar(&cfg.AuthMethod, "auth-method", "internal", "Authentication method (internal or user_token)")
	flag.StringVar(&cfg.AuthTokenHeader, "auth-token-header", getEnvAsString("AUTH_TOKEN_HEADER", config.DefaultAuthTokenHeader), "Header used to extract the token (e.g., Authorization)")
	flag.StringVar(&cfg.AuthTokenPrefix, "auth-token-prefix", getEnvAsString("AUTH_TOKEN_PREFIX", config.DefaultAuthTokenPrefix), "Prefix used in the token header (e.g., 'Bearer ')")

	// TLS configuration flags
	flag.BoolVar(&cfg.InsecureSkipVerify, "insecure-skip-verify", getEnvAsBool("INSECURE_SKIP_VERIFY", false), "Skip TLS certificate verification (useful for development, default: false)")

	// Deprecated flags - kept for backward compatibility
	flag.BoolVar(&cfg.StandaloneMode, "standalone-mode", false, "DEPRECATED: Use -deployment-mode=standalone instead")
	flag.BoolVar(&cfg.FederatedPlatform, "federated-platform", false, "DEPRECATED: Use -deployment-mode=federated instead")

	flag.Parse()

	// Handle backward compatibility: if old flags are used, override deployment mode
	if cfg.StandaloneMode {
		cfg.DeploymentMode = config.DeploymentModeStandalone
	} else if cfg.FederatedPlatform {
		cfg.DeploymentMode = config.DeploymentModeFederated
	}

	// Ensure the deprecated boolean fields are consistent with the new deployment mode
	cfg.StandaloneMode = cfg.DeploymentMode.IsStandaloneMode()
	cfg.FederatedPlatform = cfg.DeploymentMode.IsFederatedMode()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))

	//validate auth method
	if cfg.AuthMethod != config.AuthMethodInternal && cfg.AuthMethod != config.AuthMethodUser {
		logger.Error("invalid auth method: (must be internal or user_token)", "authMethod", cfg.AuthMethod)
		os.Exit(1)
	}

	// Only use for logging errors about logging configuration.
	slog.SetDefault(logger)

	policyDeps := tlsPolicyDependencies{
		getConfig: ctrl.GetConfig,
		resolve:   tlsprofile.Resolve,
		startWatcher: func(kubeConfig *restclient.Config, tlsPolicy *tlsprofile.Result) (chan error, context.CancelFunc, error) {
			scheme := runtime.NewScheme()
			utilruntime.Must(configv1.Install(scheme))
			policyManager, err := ctrl.NewManager(kubeConfig, ctrl.Options{
				Scheme:                 scheme,
				Logger:                 logr.FromSlogHandler(logger.Handler()),
				Metrics:                server.Options{BindAddress: "0"},
				HealthProbeBindAddress: "0",
			})
			if err != nil {
				return nil, nil, err
			}
			var watcherCtx context.Context
			watcherCtx, stopPolicyManager := context.WithCancel(context.Background())
			watcher := tlsprofile.NewWatcher(tlsPolicy, stopPolicyManager)
			watcher.Client = policyManager.GetClient()
			if err := watcher.SetupWithManager(policyManager); err != nil {
				return nil, nil, err
			}
			policyManagerDone := make(chan error, 1)
			go func() { policyManagerDone <- policyManager.Start(watcherCtx) }()
			return policyManagerDone, stopPolicyManager, nil
		},
	}
	tlsRuntime, err := initializeTLSPolicy(certFile, keyFile, cfg.MockK8Client, policyDeps)
	if err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}
	tlsPolicy := tlsRuntime.policy
	policyManagerDone := tlsRuntime.done
	stopPolicyManager := tlsRuntime.stop
	if tlsPolicy != nil {
		if tlsPolicy.FallbackReason != "" {
			logger.Info("unable to read cluster TLS policy, using Intermediate profile", "error", tlsPolicy.FallbackReason)
		}
		if len(tlsPolicy.UnsupportedCipherSuites) > 0 {
			logger.Warn("ignoring unsupported TLS cipher suites", "cipherSuites", tlsPolicy.UnsupportedCipherSuites)
		}
	}

	app, err := api.NewApp(cfg, slog.New(logger.Handler()))
	if err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      app.Routes(),
		IdleTimeout:  time.Minute,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	serverDone := make(chan error, 1)
	exitCode := 0

	// Start the server in a goroutine
	go func() {
		logger.Info("starting server", "addr", srv.Addr, "TLS enabled", (certFile != "" && keyFile != ""))
		var err error
		if certFile != "" && keyFile != "" {
			srv.TLSConfig = buildTLSConfig(tlsPolicy)
			if shouldDisableHTTP2(tlsPolicy) {
				srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
			}
			err = srv.ListenAndServeTLS(certFile, keyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			logger.Error("HTTP server ListenAndServe", "error", err)
		}
		serverDone <- err
	}()

	// Graceful shutdown setup
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// A TLS policy change stops the watcher manager and triggers the same graceful shutdown as a signal.
	shutdownExitCode, shutdownErr := waitForShutdown(shutdownCh, serverDone, policyManagerDone)
	exitCode = shutdownExitCode
	if shutdownErr != nil {
		logger.Error(shutdownErr.Error())
	}
	logger.Info("shutting down gracefully...")
	if stopPolicyManager != nil {
		stopPolicyManager()
	}

	// Create a context with timeout for the shutdown process
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Shutdown the HTTP server gracefully
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("server shutdown failed", "error", err)
	}

	// Shutdown the App gracefully
	if err := app.Shutdown(); err != nil {
		logger.Error("failed to shutdown Kubernetes manager", "error", err)
	}

	logger.Info("server stopped")
	os.Exit(exitCode)
}

func buildTLSConfig(policy *tlsprofile.Result) *tls.Config {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"h2", "http/1.1"},
	}
	if policy != nil && !policy.KeepExisting {
		cfg.MinVersion = policy.MinVersion
		cfg.CipherSuites = policy.CipherSuites
		// Keep ALPN consistent with the disabled HTTP/2 dispatch: advertising
		// h2 without a TLSNextProto handler makes net/http close h2-negotiated
		// connections without serving a response.
		if shouldDisableHTTP2(policy) {
			cfg.NextProtos = []string{"http/1.1"}
		}
	}
	return cfg
}

func shouldDisableHTTP2(policy *tlsprofile.Result) bool {
	return policy != nil && !policy.KeepExisting &&
		policy.MinVersion < tls.VersionTLS13 && !hasHTTP2CipherSuite(policy.CipherSuites)
}

func hasHTTP2CipherSuite(cipherSuites []uint16) bool {
	for _, suite := range cipherSuites {
		if suite == tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 || suite == tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 {
			return true
		}
	}
	return false
}
