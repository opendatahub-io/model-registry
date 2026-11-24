package mlflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kubeflow/hub/pkg/api"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Store is the SQL compatibility store for the MLflow backend. All
// methods accept a context and return checked errors; conflicts and
// not-found conditions use the shared status errors so the REST layer
// maps them to 409/404 unchanged.
type Store struct {
	db *gorm.DB
}

// NewStore wraps an existing gorm connection. The database must already
// have the MLflow adapter migrations applied.
func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

// DB exposes the underlying connection for operator tooling.
func (s *Store) DB() *gorm.DB { return s.db }

// WithinTx runs fn inside a database transaction, handing it a Store
// bound to the transaction. An error return rolls back every change made
// through the transaction-scoped Store. No network calls to MLflow may
// happen inside fn.
func (s *Store) WithinTx(ctx context.Context, fn func(*Store) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(&Store{db: tx})
	})
}

func mapDBError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return api.ErrNotFound
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return api.ErrConflict
	}
	return err
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// GetObject returns the committed, non-deleted object with the given ID.
// Rows reserved for unpublished native creates are invisible to public
// reads; the journal's internal paths load them without this filter.
func (s *Store) GetObject(ctx context.Context, id int32) (Object, error) {
	var object Object
	err := s.db.WithContext(ctx).
		Where("id = ? AND deleted = ? AND visibility = ?", id, false, VisibilityCommitted).
		First(&object).Error
	if err != nil {
		return Object{}, fmt.Errorf("no mlflow object found for id %d: %w", id, mapDBError(err))
	}
	return object, nil
}

// FindObject returns the single committed, non-deleted object of the given
// kind matching every supplied predicate. A nil parentID does not filter
// by parent; parentNone (0) selects parentless objects. Zero matches and
// multiple matches are both errors.
func (s *Store) FindObject(ctx context.Context, kind ObjectKind, name *string, parentID *int32, externalID *string) (Object, error) {
	query := s.db.WithContext(ctx).
		Where("kind = ? AND deleted = ? AND visibility = ?", kind, false, VisibilityCommitted)
	if name != nil {
		query = query.Where("name = ?", *name)
	}
	if parentID != nil {
		query = query.Where("parent_id = ?", *parentID)
	}
	if externalID != nil {
		query = query.Where("external_id = ?", *externalID)
	}

	var objects []Object
	err := query.Find(&objects).Error
	if err != nil {
		return Object{}, mapDBError(err)
	}
	switch len(objects) {
	case 0:
		return Object{}, fmt.Errorf("no %s object found for name=%v parent=%v externalId=%v: %w",
			kind, deref(name), deref(parentID), deref(externalID), api.ErrNotFound)
	case 1:
		return objects[0], nil
	default:
		return Object{}, fmt.Errorf("multiple %s objects found for name=%v parent=%v externalId=%v: %w",
			kind, deref(name), deref(parentID), deref(externalID), api.ErrConflict)
	}
}

// FindArtifactInVersion resolves a version's name through its scope or retained
// links. A legacy shared artifact keeps its one ID and remains discoverable
// through every version link, regardless of its creation scope.
func (s *Store) FindArtifactInVersion(ctx context.Context, name string, versionID int32) (Object, error) {
	query := s.db.WithContext(ctx).Where("kind = ? AND name = ? AND deleted = ? AND visibility = ?", KindModelArtifact, name, false, VisibilityCommitted)
	var linked []Object
	err := query.Where("id IN (?)", s.db.Model(&ArtifactLink{}).Select("artifact_id").Where("version_id = ?", versionID)).Find(&linked).Error
	if err != nil || len(linked) != 0 {
		return singleArtifact(linked, err)
	}
	var scoped []Object
	err = s.db.WithContext(ctx).Where("kind = ? AND name = ? AND deleted = ? AND visibility = ? AND artifact_scope_id = ?", KindModelArtifact, name, false, VisibilityCommitted, versionID).Find(&scoped).Error
	return singleArtifact(scoped, err)
}

// FindStandaloneArtifact finds an artifact created in the standalone scope.
func (s *Store) FindStandaloneArtifact(ctx context.Context, name string) (Object, error) {
	var objects []Object
	err := s.db.WithContext(ctx).Where("kind = ? AND name = ? AND artifact_scope_id = 0 AND deleted = ? AND visibility = ?", KindModelArtifact, name, false, VisibilityCommitted).Find(&objects).Error
	return singleArtifact(objects, err)
}

