package mlflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func uncertainChild(t *testing.T, backend *backendFixture) (mlflow.Operation, mlflow.Operation, *openapi.Artifact, *openapi.ModelVersion) {
	t.Helper()
	model := mustModel(t, backend, "operator-child")
	first := mustVersion(t, backend, model, "one")
	second := mustVersion(t, backend, model, "two")
	primary := sharedPrimary(t, backend, model, first, second, "s3://old")
	backend.stub.mu.Lock()
	backend.stub.commitThenFail = 1
	backend.stub.searchPageSize = 1
	backend.stub.mu.Unlock()
	_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Name: new("primary"), Uri: new("s3://new")}}, *first.Id)
	require.ErrorIs(t, err, api.ErrUnavailable)
	expireLeases(t, backend)
	var child, root mlflow.Operation
	require.NoError(t, backend.db.Where("kind = ? AND step = ?", mlflow.OperationKindVersionReplaceChild, mlflow.StepCreateDispatched).First(&child).Error)
	require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionReplaceFanOut).First(&root).Error)
	return child, root, primary, first
}

func TestOperatorRetryLateSupersededCompletion(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			child, root, _, _ := uncertainChild(t, backend)
			backend.stub.mu.Lock()
			late := backend.stub.versions["operator-child"]["3"]
			delete(backend.stub.versions["operator-child"], "3")
			backend.stub.mu.Unlock()
			require.NoError(t, backend.service.RetryCreate(t.Context(), child.OperationUUID))
			require.ErrorIs(t, backend.store.CompleteOperation(t.Context(), child.OperationUUID, child.FencingToken, new(`{"modelName":"operator-child","versionNumber":"3","source":"s3://new","generation":2,"status":"READY"}`)), api.ErrConflict)
			require.NoError(t, backend.service.ApplyOperation(t.Context(), root.OperationUUID))
			require.NoError(t, backend.service.Refresh(t.Context()))
			backend.stub.mu.Lock()
			backend.stub.versions["operator-child"]["3"] = late
			backend.stub.mu.Unlock()
			restarted := backend.newBackendService(t)
			require.NoError(t, restarted.Refresh(t.Context()))
			require.NoError(t, restarted.Refresh(t.Context()))
			var retained []mlflow.NativeDisposition
			require.NoError(t, backend.db.Where("operation_uuid = ?", child.OperationUUID).Find(&retained).Error)
			require.Len(t, retained, 1)
			assert.Equal(t, "3", retained[0].NativeVersionNumber)
			assert.Equal(t, "UNSELECTED_DUPLICATE", retained[0].Disposition)
			assert.Zero(t, backend.stub.countRequests("DELETE"))
		})
	}
}

// A bind's native verification can race a worker claim. The SQL decision must
// recheck that lease, retain all evidence, and allow later explicit resolution.
func TestOperatorChildBindWorkerRace(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			child, root, _, _ := uncertainChild(t, backend)
			duplicateChildResult(t, backend)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			selector := aliasTestService(t, backend, aliasTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/model-versions/get") {
					once.Do(func() { close(entered); <-release })
				}
				return http.DefaultTransport.RoundTrip(r)
			}))
			bound := make(chan error, 1)
			go func() { bound <- selector.BindNativeIdentity(t.Context(), child.OperationUUID, "operator-child", "99") }()
			<-entered
			require.ErrorIs(t, backend.service.ApplyOperation(t.Context(), root.OperationUUID), api.ErrConflict)
			before := operatorSQLState(t, backend)
			close(release)
			require.ErrorIs(t, <-bound, api.ErrConflict)
			assert.JSONEq(t, before, operatorSQLState(t, backend))
			expireLeases(t, backend)
			require.NoError(t, selector.BindNativeIdentity(t.Context(), child.OperationUUID, "operator-child", "99"))
			published := make(chan error, 1)
			go func() { published <- backend.service.ApplyOperation(t.Context(), root.OperationUUID) }()
			require.NoError(t, selector.BindNativeIdentity(t.Context(), child.OperationUUID, "operator-child", "99"))
			require.NoError(t, <-published)
			assert.Zero(t, backend.stub.countRequests("DELETE"))
		})
	}
}

