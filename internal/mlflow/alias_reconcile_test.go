package mlflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/require"
)

type aliasTransport func(*http.Request) (*http.Response, error)

func (f aliasTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func aliasTestService(t *testing.T, backend *backendFixture, transport http.RoundTripper) *mlflow.Service {
	t.Helper()
	native, err := client.New(backend.stub.url, &http.Client{Transport: transport, Timeout: 5 * time.Second})
	require.NoError(t, err)
	service, err := mlflow.NewService(mlflow.Config{RegistryURI: backend.stub.url, RequestTimeout: 5 * time.Second}, mlflow.NewStore(backend.db), native)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	return service
}

// An unreadable alias never authorizes replacing its target.
func TestAliasOwnershipReadFailures(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, status := range []int{403, 503, 0} {
				t.Run(http.StatusText(status), func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					model := mustModel(t, backend, "alias-read")
					version := mustVersion(t, backend, model, "v1")
					service := aliasTestService(t, backend, aliasTransport(func(r *http.Request) (*http.Response, error) {
						if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/registered-models/alias") {
							if status == 0 {
								return nil, context.DeadlineExceeded
							}
							return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
						}
						return http.DefaultTransport.RoundTrip(r)
					}))
					_, err := attachWeights(t, service, version, "weights", "s3://bucket/a")
					want := api.ErrUnavailable
					if status == 403 {
						want = api.ErrForbidden
					}
					require.ErrorIs(t, err, want)
					require.Zero(t, backend.stub.countRequests("POST registered-models/alias"))
					require.Empty(t, backend.stub.aliases(model.Name))
				})
			}
		})
	}
}

func TestAliasOwnershipRejectsUnrelatedTargets(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, identity := range []string{"unowned", "unknown", "forged", "foreign", "wrong-model"} {
				t.Run(identity, func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					model := mustModel(t, backend, "alias-owner")
					version := mustVersion(t, backend, model, "v1")
					tags := map[string]string{}
					if identity != "unowned" {
						tags["rhoai.adapter"] = adapterUUIDOf(t, backend)
						tags["rhoai.logicalId"] = *version.Id
						tags["rhoai.generation"] = "1"
					}
					if identity == "unknown" {
						tags["rhoai.logicalId"] = "999999999"
					}
					if identity == "foreign" {
						tags["rhoai.adapter"] = "another-adapter"
					}
					backend.stub.forgeVersion(model.Name, "99", tags)
					if identity == "wrong-model" {
						backend.stub.versionRecord(model.Name, "99")["name"] = "another-model"
					}
					alias := bindingOf(t, backend, *version.Id).Alias
					backend.stub.aliases(model.Name)[alias] = "99"
					_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/a")
					require.ErrorIs(t, err, api.ErrConflict)
					require.Zero(t, backend.stub.countRequests("POST registered-models/alias"))
					require.Equal(t, "99", backend.stub.aliases(model.Name)[alias])
				})
			}
		})
	}
}

func TestAliasOwnershipMissingAndHistoricalTargets(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "alias-history")
			version := mustVersion(t, backend, model, "v1")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/one")
			alias := bindingOf(t, backend, *version.Id).Alias
			require.Equal(t, "1", backend.stub.aliases(model.Name)[alias], "definite 404 creates the expected alias")
			backend.stub.aliases(model.Name)["user-production"] = "1"
			replaceAliasSource(t, backend.service, version, primary, "s3://bucket/two")
			backend.stub.aliases(model.Name)[alias] = "1"
			replaceAliasSource(t, backend.service, version, primary, "s3://bucket/three")
			require.Equal(t, "3", backend.stub.aliases(model.Name)[alias])
			require.Equal(t, "1", backend.stub.aliases(model.Name)["user-production"])
		})
	}
}

func replaceAliasSource(t *testing.T, service *mlflow.Service, version *openapi.ModelVersion, primary *openapi.Artifact, uri string) {
	t.Helper()
	_, err := service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Name: primary.ModelArtifact.Name, Uri: new(uri)}}, *version.Id)
	require.NoError(t, err)
}

