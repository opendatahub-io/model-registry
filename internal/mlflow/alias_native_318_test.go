package mlflow_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/stretchr/testify/require"
)

func TestAliasMissingNative318Registration(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			backend.service = aliasTestService(t, backend, aliasTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/registered-models/alias") &&
					backend.stub.aliases(r.URL.Query().Get("name"))[r.URL.Query().Get("alias")] == "" {
					body, err := json.Marshal(map[string]string{"error_code": "INVALID_PARAMETER_VALUE", "message": "Registered model alias " + r.URL.Query().Get("alias") + " not found."})
					require.NoError(t, err)
					return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			}))
			model := mustModel(t, backend, "native-318-missing-alias")
			version := mustVersion(t, backend, model, "one")
			artifact, err := attachWeights(t, backend.service, version, model.Name, "s3://bucket/model.onnx")
			require.NoError(t, err, "a native missing-alias response must permit initial materialization")
			require.NotNil(t, artifact.ModelArtifact.Id)
			binding := bindingOf(t, backend, *version.Id)
			require.Equal(t, mlflow.PhaseReady, binding.Phase)
			require.Equal(t, "1", *binding.NativeVersionNumber)
			require.Equal(t, "1", backend.stub.aliases(model.Name)[binding.Alias])
			require.Equal(t, 1, backend.stub.countRequests("POST registered-models/alias"))
		})
	}
}