func TestOperatorSelectedCheckpointWorkerRecovery(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "selected-recovery")
			version := mustVersion(t, backend, model, "one")
			backend.stub.commitThenFail = 1
			_, err := attachWeights(t, backend.service, version, "primary", "s3://new")
			require.ErrorIs(t, err, api.ErrUnavailable)
			expireLeases(t, backend)
			var op mlflow.Operation
			require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionMaterialize).First(&op).Error)
			var reads atomic.Int32
			selector := aliasTestService(t, backend, aliasTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/model-versions/get") && reads.Add(1) == 2 {
					return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			}))
			require.ErrorIs(t, selector.BindNativeIdentity(t.Context(), op.OperationUUID, model.Name, "1"), api.ErrUnavailable)
			selected, err := backend.store.GetOperation(t.Context(), op.OperationUUID)
			require.NoError(t, err)
			require.NotNil(t, selected.NativeResult)
			expireLeases(t, backend)
			restarted := backend.newBackendService(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- restarted.Run(ctx) }()
			require.Eventually(t, func() bool {
				current, err := backend.store.GetOperation(t.Context(), op.OperationUUID)
				return err == nil && current.State == mlflow.OperationSucceeded
			}, 3*time.Second, 10*time.Millisecond, "selected work must remain recoverable by the worker")
			cancel()
			require.NoError(t, <-done)
			assert.Equal(t, 1, len(backend.stub.versions[model.Name]))
		})
	}
}

func operatorSQLState(t *testing.T, backend *backendFixture, tables ...string) string {
	t.Helper()
	state := map[string]any{}
	if len(tables) == 0 {
		tables = []string{"MLflowObject", "MLflowOperation", "MLflowVersionBinding", "MLflowVersionHistory", "MLflowIntentResource", "MLflowNativeDisposition"}
	}
	for _, table := range tables {
		var rows []map[string]any
		require.NoError(t, backend.db.Table(table).Find(&rows).Error)
		state[table] = rows
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	return string(data)
}

func TestOperatorSelectionSurvivesAuthorizationStop(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, legacyMarker := range []bool{false, true} {
				t.Run(strconv.FormatBool(legacyMarker), func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					model := mustModel(t, backend, "selected-auth-stop")
					version := mustVersion(t, backend, model, "one")
					oldCheckpoint, err := json.Marshal(mlflow.NativeBindingResult{ModelName: model.Name, VersionNumber: "1", Source: "s3://", Generation: 1, Status: "READY"})
					require.NoError(t, err)
					source := "s3://" + strings.Repeat("a", 512-len(oldCheckpoint))
					backend.stub.commitThenFail = 1
					_, err = attachWeights(t, backend.service, version, "primary", source)
					require.ErrorIs(t, err, api.ErrUnavailable)
					expireLeases(t, backend)
					op := materializeOperationOf(t, backend)
					var reads atomic.Int32
					selector := aliasTestService(t, backend, aliasTransport(func(r *http.Request) (*http.Response, error) {
						if strings.HasSuffix(r.URL.Path, "/model-versions/get") && reads.Add(1) > 1 {
							return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
						}
						return http.DefaultTransport.RoundTrip(r)
					}))
					require.ErrorIs(t, selector.BindNativeIdentity(t.Context(), op.OperationUUID, model.Name, "1"), api.ErrForbidden)
					selected, err := backend.store.GetOperation(t.Context(), op.OperationUUID)
					require.NoError(t, err)
					require.NotNil(t, selected.NativeResult)
					if legacyMarker {
						var checkpoint map[string]any
						require.NoError(t, json.Unmarshal([]byte(*selected.NativeResult), &checkpoint))
						delete(checkpoint, "explicitSelection")
						encoded, err := json.Marshal(checkpoint)
						require.NoError(t, err)
						require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", op.OperationUUID).Update("native_result", string(encoded)).Error)
					}
					expireLeases(t, backend)
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					done := make(chan error, 1)
					go func() { done <- selector.Run(ctx) }()
					require.Eventually(t, func() bool {
						current, err := backend.store.GetOperation(t.Context(), op.OperationUUID)
						return err == nil && current.FailureCode != nil && *current.FailureCode == "DEFINITIVE_AUTH"
					}, 3*time.Second, 10*time.Millisecond)
					cancel()
					require.NoError(t, <-done)
					expireLeases(t, backend)
					restarted := backend.newBackendService(t)
					_, err = attachWeights(t, restarted, version, "primary", source)
					require.NoError(t, err)
					published, err := backend.store.GetOperation(t.Context(), op.OperationUUID)
					require.NoError(t, err)
					var retainedChoice mlflow.NativeBindingResult
					require.NoError(t, json.Unmarshal([]byte(*published.NativeResult), &retainedChoice))
					assert.True(t, retainedChoice.ExplicitSelection)
					assert.Equal(t, "1", *bindingOf(t, backend, *version.Id).NativeVersionNumber)
					late := maps.Clone(backend.stub.versions[model.Name]["1"].(map[string]any))
					late["version"] = "99"
					backend.stub.versions[model.Name]["99"] = late
					require.NoError(t, restarted.Refresh(t.Context()), "the durable choice must audit late exact alternatives after an authorization stop")
					var disposition mlflow.NativeDisposition
					require.NoError(t, backend.db.Where("operation_uuid = ? AND native_version_number = ?", op.OperationUUID, "99").First(&disposition).Error)
					assert.Equal(t, "UNSELECTED_DUPLICATE", disposition.Disposition)
					require.NoError(t, restarted.BindNativeIdentity(t.Context(), op.OperationUUID, model.Name, "1"))
					assert.Equal(t, "1", *bindingOf(t, backend, *version.Id).NativeVersionNumber)
					assert.Zero(t, backend.stub.countRequests("DELETE"))
				})
			}
		})
	}
}

