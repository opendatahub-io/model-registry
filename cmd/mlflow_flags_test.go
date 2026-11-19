package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestMLflowResourceFlags(t *testing.T) {
	previous, previousServer := proxyCfg, cfg
	changed := map[string]bool{}
	proxyCmd.Flags().VisitAll(func(f *pflag.Flag) { changed[f.Name] = f.Changed })
	t.Cleanup(func() {
		proxyCfg, cfg = previous, previousServer
		proxyCmd.Flags().VisitAll(func(f *pflag.Flag) { f.Changed = changed[f.Name] })
	})
	args := []string{
		"--mlflow-registry-uri=https://registry.example/prefix",
		"--mlflow-database-type=postgres", "--mlflow-database-dsn=postgres://test:private-password@localhost:5432/testdb",
		"--mlflow-token-file=token-file", "--mlflow-ca-file=native-ca", "--mlflow-request-timeout=17s",
		"--mlflow-database-ssl-cert=client-cert", "--mlflow-database-ssl-key=client-key",
		"--mlflow-database-ssl-root-cert=root-cert", "--mlflow-database-ssl-ca=ca-cert",
		"--mlflow-database-ssl-cipher=TLS_AES_128_GCM_SHA256",
	}
	require.NoError(t, proxyCmd.ParseFlags(args))
	expected := proxyCfg.MLflow
	expected.RefreshInterval = mlflow.DefaultRefreshInterval
	require.Equal(t, 17*time.Second, expected.RequestTimeout)
	proxyCfg.Backend = "mlflow"
	cfg.Hostname, cfg.Port = "127.0.0.1", 0
	stopProxy := errors.New("captured proxy configuration")
	configs := make(chan MLflowBackendConfig, 1)
	err := runProxyServerWithFactory(&cobra.Command{}, nil, func(_ context.Context, config MLflowBackendConfig) (api.ModelRegistryApi, func() error, error) {
		configs <- config
		return nil, nil, stopProxy
	})
	require.ErrorIs(t, err, stopProxy)
	require.Equal(t, expected, <-configs)
	for _, action := range [][]string{{"list"}, {"bind", "--operation=uuid", "--model=model", "--version=1"}, {"retry-create", "--operation=uuid", "--accept-duplicate-risk"}, {"resolve-orphan", "--operation=uuid", "--retain-native"}} {
		t.Run(action[0], func(t *testing.T) {
			called := false
			stop := errors.New("captured configuration")
			command := newMLflowOperationsCommand(func(_ context.Context, config MLflowBackendConfig) (*mlflow.Store, *client.Client, func() error, error) {
				called = true
				require.Equal(t, expected, config)
				tlsConfig, err := config.validate()
				require.NoError(t, err)
				require.True(t, tlsConfig.VerifyServerCert)
				return nil, nil, nil, stop
			})
			command.SetArgs(append(append([]string{}, args...), action...))
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			require.Nil(t, command.PersistentFlags().Lookup("mlflow-database-ssl-skip-verify"))
			require.ErrorIs(t, command.Execute(), stop)
			require.True(t, called)
		})
	}
}

func TestMLflowResourceFlagsConfig(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	previousFile := cfgFile
	t.Cleanup(func() { cfgFile = previousFile })
	cfgFile = t.TempDir() + "/settings.yaml"
	require.NoError(t, os.WriteFile(cfgFile, []byte("mlflow-registry-uri: https://registry.example\nmlflow-database-type: postgres\nmlflow-database-dsn: configured-dsn\nmlflow-database-ssl-cert: configured-cert\nmlflow-database-ssl-key: configured-key\nmlflow-database-ssl-root-cert: configured-root\nmlflow-database-ssl-ca: configured-ca\nmlflow-database-ssl-cipher: TLS_AES_128_GCM_SHA256\n"), 0600))
	stop := errors.New("captured file configuration")
	command := newMLflowOperationsCommand(func(_ context.Context, config MLflowBackendConfig) (*mlflow.Store, *client.Client, func() error, error) {
		require.Equal(t, "configured-dsn", config.DatabaseDSN)
		require.Equal(t, "configured-cert", config.DatabaseTLS.CertPath)
		require.Equal(t, "configured-key", config.DatabaseTLS.KeyPath)
		require.Equal(t, "configured-root", config.DatabaseTLS.RootCertPath)
		require.Equal(t, "configured-ca", config.DatabaseTLS.CAPath)
		require.Equal(t, "TLS_AES_128_GCM_SHA256", config.DatabaseTLS.Cipher)
		return nil, nil, nil, stop
	})
	command.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error { return initConfig(cmd) }
	command.SetArgs([]string{"list"})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	require.ErrorIs(t, command.Execute(), stop)
}

