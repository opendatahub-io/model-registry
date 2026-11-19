package errors

import (
	"errors"
	"net/http"
)

var (
	ErrBadRequest     = errors.New("bad request")
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("conflict")
	ErrNotImplemented = errors.New("not implemented")
	ErrUnavailable    = errors.New("unavailable")
	ErrUnauthorized   = errors.New("unauthorized")
	ErrForbidden      = errors.New("forbidden")
)

func ErrToStatus(err error) int {
	if errors.Is(err, ErrBadRequest) {
		return http.StatusBadRequest
	}
	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound
	}

	if errors.Is(err, ErrConflict) {
		return http.StatusConflict
	}

	if errors.Is(err, ErrNotImplemented) {
		return http.StatusNotImplemented
	}

	if errors.Is(err, ErrUnavailable) {
		return http.StatusServiceUnavailable
	}

	if errors.Is(err, ErrUnauthorized) {
		return http.StatusUnauthorized
	}

	if errors.Is(err, ErrForbidden) {
		return http.StatusForbidden
	}

	// Default error to return
	return http.StatusInternalServerError
}

func IgnoreNotFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}

	return err
}