func reconcileAliases(t *testing.T, service *mlflow.Service) error {
	t.Helper()
	return service.ReconcileAliases(t.Context())
}

// SQL fencing cannot revoke an HTTP write already accepted by the native
// registry. A later sweep must repair that late write to the committed binding.
func TestAliasDriftDelayedOldGeneration(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "late-alias")
			version := mustVersion(t, backend, model, "v1")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/one")
			entered, release := backend.stub.requestBarrier("POST registered-models/alias")
			defer release()
			stale := make(chan error, 1)
			go func() {
				_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Name: primary.ModelArtifact.Name, Uri: new("s3://bucket/two")}}, *version.Id)
				stale <- err
			}()
			waitRefreshBarrier(t, entered)
			backend.stub.mu.Lock()
			delete(backend.stub.gates, "POST registered-models/alias")
			backend.stub.mu.Unlock()
			expireLeases(t, backend)
			var operation mlflow.Operation
			require.NoError(t, backend.db.Where("kind = ? AND state = ?", mlflow.OperationKindVersionReplace, mlflow.OperationRunning).First(&operation).Error)
			fresh := backend.newBackendService(t)
			require.NoError(t, fresh.ApplyOperation(t.Context(), operation.OperationUUID))
			replaceAliasSource(t, fresh, version, primary, "s3://bucket/three")
			binding := bindingOf(t, backend, *version.Id)
			require.Equal(t, "3", *binding.NativeVersionNumber)
			release()
			require.ErrorIs(t, <-stale, api.ErrConflict, "old SQL checkpoint must fail")
			require.Equal(t, "2", backend.stub.aliases(model.Name)[binding.Alias], "the late native write really landed")
			before, err := fixture.store.GetObject(t.Context(), binding.VersionID)
			require.NoError(t, err)
			require.NoError(t, reconcileAliases(t, fresh))
			require.Equal(t, "3", backend.stub.aliases(model.Name)[binding.Alias])
			after, err := fixture.store.GetObject(t.Context(), binding.VersionID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, binding, bindingOf(t, backend, *version.Id))
		})
	}
}

func TestAliasDriftConcurrentRepairsAndRepeatedCycles(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "repair-cycles")
			version := mustVersion(t, backend, model, "v1")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/one")
			replaceAliasSource(t, backend.service, version, primary, "s3://bucket/two")
			binding := bindingOf(t, backend, *version.Id)
			before, err := fixture.store.GetObject(t.Context(), binding.VersionID)
			require.NoError(t, err)
			replica := backend.newBackendService(t)
			var previousSequence int64
			for range 2 {
				backend.stub.aliases(model.Name)[binding.Alias] = "1"
				entered, release := backend.stub.requestBarrier("POST registered-models/alias")
				defer release()
				result := make(chan error, 1)
				go func() { result <- reconcileAliases(t, backend.service) }()
				waitRefreshBarrier(t, entered)
				require.NoError(t, reconcileAliases(t, replica), "another replica defers the leased repair")
				_, err := replica.UpsertRegisteredModel(&openapi.RegisteredModel{Id: model.Id, Name: model.Name, Description: new("metadata may change during repair")})
				require.NoError(t, err, "repair reserves the logical version, not unrelated model metadata")
				creates := backend.stub.countRequests("POST model-versions/create")
				_, err = replica.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Name: primary.ModelArtifact.Name, Uri: new("s3://bucket/blocked")}}, *version.Id)
				require.ErrorIs(t, err, api.ErrConflict)
				require.Equal(t, creates, backend.stub.countRequests("POST model-versions/create"))
				release()
				require.NoError(t, <-result)
				require.Equal(t, "2", backend.stub.aliases(model.Name)[binding.Alias])
				var operations []mlflow.Operation
				require.NoError(t, backend.db.Where("kind = ?", "alias-repair").Order("request_sequence DESC").Find(&operations).Error)
				require.NotEmpty(t, operations)
				require.Equal(t, mlflow.OperationSucceeded, operations[0].State)
				require.Greater(t, operations[0].RequestSequence, previousSequence)
				previousSequence = operations[0].RequestSequence
				var reservations int64
				require.NoError(t, backend.db.Model(&mlflow.IntentResource{}).Count(&reservations).Error)
				require.Zero(t, reservations)
			}
			after, err := fixture.store.GetObject(t.Context(), binding.VersionID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, binding, bindingOf(t, backend, *version.Id))
		})
	}
}