func TestOperatorCompletedLegacyRetryUpgrade(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "completed-legacy-retry")
			version := mustVersion(t, backend, model, "one")
			backend.stub.commitThenFail = 1
			_, err := attachWeights(t, backend.service, version, "weights", "s3://source")
			require.ErrorIs(t, err, api.ErrUnavailable)
			original := materializeOperationOf(t, backend)
			expireLeases(t, backend)
			late := backend.stub.versions[model.Name]["1"]
			delete(backend.stub.versions[model.Name], "1")
			require.NoError(t, backend.service.RetryCreate(t.Context(), original.OperationUUID))
			var retry mlflow.Operation
			require.NoError(t, backend.db.Where("parent_operation_uuid = ?", original.OperationUUID).First(&retry).Error)
			require.Equal(t, mlflow.OperationSucceeded, retry.State)
			before := map[string]mlflow.Operation{}
			for _, id := range []string{original.OperationUUID, retry.OperationUUID} {
				op, err := backend.store.GetOperation(t.Context(), id)
				require.NoError(t, err)
				var candidate mlflow.MutationCandidate
				require.NoError(t, json.Unmarshal([]byte(op.CandidateJSON), &candidate))
				candidate.CodecVersion, candidate.NativeWork = 2, nil
				candidate.ReadRevisions, candidate.BindingReadRevisions = nil, nil
				for i := range candidate.Objects {
					candidate.Objects[i].Snapshot.CodecVersion = 2
				}
				encoded, err := json.Marshal(candidate)
				require.NoError(t, err)
				require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", id).Updates(map[string]any{"candidate_json": string(encoded), "payload_hash": "immutable-" + id, "request_hash": "", "request_sequence": 0, "aggregate_operation_uuid": nil, "superseded_by_operation_uuid": nil, "execution_plan_json": nil}).Error)
				before[id], err = backend.store.GetOperation(t.Context(), id)
				require.NoError(t, err)
			}
			publishedTables := []string{"MLflowObject", "MLflowVersionBinding", "MLflowVersionHistory", "MLflowArtifactLink", "MLflowIntentResource"}
			published := operatorSQLState(t, backend, publishedTables...)
			for _, invalid := range []string{"parent-reason", "semantic-intent"} {
				t.Run(invalid, func(t *testing.T) {
					if invalid == "parent-reason" {
						require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", original.OperationUUID).Update("failure_code", "UNEXPLAINED").Error)
					} else {
						changed := strings.ReplaceAll(before[retry.OperationUUID].CandidateJSON, "s3://source", "s3://forged")
						require.NotEqual(t, before[retry.OperationUUID].CandidateJSON, changed)
						require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", retry.OperationUUID).Update("candidate_json", changed).Error)
					}
					require.NoError(t, backend.store.HydrateLegacyOperationGraph(t.Context()))
					unresolved, err := backend.store.ResolveAttempt(t.Context(), original.OperationUUID)
					require.NoError(t, err)
					assert.Equal(t, original.OperationUUID, unresolved.OperationUUID)
					assert.Empty(t, unresolved.RequestHash)
					assert.JSONEq(t, published, operatorSQLState(t, backend, publishedTables...))
					for id, prior := range before {
						require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", id).Select("*").Updates(prior).Error)
					}
				})
			}
			require.NoError(t, backend.store.HydrateLegacyOperationGraph(t.Context()))
			selected, err := backend.store.ResolveAttempt(t.Context(), original.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, retry.OperationUUID, selected.OperationUUID)
			require.NotEmpty(t, selected.RequestHash)
			require.NoError(t, backend.store.HydrateLegacyOperationGraph(t.Context()))
			assert.JSONEq(t, published, operatorSQLState(t, backend, publishedTables...))
			for id, prior := range before {
				after, err := backend.store.GetOperation(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, prior.CandidateJSON, after.CandidateJSON)
				assert.Equal(t, prior.PayloadHash, after.PayloadHash)
				assert.Equal(t, prior.NativeResult, after.NativeResult)
				assert.Equal(t, prior.State, after.State)
				assert.Equal(t, prior.Step, after.Step)
				assert.Equal(t, prior.FencingToken, after.FencingToken)
				assert.Equal(t, prior.Attempts, after.Attempts)
				assert.Equal(t, selected.RequestHash, after.RequestHash)
			}
			restarted := backend.newBackendService(t)
			require.NoError(t, restarted.ApplyOperation(t.Context(), original.OperationUUID))
			backend.stub.versions[model.Name]["1"] = late
			require.NoError(t, restarted.Refresh(t.Context()))
			var disposition mlflow.NativeDisposition
			require.NoError(t, backend.db.Where("operation_uuid = ?", original.OperationUUID).First(&disposition).Error)
			assert.Equal(t, "UNSELECTED_DUPLICATE", disposition.Disposition)
			assert.Equal(t, "1", disposition.NativeVersionNumber)
			assert.Equal(t, "2", *bindingOf(t, backend, *version.Id).NativeVersionNumber)
			assert.Zero(t, backend.stub.countRequests("DELETE"))
		})
	}
}

