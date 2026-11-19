package mlflow_test

import (
	"context"
	"encoding/json"
	"maps"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func waitRefreshBarrier(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh barrier was not reached")
	}
}

// Pausing Done evaluation holds a real waiter after it has selected its flight
// but before it can receive completion. No scheduling sleeps determine order.
type pausedRefreshWaiter struct {
	context.Context
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *pausedRefreshWaiter) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Context.Done()
}

func TestRefreshFlightKeepsItsOwnError(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			entered, release := backend.stub.requestBarrier("GET registered-models/search")
			defer release()
			backend.stub.failNextRequests("GET registered-models/search", 1)
			owner := make(chan error, 1)
			go func() { owner <- backend.service.Refresh(t.Context()) }()
			waitRefreshBarrier(t, entered)
			waitContext := &pausedRefreshWaiter{Context: t.Context(), entered: make(chan struct{}), release: make(chan struct{})}
			var unblock sync.Once
			defer unblock.Do(func() { close(waitContext.release) })
			waiter := make(chan error, 1)
			go func() { waiter <- backend.service.Refresh(waitContext) }()
			waitRefreshBarrier(t, waitContext.entered)
			release()
			require.ErrorIs(t, <-owner, api.ErrUnavailable)
			seedNativeModel(t, backend, "invalid-identity", "", map[string]string{"rhoai.adapter": adapterUUIDOf(t, backend), "rhoai.logicalId": "999999999"})
			require.ErrorIs(t, backend.service.Refresh(t.Context()), api.ErrConflict)
			unblock.Do(func() { close(waitContext.release) })
			require.ErrorIs(t, <-waiter, api.ErrUnavailable, "a later flight must not replace the first flight's result")
		})
	}
}

func TestRefreshBaselineIsConsistent(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "baseline")
			version := mustVersion(t, backend, model, "first")
			binding := bindingOf(t, backend, *version.Id)
			object, err := fixture.store.GetObject(t.Context(), binding.VersionID)
			require.NoError(t, err)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var unblock sync.Once
			defer unblock.Do(func() { close(release) })
			const callback = "refresh-baseline-objects"
			require.NoError(t, backend.db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "MLflowObject" && len(tx.Statement.Selects) == 2 {
					once.Do(func() { close(entered); <-release })
				}
			}))
			defer func() { require.NoError(t, backend.db.Callback().Query().Remove(callback)) }()
			var baseline mlflow.ObservationBaseline
			result := make(chan error, 1)
			go func() {
				var err error
				baseline, err = fixture.store.CaptureObservationBaseline(t.Context())
				result <- err
			}()
			waitRefreshBarrier(t, entered)
			require.NoError(t, backend.db.Transaction(func(tx *gorm.DB) error {
				if err := tx.Model(&mlflow.Object{}).Where("id = ?", object.ID).Update("revision", object.Revision+1).Error; err != nil {
					return err
				}
				return tx.Model(&mlflow.VersionBinding{}).Where("version_id = ?", object.ID).Update("revision", binding.Revision+1).Error
			}))
			unblock.Do(func() { close(release) })
			require.NoError(t, <-result)
			require.Equal(t, object.Revision, baseline.ObjectRevisions[object.ID])
			require.Equal(t, binding.Revision, baseline.BindingRevisions[object.ID], "object and binding revisions must come from the same database snapshot")
		})
	}
}

// A second replica imports a model while the first holds an older native page.
// Unowned tags do not authorize overwriting a row absent from the baseline.
func TestRefreshDefersConcurrentUnownedImport(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "new-import", "old", nil)
			entered, release := backend.stub.requestBarrier("GET model-versions/search")
			defer release()
			first := make(chan error, 1)
			go func() { first <- backend.service.Refresh(t.Context()) }()
			waitRefreshBarrier(t, entered)
			// Remove the registered gate for later requests without releasing the
			// first handler's captured gate; its response stays deterministically old.
			backend.stub.mu.Lock()
			delete(backend.stub.gates, "GET model-versions/search")
			backend.stub.models["new-import"]["description"] = "new"
			backend.stub.mu.Unlock()
			require.NoError(t, backend.newBackendService(t).Refresh(t.Context()))
			release()
			require.NoError(t, <-first)
			rows := committedObjects(t, backend, mlflow.KindRegisteredModel)
			require.Len(t, rows, 1)
			view, err := backend.service.GetRegisteredModelById(strconv.Itoa(int(rows[0].ID)))
			require.NoError(t, err)
			require.Equal(t, "new", *view.Description)
		})
	}
}

