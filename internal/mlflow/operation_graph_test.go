package mlflow_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Preparing siblings in separate transactions leaves an orphan when a later
// version is busy. The whole attempted graph must leave no journal evidence.
func TestGraphPrepareConflictIsAtomic(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "graph-atomic")
			first := mustVersion(t, backend, model, "1")
			second := mustVersion(t, backend, model, "2")
			primary := sharedPrimary(t, backend, model, first, second, "s3://bucket/old")
			id, err := strconv.Atoi(*second.Id)
			require.NoError(t, err)
			version, err := fixture.store.GetObject(t.Context(), int32(id))
			require.NoError(t, err)
			_, err = fixture.store.PrepareOperation(t.Context(), mlflow.Operation{OperationUUID: "conflicting-version", ResourceID: version.ID, ExpectedRevision: version.Revision, Kind: "version-update", CandidateJSON: "{}", PayloadHash: "busy"})
			require.NoError(t, err)
			var before int64
			require.NoError(t, fixture.db.Model(&mlflow.Operation{}).Count(&before).Error)
			backend.stub.mu.Lock()
			nativeRequestsBefore := len(backend.stub.request)
			backend.stub.mu.Unlock()
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/new")}}, *first.Id)
			require.ErrorIs(t, err, api.ErrConflict)
			var after int64
			require.NoError(t, fixture.db.Model(&mlflow.Operation{}).Count(&after).Error)
			assert.Equal(t, before, after, "a late conflict must roll back every sibling and root")
			assert.Empty(t, childOperations(t, backend))
			backend.stub.mu.Lock()
			nativeRequestsAfter := len(backend.stub.request)
			backend.stub.mu.Unlock()
			assert.Equal(t, nativeRequestsBefore, nativeRequestsAfter, "known foreign conflict must perform no native requests, including reads")
			assert.Len(t, backend.stub.versions[model.Name], 2, "no native creates before the entire graph commits")
		})
	}
}

func TestGraphConflictDifferentRequestDoesNotResume(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "graph-conflict")
			version := mustVersion(t, backend, model, "1")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/old")
			backend.stub.mu.Lock()
			backend.stub.commitThenFail = 1
			backend.stub.mu.Unlock()
			update := func(source string) error {
				_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new(source)}}, *version.Id)
				return err
			}
			require.ErrorIs(t, update("s3://bucket/first"), api.ErrUnavailable)
			require.ErrorIs(t, update("s3://bucket/different"), api.ErrConflict)
			visible, err := backend.service.GetModelArtifactById(*primary.ModelArtifact.Id)
			require.NoError(t, err)
			assert.Equal(t, "s3://bucket/old", *visible.Uri)
		})
	}
}

func TestGraphRetryMultipleCompletedFanOuts(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "graph-repeat")
			first := mustVersion(t, backend, model, "1")
			second := mustVersion(t, backend, model, "2")
			primary := sharedPrimary(t, backend, model, first, second, "s3://bucket/old")
			for _, source := range []string{"s3://bucket/new", "s3://bucket/newer"} {
				_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new(source)}}, *first.Id)
				require.NoError(t, err)
			}
			assert.Len(t, childOperations(t, backend), 4)
			assert.Equal(t, int64(3), bindingOf(t, backend, *first.Id).ActiveGeneration)
		})
	}
}

