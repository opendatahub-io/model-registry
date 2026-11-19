package mlflow

import (
	"fmt"

	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
)

// MapClientError translates a native registry failure into the shared
// status errors used by the REST layer. Authentication and authorization
// failures map to 401/403 without exposing upstream bodies; unavailable
// dependencies map to 503; definitive invalid input maps to 400. Native
// 404s are preserved as not-found. Ambiguous mutation outcomes surface as
// 503 so callers never treat them as definitive failures.
func MapClientError(err error) error {
	if err == nil {
		return nil
	}

	if typed, ok := client.AsError(err); ok {
		switch {
		case typed.Ambiguous:
			return fmt.Errorf("%v: %w", typed, api.ErrUnavailable)
		case typed.IsUnauthorized():
			return fmt.Errorf("mlflow registry authentication failed: %w", api.ErrUnauthorized)
		case typed.IsForbidden():
			return fmt.Errorf("mlflow registry authorization failed: %w", api.ErrForbidden)
		case typed.IsDuplicate():
			// A definitive upstream duplicate: the object already exists,
			// surfaced as a conflict without echoing the upstream body.
			return fmt.Errorf("mlflow registry already contains the requested object: %w", api.ErrConflict)
		case typed.IsNotFound():
			return fmt.Errorf("%v: %w", typed, api.ErrNotFound)
		case typed.IsInvalidInput():
			return fmt.Errorf("%v: %w", typed, api.ErrBadRequest)
		default:
			return fmt.Errorf("%v: %w", typed, api.ErrUnavailable)
		}
	}
	return err
}