func TestRefreshStalePageDoesNotResurrectDeletedModel(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "delete-recreate")
			entered, release := backend.stub.requestBarrier("GET model-versions/search")
			defer release()
			first := make(chan error, 1)
			go func() { first <- backend.service.Refresh(t.Context()) }()
			waitRefreshBarrier(t, entered)
			backend.stub.mu.Lock()
			delete(backend.stub.gates, "GET model-versions/search")
			delete(backend.stub.models, model.Name)
			backend.stub.mu.Unlock()
			require.NoError(t, backend.newBackendService(t).Refresh(t.Context()))
			var deleted mlflow.Object
			require.NoError(t, backend.db.First(&deleted, mustObjectID(t, model.Id)).Error)
			require.True(t, deleted.Deleted)
			seedNativeModel(t, backend, model.Name, "recreated", nil)
			release()
			require.NoError(t, <-first)
			var after mlflow.Object
			require.NoError(t, backend.db.First(&after, deleted.ID).Error)
			require.Equal(t, deleted, after, "a stale owned page must preserve the tombstone and original public ID")
			_, err := backend.service.GetRegisteredModelById(*model.Id)
			require.ErrorIs(t, err, api.ErrNotFound)
		})
	}
}

func TestImportExternalIDConflictRollsBackObservation(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "a-first", "old", map[string]string{"rhoai.externalId": "first-id"})
			seedNativeModel(t, backend, "b-last", "old", map[string]string{"rhoai.externalId": "second-id"})
			require.NoError(t, backend.service.Refresh(t.Context()))
			before := committedObjects(t, backend, mlflow.KindRegisteredModel)
			stamp := lastRefreshTime(t, backend)
			seedNativeModel(t, backend, "a-first", "must-rollback", map[string]string{"rhoai.externalId": "first-id"})
			seedNativeModel(t, backend, "b-last", "new", map[string]string{"rhoai.externalId": "first-id"})
			require.ErrorIs(t, backend.service.Refresh(t.Context()), api.ErrConflict)
			require.Equal(t, before, committedObjects(t, backend, mlflow.KindRegisteredModel))
			require.Equal(t, stamp, lastRefreshTime(t, backend))
		})
	}
}

// The model page has already returned old when the local write commits.
// Equal timestamps deliberately rule out millisecond-based arbitration.
func TestRefreshStalePageCannotOverwriteRevision(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, replica := range []bool{false, true} {
				t.Run(map[bool]string{false: "same-service", true: "second-service"}[replica], func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					model, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "stale-page", Description: new("old")})
					require.NoError(t, err)
					original, err := fixture.store.GetObject(t.Context(), mustObjectID(t, model.Id))
					require.NoError(t, err)
					observer := backend.service
					if replica {
						observer = backend.newBackendService(t)
					}
					entered, release := backend.stub.requestBarrier("GET model-versions/search")
					defer release()
					result := make(chan error, 1)
					go func() { result <- observer.Refresh(t.Context()) }()
					waitRefreshBarrier(t, entered)
					_, err = backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Id: model.Id, Name: model.Name, Description: new("new")})
					require.NoError(t, err)
					require.NoError(t, backend.db.Model(&mlflow.Object{}).Where("id = ?", original.ID).Update("last_update_time_since_epoch", original.LastUpdateTimeSinceEpoch).Error)
					release()
					require.NoError(t, <-result)
					view, err := backend.service.GetRegisteredModelById(*model.Id)
					require.NoError(t, err)
					require.Equal(t, "new", *view.Description)
				})
			}
		})
	}
}

