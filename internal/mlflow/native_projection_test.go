package mlflow_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const escapedAuthor = "rhoai.custom.0cdb012d42d927826f4c0d7cd2f9c860373a48774e0f920aa3632ac247328bf7"

func projectionVersion(t *testing.T, backend *backendFixture, model *openapi.RegisteredModel, name string) *openapi.ModelVersion {
	t.Helper()
	integer := "-9223372036854775808"
	if name == "second" {
		integer = "9223372036854775807"
	}
	version, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{Name: name, Author: new("author-" + name), Description: new("version-" + name), CustomProperties: map[string]openapi.MetadataValue{"flavor": stringValue(name), "rhoai.author": stringValue("escaped-" + name), "label-" + name: stringValue(""), "integer": intValue(integer), "enabled": boolValue(name == "second"), "proto": protoValue("cHJvdG8="), "struct": structValue("c3RydWN0")}}, model.Id)
	require.NoError(t, err)
	return version
}

func projectionAdditional(t *testing.T, backend *backendFixture, version *openapi.ModelVersion) *openapi.Artifact {
	t.Helper()
	artifact, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Name: new("readme-" + version.Name), Description: new("additional-" + version.Name), ModelFormatName: new("text-" + version.Name), CustomProperties: map[string]openapi.MetadataValue{"audience": stringValue(version.Name)}}}, *version.Id)
	require.NoError(t, err)
	return artifact
}

func assertProjection(t *testing.T, backend *backendFixture, model *openapi.RegisteredModel, version *openapi.ModelVersion, additional *openapi.Artifact, source string, generation int64) {
	t.Helper()
	binding := bindingOf(t, backend, *version.Id)
	require.NotNil(t, binding.NativeVersionNumber)
	record := backend.stub.versionRecord(model.Name, *binding.NativeVersionNumber)
	assert.Equal(t, model.Name, record["name"])
	assert.Equal(t, source, record["source"])
	assert.Equal(t, "version-"+version.Name, record["description"])
	tags := backend.stub.versionTags(model.Name, *binding.NativeVersionNumber)
	for key, want := range map[string]string{"rhoai.versionName": version.Name, "rhoai.author": "author-" + version.Name, "flavor": version.Name, escapedAuthor: "escaped-" + version.Name, "rhoai.generation": fmt.Sprint(generation), "rhoai.logicalId": *version.Id, "rhoai.modelFormat": "onnx", "rhoai.artifactRecord.description": "weights description", "rhoai.artifact.quality": "primary-quality"} {
		assert.Equal(t, want, tags[key], key)
	}
	assert.Contains(t, tags, "label-"+version.Name)
	assert.Equal(t, "cHJvdG8=", tags["proto"])
	assert.Equal(t, "c3RydWN0", tags["struct"])
	if version.Name == "second" {
		assert.Equal(t, "9223372036854775807", tags["integer"])
		assert.Equal(t, "true", tags["enabled"])
	} else {
		assert.Equal(t, "-9223372036854775808", tags["integer"])
		assert.Equal(t, "false", tags["enabled"])
	}
	assert.NotEmpty(t, tags["rhoai.adapter"])
	assert.NotEmpty(t, tags["rhoai.operation"])
	prefix := "rhoai.artifacts." + *additional.ModelArtifact.Id + "."
	for key, want := range map[string]string{"artifactRecord.description": "additional-" + version.Name, "modelFormat": "text-" + version.Name, "artifact.audience": version.Name} {
		assert.Equal(t, want, tags[prefix+key], prefix+key)
	}
}

func TestMaterializeDescriptionAndCompleteProjection(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "projection")
			version := projectionVersion(t, backend, model, "v1")
			additional := projectionAdditional(t, backend, version)
			primary, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Name: new("weights"), Uri: new("s3://bucket/original"), Description: new("weights description"), ModelFormatName: new("onnx"), CustomProperties: map[string]openapi.MetadataValue{"quality": stringValue("primary-quality")}}}, *version.Id)
			require.NoError(t, err)
			assertProjection(t, backend, model, version, additional, "s3://bucket/original", 1)
			updated, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/replacement")}}, *version.Id)
			require.NoError(t, err)
			assert.Equal(t, primary.ModelArtifact.Id, updated.ModelArtifact.Id)
			assert.Equal(t, primary.ModelArtifact.CreateTimeSinceEpoch, updated.ModelArtifact.CreateTimeSinceEpoch)
			assertProjection(t, backend, model, version, additional, "s3://bucket/replacement", 2)
			require.NoError(t, backend.service.Refresh(t.Context()))
			versions, err := backend.service.GetModelVersions(api.ListOptions{}, model.Id)
			require.NoError(t, err)
			require.Len(t, versions.Items, 1)
			assert.Equal(t, version.Description, versions.Items[0].Description)
			assert.Equal(t, version.Author, versions.Items[0].Author)
			assert.Equal(t, version.CustomProperties, versions.Items[0].CustomProperties)
			assert.Equal(t, version.Id, versions.Items[0].Id)
			assert.Equal(t, version.CreateTimeSinceEpoch, versions.Items[0].CreateTimeSinceEpoch)
			histories := historiesOf(t, backend, *version.Id)
			require.Len(t, histories, 2)
			assert.True(t, histories[0].Superseded)
			assert.Zero(t, backend.stub.countRequests("UNEXPECTED"))
		})
	}
}

