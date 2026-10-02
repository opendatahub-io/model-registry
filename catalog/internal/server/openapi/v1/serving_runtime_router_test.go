package v1

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	runtimeservice "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/service"
	sharedmodels "github.com/kubeflow/hub/catalog/internal/db/models"
	model "github.com/kubeflow/hub/catalog/pkg/openapi"
	"github.com/kubeflow/hub/internal/platform/db/entity"
	"github.com/kubeflow/hub/internal/platform/db/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const servingRuntimeRoute = "/api/serving_runtime_catalog/v1/serving_runtimes"

// Only persistence is replaced: requests still pass through the generated
// controller, real API service, DB provider, and entity-to-response mapping.
type routedRuntimeRepository struct {
	models.ServingRuntimeRepository
	getIDs             []int32
	options            *models.ServingRuntimeListOptions
	getErr             error
	listErr            error
	parseInvalidFilter bool
}

func (r *routedRuntimeRepository) GetByID(id int32) (models.ServingRuntime, error) {
	r.getIDs = append(r.getIDs, id)
	if r.getErr != nil {
		return nil, r.getErr
	}
	return routedRuntimeEntity(), nil
}

func (r *routedRuntimeRepository) List(options *models.ServingRuntimeListOptions) (*entity.ListWrapper[models.ServingRuntime], error) {
	r.options = options
	if r.parseInvalidFilter {
		// The malformed expression must fail before query building needs a DB.
		_, err := repository.ApplyFilterQuery(nil, options, nil)
		return nil, err
	}
	if r.listErr != nil {
		return nil, r.listErr
	}
	return &entity.ListWrapper[models.ServingRuntime]{
		Items: []models.ServingRuntime{routedRuntimeEntity()}, NextPageToken: "MTI6b2ZmaWNpYWw6dmxsbQ==",
	}, nil
}

func (r *routedRuntimeRepository) GetTypeID() int32 { return 7 }

type routedVersionRepository struct {
	models.ServingRuntimeVersionRepository
	options            *models.ServingRuntimeVersionListOptions
	listErr            error
	parseInvalidFilter bool
}

func (r *routedVersionRepository) List(options *models.ServingRuntimeVersionListOptions) (*entity.ListWrapper[models.ServingRuntimeVersion], error) {
	r.options = options
	if r.parseInvalidFilter {
		_, err := repository.ApplyFilterQuery(nil, options, nil)
		return nil, err
	}
	if r.listErr != nil {
		return nil, r.listErr
	}
	return &entity.ListWrapper[models.ServingRuntimeVersion]{
		Items: []models.ServingRuntimeVersion{&models.ServingRuntimeVersionImpl{
			ID:         new(int32(21)),
			Attributes: &models.ServingRuntimeVersionAttributes{Name: new("official:vllm:1.0")},
			Properties: &[]entity.Properties{
				{Name: "version", StringValue: new("1.0")},
				{Name: "image", StringValue: new("quay.io/example/vllm:1.0")},
			},
		}},
		NextPageToken: "MjE6b2ZmaWNpYWw6dmxsbToxLjA=",
	}, nil
}

type routedPropertyOptionsRepository struct {
	sharedmodels.PropertyOptionsRepository
	calls  int
	typeID int32
	kind   sharedmodels.PropertyOptionType
	err    error
}

func (r *routedPropertyOptionsRepository) List(kind sharedmodels.PropertyOptionType, typeID int32) ([]sharedmodels.PropertyOption, error) {
	r.calls++
	r.kind, r.typeID = kind, typeID
	if r.err != nil {
		return nil, r.err
	}
	return []sharedmodels.PropertyOption{
		{Name: "provider", StringValue: []string{"Red Hat"}},
		{Name: "source_id", StringValue: []string{"official"}},
	}, nil
}

func routedRuntimeEntity() models.ServingRuntime {
	return &models.ServingRuntimeImpl{
		ID:         new(int32(12)),
		Attributes: &models.ServingRuntimeAttributes{Name: new("official:vllm")},
		Properties: &[]entity.Properties{
			{Name: "source_id", StringValue: new("official")},
			{Name: "versionCount", IntValue: new(int32(1))},
		},
	}
}

type servingRuntimeRouterFixture struct {
	router   http.Handler
	runtimes *routedRuntimeRepository
	versions *routedVersionRepository
	options  *routedPropertyOptionsRepository
}

