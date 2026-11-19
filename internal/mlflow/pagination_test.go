package mlflow_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	platformscopes "github.com/kubeflow/hub/internal/platform/db/scopes"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPageTokenRoundTrip proves the token round-trips its cursor and
// that the platform's legacy decoder recovers the last id and value
// from the same encoding.
func TestPageTokenRoundTrip(t *testing.T) {
	encoded, err := mlflow.EncodePageToken(12, mlflow.PageCursor{
		Version: 1, Kind: "model-version", ParentID: "3",
		OrderBy: "createTimeSinceEpoch", SortOrder: "ASC",
		LastValue: "1700000000000", FilterHash: "empty",
	})
	require.NoError(t, err)

	lastID, cursor, err := mlflow.DecodePageToken(encoded, mlflow.PageCursor{
		Version: 1, Kind: "model-version", ParentID: "3",
		OrderBy: "createTimeSinceEpoch", SortOrder: "ASC",
		FilterHash: "empty",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(12), lastID)
	assert.Equal(t, "1700000000000", cursor.LastValue)

	// The legacy platform cursor codec parses the same token.
	legacy, err := platformscopes.DecodeCursor(encoded)
	require.NoError(t, err)
	assert.Equal(t, int32(12), legacy.ID)
	assert.NotEmpty(t, legacy.Value)
}

// TestPageTokenRejectsInvalidTokens proves malformed structure, an
// unknown payload version, a nonpositive id, a changed query shape, and
// an oversized token are bad requests. The tokens are validated data
// cursors, not cryptographically signed ones.
func TestPageTokenRejectsInvalidTokens(t *testing.T) {
	shape := mlflow.PageCursor{
		Version: 1, Kind: "registered-model", ParentID: "",
		OrderBy: "id", SortOrder: "ASC", FilterHash: "empty",
	}
	valid, err := mlflow.EncodePageToken(5, mlflow.PageCursor{
		Version: 1, Kind: shape.Kind, ParentID: shape.ParentID,
		OrderBy: shape.OrderBy, SortOrder: shape.SortOrder,
		LastValue: "5", FilterHash: shape.FilterHash,
	})
	require.NoError(t, err)

	for name, token := range map[string]string{
		"not base64":            "!!!not-base64!!!",
		"no payload":            encodeToken(t, "5"),
		"nonpositive id":        encodeToken(t, "0:payload"),
		"negative id":           encodeToken(t, "-1:payload"),
		"non-numeric id":        encodeToken(t, "abc:payload"),
		"payload not base64url": encodeToken(t, "5:!!!"),
		"payload not json":      encodeToken(t, "5:"+rawBase64URL(t, "not-json")),
		"empty token":           "",
	} {
		if token == "" {
			continue
		}
		_, _, err := mlflow.DecodePageToken(token, shape)
		require.Error(t, err, "%s", name)
	}

	// An unknown payload version.
	_, _, err = mlflow.DecodePageToken(encodeToken(t, "5:"+rawBase64URL(
		t, `{"v":99,"kind":"registered-model","orderBy":"id","sortOrder":"ASC"}`)), shape)
	require.Error(t, err)

	// A payload matching a different query shape.
	for name, mutated := range map[string]mlflow.PageCursor{
		"kind":        {Version: 1, Kind: "model-version", OrderBy: "id", SortOrder: "ASC", FilterHash: "empty"},
		"parent":      {Version: 1, Kind: "registered-model", ParentID: "9", OrderBy: "id", SortOrder: "ASC", FilterHash: "empty"},
		"filter":      {Version: 1, Kind: "registered-model", OrderBy: "id", SortOrder: "ASC", FilterHash: "abcd"},
		"orderBy":     {Version: 1, Kind: "registered-model", OrderBy: "createTimeSinceEpoch", SortOrder: "ASC", FilterHash: "empty"},
		"sortOrder":   {Version: 1, Kind: "registered-model", OrderBy: "id", SortOrder: "DESC", FilterHash: "empty"},
		"no version ": {Kind: "registered-model", OrderBy: "id", SortOrder: "ASC", FilterHash: "empty"},
	} {
		encoded, err := mlflow.EncodePageToken(5, mutated)
		require.NoError(t, err, "%s", name)
		_, _, err = mlflow.DecodePageToken(encoded, shape)
		require.Error(t, err, "shape change: %s", name)
	}

	// A valid token still decodes after the failures.
	_, _, err = mlflow.DecodePageToken(valid, shape)
	require.NoError(t, err)
}

// TestPageTokenEnforcesMaximumLength proves a token beyond the 1024
// character platform limit is rejected in both directions.
func TestPageTokenEnforcesMaximumLength(t *testing.T) {
	_, err := mlflow.EncodePageToken(5, mlflow.PageCursor{
		Version: 1, Kind: strings.Repeat("k", 900),
	})
	require.Error(t, err)

	oversized := encodeToken(t, "5:"+rawBase64URL(t,
		`{"v":1,"kind":"`+strings.Repeat("k", 1200)+`"}`))
	_, _, err = mlflow.DecodePageToken(oversized, mlflow.PageCursor{})
	require.Error(t, err)

	// A nonpositive last id is rejected at encode time.
	_, err = mlflow.EncodePageToken(0, mlflow.PageCursor{Version: 1})
	require.Error(t, err)
	_, err = mlflow.EncodePageToken(-3, mlflow.PageCursor{Version: 1})
	require.Error(t, err)
}

// TestPageTokenFilterHash proves nil, empty, and blank filters hash as
// one shape, and distinct filter text hashes differently.
func TestPageTokenFilterHash(t *testing.T) {
	blank := " "
	assert.Equal(t, "empty", mlflow.FilterHash(nil))
	assert.Equal(t, "empty", mlflow.FilterHash(new("")))
	assert.Equal(t, "empty", mlflow.FilterHash(&blank))

	same := "name = 'a'"
	assert.Equal(t, mlflow.FilterHash(&same), mlflow.FilterHash(new("name = 'a'")))
	assert.NotEqual(t, mlflow.FilterHash(&same), mlflow.FilterHash(new("name = 'b'")))
}

// encodeToken renders one raw `<id>:<payload>` string the way
// EncodePageToken does, for hand-built invalid tokens.
func encodeToken(t *testing.T, raw string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// rawBase64URL encodes a hand-built cursor payload fragment.
func rawBase64URL(t *testing.T, raw string) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// TestPaginationDefaultsAndSizes proves nil options list everything,
// pageSize=0 does not paginate, and sized pages carry exact size and
// final-token semantics.
func TestPaginationDefaultsAndSizes(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			for _, name := range []string{"page-one", "page-two", "page-three", "page-four"} {
				mustModel(t, backend, name)
			}

			// Default options return every model, id ascending.
			all, err := backend.service.GetRegisteredModels(api.ListOptions{})
			require.NoError(t, err)
			require.Len(t, all.Items, 4)
			assert.Empty(t, all.NextPageToken)
			assert.Equal(t, int32(4), all.Size)
			assert.Equal(t, int32(0), all.PageSize)
			for i := 1; i < len(all.Items); i++ {
				less, err := parseID(*all.Items[i-1].Id)
				require.NoError(t, err)
				greater, err := parseID(*all.Items[i].Id)
				require.NoError(t, err)
				require.Less(t, less, greater)
			}

			// pageSize=0 returns the full list without a token.
			zero := int32(0)
			full, err := backend.service.GetRegisteredModels(api.ListOptions{PageSize: &zero})
			require.NoError(t, err)
			require.Len(t, full.Items, 4)
			assert.Empty(t, full.NextPageToken)

			// One-item pages walk the whole list; the final page has
			// no token.
			one := int32(1)
			var walked []string
			var token string
			for range 10 {
				options := api.ListOptions{PageSize: &one}
				if token != "" {
					options.NextPageToken = &token
				}
				page, err := backend.service.GetRegisteredModels(options)
				require.NoError(t, err)
				require.Len(t, page.Items, 1)
				assert.Equal(t, int32(1), page.Size)
				assert.Equal(t, int32(1), page.PageSize)
				walked = append(walked, page.Items[0].Name)
				if page.NextPageToken == "" {
					break
				}
				token = page.NextPageToken
			}
			assert.Equal(t, []string{"page-one", "page-two", "page-three", "page-four"}, walked)

			// A page larger than the result set returns everything with
			// no token.
			ten := int32(10)
			big, err := backend.service.GetRegisteredModels(api.ListOptions{PageSize: &ten})
			require.NoError(t, err)
			require.Len(t, big.Items, 4)
			assert.Empty(t, big.NextPageToken)
		})
	}
}

