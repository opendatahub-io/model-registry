package errors_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	platformerrors "github.com/kubeflow/hub/internal/platform/errors"
	"github.com/stretchr/testify/assert"
)

func TestErrToStatus(t *testing.T) {
	cases := []struct {
		err    error
		status int
	}{
		{platformerrors.ErrBadRequest, http.StatusBadRequest},
		{platformerrors.ErrNotFound, http.StatusNotFound},
		{platformerrors.ErrConflict, http.StatusConflict},
		{platformerrors.ErrNotImplemented, http.StatusNotImplemented},
		{platformerrors.ErrUnavailable, http.StatusServiceUnavailable},
		{platformerrors.ErrUnauthorized, http.StatusUnauthorized},
		{platformerrors.ErrForbidden, http.StatusForbidden},
		{errors.New("something else"), http.StatusInternalServerError},
		{nil, http.StatusInternalServerError},
	}
	for _, testCase := range cases {
		t.Run(fmt.Sprint(testCase.status), func(t *testing.T) {
			assert.Equal(t, testCase.status, platformerrors.ErrToStatus(testCase.err))
		})
	}
}

func TestWrappedErrors(t *testing.T) {
	wrapped := fmt.Errorf("outer context: %w", platformerrors.ErrUnavailable)
	assert.ErrorIs(t, wrapped, platformerrors.ErrUnavailable)
	assert.Equal(t, http.StatusServiceUnavailable, platformerrors.ErrToStatus(wrapped))

	doubleWrapped := fmt.Errorf("more context: %w", wrapped)
	assert.ErrorIs(t, doubleWrapped, platformerrors.ErrUnavailable)
	assert.Equal(t, http.StatusServiceUnavailable, platformerrors.ErrToStatus(doubleWrapped))
}

func TestIgnoreNotFound(t *testing.T) {
	assert.NoError(t, platformerrors.IgnoreNotFound(platformerrors.ErrNotFound))
	assert.NoError(t, platformerrors.IgnoreNotFound(fmt.Errorf("wrapped: %w", platformerrors.ErrNotFound)))

	still := fmt.Errorf("wrapped conflict: %w", platformerrors.ErrConflict)
	assert.Equal(t, still, platformerrors.IgnoreNotFound(still))
}