func TestAliasOwnershipActiveBeforePublication(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "active-alias")
			version := mustVersion(t, backend, model, "v1")
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			stale := aliasTestService(t, backend, aliasTransport(func(r *http.Request) (*http.Response, error) {
				response, err := http.DefaultTransport.RoundTrip(r)
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/registered-models/alias") {
					close(entered)
					<-release
				}
				return response, err
			}))
			result := make(chan error, 1)
			go func() { _, err := attachWeights(t, stale, version, "weights", "s3://bucket/one"); result <- err }()
			waitRefreshBarrier(t, entered)
			expireLeases(t, backend)
			operation := materializeOperationOf(t, backend)
			require.NoError(t, backend.service.ApplyOperation(t.Context(), operation.OperationUUID))
			once.Do(func() { close(release) })
			require.ErrorIs(t, <-result, api.ErrConflict)
			require.Equal(t, "1", *bindingOf(t, backend, *version.Id).NativeVersionNumber)
		})
	}
}

// Imported identities alone trigger no writes. An explicit write may use that
// exact stored parent, but must not adopt a subsequently foreign parent claim.
func TestAliasOwnershipImportedParent(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, foreign := range []bool{false, true} {
				t.Run(fmt.Sprint(foreign), func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					seedNativeModel(t, backend, "imported-alias", "", map[string]string{})
					seedNativeVersion(t, backend, "imported-alias", "1", "s3://bucket/one", "", nil)
					require.NoError(t, backend.service.Refresh(t.Context()))
					require.NoError(t, reconcileAliases(t, backend.service))
					require.Zero(t, backend.stub.countRequests("POST registered-models/alias"))
					rows := committedObjects(t, backend, mlflow.KindModelVersion)
					require.Len(t, rows, 1)
					version, err := backend.service.GetModelVersionById(fmt.Sprint(rows[0].ID))
					require.NoError(t, err)
					binding := bindingOf(t, backend, *version.Id)
					primary, err := backend.service.GetArtifactById(fmt.Sprint(*binding.PrimaryArtifactID))
					require.NoError(t, err)
					if foreign {
						backend.stub.tags("imported-alias")["rhoai.adapter"] = "foreign-adapter"
					}
					_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Name: primary.ModelArtifact.Name, Uri: new("s3://bucket/two")}}, *version.Id)
					if foreign {
						require.ErrorIs(t, err, api.ErrConflict)
						require.Zero(t, backend.stub.countRequests("POST model-versions/create"), "foreign parent must be rejected before native writes")
					} else {
						require.NoError(t, err)
						after := bindingOf(t, backend, *version.Id)
						require.NotEmpty(t, after.Alias)
						require.Equal(t, "2", backend.stub.aliases("imported-alias")[after.Alias])
					}
				})
			}
		})
	}
}