func TestGraphStoreReservationsAndRetry(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			model, err := fixture.store.ReserveObject(t.Context(), reservedModel("graph-store"))
			require.NoError(t, err)
			version, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: "1", CommittedJSON: "{}"})
			require.NoError(t, err)
			artifact, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{Kind: mlflow.KindModelArtifact, Name: "artifact", CommittedJSON: "{}"})
			require.NoError(t, err)
			makeGraph := func(rootID, childID, source string) (mlflow.Operation, []mlflow.Operation) {
				c := mlflow.MutationCandidate{CodecVersion: 3, Objects: []mlflow.ObjectChange{{ID: artifact.ID, ExpectedRevision: artifact.Revision, Snapshot: mlflow.StoredSnapshot{CodecVersion: 3, Kind: mlflow.KindModelArtifact, Resource: json.RawMessage(`{"name":"artifact"}`)}}}, ReadRevisions: map[int32]int64{version.ID: version.Revision}, Bindings: []mlflow.BindingChange{{Value: mlflow.VersionBinding{VersionID: version.ID, ModelID: model.ID, Phase: mlflow.PhaseReady, NativeModelName: new(model.Name), ActiveGeneration: 1}, ResultOperationUUID: childID}}}
				payload, err := json.Marshal(c)
				require.NoError(t, err)
				root := mlflow.Operation{OperationUUID: rootID, ResourceID: artifact.ID, Kind: mlflow.OperationKindVersionReplaceFanOut, ExpectedRevision: artifact.Revision, CandidateJSON: string(payload), PayloadHash: "root", Generation: 1}
				c = mlflow.MutationCandidate{CodecVersion: 3, NativeWork: &mlflow.NativeVersionPlan{LogicalVersionID: version.ID, ModelName: model.Name, Source: source, Generation: 1, OwnedTags: map[string]string{"rhoai.operation": childID}}}
				payload, err = json.Marshal(c)
				require.NoError(t, err)
				child := mlflow.Operation{OperationUUID: childID, ResourceID: version.ID, Kind: mlflow.OperationKindVersionReplaceChild, ExpectedRevision: version.Revision, CandidateJSON: string(payload), PayloadHash: "child", Generation: 1}
				return root, []mlflow.Operation{child}
			}
			root, children := makeGraph("root", "child", "s3://bucket/source")
			root, children, err = fixture.store.PrepareOperationGraph(t.Context(), root, children)
			require.NoError(t, err)
			var reservations []mlflow.IntentResource
			require.NoError(t, fixture.db.Order("resource_id").Find(&reservations).Error)
			require.Len(t, reservations, 2)
			assert.Equal(t, int64(1), root.RequestSequence)
			require.NotNil(t, children[0].AggregateOperationUUID)
			assert.Equal(t, root.OperationUUID, *children[0].AggregateOperationUUID)
			retry, retryChildren := makeGraph("other-root", "other-child", "s3://bucket/source")
			joined, joinedChildren, err := mlflow.NewStore(fixture.db).PrepareOperationGraph(t.Context(), retry, retryChildren)
			require.NoError(t, err)
			assert.Equal(t, root.OperationUUID, joined.OperationUUID)
			require.Len(t, joinedChildren, 1)
			assert.Equal(t, children[0].OperationUUID, joinedChildren[0].OperationUUID)
			retry, retryChildren = makeGraph("different-root", "different-child", "s3://bucket/different")
			_, _, err = fixture.store.PrepareOperationGraph(t.Context(), retry, retryChildren)
			require.ErrorIs(t, err, api.ErrConflict)
			claimed, err := fixture.store.ClaimOperation(t.Context(), children[0].OperationUUID, "worker", time.Minute)
			require.NoError(t, err)
			result := `{"modelName":"graph-store","versionNumber":"42","generation":1,"status":"READY"}`
			require.NoError(t, fixture.store.CompleteOperation(t.Context(), claimed.OperationUUID, claimed.FencingToken, &result))
			var count int64
			require.NoError(t, fixture.db.Model(&mlflow.IntentResource{}).Count(&count).Error)
			assert.Equal(t, int64(2), count)
			_, err = fixture.store.PrepareOperation(t.Context(), mlflow.Operation{OperationUUID: "intruder", ResourceID: version.ID, ExpectedRevision: version.Revision, CandidateJSON: "{}", Kind: "update"})
			require.ErrorIs(t, err, api.ErrConflict)
			claimed, err = fixture.store.ClaimOperation(t.Context(), root.OperationUUID, "worker", time.Minute)
			require.NoError(t, err)
			require.NoError(t, fixture.store.PublishOperation(t.Context(), claimed.OperationUUID, claimed.FencingToken))
			require.NoError(t, fixture.db.Model(&mlflow.IntentResource{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func TestGraphPrepareLateFailureAndConcurrentInstances(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			a, err := fixture.store.ReserveObject(t.Context(), reservedModel("graph-a"))
			require.NoError(t, err)
			b, err := fixture.store.ReserveObject(t.Context(), reservedModel("graph-b"))
			require.NoError(t, err)
			node := func(id string, obj mlflow.Object) mlflow.Operation {
				return mlflow.Operation{OperationUUID: id, ResourceID: obj.ID, ExpectedRevision: obj.Revision, Kind: "update", CandidateJSON: "{}"}
			}
			_, _, err = fixture.store.PrepareOperationGraph(t.Context(), node("same-uuid", a), []mlflow.Operation{node("same-uuid", b)})
			require.Error(t, err)
			var count int64
			require.NoError(t, fixture.db.Model(&mlflow.Operation{}).Count(&count).Error)
			assert.Zero(t, count)
			require.NoError(t, fixture.db.Model(&mlflow.IntentResource{}).Count(&count).Error)
			assert.Zero(t, count)
			start := make(chan struct{})
			results := make(chan error, 2)
			for i, objects := range [][2]mlflow.Object{{a, b}, {b, a}} {
				go func(i int, objects [2]mlflow.Object) {
					<-start
					_, _, err := mlflow.NewStore(fixture.db).PrepareOperationGraph(t.Context(), node(fmt.Sprintf("root-%d", i), objects[0]), []mlflow.Operation{node(fmt.Sprintf("child-%d", i), objects[1])})
					results <- err
				}(i, objects)
			}
			close(start)
			first, second := <-results, <-results
			if first == nil {
				require.ErrorIs(t, second, api.ErrConflict)
			} else {
				require.ErrorIs(t, first, api.ErrConflict)
				require.NoError(t, second)
			}
			require.NoError(t, fixture.db.Model(&mlflow.Operation{}).Count(&count).Error)
			assert.Equal(t, int64(2), count)
		})
	}
}

