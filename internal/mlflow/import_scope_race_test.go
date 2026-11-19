package mlflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// legacyImportRow recreates only the proven old import representation. Tests
// keep the actual committed binding/history and artifact ID/link intact.
func legacyImportRow(t *testing.T, backend *backendFixture, version *openapi.ModelVersion) mlflow.Object {
	t.Helper()
	binding := bindingOf(t, backend, *version.Id)
	var row mlflow.Object
	require.NoError(t, backend.db.First(&row, *binding.PrimaryArtifactID).Error)
	var snapshot mlflow.StoredSnapshot
	require.NoError(t, json.Unmarshal([]byte(row.CommittedJSON), &snapshot))
	var artifact openapi.ModelArtifact
	require.NoError(t, json.Unmarshal(snapshot.Resource, &artifact))
	artifact.Name = new("primary")
	resource, err := json.Marshal(&artifact)
	require.NoError(t, err)
	snapshot.Resource = resource
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	row.Name = fmt.Sprintf("native-%s-%s", *binding.NativeModelName, *binding.NativeVersionNumber)
	row.ArtifactScopeID = 0
	row.CommittedJSON = string(encoded)
	require.NoError(t, backend.db.Save(&row).Error)
	require.NoError(t, backend.db.Model(&mlflow.VersionHistory{}).Where("version_id = ?", binding.VersionID).Update("operation_uuid", "import").Error)
	return row
}

func waitBarrier(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal("database barrier did not complete:", ctx.Err())
	}
}

// TestImportedArtifactRepairCurrentGuards commits new evidence after Refresh's
// initial state scan, before its resource locks. MySQL's default REPEATABLE
// READ would keep seeing the earlier one-link/no-operation snapshot.
func TestImportedArtifactRepairCurrentGuards(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []string{"shared-link", "active-intent", "active-after-prefilter"} {
				t.Run(mode, func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					model := mustModel(t, backend, "repair-current-"+mode)
					first := mustVersion(t, backend, model, "1")
					second := mustVersion(t, backend, model, "2")
					mustPrimaryArtifact(t, backend, first, "s3://bucket/current.onnx")
					before := legacyImportRow(t, backend, first)
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					defer cancel()
					scanned := make(chan struct{})
					published := make(chan struct{})
					var once sync.Once
					callback := "task3-current-" + mode
					require.NoError(t, backend.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
						if tx.Error != nil {
							return
						}
						if mode == "active-after-prefilter" {
							id, ok := lockedObjectID(tx)
							if !ok || id != mustObjectID(t, model.Id) {
								return
							}
						} else if tx.Statement.Table != "MLflowVersionHistory" || tx.Statement.Clauses["WHERE"].Expression != nil {
							return
						}
						once.Do(func() {
							close(scanned)
							select {
							case <-published:
							case <-ctx.Done():
								tx.Error = tx.AddError(ctx.Err())
							}
						})
					}))
					defer func() { require.NoError(t, backend.db.Callback().Query().Remove(callback)) }()
					done := make(chan error, 1)
					go func() { done <- backend.service.Refresh(ctx) }()
					waitBarrier(t, ctx, scanned)
					var prepared mlflow.Operation
					if mode == "shared-link" {
						require.NoError(t, backend.db.WithContext(ctx).Create(&mlflow.ArtifactLink{ArtifactID: before.ID, VersionID: mustObjectID(t, second.Id)}).Error)
					} else {
						var err error
						prepared, err = fixture.store.PrepareOperation(ctx, mlflow.Operation{OperationUUID: "current-" + mode, ResourceID: before.ID, Kind: "artifact-update", ExpectedRevision: before.Revision, CandidateJSON: "{}", PayloadHash: "original-intent"})
						require.NoError(t, err)
					}
					close(published)
					require.NoError(t, <-done)
					got, err := fixture.store.GetObject(ctx, before.ID)
					require.NoError(t, err)
					require.Equal(t, before.Name, got.Name, "new evidence must defer correction")
					require.Equal(t, before.CommittedJSON, got.CommittedJSON)
					require.Equal(t, before.Revision, got.Revision)
					require.Zero(t, got.ArtifactScopeID)
					if mode != "shared-link" {
						actual, err := fixture.store.GetOperation(ctx, "current-"+mode)
						require.NoError(t, err)
						require.Equal(t, prepared, actual, "active immutable evidence remains intact")
					}
				})
			}
		})
	}
}

func lockedObjectID(tx *gorm.DB) (int32, bool) {
	if tx.Statement.Table != "MLflowObject" || tx.Statement.Clauses["FOR"].Expression == nil {
		return 0, false
	}
	where, ok := tx.Statement.Clauses["WHERE"].Expression.(clause.Where)
	if !ok {
		return 0, false
	}
	for _, expr := range where.Exprs {
		if condition, ok := expr.(clause.Expr); ok && condition.SQL == "id = ?" && len(condition.Vars) == 1 {
			id, ok := condition.Vars[0].(int32)
			return id, ok
		}
	}
	return 0, false
}

