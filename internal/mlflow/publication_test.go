package mlflow_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// aggregateFixture reserves a model, a pending version, and an
// artifact, and returns the pieces a multi-resource candidate needs.
type aggregateFixture struct {
	model    mlflow.Object
	version  mlflow.Object
	artifact mlflow.Object
}

func newAggregateFixture(t *testing.T, fixture *storeFixture) aggregateFixture {
	t.Helper()
	model, err := fixture.store.ReserveObject(t.Context(), reservedModel("aggregate-model"))
	require.NoError(t, err)
	version, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{
		Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: "1.0.0", Revision: 1,
	})
	require.NoError(t, err)
	artifact, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{
		Kind: mlflow.KindModelArtifact, Name: "aggregate-artifact", Revision: 1,
		CommittedJSON: `{"name":"aggregate-artifact","uri":""}`,
	})
	require.NoError(t, err)
	return aggregateFixture{model: model, version: version, artifact: artifact}
}

// seedSucceededResultOperation records a completed sibling operation
// whose native-result checkpoint publication resolves identities from.
func seedSucceededResultOperation(t *testing.T, fixture *storeFixture, resource mlflow.Object,
	uuid string, result any) mlflow.Operation {
	t.Helper()
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	operation := mlflow.Operation{
		OperationUUID:            uuid,
		ResourceID:               resource.ID,
		Kind:                     "version-native-create",
		ExpectedRevision:         resource.Revision,
		CandidateJSON:            `{"codecVersion":2}`,
		PayloadHash:              "hash-result",
		Generation:               1,
		Step:                     mlflow.StepPublished,
		State:                    mlflow.OperationSucceeded,
		NativeResult:             new(string(string(encoded))),
		FencingToken:             1,
		Attempts:                 1,
		CreateTimeSinceEpoch:     1,
		LastUpdateTimeSinceEpoch: 1,
	}
	require.NoError(t, fixture.db.Create(&operation).Error)
	return operation
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

// TestPublishAggregateSuccess proves a typed multi-resource candidate
// publishes objects, links, bindings, and history atomically, resolves
// native identities from verified result operations ( including the
// legacy plain-number checkpoint ), and supersedes prior generations.
func TestPublishAggregateSuccess(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)
			pieces := newAggregateFixture(t, fixture)

			// A prior generation exists and must be superseded.
			require.NoError(t, fixture.db.Create(&mlflow.VersionHistory{
				VersionID: pieces.version.ID, Generation: 1,
				NativeModelName: "native-model", NativeVersionNumber: "3",
				Source: "s3://bucket/old.onnx", OperationUUID: "prior-op",
			}).Error)

			result := mlflow.NativeBindingResult{
				ModelName: "native-model", VersionNumber: "5",
				Source: "s3://bucket/model.onnx", Generation: 2, Status: "READY",
			}
			resultOp := seedSucceededResultOperation(t, fixture, pieces.version,
				"27589fc6-ab04-42cc-9f8f-5f6a2c0e0f01", result)

			// The version is reserved until publication.
			require.NoError(t, fixture.db.Model(&mlflow.Object{}).
				Where("id = ?", pieces.version.ID).
				Update("visibility", mlflow.VisibilityReserved).Error)

			resourceJSON := `{"name":"1.0.0","externalId":"agg-ext","customProperties":{"count":{"metadataType":"MetadataIntValue","int_value":"42"}}}`
			candidate := mlflow.MutationCandidate{
				CodecVersion: 2,
				Objects: []mlflow.ObjectChange{{
					ID: pieces.version.ID, ExpectedRevision: pieces.version.Revision,
					Snapshot: mlflow.StoredSnapshot{
						CodecVersion: 2, Kind: mlflow.KindModelVersion,
						Resource:  json.RawMessage(resourceJSON),
						TagKeys:   map[string]string{"count": "count"},
						OwnedTags: map[string]string{"rhoai.versionName": "1.0.0"},
					},
				}},
				Links: []mlflow.ArtifactLink{
					{ArtifactID: pieces.artifact.ID, VersionID: pieces.version.ID},
				},
				Bindings: []mlflow.BindingChange{{
					Value: mlflow.VersionBinding{
						VersionID: pieces.version.ID, ModelID: pieces.model.ID,
						Phase: mlflow.PhaseReady, ActiveGeneration: 2, DesiredGeneration: 2,
						Alias:           mlflow.NormalizedAlias("1.0.0"),
						NativeModelName: new("native-model"),
					},
					ResultOperationUUID: resultOp.OperationUUID,
				}},
				History: []mlflow.HistoryChange{{
					Value: mlflow.VersionHistory{
						VersionID: pieces.version.ID, Generation: 2,
						NativeModelName: "native-model", NativeVersionNumber: "5",
						Source: "s3://bucket/model.onnx", OperationUUID: "publish-op",
					},
					SupersedePrevious: true,
				}},
			}

			operation, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "27589fc6-ab04-42cc-9f8f-5f6a2c0e0f02",
				ResourceID:       pieces.version.ID,
				Kind:             "version-materialize",
				ExpectedRevision: pieces.version.Revision,
				CandidateJSON:    mustJSON(t, candidate),
				PayloadHash:      "hash-aggregate",
				Generation:       2,
			})
			require.NoError(t, err)
			_, err = fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-1", 0)
			require.ErrorIs(t, err, api.ErrBadRequest, "lease must be positive")
			claimed, err := fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-1", time.Minute)
			require.NoError(t, err)
			require.NoError(t, fixture.store.PublishOperation(ctx, operation.OperationUUID, claimed.FencingToken))

			// The object carries the full stored snapshot; the public
			// decode extracts only the resource.
			published, err := fixture.store.GetObject(ctx, pieces.version.ID)
			require.NoError(t, err)
			require.Equal(t, pieces.version.Revision+1, published.Revision)
			require.Equal(t, mlflow.VisibilityCommitted, published.Visibility)
			require.NotNil(t, published.ExternalID)
			require.Equal(t, "agg-ext", *published.ExternalID)
			public, err := mlflow.DecodePublicSnapshot(published.CommittedJSON)
			require.NoError(t, err)
			require.JSONEq(t, resourceJSON, string(public))
			assert.NotContains(t, string(public), "tagKeys",
				"private snapshot fields must not enter the public decode")

			// The link is committed.
			var linkCount int64
			require.NoError(t, fixture.db.Model(&mlflow.ArtifactLink{}).
				Where("artifact_id = ? AND version_id = ?", pieces.artifact.ID, pieces.version.ID).
				Count(&linkCount).Error)
			require.Equal(t, int64(1), linkCount)

			// The binding resolved its native identity from the result
			// operation's checkpoint.
			var binding mlflow.VersionBinding
			require.NoError(t, fixture.db.Where("version_id = ?", pieces.version.ID).First(&binding).Error)
			require.Equal(t, mlflow.PhaseReady, binding.Phase)
			require.NotNil(t, binding.NativeVersionNumber)
			require.Equal(t, "5", *binding.NativeVersionNumber)
			require.Equal(t, int64(1), binding.Revision)

			// The new history generation is recorded and the prior one
			// superseded.
			var superseded, active int64
			require.NoError(t, fixture.db.Model(&mlflow.VersionHistory{}).
				Where("version_id = ? AND superseded = ?", pieces.version.ID, true).
				Count(&superseded).Error)
			require.NoError(t, fixture.db.Model(&mlflow.VersionHistory{}).
				Where("version_id = ? AND superseded = ?", pieces.version.ID, false).
				Count(&active).Error)
			require.Equal(t, int64(1), superseded)
			require.Equal(t, int64(1), active)

			done, err := fixture.store.GetOperation(ctx, operation.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, mlflow.OperationSucceeded, done.State)

			// A legacy plain-number checkpoint resolves as a version
			// number alone.
			legacyResource := pieces.version
			legacyResource.Revision++
			legacy := seedSucceededResultOperation(t, fixture, legacyResource,
				"27589fc6-ab04-42cc-9f8f-5f6a2c0e0f03", "17")
			require.Equal(t, `"17"`, *legacy.NativeResult)
		})
	}
}

