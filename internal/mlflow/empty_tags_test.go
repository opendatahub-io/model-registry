package mlflow_test

import (
	"testing"

	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A missing prior map key must not compare equal to a newly added empty
// value: each update must write the native tag before refresh trusts it.
func TestUpdateAddsEmptyNativeTags(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "empty-tags")
			version := mustVersion(t, backend, model, "one")
			artifact := mustPrimaryArtifact(t, backend, version, "s3://bucket/empty-tags")
			props := map[string]openapi.MetadataValue{"label": stringValue("")}
			_, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Id: model.Id, CustomProperties: props})
			require.NoError(t, err)
			_, err = backend.service.UpsertModelVersion(&openapi.ModelVersion{Id: version.Id, CustomProperties: props}, model.Id)
			require.NoError(t, err)
			_, err = backend.service.UpsertModelVersionArtifact(&openapi.Artifact{ModelArtifact: &openapi.ModelArtifact{Id: artifact.ModelArtifact.Id, CustomProperties: props}}, *version.Id)
			require.NoError(t, err)
			assert.Contains(t, backend.stub.tags(model.Name), "label")
			assert.Contains(t, backend.stub.versionTags(model.Name, "1"), "label")
			assert.Contains(t, backend.stub.versionTags(model.Name, "1"), "rhoai.artifact.label")
			restarted := backend.newBackendService(t)
			models, err := restarted.GetRegisteredModels(api.ListOptions{})
			require.NoError(t, err)
			require.Len(t, models.Items, 1)
			require.Equal(t, props, models.Items[0].CustomProperties)
			gotVersion, err := restarted.GetModelVersionById(*version.Id)
			require.NoError(t, err)
			require.Equal(t, props, gotVersion.CustomProperties)
			gotArtifact, err := restarted.GetModelArtifactById(*artifact.ModelArtifact.Id)
			require.NoError(t, err)
			require.Equal(t, props, gotArtifact.CustomProperties)
		})
	}
}
