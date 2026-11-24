package client_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/stretchr/testify/require"
)

func TestClientMissingAlias318(t *testing.T) {
	for _, tc := range []struct {
		name, code, message string
		status              int
		missing             bool
	}{
		{"missing alias", "INVALID_PARAMETER_VALUE", "Registered model alias rhoai-probe not found.", 400, true},
		{"different alias", "INVALID_PARAMETER_VALUE", "Registered model alias other not found.", 400, false},
		{"invalid name", "INVALID_PARAMETER_VALUE", "Invalid alias name: dummy-secret", 400, false},
		{"wrong code", "RESOURCE_ALREADY_EXISTS", "Registered model alias rhoai-probe not found.", 400, false},
		{"forbidden", "INVALID_PARAMETER_VALUE", "Registered model alias rhoai-probe not found.", 403, false},
		{"unavailable", "INVALID_PARAMETER_VALUE", "Registered model alias rhoai-probe not found.", 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]string{"error_code": tc.code, "message": tc.message})
			require.NoError(t, err)
			server, _ := newRecordingServer(t, func(recordedRequest) (int, string) { return tc.status, string(payload) })
			native, err := client.New(server.URL, server.Client())
			require.NoError(t, err)
			_, err = native.GetRegisteredModelAlias(t.Context(), "model", "rhoai-probe")
			typed, ok := client.AsError(err)
			require.True(t, ok)
			require.Equal(t, tc.missing, typed.IsNotFound())
			require.Equal(t, tc.status, typed.Status, "retain the actual upstream HTTP status")
			require.NotContains(t, err.Error(), "dummy-secret")
			if tc.missing {
				require.False(t, typed.IsInvalidInput())
				require.ErrorIs(t, mlflow.MapClientError(err), api.ErrNotFound)
			}
			// The same upstream payload on another endpoint remains its
			// original failure category; this is not blanket 400 handling.
			_, err = native.GetModelVersion(t.Context(), "model", "1")
			typed, ok = client.AsError(err)
			require.True(t, ok)
			require.False(t, typed.IsNotFound())
		})
	}
}

func TestNativeContractMissingAlias318(t *testing.T) {
	fixture := newNativeFixture(t)
	require.False(t, createNativeModel(t, fixture, "missing-alias").isError())
	native, err := client.New(fixture.base, nativeHTTPClient)
	require.NoError(t, err)
	_, err = native.GetRegisteredModelAlias(t.Context(), fixture.model("missing-alias"), "rhoai-probe")
	typed, ok := client.AsError(err)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, typed.Status)
	require.Equal(t, "INVALID_PARAMETER_VALUE", typed.Code)
	require.True(t, typed.IsNotFound(), "3.18.0 represents an absent alias as a structured HTTP 400")
	_, err = native.GetRegisteredModelAlias(t.Context(), fixture.model("missing-alias"), "INVALID Alias!")
	typed, ok = client.AsError(err)
	require.True(t, ok)
	require.False(t, typed.IsNotFound(), "an invalid alias never authorizes an alias write")
}
