package mlflow

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
)

// The service implements the full registry API contract; this assertion
// keeps any future interface change a compile error here.
var _ api.ModelRegistryApi = (*Service)(nil)

// Service is the MLflow-backed api.ModelRegistryApi implementation. It
// owns a cancelable lifetime context and the worker goroutines started
// on it; it borrows — and never closes — the SQL store and the native
// client, whose lifetimes belong to the startup factory. Supported
// registry methods are added by the model, version, and artifact files;
// unsupported families live in unsupported.go.
type Service struct {
	config Config
	store  *Store
	native *client.Client

	// workerID identifies this service instance to the journal's lease
	// arbitration. It is deliberately not the shared adapter identity:
	// replicas of one adapter must contend for leases as distinct
	// workers, or a replica could steal an in-flight claim from its
	// twin.
	workerID string

	ctx        context.Context
	cancel     context.CancelFunc
	closeOnce  sync.Once
	runStarted atomic.Bool
	runAlive   atomic.Bool
	runMu      sync.Mutex
	runReady   chan struct{}
	runDone    chan struct{}

	// refreshMu coalesces concurrent refreshes: later callers wait for
	// the in-flight one and share its result instead of racing a second
	// full-registry observation.
	refreshMu      sync.Mutex
	refresh        *refreshFlight
	refreshWorkers sync.WaitGroup

	// inflight tracks operations currently being applied by this
	// replica. Inline request paths and the Run sweep share one worker
	// identity, so without this guard the sweep could re-claim an
	// operation an inline application is driving, invalidating its
	// fencing token mid-flight.
	inflight sync.Map
}

// beginApplication registers one operation as being applied by this
// replica and reports whether another goroutine is already driving it.
func (s *Service) beginApplication(operationID string) (busy bool) {
	_, busy = s.inflight.LoadOrStore(operationID, struct{}{})
	return busy
}

// endApplication releases one operation's in-flight registration.
func (s *Service) endApplication(operationID string) {
	s.inflight.Delete(operationID)
}

// applying reports whether this replica is currently applying one
// operation.
func (s *Service) applying(operationID string) bool {
	_, busy := s.inflight.Load(operationID)
	return busy
}

// NewService validates the configuration and creates the service. It
// starts no workers and performs no native or SQL work; the startup
// sequence ( migration, identity initialization, initial refresh, and
// the Run loop ) is explicitly driven by the runtime.
func NewService(config Config, store *Store, native *client.Client) (*Service, error) {
	normalized, err := config.validate()
	if err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errServiceInit("a compatibility store is required")
	}
	if native == nil {
		return nil, errServiceInit("a native registry client is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		config:   normalized,
		store:    store,
		native:   native,
		workerID: uuid.NewString(),
		ctx:      ctx,
		cancel:   cancel,
		runDone:  make(chan struct{}),
		runReady: make(chan struct{}),
	}, nil
}

// Config returns the normalized service configuration.
func (s *Service) Config() Config {
	return s.config
}

// Close cancels the service lifetime and joins owned workers. It is
// idempotent and never closes the borrowed SQL connection or the
// configured HTTP transport; the startup factory's cleanup owns those.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.runMu.Lock()
		s.refreshMu.Lock()
		s.cancel()
		s.refreshMu.Unlock()
		started := s.runStarted.Load()
		s.runMu.Unlock()
		s.refreshWorkers.Wait()
		if started {
			// Run observes the canceled lifetime and returns; join it
			// so Close never leaves journal work executing.
			<-s.runDone
		}
	})
	return nil
}

// Running is closed when Run has entered its lifetime, before startup publication.
func (s *Service) Running() <-chan struct{} { return s.runReady }