func TestOperatorChildBindRejectionsAreReadOnly(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			child, _, _, _ := uncertainChild(t, backend)
			for _, invalid := range []string{"adapter", "logical", "operation", "missing-operation", "source", "generation", "missing-generation", "parent", "foreign-lease", "bound", "terminal"} {
				t.Run(invalid, func(t *testing.T) {
					native := backend.stub.versionRecord("operator-child", "3")
					original := maps.Clone(native)
					original["tags"] = maps.Clone(native["tags"].(map[string]string))
					tags := native["tags"].(map[string]string)
					model := "operator-child"
					switch invalid {
					case "adapter":
						tags["rhoai.adapter"] = "foreign"
					case "logical":
						tags["rhoai.logicalId"] = "999999999"
					case "operation":
						tags["rhoai.operation"] = "other-operation"
					case "missing-operation":
						delete(tags, "rhoai.operation")
					case "source":
						native["source"] = "s3://other"
					case "generation":
						tags["rhoai.generation"] = "9"
					case "missing-generation":
						delete(tags, "rhoai.generation")
					case "parent":
						model = "other-model"
					case "foreign-lease":
						_, err := backend.store.ClaimOperation(t.Context(), child.OperationUUID, "foreign-worker", mlflow.DefaultRequestTimeout)
						require.NoError(t, err)
					case "bound":
						require.NoError(t, backend.db.Model(&mlflow.VersionBinding{}).Where("version_id = ?", child.ResourceID).Update("native_version_number", "3").Error)
					case "terminal":
						require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", child.OperationUUID).Update("state", mlflow.OperationFailed).Error)
					}
					before := operatorSQLState(t, backend)
					require.ErrorIs(t, backend.service.BindNativeIdentity(t.Context(), child.OperationUUID, model, "3"), api.ErrConflict)
					assert.JSONEq(t, before, operatorSQLState(t, backend))
					backend.stub.versions["operator-child"]["3"] = original
					require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", child.OperationUUID).Select("*").Updates(child).Error)
					if invalid == "bound" {
						require.NoError(t, backend.db.Model(&mlflow.VersionBinding{}).Where("version_id = ?", child.ResourceID).Update("native_version_number", "1").Error)
					}
				})
			}
		})
	}
}

func TestOrphanResolutionLifecycle(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, scenario := range []string{"before-dispatch", "dispatched-match", "dispatched-no-match"} {
				t.Run(scenario, func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					model := mustModel(t, backend, "orphan-lifecycle")
					version := mustVersion(t, backend, model, "v1")
					mustPrimaryArtifact(t, backend, version, "s3://old")
					binding := bindingOf(t, backend, *version.Id)
					object, err := backend.store.GetObject(t.Context(), binding.VersionID)
					require.NoError(t, err)
					candidate := mlflow.MutationCandidate{CodecVersion: 2, Objects: []mlflow.ObjectChange{{ID: object.ID, ExpectedRevision: object.Revision, Snapshot: mlflow.StoredSnapshot{CodecVersion: 2, Kind: mlflow.KindModelVersion, Resource: json.RawMessage(`{"modelName":"orphan-lifecycle","source":"s3://abandoned"}`)}}}}
					encoded, err := json.Marshal(candidate)
					require.NoError(t, err)
					op := mlflow.Operation{OperationUUID: "legacy-orphan", ResourceID: object.ID, ExpectedRevision: object.Revision, Kind: mlflow.OperationKindVersionReplaceChild, CandidateJSON: string(encoded), PayloadHash: "immutable-lost-root", Generation: 2, State: mlflow.OperationRunning, Step: mlflow.StepCreateDispatched}
					if scenario == "before-dispatch" {
						op.Step = mlflow.StepPrepared
						op.CandidateJSON = `{}`
					}
					require.NoError(t, backend.db.Create(&op).Error)
					require.NoError(t, backend.store.HydrateLegacyOperationGraph(t.Context()))
					addNative := func() {
						backend.stub.forgeVersion(model.Name, "99", map[string]string{"rhoai.adapter": adapterUUIDOf(t, backend), "rhoai.operation": op.OperationUUID, "rhoai.logicalId": strconv.Itoa(int(object.ID)), "rhoai.generation": "2"})
						backend.stub.versionRecord(model.Name, "99")["source"] = "s3://abandoned"
					}
					if scenario == "dispatched-match" {
						addNative()
					}
					require.NoError(t, backend.service.ResolveOrphan(t.Context(), op.OperationUUID, true))
					require.Equal(t, binding, bindingOf(t, backend, *version.Id))
					resolved, err := backend.store.GetOperation(t.Context(), op.OperationUUID)
					require.NoError(t, err)
					assert.Equal(t, mlflow.OperationFailed, resolved.State)
					assert.Equal(t, op.CandidateJSON, resolved.CandidateJSON)
					assert.Nil(t, resolved.NativeResult)
					if scenario != "before-dispatch" {
						addNative() // also covers an arrival after a zero-match decision
						require.NoError(t, backend.store.HydrateLegacyOperationGraph(t.Context()))
						restarted := backend.newBackendService(t)
						require.NoError(t, restarted.Refresh(t.Context()))
						var dispositions []mlflow.NativeDisposition
						require.NoError(t, backend.db.Find(&dispositions).Error)
						require.Len(t, dispositions, 1)
						assert.Equal(t, "ORPHAN_RETAINED", dispositions[0].Disposition)
						assert.Equal(t, binding, bindingOf(t, backend, *version.Id))
					}
					var owned int64
					require.NoError(t, backend.db.Model(&mlflow.IntentResource{}).Where("root_operation_uuid = ?", op.OperationUUID).Count(&owned).Error)
					assert.Zero(t, owned)
					assert.Zero(t, backend.stub.countRequests("DELETE"))
				})
			}
		})
	}
}