func TestGraphStaleReadRevisionFencesWholeGraph(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			a, err := fixture.store.ReserveObject(t.Context(), reservedModel("guard-a"))
			require.NoError(t, err)
			b, err := fixture.store.ReserveObject(t.Context(), reservedModel("guard-b"))
			require.NoError(t, err)
			data, err := json.Marshal(mlflow.MutationCandidate{CodecVersion: 3, ReadRevisions: map[int32]int64{b.ID: b.Revision}})
			require.NoError(t, err)
			op, err := fixture.store.PrepareOperation(t.Context(), mlflow.Operation{OperationUUID: "guard", ResourceID: a.ID, ExpectedRevision: a.Revision, Kind: "update", CandidateJSON: string(data)})
			require.NoError(t, err)
			claimed, err := fixture.store.ClaimOperation(t.Context(), op.OperationUUID, "worker", time.Minute)
			require.NoError(t, err)
			require.NoError(t, fixture.db.Model(&mlflow.Object{}).Where("id = ?", b.ID).Update("revision", b.Revision+1).Error)
			_, err = fixture.store.ClaimOperation(t.Context(), op.OperationUUID, "worker", time.Minute)
			require.ErrorIs(t, err, api.ErrConflict)
			require.ErrorIs(t, fixture.store.CheckpointOperation(t.Context(), op.OperationUUID, claimed.FencingToken, mlflow.StepCreateDispatched, nil), api.ErrConflict)
			require.ErrorIs(t, fixture.store.PublishOperation(t.Context(), op.OperationUUID, claimed.FencingToken), api.ErrConflict)
		})
	}
}