// TestPaginationSortOrders proves ascending and descending sorts page
// in opposite directions, including timestamp ordering with ties.
func TestPaginationSortOrders(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			models := []*openapi.RegisteredModel{
				mustModel(t, backend, "sort-one"),
				mustModel(t, backend, "sort-two"),
				mustModel(t, backend, "sort-three"),
			}

			// Force identical create times so the id tiebreaker carries
			// the ordering, as the keyset predicate requires.
			require.NoError(t, backend.db.Model(&mlflow.Object{}).
				Where("true").Update("create_time_since_epoch", 1700000000000).Error)

			two := int32(2)
			desc := "DESC"
			orderBy := "CREATE_TIME"

			first, err := backend.service.GetRegisteredModels(api.ListOptions{
				PageSize: &two, SortOrder: &desc, OrderBy: &orderBy,
			})
			require.NoError(t, err)
			require.Len(t, first.Items, 2)
			require.NotEmpty(t, first.NextPageToken)

			second, err := backend.service.GetRegisteredModels(api.ListOptions{
				PageSize: &two, SortOrder: &desc, OrderBy: &orderBy,
				NextPageToken: new(first.NextPageToken),
			})
			require.NoError(t, err)
			require.Len(t, second.Items, 1)
			assert.Empty(t, second.NextPageToken)

			// Descending pages visit the highest ids first.
			firstID, err := parseID(*first.Items[0].Id)
			require.NoError(t, err)
			secondID, err := parseID(*first.Items[1].Id)
			require.NoError(t, err)
			require.Greater(t, firstID, secondID)
			lastID, err := parseID(*second.Items[0].Id)
			require.NoError(t, err)
			require.Greater(t, secondID, lastID)

			// Ascending over the same data visits the lowest first.
			asc := "ASC"
			up, err := backend.service.GetRegisteredModels(api.ListOptions{
				PageSize: &two, SortOrder: &asc, OrderBy: &orderBy,
			})
			require.NoError(t, err)
			require.Len(t, up.Items, 2)
			upFirstID, err := parseID(*up.Items[0].Id)
			require.NoError(t, err)
			upSecondID, err := parseID(*up.Items[1].Id)
			require.NoError(t, err)
			require.Less(t, upFirstID, upSecondID)
			assert.Equal(t, lastID, upFirstID, "ascending resumes where descending ended")

			// The tie in create times is broken by id in both
			// directions: no row appears twice across the pages.
			seen := map[string]bool{}
			for _, item := range append(append([]openapi.RegisteredModel{}, first.Items...), second.Items...) {
				require.False(t, seen[*item.Id], "row %s repeated across pages", *item.Id)
				seen[*item.Id] = true
			}
			assert.Len(t, seen, len(models))
		})
	}
}

