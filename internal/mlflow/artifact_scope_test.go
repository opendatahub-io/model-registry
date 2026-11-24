package mlflow_test

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/require"
)

func TestArtifactCreationScope(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "scope-model")
			first := mustVersion(t, backend, model, "1")
			second := mustVersion(t, backend, model, "2")
			a := mustPrimaryArtifact(t, backend, first, "s3://bucket/shared.onnx")
			b := mustPrimaryArtifact(t, backend, second, "s3://bucket/shared.onnx")
			require.NotEqual(t, *a.ModelArtifact.Id, *b.ModelArtifact.Id)
			for _, version := range []*openapi.ModelVersion{first, second} {
				found, err := backend.service.GetModelArtifactByParams(new("primary"), version.Id, nil)
				require.NoError(t, err)
				require.Equal(t, "s3://bucket/shared.onnx", *found.Uri)
			}
			retry, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Name: new("primary"), Uri: new("s3://bucket/shared.onnx"),
			}}, *first.Id)
			require.NoError(t, err)
			require.Equal(t, a.ModelArtifact.Id, retry.ModelArtifact.Id)
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{
				Name: new("primary"), Uri: new("s3://bucket/other.onnx"), Description: new("different create"),
			}}, *first.Id)
			require.ErrorIs(t, err, api.ErrConflict)
			require.Equal(t, 2, backend.stub.countRequests("POST model-versions/create"))
		})
	}
}

func TestArtifactCreationScopeIDOnlySharing(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "shared-scope-model")
			created, err := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{
				Name: new("standalone"), Uri: new("s3://bucket/shared.onnx"), Description: new("keep me"),
			})
			require.NoError(t, err)
			for _, name := range []string{"1", "2"} {
				version := mustVersion(t, backend, model, name)
				attached, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: created.Id}}, *version.Id)
				require.NoError(t, err)
				require.Equal(t, created.Id, attached.ModelArtifact.Id)
				require.Equal(t, created.Uri, attached.ModelArtifact.Uri)
				require.Equal(t, created.Description, attached.ModelArtifact.Description)
				binding := bindingOf(t, backend, *version.Id)
				require.Equal(t, mlflow.PhaseReady, binding.Phase)
				require.Equal(t, mustObjectID(t, created.Id), *binding.PrimaryArtifactID)
			}
			require.Equal(t, 2, backend.stub.countRequests("POST model-versions/create"))
			_, err = backend.service.UpsertModelArtifact(&openapi.ModelArtifact{Id: created.Id, Description: new("changed"), Name: new("ignored protected name"), CreateTimeSinceEpoch: new("1")})
			require.NoError(t, err)
			updated, err := backend.service.GetModelArtifactById(*created.Id)
			require.NoError(t, err)
			require.Equal(t, created.Name, updated.Name)
			require.Equal(t, created.Uri, updated.Uri)
			require.Equal(t, created.CreateTimeSinceEpoch, updated.CreateTimeSinceEpoch)
			require.Equal(t, 2, backend.stub.countRequests("POST model-versions/create"))
		})
	}
}

func TestStandaloneCreation(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			t.Run("nameless independent identities", func(t *testing.T) {
				first, err := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{Uri: new("s3://bucket/shared.onnx"), Description: new("first")})
				require.NoError(t, err)
				second, err := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{Uri: new("s3://bucket/shared.onnx"), Description: new("second")})
				require.NoError(t, err)
				require.NotEqual(t, *first.Id, *second.Id)
				_, err = uuid.Parse(*first.Name)
				require.NoError(t, err)
			})
			t.Run("named creates conflict", func(t *testing.T) {
				first, err := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{Name: new("unique"), Description: new("first")})
				require.NoError(t, err)
				_, err = backend.service.UpsertModelArtifact(&openapi.ModelArtifact{Name: new("unique"), Description: new("second")})
				require.ErrorIs(t, err, api.ErrConflict)
				got, err := backend.service.GetModelArtifactById(*first.Id)
				require.NoError(t, err)
				require.Equal(t, first.Description, got.Description)
			})
		})
	}
}

func TestImportedArtifactLookup(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "external", "", nil)
			for _, number := range []string{"1", "2"} {
				seedNativeVersion(t, backend, "external", number, "s3://bucket/shared.onnx", "", nil)
			}
			require.NoError(t, backend.service.Refresh(t.Context()))
			for _, version := range committedObjects(t, backend, mlflow.KindModelVersion) {
				versionID := new(strconv.FormatInt(int64(version.ID), 10))
				found, err := backend.service.GetModelArtifactByParams(new("external"), versionID, nil)
				require.NoError(t, err)
				require.Equal(t, "external", *found.Name)
				listed, err := backend.service.GetModelArtifacts(api.ListOptions{FilterQuery: new("name = 'external'")}, versionID)
				require.NoError(t, err)
				require.Len(t, listed.Items, 1)
				require.Equal(t, found.Id, listed.Items[0].Id)
			}
		})
	}
}

