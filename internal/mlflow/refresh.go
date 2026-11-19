package mlflow

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"gorm.io/gorm"
)

// ObservationBaseline is a consistent SQL view captured before any native page.
// Revisions, rather than public millisecond timestamps, fence publication.
type ObservationBaseline struct {
	ObjectRevisions  map[int32]int64
	BindingRevisions map[int32]int64
}

type refreshFlight struct {
	done chan struct{}
	err  error // immutable after done closes
}

// Refresh coalesces callers onto one bounded observation. Every waiter retains
// that flight's result, even if another flight completes before it resumes.
func (s *Service) Refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	if err := s.ctx.Err(); err != nil {
		s.refreshMu.Unlock()
		return err
	}
	if err := ctx.Err(); err != nil {
		s.refreshMu.Unlock()
		return err
	}
	if flight := s.refresh; flight != nil {
		s.refreshMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-flight.done:
			return flight.err
		}
	}
	flight := &refreshFlight{done: make(chan struct{})}
	s.refresh = flight
	// Close cancels under the same mutex before waiting: no Add can race Wait.
	s.refreshWorkers.Add(1)
	s.refreshMu.Unlock()
	defer s.refreshWorkers.Done()

	observationCtx, cancel := context.WithTimeout(ctx, s.config.RequestTimeout)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	err := s.refreshLocked(observationCtx)
	s.refreshMu.Lock()
	flight.err = err
	close(flight.done)
	s.refresh = nil
	s.refreshMu.Unlock()
	return err
}

func (s *Service) refreshLocked(ctx context.Context) error {
	adapter, err := s.store.GetAdapter(ctx)
	if err != nil {
		return err
	}
	fingerprint, err := RegistryTargetFingerprint(s.config.RegistryURI)
	if err != nil {
		return err
	}
	if fingerprint != adapter.TargetFingerprint {
		return fmt.Errorf("service target does not match the compatibility store's bound registry: %w", api.ErrConflict)
	}
	baseline, err := s.store.CaptureObservationBaseline(ctx)
	if err != nil {
		return err
	}
	index, err := s.fetchNativeIndex(ctx)
	if err != nil {
		return err
	}
	return s.store.PublishNativeObservation(ctx, baseline, index)
}

func (s *Store) CaptureObservationBaseline(ctx context.Context) (ObservationBaseline, error) {
	baseline := ObservationBaseline{ObjectRevisions: map[int32]int64{}, BindingRevisions: map[int32]int64{}}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var objects []Object
		if err := tx.Select("id", "revision").Find(&objects).Error; err != nil {
			return mapDBError(err)
		}
		for _, row := range objects {
			baseline.ObjectRevisions[row.ID] = row.Revision
		}
		var bindings []VersionBinding
		if err := tx.Select("version_id", "revision").Find(&bindings).Error; err != nil {
			return mapDBError(err)
		}
		for _, row := range bindings {
			baseline.BindingRevisions[row.VersionID] = row.Revision
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return baseline, err
}

func (s *Store) PublishNativeObservation(ctx context.Context, baseline ObservationBaseline, index *nativeIndex) error {
	// Legacy repair rereads guards after ordered resource locks. READ COMMITTED
	// is scoped to observation publication; native HTTP is already complete.
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return importNativeIndex(ctx, NewStore(tx), index, baseline)
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}

func matchesNativeBinding(binding VersionBinding, native client.ModelVersion) bool {
	return binding.NativeModelName != nil && binding.NativeVersionNumber != nil && *binding.NativeModelName == native.Name && *binding.NativeVersionNumber == native.Version
}

// Before execution plans were persisted, single-create work still recorded
// its immutable native tuple in the candidate binding/history. Observation can
// verify that tuple without hydrating metadata or touching the native registry.
func nativeObservationPlan(operation Operation) (*NativeVersionPlan, bool, error) {
	candidate, err := decodeMutationCandidate(operation.CandidateJSON)
	if err != nil {
		return nil, false, err
	}
	if candidate != nil && candidate.NativeWork != nil {
		return candidate.NativeWork, false, nil
	}
	plan, err := legacyNativePlan(operation)
	if err != nil || plan != nil {
		return plan, true, err
	}
	if operation.Kind != OperationKindVersionMaterialize && operation.Kind != OperationKindVersionReplace && operation.Kind != OperationKindVersionCreateRetry {
		return nil, false, nil
	}
	if candidate == nil || len(candidate.Bindings) != 1 || len(candidate.History) != 1 {
		return nil, true, fmt.Errorf("legacy native operation has no recorded identity: %w", api.ErrConflict)
	}
	binding, history := candidate.Bindings[0], candidate.History[0]
	if binding.ResultOperationUUID != operation.OperationUUID || history.ResultOperationUUID != operation.OperationUUID || binding.Value.VersionID != history.Value.VersionID || binding.Value.NativeModelName == nil || *binding.Value.NativeModelName != history.Value.NativeModelName || binding.Value.ActiveGeneration != history.Value.Generation || history.Value.Generation != operation.Generation {
		return nil, true, fmt.Errorf("legacy native operation binding/history disagree: %w", api.ErrConflict)
	}
	return &NativeVersionPlan{LogicalVersionID: history.Value.VersionID, ModelName: history.Value.NativeModelName, Source: history.Value.Source, Generation: history.Value.Generation}, true, nil
}