func TestFanOutProjectionIsDistinctForEachVersion(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "fanout-projection")
			first := projectionVersion(t, backend, model, "first")
			second := projectionVersion(t, backend, model, "second")
			extras := []*openapi.Artifact{projectionAdditional(t, backend, first), projectionAdditional(t, backend, second)}
			primary, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Name: new("weights"), Uri: new("s3://bucket/original"), Description: new("weights description"), ModelFormatName: new("onnx"), CustomProperties: map[string]openapi.MetadataValue{"quality": stringValue("primary-quality")}}}, *first.Id)
			require.NoError(t, err)
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id}}, *second.Id)
			require.NoError(t, err)
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/replacement")}}, *first.Id)
			require.NoError(t, err)
			for i, version := range []*openapi.ModelVersion{first, second} {
				assertProjection(t, backend, model, version, extras[i], "s3://bucket/replacement", 2)
			}
			require.NoError(t, backend.service.Refresh(t.Context()))
			for _, version := range []*openapi.ModelVersion{first, second} {
				current, err := backend.service.GetModelVersionById(*version.Id)
				require.NoError(t, err)
				assert.Equal(t, version.Description, current.Description)
				assert.Equal(t, version.Author, current.Author)
				assert.Equal(t, version.CustomProperties, current.CustomProperties)
				histories := historiesOf(t, backend, *version.Id)
				require.Len(t, histories, 2)
				assert.True(t, histories[0].Superseded)
			}
			assert.Zero(t, backend.stub.countRequests("UNEXPECTED"))
		})
	}
}

func TestNativeProjectionPriorReadFailureDoesNotPrepare(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "read-failure")
			version := mustVersion(t, backend, model, "v1")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/original")
			backend.stub.failNextRequests("GET model-versions/get", 1)
			creates := backend.stub.countRequests("POST model-versions/create")
			_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/replacement")}}, *version.Id)
			assert.ErrorIs(t, err, api.ErrUnavailable)
			assert.Equal(t, creates, backend.stub.countRequests("POST model-versions/create"))
			var count int64
			require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("kind = ?", mlflow.OperationKindVersionReplace).Count(&count).Error)
			assert.Zero(t, count, "a missing unrelated-tag read cannot become a frozen intent")
		})
	}
}

func TestNativeProjectionReservesFrozenMetadata(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			replica := backend.newBackendService(t)
			model := mustModel(t, backend, "frozen")
			version := projectionVersion(t, backend, model, "v1")
			additional := projectionAdditional(t, backend, version)
			entered, release := backend.stub.requestBarrier("POST model-versions/create")
			t.Cleanup(release)
			result := make(chan error, 1)
			go func() {
				_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/original")
				result <- err
			}()
			select {
			case <-entered:
			case err := <-result:
				t.Fatalf("ended before create: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("create barrier not reached")
			}
			operation := materializeOperationOf(t, backend)
			var candidate mlflow.MutationCandidate
			require.NoError(t, json.Unmarshal([]byte(operation.CandidateJSON), &candidate))
			assert.NotNil(t, candidate.NativeWork, "the entire projection must be persisted before dispatch")
			_, err := replica.UpsertModelVersion(&openapi.ModelVersion{Id: version.Id, Author: new("changed")}, model.Id)
			assert.ErrorIs(t, err, api.ErrConflict)
			_, err = replica.UpsertModelArtifact(&openapi.ModelArtifact{Id: additional.ModelArtifact.Id, Description: new("changed")})
			assert.ErrorIs(t, err, api.ErrConflict)
			release()
			require.NoError(t, <-result)
			tags := backend.stub.versionTags(model.Name, "1")
			assert.Equal(t, "author-v1", tags["rhoai.author"])
			assert.Equal(t, "additional-v1", tags["rhoai.artifacts."+*additional.ModelArtifact.Id+".artifactRecord.description"])
		})
	}
}

