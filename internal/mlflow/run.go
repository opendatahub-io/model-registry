package mlflow

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/kubeflow/hub/pkg/api"
)

// Run loop tuning: the poll pause between journal sweeps and the cap
// on automatic-retry backoff.
const (
	runPollInterval = 200 * time.Millisecond
	maxRetryBackoff = time.Minute
	retryBaseDelay  = 100 * time.Millisecond
)

// Definitive failure codes recorded on operations whose automatic
// retries must stop until the operator corrects the cause.
const (
	failureCodeInput = "DEFINITIVE_INPUT"
	failureCodeAuth  = "DEFINITIVE_AUTH"
)

// Run resumes journal work — pending intents and interrupted
// operations — until the caller's context or the service lifetime is
// canceled. Every application goes through the same ApplyOperation
// executor used for inline writes, so native work always runs outside
// SQL transactions under the journal's lease and fencing. Failures
// schedule a capped, jittered retry; definitive input and
// authentication failures stop automatic retries with a recorded
// failure code. Run is safe to call once per service; Close cancels
// and joins it.
func (s *Service) Run(ctx context.Context) error {
	s.runMu.Lock()
	if s.ctx.Err() != nil {
		s.runMu.Unlock()
		return s.ctx.Err()
	}
	if !s.runStarted.CompareAndSwap(false, true) {
		s.runMu.Unlock()
		return errServiceInit("the run loop is already running")
	}
	s.runAlive.Store(true)
	close(s.runReady)
	s.runMu.Unlock()
	defer close(s.runDone)
	defer s.runAlive.Store(false)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Close cancels the service lifetime, which cancels this loop.
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()

	// The first refresh publishes the current native observation before
	// the loop resumes retained work, then the loop refreshes
	// periodically from the service's own lifetime.
	lastRefresh := time.Time{}
	refreshDue := func() bool {
		return time.Since(lastRefresh) >= s.config.RefreshInterval
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if refreshDue() {
			// A failed refresh is not fatal: the loop retries on the
			// next interval and the prior observation stays committed.
			_ = s.Refresh(ctx)
			_ = s.ReconcileAliases(ctx)
			lastRefresh = time.Now()
			if ctx.Err() != nil {
				return nil
			}
		}
		worked := s.sweep(ctx)
		if ctx.Err() != nil {
			return nil
		}
		pause := runPollInterval
		if worked {
			pause = time.Millisecond // more journal work may be ready
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pause):
		}
	}
}

// sweep applies every due operation once and reports whether any
// application was attempted.
func (s *Service) sweep(ctx context.Context) bool {
	operations, err := s.store.ListOperations(ctx, OperationPending, OperationRunning)
	if err != nil {
		return false
	}
	now := nowMillis()
	worked := false
	for _, operation := range operations {
		if ctx.Err() != nil {
			return worked
		}
		if operation.FailureCode != nil {
			_, selected := explicitNativeSelection(operation)
			if *operation.FailureCode != failureCodeNativeSelected || !selected {
				continue // definitive: automatic retries stopped, even after a selection
			}
		}
		if s.applying(operation.OperationUUID) {
			// An inline request path of this replica is driving the
			// operation; claiming it now would invalidate that
			// application's fencing token.
			continue
		}
		if operation.NextRetrySinceEpoch != nil && *operation.NextRetrySinceEpoch > now {
			continue
		}
		worked = true
		cause := s.ApplyOperation(ctx, operation.OperationUUID)
		if cause == nil {
			continue
		}
		if ctx.Err() != nil {
			return worked
		}
		s.scheduleRetry(ctx, operation.OperationUUID, cause)
	}
	return worked
}

// scheduleRetry records the next retry of a failed application. The
// delay grows exponentially with the operation's attempt count, capped,
// with jitter so replicas do not retry in lockstep. Definitive input
// and authentication failures record a failure code and stop
// automatic retries until the operator corrects the cause.
func (s *Service) scheduleRetry(ctx context.Context, operationID string, cause error) {
	operation, err := s.store.GetOperation(ctx, operationID)
	if err != nil || operation.State != OperationRunning {
		return
	}
	var failureCode *string
	switch {
	case errors.Is(cause, api.ErrBadRequest), errors.Is(cause, api.ErrNotImplemented):
		failureCode = new(failureCodeInput)
	case errors.Is(cause, api.ErrUnauthorized), errors.Is(cause, api.ErrForbidden):
		failureCode = new(failureCodeAuth)
	}
	next := nowMillis() + retryDelay(operation.Attempts).Milliseconds()
	_ = s.store.RecordOperationRetry(ctx, operationID,
		operation.FencingToken, next, failureCode)
}

// retryDelay computes the capped, jittered backoff for one attempt
// count. Jitter is ±50% of the base delay so concurrent replicas
// decorrelate.
func retryDelay(attempts int) time.Duration {
	backoff := retryBaseDelay
	for range max(attempts-1, 0) {
		backoff *= 2
		if backoff >= maxRetryBackoff {
			backoff = maxRetryBackoff
			break
		}
	}
	jitter := time.Duration(rand.Int64N(int64(backoff)+1)) - backoff/2
	return min(max(backoff+jitter, time.Millisecond), maxRetryBackoff)
}
