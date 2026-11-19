package mlflow_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kubeflow/hub/internal/datastore/embedmd"
	dbservice "github.com/kubeflow/hub/internal/db/service"
	"github.com/kubeflow/hub/internal/platform/datastore"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/clause"
)

// Record EmbedMD's actual coercions separately from the exact evaluator contract:
// SQL engines may coerce literals differently, but explicit metadata discriminators
// and adjacent integer literals must remain precise.
func TestFilterExactEmbedMDReference(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			connector, err := datastore.NewConnector("embedmd", &embedmd.EmbedMDConfig{DB: fixture.db})
			require.NoError(t, err)
			_, err = connector.Connect(dbservice.DatastoreSpec())
			require.NoError(t, err)
			cleanup := func() {
				for _, table := range []string{"ArtifactProperty", "ContextProperty", "ExecutionProperty", "ParentContext", "Attribution", "Association", "Event", "Artifact", "Execution", "Context"} {
					require.NoError(t, fixture.db.Exec("DELETE FROM ?", clause.Table{Name: table}).Error)
				}
			}
			cleanup()
			t.Cleanup(cleanup)
			service := createModelRegistryService(t, fixture.db)
			// EmbedMD's existing converter/storage accepts only int32 metadata.
			// Keep this limitation explicit rather than claiming int64 parity.
			for _, value := range []string{"9007199254740993", "-9007199254740993", "-9223372036854775808", "9223372036854775807"} {
				_, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "out-of-range-" + value, CustomProperties: map[string]openapi.MetadataValue{"rank": intValue(value)}})
				require.ErrorIs(t, err, api.ErrBadRequest)
			}
			properties := exactFilterProperties()
			properties["rank"] = intValue("42")
			properties["negative"] = intValue("-42")
			properties["minimum"] = intValue("-2147483648")
			properties["maximum"] = intValue("2147483647")
			model, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "exact-reference", CustomProperties: properties})
			require.NoError(t, err)
			for _, row := range []exactFilterCase{
				{"score.double_value = 1.0", true}, {"score.int_value = 1", false},
				{"score = 1", true}, {"zero.double_value = -0.0", true}, {"zero.int_value = 0", false},
				{"rank = 42", true}, {"rank = 42.0", false}, {"rank.int_value = 42.0", true},
				{"rank.double_value = 42", false}, {"rank = '42'", false},
				{"rank > 41", true}, {"negative < -41", true},
				{"minimum = -2147483648", true}, {"maximum = 2147483647", true},
				{"rank IN (41, 42)", false}, {"rank IN (41, '42')", false},
				{"missing != 1", false}, {"`owner-team` LIKE '__'", true},
				{"`owner-team` ILIKE 'é_'", true}, {"combining LIKE '_'", false}, {"combining LIKE '__'", true},
			} {
				t.Run(row.query, func(t *testing.T) {
					list, err := service.GetRegisteredModels(api.ListOptions{FilterQuery: &row.query})
					// Existing EmbedMD IN construction gives no matches on
					// MySQL and a type error on PostgreSQL. Record this boundary;
					// the MLflow evaluator retains its established typed IN.
					if engine == "postgres" && strings.Contains(row.query, " IN ") {
						require.ErrorIs(t, err, api.ErrBadRequest)
						return
					}
					require.NoError(t, err)
					t.Logf("reference %s: %d matches (expected %t)", engine, len(list.Items), row.want)
					require.Equal(t, row.want, len(list.Items) == 1)
				})
			}
			for _, query := range []string{fmt.Sprintf("id = %s", *model.Id), fmt.Sprintf("id = '%s'", *model.Id)} {
				list, err := service.GetRegisteredModels(api.ListOptions{FilterQuery: &query})
				require.NoError(t, err)
				require.Len(t, list.Items, 1)
			}
			version, err := service.UpsertModelVersion(&openapi.ModelVersion{Name: "v1"}, model.Id)
			require.NoError(t, err)
			for _, query := range []string{fmt.Sprintf("registeredModelId = %s", *model.Id), fmt.Sprintf("registeredModelId = '%s'", *model.Id)} {
				list, err := service.GetModelVersions(api.ListOptions{FilterQuery: &query}, nil)
				require.NoError(t, err)
				require.Len(t, list.Items, 1)
				require.Equal(t, version.Id, list.Items[0].Id)
			}
		})
	}
}