// A codec2 create already dispatched before upgrade must be reconciled before
// freezing/repairing its committed SQL projection, without changing its work order.
func TestNativeProjectionLegacyUpgrade(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		for _, badIdentity := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/badIdentity=%t", engine, badIdentity), func(t *testing.T) {
				backend := newBackendFixture(t, fixture)
				model := mustModel(t, backend, "legacy-projection")
				version := projectionVersion(t, backend, model, "v1")
				additional := projectionAdditional(t, backend, version)
				backend.stub.mu.Lock()
				backend.stub.commitThenFail = 1
				backend.stub.mu.Unlock()
				_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/original")
				require.ErrorIs(t, err, api.ErrUnavailable)
				operation := materializeOperationOf(t, backend)
				var candidate mlflow.MutationCandidate
				require.NoError(t, json.Unmarshal([]byte(operation.CandidateJSON), &candidate))
				candidate.CodecVersion = 2
				candidate.NativeWork = nil
				candidate.ReadRevisions = nil
				candidate.BindingReadRevisions = nil
				for i := range candidate.Objects {
					candidate.Objects[i].Snapshot.CodecVersion = 2
				}
				payload, err := json.Marshal(candidate)
				require.NoError(t, err)
				require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", operation.OperationUUID).Updates(map[string]any{"candidate_json": string(payload), "payload_hash": "legacy-original-hash", "request_hash": "", "execution_plan_json": nil}).Error)
				require.NoError(t, backend.db.Where("root_operation_uuid = ?", operation.OperationUUID).Delete(&mlflow.IntentResource{}).Error)
				backend.stub.mu.Lock()
				record := backend.stub.versions[model.Name]["1"].(map[string]any)
				record["description"] = "old incorrect artifact description"
				tags := record["tags"].(map[string]string)
				delete(tags, "rhoai.generation")
				// Present the native view as the pre-upgrade binary left
				// it: the legacy flattened additional-artifact key, never
				// the codec-4 sub-namespaces.
				delete(tags, "rhoai.artifacts."+*additional.ModelArtifact.Id+".artifactRecord.description")
				tags["rhoai.artifacts."+*additional.ModelArtifact.Id+".description"] = "stale additional"
				if badIdentity {
					tags["rhoai.logicalId"] = "999999999"
				}
				backend.stub.mu.Unlock()
				adapter, err := fixture.store.GetAdapter(t.Context())
				require.NoError(t, err)
				_, err = fixture.store.InitializeAdapter(t.Context(), adapter.TargetFingerprint)
				require.NoError(t, err)
				expireLeases(t, backend)
				replica := backend.newBackendService(t)
				if !badIdentity {
					entered, release := backend.stub.requestBarrier("POST model-versions/set-tag")
					t.Cleanup(release)
					result := make(chan error, 1)
					go func() { result <- replica.ApplyOperation(t.Context(), operation.OperationUUID) }()
					select {
					case <-entered:
					case err := <-result:
						t.Fatalf("legacy replay ended before projection: %v", err)
					case <-time.After(5 * time.Second):
						t.Fatal("legacy projection barrier not reached")
					}
					var reservation mlflow.IntentResource
					require.NoError(t, backend.db.Where("resource_id = ?", mustObjectID(t, additional.ModelArtifact.Id)).First(&reservation).Error)
					assert.Equal(t, operation.OperationUUID, reservation.RootOperationUUID)
					_, editErr := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{Id: additional.ModelArtifact.Id, Description: new("interfering edit")})
					assert.ErrorIs(t, editErr, api.ErrConflict)
					release()
					err = <-result
				} else {
					err = replica.ApplyOperation(t.Context(), operation.OperationUUID)
				}
				after, loadErr := fixture.store.GetOperation(t.Context(), operation.OperationUUID)
				require.NoError(t, loadErr)
				assert.Equal(t, string(payload), after.CandidateJSON)
				assert.Equal(t, "legacy-original-hash", after.PayloadHash)
				assert.Equal(t, 1, backend.stub.countRequests("POST model-versions/create"))
				if badIdentity {
					assert.ErrorIs(t, err, api.ErrConflict)
					require.NotNil(t, after.FailureCode)
					assert.Equal(t, "LEGACY_GRAPH_QUARANTINED", *after.FailureCode)
					assert.Nil(t, after.ExecutionPlanJSON)
					return
				}
				require.NoError(t, err)
				require.NotNil(t, after.ExecutionPlanJSON)
				assert.Equal(t, "version-v1", backend.stub.versionRecord(model.Name, "1")["description"])
				// Legacy work replays verbatim under the legacy flattened
				// key; the codec-4 sub-namespaces are never introduced.
				legacyAdditional := backend.stub.versionTags(model.Name, "1")
				assert.Equal(t, "additional-v1", legacyAdditional["rhoai.artifacts."+*additional.ModelArtifact.Id+".description"])
				assert.NotContains(t, legacyAdditional, "rhoai.artifacts."+*additional.ModelArtifact.Id+".artifactRecord.description")
			})
		}
	}
}

func TestNativeProjectionRejectsUnknownNestedSnapshotCodec(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			object, err := fixture.store.ReserveObject(t.Context(), reservedModel("nested-codec"))
			require.NoError(t, err)
			payload, err := json.Marshal(mlflow.MutationCandidate{CodecVersion: 3, Objects: []mlflow.ObjectChange{{ID: object.ID, ExpectedRevision: object.Revision, Snapshot: mlflow.StoredSnapshot{CodecVersion: 99, Kind: mlflow.KindRegisteredModel, Resource: json.RawMessage(`{"name":"nested-codec"}`)}}}})
			require.NoError(t, err)
			_, err = fixture.store.PrepareOperation(t.Context(), mlflow.Operation{OperationUUID: "bad-nested-codec", ResourceID: object.ID, ExpectedRevision: object.Revision, Kind: "update", CandidateJSON: string(payload)})
			require.ErrorIs(t, err, api.ErrBadRequest)
		})
	}
}

func TestNativeProjectionLegacyFrozenReplayVerifiesIdentity(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "legacy-frozen")
			version := projectionVersion(t, backend, model, "v1")
			backend.stub.mu.Lock()
			backend.stub.failCreates = 1
			backend.stub.mu.Unlock()
			_, err := attachWeights(t, backend.service, version, "weights", "s3://bucket/original")
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
			// Seed an old pre-dispatch journal fixture; the rejected request
			// above created no native version.
			require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", operation.OperationUUID).Updates(map[string]any{"candidate_json": string(payload), "step": mlflow.StepPrepared}).Error)
			expireLeases(t, backend)
			backend.stub.mu.Lock()
			backend.stub.commitThenFail = 1
			backend.stub.mu.Unlock()
			require.ErrorIs(t, backend.service.ApplyOperation(t.Context(), operation.OperationUUID), api.ErrUnavailable)
			frozen, err := fixture.store.GetOperation(t.Context(), operation.OperationUUID)
			require.NoError(t, err)
			require.NotNil(t, frozen.ExecutionPlanJSON)
			backend.stub.mu.Lock()
			backend.stub.versions[model.Name]["1"].(map[string]any)["tags"].(map[string]string)["rhoai.logicalId"] = "999999999"
			backend.stub.mu.Unlock()
			expireLeases(t, backend)
			err = backend.newBackendService(t).ApplyOperation(t.Context(), operation.OperationUUID)
			require.ErrorIs(t, err, api.ErrConflict)
			after, err := fixture.store.GetOperation(t.Context(), operation.OperationUUID)
			require.NoError(t, err)
			require.NotNil(t, after.FailureCode)
			assert.Equal(t, "LEGACY_GRAPH_QUARANTINED", *after.FailureCode)
			assert.Equal(t, frozen.ExecutionPlanJSON, after.ExecutionPlanJSON)
			assert.Equal(t, 2, backend.stub.countRequests("POST model-versions/create"))
		})
	}
}