func newServingRuntimeRouterFixture(t *testing.T) *servingRuntimeRouterFixture {
	t.Helper()
	sources := serving_runtimecatalog.NewServingRuntimeSourceCollection()
	require.NoError(t, sources.MergeWithNamedQueries("test", map[string]basecatalog.PluginSource{
		"official":  {ID: "official", Labels: []string{"Official"}},
		"community": {ID: "community", Labels: []string{"Community"}},
		"unlabeled": {ID: "unlabeled"},
		"disabled":  {ID: "disabled", Labels: []string{"Official"}, Enabled: new(false)},
	}, map[string]map[string]basecatalog.FieldFilter{
		"red-hat": {"provider": {Operator: "=", Value: "Red Hat"}},
	}))
	f := &servingRuntimeRouterFixture{
		runtimes: &routedRuntimeRepository{},
		versions: &routedVersionRepository{},
		options:  &routedPropertyOptionsRepository{},
	}
	provider := serving_runtimecatalog.NewDBServingRuntimeCatalog(serving_runtimecatalog.Services{
		ServingRuntimeRepository: f.runtimes, ServingRuntimeVersionRepository: f.versions,
		PropertyOptionsRepository: f.options,
	}, sources)
	f.router = NewRouter(NewServingRuntimeCatalogServiceAPIController(NewServingRuntimeCatalogServiceAPIService(provider, sources)))
	return f
}

func (f *servingRuntimeRouterFixture) request(t *testing.T, path string, status int, body any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, servingRuntimeRoute+path, nil))
	require.Equal(t, status, recorder.Code, "GET %s: %s", path, recorder.Body.String())
	assert.Equal(t, "application/json; charset=UTF-8", recorder.Header().Get("Content-Type"))
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), body), "GET %s: %s", path, recorder.Body.String())
}

func TestServingRuntimeRouterSuccess(t *testing.T) {
	t.Run("list serializes runtime and pagination", func(t *testing.T) {
		f := newServingRuntimeRouterFixture(t)
		var body model.ServingRuntimeList
		f.request(t, "", http.StatusOK, &body)
		require.Len(t, body.Items, 1)
		assert.Equal(t, "12", body.Items[0].GetId())
		assert.Equal(t, "vllm", body.Items[0].GetName())
		assert.Equal(t, "official", body.Items[0].GetSourceId())
		assert.Equal(t, int32(1), body.Size)
		assert.Equal(t, int32(10), body.PageSize)
		assert.Equal(t, "MTI6b2ZmaWNpYWw6dmxsbQ==", body.NextPageToken)
	})
	t.Run("detail extracts numeric path ID", func(t *testing.T) {
		f := newServingRuntimeRouterFixture(t)
		var body model.ServingRuntime
		f.request(t, "/12", http.StatusOK, &body)
		assert.Equal(t, []int32{12}, f.runtimes.getIDs)
		assert.Equal(t, "12", body.GetId())
		assert.Equal(t, "vllm", body.GetName())
		assert.Equal(t, "official", body.GetSourceId())
		assert.Equal(t, int32(1), body.GetVersionCount())
	})
	t.Run("versions scope to parent and serialize artifacts", func(t *testing.T) {
		f := newServingRuntimeRouterFixture(t)
		var body model.ServingRuntimeVersionList
		f.request(t, "/12/versions", http.StatusOK, &body)
		assert.Equal(t, []int32{12}, f.runtimes.getIDs)
		require.NotNil(t, f.versions.options)
		assert.Equal(t, new(int32(12)), f.versions.options.ParentResourceID)
		require.Len(t, body.Items, 1)
		assert.Equal(t, "21", body.Items[0].GetId())
		assert.Equal(t, "vllm:1.0", body.Items[0].GetName())
		assert.Equal(t, "serving-runtime-version", body.Items[0].ArtifactType)
		assert.Equal(t, "1.0", body.Items[0].Version)
		assert.Equal(t, "quay.io/example/vllm:1.0", body.Items[0].Image)
		assert.Equal(t, int32(1), body.Size)
		assert.Equal(t, int32(10), body.PageSize)
		assert.Equal(t, "MjE6b2ZmaWNpYWw6dmxsbToxLjA=", body.NextPageToken)
	})
}