func singleArtifact(objects []Object, err error) (Object, error) {
	if err != nil {
		return Object{}, mapDBError(err)
	}
	if len(objects) == 0 {
		return Object{}, api.ErrNotFound
	}
	if len(objects) != 1 {
		return Object{}, api.ErrConflict
	}
	return objects[0], nil
}

// ListObjects returns every committed, non-deleted object of the given
// kind, ordered by ID ascending. A nil parentID lists across all parents.
func (s *Store) ListObjects(ctx context.Context, kind ObjectKind, parentID *int32) ([]Object, error) {
	query := s.db.WithContext(ctx).
		Where("kind = ? AND deleted = ? AND visibility = ?", kind, false, VisibilityCommitted).
		Order("id ASC")
	if parentID != nil {
		query = query.Where("parent_id = ?", *parentID)
	}

	var objects []Object
	err := query.Find(&objects).Error
	if err != nil {
		return nil, mapDBError(err)
	}
	return objects, nil
}

// validateReservation checks kind, parent, and any explicit ID before a
// row is written. Model artifacts are parentless objects ( their
// zero-to-many version relationships live in MLflowArtifactLink ); only
// model versions carry a parent, and it must be an existing non-deleted
// registered model.
func (s *Store) validateReservation(ctx context.Context, object Object) error {
	switch object.Kind {
	case KindRegisteredModel, KindModelArtifact:
		if object.ParentID != parentNone {
			return fmt.Errorf("object of kind %s must not have a parent: %w", object.Kind, api.ErrBadRequest)
		}
	case KindModelVersion:
		if object.ParentID == parentNone {
			return fmt.Errorf("object of kind %s requires a parent: %w", object.Kind, api.ErrBadRequest)
		}
		var parent Object
		err := s.db.WithContext(ctx).
			Where("id = ? AND deleted = ?", object.ParentID, false).
			First(&parent).Error
		if err != nil {
			// A missing or deleted parent is a malformed reservation
			// request, not a missing REST resource.
			return fmt.Errorf("no registered-model parent %d for %s object %q: %w",
				object.ParentID, object.Kind, object.Name, api.ErrBadRequest)
		}
		if parent.Kind != KindRegisteredModel {
			return fmt.Errorf("parent %d of %s object %q is a %s, not a registered model: %w",
				object.ParentID, object.Kind, object.Name, parent.Kind, api.ErrBadRequest)
		}
	default:
		return fmt.Errorf("unknown object kind %q: %w", object.Kind, api.ErrBadRequest)
	}

	if object.ArtifactScopeID < 0 || object.ArtifactScopeID > MaxKubeflowID ||
		(object.Kind != KindModelArtifact && object.ArtifactScopeID != 0) {
		return fmt.Errorf("invalid artifact scope %d for %s: %w", object.ArtifactScopeID, object.Kind, api.ErrBadRequest)
	}
	if object.ArtifactScopeID != 0 {
		var version Object
		if err := s.db.WithContext(ctx).Where("id = ? AND kind = ? AND deleted = ?", object.ArtifactScopeID, KindModelVersion, false).First(&version).Error; err != nil {
			return fmt.Errorf("artifact scope %d is not a model version: %w", object.ArtifactScopeID, api.ErrBadRequest)
		}
	}

	if object.ID != 0 && (object.ID < 1 || object.ID > MaxKubeflowID) {
		return fmt.Errorf("explicit object id %d outside the REST-valid range [1, %d]: %w",
			object.ID, MaxKubeflowID, api.ErrBadRequest)
	}
	if object.Visibility != "" && object.Visibility != VisibilityReserved && object.Visibility != VisibilityCommitted {
		return fmt.Errorf("unknown object visibility %q: %w", object.Visibility, api.ErrBadRequest)
	}
	return nil
}