func TestGraphLegacyHydration(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			model, err := fixture.store.ReserveObject(t.Context(), reservedModel("legacy-graph"))
			require.NoError(t, err)
			version, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: "1", CommittedJSON: "{}"})
			require.NoError(t, err)
			artifact, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{Kind: mlflow.KindModelArtifact, Name: "primary", CommittedJSON: "{}"})
			require.NoError(t, err)
			candidate := mlflow.MutationCandidate{CodecVersion: 2, Objects: []mlflow.ObjectChange{{ID: artifact.ID, Snapshot: mlflow.StoredSnapshot{CodecVersion: 2, Kind: mlflow.KindModelArtifact, Resource: json.RawMessage(`{"name":"primary"}`)}}}, Bindings: []mlflow.BindingChange{{Value: mlflow.VersionBinding{VersionID: version.ID, ModelID: model.ID, Phase: mlflow.PhaseReady}, ResultOperationUUID: "legacy-child"}}}
			payload, err := json.Marshal(candidate)
			require.NoError(t, err)
			root := mlflow.Operation{OperationUUID: "legacy-root", ResourceID: artifact.ID, Kind: mlflow.OperationKindVersionReplaceFanOut, CandidateJSON: string(payload), PayloadHash: "immutable-root", State: mlflow.OperationRunning, Step: mlflow.StepClaimed}
			candidate = mlflow.MutationCandidate{CodecVersion: 2, Objects: []mlflow.ObjectChange{{ID: version.ID, Snapshot: mlflow.StoredSnapshot{CodecVersion: 2, Kind: mlflow.KindModelVersion, Resource: json.RawMessage(`{"modelName":"legacy-graph","source":"s3://old/work-order"}`)}}}}
			payload, err = json.Marshal(candidate)
			require.NoError(t, err)
			child := mlflow.Operation{OperationUUID: "legacy-child", ResourceID: version.ID, Kind: mlflow.OperationKindVersionReplaceChild, CandidateJSON: string(payload), PayloadHash: "immutable-child", Generation: 2, State: mlflow.OperationSucceeded, Step: mlflow.StepNativeCreated, NativeResult: new(`{"versionNumber":"42"}`)}
			require.NoError(t, fixture.db.Create(&root).Error)
			require.NoError(t, fixture.db.Create(&child).Error)
			require.NoError(t, fixture.store.HydrateLegacyOperationGraph(t.Context()))
			require.NoError(t, fixture.store.HydrateLegacyOperationGraph(t.Context()), "hydration is idempotent")
			hydrated, err := fixture.store.GetOperation(t.Context(), child.OperationUUID)
			require.NoError(t, err)
			assert.Equal(t, child.CandidateJSON, hydrated.CandidateJSON)
			assert.Equal(t, child.PayloadHash, hydrated.PayloadHash)
			assert.Equal(t, child.NativeResult, hydrated.NativeResult)
			assert.Equal(t, mlflow.OperationSucceeded, hydrated.State)
			require.NotNil(t, hydrated.AggregateOperationUUID)
			assert.Equal(t, root.OperationUUID, *hydrated.AggregateOperationUUID)
			require.NotNil(t, hydrated.ExecutionPlanJSON)
			var reservations []mlflow.IntentResource
			require.NoError(t, fixture.db.Find(&reservations).Error)
			require.Len(t, reservations, 2)
			for _, reservation := range reservations {
				assert.Equal(t, root.OperationUUID, reservation.RootOperationUUID)
			}
			_, err = fixture.store.PrepareOperation(t.Context(), mlflow.Operation{OperationUUID: "conflict", ResourceID: version.ID, CandidateJSON: "{}"})
			require.ErrorIs(t, err, api.ErrConflict)
		})
	}
}

