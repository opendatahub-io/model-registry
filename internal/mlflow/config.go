package mlflow

import (
	"fmt"
	"time"

	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
)

// Defaults for service configuration.
const (
	DefaultRequestTimeout  = 30 * time.Second
	DefaultRefreshInterval = 30 * time.Second
)

// Config is the MLflow backend service configuration. It carries no
// credentials: database and authentication configuration belongs to the
// startup factory, which owns the SQL connection and the native
// client's transport.
type Config struct {
	// RegistryURI is the base URL of the native MLflow registry.
	RegistryURI string
	// RequestTimeout bounds each native request. Zero selects the
	// 30-second default.
	RequestTimeout time.Duration
	// RefreshInterval controls the periodic native refresh. Zero
	// selects the 30-second default.
	RefreshInterval time.Duration
}

// validate normalizes and checks the configuration.
func (c Config) validate() (Config, error) {
	if c.RegistryURI == "" {
		return Config{}, fmt.Errorf("mlflow registry URI is required: %w", api.ErrBadRequest)
	}
	_, err := client.ParseRegistryURL(c.RegistryURI)
	if err != nil {
		return Config{}, err
	}

	if c.RequestTimeout == 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.RequestTimeout < 0 {
		return Config{}, fmt.Errorf("mlflow request timeout must not be negative: %w", api.ErrBadRequest)
	}
	if c.RefreshInterval == 0 {
		c.RefreshInterval = DefaultRefreshInterval
	}
	if c.RefreshInterval < 0 {
		return Config{}, fmt.Errorf("mlflow refresh interval must not be negative: %w", api.ErrBadRequest)
	}
	return c, nil
}
