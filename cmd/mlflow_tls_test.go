package cmd

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/platform/db/mysql"
	platformtls "github.com/kubeflow/hub/internal/platform/tls"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	containermysql "github.com/testcontainers/testcontainers-go/modules/mysql"
)

// Exercise the real resource factory over verified TLS, including a database
// account that requires a client certificate. Config capture alone cannot
// establish that recovery can connect with that certificate policy.
func TestMLflowResourceFlagsTLSConnection(t *testing.T) {
	dir := t.TempDir()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "MLflow test CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600))
	certificate := func(serial int64, usage x509.ExtKeyUsage) (string, string) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, err)
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "MLflow test certificate"},
			NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{usage}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		require.NoError(t, err)
		certPath := filepath.Join(dir, template.SerialNumber.String()+".pem")
		keyPath := filepath.Join(dir, template.SerialNumber.String()+"-key.pem")
		require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
		require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600))
		return certPath, keyPath
	}
	serverCert, serverKey := certificate(2, x509.ExtKeyUsageServerAuth)
	clientCert, clientKey := certificate(3, x509.ExtKeyUsageClientAuth)
	configPath := filepath.Join(dir, "tls.cnf")
	require.NoError(t, os.WriteFile(configPath, []byte("[mysqld]\nssl-ca=/etc/mlflow-tls/ca.pem\nssl-cert=/etc/mlflow-tls/server.pem\nssl-key=/etc/mlflow-tls/server-key.pem\nrequire_secure_transport=ON\n"), 0600))
	container, err := containermysql.Run(t.Context(), "mysql:8.3",
		containermysql.WithUsername("root"), containermysql.WithPassword("testpass"), containermysql.WithDatabase("testdb"),
		containermysql.WithConfigFile(configPath),
		testcontainers.WithEnv(map[string]string{"MYSQL_ROOT_HOST": "%"}),
		testcontainers.WithFiles(
			testcontainers.ContainerFile{HostFilePath: caPath, ContainerFilePath: "/etc/mlflow-tls/ca.pem", FileMode: 0644},
			testcontainers.ContainerFile{HostFilePath: serverCert, ContainerFilePath: "/etc/mlflow-tls/server.pem", FileMode: 0644},
			testcontainers.ContainerFile{HostFilePath: serverKey, ContainerFilePath: "/etc/mlflow-tls/server-key.pem", FileMode: 0644},
		))
	testcontainers.CleanupContainer(t, container)
	require.NoError(t, err)
	config := MLflowBackendConfig{RegistryURI: "https://registry.example", DatabaseType: "mysql", DatabaseDSN: container.MustConnectionString(t.Context()),
		DatabaseTLS: platformtls.TLSConfig{CertPath: clientCert, KeyPath: clientKey, RootCertPath: caPath, CAPath: caPath}}
	store, _, cleanup, err := openMLflowResources(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanup()) })
	require.NoError(t, store.DB().Exec("ALTER USER 'root'@'%' REQUIRE X509").Error)
	// A second factory connection must actually present the configured pair.
	second, _, cleanupSecond, err := openMLflowResources(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanupSecond()) })
	var name, cipher string
	require.NoError(t, second.DB().Raw("SHOW STATUS LIKE 'Ssl_cipher'").Row().Scan(&name, &cipher))
	require.NotEmpty(t, cipher)
	withoutPair := config
	withoutPair.DatabaseTLS.CertPath, withoutPair.DatabaseTLS.KeyPath = "", ""
	tlsConfig, err := withoutPair.validate()
	require.NoError(t, err)
	_, err = mysql.NewMySQLDBConnector(config.DatabaseDSN, &tlsConfig).WithMaxRetries(1).Connect()
	require.Error(t, err, "the real server must reject the missing client certificate")
	wrongCA := config
	wrongCA.DatabaseTLS.RootCertPath, wrongCA.DatabaseTLS.CAPath = "", ""
	tlsConfig, err = wrongCA.validate()
	require.NoError(t, err)
	require.True(t, tlsConfig.VerifyServerCert)
	_, err = mysql.NewMySQLDBConnector(config.DatabaseDSN, &tlsConfig).WithMaxRetries(1).Connect()
	require.Error(t, err, "a client pair cannot disable server certificate verification")
}