// The prior-native read creates a deterministic gap between consuming SQL
// metadata and graph preparation. Rechecking a newer revision here would admit
// the old projection after a concurrent committed metadata/link change.
func TestNativeProjectionRejectsInputsChangedDuringRead(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		for _, change := range []string{"author", "additional", "membership"} {
			t.Run(engine+"/"+change, func(t *testing.T) {
				backend := newBackendFixture(t, fixture)
				replica := backend.newBackendService(t)
				model := mustModel(t, backend, "projection-input-race")
				version := projectionVersion(t, backend, model, "v1")
				additional := projectionAdditional(t, backend, version)
				primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/original")
				creates := backend.stub.countRequests("POST model-versions/create")
				entered, release := backend.stub.requestBarrier("GET model-versions/get")
				t.Cleanup(release)
				result := make(chan error, 1)
				go func() {
					_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/replacement")}}, *version.Id)
					result <- err
				}()
				select {
				case <-entered:
				case err := <-result:
					t.Fatalf("ended before prior read: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("prior read barrier not reached")
				}
				var err error
				switch change {
				case "author":
					_, err = replica.UpsertModelVersion(&openapi.ModelVersion{Id: version.Id, Author: new("changed")}, model.Id)
				case "additional":
					_, err = replica.UpsertModelArtifact(&openapi.ModelArtifact{Id: additional.ModelArtifact.Id, Description: new("changed")})
				case "membership":
					_, err = replica.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Name: new("late-readme"), Description: new("late")}}, *version.Id)
				}
				require.NoError(t, err)
				release()
				require.ErrorIs(t, <-result, api.ErrConflict)
				assert.Equal(t, creates, backend.stub.countRequests("POST model-versions/create"))
				assert.Equal(t, int64(1), bindingOf(t, backend, *version.Id).ActiveGeneration)
			})
		}
	}
}

func TestNativeProjectionRetainedTagsStayFrozenAcrossRecovery(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "retained-projection")
			version := projectionVersion(t, backend, model, "v1")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/original")
			backend.stub.mu.Lock()
			backend.stub.versions[model.Name]["1"].(map[string]any)["tags"].(map[string]string)["external-note"] = "original"
			backend.stub.mu.Unlock()
			backend.stub.failNextRequests("POST model-versions/set-tag", 1)
			_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/replacement")}}, *version.Id)
			require.ErrorIs(t, err, api.ErrUnavailable)
			var operation mlflow.Operation
			require.NoError(t, backend.db.Where("kind = ?", mlflow.OperationKindVersionReplace).First(&operation).Error)
			var candidate mlflow.MutationCandidate
			require.NoError(t, json.Unmarshal([]byte(operation.CandidateJSON), &candidate))
			require.NotNil(t, candidate.NativeWork)
			assert.Equal(t, "original", candidate.NativeWork.RetainedTags["external-note"])
			backend.stub.mu.Lock()
			backend.stub.versions[model.Name]["1"].(map[string]any)["tags"].(map[string]string)["external-note"] = "changed"
			backend.stub.mu.Unlock()
			priorRead := "GET model-versions/get [" + model.Name + "/1]"
			reads := backend.stub.countRequests(priorRead)
			expireLeases(t, backend)
			require.NoError(t, backend.newBackendService(t).ApplyOperation(t.Context(), operation.OperationUUID))
			assert.Equal(t, reads, backend.stub.countRequests(priorRead), "replay uses recorded inputs; verifying the new generation is independent")
			assert.Equal(t, "original", backend.stub.versionTags(model.Name, "2")["external-note"])
			assert.Equal(t, 2, backend.stub.countRequests("POST model-versions/create"))
		})
	}
}

func TestNativeProjectionAdditionalEditPreservesPrimaryProjection(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "additional-edit")
			version := projectionVersion(t, backend, model, "v1")
			additional := projectionAdditional(t, backend, version)
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/original")
			_, err := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{Id: additional.ModelArtifact.Id, Description: new("edited")})
			require.NoError(t, err)
			tags := backend.stub.versionTags(model.Name, "1")
			assert.Equal(t, "primary", tags["rhoai.artifactRecord.name"])
			assert.Equal(t, "edited", tags["rhoai.artifacts."+*additional.ModelArtifact.Id+".artifactRecord.description"])
			assert.Equal(t, "version-v1", backend.stub.versionRecord(model.Name, "1")["description"])
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: primary.ModelArtifact.Uri}}, *version.Id)
			require.NoError(t, err)
			assert.Equal(t, 1, backend.stub.countRequests("POST model-versions/create"))
			assert.Equal(t, int64(1), bindingOf(t, backend, *version.Id).ActiveGeneration)
		})
	}
}