// Metadata already written natively is still a candidate until SQL publishes.
func TestRefreshDoesNotPublishReservedMetadata(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "pending-metadata", Description: new("old"), Owner: new("old-owner")})
			require.NoError(t, err)
			entered, release := backend.stub.requestBarrier("POST registered-models/set-tag")
			defer release()
			result := make(chan error, 1)
			go func() {
				_, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Id: model.Id, Name: model.Name, Description: new("candidate"), Owner: new("new-owner")})
				result <- err
			}()
			waitRefreshBarrier(t, entered)
			require.NoError(t, backend.newBackendService(t).Refresh(t.Context()))
			view, err := backend.service.GetRegisteredModelById(*model.Id)
			require.NoError(t, err)
			require.Equal(t, "old", *view.Description)
			release()
			require.NoError(t, <-result, "refresh must not obsolete the journal revision")
		})
	}
}

func TestRefreshFlightCanceledWaiterAndClose(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			entered, release := backend.stub.requestBarrier("GET registered-models/search")
			defer release()
			owner := make(chan error, 1)
			go func() { owner <- backend.service.Refresh(t.Context()) }()
			waitRefreshBarrier(t, entered)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waitContext := &pausedRefreshWaiter{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
			var unblock sync.Once
			defer unblock.Do(func() { close(waitContext.release) })
			waiter := make(chan error, 1)
			go func() { waiter <- backend.service.Refresh(waitContext) }()
			waitRefreshBarrier(t, waitContext.entered)
			cancel()
			unblock.Do(func() { close(waitContext.release) })
			select {
			case err := <-waiter:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				t.Error("a canceled waiter remained blocked on the owner's native request")
			}
			require.NoError(t, backend.service.Close())
			select {
			case err := <-owner:
				require.Error(t, err)
			case <-time.After(time.Second):
				t.Error("Close returned while its refresh was still running")
			}
			release()
			require.ErrorIs(t, backend.service.Refresh(t.Context()), context.Canceled)
		})
	}
}

func TestImportExternalReservedFieldsAndBoundVersion(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "external-fields", "old", map[string]string{"rhoai.owner": "old-owner"})
			seedNativeVersion(t, backend, "external-fields", "1", "s3://bucket/model", "old", nil)
			require.NoError(t, backend.service.Refresh(t.Context()))
			model := committedObjects(t, backend, mlflow.KindRegisteredModel)[0]
			version := committedObjects(t, backend, mlflow.KindModelVersion)[0]
			seedNativeModel(t, backend, "external-fields", "new", map[string]string{"rhoai.owner": "new-owner", "rhoai.externalId": "model-external", "rhoai.state": "ARCHIVED"})
			seedNativeVersion(t, backend, "external-fields", "1", "s3://bucket/model", "new-version", map[string]string{"rhoai.author": "new-author", "rhoai.externalId": "version-external", "rhoai.state": "ARCHIVED", "note": "changed"})
			for range 2 {
				require.NoError(t, backend.service.Refresh(t.Context()))
			}
			models := committedObjects(t, backend, mlflow.KindRegisteredModel)
			versions := committedObjects(t, backend, mlflow.KindModelVersion)
			require.Len(t, models, 1)
			require.Len(t, versions, 1)
			require.Equal(t, model.ID, models[0].ID)
			require.Equal(t, version.ID, versions[0].ID)
			m, err := backend.service.GetRegisteredModelById(strconv.Itoa(int(model.ID)))
			require.NoError(t, err)
			require.Equal(t, "new-owner", *m.Owner)
			require.Equal(t, "model-external", *models[0].ExternalID)
			require.Equal(t, openapi.REGISTEREDMODELSTATE_ARCHIVED, *m.State)
			v, err := backend.service.GetModelVersionById(strconv.Itoa(int(version.ID)))
			require.NoError(t, err)
			require.Equal(t, "new-version", *v.Description)
			require.Equal(t, "new-author", *v.Author)
			require.Equal(t, "version-external", *versions[0].ExternalID)
			require.Equal(t, openapi.MODELVERSIONSTATE_ARCHIVED, *v.State)
			require.Equal(t, "changed", v.CustomProperties["note"].MetadataStringValue.StringValue)
		})
	}
}