func TestAliasDriftRepairFencesCurrentDependencies(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, dependency := range []string{"object", "binding", "parent", "tuple"} {
				t.Run(dependency, func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					model := mustModel(t, backend, "repair-fence")
					version := mustVersion(t, backend, model, "v1")
					mustPrimaryArtifact(t, backend, version, "s3://bucket/one")
					binding := bindingOf(t, backend, *version.Id)
					operation, err := fixture.store.PrepareAliasRepair(t.Context(), binding.VersionID)
					require.NoError(t, err)
					claimed, err := fixture.store.ClaimOperation(t.Context(), operation.OperationUUID, "repair-test", time.Minute)
					require.NoError(t, err)
					switch dependency {
					case "object":
						require.NoError(t, backend.db.Model(&mlflow.Object{}).Where("id = ?", binding.VersionID).Update("revision", claimed.ExpectedRevision+1).Error)
					case "binding":
						require.NoError(t, backend.db.Model(&mlflow.VersionBinding{}).Where("version_id = ?", binding.VersionID).Update("revision", binding.Revision+1).Error)
					case "parent":
						require.NoError(t, backend.db.Model(&mlflow.Object{}).Where("id = ?", binding.ModelID).Update("name", "changed-parent-identity").Error)
					default:
						require.NoError(t, backend.db.Model(&mlflow.VersionBinding{}).Where("version_id = ?", binding.VersionID).Update("alias", "different-alias").Error)
					}
					require.ErrorIs(t, fixture.store.PublishOperation(t.Context(), claimed.OperationUUID, claimed.FencingToken), api.ErrConflict)
					stored, err := fixture.store.GetOperation(t.Context(), operation.OperationUUID)
					require.NoError(t, err)
					require.Equal(t, mlflow.OperationRunning, stored.State)
					var reservations int64
					require.NoError(t, backend.db.Model(&mlflow.IntentResource{}).Where("root_operation_uuid = ?", claimed.OperationUUID).Count(&reservations).Error)
					require.Positive(t, reservations)
				})
			}
		})
	}
}

func TestAliasDriftPlanValidationAndSemanticIdentity(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "repair-plan")
			version := mustVersion(t, backend, model, "v1")
			mustPrimaryArtifact(t, backend, version, "s3://bucket/one")
			binding := bindingOf(t, backend, *version.Id)
			object, err := fixture.store.GetObject(t.Context(), binding.VersionID)
			require.NoError(t, err)
			before := aliasPublishedRows(t, backend)
			var beforeOperations []mlflow.Operation
			require.NoError(t, backend.db.Order("operation_uuid").Find(&beforeOperations).Error)
			for i, payload := range []string{`{}`, `{"codecVersion":3}`} {
				_, err := fixture.store.PrepareOperation(t.Context(), mlflow.Operation{OperationUUID: fmt.Sprintf("missing-alias-plan-%d", i), ResourceID: binding.VersionID, Kind: mlflow.OperationKindAliasRepair, ExpectedRevision: object.Revision, Generation: binding.ActiveGeneration, CandidateJSON: payload})
				require.ErrorIs(t, err, api.ErrBadRequest, "alias repair has no legacy untyped form")
				var afterOperations []mlflow.Operation
				require.NoError(t, backend.db.Order("operation_uuid").Find(&afterOperations).Error)
				require.Equal(t, beforeOperations, afterOperations)
				require.Equal(t, before, aliasPublishedRows(t, backend))
				var reservations int64
				require.NoError(t, backend.db.Model(&mlflow.IntentResource{}).Count(&reservations).Error)
				require.Zero(t, reservations)
			}
			operation, err := fixture.store.PrepareAliasRepair(t.Context(), binding.VersionID)
			require.NoError(t, err)
			var candidate mlflow.MutationCandidate
			require.NoError(t, json.Unmarshal([]byte(operation.CandidateJSON), &candidate))
			retry := operation
			retry.OperationUUID = "alias-semantic-retry"
			candidate.AliasRepair.Desired.Source = "s3://bucket/different"
			payload, err := json.Marshal(candidate)
			require.NoError(t, err)
			retry.CandidateJSON = string(payload)
			_, err = fixture.store.PrepareOperation(t.Context(), retry)
			require.ErrorIs(t, err, api.ErrConflict, "a different desired target must not join the active repair")
			candidate.AliasRepair.Alias = ""
			payload, err = json.Marshal(candidate)
			require.NoError(t, err)
			retry.CandidateJSON = string(payload)
			_, err = fixture.store.PrepareOperation(t.Context(), retry)
			require.ErrorIs(t, err, api.ErrBadRequest)
		})
	}
}

