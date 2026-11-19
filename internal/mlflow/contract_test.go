package mlflow_test

import (
	"testing"

	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runModelContract asserts the backend-agnostic behavior every supported
// api.ModelRegistryApi backend must provide for models, model versions,
// and model artifacts. It characterizes the existing EmbedMD backend; the
// MLflow compatibility backend must satisfy the same contract.
func runModelContract(t *testing.T, service api.ModelRegistryApi) {
	t.Helper()

	t.Run("registration round trip", func(t *testing.T) {
		rm, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "compat-model"})
		require.NoError(t, err)
		require.NotNil(t, rm.Id)
		require.NotEmpty(t, *rm.Id)

		mv, err := service.UpsertModelVersion(&openapi.ModelVersion{Name: "1.0.0"}, rm.Id)
		require.NoError(t, err)

		got, err := service.GetModelVersionById(*mv.Id)
		require.NoError(t, err)
		require.Equal(t, *mv.Id, *got.Id)
		require.Equal(t, "1.0.0", got.Name)
		require.Equal(t, *rm.Id, got.RegisteredModelId)

		// The model version is visible in both the parent-scoped and
		// the global list before any artifact exists.
		byParent, err := service.GetModelVersions(api.ListOptions{}, rm.Id)
		require.NoError(t, err)
		assert.Len(t, byParent.Items, 1)

		global, err := service.GetModelVersions(api.ListOptions{}, nil)
		require.NoError(t, err)
		assert.Len(t, global.Items, 1)
	})

	t.Run("partial update preserves omitted fields", func(t *testing.T) {
		rm, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Name:        "compat-partial",
			Description: new("Original description"),
			Owner:       new("original-owner"),
			ExternalId:  new("original-ext-id"),
			State:       (*openapi.RegisteredModelState)(new(string(openapi.REGISTEREDMODELSTATE_LIVE))),
			CustomProperties: map[string]openapi.MetadataValue{
				"framework": stringValue("tensorflow"),
			},
		})
		require.NoError(t, err)

		updated, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Id:          rm.Id,
			Description: new("Updated description"),
		})
		require.NoError(t, err)
		require.Equal(t, "Updated description", *updated.Description)
		require.Equal(t, "original-owner", *updated.Owner)
		require.Equal(t, "original-ext-id", *updated.ExternalId)
		require.Equal(t, openapi.REGISTEREDMODELSTATE_LIVE, *updated.State)
		require.Contains(t, updated.CustomProperties, "framework")

		// An explicitly supplied custom-property map replaces the map.
		replaced, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Id: rm.Id,
			CustomProperties: map[string]openapi.MetadataValue{
				"environment": stringValue("production"),
			},
		})
		require.NoError(t, err)
		require.Contains(t, replaced.CustomProperties, "environment")
		require.NotContains(t, replaced.CustomProperties, "framework")

		// Nil description is ignored; empty string clears the field.
		cleared, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Id:          rm.Id,
			Description: new(""),
		})
		require.NoError(t, err)
		require.Equal(t, "", *cleared.Description)
	})

	t.Run("protected fields are immutable", func(t *testing.T) {
		rm, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Name:        "compat-protected",
			Description: new("Original description"),
		})
		require.NoError(t, err)
		originalCreateTime := *rm.CreateTimeSinceEpoch

		updated, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Id:                       rm.Id,
			Name:                     "HACKED_NAME",
			CreateTimeSinceEpoch:     new("9999999999"),
			LastUpdateTimeSinceEpoch: new("8888888888"),
			Description:              new("Updated description"),
		})
		require.NoError(t, err)
		require.Equal(t, "compat-protected", updated.Name)
		require.Equal(t, originalCreateTime, *updated.CreateTimeSinceEpoch)
		require.Equal(t, "Updated description", *updated.Description)

		mv, err := service.UpsertModelVersion(&openapi.ModelVersion{Name: "1.0.0"}, rm.Id)
		require.NoError(t, err)

		updatedMv, err := service.UpsertModelVersion(&openapi.ModelVersion{
			Id:                mv.Id,
			Name:              "HACKED_NAME",
			RegisteredModelId: "999999",
			Author:            new("updated-author"),
		}, nil)
		require.NoError(t, err)
		require.Equal(t, "1.0.0", updatedMv.Name)
		require.Equal(t, *rm.Id, updatedMv.RegisteredModelId)
		require.Equal(t, "updated-author", *updatedMv.Author)
	})

	t.Run("name and external id uniqueness", func(t *testing.T) {
		_, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "compat-dup"})
		require.NoError(t, err)

		_, err = service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "compat-dup"})
		require.Error(t, err)
		assert.Equal(t, httpConflict, api.ErrToStatus(err))

		// External-id lookup of an unknown external ID is not found.
		_, err = service.GetRegisteredModelByParams(new("compat-ext-name"), new("compat-ext-123"))
		require.Error(t, err)
		assert.Equal(t, httpNotFound, api.ErrToStatus(err))

		_, err = service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Name:       "compat-ext-name",
			ExternalId: new("compat-ext-123"),
		})
		require.NoError(t, err)

		byParams, err := service.GetRegisteredModelByParams(nil, new("compat-ext-123"))
		require.NoError(t, err)
		require.Equal(t, "compat-ext-name", byParams.Name)
	})

	t.Run("version names unique per model", func(t *testing.T) {
		rmA, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "compat-versioned-a"})
		require.NoError(t, err)
		rmB, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "compat-versioned-b"})
		require.NoError(t, err)

		_, err = service.UpsertModelVersion(&openapi.ModelVersion{Name: "2.0.0"}, rmA.Id)
		require.NoError(t, err)
		_, err = service.UpsertModelVersion(&openapi.ModelVersion{Name: "2.0.0"}, rmA.Id)
		require.Error(t, err)
		assert.Equal(t, httpConflict, api.ErrToStatus(err))

		// The same version name under a different model is allowed.
		_, err = service.UpsertModelVersion(&openapi.ModelVersion{Name: "2.0.0"}, rmB.Id)
		require.NoError(t, err)
	})

	t.Run("standalone and empty-uri artifacts", func(t *testing.T) {
		// A model artifact can exist with an empty URI and no parent.
		created, err := service.UpsertArtifact(&openapi.Artifact{
			ModelArtifact: &openapi.ModelArtifact{
				Name: new("compat-empty-uri"),
				Uri:  new(""),
			},
		})
		require.NoError(t, err)
		require.NotNil(t, created.ModelArtifact.Id)
		require.NotNil(t, *created.ModelArtifact.Uri)
		require.Equal(t, "", *created.ModelArtifact.Uri)

		// A nameless standalone artifact gets a generated UUID name.
		nameless, err := service.UpsertArtifact(&openapi.Artifact{
			ModelArtifact: &openapi.ModelArtifact{},
		})
		require.NoError(t, err)
		require.NotNil(t, nameless.ModelArtifact.Name)
		require.NotEmpty(t, *nameless.ModelArtifact.Name)
	})

	t.Run("nameless artifacts with equal sources have distinct identities", func(t *testing.T) {
		first, err := service.UpsertModelArtifact(&openapi.ModelArtifact{
			Uri: new("s3://bucket/shared.onnx"), Description: new("first"),
		})
		require.NoError(t, err)
		second, err := service.UpsertModelArtifact(&openapi.ModelArtifact{
			Uri: new("s3://bucket/shared.onnx"), Description: new("second"),
		})
		require.NoError(t, err)
		require.NotEqual(t, *first.Id, *second.Id)
	})

	t.Run("artifact attachment and multi-parent links", func(t *testing.T) {
		rm, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "compat-attach"})
		require.NoError(t, err)
		mv1, err := service.UpsertModelVersion(&openapi.ModelVersion{Name: "1.0.0"}, rm.Id)
		require.NoError(t, err)
		mv2, err := service.UpsertModelVersion(&openapi.ModelVersion{Name: "2.0.0"}, rm.Id)
		require.NoError(t, err)

		attached, err := service.UpsertModelVersionArtifact(&openapi.Artifact{
			ModelArtifact: &openapi.ModelArtifact{
				Name: new("compat-attach-artifact"),
				Uri:  new("s3://bucket/model.onnx"),
			},
		}, *mv1.Id)
		require.NoError(t, err)
		require.NotNil(t, attached.ModelArtifact.Id)

		// The artifact is visible through the version-scoped list.
		arts, err := service.GetModelArtifacts(api.ListOptions{}, mv1.Id)
		require.NoError(t, err)
		require.Len(t, arts.Items, 1)
		require.Equal(t, *attached.ModelArtifact.Id, *arts.Items[0].Id)

		// The same artifact, referenced by ID, can be attached to a
		// second version: the multi-parent link is preserved.
		reattached, err := service.UpsertModelVersionArtifact(&openapi.Artifact{
			ModelArtifact: &openapi.ModelArtifact{
				Id: attached.ModelArtifact.Id,
			},
		}, *mv2.Id)
		require.NoError(t, err)
		require.Equal(t, *attached.ModelArtifact.Id, *reattached.ModelArtifact.Id)

		arts2, err := service.GetModelArtifacts(api.ListOptions{}, mv2.Id)
		require.NoError(t, err)
		require.Len(t, arts2.Items, 1)

		// A standalone update by ID does not need the parent version.
		updated, err := service.UpsertArtifact(&openapi.Artifact{
			ModelArtifact: &openapi.ModelArtifact{
				Id:  attached.ModelArtifact.Id,
				Uri: new("s3://bucket/model-v2.onnx"),
			},
		})
		require.NoError(t, err)
		require.Equal(t, "s3://bucket/model-v2.onnx", *updated.ModelArtifact.Uri)
	})

	t.Run("list defaults, ordering, and pagination", func(t *testing.T) {
		rmA, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "compat-page-a"})
		require.NoError(t, err)
		rmB, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "compat-page-b"})
		require.NoError(t, err)

		// Default list options return every model ordered by ID ascending.
		all, err := service.GetRegisteredModels(api.ListOptions{})
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(all.Items), 2)
		ids := []int32{}
		for _, item := range all.Items {
			id, err := parseID(*item.Id)
			require.NoError(t, err)
			ids = append(ids, id)
		}
		for i := 1; i < len(ids); i++ {
			require.Less(t, ids[i-1], ids[i], "default ordering must be ID ascending")
		}
		require.Equal(t, int32(len(all.Items)), all.Size)

		// pageSize=1 returns one item and a token for the next page.
		pageSize := int32(1)
		first, err := service.GetRegisteredModels(api.ListOptions{PageSize: &pageSize})
		require.NoError(t, err)
		require.Len(t, first.Items, 1)
		require.Equal(t, int32(1), first.Size)
		require.NotNil(t, first.NextPageToken)

		second, err := service.GetRegisteredModels(api.ListOptions{PageSize: &pageSize, NextPageToken: new(first.NextPageToken)})
		require.NoError(t, err)
		require.Len(t, second.Items, 1)
		require.NotEqual(t, *first.Items[0].Id, *second.Items[0].Id)

		_ = rmA
		_ = rmB
	})

	t.Run("filtering", func(t *testing.T) {
		_, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Name:       "compat-filter-pytorch",
			ExternalId: new("compat-filter-ext-001"),
			CustomProperties: map[string]openapi.MetadataValue{
				"framework": stringValue("pytorch"),
				"accuracy":  doubleValue(0.95),
			},
		})
		require.NoError(t, err)

		exact, err := service.GetRegisteredModels(api.ListOptions{FilterQuery: new("name = 'compat-filter-pytorch'")})
		require.NoError(t, err)
		require.Len(t, exact.Items, 1)

		like, err := service.GetRegisteredModels(api.ListOptions{FilterQuery: new("name LIKE 'compat-filter-%'")})
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(like.Items), 1)

		byExt, err := service.GetRegisteredModels(api.ListOptions{FilterQuery: new("externalId = 'compat-filter-ext-001'")})
		require.NoError(t, err)
		require.Len(t, byExt.Items, 1)

		byProp, err := service.GetRegisteredModels(api.ListOptions{FilterQuery: new("framework = 'pytorch'")})
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(byProp.Items), 1)

		byNumeric, err := service.GetRegisteredModels(api.ListOptions{FilterQuery: new("accuracy > 0.9")})
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(byNumeric.Items), 1)

		none, err := service.GetRegisteredModels(api.ListOptions{FilterQuery: new("name = 'compat-nonexistent'")})
		require.NoError(t, err)
		require.Empty(t, none.Items)
	})

	t.Run("external id uniqueness scope", func(t *testing.T) {
		// The reference backend stores registered models and model
		// versions in one table and model artifacts in another, so an
		// external ID is unique across models and versions jointly and
		// separately within artifacts.
		rmA, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Name: "compat-ext-scope-a", ExternalId: new("compat-scope-model-ext"),
		})
		require.NoError(t, err)
		rmB, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "compat-ext-scope-b"})
		require.NoError(t, err)

		_, err = service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Name: "compat-ext-scope-c", ExternalId: new("compat-scope-model-ext"),
		})
		require.Error(t, err)
		assert.Equal(t, httpConflict, api.ErrToStatus(err))

		_, err = service.UpsertModelVersion(&openapi.ModelVersion{
			Name: "1.0.0", ExternalId: new("compat-scope-version-ext"),
		}, rmB.Id)
		require.NoError(t, err)
		_, err = service.UpsertModelVersion(&openapi.ModelVersion{
			Name: "2.0.0", ExternalId: new("compat-scope-version-ext"),
		}, rmA.Id)
		require.Error(t, err)
		assert.Equal(t, httpConflict, api.ErrToStatus(err))
		_, err = service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Name: "compat-ext-scope-d", ExternalId: new("compat-scope-version-ext"),
		})
		require.Error(t, err)
		assert.Equal(t, httpConflict, api.ErrToStatus(err))

		// An artifact may reuse a model's external ID...
		_, err = service.UpsertArtifact(&openapi.Artifact{
			ModelArtifact: &openapi.ModelArtifact{
				Name: new("compat-ext-scope-artifact"), Uri: new("s3://bucket/model.onnx"),
				ExternalId: new("compat-scope-model-ext"),
			},
		})
		require.NoError(t, err)
		// ...but not another artifact's.
		_, err = service.UpsertArtifact(&openapi.Artifact{
			ModelArtifact: &openapi.ModelArtifact{
				Name: new("compat-ext-scope-artifact-2"), Uri: new("s3://bucket/model.onnx"),
				ExternalId: new("compat-scope-model-ext"),
			},
		})
		require.Error(t, err)
		assert.Equal(t, httpConflict, api.ErrToStatus(err))

		// Unset external IDs never conflict.
		_, err = service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "compat-ext-scope-e"})
		require.NoError(t, err)
	})

	t.Run("case sensitive names", func(t *testing.T) {
		lower, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "compat-case-name"})
		require.NoError(t, err)

		// Names differing only in case are distinct resources; exact
		// Kubeflow names are preserved. ( The MySQL reference fixture
		// runs with a binary collation so it matches PostgreSQL. )
		upper, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "Compat-Case-Name"})
		require.NoError(t, err)
		require.NotEqual(t, *lower.Id, *upper.Id)

		byName, err := service.GetRegisteredModelByParams(new("compat-case-name"), nil)
		require.NoError(t, err)
		require.Equal(t, *lower.Id, *byName.Id)
		byUpper, err := service.GetRegisteredModelByParams(new("Compat-Case-Name"), nil)
		require.NoError(t, err)
		require.Equal(t, *upper.Id, *byUpper.Id)

		// Filters are case-sensitive for equality.
		exact, err := service.GetRegisteredModels(api.ListOptions{FilterQuery: new("name = 'Compat-Case-Name'")})
		require.NoError(t, err)
		require.Len(t, exact.Items, 1)
		require.Equal(t, *upper.Id, *exact.Items[0].Id)
	})

	t.Run("pageSize zero returns the full list", func(t *testing.T) {
		zero := int32(0)
		list, err := service.GetRegisteredModels(api.ListOptions{PageSize: &zero})
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(list.Items), 1)
		require.Equal(t, int32(len(list.Items)), list.Size)
		assert.Empty(t, list.NextPageToken, "pageSize=0 must not paginate")
	})

	t.Run("filter conjunction and disjunction precedence", func(t *testing.T) {
		_, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Name: "compat-bool-pytorch",
			CustomProperties: map[string]openapi.MetadataValue{
				"framework": stringValue("pytorch"),
				"stage":     stringValue("prod"),
			},
		})
		require.NoError(t, err)
		_, err = service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Name: "compat-bool-tensorflow",
			CustomProperties: map[string]openapi.MetadataValue{
				"framework": stringValue("tensorflow"),
				"stage":     stringValue("dev"),
			},
		})
		require.NoError(t, err)

		both, err := service.GetRegisteredModels(api.ListOptions{
			FilterQuery: new("framework = 'pytorch' AND stage = 'prod'"),
		})
		require.NoError(t, err)
		require.Len(t, both.Items, 1)

		// OR binds looser than AND: this matches the pytorch/prod model
		// plus every dev model.
		mixed, err := service.GetRegisteredModels(api.ListOptions{
			FilterQuery: new("framework = 'pytorch' AND stage = 'prod' OR stage = 'dev'"),
		})
		require.NoError(t, err)
		names := []string{}
		for _, item := range mixed.Items {
			names = append(names, item.Name)
		}
		require.Contains(t, names, "compat-bool-pytorch")
		require.Contains(t, names, "compat-bool-tensorflow")
	})

	t.Run("typed metadata round trip", func(t *testing.T) {
		rm, err := service.UpsertRegisteredModel(&openapi.RegisteredModel{
			Name: "compat-typed",
			CustomProperties: map[string]openapi.MetadataValue{
				"str":    stringValue("hello"),
				"empty":  stringValue(""),
				"int":    intValue("42"),
				"double": doubleValue(1.25),
				"bool":   boolValue(true),
			},
		})
		require.NoError(t, err)

		got, err := service.GetRegisteredModelById(*rm.Id)
		require.NoError(t, err)
		require.Contains(t, got.CustomProperties, "str")
		require.Equal(t, "hello", got.CustomProperties["str"].MetadataStringValue.GetStringValue())
		require.Contains(t, got.CustomProperties, "empty")
		require.Equal(t, "", got.CustomProperties["empty"].MetadataStringValue.GetStringValue())
		require.Equal(t, "42", got.CustomProperties["int"].MetadataIntValue.GetIntValue())
		require.Equal(t, 1.25, got.CustomProperties["double"].MetadataDoubleValue.GetDoubleValue())
		require.True(t, got.CustomProperties["bool"].MetadataBoolValue.GetBoolValue())
	})
}

// TestEmbedMDModelContract runs the backend-agnostic contract against the
// existing EmbedMD implementation to establish the reference behavior.
func TestEmbedMDModelContract(t *testing.T) {
	service, cleanup := setupEmbedMDService(t)
	defer cleanup()

	runModelContract(t, service)
}

// TestMLflowModelContract runs the same backend-agnostic contract
// against the MLflow compatibility backend on both database engines.
// The compile-time interface assertion in the package guarantees no
// interface method is absent; this test guarantees the behavior.
func TestMLflowModelContract(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			runModelContract(t, backend.service)
		})
	}
}