func TestImportInvalidReservedTagsPreserveCompleteView(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, invalid := range []string{"rhoai.state", "rhoai.versionName", "rhoai.logicalId", "rhoai.generation", "generation-mismatch", "rhoai.adapter", "stripped-adapter-foreign-logical-id"} {
				t.Run(invalid, func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					model := mustModel(t, backend, "identity-tags")
					version := mustVersion(t, backend, model, "logical")
					mustPrimaryArtifact(t, backend, version, "s3://bucket/model")
					before := committedObjects(t, backend, mlflow.KindModelVersion)
					backend.stub.mu.Lock()
					tags := backend.stub.versions[model.Name]["1"].(map[string]any)["tags"].(map[string]string)
					switch invalid {
					case "stripped-adapter-foreign-logical-id":
						delete(tags, "rhoai.adapter")
						tags["rhoai.logicalId"] = "999999999"
					case "generation-mismatch":
						tags["rhoai.generation"] = "2"
					default:
						tags[invalid] = "invalid"
					}
					backend.stub.models[model.Name]["description"] = "must-roll-back"
					backend.stub.mu.Unlock()
					require.ErrorIs(t, backend.service.Refresh(t.Context()), api.ErrConflict)
					require.Equal(t, before, committedObjects(t, backend, mlflow.KindModelVersion))
					m, err := backend.service.GetRegisteredModelById(*model.Id)
					require.NoError(t, err)
					require.NotEqual(t, "must-roll-back", m.GetDescription())
				})
			}
		})
	}
}

func TestImportModelRejectsForeignIdentity(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, invalid := range []string{"rhoai.adapter", "stripped-adapter-foreign-logical-id"} {
				t.Run(invalid, func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					model := mustModel(t, backend, "foreign-model-identity")
					before := committedObjects(t, backend, mlflow.KindRegisteredModel)
					backend.stub.mu.Lock()
					tags := backend.stub.models[model.Name]["tags"].(map[string]string)
					if invalid == "rhoai.adapter" {
						tags[invalid] = "another-adapter"
					} else {
						delete(tags, "rhoai.adapter")
						tags["rhoai.logicalId"] = "999999999"
					}
					backend.stub.models[model.Name]["description"] = "must-not-publish"
					backend.stub.mu.Unlock()
					require.ErrorIs(t, backend.service.Refresh(t.Context()), api.ErrConflict)
					require.Equal(t, before, committedObjects(t, backend, mlflow.KindRegisteredModel))
				})
			}
		})
	}
}

func TestImportInitiallyForeignAdapterConflictsConsistently(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "foreign-import", "", map[string]string{"rhoai.adapter": "another-adapter"})
			for range 2 {
				require.ErrorIs(t, backend.service.Refresh(t.Context()), api.ErrConflict)
				require.Empty(t, committedObjects(t, backend, mlflow.KindRegisteredModel))
			}
		})
	}
}

func TestImportStrippedIdentityUsesEstablishedBinding(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "stripped-identity")
			version := mustVersion(t, backend, model, "logical")
			mustPrimaryArtifact(t, backend, version, "s3://bucket/model")
			backend.stub.mu.Lock()
			delete(backend.stub.models[model.Name]["tags"].(map[string]string), "rhoai.logicalId")
			tags := backend.stub.versions[model.Name]["1"].(map[string]any)["tags"].(map[string]string)
			delete(tags, "rhoai.logicalId")
			delete(tags, "rhoai.generation")
			backend.stub.versions[model.Name]["1"].(map[string]any)["description"] = "external-change"
			backend.stub.mu.Unlock()
			require.NoError(t, backend.service.Refresh(t.Context()))
			view, err := backend.service.GetModelVersionById(*version.Id)
			require.NoError(t, err)
			require.Equal(t, "external-change", *view.Description)
			require.Equal(t, *version.Id, *view.Id)
		})
	}
}