func TestAliasDriftPublicationAndPeriodicRepair(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "repair-hooks")
			version := mustVersion(t, backend, model, "v1")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/one")
			replaceAliasSource(t, backend.service, version, primary, "s3://bucket/two")
			binding := bindingOf(t, backend, *version.Id)
			backend.stub.aliases(model.Name)[binding.Alias] = "1"
			_, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Id: model.Id, Name: model.Name, Description: new("publication triggers repair")})
			require.NoError(t, err)
			require.Equal(t, "2", backend.stub.aliases(model.Name)[binding.Alias])

			// Hold the first periodic sweep after its healthy native read. Drift
			// introduced here must be fixed by a later interval, not startup alone.
			entered, release := make(chan struct{}), make(chan struct{})
			var first sync.Once
			var unblock sync.Once
			defer unblock.Do(func() { close(release) })
			native, err := client.New(backend.stub.url, &http.Client{Transport: aliasTransport(func(r *http.Request) (*http.Response, error) {
				response, err := http.DefaultTransport.RoundTrip(r)
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/registered-models/alias") {
					first.Do(func() { close(entered); <-release })
				}
				return response, err
			}), Timeout: 5 * time.Second})
			require.NoError(t, err)
			runner, err := mlflow.NewService(mlflow.Config{RegistryURI: backend.stub.url, RequestTimeout: 5 * time.Second, RefreshInterval: 20 * time.Millisecond}, mlflow.NewStore(backend.db), native)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, runner.Close()) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- runner.Run(ctx) }()
			waitRefreshBarrier(t, entered)
			backend.stub.mu.Lock()
			backend.stub.models[model.Name]["aliases"].(map[string]string)[binding.Alias] = "1"
			backend.stub.mu.Unlock()
			unblock.Do(func() { close(release) })
			require.Eventually(t, func() bool {
				backend.stub.mu.Lock()
				defer backend.stub.mu.Unlock()
				return backend.stub.models[model.Name]["aliases"].(map[string]string)[binding.Alias] == "2"
			}, 5*time.Second, 10*time.Millisecond)
			cancel()
			require.NoError(t, <-done)
		})
	}
}

func TestAliasOwnershipImportedMetadataRejectsForeignParent(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, kind := range []string{"model", "version", "artifact"} {
				t.Run(kind, func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					seedNativeModel(t, backend, "foreign-parent", "", map[string]string{})
					seedNativeVersion(t, backend, "foreign-parent", "1", "s3://bucket/one", "", map[string]string{})
					require.NoError(t, backend.service.Refresh(t.Context()))
					models := committedObjects(t, backend, mlflow.KindRegisteredModel)
					versions := committedObjects(t, backend, mlflow.KindModelVersion)
					artifacts := committedObjects(t, backend, mlflow.KindModelArtifact)
					backend.stub.tags("foreign-parent")["rhoai.adapter"] = "another-adapter"
					var err error
					switch kind {
					case "model":
						_, err = backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Id: new(fmt.Sprint(models[0].ID)), Name: models[0].Name, Description: new("changed")})
					case "version":
						_, err = backend.service.UpsertModelVersion(&openapi.ModelVersion{Id: new(fmt.Sprint(versions[0].ID)), Name: versions[0].Name, Description: new("changed")}, new(fmt.Sprint(models[0].ID)))
					case "artifact":
						_, err = backend.service.UpsertModelArtifact(&openapi.ModelArtifact{Id: new(fmt.Sprint(artifacts[0].ID)), Name: new(artifacts[0].Name), Description: new("changed")})
					}
					require.ErrorIs(t, err, api.ErrConflict)
					require.Zero(t, backend.stub.countRequests("POST"))
					require.Zero(t, backend.stub.countRequests("PATCH"))
					require.Zero(t, backend.stub.countRequests("DELETE"))
					require.Equal(t, "another-adapter", backend.stub.tags("foreign-parent")["rhoai.adapter"])
				})
			}
		})
	}
}