// ReserveObject allocates a stable numeric ID for a new object and
// inserts its initial snapshot. The kind/parent/name uniqueness is
// enforced by the database: a duplicate logical name returns a conflict
// and allocates nothing. Validation, insertion, generated-ID validation,
// and commit happen inside one transaction ( a savepoint when nested in
// WithinTx ), so an overflowed allocation — or a caller context canceled
// right after the INSERT — rolls the row back with the transaction rather
// than relying on a compensating delete that the same canceled context
// could prevent. The call fails explicitly if the ID range allowed by the
// REST validation is exhausted; an overflowed allocation leaves no row,
// visible or persisted, so callers never need to delete a failed
// reservation manually.
func (s *Store) ReserveObject(ctx context.Context, object Object) (Object, error) {
	var created Object
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txStore := &Store{db: tx}
		if err := txStore.validateReservation(ctx, object); err != nil {
			return err
		}

		reservation := object
		reservation.CreateTimeSinceEpoch = nowMillis()
		reservation.LastUpdateTimeSinceEpoch = reservation.CreateTimeSinceEpoch
		reservation.Deleted = false
		if reservation.Visibility == "" {
			reservation.Visibility = VisibilityCommitted
		}

		if err := tx.Create(&reservation).Error; err != nil {
			return fmt.Errorf("reserving %s object %q: %w", object.Kind, object.Name, mapDBError(err))
		}
		if reservation.ID > MaxKubeflowID {
			// Returning an error rolls the transaction ( or savepoint )
			// back, removing the unusable row.
			return fmt.Errorf("mlflow object id space exhausted (allocated %d > %d): %w",
				reservation.ID, MaxKubeflowID, api.ErrConflict)
		}
		created = reservation
		return nil
	})
	if err != nil {
		return Object{}, err
	}
	return created, nil
}

// lockResource loads the object row under an UPDATE lock. Every journal
// path that touches both a resource and its operations locks the
// resource row first, so concurrent prepares, claims, and publications
// on one resource serialize in the database rather than in process.
func lockResource(tx *gorm.DB, id int32) (Object, error) {
	var object Object
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", id).First(&object).Error
	if err != nil {
		return Object{}, mapDBError(err)
	}
	return object, nil
}

// lockOperationResource loads an operation and its resource under UPDATE
// locks in the resource-before-operation order, re-reading the operation
// after the resource lock so a concurrent publication cannot interleave.
func lockOperationResource(tx *gorm.DB, operationID string) (Operation, Object, error) {
	operation, objects, err := lockOperationGraph(tx, operationID)
	if err != nil {
		return Operation{}, Object{}, err
	}
	return operation, objects[operation.ResourceID], nil
}

// ensureIntentCurrent rejects an operation whose resource has moved past
// the revision the intent was prepared against. Such an intent is
// obsolete: its candidate was computed against state that no longer
// exists, so claiming, checkpointing, heartbeating, or publishing it
// must change nothing.
func ensureIntentCurrent(operation Operation, object Object) error {
	if object.Revision != operation.ExpectedRevision {
		return fmt.Errorf("operation %s is obsolete: resource %d is at revision %d, not %d: %w",
			operation.OperationUUID, object.ID, object.Revision, operation.ExpectedRevision, api.ErrConflict)
	}
	return nil
}

// PrepareOperation is the one-node graph wrapper.
func (s *Store) PrepareOperation(ctx context.Context, operation Operation) (Operation, error) {
	root, _, err := s.PrepareOperationGraph(ctx, operation, nil)
	return root, err
}

