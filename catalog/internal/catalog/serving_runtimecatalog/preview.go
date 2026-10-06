package serving_runtimecatalog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	model "github.com/kubeflow/hub/catalog/pkg/openapi"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// ServingRuntimePreviewConfig contains the source settings used for name discovery.
// Other source fields are ignored; serving runtime previews do not apply filters.
type ServingRuntimePreviewConfig struct {
	Type       string         `json:"type" yaml:"type"`
	Properties map[string]any `json:"properties,omitempty" yaml:"properties,omitempty"`
}

func ParseServingRuntimePreviewConfig(configBytes []byte) (*ServingRuntimePreviewConfig, error) {
	var config ServingRuntimePreviewConfig
	if err := yaml.Unmarshal(configBytes, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	if config.Type == "" {
		return nil, fmt.Errorf("missing required field: type")
	}
	return &config, nil
}

// PreviewSourceRuntimes discovers names without validating runtime metadata or
// versions. Uploaded catalog data takes precedence over the configured file path.
func PreviewSourceRuntimes(ctx context.Context, config *ServingRuntimePreviewConfig, catalogDataBytes []byte) ([]model.AssetPreviewResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.Type != "yaml" {
		return nil, fmt.Errorf("unsupported source type for serving runtime preview: %s", config.Type)
	}
	catalogBytes := catalogDataBytes
	if len(catalogBytes) == 0 {
		path, ok := config.Properties[yamlServingRuntimeCatalogPathKey].(string)
		if !ok || path == "" {
			return nil, fmt.Errorf("missing required property: %s (provide catalogData file or set yamlCatalogPath in config)", yamlServingRuntimeCatalogPathKey)
		}
		if !filepath.IsAbs(path) {
			cwd, err := os.Getwd()
			if err != nil {
				return nil, fmt.Errorf("failed to get working directory: %w", err)
			}
			path = filepath.Join(cwd, path)
		}
		var err error
		catalogBytes, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read catalog file %s: %w", path, err)
		}
	}
	// Decode only names so discovery remains independent of loader validation.
	// ServingRuntimes is a pointer so a missing/mistyped top-level key (nil) can be
	// told apart from an explicit, intentionally empty list ("serving_runtimes: []"),
	// matching the same distinction loadServingRuntimesFromYAML makes for the loader.
	var catalog struct {
		ServingRuntimes *[]struct {
			Name string `json:"name"`
		} `json:"serving_runtimes"`
	}
	if err := yaml.Unmarshal(catalogBytes, &catalog); err != nil {
		return nil, fmt.Errorf("failed to parse catalog file: %w", err)
	}
	if catalog.ServingRuntimes == nil {
		return nil, fmt.Errorf("serving_runtime catalog has no top-level 'serving_runtimes' key; " +
			"refusing to treat it as an empty catalog (use `serving_runtimes: []` to empty a source)")
	}
	results := make([]model.AssetPreviewResult, 0, len(*catalog.ServingRuntimes))
	for _, runtime := range *catalog.ServingRuntimes {
		if runtime.Name == "" {
			continue
		}
		results = append(results, model.AssetPreviewResult{Name: runtime.Name, Included: true})
	}
	return results, nil
}