func duplicateChildResult(t *testing.T, backend *backendFixture) {
	t.Helper()
	backend.stub.mu.Lock()
	defer backend.stub.mu.Unlock()
	copy := maps.Clone(backend.stub.versions["operator-child"]["3"].(map[string]any))
	copy["tags"] = maps.Clone(copy["tags"].(map[string]string))
	copy["version"] = "99"
	backend.stub.versions["operator-child"]["99"] = copy
}

// Removing typed child handling, checkpointed selection, or retry-chain
// resolution must leave the root unpublishable and fail these assertions.
func TestOperatorChildResolution(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, action := range []string{"bind", "retry", "legacy-retry"} {
				t.Run(action, func(t *testing.T) {
					cleanupMLflowTables(t, fixture.db)
					backend := newBackendFixture(t, fixture)
					child, root, primary, first := uncertainChild(t, backend)
					if action == "legacy-retry" {
						var legacy mlflow.MutationCandidate
						require.NoError(t, json.Unmarshal([]byte(child.CandidateJSON), &legacy))
						legacy.CodecVersion, legacy.NativeWork = 2, nil
						legacy.Objects = []mlflow.ObjectChange{{ID: child.ResourceID, ExpectedRevision: child.ExpectedRevision, Snapshot: mlflow.StoredSnapshot{CodecVersion: 2, Kind: mlflow.KindModelVersion, Resource: json.RawMessage(`{"modelName":"operator-child","source":"s3://new"}`)}}}
						encoded, err := json.Marshal(legacy)
						require.NoError(t, err)
						require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", child.OperationUUID).Update("candidate_json", string(encoded)).Error)
					}
					if action == "bind" {
						require.NoError(t, backend.service.BindNativeIdentity(t.Context(), child.OperationUUID, "operator-child", "3"))
					} else {
						require.NoError(t, backend.service.RetryCreate(t.Context(), child.OperationUUID))
						var previous mlflow.Operation
						require.NoError(t, backend.db.Where("operation_uuid = ?", child.OperationUUID).First(&previous).Error)
						require.NotNil(t, previous.SupersededByOperationUUID)
						var retry mlflow.Operation
						require.NoError(t, backend.db.Where("operation_uuid = ?", *previous.SupersededByOperationUUID).First(&retry).Error)
						assert.Equal(t, mlflow.OperationKindVersionReplaceChild, retry.Kind)
						assert.Equal(t, root.OperationUUID, *retry.AggregateOperationUUID)
						assert.Equal(t, mlflow.OperationSucceeded, retry.State)
					}
					var pending mlflow.Operation
					require.NoError(t, backend.db.Where("operation_uuid = ?", root.OperationUUID).First(&pending).Error)
					assert.Equal(t, root.CandidateJSON, pending.CandidateJSON)
					assert.NotEqual(t, mlflow.OperationSucceeded, pending.State)
					var owned int64
					require.NoError(t, backend.db.Model(&mlflow.IntentResource{}).Where("root_operation_uuid = ?", root.OperationUUID).Count(&owned).Error)
					assert.Positive(t, owned)
					restarted := backend.newBackendService(t)
					expireLeases(t, backend)
					// The identical public request must join the same root and follow
					// the original immutable child reference across the retry.
					_, err := restarted.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Name: new("primary"), Uri: new("s3://new")}}, *first.Id)
					require.NoError(t, err)
					assert.Equal(t, int64(2), bindingOf(t, backend, *first.Id).ActiveGeneration)
					require.NoError(t, restarted.Refresh(t.Context()))
					assert.Zero(t, backend.stub.countRequests("DELETE"))
				})
			}
		})
	}
}

