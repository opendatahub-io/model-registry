package cmd

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/golang/glog"
	"github.com/kubeflow/model-registry/internal/datastore"
	"github.com/kubeflow/model-registry/internal/datastore/embedmd"
	"github.com/kubeflow/model-registry/internal/proxy"
	"github.com/kubeflow/model-registry/internal/server/openapi"
	"github.com/kubeflow/model-registry/internal/tls"
	"github.com/spf13/cobra"
)

type ProxyConfig struct {
	Datastore datastore.Datastore
}

const (
	// datastoreUnavailableMessage is the message returned when the datastore service is down or unavailable.
	datastoreUnavailableMessage = "Datastore service is down or unavailable. Please check that the database is reachable and try again later."
)

var (
	proxyCfg = ProxyConfig{
		Datastore: datastore.Datastore{
			Type: "embedmd",
			EmbedMD: embedmd.EmbedMDConfig{
				TLSConfig: &tls.TLSConfig{},
			},
		},
	}

	// proxyCmd represents the proxy command
	proxyCmd = &cobra.Command{
		Use:   "proxy",
		Short: "Starts the go OpenAPI proxy server to connect to a metadata store",
		Long: `This command launches the go OpenAPI proxy server.

The server connects to a metadata store, currently only MLMD is supported. It supports options to customize the
hostname and port where it listens.`,
		RunE: runProxyServer,
	}
)

func runProxyServer(cmd *cobra.Command, args []string) error {
	var (
		ds datastore.Connector
		wg sync.WaitGroup
	)

	router := proxy.NewDynamicRouter()

	router.SetRouter(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, datastoreUnavailableMessage, http.StatusServiceUnavailable)
	}))

	// readiness probe requires schema_migrations.dirty to be false before allowing traffic
	readinessHandler := proxy.ReadinessHandler(proxyCfg.Datastore)

	// route /readyz/isDirty to readinessHandler, all other paths to the dynamic router
	mainHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if strings.HasSuffix(r.URL.Path, "/readyz/isDirty") {
			readinessHandler.ServeHTTP(w, r)
			return
		}

		router.ServeHTTP(w, r)
	})

	errChan := make(chan error, 1)

	wg.Add(2)

	go func() {
		defer close(errChan)
		wg.Wait()
	}()

	// Start the connection to the Datastore server in a separate goroutine, so that
	// we can start the proxy server and start serving requests while we wait
	// for the connection to be established.
	go func() {
		var (
			err error
		)

		defer wg.Done()

		ds, err = datastore.NewConnector(proxyCfg.Datastore)
		if err != nil {
			errChan <- fmt.Errorf("error creating datastore: %w", err)
			return
		}

		conn, err := ds.Connect()
		if err != nil {
			// {{ALERT}} is used to identify this error in pod logs, DO NOT REMOVE
			errChan <- fmt.Errorf("{{ALERT}} error connecting to datastore: %w", err)
			return
		}

		ModelRegistryServiceAPIService := openapi.NewModelRegistryServiceAPIService(conn)
		ModelRegistryServiceAPIController := openapi.NewModelRegistryServiceAPIController(ModelRegistryServiceAPIService)

		router.SetRouter(openapi.NewRouter(ModelRegistryServiceAPIController))
	}()

	// Start the proxy server in a separate goroutine so that we can handle
	// errors from both the proxy server and the connection to the Datastore server.
	go func() {
		defer wg.Done()

		glog.Infof("Proxy server started at %s:%v", cfg.Hostname, cfg.Port)

		err := http.ListenAndServe(fmt.Sprintf("%s:%d", cfg.Hostname, cfg.Port), mainHandler)
		if err != nil {
			errChan <- fmt.Errorf("error starting proxy server: %w", err)
		}
	}()

	defer func() {
		if ds != nil {
			//nolint:errcheck
			ds.Teardown()
		}
	}()

	// Wait for either the Datastore server connection or the proxy server to return an error
	// or for both to finish successfully.
	return <-errChan
}

func init() {
	rootCmd.AddCommand(proxyCmd)

	proxyCmd.Flags().StringVarP(&cfg.Hostname, "hostname", "n", cfg.Hostname, "Proxy server listen hostname")
	proxyCmd.Flags().IntVarP(&cfg.Port, "port", "p", cfg.Port, "Proxy server listen port")

	proxyCmd.Flags().StringVar(&proxyCfg.Datastore.MLMD.Hostname, "mlmd-hostname", proxyCfg.Datastore.MLMD.Hostname, "MLMD hostname")
	proxyCmd.Flags().IntVar(&proxyCfg.Datastore.MLMD.Port, "mlmd-port", proxyCfg.Datastore.MLMD.Port, "MLMD port")

	proxyCmd.Flags().StringVar(&proxyCfg.Datastore.EmbedMD.DatabaseType, "embedmd-database-type", "mysql", "EmbedMD database type")
	proxyCmd.Flags().StringVar(&proxyCfg.Datastore.EmbedMD.DatabaseDSN, "embedmd-database-dsn", "", "EmbedMD database DSN")
	proxyCmd.Flags().StringVar(&proxyCfg.Datastore.EmbedMD.TLSConfig.CertPath, "embedmd-database-ssl-cert", "", "EmbedMD SSL cert path")
	proxyCmd.Flags().StringVar(&proxyCfg.Datastore.EmbedMD.TLSConfig.KeyPath, "embedmd-database-ssl-key", "", "EmbedMD SSL key path")
	proxyCmd.Flags().StringVar(&proxyCfg.Datastore.EmbedMD.TLSConfig.RootCertPath, "embedmd-database-ssl-root-cert", "", "EmbedMD SSL root cert path")
	proxyCmd.Flags().StringVar(&proxyCfg.Datastore.EmbedMD.TLSConfig.CAPath, "embedmd-database-ssl-ca", "", "EmbedMD SSL CA path")
	proxyCmd.Flags().StringVar(&proxyCfg.Datastore.EmbedMD.TLSConfig.Cipher, "embedmd-database-ssl-cipher", "", "Colon-separated list of allowed TLS ciphers for the EmbedMD database connection. Values are from the list at https://pkg.go.dev/crypto/tls#pkg-constants e.g. 'TLS_AES_128_GCM_SHA256:TLS_CHACHA20_POLY1305_SHA256'")
	proxyCmd.Flags().BoolVar(&proxyCfg.Datastore.EmbedMD.TLSConfig.VerifyServerCert, "embedmd-database-ssl-verify-server-cert", false, "EmbedMD SSL verify server cert")

	proxyCmd.Flags().StringVar(&proxyCfg.Datastore.Type, "datastore-type", proxyCfg.Datastore.Type, "Datastore type")
}
