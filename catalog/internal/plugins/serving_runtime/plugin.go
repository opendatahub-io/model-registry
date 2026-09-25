package serving_runtime

import (
	"context"

	"github.com/go-chi/chi/v5"

	mapset "github.com/deckarep/golang-set/v2"

	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog"
	servingRuntimeVersionmodels "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	servingRuntimemodels "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	serving_runtimeservice "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/service"
	"github.com/kubeflow/hub/catalog/internal/db/models"
	"github.com/kubeflow/hub/catalog/internal/plugin"
	v1 "github.com/kubeflow/hub/catalog/internal/server/openapi/v1"
	"github.com/kubeflow/hub/internal/platform/datastore"
)

type Plugin struct {
	*plugin.PluginBase
	loader   *serving_runtimecatalog.ServingRuntimeLoader
	services serving_runtimecatalog.Services
}

func (p *Plugin) Name() string                   { return "serving_runtime" }
func (p *Plugin) Version() string                { return "v1" }
func (p *Plugin) Description() string            { return "Serving runtime catalog" }
func (p *Plugin) BasePath() string               { return "/api/serving_runtime_catalog/v1" }
func (p *Plugin) Healthy() bool                  { return true }
func (p *Plugin) Migrations() []plugin.Migration { return nil }

func (p *Plugin) DatastoreEntries() []plugin.DatastoreEntry {
	return []plugin.DatastoreEntry{
		{
			TypeName: "kf.ServingRuntime",
			Category: "context",
			Spec: datastore.NewSpecType(serving_runtimeservice.NewServingRuntimeRepository).
				AddString("source_id").
				AddString("displayName").
				AddString("description").
				AddString("provider").
				AddString("readme").
				AddString("logo").
				AddString("license").
				AddString("licenseLink").
				AddString("documentationUrl").
				AddString("repositoryUrl").
				AddStruct("tags").
				AddStruct("supportedModelFormats").
				AddStruct("capabilities").
				AddInt("versionCount").
				AddString("publishedDate").
				AddString("lastUpdated"),
		},
		{
			TypeName: "kf.ServingRuntimeVersion",
			Category: "artifact",
			Spec: datastore.NewSpecType(serving_runtimeservice.NewServingRuntimeVersionRepository).
				AddString("source_id").
				AddString("artifactType").
				AddString("version").
				AddString("image").
				AddString("supportLevel").
				AddStruct("supportedModelFormats").
				AddStruct("protocolVersions").
				AddStruct("recommendedResources").
				AddStruct("defaultArgs").
				AddStruct("env").
				AddString("template").
				AddBoolean("deprecated").
				AddString("publishedDate"),
		},
	}
}

func (p *Plugin) Init(_ context.Context, cfg plugin.Config) error {
	p.services = serving_runtimecatalog.Services{
		ServingRuntimeRepository:        plugin.GetRepo[servingRuntimemodels.ServingRuntimeRepository](cfg.RepoSet),
		ServingRuntimeVersionRepository: plugin.GetRepo[servingRuntimeVersionmodels.ServingRuntimeVersionRepository](cfg.RepoSet),
		CatalogSourceRepository:         plugin.GetRepo[models.CatalogSourceRepository](cfg.RepoSet),
		PropertyOptionsRepository:       plugin.GetRepo[models.PropertyOptionsRepository](cfg.RepoSet),
	}

	base := basecatalog.NewBaseLoader(cfg.ConfigPaths)
	p.loader = serving_runtimecatalog.NewServingRuntimeLoader(p.services, base)

	p.PluginBase = plugin.NewPluginBase(plugin.PluginBaseConfig{
		Name:        "serving_runtime",
		State:       base,
		Loader:      p.loader,
		FileWatcher: basecatalog.GetMonitor(),
		SourceIDs: func() mapset.Set[string] {
			ids := mapset.NewSet[string]()
			for id := range p.loader.Sources.AllSources() {
				ids.Add(id)
			}
			return ids
		},
	})

	return nil
}

// ServingRuntimeSources returns the loader's source collection for the shared sources endpoint.
func (p *Plugin) ServingRuntimeSources() *serving_runtimecatalog.ServingRuntimeSourceCollection {
	return p.loader.Sources
}

func (p *Plugin) RegisterRoutes(router chi.Router) error {
	provider := serving_runtimecatalog.NewDBServingRuntimeCatalog(p.services, p.loader.Sources)

	// v1 routes
	v1Ctrl := v1.NewServingRuntimeCatalogServiceAPIController(
		v1.NewServingRuntimeCatalogServiceAPIService(provider, p.loader.Sources),
	)
	for _, route := range v1Ctrl.OrderedRoutes() {
		router.Method(route.Method, route.Pattern, route.HandlerFunc)
	}

	return nil
}