// TestPublishAggregateRollback injects a failure after each SQL write
// of an aggregate publication and proves every prior row, revision,
// timestamp, and visibility remains unchanged.
func TestPublishAggregateRollback(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)
			pieces := newAggregateFixture(t, fixture)

			result := mlflow.NativeBindingResult{
				ModelName: "native-model", VersionNumber: "5",
				Source: "s3://bucket/model.onnx", Generation: 1, Status: "READY",
			}
			resultOp := seedSucceededResultOperation(t, fixture, pieces.version,
				"37589fc6-ab04-42cc-9f8f-5f6a2c0e0f01", result)

			candidate := mlflow.MutationCandidate{
				CodecVersion: 2,
				Objects: []mlflow.ObjectChange{
					{ID: pieces.version.ID, ExpectedRevision: pieces.version.Revision,
						Snapshot: mlflow.StoredSnapshot{CodecVersion: 2, Kind: mlflow.KindModelVersion,
							Resource: json.RawMessage(`{"name":"1.0.0","description":"updated"}`)}},
					{ID: pieces.artifact.ID, ExpectedRevision: pieces.artifact.Revision,
						Snapshot: mlflow.StoredSnapshot{CodecVersion: 2, Kind: mlflow.KindModelArtifact,
							Resource: json.RawMessage(`{"name":"aggregate-artifact","uri":"s3://bucket/model.onnx"}`)}},
				},
				Links: []mlflow.ArtifactLink{
					{ArtifactID: pieces.artifact.ID, VersionID: pieces.version.ID},
				},
				Bindings: []mlflow.BindingChange{{
					Value: mlflow.VersionBinding{
						VersionID: pieces.version.ID, ModelID: pieces.model.ID,
						Phase: mlflow.PhaseReady, ActiveGeneration: 1, DesiredGeneration: 1,
						Alias: mlflow.NormalizedAlias("1.0.0"),
					},
					ResultOperationUUID: resultOp.OperationUUID,
				}},
				History: []mlflow.HistoryChange{{
					Value: mlflow.VersionHistory{
						VersionID: pieces.version.ID, Generation: 1,
						NativeModelName: "native-model", NativeVersionNumber: "5",
						Source: "s3://bucket/model.onnx", OperationUUID: "rollback-op",
					},
				}},
			}

			operation, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "37589fc6-ab04-42cc-9f8f-5f6a2c0e0f02",
				ResourceID:       pieces.version.ID,
				Kind:             "version-materialize",
				ExpectedRevision: pieces.version.Revision,
				CandidateJSON:    mustJSON(t, candidate),
				PayloadHash:      "hash-rollback",
				Generation:       1,
			})
			require.NoError(t, err)
			claimed, err := fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-1", time.Minute)
			require.NoError(t, err)

			beforeVersion, err := fixture.store.GetObject(ctx, pieces.version.ID)
			require.NoError(t, err)
			beforeArtifact, err := fixture.store.GetObject(ctx, pieces.artifact.ID)
			require.NoError(t, err)

			// The publication performs six SQL writes: two object
			// updates, the link insert, the binding insert, the history
			// insert, and the operation completion. Fail after each in
			// turn.
			const totalWrites = 6
			for failAfter := 1; failAfter <= totalWrites; failAfter++ {
				t.Run(fmt.Sprintf("fail after write %d", failAfter), func(t *testing.T) {
					writes := 0
					fail := func(tx *gorm.DB) {
						writes++
						if writes == failAfter {
							_ = tx.AddError(fmt.Errorf("injected failure after write %d", failAfter))
						}
					}
					require.NoError(t, fixture.db.Callback().Create().After("gorm:create").Register("test:fail_write", fail))
					require.NoError(t, fixture.db.Callback().Update().After("gorm:update").Register("test:fail_write", fail))
					t.Cleanup(func() {
						_ = fixture.db.Callback().Create().Remove("test:fail_write")
						_ = fixture.db.Callback().Update().Remove("test:fail_write")
					})

					err := fixture.store.PublishOperation(ctx, operation.OperationUUID, claimed.FencingToken)
					require.Error(t, err, "write %d must fail", failAfter)

					// Every row is unchanged.
					afterVersion, err := fixture.store.GetObject(ctx, pieces.version.ID)
					require.NoError(t, err)
					require.Equal(t, beforeVersion, afterVersion)
					afterArtifact, err := fixture.store.GetObject(ctx, pieces.artifact.ID)
					require.NoError(t, err)
					require.Equal(t, beforeArtifact, afterArtifact)

					var bindingCount, historyCount, linkCount int64
					require.NoError(t, fixture.db.Model(&mlflow.VersionBinding{}).
						Where("version_id = ?", pieces.version.ID).Count(&bindingCount).Error)
					require.NoError(t, fixture.db.Model(&mlflow.VersionHistory{}).
						Where("version_id = ?", pieces.version.ID).Count(&historyCount).Error)
					require.NoError(t, fixture.db.Model(&mlflow.ArtifactLink{}).
						Where("version_id = ?", pieces.version.ID).Count(&linkCount).Error)
					require.Zero(t, bindingCount)
					require.Zero(t, historyCount)
					require.Zero(t, linkCount)

					afterOp, err := fixture.store.GetOperation(ctx, operation.OperationUUID)
					require.NoError(t, err)
					require.Equal(t, mlflow.OperationRunning, afterOp.State)
					require.NotEqual(t, mlflow.StepPublished, afterOp.Step)
				})
			}

			// With no injected failure the same publication completes.
			require.NoError(t, fixture.store.PublishOperation(ctx, operation.OperationUUID, claimed.FencingToken))
			done, err := fixture.store.GetOperation(ctx, operation.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, mlflow.OperationSucceeded, done.State)
		})
	}
}