func TestGraphLegacyOrphansAndRetry(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, step := range []string{mlflow.StepPrepared, mlflow.StepCreateDispatched} {
				object, err := fixture.store.ReserveObject(t.Context(), reservedModel(step))
				require.NoError(t, err)
				op := mlflow.Operation{OperationUUID: step, ResourceID: object.ID, ExpectedRevision: object.Revision, Kind: mlflow.OperationKindVersionReplaceChild, State: mlflow.OperationPending, Step: step, CandidateJSON: `{"codecVersion":2}`, PayloadHash: "retained"}
				require.NoError(t, fixture.db.Create(&op).Error)
			}
			object, err := fixture.store.ReserveObject(t.Context(), reservedModel("retry"))
			require.NoError(t, err)
			parent := mlflow.Operation{OperationUUID: "original", ResourceID: object.ID, ExpectedRevision: object.Revision, Kind: mlflow.OperationKindVersionMaterialize, State: mlflow.OperationFailed, FailureCode: new(mlflow.FailureCodeOperatorRetry), Step: mlflow.StepCreateDispatched, CandidateJSON: "{}", PayloadHash: "original"}
			retry := mlflow.Operation{OperationUUID: "retry", ResourceID: object.ID, ExpectedRevision: object.Revision, Kind: mlflow.OperationKindVersionCreateRetry, State: mlflow.OperationPending, Step: mlflow.StepPrepared, CandidateJSON: "{}", ParentOperationUUID: new(parent.OperationUUID), PayloadHash: "retry"}
			require.NoError(t, fixture.db.Create(&parent).Error)
			require.NoError(t, fixture.db.Create(&retry).Error)
			require.NoError(t, fixture.store.HydrateLegacyOperationGraph(t.Context()))
			prepared, err := fixture.store.GetOperation(t.Context(), mlflow.StepPrepared)
			require.NoError(t, err)
			assert.Equal(t, mlflow.OperationFailed, prepared.State)
			assert.Equal(t, "LEGACY_ORPHAN_BEFORE_DISPATCH", *prepared.FailureCode)
			dispatched, err := fixture.store.GetOperation(t.Context(), mlflow.StepCreateDispatched)
			require.NoError(t, err)
			assert.Equal(t, mlflow.OperationPending, dispatched.State)
			assert.Equal(t, mlflow.StepCreateDispatched, dispatched.Step)
			assert.Equal(t, "LEGACY_GRAPH_QUARANTINED", *dispatched.FailureCode)
			restored, err := fixture.store.GetOperation(t.Context(), retry.OperationUUID)
			require.NoError(t, err)
			require.NotNil(t, restored.AggregateOperationUUID)
			assert.Equal(t, parent.OperationUUID, *restored.AggregateOperationUUID)
			original, err := fixture.store.GetOperation(t.Context(), parent.OperationUUID)
			require.NoError(t, err)
			assert.Equal(t, retry.OperationUUID, *original.SupersededByOperationUUID)
			_, err = fixture.store.ClaimOperation(t.Context(), retry.OperationUUID, "worker", time.Minute)
			require.NoError(t, err)
		})
	}
}

func TestGraphSnapshotCodecCompatibility(t *testing.T) {
	for _, version := range []int{2, 3} {
		public, err := mlflow.DecodePublicSnapshot(fmt.Sprintf(`{"codecVersion":%d,"kind":"model-version","resource":{"name":"logical"}}`, version))
		require.NoError(t, err)
		assert.JSONEq(t, `{"name":"logical"}`, string(public))
	}
	_, err := mlflow.DecodePublicSnapshot(`{"codecVersion":99,"resource":{}}`)
	require.ErrorIs(t, err, api.ErrBadRequest)
	public, err := mlflow.DecodePublicSnapshot(`{"name":"flat"}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"flat"}`, string(public))
}

