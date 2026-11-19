package mlflow_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServiceConstructorValidation proves invalid configurations are
// rejected and defaults applied.
func TestServiceConstructorValidation(t *testing.T) {
	server := httptest.NewServer(http.NewServeMux())
	t.Cleanup(server.Close)
	native, err := client.New(server.URL, server.Client())
	require.NoError(t, err)

	for name, config := range map[string]mlflow.Config{
		"missing URI":  {RegistryURI: ""},
		"invalid URI":  {RegistryURI: "not a url"},
		"wrong scheme": {RegistryURI: "ftp://example.com"},
		"missing host": {RegistryURI: "http://"},
		"neg timeout":  {RegistryURI: server.URL, RequestTimeout: -time.Second},
		"neg refresh":  {RegistryURI: server.URL, RefreshInterval: -time.Second},
	} {
		_, err := mlflow.NewService(config, &mlflow.Store{}, native)
		require.ErrorIs(t, err, api.ErrBadRequest, "%s must be rejected", name)
	}

	for name, missing := range map[string]func(config mlflow.Config) (*mlflow.Service, error){
		"nil store": func(config mlflow.Config) (*mlflow.Service, error) {
			return mlflow.NewService(config, nil, native)
		},
		"nil client": func(config mlflow.Config) (*mlflow.Service, error) {
			return mlflow.NewService(config, &mlflow.Store{}, nil)
		},
	} {
		_, err := missing(mlflow.Config{RegistryURI: server.URL})
		require.ErrorIs(t, err, api.ErrBadRequest, "%s must be rejected", name)
	}

	// Zero timeouts select the documented 30-second defaults.
	service, err := mlflow.NewService(mlflow.Config{RegistryURI: server.URL}, &mlflow.Store{}, native)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	assert.Equal(t, 30*time.Second, service.Config().RequestTimeout)
	assert.Equal(t, 30*time.Second, service.Config().RefreshInterval)

	explicit, err := mlflow.NewService(mlflow.Config{
		RegistryURI:     server.URL,
		RequestTimeout:  5 * time.Second,
		RefreshInterval: time.Minute,
	}, &mlflow.Store{}, native)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, explicit.Close()) })
	assert.Equal(t, 5*time.Second, explicit.Config().RequestTimeout)
	assert.Equal(t, time.Minute, explicit.Config().RefreshInterval)
}

// TestServiceCloseIsIdempotentAndBorrowsResources proves Close cancels
// and joins owned work, can be called repeatedly, and never closes the
// borrowed SQL connection or native transport.
func TestServiceCloseIsIdempotentAndBorrowsResources(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			fixture.cleanup(t)

			server := httptest.NewServer(http.NewServeMux())
			t.Cleanup(server.Close)
			native, err := client.New(server.URL, server.Client())
			require.NoError(t, err)

			service, err := mlflow.NewService(mlflow.Config{RegistryURI: server.URL},
				mlflow.NewStore(fixture.db), native)
			require.NoError(t, err)

			require.NoError(t, service.Close())
			require.NoError(t, service.Close(), "Close must be idempotent")

			// The borrowed SQL connection still works after Close.
			created, err := mlflow.NewStore(fixture.db).ReserveObject(t.Context(),
				reservedModel("close-borrowed-model"))
			require.NoError(t, err)
			assert.NotZero(t, created.ID)

			// The borrowed native transport still works after Close: a
			// read reaches the server ( a typed 404 ) instead of failing
			// on a closed connection.
			_, readErr := native.GetRegisteredModel(t.Context(), "close-borrowed-model")
			require.Error(t, readErr)
			_, typed := client.AsError(readErr)
			require.True(t, typed, "the transport must still reach the server: %v", readErr)
		})
	}
}