func TestNativeProjectionLegacySuccessfulChildrenUpgrade(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "legacy-children")
			first := projectionVersion(t, backend, model, "first")
			second := projectionVersion(t, backend, model, "second")
			extras := []*openapi.Artifact{projectionAdditional(t, backend, first), projectionAdditional(t, backend, second)}
			primary := sharedPrimary(t, backend, model, first, second, "s3://bucket/original")
			for _, version := range []*openapi.ModelVersion{first, second} {
				binding := bindingOf(t, backend, *version.Id)
				backend.stub.mu.Lock()
				backend.stub.versions[model.Name][*binding.NativeVersionNumber].(map[string]any)["tags"].(map[string]string)["external-note"] = "external-" + version.Name
				backend.stub.mu.Unlock()
			}
			backend.stub.failNextRequests("POST registered-models/alias", 1)
			_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/replacement")}}, *first.Id)
			require.ErrorIs(t, err, api.ErrUnavailable)
			var operations []mlflow.Operation
			require.NoError(t, backend.db.Where("kind IN ?", []string{mlflow.OperationKindVersionReplaceFanOut, mlflow.OperationKindVersionReplaceChild}).Find(&operations).Error)
			require.Len(t, operations, 3)
			var root string
			recorded := map[string]string{}
			for _, op := range operations {
				var candidate mlflow.MutationCandidate
				require.NoError(t, json.Unmarshal([]byte(op.CandidateJSON), &candidate))
				candidate.CodecVersion = 2
				candidate.ReadRevisions, candidate.BindingReadRevisions = nil, nil
				if op.Kind == mlflow.OperationKindVersionReplaceChild {
					require.Equal(t, mlflow.OperationSucceeded, op.State)
					payload, err := json.Marshal(map[string]string{"modelName": candidate.NativeWork.ModelName, "source": candidate.NativeWork.Source})
					require.NoError(t, err)
					candidate.NativeWork = nil
					candidate.Objects = []mlflow.ObjectChange{{ID: op.ResourceID, ExpectedRevision: op.ExpectedRevision, Snapshot: mlflow.StoredSnapshot{Kind: mlflow.KindModelVersion, Resource: payload}}}
					var result mlflow.NativeBindingResult
					require.NoError(t, json.Unmarshal([]byte(*op.NativeResult), &result))
					backend.stub.mu.Lock()
					native := backend.stub.versions[model.Name][result.VersionNumber].(map[string]any)
					native["description"] = ""
					native["tags"] = map[string]string{"rhoai.adapter": adapterUUIDOf(t, backend), "rhoai.logicalId": fmt.Sprint(op.ResourceID), "rhoai.operation": op.OperationUUID}
					backend.stub.mu.Unlock()
				} else {
					root = op.OperationUUID
				}
				for i := range candidate.Objects {
					candidate.Objects[i].Snapshot.CodecVersion = 2
				}
				payload, err := json.Marshal(candidate)
				require.NoError(t, err)
				recorded[op.OperationUUID] = string(payload)
				require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", op.OperationUUID).Updates(map[string]any{"candidate_json": string(payload), "payload_hash": "legacy-payload", "request_hash": "", "execution_plan_json": nil, "aggregate_operation_uuid": nil}).Error)
			}
			require.NoError(t, backend.db.Where("root_operation_uuid = ?", root).Delete(&mlflow.IntentResource{}).Error)
			adapter, err := fixture.store.GetAdapter(t.Context())
			require.NoError(t, err)
			_, err = fixture.store.InitializeAdapter(t.Context(), adapter.TargetFingerprint)
			require.NoError(t, err)
			expireLeases(t, backend)
			require.NoError(t, backend.newBackendService(t).ApplyOperation(t.Context(), root))
			for i, version := range []*openapi.ModelVersion{first, second} {
				binding := bindingOf(t, backend, *version.Id)
				record := backend.stub.versionRecord(model.Name, *binding.NativeVersionNumber)
				assert.Equal(t, "version-"+version.Name, record["description"])
				tags := backend.stub.versionTags(model.Name, *binding.NativeVersionNumber)
				assert.Equal(t, "author-"+version.Name, tags["rhoai.author"])
				assert.Equal(t, "additional-"+version.Name, tags["rhoai.artifacts."+*extras[i].ModelArtifact.Id+".description"])
				assert.Equal(t, "external-"+version.Name, tags["external-note"])
			}
			for _, child := range childOperations(t, backend) {
				assert.Equal(t, recorded[child.OperationUUID], child.CandidateJSON)
				assert.Equal(t, "legacy-payload", child.PayloadHash)
				require.NotNil(t, child.ExecutionPlanJSON)
			}
			assert.Equal(t, 4, backend.stub.countRequests("POST model-versions/create"))
		})
	}
}

// rewriteArtifactSnapshotCodec rewrites a committed artifact snapshot's
// codec version and owned-tag projection, simulating rows written by an
// older binary.
func rewriteArtifactSnapshotCodec(t *testing.T, backend *backendFixture, id string, codec int, owned map[string]string) {
	t.Helper()
	var row mlflow.Object
	require.NoError(t, backend.db.Where("id = ?", mustObjectID(t, new(id))).First(&row).Error)
	var snapshot mlflow.StoredSnapshot
	require.NoError(t, json.Unmarshal([]byte(row.CommittedJSON), &snapshot))
	snapshot.CodecVersion = codec
	if owned != nil {
		snapshot.OwnedTags = owned
	}
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NoError(t, backend.db.Model(&mlflow.Object{}).Where("id = ?", row.ID).Update("committed_json", string(encoded)).Error)
}

