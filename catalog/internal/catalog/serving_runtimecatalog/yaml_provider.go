package serving_runtimecatalog

import (
	"fmt"
	"io"
	"os"

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
	CustomProperties      *map[string]yamlMetadataValue       `yaml:"customProperties,omitempty" json:"customProperties,omitempty"`
	Versions              []yamlServingRuntimeVersion         `yaml:"versions,omitempty" json:"versions,omitempty"`
}

// yamlServingRuntimeVersion is the on-disk representation of a serving_runtime version.
type yamlServingRuntimeVersion struct {
	Version               string                                        `yaml:"version" json:"version"`
	Image                 string                                        `yaml:"image" json:"image"`
	SupportLevel          *string                                       `yaml:"supportLevel,omitempty" json:"supportLevel,omitempty"`
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
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read serving_runtime catalog file %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxRuntimeCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read serving_runtime catalog file %s: %w", path, err)
	}
	if len(data) > maxRuntimeCatalogBytes {
		return nil, fmt.Errorf("serving_runtime catalog file %s exceeds %d bytes", path, maxRuntimeCatalogBytes)
	}
	entries, err := parseServingRuntimesYAML(data)
	if err != nil {
		return nil, fmt.Errorf("invalid serving_runtime catalog file %s: %w", path, err)
	}
	return entries, nil
}

// parseServingRuntimesYAML validates the same bytes that the loader will publish.
// A caller must not re-read the file after this function succeeds.
func parseServingRuntimesYAML(data []byte) ([]yamlServingRuntime, error) {
	if len(data) > maxRuntimeCatalogBytes {
		return nil, fmt.Errorf("serving_runtime catalog exceeds %d bytes", maxRuntimeCatalogBytes)
	}
	if err := inspectRuntimeYAMLStructure(data); err != nil {
		return nil, err
	}
	var catalog yamlServingRuntimeCatalog
	if err := yaml.UnmarshalStrict(data, &catalog); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	if err := validateServingRuntimeCatalog(&catalog); err != nil {
		return nil, err
	}
	return catalog.ServingRuntimes, nil
}