// TestPaginationTokenShapeBinding proves a token refuses a different
// kind, parent scope, filter, or sort.
func TestPaginationTokenShapeBinding(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			model := mustModel(t, backend, "shape-model")
			mustModel(t, backend, "shape-model-two")
			mustVersion(t, backend, model, "1.0.0")

			one := int32(1)
			models, err := backend.service.GetRegisteredModels(api.ListOptions{PageSize: &one})
			require.NoError(t, err)
			require.NotEmpty(t, models.NextPageToken)
			token := models.NextPageToken

			// The same scope and shape still works.
			again, err := backend.service.GetRegisteredModels(api.ListOptions{
				PageSize: &one, NextPageToken: new(token),
			})
			require.NoError(t, err)
			assert.Empty(t, again.NextPageToken)

			// A different filter is a different shape.
			filter := "name = 'shape-model'"
			_, err = backend.service.GetRegisteredModels(api.ListOptions{
				PageSize: &one, NextPageToken: new(token), FilterQuery: &filter,
			})
			require.Error(t, err)
			assert.Equal(t, httpBadRequest, api.ErrToStatus(err))

			// A different sort is a different shape.
			desc := "DESC"
			_, err = backend.service.GetRegisteredModels(api.ListOptions{
				PageSize: &one, NextPageToken: new(token), SortOrder: &desc,
			})
			require.Error(t, err)
			assert.Equal(t, httpBadRequest, api.ErrToStatus(err))

			// A different kind is a different shape.
			_, err = backend.service.GetModelVersions(api.ListOptions{
				PageSize: &one, NextPageToken: new(token),
			}, nil)
			require.Error(t, err)
			assert.Equal(t, httpBadRequest, api.ErrToStatus(err))

			// A parent-scoped list is a different shape from a global
			// one.
			_, err = backend.service.GetModelVersions(api.ListOptions{
				PageSize: &one,
			}, model.Id)
			require.NoError(t, err)
			_, err = backend.service.GetModelVersions(api.ListOptions{
				PageSize: &one,
			}, nil)
			require.NoError(t, err)
		})
	}
}