func TestImportedArtifactLookupRepairsOnlyProvenIdleImports(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedNativeModel(t, backend, "legacy", "", nil)
			for _, n := range []string{"1", "2", "3", "4", "5"} {
				seedNativeVersion(t, backend, "legacy", n, "s3://bucket/legacy.onnx", "", nil)
			}
			require.NoError(t, backend.service.Refresh(t.Context()))
			versions := committedObjects(t, backend, mlflow.KindModelVersion)
			var artifacts []mlflow.Object
			for _, v := range versions {
				binding := bindingOf(t, backend, strconv.FormatInt(int64(v.ID), 10))
				var a mlflow.Object
				require.NoError(t, backend.db.First(&a, *binding.PrimaryArtifactID).Error)
				var snapshot mlflow.StoredSnapshot
				require.NoError(t, json.Unmarshal([]byte(a.CommittedJSON), &snapshot))
				var resource openapi.ModelArtifact
				require.NoError(t, json.Unmarshal(snapshot.Resource, &resource))
				resource.Name = new("primary")
				resourceJSON, err := json.Marshal(resource)
				require.NoError(t, err)
				snapshot.Resource = resourceJSON
				encoded, err := json.Marshal(snapshot)
				require.NoError(t, err)
				a.Name = fmt.Sprintf("native-legacy-%s", v.Name)
				a.CommittedJSON = string(encoded)
				a.ArtifactScopeID = 0
				require.NoError(t, backend.db.Save(&a).Error)
				artifacts = append(artifacts, a)
			}
			// A prefix alone does not prove import provenance.
			require.NoError(t, backend.db.Model(&mlflow.VersionHistory{}).Where("version_id = ?", versions[1].ID).Update("operation_uuid", "local-create").Error)
			// Active candidate evidence must remain untouched until it resolves.
			require.NoError(t, backend.db.Create(&mlflow.Operation{OperationUUID: "active-legacy", ResourceID: artifacts[2].ID, Kind: "artifact-update", ExpectedRevision: artifacts[2].Revision, CandidateJSON: "{}", PayloadHash: "retained", State: mlflow.OperationPending, Step: mlflow.StepPrepared}).Error)
			// Existing explicit sharing must never be split or renamed incidentally.
			require.NoError(t, backend.db.Create(&mlflow.ArtifactLink{ArtifactID: artifacts[3].ID, VersionID: versions[0].ID}).Error)
			var originalLinks []mlflow.ArtifactLink
			var originalHistory []mlflow.VersionHistory
			require.NoError(t, backend.db.Order("artifact_id, version_id").Find(&originalLinks).Error)
			require.NoError(t, backend.db.Order("id").Find(&originalHistory).Error)
			require.NoError(t, backend.service.Refresh(t.Context()))
			for i, prior := range artifacts {
				got, err := fixture.store.GetObject(t.Context(), prior.ID)
				require.NoError(t, err)
				require.Equal(t, prior.CreateTimeSinceEpoch, got.CreateTimeSinceEpoch)
				if i == 0 || i == 4 {
					require.Equal(t, "legacy", got.Name)
					require.Equal(t, versions[i].ID, got.ArtifactScopeID)
					view, err := backend.service.GetModelArtifactById(strconv.FormatInt(int64(got.ID), 10))
					require.NoError(t, err)
					require.Equal(t, "legacy", *view.Name)
				} else {
					require.Equal(t, prior.Name, got.Name)
					require.Equal(t, prior.CommittedJSON, got.CommittedJSON)
				}
			}
			for _, i := range []int{0, 4} {
				found, err := backend.service.GetModelArtifactByParams(new("legacy"), new(strconv.FormatInt(int64(versions[i].ID), 10)), nil)
				require.NoError(t, err)
				require.Equal(t, artifacts[i].ID, mustObjectID(t, found.Id))
			}
			var audits []mlflow.Operation
			require.NoError(t, backend.db.Where("kind = ?", "import-artifact-name-correction").Find(&audits).Error)
			require.Len(t, audits, 2)
			for _, audit := range audits {
				require.Equal(t, mlflow.OperationSucceeded, audit.State)
				require.Contains(t, audit.CandidateJSON, `"previousName":"native-legacy-`)
			}
			var links []mlflow.ArtifactLink
			var history []mlflow.VersionHistory
			require.NoError(t, backend.db.Order("artifact_id, version_id").Find(&links).Error)
			require.NoError(t, backend.db.Order("id").Find(&history).Error)
			require.Equal(t, originalLinks, links)
			require.Equal(t, originalHistory, history)
			require.NoError(t, backend.db.Model(&mlflow.Operation{}).Where("operation_uuid = ?", "active-legacy").Update("state", mlflow.OperationFailed).Error)
			require.NoError(t, backend.service.Refresh(t.Context()))
			repaired, err := fixture.store.GetObject(t.Context(), artifacts[2].ID)
			require.NoError(t, err)
			require.Equal(t, "legacy", repaired.Name)
		})
	}
}

