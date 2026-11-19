package mlflow_test

import (
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustVersion creates a committed pending model version for artifact
// tests.
func mustVersion(t *testing.T, backend *backendFixture, model *openapi.RegisteredModel,
	name string) *openapi.ModelVersion {
	t.Helper()
	version, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{Name: name}, model.Id)
	require.NoError(t, err)
	return version
}

// mustPrimaryArtifact attaches a source-bearing model artifact to a
// version, materializing it.
func mustPrimaryArtifact(t *testing.T, backend *backendFixture, version *openapi.ModelVersion,
	uri string) *openapi.Artifact {
	t.Helper()
	artifact, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
		ModelArtifact: &openapi.ModelArtifact{
			Name: new("primary"), Uri: new(uri),
		},
	}, *version.Id)
	require.NoError(t, err)
	return artifact
}

// bindingOf loads the committed version binding.
func bindingOf(t *testing.T, backend *backendFixture, versionID string) mlflow.VersionBinding {
	t.Helper()
	var binding mlflow.VersionBinding
	require.NoError(t, backend.db.Where("version_id = ?", mustObjectID(t, new(versionID))).
		First(&binding).Error)
	return binding
}

// TestArtifactMaterializesVersionProjection proves the version's own
// projection — its logical name, author, and custom properties —
// reaches the native model version at materialization. Those tags live
// in the version's SQL snapshot, and the update path only pushes tags
// that changed after creation, so materialization must carry them or
// the native view could never reconstruct the logical version name.
func TestArtifactMaterializesVersionProjection(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "projected-model")
			version, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name:   "1.0.0",
				Author: new("the-author"),
				CustomProperties: map[string]openapi.MetadataValue{
					"flavor": stringValue("vanilla"),
				},
			}, model.Id)
			require.NoError(t, err)

			mustPrimaryArtifact(t, backend, version, "s3://bucket/model.onnx")

			tags := backend.stub.versionTags("projected-model", "1")
			assert.Equal(t, "1.0.0", tags["rhoai.versionName"])
			assert.Equal(t, "the-author", tags["rhoai.author"])
			assert.Equal(t, "vanilla", tags["flavor"])

			// The update path still synchronizes changed tags, and the
			// projection stays present after a metadata-only edit.
			updated, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Id: version.Id, CustomProperties: map[string]openapi.MetadataValue{
					"flavor": stringValue("chocolate"),
				},
			}, model.Id)
			require.NoError(t, err)
			require.NotNil(t, updated.Id)
			tags = backend.stub.versionTags("projected-model", "1")
			assert.Equal(t, "chocolate", tags["flavor"])
			assert.Equal(t, "1.0.0", tags["rhoai.versionName"])
		})
	}
}

// TestArtifactMaterializesNativeVersion proves the Python-model
// sequence — model, version, artifact — ends with one native
// model-version create carrying the real source and the adapter's
// ownership tags, an adapter-owned alias, and an atomic SQL publication
// of the artifact, link, primary binding, and first history
// generation. No tracking endpoint is ever requested.
func TestArtifactMaterializesNativeVersion(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "served-model")
			version := mustVersion(t, backend, model, "1.0.0")

			attached, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Name:               new("weights"),
					Uri:                new("s3://bucket/model.tar.gz"),
					ModelFormatName:    new("onnx"),
					ModelFormatVersion: new("1"),
					StorageKey:         new("s3-creds"),
					Description:        new("trained weights"),
					CustomProperties: map[string]openapi.MetadataValue{
						"accuracy": doubleValue(0.97),
					},
				},
			}, *version.Id)
			require.NoError(t, err)
			require.NotNil(t, attached.ModelArtifact.Id)
			require.Equal(t, "s3://bucket/model.tar.gz", *attached.ModelArtifact.Uri)
			require.Equal(t, "onnx", *attached.ModelArtifact.ModelFormatName)
			require.Equal(t, "s3-creds", *attached.ModelArtifact.StorageKey)
			require.Equal(t, 0.97, attached.ModelArtifact.CustomProperties["accuracy"].MetadataDoubleValue.DoubleValue)

			// Exactly one native create, carrying the real source and the
			// ownership tags.
			require.Equal(t, 1, backend.stub.countRequests("POST model-versions/create"))
			record := backend.stub.versionRecord("served-model", "1")
			assert.Equal(t, "s3://bucket/model.tar.gz", record["source"])
			tags := backend.stub.versionTags("served-model", "1")
			assert.NotEmpty(t, tags["rhoai.adapter"])
			assert.Equal(t, *version.Id, tags["rhoai.logicalId"])
			assert.NotEmpty(t, tags["rhoai.operation"])
			assert.Equal(t, "s3://bucket/model.tar.gz", tags["rhoai.sourceUri"])
			assert.Equal(t, "onnx", tags["rhoai.modelFormat"])

			// The adapter-owned alias points at the native version.
			binding := bindingOf(t, backend, *version.Id)
			assert.Equal(t, mlflow.PhaseReady, binding.Phase)
			require.NotNil(t, binding.NativeModelName)
			require.NotNil(t, binding.NativeVersionNumber)
			assert.Equal(t, "served-model", *binding.NativeModelName)
			assert.Equal(t, "1", *binding.NativeVersionNumber)
			require.NotNil(t, binding.PrimaryArtifactID)
			assert.Equal(t, mustObjectID(t, attached.ModelArtifact.Id), *binding.PrimaryArtifactID)
			assert.Equal(t, "1", backend.stub.aliases("served-model")[binding.Alias])

			// The first history generation is recorded with the resolved
			// native identity.
			var history mlflow.VersionHistory
			require.NoError(t, backend.db.Where("version_id = ?", mustObjectID(t, version.Id)).
				First(&history).Error)
			assert.Equal(t, int64(1), history.Generation)
			assert.Equal(t, "served-model", history.NativeModelName)
			assert.Equal(t, "1", history.NativeVersionNumber)
			assert.Equal(t, "s3://bucket/model.tar.gz", history.Source)

			// The artifact and its link survive a restart.
			other := backend.newBackendService(t)
			restarted, err := other.GetArtifactById(*attached.ModelArtifact.Id)
			require.NoError(t, err)
			assert.Equal(t, attached.ModelArtifact, restarted.ModelArtifact)

			// Unknown ids fail cleanly.
			_, err = backend.service.GetArtifactById("not-an-id")
			require.ErrorIs(t, err, api.ErrBadRequest)
			_, err = backend.service.GetArtifactById("999999999")
			require.ErrorIs(t, err, api.ErrNotFound)

			assert.Empty(t, backend.stub.countRequests("UNEXPECTED"),
				"the adapter must never touch tracking endpoints")
		})
	}
}

