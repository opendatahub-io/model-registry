package api

import (
	platformerrors "github.com/kubeflow/hub/internal/platform/errors"
)

// Re-export from platform/errors for backward compatibility
var (
	ErrBadRequest     = platformerrors.ErrBadRequest
	ErrNotFound       = platformerrors.ErrNotFound
	ErrConflict       = platformerrors.ErrConflict
	ErrNotImplemented = platformerrors.ErrNotImplemented
	ErrUnavailable    = platformerrors.ErrUnavailable
	ErrUnauthorized   = platformerrors.ErrUnauthorized
	ErrForbidden      = platformerrors.ErrForbidden
)

var ErrToStatus = platformerrors.ErrToStatus
var IgnoreNotFound = platformerrors.IgnoreNotFound