func TestOperatorDuplicateSelection(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			child, root, _, first := uncertainChild(t, backend)
			duplicateChildResult(t, backend)
			require.ErrorIs(t, backend.service.ApplyOperation(t.Context(), root.OperationUUID), api.ErrConflict)
			expireLeases(t, backend)
			// Fail after inserting alternatives but before the selected-result
			// checkpoint. The decision and its lease must roll back together.
			before := operatorSQLState(t, backend)
			injected := errors.New("selection checkpoint failed")
			const callback = "test:selection-checkpoint-failure"
			require.NoError(t, backend.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if values, ok := tx.Statement.Dest.(map[string]any); ok && tx.Statement.Table == "MLflowOperation" && values["native_result"] != nil {
					_ = tx.AddError(injected) // GORM returns the callback's statement error.
				}
			}))
			err := backend.service.BindNativeIdentity(t.Context(), child.OperationUUID, "operator-child", "99")
			require.NoError(t, backend.db.Callback().Update().Remove(callback))
			require.ErrorIs(t, err, injected)
			assert.JSONEq(t, before, operatorSQLState(t, backend))
			require.NoError(t, backend.service.BindNativeIdentity(t.Context(), child.OperationUUID, "operator-child", "99"))
			require.NoError(t, backend.service.BindNativeIdentity(t.Context(), child.OperationUUID, "operator-child", "99"))
			require.ErrorIs(t, backend.service.BindNativeIdentity(t.Context(), child.OperationUUID, "operator-child", "3"), api.ErrConflict)
			var dispositions []struct{ NativeVersionNumber, Disposition string }
			require.NoError(t, backend.db.Table("MLflowNativeDisposition").Find(&dispositions).Error)
			require.Len(t, dispositions, 1)
			assert.Equal(t, "3", dispositions[0].NativeVersionNumber)
			assert.Equal(t, "UNSELECTED_DUPLICATE", dispositions[0].Disposition)
			restarted := backend.newBackendService(t)
			expireLeases(t, backend)
			require.NoError(t, restarted.ApplyOperation(t.Context(), root.OperationUUID))
			require.NoError(t, restarted.Refresh(t.Context()))
			assert.Equal(t, int64(2), bindingOf(t, backend, *first.Id).ActiveGeneration)
			// A late old worker may move the alias to the retained alternative.
			// The recorded disposition proves ownership and permits repair.
			var chosen mlflow.VersionBinding
			require.NoError(t, backend.db.Where("version_id = ?", child.ResourceID).First(&chosen).Error)
			backend.stub.aliases("operator-child")[chosen.Alias] = "3"
			require.NoError(t, restarted.ReconcileAliases(t.Context()))
			assert.Equal(t, "99", backend.stub.aliases("operator-child")[chosen.Alias])
			var other mlflow.VersionBinding
			require.NoError(t, backend.db.Where("model_id = ? AND version_id <> ?", chosen.ModelID, child.ResourceID).First(&other).Error)
			backend.stub.aliases("operator-child")[other.Alias] = "3"
			require.ErrorIs(t, restarted.ReconcileAliases(t.Context()), api.ErrConflict)
			assert.Equal(t, "3", backend.stub.aliases("operator-child")[other.Alias], "retained identity cannot authorize another logical version")
			backend.stub.aliases("operator-child")[other.Alias] = *other.NativeVersionNumber
			// Reusing the retained number with contradictory evidence cannot
			// hide it from refresh or authorize an alias overwrite.
			backend.stub.versionRecord("operator-child", "3")["source"] = "s3://foreign"
			backend.stub.aliases("operator-child")[chosen.Alias] = "3"
			require.ErrorIs(t, restarted.Refresh(t.Context()), api.ErrConflict)
			require.ErrorIs(t, restarted.ReconcileAliases(t.Context()), api.ErrConflict)
			assert.Equal(t, "3", backend.stub.aliases("operator-child")[chosen.Alias])
			assert.Zero(t, backend.stub.countRequests("DELETE"))
		})
	}
}

