package mlflow_test

import (
	"testing"

	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// filterFixtureModel builds one registered model carrying the truth
// table's typed custom properties.
func filterFixtureModel(name string, properties map[string]openapi.MetadataValue) *openapi.RegisteredModel {
	return &openapi.RegisteredModel{Name: name, CustomProperties: properties}
}

// seedFilterTruthTable creates three locally committed models:
//
//	alpha — framework pytorch, accuracy 0.95, rank 42, trained true
//	beta  — framework tensorflow, accuracy 0.5, rank 7, trained false,
//	        plus the escaped-name label `owner-team`
//	gamma — no custom properties
func seedFilterTruthTable(t *testing.T, backend *backendFixture) {
	t.Helper()
	_, err := backend.service.UpsertRegisteredModel(filterFixtureModel("alpha", map[string]openapi.MetadataValue{
		"framework": stringValue("pytorch"),
		"accuracy":  doubleValue(0.95),
		"rank":      intValue("42"),
		"trained":   boolValue(true),
	}))
	require.NoError(t, err)
	_, err = backend.service.UpsertRegisteredModel(filterFixtureModel("beta", map[string]openapi.MetadataValue{
		"framework":  stringValue("tensorflow"),
		"accuracy":   doubleValue(0.5),
		"rank":       intValue("7"),
		"trained":    boolValue(false),
		"owner-team": stringValue("fedora"),
	}))
	require.NoError(t, err)
	_, err = backend.service.UpsertRegisteredModel(filterFixtureModel("gamma", nil))
	require.NoError(t, err)
}

// filterNames runs one filter query and returns the matched model names.
func filterNames(t *testing.T, backend *backendFixture, query string) ([]string, error) {
	t.Helper()
	list, err := backend.service.GetRegisteredModels(api.ListOptions{FilterQuery: &query})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.Name)
	}
	return names, nil
}

// TestFilterTruthTable exercises typed property comparisons, missing
// properties, labels, string/number typing, explicit metadata types,
// boolean operators with precedence and grouping, IN, comparisons, and
// LIKE/ILIKE over committed objects.
func TestFilterTruthTable(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			seedFilterTruthTable(t, backend)

			for _, row := range []struct {
				query    string
				expected []string
			}{
				// Built-in string properties.
				{query: "name = 'alpha'", expected: []string{"alpha"}},
				{query: "name != 'alpha'", expected: []string{"beta", "gamma"}},
				{query: "name <> 'alpha'", expected: []string{"beta", "gamma"}},
				{query: "name > 'beta'", expected: []string{"gamma"}},
				{query: "name LIKE 'al%'", expected: []string{"alpha"}},
				{query: "name LIKE '%eta'", expected: []string{"beta"}},
				{query: "name LIKE '_eta'", expected: []string{"beta"}},
				{query: "name LIKE 'AL%'", expected: []string{}},
				{query: "name ILIKE 'AL%'", expected: []string{"alpha"}},

				// Typed custom properties.
				{query: "framework = 'pytorch'", expected: []string{"alpha"}},
				{query: "accuracy > 0.6", expected: []string{"alpha"}},
				{query: "accuracy >= 0.5", expected: []string{"alpha", "beta"}},
				{query: "accuracy < 1", expected: []string{"alpha", "beta"}},
				{query: "rank = 42", expected: []string{"alpha"}},
				{query: "rank = 42.0", expected: []string{"alpha"}},
				{query: "trained = true", expected: []string{"alpha"}},
				{query: "trained = false", expected: []string{"beta"}},

				// Missing properties match nothing, including inequality,
				// as the platform's property join does.
				{query: "missing = 'x'", expected: []string{}},
				{query: "missing != 'x'", expected: []string{}},
				{query: "missing > 1", expected: []string{}},

				// Strings and numbers never compare across types.
				{query: "framework > 1", expected: []string{}},
				{query: "accuracy > 'x'", expected: []string{}},

				// Explicit metadata type suffixes.
				{query: "framework.string_value = 'pytorch'", expected: []string{"alpha"}},
				{query: "accuracy.double_value > 0.6", expected: []string{"alpha"}},
				{query: "rank.int_value = 42", expected: []string{"alpha"}},
				{query: "trained.bool_value = true", expected: []string{"alpha"}},
				{query: "accuracy.int_value > 0", expected: []string{}},

				// Escaped property names.
				{query: "`owner-team` = 'fedora'", expected: []string{"beta"}},

				// IN membership.
				{query: "framework IN ('pytorch', 'xgboost')", expected: []string{"alpha"}},
				{query: "rank IN (7, 42)", expected: []string{"alpha", "beta"}},
				{query: "rank IN ('7')", expected: []string{}},

				// Boolean operators, precedence, and grouping.
				{query: "framework = 'pytorch' AND accuracy > 0.9", expected: []string{"alpha"}},
				{query: "framework = 'xgboost' OR rank = 7", expected: []string{"beta"}},
				{query: "framework = 'pytorch' AND accuracy > 0.9 OR rank = 7", expected: []string{"alpha", "beta"}},
				{query: "framework = 'pytorch' AND (accuracy > 0.9 OR rank = 7)", expected: []string{"alpha"}},
				{query: "(name = 'alpha' OR name = 'beta') AND trained = false", expected: []string{"beta"}},

				// Built-in identifier properties.
				{query: "externalId = 'none'", expected: []string{}},
			} {
				names, err := filterNames(t, backend, row.query)
				require.NoError(t, err, "query %q", row.query)
				assert.Equal(t, row.expected, names, "query %q", row.query)
			}

			// Quoted numeric IDs remain compatible with the public string ID.
			list, err := backend.service.GetRegisteredModels(api.ListOptions{})
			require.NoError(t, err)
			var alphaID string
			for _, item := range list.Items {
				if item.Name == "alpha" {
					alphaID = *item.Id
				}
			}
			require.NotEmpty(t, alphaID)
			names, err := filterNames(t, backend, "id = '"+alphaID+"'")
			require.NoError(t, err)
			assert.Equal(t, []string{"alpha"}, names)
		})
	}
}