func TestMLflowResourceFlagsEnvironment(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	previousFile := cfgFile
	cfgFile = ""
	t.Cleanup(func() { cfgFile = previousFile })
	for key, value := range map[string]string{
		"MLFLOW_REGISTRY_URI":  "https://registry.example",
		"MLFLOW_DATABASE_TYPE": "postgres", "MLFLOW_DATABASE_DSN": "environment-dsn",
		"MLFLOW_DATABASE_SSL_CERT": "environment-cert", "MLFLOW_DATABASE_SSL_KEY": "environment-key",
		"MLFLOW_DATABASE_SSL_ROOT_CERT": "environment-root", "MLFLOW_DATABASE_SSL_CA": "environment-ca",
		"MLFLOW_DATABASE_SSL_CIPHER": "TLS_AES_128_GCM_SHA256",
	} {
		t.Setenv(EnvPrefix+"_"+key, value)
	}
	stop := errors.New("captured environment")
	command := newMLflowOperationsCommand(func(_ context.Context, config MLflowBackendConfig) (*mlflow.Store, *client.Client, func() error, error) {
		require.Equal(t, "environment-dsn", config.DatabaseDSN)
		require.Equal(t, "environment-cert", config.DatabaseTLS.CertPath)
		require.Equal(t, "environment-key", config.DatabaseTLS.KeyPath)
		require.Equal(t, "environment-root", config.DatabaseTLS.RootCertPath)
		require.Equal(t, "environment-ca", config.DatabaseTLS.CAPath)
		require.Equal(t, "TLS_AES_128_GCM_SHA256", config.DatabaseTLS.Cipher)
		return nil, nil, nil, stop
	})
	command.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error { return initConfig(cmd) }
	command.SetArgs([]string{"list"})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	require.ErrorIs(t, command.Execute(), stop)
}

func TestRegistryURLRedactionCommand(t *testing.T) {
	for _, raw := range []string{"https://user:dummy-secret@registry.example", "https://user:dummy-secret%zz@registry.example", "https://registry.example?token=dummy-secret"} {
		t.Run(raw, func(t *testing.T) {
			command := newMLflowOperationsCommand(func(context.Context, MLflowBackendConfig) (*mlflow.Store, *client.Client, func() error, error) {
				t.Fatal("invalid registry URL must fail before resource factory")
				return nil, nil, nil, nil
			})
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetArgs([]string{"--mlflow-registry-uri=" + raw, "list"})
			err := command.Execute()
			require.ErrorIs(t, err, api.ErrBadRequest)
			require.NotContains(t, err.Error()+output.String(), "dummy-secret")
			require.NotContains(t, err.Error()+output.String(), raw)
			_, _, _, err = openMLflowResources(t.Context(), MLflowBackendConfig{RegistryURI: raw, DatabaseType: "mysql", DatabaseDSN: "unused"})
			require.ErrorIs(t, err, api.ErrBadRequest)
			require.NotContains(t, err.Error(), "dummy-secret")
		})
	}
}

func TestRegistryURLRedactionDatabaseConfiguration(t *testing.T) {
	for engine, dsn := range map[string]string{
		"postgres": "postgres://user:dummy-secret%zz@database.example/db",
		"mysql":    "dummy-secret-invalid-dsn",
	} {
		t.Run(engine, func(t *testing.T) {
			config := MLflowBackendConfig{RegistryURI: "https://registry.example", DatabaseType: engine, DatabaseDSN: dsn}
			_, err := config.validate()
			require.Error(t, err)
			require.NotContains(t, err.Error(), "dummy-secret")
			_, _, _, err = openMLflowResources(t.Context(), config)
			require.ErrorIs(t, err, api.ErrBadRequest)
			require.NotContains(t, err.Error(), "dummy-secret")
		})
	}
}

func TestRegistryURLRedactionEffectivePostgresTLS(t *testing.T) {
	dir := t.TempDir()
	invalid := dir + "/invalid.pem"
	require.NoError(t, os.WriteFile(invalid, []byte("not a certificate or key"), 0600))
	const dsn = "postgres://test:dummy-password@127.0.0.1:1/testdb?application_name=dummy-query-secret"
	for name, flags := range map[string][]string{
		"missing root":        {"--mlflow-database-ssl-root-cert=" + dir + "/missing.pem"},
		"invalid root":        {"--mlflow-database-ssl-root-cert=" + invalid},
		"missing CA":          {"--mlflow-database-ssl-ca=" + dir + "/missing.pem"},
		"missing client pair": {"--mlflow-database-ssl-cert=" + dir + "/missing-cert.pem", "--mlflow-database-ssl-key=" + dir + "/missing-key.pem"},
		"invalid client pair": {"--mlflow-database-ssl-cert=" + invalid, "--mlflow-database-ssl-key=" + invalid},
	} {
		t.Run(name, func(t *testing.T) {
			command := newMLflowOperationsCommand(openMLflowResources)
			args := []string{"--mlflow-registry-uri=http://127.0.0.1:5000", "--mlflow-database-type=postgres", "--mlflow-database-dsn=" + dsn}
			command.SetArgs(append(append(args, flags...), "list"))
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			err := command.Execute()
			require.Error(t, err)
			for _, secret := range []string{dsn, "postgres://", "dummy-password", "dummy-query-secret"} {
				require.NotContains(t, err.Error()+output.String(), secret)
			}
			require.ErrorIs(t, err, api.ErrBadRequest)
		})
	}
}