func TestOperatorDuplicateArrivesAfterSelectedPublication(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			child, root, _, _ := uncertainChild(t, backend)
			require.NoError(t, backend.service.BindNativeIdentity(t.Context(), child.OperationUUID, "operator-child", "3"))
			require.NoError(t, backend.service.ApplyOperation(t.Context(), root.OperationUUID))
			completed, err := backend.store.GetOperation(t.Context(), child.OperationUUID)
			require.NoError(t, err)
			var choice mlflow.NativeBindingResult
			require.NoError(t, json.Unmarshal([]byte(*completed.NativeResult), &choice))
			assert.True(t, choice.ExplicitSelection, "child completion must retain the operator decision")
			var binding mlflow.VersionBinding
			require.NoError(t, backend.db.Where("version_id = ?", child.ResourceID).First(&binding).Error)
			duplicateChildResult(t, backend) // exact alternative arrives after publication
			restarted := backend.newBackendService(t)
			require.NoError(t, restarted.Refresh(t.Context()))
			var retained []mlflow.NativeDisposition
			require.NoError(t, backend.db.Where("operation_uuid = ?", child.OperationUUID).Find(&retained).Error)
			require.Len(t, retained, 1)
			assert.Equal(t, "99", retained[0].NativeVersionNumber)
			assert.Equal(t, "UNSELECTED_DUPLICATE", retained[0].Disposition)
			assert.Equal(t, child.OperationUUID, retained[0].ResolvingOperationUUID)
			backend.stub.aliases("operator-child")[binding.Alias] = "99"
			require.NoError(t, restarted.ReconcileAliases(t.Context()))
			assert.Equal(t, "3", backend.stub.aliases("operator-child")[binding.Alias])
			var after mlflow.VersionBinding
			require.NoError(t, backend.db.Where("version_id = ?", child.ResourceID).First(&after).Error)
			assert.Equal(t, binding, after)
			// Completion alone is not an operator selection: a late duplicate
			// of the automatically completed sibling remains unexplained.
			backend.stub.mu.Lock()
			unknown := maps.Clone(backend.stub.versions["operator-child"]["4"].(map[string]any))
			unknown["version"] = "100"
			unknown["tags"] = maps.Clone(unknown["tags"].(map[string]string))
			backend.stub.versions["operator-child"]["100"] = unknown
			backend.stub.mu.Unlock()
			require.ErrorIs(t, restarted.Refresh(t.Context()), api.ErrConflict)
			assert.Zero(t, backend.stub.countRequests("DELETE"))
		})
	}
}

func TestOperatorLegacyRetryUpgradeResolvesOriginalIntent(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "legacy-retry-upgrade")
			version := mustVersion(t, backend, model, "one")
			backend.stub.commitThenFail = 1
			_, err := attachWeights(t, backend.service, version, "weights", "s3://source")
			require.ErrorIs(t, err, api.ErrUnavailable)
			original := materializeOperationOf(t, backend)
			expireLeases(t, backend)
			backend.stub.failCreates = 1
			require.ErrorIs(t, backend.service.RetryCreate(t.Context(), original.OperationUUID), api.ErrUnavailable)
			var retry mlflow.Operation
			require.NoError(t, backend.db.Where("parent_operation_uuid = ?", original.OperationUUID).First(&retry).Error)
			// Reproduce persisted pre-graph retry rows: the original failed
			// attempt and retry have only immutable work and the parent edge.
			before := map[string]mlflow.Operation{}
			for _, id := range []string{original.OperationUUID, retry.OperationUUID} {
				op, err := backend.store.GetOperation(t.Context(), id)
				require.NoError(t, err)
				var candidate mlflow.MutationCandidate
				require.NoError(t, json.Unmarshal([]byte(op.CandidateJSON), &candidate))
				candidate.CodecVersion, candidate.NativeWork = 2, nil
				candidate.ReadRevisions, candidate.BindingReadRevisions = nil, nil
				for i := range candidate.Objects {
					candidate.Objects[i].Snapshot.CodecVersion = 2
				}
				encoded, err := json.Marshal(candidate)
				require.NoError(t, err)
				require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", id).Updates(map[string]any{"candidate_json": string(encoded), "payload_hash": "immutable-" + id, "request_hash": "", "request_sequence": 0, "aggregate_operation_uuid": nil, "superseded_by_operation_uuid": nil, "execution_plan_json": nil}).Error)
				before[id], err = backend.store.GetOperation(t.Context(), id)
				require.NoError(t, err)
			}
			require.NoError(t, backend.db.Where("root_operation_uuid = ?", original.OperationUUID).Delete(&mlflow.IntentResource{}).Error)
			backend.stub.mu.Lock()
			native := maps.Clone(backend.stub.versions[model.Name]["1"].(map[string]any))
			native["version"] = "2"
			tags := maps.Clone(native["tags"].(map[string]string))
			tags["rhoai.operation"] = retry.OperationUUID
			native["tags"] = tags
			backend.stub.versions[model.Name]["2"] = native
			backend.stub.mu.Unlock()
			require.NoError(t, backend.store.HydrateLegacyOperationGraph(t.Context()))
			selected, err := backend.store.ResolveAttempt(t.Context(), original.OperationUUID)
			require.NoError(t, err)
			assert.Equal(t, retry.OperationUUID, selected.OperationUUID)
			require.NoError(t, backend.store.HydrateLegacyOperationGraph(t.Context()), "repeated upgrade preserves the canonical chain")
			for id, prior := range before {
				after, err := backend.store.GetOperation(t.Context(), id)
				require.NoError(t, err)
				assert.Equal(t, prior.CandidateJSON, after.CandidateJSON)
				assert.Equal(t, prior.PayloadHash, after.PayloadHash)
				assert.Equal(t, prior.NativeResult, after.NativeResult)
				assert.Equal(t, prior.Step, after.Step)
				assert.Equal(t, prior.FencingToken, after.FencingToken)
				assert.Equal(t, prior.Attempts, after.Attempts)
				assert.Equal(t, selected.RequestHash, after.RequestHash)
			}
			expireLeases(t, backend)
			restarted := backend.newBackendService(t)
			// The original API request must rejoin the selected legacy retry.
			_, err = attachWeights(t, restarted, version, "weights", "s3://source")
			require.NoError(t, err)
			require.NoError(t, restarted.ApplyOperation(t.Context(), original.OperationUUID))
			assert.Equal(t, "2", *bindingOf(t, backend, *version.Id).NativeVersionNumber)
			require.NoError(t, restarted.Refresh(t.Context()))
			var retained []mlflow.NativeDisposition
			require.NoError(t, backend.db.Where("operation_uuid = ?", original.OperationUUID).Find(&retained).Error)
			require.Len(t, retained, 1)
			assert.Equal(t, "1", retained[0].NativeVersionNumber)
			assert.Equal(t, 2, backend.stub.countRequests("POST model-versions/create"))
		})
	}
}

