package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// TransportConfig describes how the client authenticates to the MLflow
// registry and which CA bundle to trust. Exactly one authentication mode
// is allowed: a bearer-token file, or basic auth with a username and a
// password file. Credentials are read from files, never from flags, URLs,
// or logs.
type TransportConfig struct {
	// BearerTokenFile holds a bearer token for Authorization headers.
	BearerTokenFile string
	// Username and PasswordFile configure basic authentication.
	Username     string
	PasswordFile string
	// CAFile is an optional PEM bundle used to verify the registry's
	// TLS certificate; when empty the system roots are used.
	CAFile string
}

// HTTPClient builds the configured HTTP client. Certificate verification
// is always on.
func (c TransportConfig) HTTPClient() (*http.Client, error) {
	if c.BearerTokenFile != "" && (c.Username != "" || c.PasswordFile != "") {
		return nil, fmt.Errorf("mlflow client authentication must be either a bearer token or basic auth, not both")
	}
	if (c.Username == "") != (c.PasswordFile == "") {
		return nil, fmt.Errorf("mlflow client basic auth requires both username and password file")
	}

	var transport http.RoundTripper = &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}

	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading mlflow CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("mlflow CA file %s contains no certificates", c.CAFile)
		}
		base, ok := transport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("unexpected transport type for CA configuration")
		}
		base.TLSClientConfig.RootCAs = pool
	}

	switch {
	case c.BearerTokenFile != "":
		token, err := os.ReadFile(c.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading mlflow bearer token file: %w", err)
		}
		transport = &authTransport{base: transport, authorization: "Bearer " + string(trimSpaceBytes(token))}
	case c.Username != "":
		password, err := os.ReadFile(c.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("reading mlflow password file: %w", err)
		}
		transport = &authTransport{base: transport, username: c.Username, password: string(trimSpaceBytes(password))}
	}

	return &http.Client{Transport: transport, Timeout: defaultTimeout}, nil
}

// authTransport injects configured credentials into every request.
// Incoming Kubeflow request tokens are never forwarded.
type authTransport struct {
	base          http.RoundTripper
	authorization string
	username      string
	password      string
}

// RoundTrip implements http.RoundTripper.
func (a *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone to avoid mutating a request the caller may reuse.
	clone := req.Clone(req.Context())
	clone.Header.Del("Authorization")
	if a.authorization != "" {
		clone.Header.Set("Authorization", a.authorization)
	}
	if a.username != "" {
		clone.SetBasicAuth(a.username, a.password)
	}

	base := a.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

func trimSpaceBytes(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpaceByte(b[start]) {
		start++
	}
	for end > start && isSpaceByte(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// CloseIdleConnections forwards cleanup through the authentication wrapper.
func (a *authTransport) CloseIdleConnections() {
	if closer, ok := a.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