func TestArtifactScopeStore(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			model, err := fixture.store.ReserveObject(ctx, mlflow.Object{Kind: mlflow.KindRegisteredModel, Name: "scope-store"})
			require.NoError(t, err)
			version, err := fixture.store.ReserveObject(ctx, mlflow.Object{Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: "1"})
			require.NoError(t, err)
			for _, object := range []mlflow.Object{
				{Kind: mlflow.KindModelArtifact, Name: "negative", ArtifactScopeID: -1},
				{Kind: mlflow.KindModelArtifact, Name: "overflow", ArtifactScopeID: mlflow.MaxKubeflowID + 1},
				{Kind: mlflow.KindModelArtifact, Name: "model-kind", ArtifactScopeID: model.ID},
				{Kind: mlflow.KindModelArtifact, Name: "missing", ArtifactScopeID: 888888888},
				{Kind: mlflow.KindRegisteredModel, Name: "scoped-model", ArtifactScopeID: version.ID},
				{Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: "scoped-version", ArtifactScopeID: version.ID},
			} {
				_, err := fixture.store.ReserveObject(ctx, object)
				require.ErrorIs(t, err, api.ErrBadRequest)
			}
			standalone, err := fixture.store.ReserveObject(ctx, mlflow.Object{Kind: mlflow.KindModelArtifact, Name: "exact", ExternalID: new("scoped-external")})
			require.NoError(t, err)
			scoped, err := fixture.store.ReserveObject(ctx, mlflow.Object{Kind: mlflow.KindModelArtifact, Name: "exact", ArtifactScopeID: version.ID})
			require.NoError(t, err)
			for _, name := range []string{"Exact", "exact "} {
				_, err := fixture.store.ReserveObject(ctx, mlflow.Object{Kind: mlflow.KindModelArtifact, Name: name, ArtifactScopeID: version.ID})
				require.NoError(t, err)
			}
			_, err = fixture.store.ReserveObject(ctx, mlflow.Object{Kind: mlflow.KindModelArtifact, Name: "different", ArtifactScopeID: version.ID, ExternalID: new("scoped-external")})
			require.ErrorIs(t, err, api.ErrConflict)
			found, err := fixture.store.FindStandaloneArtifact(ctx, "exact")
			require.NoError(t, err)
			require.Equal(t, standalone.ID, found.ID)
			found, err = fixture.store.FindArtifactInVersion(ctx, "exact", version.ID)
			require.NoError(t, err)
			require.Equal(t, scoped.ID, found.ID)
			_, err = fixture.store.FindArtifactInVersion(ctx, "exact  ", version.ID)
			require.ErrorIs(t, err, api.ErrNotFound)
			// Old shared scope-zero rows remain discoverable through their links.
			legacy, err := fixture.store.ReserveObject(ctx, mlflow.Object{Kind: mlflow.KindModelArtifact, Name: "legacy"})
			require.NoError(t, err)
			require.NoError(t, fixture.db.Create(&mlflow.ArtifactLink{ArtifactID: legacy.ID, VersionID: version.ID}).Error)
			found, err = fixture.store.FindArtifactInVersion(ctx, "legacy", version.ID)
			require.NoError(t, err)
			require.Equal(t, legacy.ID, found.ID)
			require.Zero(t, found.ArtifactScopeID)
		})
	}
}

func TestArtifactCreationScopeReservedPayload(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "scope-reserved")
			version := mustVersion(t, backend, model, "1")
			request := &openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Name: new("weights"), Uri: new("s3://bucket/weights.onnx"), Description: new("original")}}
			backend.stub.mu.Lock()
			backend.stub.commitThenFail = 1
			backend.stub.mu.Unlock()
			_, err := backend.service.UpsertModelVersionArtifact(request, *version.Id)
			require.ErrorIs(t, err, api.ErrUnavailable)
			var reserved mlflow.Object
			require.NoError(t, backend.db.Where("kind = ? AND name = ?", mlflow.KindModelArtifact, "weights").First(&reserved).Error)
			// Simulate an upgrade of an unlinked legacy reservation; immutable
			// journal links identify its version even while the row stays scope zero.
			require.NoError(t, backend.db.Model(&mlflow.Object{}).Where("id = ?", reserved.ID).Update("artifact_scope_id", 0).Error)
			changed := *request.ModelArtifact
			changed.Description = new("conflicting")
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &changed}, *version.Id)
			require.ErrorIs(t, err, api.ErrConflict)
			retry, err := backend.service.UpsertModelVersionArtifact(request, *version.Id)
			require.NoError(t, err)
			require.Equal(t, reserved.ID, mustObjectID(t, retry.ModelArtifact.Id))
			require.Equal(t, 1, backend.stub.countRequests("POST model-versions/create"))
			require.Equal(t, "original", *retry.ModelArtifact.Description)
		})
	}
}