// TestArtifactSourcelessAndAdditional proves a source-less attachment
// leaves the version pending with no native create, and an additional
// artifact attached after materialization projects under its ID
// namespace without replacing the primary or creating another native
// version.
func TestArtifactSourcelessAndAdditional(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "mixed-model")
			version := mustVersion(t, backend, model, "2.0.0")

			// A source-less attachment commits in SQL and leaves the
			// version pending.
			metadata, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Name: new("readme"), Description: new("no source"),
				},
			}, *version.Id)
			require.NoError(t, err)
			require.NotNil(t, metadata.ModelArtifact.Id)
			require.Zero(t, backend.stub.countRequests("model-versions"),
				"a source-less attachment issues no native version requests")
			binding := bindingOf(t, backend, *version.Id)
			assert.Equal(t, mlflow.PhasePendingSource, binding.Phase)
			assert.Nil(t, binding.NativeModelName)

			// The first source-bearing attachment materializes.
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/primary.tar.gz")
			require.NotNil(t, primary.ModelArtifact.Id)
			require.Equal(t, 1, backend.stub.countRequests("POST model-versions/create"))
			binding = bindingOf(t, backend, *version.Id)
			primaryID := *binding.PrimaryArtifactID

			// An additional artifact projects under its ID namespace.
			additional, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Name: new("extras"), Uri: new("s3://bucket/extras.tar.gz"),
					CustomProperties: map[string]openapi.MetadataValue{
						"kind": stringValue("auxiliary"),
					},
				},
			}, *version.Id)
			require.NoError(t, err)
			require.Equal(t, 1, backend.stub.countRequests("POST model-versions/create"),
				"an additional artifact creates no native version")

			binding = bindingOf(t, backend, *version.Id)
			assert.Equal(t, primaryID, *binding.PrimaryArtifactID, "the primary is unchanged")
			tags := backend.stub.versionTags("mixed-model", "1")
			prefix := "rhoai.artifacts." + *additional.ModelArtifact.Id + "."
			assert.Equal(t, "s3://bucket/extras.tar.gz", tags[prefix+"sourceUri"])
			assert.Equal(t, "auxiliary", tags[prefix+"artifact.kind"])
			assert.Equal(t, "s3://bucket/primary.tar.gz", tags["rhoai.sourceUri"],
				"the primary projection is untouched")

			// All three artifacts are linked to the version.
			var links int64
			require.NoError(t, backend.db.Model(&mlflow.ArtifactLink{}).
				Where("version_id = ?", mustObjectID(t, version.Id)).Count(&links).Error)
			assert.Equal(t, int64(3), links)
		})
	}
}