func TestImportRetainedUntaggedHistoryIsNotReimported(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			mustModel(t, backend, "import-history")
			seedNativeVersion(t, backend, "import-history", "1", "s3://bucket/old", "", nil)
			require.NoError(t, backend.service.Refresh(t.Context()))
			versions := committedObjects(t, backend, mlflow.KindModelVersion)
			require.Len(t, versions, 1)
			versionID := strconv.Itoa(int(versions[0].ID))
			binding := bindingOf(t, backend, versionID)
			artifactID := strconv.Itoa(int(*binding.PrimaryArtifactID))
			_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: &artifactID, Uri: new("s3://bucket/new")}}, versionID)
			require.NoError(t, err)
			before := bindingOf(t, backend, versionID)
			require.Equal(t, int64(2), before.ActiveGeneration)
			for range 2 {
				require.NoError(t, backend.service.Refresh(t.Context()), "retained untagged import history cannot claim the logical name again")
			}
			require.Len(t, committedObjects(t, backend, mlflow.KindModelVersion), 1)
			require.Equal(t, before, bindingOf(t, backend, versionID))
			artifact, err := backend.service.GetModelArtifactById(artifactID)
			require.NoError(t, err)
			require.Equal(t, "s3://bucket/new", *artifact.Uri)
			backend.stub.mu.Lock()
			backend.stub.versions["import-history"]["1"].(map[string]any)["tags"] = map[string]string{"rhoai.logicalId": "999999999"}
			backend.stub.mu.Unlock()
			require.ErrorIs(t, backend.service.Refresh(t.Context()), api.ErrConflict, "history cannot excuse a present forged identity")
		})
	}
}

func TestRefreshExternalMetadataThenLocalRestore(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "restore-observed", Owner: new("original-owner"), CustomProperties: map[string]openapi.MetadataValue{"note": stringValue("original-note")}})
			require.NoError(t, err)
			version, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{Name: "first", Author: new("original-author"), State: new(openapi.MODELVERSIONSTATE_LIVE), CustomProperties: map[string]openapi.MetadataValue{"score": intValue("1")}}, model.Id)
			require.NoError(t, err)
			mustPrimaryArtifact(t, backend, version, "s3://bucket/model")
			backend.stub.mu.Lock()
			modelTags := backend.stub.models[model.Name]["tags"].(map[string]string)
			modelTags["rhoai.owner"] = "external-owner"
			modelTags["note"] = "external-note"
			versionTags := backend.stub.versions[model.Name]["1"].(map[string]any)["tags"].(map[string]string)
			versionTags["rhoai.author"] = "external-author"
			versionTags["rhoai.state"] = "ARCHIVED"
			versionTags["score"] = "2"
			versionTags["rhoai.artifact.marker"] = "keep-artifact-tag"
			backend.stub.mu.Unlock()
			require.NoError(t, backend.service.Refresh(t.Context()))
			_, err = backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Id: model.Id, Name: model.Name, Owner: new("original-owner"), CustomProperties: map[string]openapi.MetadataValue{"note": stringValue("original-note")}})
			require.NoError(t, err)
			_, err = backend.service.UpsertModelVersion(&openapi.ModelVersion{Id: version.Id, Name: version.Name, Author: new("original-author"), State: new(openapi.MODELVERSIONSTATE_LIVE), CustomProperties: map[string]openapi.MetadataValue{"score": intValue("1")}}, model.Id)
			require.NoError(t, err)
			backend.stub.mu.Lock()
			gotOwner, gotNote := modelTags["rhoai.owner"], modelTags["note"]
			gotAuthor, gotState, gotScore := versionTags["rhoai.author"], versionTags["rhoai.state"], versionTags["score"]
			gotArtifact := versionTags["rhoai.artifact.marker"]
			count := len(backend.stub.versions[model.Name])
			backend.stub.mu.Unlock()
			require.Equal(t, "original-owner", gotOwner)
			require.Equal(t, "original-note", gotNote)
			require.Equal(t, "original-author", gotAuthor)
			require.Equal(t, "LIVE", gotState)
			require.Equal(t, "1", gotScore)
			require.Equal(t, "keep-artifact-tag", gotArtifact)
			require.Equal(t, 1, count, "metadata updates never create another generation")
			require.NoError(t, backend.service.Refresh(t.Context()))
			m, err := backend.service.GetRegisteredModelById(*model.Id)
			require.NoError(t, err)
			require.Equal(t, "original-owner", *m.Owner)
			v, err := backend.service.GetModelVersionById(*version.Id)
			require.NoError(t, err)
			require.Equal(t, "original-author", *v.Author)
			require.Equal(t, "1", v.CustomProperties["score"].MetadataIntValue.IntValue)
		})
	}
}