func TestGraphRetryResumesRecordedFanOut(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "graph-resume")
			first := mustVersion(t, backend, model, "1")
			second := mustVersion(t, backend, model, "2")
			primary := sharedPrimary(t, backend, model, first, second, "s3://bucket/original")
			backend.stub.mu.Lock()
			backend.stub.commitThenFail = 1
			backend.stub.mu.Unlock()
			update := func(service *mlflow.Service) error {
				_, err := service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/replacement")}}, *first.Id)
				return err
			}
			require.ErrorIs(t, update(backend.service), api.ErrUnavailable)
			children := childOperations(t, backend)
			require.Len(t, children, 2)
			replica := backend.newBackendService(t)
			expireLeases(t, backend)
			require.NoError(t, update(replica), "same semantic request must join the persisted graph after restart")
			after := childOperations(t, backend)
			require.Len(t, after, 2)
			expectedIDs := []string{children[0].OperationUUID, children[1].OperationUUID}
			assert.ElementsMatch(t, expectedIDs, []string{after[0].OperationUUID, after[1].OperationUUID})
			for _, child := range after {
				assert.Equal(t, mlflow.OperationSucceeded, child.State)
			}
			assert.Len(t, backend.stub.versions[model.Name], 4)
			visible, err := replica.GetModelArtifactById(*primary.ModelArtifact.Id)
			require.NoError(t, err)
			assert.Equal(t, "s3://bucket/replacement", *visible.Uri)
		})
	}
}

func TestGraphBindingRevisionFencesEveryStage(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			model, err := fixture.store.ReserveObject(t.Context(), reservedModel("binding-guard"))
			require.NoError(t, err)
			version, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: "1"})
			require.NoError(t, err)
			binding := mlflow.VersionBinding{VersionID: version.ID, ModelID: model.ID, Phase: mlflow.PhasePendingSource, Revision: 1}
			require.NoError(t, fixture.db.Create(&binding).Error)
			candidate, err := json.Marshal(mlflow.MutationCandidate{CodecVersion: 3, BindingReadRevisions: map[int32]int64{version.ID: 1}})
			require.NoError(t, err)
			operation := mlflow.Operation{OperationUUID: "binding-guard", ResourceID: version.ID, ExpectedRevision: version.Revision, Kind: "guard", CandidateJSON: string(candidate)}
			_, err = fixture.store.PrepareOperation(t.Context(), operation)
			require.NoError(t, err)
			claimed, err := fixture.store.ClaimOperation(t.Context(), operation.OperationUUID, "worker", time.Minute)
			require.NoError(t, err)
			require.NoError(t, fixture.db.Model(&mlflow.VersionBinding{}).Where("id = ?", binding.ID).Update("revision", 2).Error)
			operation.OperationUUID = "second-guard"
			_, err = fixture.store.PrepareOperation(t.Context(), operation)
			require.ErrorIs(t, err, api.ErrConflict)
			_, err = fixture.store.ClaimOperation(t.Context(), claimed.OperationUUID, "worker", time.Minute)
			require.ErrorIs(t, err, api.ErrConflict)
			require.ErrorIs(t, fixture.store.CheckpointOperation(t.Context(), claimed.OperationUUID, claimed.FencingToken, mlflow.StepCreateDispatched, nil), api.ErrConflict)
			require.ErrorIs(t, fixture.store.PublishOperation(t.Context(), claimed.OperationUUID, claimed.FencingToken), api.ErrConflict)
		})
	}
}

func TestGraphAdapterCompatibility(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			adapter, err := fixture.store.InitializeAdapter(t.Context(), "compatibility")
			require.NoError(t, err)
			assert.Equal(t, 2, adapter.SchemaVersion)
			assert.Equal(t, 4, adapter.CodecVersion)
			for _, column := range []string{"schema_version", "codec_version"} {
				require.NoError(t, fixture.db.Model(&mlflow.Adapter{}).Where("id = ?", adapter.ID).Update(column, 99).Error)
				_, err := fixture.store.InitializeAdapter(t.Context(), "compatibility")
				require.ErrorIs(t, err, api.ErrConflict)
				require.NoError(t, fixture.db.Model(&mlflow.Adapter{}).Where("id = ?", adapter.ID).Updates(map[string]any{"schema_version": 2, "codec_version": 3}).Error)
			}
		})
	}
}