// TestFilterVersionScopeRelations proves version built-ins filter within
// a parent-scoped list: registeredModelId and author.
func TestFilterVersionScopeRelations(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			modelA := mustModel(t, backend, "filter-scope-a")
			modelB := mustModel(t, backend, "filter-scope-b")
			versionA := mustVersion(t, backend, modelA, "1.0.0")
			_, err := backend.service.UpsertModelVersion(&openapi.ModelVersion{
				Name: "2.0.0", Author: new("data-scientist"),
			}, modelA.Id)
			require.NoError(t, err)
			mustVersion(t, backend, modelB, "1.0.0")

			byModel, err := backend.service.GetModelVersions(api.ListOptions{
				FilterQuery: new("registeredModelId = '" + *modelA.Id + "'"),
			}, nil)
			require.NoError(t, err)
			assert.Len(t, byModel.Items, 2)

			byAuthor, err := backend.service.GetModelVersions(api.ListOptions{
				FilterQuery: new("author = 'data-scientist'"),
			}, nil)
			require.NoError(t, err)
			require.Len(t, byAuthor.Items, 1)
			assert.Equal(t, "2.0.0", byAuthor.Items[0].Name)

			// The filter composes with the parent scope: the same
			// author filter under model B matches nothing.
			none, err := backend.service.GetModelVersions(api.ListOptions{
				FilterQuery: new("author = 'data-scientist'"),
			}, modelB.Id)
			require.NoError(t, err)
			assert.Empty(t, none.Items)

			_ = versionA
		})
	}
}

// TestFilterUnsupportedRelations proves filters naming serving or
// experiment relations are refused as unimplemented rather than
// silently matching nothing.
func TestFilterUnsupportedRelations(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			mustModel(t, backend, "filter-relations")

			for _, query := range []string{
				"servingEnvironmentId = '1'",
				"runtime = 'kserve'",
				"desiredState = 'DEPLOYED'",
				"experimentId = '1'",
				"experimentRunId = '1'",
				"status = 'RUNNING'",
				"startTimeSinceEpoch > 0",
				"endTimeSinceEpoch > 0",
			} {
				_, err := backend.service.GetRegisteredModels(api.ListOptions{FilterQuery: &query})
				require.Error(t, err, "query %q", query)
				assert.Equal(t, httpNotImplemented, api.ErrToStatus(err), "query %q", query)
			}
		})
	}
}

// TestFilterMalformedQueries proves a query the parser rejects is a bad
// request, and that an unknown property name is a custom property that
// simply matches nothing.
func TestFilterMalformedQueries(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			mustModel(t, backend, "filter-malformed")

			for _, query := range []string{
				"name =",
				"name === 'x'",
				"AND name = 'x'",
				"name = 'x' AND",
				"(name = 'x'",
				"name IN 'x'",
			} {
				_, err := backend.service.GetRegisteredModels(api.ListOptions{FilterQuery: &query})
				require.Error(t, err, "query %q", query)
				assert.Equal(t, httpBadRequest, api.ErrToStatus(err), "query %q", query)
			}

			// An unknown property name is treated as a custom property
			// that no object carries: no match, no error.
			names, err := filterNames(t, backend, "notAProperty = 'x'")
			require.NoError(t, err)
			assert.Empty(t, names)
		})
	}
}

// TestFilterFailedRefreshReturnsUnavailable proves a list whose refresh
// fails answers unavailable instead of a stale page.
func TestFilterFailedRefreshReturnsUnavailable(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			mustModel(t, backend, "filter-unavailable")

			backend.stub.failNextRequests("GET registered-models/search", 1)
			_, err := backend.service.GetRegisteredModels(api.ListOptions{})
			require.Error(t, err)
			assert.Equal(t, httpServiceUnavailable, api.ErrToStatus(err))

			// A find-by-params refresh failure is also unavailable.
			backend.stub.failNextRequests("GET registered-models/search", 1)
			_, err = backend.service.GetRegisteredModelByParams(new("filter-unavailable"), nil)
			require.Error(t, err)
			assert.Equal(t, httpServiceUnavailable, api.ErrToStatus(err))
		})
	}
}