// TestPaginationParentScopeWithImportedRecords proves a parent-scoped
// list pages over locally committed and imported records together, and
// the global list contains both as well.
func TestPaginationParentScopeWithImportedRecords(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)

			// A locally created model with two committed versions.
			local := mustModel(t, backend, "scope-local")
			mustVersion(t, backend, local, "1.0.0")
			mustVersion(t, backend, local, "2.0.0")

			// An imported native model with one version.
			seedNativeModel(t, backend, "scope-imported", "", nil)
			seedNativeVersion(t, backend, "scope-imported", "1", "s3://bucket/imported.tar.gz", "", nil)
			require.NoError(t, backend.service.Refresh(t.Context()))

			global, err := backend.service.GetModelVersions(api.ListOptions{}, nil)
			require.NoError(t, err)
			assert.Len(t, global.Items, 3)

			scoped, err := backend.service.GetModelVersions(api.ListOptions{}, local.Id)
			require.NoError(t, err)
			assert.Len(t, scoped.Items, 2)

			// One-item pages walk the parent scope exactly.
			one := int32(1)
			var names []string
			var token string
			for range 10 {
				options := api.ListOptions{PageSize: &one}
				if token != "" {
					options.NextPageToken = &token
				}
				page, err := backend.service.GetModelVersions(options, local.Id)
				require.NoError(t, err)
				require.Len(t, page.Items, 1)
				names = append(names, page.Items[0].Name)
				if page.NextPageToken == "" {
					break
				}
				token = page.NextPageToken
			}
			assert.Len(t, names, 2)

			// The parent-scoped token does not open the global list.
			page, err := backend.service.GetModelVersions(api.ListOptions{PageSize: &one}, local.Id)
			require.NoError(t, err)
			require.NotEmpty(t, page.NextPageToken)
			_, err = backend.service.GetModelVersions(api.ListOptions{
				PageSize: &one, NextPageToken: new(page.NextPageToken),
			}, nil)
			require.Error(t, err)
			assert.Equal(t, httpBadRequest, api.ErrToStatus(err))
		})
	}
}
