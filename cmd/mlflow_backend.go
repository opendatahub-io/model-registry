package cmd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/golang/glog"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kubeflow/hub/internal/db/types"
	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/mlflow/client"
	platformdb "github.com/kubeflow/hub/internal/platform/db"
	"github.com/kubeflow/hub/internal/platform/db/mysql"
	"github.com/kubeflow/hub/internal/platform/db/postgres"
	platformtls "github.com/kubeflow/hub/internal/platform/tls"
	"github.com/kubeflow/hub/pkg/api"
	"gorm.io/gorm"
)

// MLflowBackendConfig is the runtime configuration of the MLflow
// compatibility backend. It mirrors EmbedMDConfig's shape: the DSN may
// carry credentials and is supplied only through flags, environment
// variables, or the config file — never logged. Authentication
// material for the native registry stays in files ( TokenFile,
// PasswordFile ) for the same reason.
type MLflowBackendConfig struct {
	RegistryURI     string
	DatabaseType    string
	DatabaseDSN     string
	TokenFile       string
	Username        string
	PasswordFile    string
	CAFile          string
	RequestTimeout  time.Duration
	RefreshInterval time.Duration
	DatabaseTLS     platformtls.TLSConfig
}

// validate checks the configuration and returns the normalized SQL TLS
// settings. Unlike EmbedMD — whose verify flag defaults to off even
// when TLS material is configured — any configured TLS material ( CA,
// client pair, or cipher list ) forces server-certificate verification
// on with no opt-out flag; a configuration with no TLS material is a
// plain connection. A one-sided client certificate/key pair is
// rejected.
func (c MLflowBackendConfig) validate() (platformtls.TLSConfig, error) {
	if c.RegistryURI == "" {
		return platformtls.TLSConfig{}, errors.New("an mlflow registry URI is required")
	}
	if _, err := client.ParseRegistryURL(c.RegistryURI); err != nil {
		return platformtls.TLSConfig{}, err
	}
	if c.DatabaseType != types.DatabaseTypeMySQL && c.DatabaseType != types.DatabaseTypePostgres {
		return platformtls.TLSConfig{}, fmt.Errorf("unsupported mlflow database type %q: supported types: %s, %s",
			c.DatabaseType, types.DatabaseTypeMySQL, types.DatabaseTypePostgres)
	}
	if c.DatabaseDSN == "" {
		return platformtls.TLSConfig{}, errors.New("an mlflow database DSN is required")
	}
	// Validate before connector construction: driver parse errors can include
	// the complete credential-bearing DSN, including on connector log paths.
	var dsnErr error
	if c.DatabaseType == types.DatabaseTypeMySQL {
		_, dsnErr = mysqldriver.ParseDSN(c.DatabaseDSN)
	} else {
		_, dsnErr = pgx.ParseConfig(c.DatabaseDSN)
	}
	if dsnErr != nil {
		return platformtls.TLSConfig{}, errors.New("invalid mlflow database DSN")
	}
	if (c.TokenFile != "") && (c.Username != "" || c.PasswordFile != "") {
		return platformtls.TLSConfig{}, errors.New("mlflow registry authentication must use either a token file or basic auth, not both")
	}
	if (c.Username == "") != (c.PasswordFile == "") {
		return platformtls.TLSConfig{}, errors.New("mlflow registry basic auth requires both a username and a password file")
	}

	tlsConfig := c.DatabaseTLS
	if (tlsConfig.CertPath == "") != (tlsConfig.KeyPath == "") {
		return platformtls.TLSConfig{}, errors.New("mlflow database TLS requires both a client certificate and a key")
	}
	configuredTLS := tlsConfig.CertPath != "" || tlsConfig.KeyPath != "" ||
		tlsConfig.RootCertPath != "" || tlsConfig.CAPath != "" || tlsConfig.Cipher != ""
	// Any TLS material forces verification on; none leaves a plain
	// connection, which has no certificate to verify.
	tlsConfig.VerifyServerCert = configuredTLS
	return tlsConfig, nil
}

// MLflowResourceFactory opens the backend's owned resources: the SQL
// compatibility connection and the native registry client. It connects
// and builds only — no migration, identity initialization, refresh, or
// worker starts — so operator inspection can reuse the transport and
// connection configuration without triggering mutating startup work.
// The returned cleanup closes the SQL connection and the configured
// HTTP transport's idle connections; it is idempotent and safe to call
// on a nil error path.
type MLflowResourceFactory func(context.Context, MLflowBackendConfig) (*mlflow.Store, *client.Client, func() error, error)

