package client

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Error is a typed failure from the MLflow registry REST API. Status is
// the HTTP status, Code the upstream MLflow error code when present.
//
// Ambiguous marks a mutation whose outcome is unknown — the request was
// sent but the response never arrived. Such errors must never be blindly
// retried when the underlying call is non-idempotent.
type Error struct {
	Status    int
	Code      string
	Message   string
	Ambiguous bool
	// Cause is the underlying transport error, when there was one; it
	// keeps errors.Is working through the typed error ( for example
	// context.Canceled ).
	Cause error
	// MLflow 3.18.0 reports an absent alias as a structured 400. Only
	// the alias-read endpoint's exact missing-alias response sets this;
	// the original response text is never retained.
	missingAlias bool
}

// Unwrap exposes the underlying transport error.
func (e *Error) Unwrap() error { return e.Cause }

// Error implements the error interface. Message content is controlled by
// this package: upstream response bodies are reduced to a validated error
// code, and prose from upstream is never forwarded.
func (e *Error) Error() string {
	if e.Message != "" {
		if e.Code != "" {
			return fmt.Sprintf("mlflow registry error (status %d, code %s): %s", e.Status, e.Code, e.Message)
		}
		return fmt.Sprintf("mlflow registry error (status %d): %s", e.Status, e.Message)
	}
	if e.Code != "" {
		return fmt.Sprintf("mlflow registry error (status %d, code %s)", e.Status, e.Code)
	}
	return fmt.Sprintf("mlflow registry error (status %d)", e.Status)
}

// validErrorCode reports whether a decoded upstream error code has the
// shape of an MLflow error code ( uppercase letters, digits, underscores ).
// Anything else — prose, HTML, credentials — is discarded.
func validErrorCode(code string) bool {
	if code == "" {
		return false
	}
	for _, r := range code {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}

// IsUnauthorized reports an authentication failure (401).
func (e *Error) IsUnauthorized() bool { return e.Status == http.StatusUnauthorized }

// IsForbidden reports an authorization failure (403).
func (e *Error) IsForbidden() bool { return e.Status == http.StatusForbidden }

// IsNotFound reports a native 404 or a verified missing-alias response.
func (e *Error) IsNotFound() bool { return e.Status == http.StatusNotFound || e.missingAlias }

// IsInvalidInput reports a definitive upstream rejection of the request
// payload; such failures must not be retried until the input changes.
func (e *Error) IsInvalidInput() bool {
	return !e.missingAlias && (e.Status == http.StatusBadRequest || e.Status == http.StatusUnprocessableEntity)
}

// duplicateCodes are upstream error codes that definitively report the
// requested object already exists.
var duplicateCodes = map[string]bool{
	"RESOURCE_ALREADY_EXISTS":         true,
	"REGISTERED_MODEL_ALREADY_EXISTS": true,
}

// IsDuplicate reports a definitive upstream duplicate-identity rejection.
func (e *Error) IsDuplicate() bool {
	return duplicateCodes[e.Code]
}

// AsError extracts a *Error from err's chain.
func AsError(err error) (*Error, bool) {
	var typed *Error
	if errors.As(err, &typed) {
		return typed, true
	}
	return nil, false
}

// sensitiveQueryKeys are removed from URLs embedded in error messages.
var sensitiveQueryKeys = []string{"token", "password", "secret", "key", "signature", "X-Amz-Signature", "X-Goog-Signature"}

// RedactURL strips sensitive query parameters from a URL string for
// inclusion in logs or errors. Unparseable strings are returned unchanged
// minus any credential-style userinfo.
func RedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if parsed.User != nil {
		parsed.User = nil
	}
	changed := false
	values := parsed.Query()
	for _, key := range sensitiveQueryKeys {
		for _, candidate := range []string{key, strings.ToLower(key), strings.ToUpper(key)} {
			if values.Has(candidate) {
				values.Set(candidate, "REDACTED")
				changed = true
			}
		}
	}
	if changed {
		parsed.RawQuery = values.Encode()
	}
	return parsed.String()
}
