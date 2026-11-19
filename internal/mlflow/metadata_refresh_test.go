package mlflow_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Losing a prior discriminator, ignoring an invalid edit, or deleting an
// escaped property on a complete observation must fail this regression.
func TestExternalMetadataListRefresh(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			before := map[string]openapi.MetadataValue{
				"proto": protoValue("cHJvdG8="), "struct": structValue("c3RydWN0"),
				"labels.env": stringValue(""), "false": boolValue(false),
				"min": intValue("-9223372036854775808"), "max": intValue("9223372036854775807"),
				"double": doubleValue(1.0), "rhoai.user": protoValue("cHJvdG8="),
				"removed": stringValue("remove after complete observation"),
			}
			model, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name: "typed-refresh", CustomProperties: before,
			})
			require.NoError(t, err)
			version, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name: "1.0.0", CustomProperties: before,
			}, model.Id)
			require.NoError(t, err)
			mustPrimaryArtifact(t, backend, version, "s3://bucket/typed")
			check := func(want map[string]openapi.MetadataValue) {
				t.Helper()
				models, err := backend.service.GetRegisteredModels(api.ListOptions{})
				require.NoError(t, err)
				require.Len(t, models.Items, 1)
				assert.Equal(t, want, models.Items[0].CustomProperties)
				versions, err := backend.service.GetModelVersions(api.ListOptions{}, model.Id)
				require.NoError(t, err)
				require.Len(t, versions.Items, 1)
				assert.Equal(t, want, versions.Items[0].CustomProperties)
				gotModel, err := backend.service.GetRegisteredModelById(*model.Id)
				require.NoError(t, err)
				assert.Equal(t, want, gotModel.CustomProperties)
				gotVersion, err := backend.service.GetModelVersionById(*version.Id)
				require.NoError(t, err)
				assert.Equal(t, want, gotVersion.CustomProperties)
			}
			check(before)
			assertStable := func() {
				t.Helper()
				var rows []mlflow.Object
				require.NoError(t, backend.db.Order("id").Find(&rows).Error)
				for range 2 {
					require.NoError(t, backend.service.Refresh(t.Context()))
					var after []mlflow.Object
					require.NoError(t, backend.db.Order("id").Find(&after).Error)
					assert.Equal(t, rows, after, "unchanged refresh preserves revisions, timestamps and JSON")
				}
			}
			assertStable()
			edit := func(values map[string]string) {
				t.Helper()
				backend.stub.mu.Lock()
				defer backend.stub.mu.Unlock()
				for _, tags := range []map[string]string{
					backend.stub.models[model.Name]["tags"].(map[string]string),
					backend.stub.versions[model.Name]["1"].(map[string]any)["tags"].(map[string]string),
				} {
					delete(tags, "removed")
					for key, value := range values {
						tags[mlflow.EscapeTagKey(key)] = value
					}
				}
			}
			edit(map[string]string{
				"proto": "bmV3", "struct": "bmV3", "labels.env": "prod", "false": "true",
				"min": "9223372036854775807", "max": "-9223372036854775808", "double": "2.5",
				"rhoai.user": "bmV3", "new": "outside",
			})
			valid := map[string]openapi.MetadataValue{
				"proto": protoValue("bmV3"), "struct": structValue("bmV3"),
				"labels.env": stringValue("prod"), "false": boolValue(true),
				"min": intValue("9223372036854775807"), "max": intValue("-9223372036854775808"),
				"double": doubleValue(2.5), "rhoai.user": protoValue("bmV3"), "new": stringValue("outside"),
			}
			check(valid)
			assertStable()
			invalid := map[string]string{
				"proto": "not base64!", "struct": "also invalid!", "false": "perhaps",
				"min": "-9223372036854775809", "max": "9223372036854775808", "double": "not a number",
				"rhoai.user": "bad escaped payload!",
			}
			edit(invalid)
			for key, value := range invalid {
				valid[key] = stringValue(value)
			}
			check(valid)
			assertStable()
		})
	}
}

// Accepting NaN/Inf into a typed double used to panic in mustMarshal.
func TestRefreshNonfinite(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			values := []string{"NaN", "+Inf", "-Inf", "1e9999", "-1e9999"}
			properties := map[string]openapi.MetadataValue{}
			for i := range values {
				properties[fmt.Sprint(i)] = doubleValue(1)
			}
			model, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name: "nonfinite", CustomProperties: properties,
			})
			require.NoError(t, err)
			version, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{Name: "1", CustomProperties: properties}, model.Id)
			require.NoError(t, err)
			mustPrimaryArtifact(t, backend, version, "s3://bucket/nonfinite")
			for i, value := range values {
				backend.stub.tags(model.Name)[fmt.Sprint(i)] = value
				backend.stub.versionTags(model.Name, "1")[fmt.Sprint(i)] = value
			}
			require.NotPanics(t, func() { require.NoError(t, backend.service.Refresh(t.Context())) })
			gotModel, err := backend.service.GetRegisteredModelById(*model.Id)
			require.NoError(t, err)
			gotVersion, err := backend.service.GetModelVersionById(*version.Id)
			require.NoError(t, err)
			for i, value := range values {
				assert.Equal(t, stringValue(value), gotModel.CustomProperties[fmt.Sprint(i)])
				assert.Equal(t, stringValue(value), gotVersion.CustomProperties[fmt.Sprint(i)])
			}
			var rows []mlflow.Object
			require.NoError(t, backend.db.Find(&rows).Error)
			for _, row := range rows {
				assert.True(t, json.Valid([]byte(row.CommittedJSON)))
			}
		})
	}
}

// A native value outside the projection bound must abort the entire
// observation, including an earlier import and the success timestamp.
func TestExternalMetadataInvalidObservationAtomic(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "z-existing")
			version := mustVersion(t, backend, model, "1")
			mustPrimaryArtifact(t, backend, version, "s3://bucket/checked")
			require.NoError(t, backend.service.Refresh(t.Context()))
			var before []mlflow.Object
			require.NoError(t, backend.db.Order("id").Find(&before).Error)
			stamp := lastRefreshTime(t, backend)
			for _, target := range []string{"import-model", "import-version", "refresh-model", "refresh-version"} {
				t.Run(target, func(t *testing.T) {
					seedNativeModel(t, backend, "a-import-first", "", map[string]string{})
					tags := backend.stub.tags(model.Name)
					switch target {
					case "import-model":
						tags = backend.stub.tags("a-import-first")
					case "import-version":
						seedNativeVersion(t, backend, "a-import-first", "1", "s3://bucket/imported", "", map[string]string{})
						tags = backend.stub.versionTags("a-import-first", "1")
					case "refresh-version":
						tags = backend.stub.versionTags(model.Name, "1")
					}
					tags["oversized"] = strings.Repeat("x", 5001)
					t.Cleanup(func() {
						delete(tags, "oversized")
						backend.stub.mu.Lock()
						delete(backend.stub.models, "a-import-first")
						delete(backend.stub.versions, "a-import-first")
						backend.stub.mu.Unlock()
					})
					require.NotPanics(t, func() { require.Error(t, backend.service.Refresh(t.Context())) })
					var after []mlflow.Object
					require.NoError(t, backend.db.Order("id").Find(&after).Error)
					assert.Equal(t, before, after)
					assert.Equal(t, stamp, lastRefreshTime(t, backend))
				})
			}
		})
	}
}