func TestRefreshKnownOperationCannotHideConflictingNativeIdentity(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, mismatch := range []string{"model", "source"} {
				t.Run(mismatch, func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					model := mustModel(t, backend, "known-operation")
					other := mustModel(t, backend, "other-parent")
					version := mustVersion(t, backend, model, "first")
					backend.stub.mu.Lock()
					backend.stub.commitThenFail = 1
					backend.stub.mu.Unlock()
					_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/original")
					require.ErrorIs(t, err, api.ErrUnavailable)
					backend.stub.mu.Lock()
					tags := maps.Clone(backend.stub.versions[model.Name]["1"].(map[string]any)["tags"].(map[string]string))
					backend.stub.mu.Unlock()
					name, source := model.Name, "s3://bucket/original"
					if mismatch == "model" {
						name = other.Name
					} else {
						source = "s3://bucket/forged"
					}
					seedNativeVersion(t, backend, name, "99", source, "", tags)
					var before, after []mlflow.Object
					require.NoError(t, backend.db.Order("id").Find(&before).Error)
					require.ErrorIs(t, backend.service.Refresh(t.Context()), api.ErrConflict, "a known pending UUID cannot exempt a different native tuple")
					require.NoError(t, backend.db.Order("id").Find(&after).Error)
					require.Equal(t, before, after)
				})
			}
		})
	}
}

func TestRefreshMetadataOperationCannotHideAnotherNativeVersion(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "metadata-operation")
			version := mustVersion(t, backend, model, "first")
			mustPrimaryArtifact(t, backend, version, "s3://bucket/model")
			entered, release := backend.stub.requestBarrier("POST model-versions/set-tag")
			defer release()
			updated := make(chan error, 1)
			go func() {
				_, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{Id: version.Id, Name: version.Name, Author: new("candidate-author")}, model.Id)
				updated <- err
			}()
			waitRefreshBarrier(t, entered)
			var operation mlflow.Operation
			require.NoError(t, backend.db.Where("kind = ? AND state = ?", mlflow.OperationKindVersionUpdate, mlflow.OperationRunning).First(&operation).Error)
			backend.stub.mu.Lock()
			tags := maps.Clone(backend.stub.versions[model.Name]["1"].(map[string]any)["tags"].(map[string]string))
			backend.stub.mu.Unlock()
			tags["rhoai.operation"] = operation.OperationUUID
			seedNativeVersion(t, backend, model.Name, "99", "s3://bucket/model", "", tags)
			err := backend.service.Refresh(t.Context())
			release()
			require.NoError(t, <-updated)
			require.ErrorIs(t, err, api.ErrConflict, "metadata operation ownership is limited to its established native binding")
		})
	}
}

func TestRefreshMetadataOperationOnRetainedHistory(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "metadata-history")
			version := mustVersion(t, backend, model, "first")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/old")
			_, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{Id: version.Id, Name: version.Name, Author: new("edited-author")}, model.Id)
			require.NoError(t, err)
			var operation mlflow.Operation
			require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionUpdate).First(&operation).Error)
			backend.stub.mu.Lock()
			backend.stub.versions[model.Name]["1"].(map[string]any)["tags"].(map[string]string)["rhoai.operation"] = operation.OperationUUID
			backend.stub.mu.Unlock()
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/new")}}, *version.Id)
			require.NoError(t, err)
			before := bindingOf(t, backend, *version.Id)
			require.Equal(t, int64(2), before.ActiveGeneration)
			require.NoError(t, backend.service.Refresh(t.Context()))
			require.Equal(t, before, bindingOf(t, backend, *version.Id))
			require.Len(t, committedObjects(t, backend, mlflow.KindModelVersion), 1)
		})
	}
}

