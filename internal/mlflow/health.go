package mlflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
)

// healthCheckTimeout bounds the whole health probe, including the
// native registry read, so a hung dependency fails the probe rather
// than blocking the readiness endpoint.
const healthCheckTimeout = 10 * time.Second

// CheckHealth is the optional backend health capability dispatched by
// the proxy's readiness endpoints before the generic serving and
// experiment probes ( which this backend does not support ). It proves
// three dependencies in order: the SQL compatibility store answers a
// ping, the adapter identity is initialized with a completed initial
// refresh and a running reconciliation loop, and the native registry
// answers one bounded read. A healthy backend returns nil; every
// failure describes the broken dependency without leaking credentials
// or response bodies.
func (s *Service) CheckHealth(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	sqlDB, err := s.store.DB().DB()
	if err != nil {
		return fmt.Errorf("mlflow compatibility store is not connected: %w: %w", err, api.ErrUnavailable)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return fmt.Errorf("mlflow compatibility store is unreachable: %w: %w", err, api.ErrUnavailable)
	}

	// The adapter identity and its completed initial refresh prove the
	// startup sequence finished; a missing row or a null refresh
	// timestamp means the backend is still initializing.
	adapter, err := s.store.GetAdapter(ctx)
	if err != nil {
		if errors.Is(err, api.ErrNotFound) {
			return fmt.Errorf("mlflow adapter identity is not initialized: %w", api.ErrUnavailable)
		}
		return fmt.Errorf("reading the mlflow adapter identity: %w: %w", err, api.ErrUnavailable)
	}
	if adapter.LastRefreshTime == nil {
		return fmt.Errorf("the initial mlflow refresh has not completed: %w", api.ErrUnavailable)
	}
	if !s.runAlive.Load() {
		return fmt.Errorf("the mlflow reconciliation loop is not running: %w", api.ErrUnavailable)
	}

	// One bounded native read proves the registry answers using only
	// supported registry APIs.
	if _, err := s.native.SearchRegisteredModels(ctx, client.SearchRequest{MaxResults: 1}); err != nil {
		return fmt.Errorf("the mlflow registry is unreachable: %w: %w", MapClientError(err), api.ErrUnavailable)
	}
	return nil
}