// TestArtifactStandaloneAndNameless proves standalone artifacts commit
// in SQL with no native projection, nameless artifacts get
// independent generated names, and non-model
// artifact unions are rejected as unsupported.
func TestArtifactStandaloneAndNameless(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			requestsBefore := len(backend.stub.requests())

			created, err := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{
				Name: new("orphan-weights"), Uri: new("s3://bucket/orphan.tar.gz"),
			})
			require.NoError(t, err)
			require.NotNil(t, created.Id)
			require.Equal(t, requestsBefore, len(backend.stub.requests()),
				"standalone artifacts never touch the native registry")

			byID, err := backend.service.GetModelArtifactById(*created.Id)
			require.NoError(t, err)
			require.Equal(t, created, byID)

			// Every stored model artifact names its own type: the create
			// converters drop artifactType (the union variant carries it),
			// and clients need it as the response discriminator.
			require.NotNil(t, created.ArtifactType)
			require.Equal(t, "model-artifact", *created.ArtifactType)

			// Each nameless create gets an independent generated name.
			first, err := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{
				Uri: new("s3://bucket/anonymous.tar.gz"),
			})
			require.NoError(t, err)
			second, err := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{
				Uri: new("s3://bucket/anonymous.tar.gz"),
			})
			require.NoError(t, err)
			assert.NotEqual(t, *first.Id, *second.Id, "nameless creates have independent identities")
			assert.NotEqual(t, *created.Id, *first.Id)

			var artifacts int64
			require.NoError(t, backend.db.Model(&mlflow.Object{}).
				Where("kind = ?", mlflow.KindModelArtifact).Count(&artifacts).Error)
			assert.Equal(t, int64(3), artifacts)

			// Metadata edits republish in SQL.
			edited, err := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{
				Id: created.Id, Name: new("orphan-weights"), Uri: new("s3://bucket/orphan.tar.gz"),
				Description: new("edited"),
			})
			require.NoError(t, err)
			assert.Equal(t, *created.Id, *edited.Id)
			assert.Equal(t, "edited", *edited.Description)

			// The union wrappers work for model artifacts and reject
			// everything else.
			wrapped, err := backend.service.UpsertArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{Name: new("wrapped")},
			})
			require.NoError(t, err)
			assert.NotNil(t, wrapped.ModelArtifact.Id)
			require.NotNil(t, wrapped.ModelArtifact.ArtifactType)
			require.Equal(t, "model-artifact", *wrapped.ModelArtifact.ArtifactType)

			_, err = backend.service.UpsertArtifact(&openapi.Artifact{
				DataSet: &openapi.DataSet{Name: new("nope")},
			})
			require.ErrorIs(t, err, api.ErrNotImplemented)
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				DataSet: &openapi.DataSet{Name: new("nope")},
			}, "1")
			require.ErrorIs(t, err, api.ErrNotImplemented)
		})
	}
}

// TestArtifactValidationAndConflicts proves parent and identity
// validation: bad ids, wrong parents, protected names, and the primary
// source of a materialized version refusing in-place changes.
func TestArtifactValidationAndConflicts(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "validated")
			other := mustModel(t, backend, "other-parent")
			version := mustVersion(t, backend, model, "1.0.0")
			primary := mustPrimaryArtifact(t, backend, version, "s3://bucket/primary.tar.gz")

			// Invalid version references.
			_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{Name: new("x")},
			}, "not-an-id")
			require.ErrorIs(t, err, api.ErrBadRequest)
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{Name: new("x")},
			}, "888888888")
			require.ErrorIs(t, err, api.ErrNotFound)
			// A model id is not a version id.
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{Name: new("x")},
			}, *model.Id)
			require.ErrorIs(t, err, api.ErrBadRequest)

			// A second source-bearing artifact attaches as an additional
			// artifact; it never replaces the primary.
			challenger, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{Name: new("challenger"), Uri: new("s3://bucket/other.tar.gz")},
			}, *version.Id)
			require.NoError(t, err)
			require.NotNil(t, challenger.ModelArtifact.Id)
			assert.Equal(t, mustObjectID(t, primary.ModelArtifact.Id),
				*bindingOf(t, backend, *version.Id).PrimaryArtifactID)

			// The same URI with edited metadata is accepted. (A changed
			// URI is a source replacement, covered by TestReplace*.)
			edited, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Id: primary.ModelArtifact.Id, Name: new("primary"),
					Uri:         new("s3://bucket/primary.tar.gz"),
					Description: new("annotated"),
				},
			}, *version.Id)
			require.NoError(t, err)
			assert.Equal(t, "annotated", *edited.ModelArtifact.Description)

			// A primary is not shared across models, even by ID.
			otherVersion := mustVersion(t, backend, other, "1.0.0")
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Id: primary.ModelArtifact.Id, Name: new("primary"),
					Uri: new("s3://bucket/primary.tar.gz"),
				},
			}, *otherVersion.Id)
			require.ErrorIs(t, err, api.ErrConflict)

			// Protected names remain unchanged, matching the core reconciler.
			protected, err := backend.service.UpsertModelArtifact(&openapi.ModelArtifact{
				Id: primary.ModelArtifact.Id, Name: new("renamed"),
			})
			require.NoError(t, err)
			require.Equal(t, primary.ModelArtifact.Name, protected.Name)

		})
	}
}

