package activation

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/kubeflow/hub/catalog/internal/catalog/agentcatalog"
	agentmodels "github.com/kubeflow/hub/catalog/internal/catalog/agentcatalog/models"
	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/mcpcatalog"
	mcpmodels "github.com/kubeflow/hub/catalog/internal/catalog/mcpcatalog/models"
	"github.com/kubeflow/hub/catalog/internal/catalog/modelcatalog"
	modelmodels "github.com/kubeflow/hub/catalog/internal/catalog/modelcatalog/models"
	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog"
	runtimemodels "github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	"github.com/kubeflow/hub/catalog/internal/db/models"
	"github.com/kubeflow/hub/catalog/internal/plugin"
	"github.com/kubeflow/hub/internal/platform/datastore"
	entity "github.com/kubeflow/hub/internal/platform/db/entity"
)

// ImportRequired is synchronous and only accepts transaction-bound repositories.
// No plugin leader or background watcher participates in publishing a candidate.
func ImportRequired(ctx context.Context, configs, benchmarkPaths []string, repos datastore.RepoSet) error {
	modelServices := modelcatalog.Services{
		CatalogModelRepository:           plugin.GetRepo[modelmodels.CatalogModelRepository](repos),
		CatalogModelArtifactRepository:   plugin.GetRepo[modelmodels.CatalogModelArtifactRepository](repos),
		CatalogMetricsArtifactRepository: plugin.GetRepo[modelmodels.CatalogMetricsArtifactRepository](repos),
	}
	metrics, err := modelcatalog.NewPerformanceMetricsLoader(benchmarkPaths, modelServices.CatalogModelRepository, modelServices.CatalogMetricsArtifactRepository, repos.TypeMap())
	if err != nil {
		return err
	}
	mcpServices := mcpcatalog.Services{
		MCPServerRepository:     plugin.GetRepo[mcpmodels.MCPServerRepository](repos),
		MCPServerToolRepository: plugin.GetRepo[mcpmodels.MCPServerToolRepository](repos),
	}
	agentServices := agentcatalog.Services{
		AgentRepository:                 plugin.GetRepo[agentmodels.AgentRepository](repos),
		AgentTemplateArtifactRepository: plugin.GetRepo[agentmodels.AgentTemplateArtifactRepository](repos),
	}
	runtimeServices := serving_runtimecatalog.Services{
		ServingRuntimeRepository:        plugin.GetRepo[runtimemodels.ServingRuntimeRepository](repos),
		ServingRuntimeVersionRepository: plugin.GetRepo[runtimemodels.ServingRuntimeVersionRepository](repos),
	}
	sourceRepo := plugin.GetRepo[models.CatalogSourceRepository](repos)
	for _, configPath := range configs {
		config, err := basecatalog.ReadSourceConfig(configPath)
		if err != nil {
			return err
		}
		load := func(id string, enabled bool, properties map[string]any, importSource func(string) error) error {
			status := basecatalog.SourceStatusDisabled
			if enabled {
				path, ok := properties["yamlCatalogPath"].(string)
				if !ok || path == "" {
					return fmt.Errorf("required source %q has no YAML path", id)
				}
				if !filepath.IsAbs(path) {
					path = filepath.Join(filepath.Dir(configPath), path)
				}
				if err := importSource(path); err != nil {
					return fmt.Errorf("required source %q: %w", id, err)
				}
				status = basecatalog.SourceStatusAvailable
			}
			props := []entity.Properties{entity.NewStringProperty("status", status, false)}
			_, err := sourceRepo.Save(&models.CatalogSourceImpl{Attributes: &models.CatalogSourceAttributes{Name: &id}, Properties: &props})
			return err
		}
		for _, source := range config.GetModelCatalogs() {
			if err := load(source.Id, source.Enabled == nil || *source.Enabled, source.Properties, func(path string) error {
				return modelcatalog.ImportRequired(ctx, path, &source, modelServices, metrics)
			}); err != nil {
				return err
			}
		}
		for _, source := range config.MCPCatalogs {
			if err := load(source.ID, source.IsEnabled(), source.Properties, func(path string) error {
				return mcpcatalog.ImportRequired(ctx, path, &source, mcpServices)
			}); err != nil {
				return err
			}
		}
		for _, source := range config.AgentCatalogs {
			if err := load(source.ID, source.IsEnabled(), source.Properties, func(path string) error {
				return agentcatalog.ImportRequired(ctx, path, source.ID, agentServices)
			}); err != nil {
				return err
			}
		}
		for _, source := range config.ServingRuntimeCatalogs {
			if err := load(source.ID, source.IsEnabled(), source.Properties, func(path string) error {
				return serving_runtimecatalog.ImportRequired(ctx, path, source.ID, runtimeServices)
			}); err != nil {
				return err
			}
		}
	}
	options := plugin.GetRepo[models.PropertyOptionsRepository](repos)
	if err := options.Refresh(models.ContextPropertyOptionType); err != nil {
		return err
	}
	return options.Refresh(models.ArtifactPropertyOptionType)
}
