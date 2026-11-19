package cmd

import (
	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/spf13/pflag"
)

// bindMLflowResourceFlags keeps serving and operator recovery on the same
// resource and certificate policy. Refresh scheduling belongs to the proxy.
func bindMLflowResourceFlags(flags *pflag.FlagSet, config *MLflowBackendConfig) {
	flags.StringVar(&config.RegistryURI, "mlflow-registry-uri", "", "MLflow registry base URL (http or https)")
	flags.StringVar(&config.DatabaseType, "mlflow-database-type", "mysql", "MLflow compatibility database type (mysql or postgres)")
	flags.StringVar(&config.DatabaseDSN, "mlflow-database-dsn", "", "MLflow compatibility database DSN; supply credentials through this setting from the environment or a config file, never through logs")
	flags.StringVar(&config.TokenFile, "mlflow-token-file", "", "File containing the MLflow registry bearer token")
	flags.StringVar(&config.Username, "mlflow-username", "", "MLflow registry basic-auth username")
	flags.StringVar(&config.PasswordFile, "mlflow-password-file", "", "File containing the MLflow registry basic-auth password")
	flags.StringVar(&config.CAFile, "mlflow-ca-file", "", "PEM bundle used to verify the MLflow registry certificate")
	flags.DurationVar(&config.RequestTimeout, "mlflow-request-timeout", mlflow.DefaultRequestTimeout, "Timeout for each MLflow registry request")
	flags.StringVar(&config.DatabaseTLS.CertPath, "mlflow-database-ssl-cert", "", "MLflow database SSL client certificate path")
	flags.StringVar(&config.DatabaseTLS.KeyPath, "mlflow-database-ssl-key", "", "MLflow database SSL client key path")
	flags.StringVar(&config.DatabaseTLS.RootCertPath, "mlflow-database-ssl-root-cert", "", "MLflow database SSL root certificate path")
	flags.StringVar(&config.DatabaseTLS.CAPath, "mlflow-database-ssl-ca", "", "MLflow database SSL CA path")
	flags.StringVar(&config.DatabaseTLS.Cipher, "mlflow-database-ssl-cipher", "", "Colon-separated list of allowed TLS ciphers for the MLflow database connection, e.g. 'TLS_AES_128_GCM_SHA256:TLS_CHACHA20_POLY1305_SHA256'")
}
