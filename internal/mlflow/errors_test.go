package mlflow_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMapClientError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"unauthorized", &client.Error{Status: http.StatusUnauthorized, Code: "PERMISSION_DENIED"}, http.StatusUnauthorized},
		{"forbidden", &client.Error{Status: http.StatusForbidden, Code: "PERMISSION_DENIED"}, http.StatusForbidden},
		{"not found", &client.Error{Status: http.StatusNotFound, Code: "RESOURCE_DOES_NOT_EXIST"}, http.StatusNotFound},
		{"invalid input", &client.Error{Status: http.StatusBadRequest, Code: "INVALID_PARAMETER_VALUE"}, http.StatusBadRequest},
		{"duplicate identity", &client.Error{Status: http.StatusBadRequest, Code: "RESOURCE_ALREADY_EXISTS"}, http.StatusConflict},
		{"server error", &client.Error{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR"}, http.StatusServiceUnavailable},
		{"ambiguous", &client.Error{Status: http.StatusServiceUnavailable, Ambiguous: true}, http.StatusServiceUnavailable},
		{"other", fmt.Errorf("plain failure"), http.StatusInternalServerError},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mapped := mlflow.MapClientError(testCase.err)
			require.Error(t, mapped)
			assert.Equal(t, testCase.status, api.ErrToStatus(mapped))
		})
	}
}