func TestServingRuntimeRouterQueryParameters(t *testing.T) {
	query := url.Values{
		"filterQuery": {"provider = 'Red Hat'"}, "pageSize": {"3"},
		"orderBy": {"NAME"}, "sortOrder": {"DESC"}, "nextPageToken": {"MTI6b2ZmaWNpYWw6dmxsbQ=="},
	}
	t.Run("runtime search and filter decode without losing punctuation", func(t *testing.T) {
		f := newServingRuntimeRouterFixture(t)
		runtimeQuery := maps.Clone(query)
		runtimeQuery.Set("q", "GPU + CPU & inference")
		runtimeQuery.Set("name", "vllm%")
		var body model.ServingRuntimeList
		f.request(t, "?"+runtimeQuery.Encode(), http.StatusOK, &body)
		require.NotNil(t, f.runtimes.options)
		assert.Equal(t, new("GPU + CPU & inference"), f.runtimes.options.Query)
		assert.Equal(t, new("vllm%"), f.runtimes.options.Name)
		assert.Equal(t, "provider = 'Red Hat'", f.runtimes.options.GetFilterQuery())
		assert.Equal(t, int32(3), f.runtimes.options.GetPageSize())
		assert.Equal(t, "NAME", f.runtimes.options.GetOrderBy())
		assert.Equal(t, "DESC", f.runtimes.options.GetSortOrder())
		assert.Equal(t, "MTI6b2ZmaWNpYWw6dmxsbQ==", f.runtimes.options.GetNextPageToken())
		assert.Equal(t, int32(3), body.PageSize)
	})
	t.Run("version filter and pagination reach repository", func(t *testing.T) {
		f := newServingRuntimeRouterFixture(t)
		versionQuery := maps.Clone(query)
		versionQuery.Set("filterQuery", "version = '1.0'")
		var body model.ServingRuntimeVersionList
		f.request(t, "/12/versions?"+versionQuery.Encode(), http.StatusOK, &body)
		require.NotNil(t, f.versions.options)
		assert.Equal(t, "version = '1.0'", f.versions.options.GetFilterQuery())
		assert.Equal(t, int32(3), f.versions.options.GetPageSize())
		assert.Equal(t, "NAME", f.versions.options.GetOrderBy())
		assert.Equal(t, "DESC", f.versions.options.GetSortOrder())
		assert.Equal(t, "MTI6b2ZmaWNpYWw6dmxsbQ==", f.versions.options.GetNextPageToken())
		assert.Equal(t, int32(3), body.PageSize)
	})
}

func TestServingRuntimeRouterSourceFilters(t *testing.T) {
	for _, tc := range []struct {
		query string
		ids   []string
	}{
		{"source=official", []string{"official"}},
		{"source=official,community", []string{"official", "community"}},
		{"sourceLabel=oFFiCIAL", []string{"official"}},
		{"sourceLabel=official,community", []string{"community", "official"}},
		{"sourceLabel=null", []string{"unlabeled"}},
		{"source=", nil},
		{"sourceLabel=", nil},
		// RHOAIENG-96061: the generated controller uses query.Get before
		// splitting commas, so repeated parameters currently use only the first.
		{"source=official&source=community", []string{"official"}},
		{"source=community&source=official", []string{"community"}},
		{"sourceLabel=official&sourceLabel=community", []string{"official"}},
		{"sourceLabel=community&sourceLabel=official", []string{"community"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			f := newServingRuntimeRouterFixture(t)
			var body model.ServingRuntimeList
			f.request(t, "?"+tc.query, http.StatusOK, &body)
			require.NotNil(t, f.runtimes.options)
			if tc.ids == nil {
				assert.Nil(t, f.runtimes.options.SourceIDs)
			} else {
				require.NotNil(t, f.runtimes.options.SourceIDs)
				assert.Equal(t, tc.ids, *f.runtimes.options.SourceIDs)
			}
		})
	}
	t.Run("unknown label returns empty array without datastore access", func(t *testing.T) {
		f := newServingRuntimeRouterFixture(t)
		var body model.ServingRuntimeList
		f.request(t, "?sourceLabel=missing&pageSize=3", http.StatusOK, &body)
		require.NotNil(t, body.Items)
		assert.Empty(t, body.Items)
		assert.Zero(t, body.Size)
		assert.Equal(t, int32(3), body.PageSize)
		assert.Empty(t, body.NextPageToken)
		assert.Nil(t, f.runtimes.options)
	})
}

func TestServingRuntimeRouterFilterOptions(t *testing.T) {
	f := newServingRuntimeRouterFixture(t)
	var body model.FilterOptionsList
	f.request(t, "/filter_options", http.StatusOK, &body)
	require.NotNil(t, body.Filters)
	assert.Equal(t, model.FilterOption{Type: "string", Values: []any{"Red Hat"}}, (*body.Filters)["provider"])
	for _, name := range []string{"capabilities.requiresGPU", "capabilities.multiModel"} {
		assert.Equal(t, model.FilterOption{Type: "boolean", Values: []any{false, true}}, (*body.Filters)[name])
	}
	assert.NotContains(t, *body.Filters, "source_id")
	require.NotNil(t, body.NamedQueries)
	assert.Equal(t, model.FieldFilter{Operator: "=", Value: "Red Hat"}, (*body.NamedQueries)["red-hat"]["provider"])
	assert.Equal(t, 1, f.options.calls)
	assert.Equal(t, sharedmodels.ContextPropertyOptionType, f.options.kind)
	assert.Equal(t, int32(7), f.options.typeID)
	assert.Empty(t, f.runtimes.getIDs, "filter_options must not be treated as a runtime ID")
}

func TestServingRuntimeRouterRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct{ path, message string }{
		{"/abc", "invalid serving_runtime ID"},
		{"/abc/versions", "invalid serving_runtime ID"},
		{"/2147483648", "invalid serving_runtime ID"},
		{"/2147483648/versions", "invalid serving_runtime ID"},
		{"?source=official&sourceLabel=official", "cannot be used together"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			f := newServingRuntimeRouterFixture(t)
			var body model.Error
			f.request(t, tc.path, http.StatusBadRequest, &body)
			assert.Equal(t, "Bad Request", body.Code)
			assert.Contains(t, body.Message, tc.message)
			assert.Empty(t, f.runtimes.getIDs)
			assert.Nil(t, f.runtimes.options)
			assert.Nil(t, f.versions.options)
		})
	}
	for _, prefix := range []string{"", "/12/versions"} {
		for _, tc := range []struct{ query, message string }{
			{"orderBy=unknown", "orderBy"},
			{"orderBy=RECOMMENDED", "orderBy"},
			{"sortOrder=unknown", "sortOrder"},
			{"pageSize=abc", "pageSize"},
			{"pageSize=0", "pageSize"},
			{"pageSize=-1", "pageSize"},
			{"pageSize=2147483648", "pageSize"},
			{"nextPageToken=invalid", "nextPageToken"},
			{"q=%ZZ", "invalid URL escape"},
		} {
			t.Run(prefix+"?"+tc.query, func(t *testing.T) {
				f := newServingRuntimeRouterFixture(t)
				var body model.Error
				f.request(t, prefix+"?"+tc.query, http.StatusBadRequest, &body)
				assert.Equal(t, "Bad Request", body.Code)
				assert.Contains(t, body.Message, tc.message)
				assert.Empty(t, f.runtimes.getIDs)
				assert.Nil(t, f.runtimes.options)
				assert.Nil(t, f.versions.options)
			})
		}
	}
}

func TestServingRuntimeRouterInvalidFilterSyntax(t *testing.T) {
	// Exercise the same parser and bad-request classification used by the real
	// repositories. A syntax error returns before a DB query or mapping is needed.
	const invalidFilter = "provider ="
	for _, prefix := range []string{"", "/12/versions"} {
		t.Run(prefix+"?filterQuery="+invalidFilter, func(t *testing.T) {
			f := newServingRuntimeRouterFixture(t)
			if prefix == "" {
				f.runtimes.parseInvalidFilter = true
			} else {
				f.versions.parseInvalidFilter = true
			}
			var body model.Error
			f.request(t, prefix+"?"+url.Values{"filterQuery": {invalidFilter}}.Encode(), http.StatusBadRequest, &body)
			assert.Equal(t, "Bad Request", body.Code)
			assert.Contains(t, body.Message, "invalid filter query syntax")
			if prefix == "" {
				require.NotNil(t, f.runtimes.options)
				assert.Equal(t, invalidFilter, f.runtimes.options.GetFilterQuery())
			} else {
				require.NotNil(t, f.versions.options)
				assert.Equal(t, invalidFilter, f.versions.options.GetFilterQuery())
			}
		})
	}
}

func TestServingRuntimeRouterNotFound(t *testing.T) {
	for _, path := range []string{"/999", "/999/versions"} {
		t.Run(path, func(t *testing.T) {
			f := newServingRuntimeRouterFixture(t)
			f.runtimes.getErr = runtimeservice.ErrServingRuntimeNotFound
			var body model.Error
			f.request(t, path, http.StatusNotFound, &body)
			assert.Equal(t, "Not Found", body.Code)
			assert.Contains(t, body.Message, "999")
			assert.Contains(t, body.Message, "not found")
			assert.Equal(t, []int32{999}, f.runtimes.getIDs)
			assert.Nil(t, f.versions.options, "a missing parent must not query versions")
		})
	}
}

func TestServingRuntimeRouterDatastoreErrors(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		fail       func(*servingRuntimeRouterFixture, error)
	}{
		{"runtime list", "", func(f *servingRuntimeRouterFixture, err error) { f.runtimes.listErr = err }},
		{"runtime detail", "/12", func(f *servingRuntimeRouterFixture, err error) { f.runtimes.getErr = err }},
		{"version list", "/12/versions", func(f *servingRuntimeRouterFixture, err error) { f.versions.listErr = err }},
		{"version parent lookup", "/12/versions", func(f *servingRuntimeRouterFixture, err error) { f.runtimes.getErr = err }},
		{"filter options", "/filter_options", func(f *servingRuntimeRouterFixture, err error) { f.options.err = err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServingRuntimeRouterFixture(t)
			tc.fail(f, errors.New("database unavailable"))
			var body model.Error
			f.request(t, tc.path, http.StatusInternalServerError, &body)
			assert.Equal(t, "Internal Server Error", body.Code)
			assert.Contains(t, body.Message, "database unavailable")
			if tc.name == "version parent lookup" {
				assert.Nil(t, f.versions.options)
			}
		})
	}
}