// ClaimOperation acquires the operation's lease for a worker. Each claim
// mints a new fencing token; every subsequent state change must present
// it. A live lease held by another worker, a terminal state, or an
// obsolete intent whose resource has moved past the expected revision is
// a conflict rejected before any remote dispatch. An expired lease may
// be reclaimed by any worker; reclamation preserves the persisted
// execution step, native result, and candidate so the new owner resumes
// rather than restarts.
func (s *Store) ClaimOperation(ctx context.Context, operationID, workerID string, lease time.Duration) (Operation, error) {
	if lease <= 0 {
		return Operation{}, fmt.Errorf("claiming operation %s: lease duration must be positive: %w", operationID, api.ErrBadRequest)
	}

	var claimed Operation
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		operation, object, err := lockOperationResource(tx, operationID)
		if err != nil {
			return err
		}
		if err := ensureIntentCurrent(operation, object); err != nil {
			return err
		}

		if operation.State == OperationSucceeded || operation.State == OperationFailed {
			return fmt.Errorf("operation %s already %s: %w", operationID, operation.State, api.ErrConflict)
		}
		if operation.LeaseOwner != nil && *operation.LeaseOwner != workerID &&
			operation.LeaseExpiresSinceEpoch != nil && *operation.LeaseExpiresSinceEpoch > nowMillis() {
			return fmt.Errorf("operation %s is leased by %s: %w", operationID, *operation.LeaseOwner, api.ErrConflict)
		}

		// Only the first claim of a freshly prepared intent advances the
		// step; reclaiming preserves whatever execution checkpoint the
		// previous owner persisted.
		step := operation.Step
		if step == StepPrepared {
			step = StepClaimed
		}

		now := nowMillis()
		expires := now + lease.Milliseconds()
		updates := map[string]any{
			"state":                        OperationRunning,
			"step":                         step,
			"lease_owner":                  workerID,
			"lease_expires_since_epoch":    expires,
			"fencing_token":                operation.FencingToken + 1,
			"attempts":                     operation.Attempts + 1,
			"last_update_time_since_epoch": now,
		}
		result := tx.Model(&Operation{}).
			Where("operation_uuid = ? AND fencing_token = ? AND state = ?", operationID, operation.FencingToken, operation.State).
			Updates(updates)
		if result.Error != nil {
			return mapDBError(result.Error)
		}
		if result.RowsAffected == 0 {
			// Lost a race with another claimant; zero affected rows is a
			// conflict, not a successful completion.
			return fmt.Errorf("operation %s claim lost a race: %w", operationID, api.ErrConflict)
		}

		err = tx.Where("operation_uuid = ?", operationID).First(&claimed).Error
		if err != nil {
			return mapDBError(err)
		}
		return nil
	})
	if err != nil {
		return Operation{}, fmt.Errorf("claiming operation %s: %w", operationID, err)
	}
	return claimed, nil
}

// HeartbeatOperation renews the caller's lease on a running operation.
// The caller must present the current fencing token and hold an unexpired
// lease; a stale token, an already-expired lease, or an obsolete intent
// whose resource has moved past the expected revision is a conflict.
func (s *Store) HeartbeatOperation(ctx context.Context, operationID string, fence int64, lease time.Duration) error {
	if lease <= 0 {
		return fmt.Errorf("heartbeating operation %s: lease duration must be positive: %w", operationID, api.ErrBadRequest)
	}
	now := nowMillis()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		operation, object, err := lockOperationResource(tx, operationID)
		if err != nil {
			return err
		}
		if err := ensureIntentCurrent(operation, object); err != nil {
			return err
		}
		result := tx.Model(&Operation{}).
			Where("operation_uuid = ? AND fencing_token = ? AND state = ? AND lease_expires_since_epoch > ?",
				operationID, fence, OperationRunning, now).
			Updates(map[string]any{
				"lease_expires_since_epoch":    now + lease.Milliseconds(),
				"last_update_time_since_epoch": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("stale fencing token %d, expired lease, or not running: %w", fence, api.ErrConflict)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("heartbeating operation %s: %w", operationID, err)
	}
	return nil
}

// CheckpointOperation persists an execution checkpoint under the caller's
// fencing token. A stale token, a non-running state, an expired lease, or
// an obsolete intent whose resource has moved past the expected revision
// makes no changes and returns a conflict.
func (s *Store) CheckpointOperation(ctx context.Context, operationID string, fence int64, step string, nativeVersion *string) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		operation, object, err := lockOperationResource(tx, operationID)
		if err != nil {
			return err
		}
		if err := ensureIntentCurrent(operation, object); err != nil {
			return err
		}
		updates := map[string]any{
			"step":                         step,
			"last_update_time_since_epoch": nowMillis(),
		}
		nativeVersion, err = retainExplicitNativeSelection(operation, nativeVersion)
		if err != nil {
			return err
		}
		if nativeVersion != nil {
			updates["native_result"] = *nativeVersion
		}
		result := tx.Model(&Operation{}).
			Where("operation_uuid = ? AND fencing_token = ? AND state = ? AND lease_expires_since_epoch > ?",
				operationID, fence, OperationRunning, nowMillis()).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("stale fencing token %d or expired lease: %w", fence, api.ErrConflict)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("checkpointing operation %s at %s: %w", operationID, step, err)
	}
	return nil
}

// candidateExternalID extracts the externalId field from an operation's
// immutable candidate snapshot, so publication can keep the queryable
// column synchronized with the committed JSON. A candidate without the
// field yields nil.
func candidateExternalID(candidateJSON string) (*string, error) {
	var candidate struct {
		ExternalID *string `json:"externalId"`
	}
	if err := json.Unmarshal([]byte(candidateJSON), &candidate); err != nil {
		return nil, fmt.Errorf("decoding candidate snapshot: %w", err)
	}
	return candidate.ExternalID, nil
}