// setNativeAdditionalTags replaces the additional-artifact keys of a
// native model version, simulating the tag layout an older binary
// wrote. Keys maps relative suffixes to values; every other
// additional-artifact key of this artifact is removed.
func setNativeAdditionalTags(t *testing.T, backend *backendFixture, model string, number, artifactID, prefix string, keys map[string]string) {
	t.Helper()
	backend.stub.mu.Lock()
	defer backend.stub.mu.Unlock()
	tags := backend.stub.versions[model][number].(map[string]any)["tags"].(map[string]string)
	for key := range tags {
		if strings.HasPrefix(key, prefix) {
			delete(tags, key)
		}
	}
	for key, value := range keys {
		tags[prefix+key] = value
	}
}

// TestAdditionalArtifactNamespaceProjection covers create, metadata
// update, refresh, and source replacement while a mirrored field and a
// custom property share one name: both values must survive in distinct
// sub-namespaces through every path.
func TestAdditionalArtifactNamespaceProjection(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "namespace-projection")
			version := mustVersion(t, backend, model, "1.0.0")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/primary")
			additional, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Name: new("record-name"), Description: new("record-description"), ModelFormatName: new("onnx"),
				CustomProperties: map[string]openapi.MetadataValue{
					"name":        stringValue("property-name"),
					"description": stringValue("property-description"),
				},
			}}, *version.Id)
			require.NoError(t, err)
			id := *additional.ModelArtifact.Id
			prefix := "rhoai.artifacts." + id + "."
			tags := backend.stub.versionTags(model.Name, "1")
			assert.Equal(t, "record-name", tags[prefix+"artifactRecord.name"])
			assert.Equal(t, "property-name", tags[prefix+"artifact.name"])
			assert.Equal(t, "record-description", tags[prefix+"artifactRecord.description"])
			assert.Equal(t, "property-description", tags[prefix+"artifact.description"])
			assert.Equal(t, "onnx", tags[prefix+"modelFormat"])

			// A metadata update rewrites both namespaces.
			_, err = backend.service.UpsertModelArtifact(&openapi.ModelArtifact{Id: &id, Description: new("edited-record")})
			require.NoError(t, err)
			tags = backend.stub.versionTags(model.Name, "1")
			assert.Equal(t, "edited-record", tags[prefix+"artifactRecord.description"])
			assert.Equal(t, "property-description", tags[prefix+"artifact.description"])
			assert.Equal(t, "record-name", tags[prefix+"artifactRecord.name"])
			assert.Equal(t, "property-name", tags[prefix+"artifact.name"])

			// Refresh never diffs artifact tags: both namespaces survive.
			require.NoError(t, backend.service.Refresh(t.Context()))
			tags = backend.stub.versionTags(model.Name, "1")
			assert.Equal(t, "edited-record", tags[prefix+"artifactRecord.description"])
			assert.Equal(t, "property-name", tags[prefix+"artifact.name"])
			view, err := backend.service.GetModelArtifactById(id)
			require.NoError(t, err)
			assert.Equal(t, "record-name", *view.Name)
			assert.Equal(t, "property-name", view.CustomProperties["name"].MetadataStringValue.StringValue)

			// A source replacement rebuilds the complete projection with
			// both namespaces on the new generation.
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: primary.ModelArtifact.Id, Uri: new("s3://bucket/replacement")}}, *version.Id)
			require.NoError(t, err)
			binding := bindingOf(t, backend, *version.Id)
			tags = backend.stub.versionTags(model.Name, *binding.NativeVersionNumber)
			assert.Equal(t, "record-name", tags[prefix+"artifactRecord.name"])
			assert.Equal(t, "property-name", tags[prefix+"artifact.name"])
			assert.Equal(t, "edited-record", tags[prefix+"artifactRecord.description"])
		})
	}
}

// TestAdditionalArtifactLegacySnapshotMigration proves the first codec-4
// write against a legacy ( codec 3 ) committed snapshot retires the old
// flattened keys — including a collision the legacy projection already
// overwrote natively — and rewrites both sub-namespace keys from the
// committed SQL resource, the migration authority.
func TestAdditionalArtifactLegacySnapshotMigration(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "legacy-migration")
			version := mustVersion(t, backend, model, "1.0.0")
			mustPrimaryArtifact(t, backend, version, "s3://bucket/primary")
			additional, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Name: new("record-name"), ModelFormatName: new("onnx"),
				CustomProperties: map[string]openapi.MetadataValue{
					"name": stringValue("property-name"),
				},
			}}, *version.Id)
			require.NoError(t, err)
			id := *additional.ModelArtifact.Id
			prefix := "rhoai.artifacts." + id + "."

			// Present SQL and native state as the pre-upgrade binary left
			// it: a codec-3 snapshot and the flattened native keys, where
			// the field/property collision already lost the record name.
			rewriteArtifactSnapshotCodec(t, backend, id, 3, map[string]string{
				prefix + "name":        "property-name",
				prefix + "modelFormat": "onnx",
			})
			setNativeAdditionalTags(t, backend, model.Name, "1", id, prefix, map[string]string{
				"name": "property-name", "modelFormat": "onnx",
			})

			// The first v4 write migrates the namespace.
			_, err = backend.service.UpsertModelArtifact(&openapi.ModelArtifact{Id: &id, Description: new("migrated-record")})
			require.NoError(t, err)
			tags := backend.stub.versionTags(model.Name, "1")
			assert.NotContains(t, tags, prefix+"name", "the flattened collision key must be retired")
			assert.Equal(t, "record-name", tags[prefix+"artifactRecord.name"], "the SQL resource is the migration authority")
			assert.Equal(t, "property-name", tags[prefix+"artifact.name"])
			assert.Equal(t, "migrated-record", tags[prefix+"artifactRecord.description"])
			assert.Equal(t, "onnx", tags[prefix+"modelFormat"])
		})
	}
}