// openMLflowResources is the production resource factory.
func openMLflowResources(ctx context.Context, config MLflowBackendConfig) (*mlflow.Store, *client.Client, func() error, error) {
	tlsConfig, err := config.validate()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid mlflow backend configuration: %w: %w", err, api.ErrBadRequest)
	}

	var connector platformdb.Connector
	switch config.DatabaseType {
	case types.DatabaseTypeMySQL:
		connector = mysql.NewMySQLDBConnector(config.DatabaseDSN, &tlsConfig)
	case types.DatabaseTypePostgres:
		postgresConnector := postgres.NewPostgresDBConnector(config.DatabaseDSN, &tlsConfig)
		// TLS flags change the effective DSN. Validate that form before
		// connecting, discarding parse errors that include the whole DSN.
		if tlsConfig.VerifyServerCert {
			effectiveDSN, err := postgresConnector.BuildDSNWithTLS()
			if err == nil {
				_, err = pgx.ParseConfig(effectiveDSN)
			}
			if err != nil {
				return nil, nil, nil, fmt.Errorf("invalid mlflow database TLS or connection settings: %w", api.ErrBadRequest)
			}
		}
		connector = postgresConnector
	default:
		return nil, nil, nil, fmt.Errorf("unsupported mlflow database type %q: %w", config.DatabaseType, api.ErrBadRequest)
	}

	var db *gorm.DB
	if contextual, ok := connector.(interface {
		ConnectContext(context.Context) (*gorm.DB, error)
	}); ok {
		db, err = contextual.ConnectContext(ctx)
	} else {
		db, err = connector.Connect()
	}
	if err != nil {
		// Files/environment may change after preflight. Never forward a
		// later driver's parse error containing credentials or DSN queries.
		var parseErr *pgconn.ParseConfigError
		if errors.As(err, &parseErr) {
			return nil, nil, nil, fmt.Errorf("invalid mlflow database TLS or connection settings: %w", api.ErrBadRequest)
		}
		return nil, nil, nil, fmt.Errorf("connecting the mlflow compatibility database: %w", err)
	}

	transportConfig := client.TransportConfig{
		BearerTokenFile: config.TokenFile,
		Username:        config.Username,
		PasswordFile:    config.PasswordFile,
		CAFile:          config.CAFile,
	}
	httpClient, err := transportConfig.HTTPClient()
	if err != nil {
		closeDB(db)
		return nil, nil, nil, fmt.Errorf("building the mlflow registry transport: %w", err)
	}
	httpClient.Timeout = config.RequestTimeout

	native, err := client.New(config.RegistryURI, httpClient)
	if err != nil {
		closeDB(db)
		return nil, nil, nil, err
	}

	cleanup := func() error {
		native.CloseIdleConnections()
		closeDB(db)
		return nil
	}
	return mlflow.NewStore(db), native, cleanup, nil
}

// closeDB closes one gorm handle's underlying SQL connection pool,
// reporting nothing: it runs on cleanup paths where the error has no
// handler.
func closeDB(db *gorm.DB) {
	sqlDB, err := db.DB()
	if err != nil {
		return
	}
	_ = sqlDB.Close()
}

// newRegistryBackend performs the full MLflow backend startup against
// the configured settings: open resources, migrate the compatibility
// schema, initialize the adapter identity, complete the initial
// refresh, and start the reconciliation loop. Only then is the service
// published; the returned cleanup cancels and joins the service
// workers before closing the SQL connection and the transport's idle
// connections. It runs on initialization error, cancellation, and
// shutdown, and is idempotent.
func newRegistryBackend(ctx context.Context, config MLflowBackendConfig) (api.ModelRegistryApi, func() error, error) {
	return newRegistryBackendWithFactories(ctx, config, openMLflowResources, platformdb.NewDBMigrator)
}

func newRegistryBackendWithFactories(ctx context.Context, config MLflowBackendConfig, resources MLflowResourceFactory, migration platformdb.MigratorFactory) (api.ModelRegistryApi, func() error, error) {
	store, native, cleanup, err := resources(ctx, config)
	if err != nil {
		if cleanup != nil {
			_ = cleanup()
		}
		return nil, nil, err
	}
	var cleanupOnce sync.Once
	finalCleanup := func() {
		cleanupOnce.Do(func() { _ = cleanup() })
	}
	abort := func(cause error) (api.ModelRegistryApi, func() error, error) {
		finalCleanup()
		return nil, nil, cause
	}

	if err := ctx.Err(); err != nil {
		return abort(fmt.Errorf("mlflow backend startup canceled: %w", err))
	}

	// Migrations run through the same platform migrators EmbedMD uses,
	// against this backend's separate connection.
	migrator, err := migration(store.DB())
	if err != nil {
		return abort(fmt.Errorf("creating the mlflow compatibility migrator: %w", err))
	}
	if closer, ok := migrator.(interface{ Close() error }); ok {
		defer func() { _ = closer.Close() }()
	}
	if err := migrator.Migrate(); err != nil {
		return abort(fmt.Errorf("migrating the mlflow compatibility database: %w", err))
	}

	service, err := mlflow.NewService(mlflow.Config{
		RegistryURI:     config.RegistryURI,
		RequestTimeout:  config.RequestTimeout,
		RefreshInterval: config.RefreshInterval,
	}, store, native)
	if err != nil {
		return abort(err)
	}
	stopService := func() { _ = service.Close() }

	fingerprint, err := mlflow.RegistryTargetFingerprint(config.RegistryURI)
	if err != nil {
		stopService()
		return abort(err)
	}
	if _, err := store.InitializeAdapter(ctx, fingerprint); err != nil {
		stopService()
		return abort(fmt.Errorf("initializing the mlflow adapter identity: %w", err))
	}

	// The initial refresh publishes the current native observation
	// before the service is published; a failed refresh fails startup.
	if err := service.Refresh(ctx); err != nil {
		stopService()
		return abort(fmt.Errorf("completing the initial mlflow refresh: %w", err))
	}

	// The reconciliation loop runs for the command's lifetime; service
	// cancellation is tied to the caller's context so shutdown joins
	// the workers before the cleanup closes SQL and idle connections.
	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		_ = service.Run(ctx)
	}()
	select {
	case <-service.Running():
	case <-ctx.Done():
		stopService()
		<-workersDone
		return abort(ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		stopService()
		<-workersDone
		return abort(err)
	}

	shutdown := func() error {
		_ = service.Close()
		<-workersDone
		finalCleanup()
		return nil
	}
	glog.Info("MLflow backend started")
	return service, shutdown, nil
}