// The version revision guards membership of the linked-artifact projection,
// including links committed after candidate construction but before prepare.
func TestGraphPrepareRejectsChangedArtifactSet(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "graph-link-set")
			version := mustVersion(t, backend, model, "1")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/primary")
			versionID, err := strconv.Atoi(*version.Id)
			require.NoError(t, err)
			artifactID, err := strconv.Atoi(*primary.ModelArtifact.Id)
			require.NoError(t, err)
			before, err := fixture.store.GetObject(t.Context(), int32(versionID))
			require.NoError(t, err)
			artifact, err := fixture.store.GetObject(t.Context(), int32(artifactID))
			require.NoError(t, err)
			payload, err := json.Marshal(mlflow.MutationCandidate{CodecVersion: 3, ReadRevisions: map[int32]int64{before.ID: before.Revision, artifact.ID: artifact.Revision}})
			require.NoError(t, err)
			candidateReady, linkCommitted := make(chan struct{}), make(chan struct{})
			result := make(chan error, 1)
			go func() {
				close(candidateReady)
				<-linkCommitted
				_, err := fixture.store.PrepareOperation(t.Context(), mlflow.Operation{OperationUUID: "stale-link-set", ResourceID: artifact.ID, ExpectedRevision: artifact.Revision, Kind: "replace-projection", CandidateJSON: string(payload)})
				result <- err
			}()
			<-candidateReady
			additional, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Name: new("additional"), Uri: new("s3://bucket/additional")}}, *version.Id)
			close(linkCommitted)
			require.NoError(t, err)
			require.ErrorIs(t, <-result, api.ErrConflict, "a new link must invalidate the old projection input set")
			after, err := fixture.store.GetObject(t.Context(), before.ID)
			require.NoError(t, err)
			assert.Equal(t, before.Revision+1, after.Revision)
			assert.Equal(t, before.CommittedJSON, after.CommittedJSON)
			assert.Equal(t, before.LastUpdateTimeSinceEpoch, after.LastUpdateTimeSinceEpoch)
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: additional.ModelArtifact.Id}}, *version.Id)
			require.NoError(t, err)
			repeated, err := fixture.store.GetObject(t.Context(), before.ID)
			require.NoError(t, err)
			assert.Equal(t, after.Revision, repeated.Revision, "duplicate link retries must not advance membership revision")
		})
	}
}