// TestAdditionalArtifactInterruptedLegacyReplay proves an interrupted
// codec-3 artifact update replays verbatim under the legacy flattened
// projection after the upgrade: the recorded keys are applied exactly,
// and the codec-4 sub-namespaces are never introduced by replay.
func TestAdditionalArtifactInterruptedLegacyReplay(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "legacy-replay")
			version := mustVersion(t, backend, model, "1.0.0")
			mustPrimaryArtifact(t, backend, version, "s3://bucket/primary")
			additional, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Name: new("record-name"),
				CustomProperties: map[string]openapi.MetadataValue{
					"name": stringValue("property-name"),
				},
			}}, *version.Id)
			require.NoError(t, err)
			id := *additional.ModelArtifact.Id
			prefix := "rhoai.artifacts." + id + "."

			// Interrupt a v4 update mid-projection.
			backend.stub.failNextRequests("POST model-versions/set-tag", 1)
			_, err = backend.service.UpsertModelArtifact(&openapi.ModelArtifact{Id: &id, Description: new("legacy-edit")})
			require.ErrorIs(t, err, api.ErrUnavailable)

			// Rewrite the interrupted work as codec 3 with its legacy
			// owned-tag projection, and present the committed snapshot and
			// native tags in their legacy form.
			var operation mlflow.Operation
			require.NoError(t, backend.db.Where("kind = ? AND resource_id = ?", mlflow.OperationKindArtifactUpdate, mustObjectID(t, &id)).First(&operation).Error)
			var candidate mlflow.MutationCandidate
			require.NoError(t, json.Unmarshal([]byte(operation.CandidateJSON), &candidate))
			candidate.CodecVersion = 3
			candidate.Objects[0].Snapshot.CodecVersion = 3
			candidate.Objects[0].Snapshot.OwnedTags = map[string]string{
				prefix + "name":        "property-name",
				prefix + "description": "legacy-edit",
			}
			payload, err := json.Marshal(candidate)
			require.NoError(t, err)
			require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", operation.OperationUUID).
				Updates(map[string]any{"candidate_json": string(payload), "payload_hash": "legacy-hash", "request_hash": ""}).Error)
			rewriteArtifactSnapshotCodec(t, backend, id, 3, map[string]string{
				prefix + "name": "property-name",
			})
			setNativeAdditionalTags(t, backend, model.Name, "1", id, prefix, map[string]string{
				"name": "property-name",
			})

			expireLeases(t, backend)
			require.NoError(t, backend.newBackendService(t).ApplyOperation(t.Context(), operation.OperationUUID))
			tags := backend.stub.versionTags(model.Name, "1")
			assert.Equal(t, "legacy-edit", tags[prefix+"description"], "recorded legacy keys replay verbatim")
			assert.Equal(t, "property-name", tags[prefix+"name"])
			assert.NotContains(t, tags, prefix+"artifactRecord.description")
			assert.NotContains(t, tags, prefix+"artifact.name")
		})
	}
}

// sharedArtifactRoles attaches one artifact to two materialized versions
// in the requested role combination and returns it.
func sharedArtifactRoles(t *testing.T, backend *backendFixture, model *openapi.RegisteredModel,
	first, second *openapi.ModelVersion, roles string) *openapi.Artifact {
	t.Helper()
	switch roles {
	case "primary/primary":
		return sharedPrimary(t, backend, model, first, second, "s3://bucket/shared")
	case "additional/additional":
		mustPrimaryArtifact(t, backend, first, "s3://bucket/first")
		mustPrimaryArtifact(t, backend, second, "s3://bucket/second")
		shared, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
			Name: new("shared-extra"), Uri: new("s3://bucket/extra"), Description: new("shared-v1"),
			CustomProperties: map[string]openapi.MetadataValue{"note": stringValue("n1")},
		}}, *first.Id)
		require.NoError(t, err)
		shared, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: shared.ModelArtifact.Id}}, *second.Id)
		require.NoError(t, err)
		return shared
	case "primary/additional":
		shared := mustPrimaryArtifact(t, backend, first, "s3://bucket/shared")
		mustPrimaryArtifact(t, backend, second, "s3://bucket/second")
		_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: shared.ModelArtifact.Id}}, *second.Id)
		require.NoError(t, err)
		return shared
	default:
		t.Fatalf("unknown role combination %q", roles)
		return nil
	}
}

