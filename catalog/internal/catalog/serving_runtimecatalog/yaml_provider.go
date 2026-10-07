package serving_runtimecatalog

import (
	"fmt"
	"io"
	"os"

	openapi "github.com/kubeflow/hub/catalog/pkg/openapi"
	yamlv3 "gopkg.in/yaml.v3"
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
	Version                   string                                        `yaml:"version" json:"version"`
	Image                     string                                        `yaml:"image" json:"image"`
	SupportLevel              *string                                       `yaml:"supportLevel,omitempty" json:"supportLevel,omitempty"`
	SupportedModelFormats     []openapi.SupportedModelFormat                `yaml:"supportedModelFormats,omitempty" json:"supportedModelFormats,omitempty"`
	ProtocolVersions          []string                                      `yaml:"protocolVersions,omitempty" json:"protocolVersions,omitempty"`
	RecommendedResources      *openapi.ServingRuntimeResourceRecommendation `yaml:"recommendedResources,omitempty" json:"recommendedResources,omitempty"`
	DefaultArgs               []string                                      `yaml:"defaultArgs,omitempty" json:"defaultArgs,omitempty"`
	Env                       []openapi.ServingRuntimeEnvVar                `yaml:"env,omitempty" json:"env,omitempty"`
	ServingRuntimeTemplate    *string                                       `yaml:"servingRuntimeTemplate,omitempty" json:"servingRuntimeTemplate,omitempty"`
	LlmInferenceServiceConfig *string                                       `yaml:"llmInferenceServiceConfig,omitempty" json:"llmInferenceServiceConfig,omitempty"`
	Deprecated                *bool                                         `yaml:"deprecated,omitempty" json:"deprecated,omitempty"`
	PublishedDate             *string                                       `yaml:"publishedDate,omitempty" json:"publishedDate,omitempty"`
	ExternalID                *string                                       `yaml:"externalId,omitempty" json:"externalId,omitempty"`
}

// validatedServingRuntimeFamily keeps a family's identity and validation
// result together so callers can skip rejected families without losing their
// names for cleanup decisions.
type validatedServingRuntimeFamily struct {
	Name    string
	Runtime yamlServingRuntime
	Err     error
}

// loadServingRuntimeFamiliesFromYAML reads the data once and reports validation
// independently for each family. Document-level failures have no family result.
func loadServingRuntimeFamiliesFromYAML(path string) ([]validatedServingRuntimeFamily, error) {
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
	families, err := parseServingRuntimeFamiliesYAML(data)
	if err != nil {
		return nil, fmt.Errorf("invalid serving_runtime catalog file %s: %w", path, err)
	}
	return families, nil
}

// parseServingRuntimeFamiliesYAML parses the document before any family is
// published. A version error rejects its whole family, not its siblings.
func parseServingRuntimeFamiliesYAML(data []byte) ([]validatedServingRuntimeFamily, error) {
	if len(data) > maxRuntimeCatalogBytes {
		return nil, fmt.Errorf("serving_runtime catalog exceeds %d bytes", maxRuntimeCatalogBytes)
	}
	nodes, err := runtimeYAMLFamilyNodes(data)
	if err != nil {
		return nil, err
	}
	names := make(map[string]int, len(nodes))
	for _, node := range nodes {
		names[runtimeYAMLFamilyName(node)]++
	}
	families := make([]validatedServingRuntimeFamily, 0, len(nodes))
	shape := runtimeYAMLShape().fields["serving_runtimes"].items
	for index, node := range nodes {
		name := runtimeYAMLFamilyName(node)
		path := fmt.Sprintf("serving_runtimes[%d]", index)
		issues := &runtimeValidationErrors{}
		inspectRuntimeYAMLNode(node, shape, path, "", "", issues)
		var runtime yamlServingRuntime
		if issues.err() == nil {
			encoded, err := yamlv3.Marshal(node)
			if err != nil {
				issues.add(name, "", path, "failed to encode YAML family")
			} else if err := yaml.UnmarshalStrict(encoded, &runtime); err != nil {
				issues.add(name, "", path, "failed to decode YAML family: "+diagnosticText(err.Error(), 512))
			} else {
				issues.appendIssues(validateServingRuntimeFamily(runtime, index, names[name] > 1))
			}
		}
		families = append(families, validatedServingRuntimeFamily{Name: name, Runtime: runtime, Err: issues.err()})
	}
	return families, nil
}

// loadServingRuntimesFromYAML keeps the existing all-or-nothing loader behavior
// for callers that do not yet process per-family results.
func loadServingRuntimesFromYAML(path string) ([]yamlServingRuntime, error) {
	families, err := loadServingRuntimeFamiliesFromYAML(path)
	if err != nil {
		return nil, err
	}
	return validatedRuntimes(families)
}

// parseServingRuntimesYAML is the strict compatibility interface for callers
// that cannot publish partial results yet.
func parseServingRuntimesYAML(data []byte) ([]yamlServingRuntime, error) {
	families, err := parseServingRuntimeFamiliesYAML(data)
	if err != nil {
		return nil, err
	}
	return validatedRuntimes(families)
}

func validatedRuntimes(families []validatedServingRuntimeFamily) ([]yamlServingRuntime, error) {
	entries := make([]yamlServingRuntime, 0, len(families))
	issues := &runtimeValidationErrors{}
	for index, family := range families {
		if family.Err != nil {
			if familyIssues, ok := family.Err.(*runtimeValidationErrors); ok {
				issues.appendIssues(familyIssues)
			} else {
				issues.add(family.Name, "", fmt.Sprintf("serving_runtimes[%d]", index), diagnosticText(family.Err.Error(), 512))
			}
			continue
		}
		entries = append(entries, family.Runtime)
	}
	if err := issues.err(); err != nil {
		return nil, err
	}
	return entries, nil
}