func TestGraphRetryHydratedCodecTwoFanOut(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "graph-codec-upgrade")
			first := mustVersion(t, backend, model, "1")
			second := mustVersion(t, backend, model, "2")
			primary := sharedPrimary(t, backend, model, first, second, "s3://bucket/old")
			backend.stub.mu.Lock()
			backend.stub.commitThenFail = 1
			backend.stub.mu.Unlock()
			update := func(service *mlflow.Service) error {
				_, err := service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/new"), CustomProperties: map[string]openapi.MetadataValue{"count": {MetadataIntValue: &openapi.MetadataIntValue{MetadataType: "MetadataIntValue", IntValue: "9007199254740993"}}}}}, *first.Id)
				return err
			}
			require.ErrorIs(t, update(backend.service), api.ErrUnavailable)
			var operations []mlflow.Operation
			require.NoError(t, fixture.db.Where("state IN ?", []mlflow.OperationState{mlflow.OperationPending, mlflow.OperationRunning}).Find(&operations).Error)
			require.Len(t, operations, 3)
			require.NoError(t, fixture.db.Where("1=1").Delete(&mlflow.IntentResource{}).Error)
			recorded := map[string]string{}
			recordedHashes := map[string]string{}
			for _, operation := range operations {
				var candidate mlflow.MutationCandidate
				require.NoError(t, json.Unmarshal([]byte(operation.CandidateJSON), &candidate))
				candidate.CodecVersion = 2
				candidate.ReadRevisions = nil
				candidate.BindingReadRevisions = nil
				if operation.Kind == mlflow.OperationKindVersionReplaceChild {
					work := candidate.NativeWork
					require.NotNil(t, work)
					data, err := json.Marshal(map[string]string{"modelName": work.ModelName, "source": work.Source})
					require.NoError(t, err)
					candidate.NativeWork = nil
					candidate.Objects = []mlflow.ObjectChange{{ID: operation.ResourceID, ExpectedRevision: operation.ExpectedRevision, Snapshot: mlflow.StoredSnapshot{CodecVersion: 2, Kind: mlflow.KindModelVersion, Resource: data}}}
				}
				for i := range candidate.Objects {
					candidate.Objects[i].Snapshot.CodecVersion = 2
				}
				data, err := json.Marshal(candidate)
				require.NoError(t, err)
				recorded[operation.OperationUUID] = string(data)
				recordedHashes[operation.OperationUUID] = fmt.Sprintf("%x", sha256.Sum256(data))
				require.NoError(t, fixture.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", operation.OperationUUID).Updates(map[string]any{"candidate_json": string(data), "payload_hash": recordedHashes[operation.OperationUUID], "request_hash": "", "aggregate_operation_uuid": nil, "execution_plan_json": nil}).Error)
			}
			adapter, err := fixture.store.GetAdapter(t.Context())
			require.NoError(t, err)
			_, err = fixture.store.InitializeAdapter(t.Context(), adapter.TargetFingerprint)
			require.NoError(t, err)
			replica := backend.newBackendService(t)
			expireLeases(t, backend)
			require.NoError(t, update(replica), "the same typed desired request must join its hydrated codec2 graph")
			for id, payload := range recorded {
				op, err := fixture.store.GetOperation(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, payload, op.CandidateJSON)
				assert.Equal(t, recordedHashes[id], op.PayloadHash)
				assert.Equal(t, mlflow.OperationSucceeded, op.State)
			}
			assert.Len(t, childOperations(t, backend), 2)
			assert.Len(t, backend.stub.versions[model.Name], 4)
		})
	}
}

func TestGraphHydrationQuarantinesStaleDispatchWithReservations(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			object, err := fixture.store.ReserveObject(t.Context(), reservedModel("stale-dispatched"))
			require.NoError(t, err)
			consumed, err := fixture.store.ReserveObject(t.Context(), reservedModel("stale-consumed"))
			require.NoError(t, err)
			payload, err := json.Marshal(mlflow.MutationCandidate{CodecVersion: 2, ReadRevisions: map[int32]int64{consumed.ID: consumed.Revision}})
			require.NoError(t, err)
			old := mlflow.Operation{OperationUUID: "old-dispatched", ResourceID: object.ID, ExpectedRevision: object.Revision - 1, Kind: mlflow.OperationKindVersionMaterialize, CandidateJSON: string(payload), PayloadHash: "immutable", State: mlflow.OperationRunning, Step: mlflow.StepCreateDispatched, NativeResult: new("uncertain-evidence")}
			require.NoError(t, fixture.db.Create(&old).Error)
			require.NoError(t, fixture.store.HydrateLegacyOperationGraph(t.Context()))
			after, err := fixture.store.GetOperation(t.Context(), old.OperationUUID)
			require.NoError(t, err)
			require.NotNil(t, after.FailureCode)
			assert.Equal(t, "LEGACY_GRAPH_QUARANTINED", *after.FailureCode)
			for _, resource := range []mlflow.Object{object, consumed} {
				_, err = fixture.store.PrepareOperation(t.Context(), mlflow.Operation{OperationUUID: fmt.Sprintf("new-%d", resource.ID), ResourceID: resource.ID, ExpectedRevision: resource.Revision, Kind: "update", CandidateJSON: "{}"})
				assert.ErrorIs(t, err, api.ErrConflict, "unresolved dispatch must keep known dependency %d reserved", resource.ID)
			}
			assert.Equal(t, old.CandidateJSON, after.CandidateJSON)
			assert.Equal(t, old.PayloadHash, after.PayloadHash)
			assert.Equal(t, old.NativeResult, after.NativeResult)
			assert.Equal(t, old.Step, after.Step)
		})
	}
}