// TestImportedArtifactRepairGlobalLockOrder forces a real share publisher to
// hold version 3 while correction holds version 2. It requests artifact 4
// exactly when correction requests version 3. Pair order 2,4,3,5 deadlocks;
// global order 2,3,4,5 lets the publisher finish before correction proceeds.
func TestImportedArtifactRepairGlobalLockOrder(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "repair-order")
			first := mustVersion(t, backend, model, "1")
			second := mustVersion(t, backend, model, "2")
			mustPrimaryArtifact(t, backend, first, "s3://bucket/one.onnx")
			mustPrimaryArtifact(t, backend, second, "s3://bucket/two.onnx")
			original := legacyImportRow(t, backend, first)
			legacyImportRow(t, backend, second)
			firstID, secondID := mustObjectID(t, first.Id), mustObjectID(t, second.Id)
			require.Less(t, firstID, secondID)
			require.Less(t, secondID, original.ID)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			firstLocked := make(chan struct{})
			publisherLocked := make(chan struct{})
			requestingSecond := make(chan struct{})
			var firstOnce, secondOnce sync.Once
			callback := "task3-global-order"
			require.NoError(t, backend.db.Callback().Query().After("gorm:query").Register(callback+"-after", func(tx *gorm.DB) {
				if id, ok := lockedObjectID(tx); ok && id == firstID && tx.Error == nil {
					firstOnce.Do(func() {
						close(firstLocked)
						select {
						case <-publisherLocked:
						case <-ctx.Done():
							tx.Error = tx.AddError(ctx.Err())
						}
					})
				}
			}))
			require.NoError(t, backend.db.Callback().Query().Before("gorm:query").Register(callback+"-before", func(tx *gorm.DB) {
				if id, ok := lockedObjectID(tx); ok && id == secondID {
					secondOnce.Do(func() { close(requestingSecond) })
				}
			}))
			defer func() {
				require.NoError(t, backend.db.Callback().Query().Remove(callback+"-after"))
				require.NoError(t, backend.db.Callback().Query().Remove(callback+"-before"))
			}()
			refreshed := make(chan error, 1)
			published := make(chan error, 1)
			go func() { refreshed <- backend.service.Refresh(ctx) }()
			waitBarrier(t, ctx, firstLocked)
			go func() {
				published <- backend.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					// The same ascending resource order as PublishOperation for sharing
					// artifact 4 with version 3. Raw avoids the refresh query barriers.
					table := tx.Statement.Quote("MLflowObject")
					var id int32
					if err := tx.Raw("SELECT id FROM "+table+" WHERE id = ? FOR UPDATE", secondID).Scan(&id).Error; err != nil {
						return err
					}
					close(publisherLocked)
					select {
					case <-requestingSecond:
					case <-ctx.Done():
						return ctx.Err()
					}
					if err := tx.Raw("SELECT id FROM "+table+" WHERE id = ? FOR UPDATE", original.ID).Scan(&id).Error; err != nil {
						return err
					}
					return tx.Create(&mlflow.ArtifactLink{ArtifactID: original.ID, VersionID: secondID}).Error
				})
			}()
			refreshErr, publishErr := <-refreshed, <-published
			require.NoError(t, refreshErr, "correction must not deadlock the share publisher")
			require.NoError(t, publishErr, "share publication must retain its global lock order")
			got, err := fixture.store.GetObject(ctx, original.ID)
			require.NoError(t, err)
			require.Equal(t, original.Name, got.Name, "a newly shared artifact is deferred")
			require.Equal(t, original.CommittedJSON, got.CommittedJSON)
			found, err := fixture.store.FindArtifactInVersion(ctx, original.Name, mustObjectID(t, second.Id))
			require.NoError(t, err)
			require.Equal(t, strconv.FormatInt(int64(original.ID), 10), strconv.FormatInt(int64(found.ID), 10))
		})
	}
}

// Normal refresh must not pay the one-time upgrade's broad lock cost.
func TestImportedArtifactRepairSkipsOrdinaryResourceLocks(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "ordinary-refresh")
			version := mustVersion(t, backend, model, "1")
			mustPrimaryArtifact(t, backend, version, "s3://bucket/ordinary.onnx")
			unrelated, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{Kind: mlflow.KindModelArtifact, Name: "unrelated-sql-artifact"})
			require.NoError(t, err)
			locks := 0
			callback := "task3-normal-refresh-locks"
			require.NoError(t, backend.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
				if id, ok := lockedObjectID(tx); ok && id == unrelated.ID {
					locks++
				}
			}))
			defer func() { require.NoError(t, backend.db.Callback().Query().Remove(callback)) }()
			require.NoError(t, backend.service.Refresh(t.Context()))
			require.Zero(t, locks, "ordinary revision locks must not expand into legacy repair's whole resource set")
		})
	}
}