// PublishOperation atomically applies the operation's immutable candidate
// to the committed object and completes the operation. Publication is
// conditional on the object still being at the expected revision and the
// caller holding the current fencing token on an unexpired lease; any
// mismatch — including a candidate whose external ID collides with
// another object's — changes nothing and returns a conflict. Queryable
// mutable columns ( including external_id ) are synchronized with the
// committed snapshot in the same transaction so indexes never serve
// stale values.
func (s *Store) PublishOperation(ctx context.Context, operationID string, fence int64) error {
	// Decode the journal payload before opening the transaction; an
	// unknown codec version fails without mutating anything.
	var operation Operation
	err := s.db.WithContext(ctx).Where("operation_uuid = ?", operationID).First(&operation).Error
	if err != nil {
		return fmt.Errorf("publishing operation %s: %w", operationID, mapDBError(err))
	}
	candidate, err := decodeMutationCandidate(operation.CandidateJSON)
	if err != nil {
		return fmt.Errorf("publishing operation %s: %w", operationID, err)
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, objects, err := lockOperationGraph(tx, operationID)
		if err != nil {
			return err
		}
		if err := requireAggregatePublisher(tx, locked); err != nil {
			return err
		}
		if locked.FencingToken != fence {
			return fmt.Errorf("operation %s fencing token mismatch (have %d, got %d): %w",
				operationID, locked.FencingToken, fence, api.ErrConflict)
		}
		if locked.State != OperationRunning {
			return fmt.Errorf("operation %s is %s, not running: %w", operationID, locked.State, api.ErrConflict)
		}
		if locked.LeaseExpiresSinceEpoch == nil || *locked.LeaseExpiresSinceEpoch <= nowMillis() {
			return fmt.Errorf("operation %s lease expired; reclaim before publishing: %w", operationID, api.ErrConflict)
		}

		if candidate != nil {
			if err := publishMutationCandidate(tx, locked, candidate, objects); err != nil {
				return err
			}
		} else {
			externalID, err := candidateExternalID(locked.CandidateJSON)
			if err != nil {
				return err
			}
			result := tx.Model(&Object{}).
				Where("id = ? AND revision = ?", locked.ResourceID, locked.ExpectedRevision).
				Updates(map[string]any{
					"committed_json":               locked.CandidateJSON,
					"revision":                     locked.ExpectedRevision + 1,
					"external_id":                  externalID,
					"visibility":                   VisibilityCommitted,
					"last_update_time_since_epoch": nowMillis(),
				})
			if result.Error != nil {
				// A duplicate external ID ( or name ) is an identity
				// conflict, not an internal error; the transaction rolls
				// back.
				return mapDBError(result.Error)
			}
			if result.RowsAffected == 0 {
				return fmt.Errorf("object %d moved past revision %d; refusing to publish operation %s: %w",
					locked.ResourceID, locked.ExpectedRevision, operationID, api.ErrConflict)
			}
		}

		// Completion is the final transactional step.
		result := tx.Model(&Operation{}).
			Where("operation_uuid = ? AND fencing_token = ? AND state = ?", operationID, fence, OperationRunning).
			Updates(map[string]any{
				"state":                        OperationSucceeded,
				"step":                         StepPublished,
				"lease_owner":                  nil,
				"lease_expires_since_epoch":    nil,
				"last_update_time_since_epoch": nowMillis(),
			})
		if result.Error != nil {
			return mapDBError(result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("operation %s completion lost its lease: %w", operationID, api.ErrConflict)
		}
		if err := tx.Where("root_operation_uuid = ?", graphRoot(locked)).Delete(&IntentResource{}).Error; err != nil {
			return mapDBError(err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("publishing operation %s: %w", operationID, err)
	}
	return nil
}

// GetOperation loads an operation by UUID.
func (s *Store) GetOperation(ctx context.Context, operationID string) (Operation, error) {
	var operation Operation
	err := s.db.WithContext(ctx).Where("operation_uuid = ?", operationID).First(&operation).Error
	if err != nil {
		return Operation{}, fmt.Errorf("no operation found for uuid %s: %w", operationID, mapDBError(err))
	}
	return operation, nil
}

// ListOperations returns operations in the given states, ordered by
// creation time.
func (s *Store) ListOperations(ctx context.Context, states ...OperationState) ([]Operation, error) {
	query := s.db.WithContext(ctx).Order("create_time_since_epoch ASC")
	if len(states) > 0 {
		query = query.Where("state IN ?", states)
	}

	var operations []Operation
	err := query.Find(&operations).Error
	if err != nil {
		return nil, mapDBError(err)
	}
	return operations, nil
}

func deref[T any](v *T) any {
	if v == nil {
		return nil
	}
	return *v
}

// RecordOperationRetry schedules the next automatic retry of a running
// operation. A non-nil failure code marks the failure definitive and
// stops automatic retries until the operator corrects the cause; the
// fencing token guards against a stale worker recording over a new
// owner's claim.
func (s *Store) RecordOperationRetry(ctx context.Context, operationID string,
	fence int64, nextRetrySinceEpoch int64, failureCode *string) error {
	updates := map[string]any{
		"next_retry_since_epoch":       nextRetrySinceEpoch,
		"last_update_time_since_epoch": nowMillis(),
	}
	if failureCode != nil {
		updates["failure_code"] = *failureCode
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		operation, _, err := lockOperationResource(tx, operationID)
		if err != nil {
			return err
		}
		checkpoint, err := retainExplicitNativeSelection(operation, operation.NativeResult)
		if err != nil {
			return err
		}
		if checkpoint != nil {
			updates["native_result"] = *checkpoint
		}
		result := tx.Model(&Operation{}).
			Where("operation_uuid = ? AND fencing_token = ? AND state = ? AND lease_expires_since_epoch > ?",
				operationID, fence, OperationRunning, nowMillis()).
			Updates(updates)
		if result.Error != nil {
			return mapDBError(result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("operation %s retry recording lost its lease: %w",
				operationID, api.ErrConflict)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("recording retry for operation %s: %w", operationID, err)
	}
	return nil
}

// SupersedeOperation terminalizes a running operation as superseded by
// an operator-authorized retry. It changes only the state, failure
// code, and lease: the execution step, native result, attempt count,
// and every other evidence column are retained so the superseded
// attempt stays auditable. The fencing token guards against a stale
// worker superseding a new owner's claim.
func (s *Store) SupersedeOperation(ctx context.Context, operationID string,
	fence int64, failureCode string) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, _, err := lockOperationResource(tx, operationID); err != nil {
			return err
		}
		result := tx.Model(&Operation{}).
			Where("operation_uuid = ? AND fencing_token = ? AND state = ? AND lease_expires_since_epoch > ?",
				operationID, fence, OperationRunning, nowMillis()).
			Updates(map[string]any{
				"state":                        OperationFailed,
				"failure_code":                 failureCode,
				"lease_owner":                  nil,
				"lease_expires_since_epoch":    nil,
				"last_update_time_since_epoch": nowMillis(),
			})
		if result.Error != nil {
			return mapDBError(result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("operation %s supersede lost its lease: %w", operationID, api.ErrConflict)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("superseding operation %s: %w", operationID, err)
	}
	return nil
}

// CompleteOperation finalizes a running operation with a native result
// but no candidate publication. It is the terminal step for fan-out
// child operations whose SQL effects belong to the parent's atomic
// publication.
func (s *Store) CompleteOperation(ctx context.Context, operationID string,
	fence int64, nativeResult *string) error {
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		operation, _, err := lockOperationResource(tx, operationID)
		if err != nil {
			return err
		}
		nativeResult, err = retainExplicitNativeSelection(operation, nativeResult)
		if err != nil {
			return err
		}
		updates := map[string]any{
			"state":                        OperationSucceeded,
			"step":                         StepNativeCreated,
			"lease_owner":                  nil,
			"lease_expires_since_epoch":    nil,
			"last_update_time_since_epoch": nowMillis(),
		}
		if nativeResult != nil {
			updates["native_result"] = *nativeResult
		}
		result := tx.Model(&Operation{}).
			Where("operation_uuid = ? AND fencing_token = ? AND state = ? AND lease_expires_since_epoch > ?",
				operationID, fence, OperationRunning, nowMillis()).
			Updates(updates)
		if result.Error != nil {
			return mapDBError(result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("operation %s completion lost its lease: %w",
				operationID, api.ErrConflict)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("completing operation %s: %w", operationID, err)
	}
	return nil
}