// An insertion failure after supersession must roll back the lease, terminal
// state and retry link together, keeping the graph recoverable.
func TestOperatorRetryAtomic(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			child, _, _, _ := uncertainChild(t, backend)
			injected := errors.New("replacement insertion failed")
			const callback = "test:retry-create-failure"
			require.NoError(t, backend.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if op, ok := tx.Statement.Dest.(*mlflow.Operation); ok && op.ParentOperationUUID != nil {
					_ = tx.AddError(injected) // GORM propagates Statement.Error from the callback.
				}
			}))
			defer func() { require.NoError(t, backend.db.Callback().Create().Remove(callback)) }()
			err := backend.service.RetryCreate(t.Context(), child.OperationUUID)
			require.ErrorIs(t, err, injected)
			after, err := backend.store.GetOperation(t.Context(), child.OperationUUID)
			require.NoError(t, err)
			assert.Equal(t, child, after)
		})
	}
}

func TestOrphanResolution(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			resolver, ok := any(backend.service).(interface {
				ResolveOrphan(context.Context, string, bool) error
			})
			require.True(t, ok, "explicit orphan resolution must be available")
			child, _, _, _ := uncertainChild(t, backend)
			require.ErrorIs(t, resolver.ResolveOrphan(t.Context(), child.OperationUUID, true), api.ErrConflict)
			before, err := backend.store.GetOperation(t.Context(), child.OperationUUID)
			require.NoError(t, err)
			assert.Equal(t, child, before)
			// A legacy work order survives without its aggregate metadata. The
			// operator may retain native results but cannot recover that payload.
			orphan := child
			orphan.OperationUUID = "legacy-orphan"
			orphan.AggregateOperationUUID = nil
			orphan.RequestHash = ""
			orphan.RequestSequence += 10
			var candidate mlflow.MutationCandidate
			require.NoError(t, json.Unmarshal([]byte(child.CandidateJSON), &candidate))
			candidate.NativeWork.OwnedTags["rhoai.operation"] = orphan.OperationUUID
			encoded, err := json.Marshal(candidate)
			require.NoError(t, err)
			orphan.CandidateJSON = string(encoded)
			require.NoError(t, backend.db.Create(&orphan).Error)
			require.ErrorIs(t, resolver.ResolveOrphan(t.Context(), orphan.OperationUUID, false), api.ErrBadRequest)
			// A valid root owns this resource: resolving an unrelated orphan
			// must neither release that reservation nor alter committed bindings.
			require.NoError(t, resolver.ResolveOrphan(t.Context(), orphan.OperationUUID, true))
			var reservations []mlflow.IntentResource
			require.NoError(t, backend.db.Where("resource_id = ?", child.ResourceID).Find(&reservations).Error)
			require.Len(t, reservations, 1)
			assert.Equal(t, *child.AggregateOperationUUID, reservations[0].RootOperationUUID)
			resolved, err := backend.store.GetOperation(t.Context(), orphan.OperationUUID)
			require.NoError(t, err)
			assert.Equal(t, mlflow.OperationFailed, resolved.State)
			assert.Equal(t, orphan.CandidateJSON, resolved.CandidateJSON)
			assert.Equal(t, orphan.Step, resolved.Step)
			assert.Nil(t, resolved.NativeResult)
		})
	}
}
