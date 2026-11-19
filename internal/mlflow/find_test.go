package mlflow_test

import (
	"testing"

	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFindRegisteredModelByParams proves name and external-id lookups,
// their validation, and not-found behavior.
func TestFindRegisteredModelByParams(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "find-model")
			_, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name: "find-model-ext", ExternalId: new("find-ext-1"),
			})
			require.NoError(t, err)

			byName, err := backend.service.GetRegisteredModelByParams(new("find-model"), nil)
			require.NoError(t, err)
			assert.Equal(t, *model.Id, *byName.Id)

			byExternal, err := backend.service.GetRegisteredModelByParams(nil, new("find-ext-1"))
			require.NoError(t, err)
			assert.Equal(t, "find-model-ext", byExternal.Name)

			// Both predicates must agree.
			both, err := backend.service.GetRegisteredModelByParams(new("find-model-ext"), new("find-ext-1"))
			require.NoError(t, err)
			assert.Equal(t, "find-model-ext", both.Name)

			_, err = backend.service.GetRegisteredModelByParams(new("find-model"), new("find-ext-1"))
			require.Error(t, err)
			assert.Equal(t, httpNotFound, api.ErrToStatus(err))

			_, err = backend.service.GetRegisteredModelByParams(new("no-such-model"), nil)
			require.Error(t, err)
			assert.Equal(t, httpNotFound, api.ErrToStatus(err))

			_, err = backend.service.GetRegisteredModelByParams(nil, nil)
			require.Error(t, err)
			assert.Equal(t, httpBadRequest, api.ErrToStatus(err))
		})
	}
}

// TestFindModelVersionByParams proves version lookups by name and
// parent, by external id, and their validation.
func TestFindModelVersionByParams(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			modelA := mustModel(t, backend, "find-version-a")
			modelB := mustModel(t, backend, "find-version-b")
			version := mustVersion(t, backend, modelA, "1.0.0")
			_, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name: "2.0.0", ExternalId: new("find-version-ext"),
			}, modelA.Id)
			require.NoError(t, err)
			// The same name under another model is a distinct version.
			mustVersion(t, backend, modelB, "1.0.0")

			byNameAndParent, err := backend.service.GetModelVersionByParams(
				new("1.0.0"), modelA.Id, nil)
			require.NoError(t, err)
			assert.Equal(t, *version.Id, *byNameAndParent.Id)

			byExternal, err := backend.service.GetModelVersionByParams(nil, nil, new("find-version-ext"))
			require.NoError(t, err)
			assert.Equal(t, "2.0.0", byExternal.Name)

			// A name alone is ambiguous across parents.
			_, err = backend.service.GetModelVersionByParams(new("1.0.0"), nil, nil)
			require.Error(t, err)
			assert.Equal(t, httpBadRequest, api.ErrToStatus(err))

			// A name under the wrong parent is not found.
			_, err = backend.service.GetModelVersionByParams(new("2.0.0"), modelB.Id, nil)
			require.Error(t, err)
			assert.Equal(t, httpNotFound, api.ErrToStatus(err))

			_, err = backend.service.GetModelVersionByParams(nil, nil, nil)
			require.Error(t, err)
			assert.Equal(t, httpBadRequest, api.ErrToStatus(err))

			_, err = backend.service.GetModelVersionByParams(new("1.0.0"), new("not-an-id"), nil)
			require.Error(t, err)
			assert.Equal(t, httpBadRequest, api.ErrToStatus(err))
		})
	}
}

// TestFindArtifactsByParams proves artifact lookups by name and version
// parent, by external id, the union artifact endpoint, and the
// artifact-type query parameter.
func TestFindArtifactsByParams(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "find-artifact-model")
			versionOne := mustVersion(t, backend, model, "1.0.0")
			versionTwo := mustVersion(t, backend, model, "2.0.0")

			attached, err := backend.service.UpsertModelVersionArtifact(&openapi.Artifact{
				ModelArtifact: &openapi.ModelArtifact{
					Name: new("find-artifact"), Uri: new("s3://bucket/find.tar.gz"),
					ExternalId: new("find-artifact-ext"),
				},
			}, *versionOne.Id)
			require.NoError(t, err)

			byNameAndParent, err := backend.service.GetModelArtifactByParams(
				new("find-artifact"), versionOne.Id, nil)
			require.NoError(t, err)
			assert.Equal(t, *attached.ModelArtifact.Id, *byNameAndParent.Id)

			// The union endpoint returns the same artifact.
			union, err := backend.service.GetArtifactByParams(
				new("find-artifact"), versionOne.Id, nil)
			require.NoError(t, err)
			require.NotNil(t, union.ModelArtifact)
			assert.Equal(t, *attached.ModelArtifact.Id, *union.ModelArtifact.Id)

			byExternal, err := backend.service.GetModelArtifactByParams(nil, nil, new("find-artifact-ext"))
			require.NoError(t, err)
			assert.Equal(t, "find-artifact", *byExternal.Name)

			// The artifact is not linked to the second version.
			_, err = backend.service.GetModelArtifactByParams(new("find-artifact"), versionTwo.Id, nil)
			require.Error(t, err)
			assert.Equal(t, httpNotFound, api.ErrToStatus(err))

			_, err = backend.service.GetModelArtifactByParams(nil, nil, nil)
			require.Error(t, err)
			assert.Equal(t, httpBadRequest, api.ErrToStatus(err))

			// Generic artifact lists include model artifacts; other
			// types list empty.
			all, err := backend.service.GetArtifacts(
				openapi.ARTIFACTTYPEQUERYPARAM_MODEL_ARTIFACT, api.ListOptions{}, nil)
			require.NoError(t, err)
			assert.Len(t, all.Items, 1)

			untyped, err := backend.service.GetArtifacts("", api.ListOptions{}, nil)
			require.NoError(t, err)
			assert.Len(t, untyped.Items, 1)

			docs, err := backend.service.GetArtifacts(
				openapi.ARTIFACTTYPEQUERYPARAM_DOC_ARTIFACT, api.ListOptions{}, nil)
			require.NoError(t, err)
			assert.Empty(t, docs.Items)

			// A parent-scoped artifact list follows the version links.
			scoped, err := backend.service.GetArtifacts(
				openapi.ARTIFACTTYPEQUERYPARAM_MODEL_ARTIFACT, api.ListOptions{}, versionOne.Id)
			require.NoError(t, err)
			assert.Len(t, scoped.Items, 1)
			otherScope, err := backend.service.GetArtifacts(
				openapi.ARTIFACTTYPEQUERYPARAM_MODEL_ARTIFACT, api.ListOptions{}, versionTwo.Id)
			require.NoError(t, err)
			assert.Empty(t, otherScope.Items)
		})
	}
}