// TestPublishCandidateValidation proves malformed candidates are
// rejected without changing SQL state.
func TestPublishCandidateValidation(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)
			pieces := newAggregateFixture(t, fixture)
			otherModel, err := fixture.store.ReserveObject(ctx, reservedModel("aggregate-other-model"))
			require.NoError(t, err)

			// Each case reserves a fresh version: a failed publication
			// leaves its intent active on the resource, so cases cannot
			// share one.
			caseNumber := 0
			newCase := func() mlflow.Object {
				caseNumber++
				version, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{
					Kind: mlflow.KindModelVersion, ParentID: pieces.model.ID,
					Name: fmt.Sprintf("1.%d.0", caseNumber), Revision: 1,
				})
				require.NoError(t, err)
				return version
			}
			resultOpFor := func(version mlflow.Object) mlflow.Operation {
				return seedSucceededResultOperation(t, fixture, version,
					fmt.Sprintf("47589fc6-ab04-42cc-9f8f-%012d", caseNumber), mlflow.NativeBindingResult{
						ModelName: "native-model", VersionNumber: "5",
						Generation: 1, Status: "READY",
					})
			}
			prepare := func(version mlflow.Object, candidateJSON string) (mlflow.Operation, error) {
				return fixture.store.PrepareOperation(ctx, mlflow.Operation{
					OperationUUID:    fmt.Sprintf("57589fc6-ab04-42cc-9f8f-%012d", caseNumber),
					ResourceID:       version.ID,
					Kind:             "version-materialize",
					ExpectedRevision: version.Revision,
					CandidateJSON:    candidateJSON,
					PayloadHash:      fmt.Sprintf("hash-%d", caseNumber),
					Generation:       1,
				})
			}
			claimAndPublish := func(t *testing.T, operation mlflow.Operation) error {
				claimed, err := fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-1", time.Minute)
				if err != nil {
					return err
				}
				return fixture.store.PublishOperation(ctx, operation.OperationUUID, claimed.FencingToken)
			}

			// Unknown codecs are now rejected before any graph is persisted.
			_, err = prepare(newCase(), `{"codecVersion":99,"objects":[]}`)
			require.ErrorIs(t, err, api.ErrBadRequest)

			// Duplicate resource entries fail preparation before reserving anything.
			dupVersion := newCase()
			_, err = prepare(dupVersion, mustJSON(t, mlflow.MutationCandidate{
				CodecVersion: 2,
				Objects: []mlflow.ObjectChange{
					{ID: dupVersion.ID, ExpectedRevision: 1,
						Snapshot: mlflow.StoredSnapshot{CodecVersion: 2, Kind: mlflow.KindModelVersion,
							Resource: json.RawMessage(`{}`)}},
					{ID: dupVersion.ID, ExpectedRevision: 1,
						Snapshot: mlflow.StoredSnapshot{CodecVersion: 2, Kind: mlflow.KindModelVersion,
							Resource: json.RawMessage(`{}`)}},
				},
			}))
			require.ErrorIs(t, err, api.ErrConflict)

			// A link referencing a non-artifact is rejected mid-
			// transaction with no rows changed.
			linkVersion := newCase()
			badLink, err := prepare(linkVersion, mustJSON(t, mlflow.MutationCandidate{
				CodecVersion: 2,
				Links: []mlflow.ArtifactLink{
					{ArtifactID: pieces.model.ID, VersionID: linkVersion.ID},
				},
			}))
			require.NoError(t, err)
			require.ErrorIs(t, claimAndPublish(t, badLink), api.ErrBadRequest)
			var linkCount int64
			require.NoError(t, fixture.db.Model(&mlflow.ArtifactLink{}).
				Where("version_id = ?", linkVersion.ID).Count(&linkCount).Error)
			require.Zero(t, linkCount)

			// A binding referencing the wrong parent model is rejected.
			bindingVersion := newCase()
			badBinding, err := prepare(bindingVersion, mustJSON(t, mlflow.MutationCandidate{
				CodecVersion: 2,
				Bindings: []mlflow.BindingChange{{
					Value: mlflow.VersionBinding{
						VersionID: bindingVersion.ID, ModelID: otherModel.ID,
						Phase: mlflow.PhasePendingSource,
					},
				}},
			}))
			require.NoError(t, err)
			require.ErrorIs(t, claimAndPublish(t, badBinding), api.ErrBadRequest)

			// A missing result operation is a conflict, not a guess.
			missingVersion := newCase()
			missingResult, err := prepare(missingVersion, mustJSON(t, mlflow.MutationCandidate{
				CodecVersion: 2,
				Bindings: []mlflow.BindingChange{{
					Value: mlflow.VersionBinding{
						VersionID: missingVersion.ID, ModelID: pieces.model.ID,
						Phase: mlflow.PhaseReady, ActiveGeneration: 1, DesiredGeneration: 1,
						NativeModelName: new("native-model"),
					},
					ResultOperationUUID: "47589fc6-ab04-42cc-9f8f-ffffffffffff",
				}},
			}))
			require.NoError(t, err)
			require.ErrorIs(t, claimAndPublish(t, missingResult), api.ErrNotFound)

			// A mismatched result ( wrong model ) is a conflict.
			mismatchVersion := newCase()
			mismatched, err := prepare(mismatchVersion, mustJSON(t, mlflow.MutationCandidate{
				CodecVersion: 2,
				Bindings: []mlflow.BindingChange{{
					Value: mlflow.VersionBinding{
						VersionID: mismatchVersion.ID, ModelID: pieces.model.ID,
						Phase: mlflow.PhaseReady, ActiveGeneration: 1, DesiredGeneration: 1,
						NativeModelName: new("other-native-model"),
					},
					ResultOperationUUID: resultOpFor(mismatchVersion).OperationUUID,
				}},
			}))
			require.NoError(t, err)
			require.ErrorIs(t, claimAndPublish(t, mismatched), api.ErrConflict)

			// An existing binding with the wrong expected revision is a
			// conflict.
			staleVersion := newCase()
			require.NoError(t, fixture.db.Create(&mlflow.VersionBinding{
				VersionID: staleVersion.ID, ModelID: pieces.model.ID,
				Phase: mlflow.PhasePendingSource, ActiveGeneration: 1, DesiredGeneration: 1,
				Alias: "alias", Revision: 1,
			}).Error)
			_, err = prepare(staleVersion, mustJSON(t, mlflow.MutationCandidate{
				CodecVersion: 2,
				Bindings: []mlflow.BindingChange{{
					ExpectedRevision: 5,
					Value: mlflow.VersionBinding{
						VersionID: staleVersion.ID, ModelID: pieces.model.ID,
						Phase: mlflow.PhaseReady, ActiveGeneration: 1, DesiredGeneration: 1,
					},
				}},
			}))
			require.ErrorIs(t, err, api.ErrConflict)
			var binding mlflow.VersionBinding
			require.NoError(t, fixture.db.Where("version_id = ?", staleVersion.ID).First(&binding).Error)
			require.Equal(t, mlflow.PhasePendingSource, binding.Phase, "the refused publication must not change the binding")

			// An empty candidate completes without changing anything.
			emptyVersion := newCase()
			empty, err := prepare(emptyVersion, mustJSON(t, mlflow.MutationCandidate{CodecVersion: 2}))
			require.NoError(t, err)
			require.NoError(t, claimAndPublish(t, empty))
			after, err := fixture.store.GetObject(ctx, emptyVersion.ID)
			require.NoError(t, err)
			require.Equal(t, emptyVersion.Revision, after.Revision)
			require.Equal(t, emptyVersion.CommittedJSON, after.CommittedJSON)
		})
	}
}