func TestRefreshLegacyUncertainWorkUsesRecordedTuple(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "legacy-observation")
			version := mustVersion(t, backend, model, "first")
			backend.stub.mu.Lock()
			backend.stub.commitThenFail = 1
			backend.stub.mu.Unlock()
			_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/legacy")
			require.ErrorIs(t, err, api.ErrUnavailable)
			operation := materializeOperationOf(t, backend)
			var candidate mlflow.MutationCandidate
			require.NoError(t, json.Unmarshal([]byte(operation.CandidateJSON), &candidate))
			candidate.CodecVersion = 2
			candidate.NativeWork = nil
			for i := range candidate.Objects {
				candidate.Objects[i].Snapshot.CodecVersion = 2
			}
			payload, err := json.Marshal(candidate)
			require.NoError(t, err)
			require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", operation.OperationUUID).Updates(map[string]any{"candidate_json": string(payload), "execution_plan_json": nil}).Error)
			backend.stub.mu.Lock()
			tags := backend.stub.versions[model.Name]["1"].(map[string]any)["tags"].(map[string]string)
			delete(tags, "rhoai.generation")
			forgedTags := maps.Clone(tags)
			backend.stub.mu.Unlock()
			require.NoError(t, backend.service.Refresh(t.Context()), "legacy uncertain work remains hidden without requiring an execution-plan write")
			require.Empty(t, committedObjects(t, backend, mlflow.KindModelArtifact))
			seedNativeVersion(t, backend, model.Name, "99", "s3://bucket/forged", "", forgedTags)
			require.ErrorIs(t, backend.service.Refresh(t.Context()), api.ErrConflict)
			retained, err := fixture.store.GetOperation(t.Context(), operation.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, string(payload), retained.CandidateJSON)
			require.Nil(t, retained.ExecutionPlanJSON)
		})
	}
}

func TestRefreshRecordedHistoryRejectsChangedSource(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []string{"historical", "active-stripped-operation", "active-unknown-operation"} {
				t.Run(mode, func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					mustModel(t, backend, "source-proof")
					seedNativeVersion(t, backend, "source-proof", "1", "s3://bucket/original", "", nil)
					require.NoError(t, backend.service.Refresh(t.Context()))
					version := committedObjects(t, backend, mlflow.KindModelVersion)[0]
					versionID := strconv.Itoa(int(version.ID))
					if mode == "historical" {
						binding := bindingOf(t, backend, versionID)
						artifactID := strconv.Itoa(int(*binding.PrimaryArtifactID))
						_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: &artifactID, Uri: new("s3://bucket/replaced")}}, versionID)
						require.NoError(t, err)
					}
					var before, after []mlflow.Object
					require.NoError(t, backend.db.Order("id").Find(&before).Error)
					stamp := lastRefreshTime(t, backend)
					backend.stub.mu.Lock()
					native := backend.stub.versions["source-proof"]["1"].(map[string]any)
					native["source"] = "https://storage.example/reused?signature=must-stay-private"
					if mode == "active-unknown-operation" {
						native["tags"] = map[string]string{"rhoai.operation": "unknown-operation"}
					}
					backend.stub.mu.Unlock()
					err := backend.service.Refresh(t.Context())
					require.ErrorIs(t, err, api.ErrConflict, "a reused native number is not proof when the immutable source changed")
					require.NotContains(t, err.Error(), "must-stay-private")
					require.NoError(t, backend.db.Order("id").Find(&after).Error)
					require.Equal(t, before, after)
					require.Equal(t, stamp, lastRefreshTime(t, backend))
				})
			}
		})
	}
}
