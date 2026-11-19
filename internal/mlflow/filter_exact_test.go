package mlflow_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	platformfilter "github.com/kubeflow/hub/internal/platform/db/filter"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/require"
)

type exactFilterCase struct {
	query string
	want  bool
}

func exactFilterProperties() map[string]openapi.MetadataValue {
	return map[string]openapi.MetadataValue{
		"score": doubleValue(1), "zero": doubleValue(math.Copysign(0, -1)),
		"rank": intValue("9007199254740993"), "minimum": intValue("-9223372036854775808"),
		"maximum": intValue("9223372036854775807"), "negative": intValue("-9007199254740993"),
		"owner-team": stringValue("É猫"), "combining": stringValue("e\u0301"),
	}
}

func exactFilterCases() []exactFilterCase {
	return []exactFilterCase{
		{"score.double_value = 1.0", true}, {"score.int_value = 1", false},
		{"score = 1", true}, {"zero.double_value = -0.0", true}, {"zero.int_value = 0", false},
		{"rank = 9007199254740993", true}, {"rank = 9007199254740992", false},
		{"rank > 9007199254740992", true}, {"rank > 9007199254740992.0", true},
		{"rank = 9007199254740992.0", false}, {"rank < 9007199254740994", true},
		{"negative < -9007199254740992", true}, {"negative = -9007199254740992", false},
		{"minimum = -9223372036854775808", true}, {"maximum = 9223372036854775807", true},
		{"maximum < 9223372036854775808.0", true}, {"minimum = -9223372036854775808.0", true},
		{"rank IN (9007199254740992, 9007199254740993)", true},
		{"rank IN (9007199254740992, '9007199254740993')", false},
		{"rank.double_value = 9007199254740993", false},
		{"rank.int_value = 9007199254740993 AND (score = 1 OR score = 2)", true},
		{"missing != 1", false}, {"rank = '9007199254740993'", false},
	}
}

func exactFilterObject(t *testing.T) mlflow.Object {
	t.Helper()
	resource, err := json.Marshal(&openapi.RegisteredModel{Name: "exact-types", CustomProperties: exactFilterProperties()})
	require.NoError(t, err)
	snapshot, err := json.Marshal(mlflow.StoredSnapshot{CodecVersion: 3, Kind: mlflow.KindRegisteredModel, Resource: resource})
	require.NoError(t, err)
	return mlflow.Object{ID: 123, Kind: mlflow.KindRegisteredModel, Name: "exact-types", CommittedJSON: string(snapshot)}
}

func assertExactFilter(t *testing.T, object mlflow.Object, row exactFilterCase) {
	t.Helper()
	expression, err := platformfilter.Parse(row.query)
	require.NoError(t, err)
	matched, err := mlflow.EvaluateFilter(expression, object, nil)
	require.NoError(t, err)
	require.Equal(t, row.want, matched, row.query)
}

func TestFilterExact(t *testing.T) {
	for _, row := range exactFilterCases() {
		t.Run(row.query, func(t *testing.T) { assertExactFilter(t, exactFilterObject(t), row) })
	}
}

func TestFilterUnicode(t *testing.T) {
	for _, row := range []exactFilterCase{
		{"`owner-team` LIKE '__'", true}, {"`owner-team` LIKE '_猫'", true},
		{"`owner-team` LIKE '%猫'", true}, {"`owner-team` ILIKE 'é_'", true},
		{"`owner-team` LIKE 'é_'", false}, {"combining LIKE '_'", false}, {"combining LIKE '__'", true},
	} {
		t.Run(row.query, func(t *testing.T) { assertExactFilter(t, exactFilterObject(t), row) })
	}
}

func TestFilterBuiltinIDs(t *testing.T) {
	for _, row := range []exactFilterCase{
		{"id = 123", true}, {"id = '123'", true}, {"id > 122", true}, {"id IN (122, 123)", true},
		{"registeredModelId = 77", true}, {"registeredModelId = '77'", true},
		{"createTimeSinceEpoch = 9007199254740993", true}, {"createTimeSinceEpoch = 9007199254740992", false},
	} {
		t.Run(row.query, func(t *testing.T) {
			object := exactFilterObject(t)
			object.Kind = mlflow.KindModelVersion
			object.ParentID = 77
			object.CreateTimeSinceEpoch = 9007199254740993
			assertExactFilter(t, object, row)
		})
	}
}

// The same immutable truth table exercises committed SQL snapshots and list
// evaluation on both compatibility engines, reusing one fixture per engine.
func TestFilterExactBackend(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "exact-backend", CustomProperties: exactFilterProperties()})
			require.NoError(t, err)
			rows := append(exactFilterCases(), exactFilterCase{"`owner-team` LIKE '__'", true}, exactFilterCase{"`owner-team` ILIKE 'é_'", true}, exactFilterCase{"combining LIKE '__'", true}, exactFilterCase{"id = " + *model.Id, true})
			for _, row := range rows {
				t.Run(row.query, func(t *testing.T) {
					list, err := backend.service.GetRegisteredModels(api.ListOptions{FilterQuery: &row.query})
					require.NoError(t, err)
					require.Equal(t, row.want, len(list.Items) == 1)
				})
			}
		})
	}
}
