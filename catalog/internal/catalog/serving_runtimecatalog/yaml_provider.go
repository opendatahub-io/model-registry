package serving_runtimecatalog

import (
	"fmt"
	"os"
	"strings"

	openapi "github.com/kubeflow/hub/catalog/pkg/openapi"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// yamlServingRuntimeCatalogPathKey is the source property holding the path to the YAML data file.
const yamlServingRuntimeCatalogPathKey = "yamlCatalogPath"

// yamlServingRuntime is the on-disk representation of a serving_runtime entry.
// Every field carries both yaml and json tags: k8s yaml.Unmarshal converts YAML to
// JSON internally, so json tags are required for fields with underscores/camelCase.
// Nested complex fields reuse the generated OpenAPI types (json-tagged), which
// k8s yaml.Unmarshal handles transparently.
type yamlServingRuntime struct {
	Name                  string                              `yaml:"name" json:"name"`
	DisplayName           *string                             `yaml:"displayName,omitempty" json:"displayName,omitempty"`
	Provider              *string                             `yaml:"provider,omitempty" json:"provider,omitempty"`
	Description           *string                             `yaml:"description,omitempty" json:"description,omitempty"`
	Readme                *string                             `yaml:"readme,omitempty" json:"readme,omitempty"`
	Logo                  *string                             `yaml:"logo,omitempty" json:"logo,omitempty"`
	Tags                  []string                            `yaml:"tags,omitempty" json:"tags,omitempty"`
	License               *string                             `yaml:"license,omitempty" json:"license,omitempty"`
	LicenseLink           *string                             `yaml:"licenseLink,omitempty" json:"licenseLink,omitempty"`
	DocumentationURL      *string                             `yaml:"documentationUrl,omitempty" json:"documentationUrl,omitempty"`
	RepositoryURL         *string                             `yaml:"repositoryUrl,omitempty" json:"repositoryUrl,omitempty"`
	SupportedModelFormats []openapi.SupportedModelFormat      `yaml:"supportedModelFormats,omitempty" json:"supportedModelFormats,omitempty"`
	Capabilities          *openapi.ServingRuntimeCapabilities `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	PublishedDate         *string                             `yaml:"publishedDate,omitempty" json:"publishedDate,omitempty"`
	LastUpdated           *string                             `yaml:"lastUpdated,omitempty" json:"lastUpdated,omitempty"`
	ExternalID            *string                             `yaml:"externalId,omitempty" json:"externalId,omitempty"`
	CustomProperties      *map[string]openapi.MetadataValue   `yaml:"customProperties,omitempty" json:"customProperties,omitempty"`
	Versions              []yamlServingRuntimeVersion         `yaml:"versions,omitempty" json:"versions,omitempty"`
}

// yamlServingRuntimeVersion is the on-disk representation of a serving_runtime version.
type yamlServingRuntimeVersion struct {
	Version               string                                        `yaml:"version" json:"version"`
	Image                 string                                        `yaml:"image" json:"image"`
	SupportLevel          *openapi.ServingRuntimeSupportLevel           `yaml:"supportLevel,omitempty" json:"supportLevel,omitempty"`
	SupportedModelFormats []openapi.SupportedModelFormat                `yaml:"supportedModelFormats,omitempty" json:"supportedModelFormats,omitempty"`
	ProtocolVersions      []string                                      `yaml:"protocolVersions,omitempty" json:"protocolVersions,omitempty"`
	RecommendedResources  *openapi.ServingRuntimeResourceRecommendation `yaml:"recommendedResources,omitempty" json:"recommendedResources,omitempty"`
	DefaultArgs           []string                                      `yaml:"defaultArgs,omitempty" json:"defaultArgs,omitempty"`
	Env                   []openapi.ServingRuntimeEnvVar                `yaml:"env,omitempty" json:"env,omitempty"`
	Template              *string                                       `yaml:"template,omitempty" json:"template,omitempty"`
	Deprecated            *bool                                         `yaml:"deprecated,omitempty" json:"deprecated,omitempty"`
	PublishedDate         *string                                       `yaml:"publishedDate,omitempty" json:"publishedDate,omitempty"`
	ExternalID            *string                                       `yaml:"externalId,omitempty" json:"externalId,omitempty"`
}

// yamlServingRuntimeCatalog is the top-level structure of a serving_runtime YAML data file.
type yamlServingRuntimeCatalog struct {
	Source          string               `yaml:"source" json:"source"`
	ServingRuntimes []yamlServingRuntime `yaml:"serving_runtimes" json:"serving_runtimes"`
}

// loadServingRuntimesFromYAML reads and parses a serving_runtime YAML data file.
func loadServingRuntimesFromYAML(path string) ([]yamlServingRuntime, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read serving_runtime catalog file %s: %w", path, err)
	}

	var catalog yamlServingRuntimeCatalog
	if err := yaml.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("failed to parse serving_runtime catalog file %s: %w", path, err)
	}
	names := make(map[string]bool, len(catalog.ServingRuntimes))
	for _, runtime := range catalog.ServingRuntimes {
		if strings.TrimSpace(runtime.Name) == "" {
			return nil, fmt.Errorf("serving_runtime in %s has no name", path)
		}
		if names[runtime.Name] {
			return nil, fmt.Errorf("duplicate serving_runtime %q in %s", runtime.Name, path)
		}
		names[runtime.Name] = true
		versions := make(map[string]bool, len(runtime.Versions))
		for _, version := range runtime.Versions {
			if strings.TrimSpace(version.Version) == "" || strings.TrimSpace(version.Image) == "" {
				return nil, fmt.Errorf("serving_runtime %q in %s has a version without version or image", runtime.Name, path)
			}
			if versions[version.Version] {
				return nil, fmt.Errorf("duplicate version %q for serving_runtime %q in %s", version.Version, runtime.Name, path)
			}
			versions[version.Version] = true
		}
	}

	return catalog.ServingRuntimes, nil
}
