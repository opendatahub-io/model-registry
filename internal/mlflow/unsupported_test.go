package mlflow_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kubeflow/hub/internal/mlflow/client"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnsupportedFamilies proves every unsupported method family
// returns a stable 501 with a backend/resource message and no side
// effects. Unsupported list methods error rather than returning empty
// success.
func TestUnsupportedFamilies(t *testing.T) {
	service := newLifetimeTestService(t)

	calls := map[string]func() error{
		"GetRegisteredModelByInferenceService": func() error {
			_, err := service.GetRegisteredModelByInferenceService("1")
			return err
		},
		"GetModelVersionByInferenceService": func() error {
			_, err := service.GetModelVersionByInferenceService("1")
			return err
		},
		"GetModelArtifactByInferenceService": func() error {
			_, err := service.GetModelArtifactByInferenceService("1")
			return err
		},
		"UpsertServingEnvironment": func() error {
			_, err := service.UpsertServingEnvironment(nil)
			return err
		},
		"GetServingEnvironmentById": func() error {
			_, err := service.GetServingEnvironmentById("1")
			return err
		},
		"GetServingEnvironmentByParams": func() error {
			_, err := service.GetServingEnvironmentByParams(nil, nil)
			return err
		},
		"GetServingEnvironments": func() error {
			_, err := service.GetServingEnvironments(api.ListOptions{})
			return err
		},
		"UpsertInferenceService": func() error {
			_, err := service.UpsertInferenceService(nil)
			return err
		},
		"GetInferenceServiceById": func() error {
			_, err := service.GetInferenceServiceById("1")
			return err
		},
		"GetInferenceServiceByParams": func() error {
			_, err := service.GetInferenceServiceByParams(nil, nil, nil)
			return err
		},
		"GetInferenceServices": func() error {
			_, err := service.GetInferenceServices(api.ListOptions{}, nil, nil)
			return err
		},
		"UpsertServeModel": func() error {
			_, err := service.UpsertServeModel(nil, nil)
			return err
		},
		"GetServeModelById": func() error {
			_, err := service.GetServeModelById("1")
			return err
		},
		"GetServeModels": func() error {
			_, err := service.GetServeModels(api.ListOptions{}, nil)
			return err
		},
		"UpsertExperiment": func() error {
			_, err := service.UpsertExperiment(nil)
			return err
		},
		"GetExperimentById": func() error {
			_, err := service.GetExperimentById("1")
			return err
		},
		"GetExperimentByParams": func() error {
			_, err := service.GetExperimentByParams(nil, nil)
			return err
		},
		"GetExperiments": func() error {
			_, err := service.GetExperiments(api.ListOptions{})
			return err
		},
		"UpsertExperimentRun": func() error {
			_, err := service.UpsertExperimentRun(nil, nil)
			return err
		},
		"GetExperimentRunById": func() error {
			_, err := service.GetExperimentRunById("1")
			return err
		},
		"GetExperimentRunByParams": func() error {
			_, err := service.GetExperimentRunByParams(nil, nil, nil)
			return err
		},
		"GetExperimentRuns": func() error {
			_, err := service.GetExperimentRuns(api.ListOptions{}, nil)
			return err
		},
		"UpsertExperimentRunArtifact": func() error {
			_, err := service.UpsertExperimentRunArtifact(nil, "1")
			return err
		},
		"GetExperimentRunArtifacts": func() error {
			_, err := service.GetExperimentRunArtifacts("", api.ListOptions{}, nil)
			return err
		},
		"GetExperimentRunMetricHistory": func() error {
			_, err := service.GetExperimentRunMetricHistory(nil, nil, api.ListOptions{}, nil)
			return err
		},
	}

	require.NotEmpty(t, calls)
	for name, call := range calls {
		err := call()
		require.ErrorIs(t, err, api.ErrNotImplemented, "%s must be 501", name)
		assert.Equal(t, http.StatusNotImplemented, api.ErrToStatus(err), "%s", name)
		assert.Contains(t, err.Error(), "mlflow backend does not support", "%s message", name)
	}
}

// newLifetimeTestService builds a Service over an httptest native
// server that fails any request, so tests prove unsupported methods
// issue no native calls and no database activity is needed.
func newLifetimeTestService(t *testing.T) *mlflow.Service {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unsupported methods must not issue native requests")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	native, err := client.New(server.URL, server.Client())
	require.NoError(t, err)
	service, err := mlflow.NewService(mlflow.Config{
		RegistryURI: server.URL,
	}, &mlflow.Store{}, native)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	return service
}