// assertSharedProjection checks one binding's native view after a shared
// metadata update: the role-correct keys carry the new value, the
// generation and source are unchanged, and unrelated tags survive.
func assertSharedProjection(t *testing.T, backend *backendFixture, model *openapi.RegisteredModel,
	version *openapi.ModelVersion, primary bool, artifactID, description, source string, hasNote bool, generation int64) {
	t.Helper()
	binding := bindingOf(t, backend, *version.Id)
	require.NotNil(t, binding.NativeVersionNumber)
	assert.Equal(t, generation, binding.ActiveGeneration, "a metadata edit never changes the generation")
	record := backend.stub.versionRecord(model.Name, *binding.NativeVersionNumber)
	assert.Equal(t, source, record["source"], "a metadata edit never changes the source")
	tags := backend.stub.versionTags(model.Name, *binding.NativeVersionNumber)
	if primary {
		assert.Equal(t, description, tags["rhoai.artifactRecord.description"])
	} else {
		assert.Equal(t, description, tags["rhoai.artifacts."+artifactID+".artifactRecord.description"])
		if hasNote {
			assert.Equal(t, "n1", tags["rhoai.artifacts."+artifactID+".artifact.note"])
		}
	}
	assert.Equal(t, "external-"+version.Name, tags["external-note"], "unrelated tags are preserved")
}

// TestSharedArtifactMetadataProjectsEveryBinding proves a metadata edit
// of an artifact shared by several materialized versions reaches every
// binding's native projection in that binding's role, regardless of
// which version the update is issued through.
func TestSharedArtifactMetadataProjectsEveryBinding(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		for _, roles := range []string{"primary/primary", "additional/additional", "primary/additional"} {
			t.Run(engine+"/"+roles, func(t *testing.T) {
				backend := newBackendFixture(t, fixture)
				model := mustModel(t, backend, "shared-"+strings.ReplaceAll(roles, "/", "-"))
				first := mustVersion(t, backend, model, "1.0.0")
				second := mustVersion(t, backend, model, "2.0.0")
				shared := sharedArtifactRoles(t, backend, model, first, second, roles)
				artifactID := *shared.ModelArtifact.Id
				firstSource, secondSource := "s3://bucket/first", "s3://bucket/second"
				if roles == "primary/primary" {
					firstSource, secondSource = "s3://bucket/shared", "s3://bucket/shared"
				}
				if roles == "primary/additional" {
					firstSource = "s3://bucket/shared"
				}
				firstPrimary, secondPrimary := false, false
				switch roles {
				case "primary/primary":
					firstPrimary, secondPrimary = true, true
				case "primary/additional":
					firstPrimary = true
				}
				for _, version := range []*openapi.ModelVersion{first, second} {
					binding := bindingOf(t, backend, *version.Id)
					backend.stub.mu.Lock()
					backend.stub.versions[model.Name][*binding.NativeVersionNumber].(map[string]any)["tags"].(map[string]string)["external-note"] = "external-" + version.Name
					backend.stub.mu.Unlock()
				}

				// Update through the first version: both bindings project.
				_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
					Id: shared.ModelArtifact.Id, Description: new("updated-through-first"),
				}}, *first.Id)
				require.NoError(t, err)
				assertSharedProjection(t, backend, model, first, firstPrimary, artifactID, "updated-through-first", firstSource, roles == "additional/additional", 1)
				assertSharedProjection(t, backend, model, second, secondPrimary, artifactID, "updated-through-first", secondSource, roles == "additional/additional", 1)

				// Update through the second version: both project again.
				_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
					Id: shared.ModelArtifact.Id, Description: new("updated-through-second"),
				}}, *second.Id)
				require.NoError(t, err)
				assertSharedProjection(t, backend, model, first, firstPrimary, artifactID, "updated-through-second", firstSource, roles == "additional/additional", 1)
				assertSharedProjection(t, backend, model, second, secondPrimary, artifactID, "updated-through-second", secondSource, roles == "additional/additional", 1)

				view, err := backend.service.GetModelArtifactById(artifactID)
				require.NoError(t, err)
				assert.Equal(t, "updated-through-second", *view.Description)
				assert.Zero(t, backend.stub.countRequests("UNEXPECTED"))
			})
		}
	}
}

// TestSharedArtifactPendingLinkSkipped proves a pending linked version
// alongside materialized ones is skipped by the fan-out — its artifact
// work stays in SQL — while the materialized bindings still project.
func TestSharedArtifactPendingLinkSkipped(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "shared-pending")
			first := mustVersion(t, backend, model, "1.0.0")
			second := mustVersion(t, backend, model, "2.0.0")
			mustPrimaryArtifact(t, backend, first, "s3://bucket/first")
			mustPrimaryArtifact(t, backend, second, "s3://bucket/second")
			shared, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Name: new("pending-extra"), Description: new("shared-v1"),
				CustomProperties: map[string]openapi.MetadataValue{"note": stringValue("n1")},
			}}, *first.Id)
			require.NoError(t, err)
			// A source-less artifact links to the pending version too.
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Id: shared.ModelArtifact.Id,
			}}, *second.Id)
			require.NoError(t, err)
			pending := mustVersion(t, backend, model, "3.0.0")
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Id: shared.ModelArtifact.Id,
			}}, *pending.Id)
			require.NoError(t, err)
			binding := bindingOf(t, backend, *pending.Id)
			assert.Nil(t, binding.NativeModelName, "the third version stays pending")

			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Id: shared.ModelArtifact.Id, Description: new("updated"),
			}}, *first.Id)
			require.NoError(t, err)
			tags := backend.stub.versionTags(model.Name, "1")
			assert.Equal(t, "updated", tags["rhoai.artifacts."+*shared.ModelArtifact.Id+".artifactRecord.description"])
			assert.Nil(t, bindingOf(t, backend, *pending.Id).NativeModelName, "the pending version never materializes from a metadata edit")
		})
	}
}