// TestSnapshotPublicDecoding proves typed resource payloads survive
// committed storage byte-for-byte — integer strings are never decoded
// through float64, proto payloads and type URLs round-trip — and that
// only the resource is exposed publicly.
func TestSnapshotPublicDecoding(t *testing.T) {
	resourceJSON := `{"name":"1.0.0","externalId":"snap-ext","customProperties":{"big":{"metadataType":"MetadataIntValue","int_value":"999999999"},"proto":{"metadataType":"MetadataProtoValue","type":"type.googleapis.com/foo.Bar","proto_value":"cHJvdG8="},"dbl":{"metadataType":"MetadataDoubleValue","double_value":0.1}}}`
	snapshot := mlflow.StoredSnapshot{
		CodecVersion: 2, Kind: mlflow.KindModelVersion,
		Resource:  json.RawMessage(resourceJSON),
		TagKeys:   map[string]string{"big": "big"},
		OwnedTags: map[string]string{"rhoai.versionName": "1.0.0"},
	}
	committed, err := json.Marshal(snapshot)
	require.NoError(t, err)

	public, err := mlflow.DecodePublicSnapshot(string(committed))
	require.NoError(t, err)
	require.JSONEq(t, resourceJSON, string(public))
	assert.NotContains(t, string(public), "tagKeys")
	assert.NotContains(t, string(public), "ownedTags")

	// Integer strings and doubles survive exactly, not through float64.
	var resource struct {
		CustomProperties map[string]json.RawMessage `json:"customProperties"`
	}
	require.NoError(t, json.Unmarshal(public, &resource))
	assert.JSONEq(t, `{"metadataType":"MetadataIntValue","int_value":"999999999"}`,
		string(resource.CustomProperties["big"]))
	assert.JSONEq(t, `{"metadataType":"MetadataDoubleValue","double_value":0.1}`,
		string(resource.CustomProperties["dbl"]))

	// A legacy flat snapshot is returned unchanged.
	legacy := `{"name":"legacy-model","externalId":"legacy-ext"}`
	public, err = mlflow.DecodePublicSnapshot(legacy)
	require.NoError(t, err)
	require.JSONEq(t, legacy, string(public))

	// Unknown stored versions are refused.
	_, err = mlflow.DecodePublicSnapshot(`{"codecVersion":7,"resource":{}}`)
	require.ErrorIs(t, err, api.ErrBadRequest)
}