// Recovery must repeat the parent check even when native creation has already
// checkpointed, including succeeded legacy children whose tags are replayed by
// their aggregate parent.
func TestAliasOwnershipReplayRejectsForeignParentBeforeWrites(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			for _, mode := range []string{"single", "legacy-single", "fanout", "legacy-fanout"} {
				t.Run(mode, func(t *testing.T) {
					backend := newBackendFixture(t, fixture)
					model := mustModel(t, backend, "replay-parent")
					first := mustVersion(t, backend, model, "first")
					var operation mlflow.Operation
					if strings.Contains(mode, "fanout") {
						second := mustVersion(t, backend, model, "second")
						primary := sharedPrimary(t, backend, model, first, second, "s3://bucket/one")
						backend.stub.failNextRequests("POST registered-models/alias", 1)
						_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/two")}}, *first.Id)
						require.ErrorIs(t, err, api.ErrUnavailable)
						require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionReplaceFanOut).First(&operation).Error)
						children := childOperations(t, backend)
						require.Len(t, children, 2)
						for _, child := range children {
							require.Equal(t, mlflow.OperationSucceeded, child.State)
						}
					} else {
						backend.stub.failNextRequests("POST model-versions/set-tag", 1)
						_, err := attachWeights(t, backend.service, first, "weights", "s3://bucket/one")
						require.ErrorIs(t, err, api.ErrUnavailable)
						operation = materializeOperationOf(t, backend)
						require.NotNil(t, operation.NativeResult)
					}
					if strings.HasPrefix(mode, "legacy-") {
						members := append([]mlflow.Operation{operation}, childOperations(t, backend)...)
						for _, member := range members {
							var candidate mlflow.MutationCandidate
							require.NoError(t, json.Unmarshal([]byte(member.CandidateJSON), &candidate))
							candidate.CodecVersion = 2
							candidate.ReadRevisions, candidate.BindingReadRevisions = nil, nil
							if member.Kind == mlflow.OperationKindVersionReplaceChild {
								payload, err := json.Marshal(map[string]string{"modelName": candidate.NativeWork.ModelName, "source": candidate.NativeWork.Source})
								require.NoError(t, err)
								candidate.Objects = []mlflow.ObjectChange{{ID: member.ResourceID, ExpectedRevision: member.ExpectedRevision, Snapshot: mlflow.StoredSnapshot{Kind: mlflow.KindModelVersion, Resource: payload}}}
							}
							candidate.NativeWork = nil
							for i := range candidate.Objects {
								candidate.Objects[i].Snapshot.CodecVersion = 2
							}
							payload, err := json.Marshal(candidate)
							require.NoError(t, err)
							require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", member.OperationUUID).Update("candidate_json", string(payload)).Error)
						}
					}
					backend.stub.tags(model.Name)["rhoai.adapter"] = "foreign-adapter"
					before := aliasPublishedRows(t, backend)
					writes := backend.stub.countRequests("POST") + backend.stub.countRequests("PATCH") + backend.stub.countRequests("DELETE")
					expireLeases(t, backend)
					err := backend.newBackendService(t).ApplyOperation(t.Context(), operation.OperationUUID)
					require.ErrorIs(t, err, api.ErrConflict)
					require.Equal(t, writes, backend.stub.countRequests("POST")+backend.stub.countRequests("PATCH")+backend.stub.countRequests("DELETE"), "replay must reject before any native mutation")
					require.Equal(t, before, aliasPublishedRows(t, backend))
					after, err := fixture.store.GetOperation(t.Context(), operation.OperationUUID)
					require.NoError(t, err)
					require.NotEqual(t, mlflow.OperationSucceeded, after.State)
					require.Equal(t, operation.NativeResult, after.NativeResult)
				})
			}
		})
	}
}

func aliasPublishedRows(t *testing.T, backend *backendFixture) string {
	t.Helper()
	var rows struct {
		Objects  []mlflow.Object
		Bindings []mlflow.VersionBinding
		History  []mlflow.VersionHistory
		Links    []mlflow.ArtifactLink
	}
	require.NoError(t, backend.db.Order("id").Find(&rows.Objects).Error)
	require.NoError(t, backend.db.Order("id").Find(&rows.Bindings).Error)
	require.NoError(t, backend.db.Order("id").Find(&rows.History).Error)
	require.NoError(t, backend.db.Order("artifact_id, version_id").Find(&rows.Links).Error)
	payload, err := json.Marshal(rows)
	require.NoError(t, err)
	return string(payload)
}