// TestMaterializeRetriesAfterNativeFailure proves an unknown native
// create outcome retains the intent, leaves nothing visible, and never
// blindly re-creates: a retry whose dispatched create still has no
// owned native version stays in doubt. Once the registry commits a
// create whose outcome was never returned, the retry's reconciliation
// adopts that single owned version and completes — creating exactly
// one native version in all.
func TestMaterializeRetriesAfterNativeFailure(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "resilient")
			version := mustVersion(t, backend, model, "1.0.0")

			backend.stub.mu.Lock()
			backend.stub.failCreates = 1
			backend.stub.mu.Unlock()

			_, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Name: new("weights"), Uri: new("s3://bucket/weights.tar.gz"),
				},
			}, *version.Id)
			require.ErrorIs(t, err, api.ErrUnavailable)

			// Nothing is visible and the binding is untouched.
			var committed int64
			require.NoError(t, backend.db.Model(&mlflow.Object{}).
				Where("kind = ? AND visibility = ?", mlflow.KindModelArtifact, mlflow.VisibilityCommitted).
				Count(&committed).Error)
			require.Zero(t, committed)
			assert.Equal(t, mlflow.PhasePendingSource, bindingOf(t, backend, *version.Id).Phase)

			// The retry reconciles the dispatched create, finds no owned
			// native version, and stays in doubt — no blind re-create.
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Name: new("weights"), Uri: new("s3://bucket/weights.tar.gz"),
				},
			}, *version.Id)
			require.ErrorIs(t, err, api.ErrUnavailable)
			require.Equal(t, 1, backend.stub.countRequests("POST model-versions/create"))
			assert.Zero(t, len(backend.stub.versions["resilient"]))

			// A create the registry committed server-side but whose
			// response was dropped is adopted by the retry's
			// reconciliation: exactly one owned native version.
			committedModel := mustModel(t, backend, "committed")
			committedVersion := mustVersion(t, backend, committedModel, "1.0.0")
			backend.stub.mu.Lock()
			backend.stub.commitThenFail = 1
			backend.stub.mu.Unlock()
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Name: new("committed-weights"), Uri: new("s3://bucket/weights.tar.gz"),
				},
			}, *committedVersion.Id)
			require.ErrorIs(t, err, api.ErrUnavailable)
			attached, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Name: new("committed-weights"), Uri: new("s3://bucket/weights.tar.gz"),
				},
			}, *committedVersion.Id)
			require.NoError(t, err)
			require.NotNil(t, attached.ModelArtifact.Id)
			assert.Equal(t, 1, len(backend.stub.versions["committed"]),
				"the retry adopts the committed version, creating no second one")
			assert.Equal(t, mlflow.PhaseReady, bindingOf(t, backend, *committedVersion.Id).Phase)
		})
	}
}

// TestMaterializeWaitsForRegistration proves a created native version
// is not published until its registration completes: a version created
// in PENDING_REGISTRATION is polled and published once READY, and a
// failed registration is a definitive conflict that leaves nothing
// visible.
func TestMaterializeWaitsForRegistration(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "registering")
			version := mustVersion(t, backend, model, "1.0.0")

			// The stub reports PENDING_REGISTRATION on create and flips
			// to READY on the first status read.
			backend.stub.mu.Lock()
			backend.stub.createStatus = "PENDING_REGISTRATION"
			backend.stub.mu.Unlock()
			backend.stub.mu.Lock()
			// flipStatus simulates the registry completing registration.
			backend.stub.flipStatus = true
			backend.stub.mu.Unlock()

			attached, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Name: new("weights"), Uri: new("s3://bucket/weights.tar.gz"),
				},
			}, *version.Id)
			require.NoError(t, err)
			require.NotNil(t, attached.ModelArtifact.Id)
			assert.GreaterOrEqual(t, backend.stub.countRequests("GET model-versions/get"), 1,
				"the materialization polls registration status")
			assert.Equal(t, mlflow.PhaseReady, bindingOf(t, backend, *version.Id).Phase)

			// A failed registration is definitive.
			failing := mustVersion(t, backend, model, "2.0.0")
			backend.stub.mu.Lock()
			backend.stub.createStatus = "FAILED_REGISTRATION"
			backend.stub.flipStatus = false
			backend.stub.mu.Unlock()
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Name: new("broken"), Uri: new("s3://bucket/broken.tar.gz"),
				},
			}, *failing.Id)
			require.ErrorIs(t, err, api.ErrConflict)
			assert.Equal(t, mlflow.PhasePendingSource, bindingOf(t, backend, *failing.Id).Phase)
		})
	}
}
