package serving_runtimecatalog

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type preflightRuntimeRepository struct {
	models.ServingRuntimeRepository
	calls int
}

func (r *preflightRuntimeRepository) GetByName(string) (models.ServingRuntime, error) {
	r.calls++
	return nil, errors.New("repository should not be reached")
}

func (r *preflightRuntimeRepository) Save(models.ServingRuntime) (models.ServingRuntime, error) {
	r.calls++
	return nil, errors.New("repository should not be reached")
}

const validRuntimeYAML = `serving_runtimes:
  - name: vllm
    license: apache-2.0
    customProperties:
      owner: {metadataType: MetadataStringValue, string_value: team}
    versions:
      - version: "1"
        image: registry.example.com/vllm:1
        supportLevel: supported
`

func TestServingRuntimeFamilyValidationKeepsSiblingResults(t *testing.T) {
	var data strings.Builder
	data.WriteString("serving_runtimes:\n")
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&data, "  - name: runtime-%d\n    versions:\n      - {version: '1', image: registry.example.com/runtime-%d:1}\n", i, i)
		if i == 6 {
			data.WriteString("      - {version: '2', image: '???'}\n")
		}
	}
	families, err := parseServingRuntimeFamiliesYAML([]byte(data.String()))
	require.NoError(t, err)
	require.Len(t, families, 6)
	for i := 0; i < 5; i++ {
		assert.NoError(t, families[i].Err, "family %d should be eligible for loading", i+1)
	}
	assert.Equal(t, "runtime-6", families[5].Name)
	require.Error(t, families[5].Err)
	assert.Contains(t, families[5].Err.Error(), `version="2"`)
	assert.Contains(t, families[5].Err.Error(), "serving_runtimes[5].versions[1].image")
	_, err = parseServingRuntimesYAML([]byte(data.String()))
	require.Error(t, err, "the strict compatibility API must reject mixed results")
}

func TestServingRuntimeFamilyValidationSeparatesDocumentAndFamilyErrors(t *testing.T) {
	for _, tc := range []struct {
		name, badFamily, field string
	}{
		{"unknown version field", "    versions: [{version: '1', image: registry.example.com/bad:v1, supportLevl: supported}]\n", "supportLevl"},
		{"invalid field type", "    versions: [{version: '1', image: registry.example.com/bad:v1, deprecated: not-a-bool}]\n", "serving_runtimes[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := "serving_runtimes:\n  - name: good\n    versions: [{version: '1', image: registry.example.com/good:v1}]\n  - name: bad\n" + tc.badFamily
			families, err := parseServingRuntimeFamiliesYAML([]byte(data))
			require.NoError(t, err)
			require.Len(t, families, 2)
			assert.NoError(t, families[0].Err)
			require.Error(t, families[1].Err)
			assert.Contains(t, families[1].Err.Error(), tc.field)
		})
	}
	families, err := parseServingRuntimeFamiliesYAML([]byte("serving_runtimes: ["))
	require.Error(t, err)
	assert.Empty(t, families, "a document syntax error must not produce loadable families")
}

func TestServingRuntimeFamilyValidationRejectsBothDuplicateNames(t *testing.T) {
	data := `serving_runtimes:
  - name: repeated
  - name: independent
  - name: repeated
`
	families, err := parseServingRuntimeFamiliesYAML([]byte(data))
	require.NoError(t, err)
	require.Len(t, families, 3)
	assert.ErrorContains(t, families[0].Err, "duplicate runtime name")
	assert.NoError(t, families[1].Err)
	assert.ErrorContains(t, families[2].Err, "duplicate runtime name")
}

func TestServingRuntimeValidationRejectsUnknownAndMisplacedFields(t *testing.T) {
	for _, tc := range []struct{ name, data, field string }{
		{"unknown version field", strings.Replace(validRuntimeYAML, "supportLevel: supported", "supportLevl: supported", 1), "supportLevl"},
		{"wrong field capitalization", strings.Replace(validRuntimeYAML, "supportLevel: supported", "supportlevel: supported", 1), "supportlevel"},
		{"wrong minimumRHOAIVersion capitalization", strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        minimumRhoaiVersion: '3.0'\n        supportLevel: supported", 1), "minimumRhoaiVersion"},
		{"misplaced version field", strings.Replace(validRuntimeYAML, "    license: apache-2.0", "    image: registry.example.com/vllm:1\n    license: apache-2.0", 1), "image"},
		{"duplicate key", strings.Replace(validRuntimeYAML, "    license: apache-2.0", "    license: apache-2.0\n    license: mit", 1), "license"},
		{"unknown custom property field", strings.Replace(validRuntimeYAML, "string_value: team", "string_value: team, extra: ignored", 1), "customProperties"},
		{"unknown custom property type", strings.Replace(validRuntimeYAML, "MetadataStringValue", "UnknownValue", 1), "customProperties"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseServingRuntimesYAML([]byte(tc.data))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.field)
		})
	}
	_, err := parseServingRuntimesYAML([]byte(strings.Replace(validRuntimeYAML, "supportLevel: supported", "supportLevl: supported", 1)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `runtime="vllm"`)
	assert.Contains(t, err.Error(), `version="1"`)
	assert.Contains(t, err.Error(), "serving_runtimes[0].versions[0].supportLevl")
}

func TestServingRuntimeValidationAggregatesIndependentVersionErrors(t *testing.T) {
	data := `serving_runtimes:
  - name: vllm
    versions:
      - version: "1"
        image: registry.example.com/vllm:1
        supportLevel: unknown
      - version: "2"
        image: registry.example.com/vllm:2
        env:
          - name: HF_TOKEN
            secret: true
            defaultValue: unsafe-literal
`
	_, err := parseServingRuntimesYAML([]byte(data))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `version="1"`)
	assert.Contains(t, err.Error(), "supportLevel")
	assert.Contains(t, err.Error(), `version="2"`)
	assert.Contains(t, err.Error(), "env[0].defaultValue")
	assert.NotContains(t, err.Error(), "unsafe-literal")
}

func TestServingRuntimeValidationFields(t *testing.T) {
	for _, tc := range []struct{ name, data, field string }{
		{"bad image", strings.Replace(validRuntimeYAML, "registry.example.com/vllm:1", "???", 1), "image"},
		{"unpinned image", strings.Replace(validRuntimeYAML, "registry.example.com/vllm:1", "registry.example.com/vllm", 1), "image"},
		{"latest image", strings.Replace(validRuntimeYAML, "registry.example.com/vllm:1", "registry.example.com/vllm:latest", 1), "image"},
		{"oversized license", strings.Replace(validRuntimeYAML, "apache-2.0", strings.Repeat("a", maxStringBytes+1), 1), "license"},
		{"bad date", strings.Replace(validRuntimeYAML, "    license: apache-2.0", "    publishedDate: yesterday\n    license: apache-2.0", 1), "publishedDate"},
		{"bad URL", strings.Replace(validRuntimeYAML, "    license: apache-2.0", "    repositoryUrl: javascript:alert(1)\n    license: apache-2.0", 1), "repositoryUrl"},
		{"bad quantity", strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        recommendedResources:\n          recommended: {cpu: invalid}\n        supportLevel: supported", 1), "recommendedResources.recommended.cpu"},
		{"bad accelerator", strings.Replace(validRuntimeYAML, "    license: apache-2.0", "    capabilities: {supportedAccelerators: [not-qualified]}\n    license: apache-2.0", 1), "supportedAccelerators"},
		{"bad model format", strings.Replace(validRuntimeYAML, "    license: apache-2.0", "    supportedModelFormats: [{autoSelect: true}]\n    license: apache-2.0", 1), "supportedModelFormats[0].name"},
		{"reserved custom property", strings.Replace(validRuntimeYAML, "owner: {metadataType: MetadataStringValue, string_value: team}", "source_id: {metadataType: MetadataStringValue, string_value: other}", 1), "customProperties"},
		{"overflowing custom property", strings.Replace(validRuntimeYAML, "owner: {metadataType: MetadataStringValue, string_value: team}", "priority: {metadataType: MetadataIntValue, int_value: '3000000000'}", 1), "int_value"},
		{"bad template version", strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        servingRuntimeTemplate: '{\"apiVersion\":\"serving.kserve.io/v1beta1\",\"kind\":\"ServingRuntime\",\"spec\":{\"containers\":[{\"image\":\"registry.example.com/vllm:1\"}]}}'\n        supportLevel: supported", 1), "servingRuntimeTemplate.apiVersion"},
		{"template literal secret", strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        servingRuntimeTemplate: '{\"apiVersion\":\"serving.kserve.io/v1alpha1\",\"kind\":\"ServingRuntime\",\"spec\":{\"containers\":[{\"name\":\"runtime\",\"image\":\"registry.example.com/vllm:1\",\"env\":[{\"name\":\"HF_TOKEN\",\"value\":\"unsafe-literal\"}]}]}}'\n        supportLevel: supported", 1), "env[0].value"},
		{"bad minimumRHOAIVersion", strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        minimumRHOAIVersion: latest\n        supportLevel: supported", 1), "minimumRHOAIVersion"},
		{"template wrapper without runtime", strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        servingRuntimeTemplate: '{\"apiVersion\":\"template.openshift.io/v1\",\"kind\":\"Template\",\"objects\":[{\"apiVersion\":\"v1\",\"kind\":\"ConfigMap\"}]}'\n        supportLevel: supported", 1), "servingRuntimeTemplate.objects"},
		{"template wrapper with two runtimes", strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        servingRuntimeTemplate: '{\"apiVersion\":\"template.openshift.io/v1\",\"kind\":\"Template\",\"objects\":[{\"kind\":\"ServingRuntime\"},{\"kind\":\"ServingRuntime\"}]}'\n        supportLevel: supported", 1), "exactly one ServingRuntime"},
		{"template wrapper bad API version", strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        servingRuntimeTemplate: '{\"apiVersion\":\"template.openshift.io/v2\",\"kind\":\"Template\",\"objects\":[{\"apiVersion\":\"serving.kserve.io/v1alpha1\",\"kind\":\"ServingRuntime\",\"spec\":{\"containers\":[{\"name\":\"runtime\",\"image\":\"registry.example.com/vllm:1\"}]}}]}'\n        supportLevel: supported", 1), "servingRuntimeTemplate.apiVersion"},
		{"template wrapper image mismatch", strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        servingRuntimeTemplate: '{\"apiVersion\":\"template.openshift.io/v1\",\"kind\":\"Template\",\"objects\":[{\"apiVersion\":\"serving.kserve.io/v1alpha1\",\"kind\":\"ServingRuntime\",\"spec\":{\"containers\":[{\"name\":\"runtime\",\"image\":\"registry.example.com/other:1\"}]}}]}'\n        supportLevel: supported", 1), "servingRuntimeTemplate.objects[0].spec.containers"},
		{"version format absent from family", strings.Replace(strings.Replace(validRuntimeYAML, "    license: apache-2.0", "    supportedModelFormats: [{name: safetensors}]\n    license: apache-2.0", 1), "        supportLevel: supported", "        supportedModelFormats: [{name: onnx}]\n        supportLevel: supported", 1), "not listed by the runtime family"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseServingRuntimesYAML([]byte(tc.data))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.field)
		})
	}
}

func TestServingRuntimeValidationAcceleratorQuantity(t *testing.T) {
	for _, tc := range []struct {
		value   string
		wantErr bool
	}{
		{value: "1"},
		{value: "1000m"},
		{value: "1.0"},
		{value: "3Ki"},
		{value: "1500m", wantErr: true},
		{value: "0.5", wantErr: true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			resources := fmt.Sprintf("        recommendedResources:\n          recommended:\n            accelerator:\n              nvidia.com/gpu: %q\n        supportLevel: supported", tc.value)
			data := strings.Replace(validRuntimeYAML, "        supportLevel: supported", resources, 1)
			entries, err := parseServingRuntimesYAML([]byte(data))
			if tc.wantErr {
				require.ErrorContains(t, err, "accelerator quantity must be a whole number")
				return
			}
			require.NoError(t, err)
			require.Len(t, entries, 1)
		})
	}
}

func TestServingRuntimeValidationPreservesCustomLicense(t *testing.T) {
	const license = "custom-proprietary-license-1.0"
	data := strings.Replace(validRuntimeYAML, "apache-2.0", license, 1)
	entries, err := parseServingRuntimesYAML([]byte(data))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NotNil(t, entries[0].License)
	assert.Equal(t, license, *entries[0].License)
}

func TestServingRuntimeValidationMissingListAndExplicitEmptyList(t *testing.T) {
	_, err := parseServingRuntimesYAML([]byte("source: redhat\n"))
	require.ErrorContains(t, err, "serving_runtimes")
	entries, err := parseServingRuntimesYAML([]byte("serving_runtimes: []\n"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestServingRuntimeValidationLimits(t *testing.T) {
	t.Run("oversized file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "large.yaml")
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", maxRuntimeCatalogBytes+1)), 0600))
		_, err := loadServingRuntimesFromYAML(path)
		require.ErrorContains(t, err, "exceeds")
	})
	t.Run("oversized readme", func(t *testing.T) {
		data := strings.Replace(validRuntimeYAML, "    license: apache-2.0", "    readme: "+strings.Repeat("a", maxReadmeBytes+1)+"\n    license: apache-2.0", 1)
		_, err := parseServingRuntimesYAML([]byte(data))
		require.ErrorContains(t, err, "readme")
	})
	t.Run("readme at limit", func(t *testing.T) {
		data := strings.Replace(validRuntimeYAML, "    license: apache-2.0", "    readme: "+strings.Repeat("a", maxReadmeBytes)+"\n    license: apache-2.0", 1)
		_, err := parseServingRuntimesYAML([]byte(data))
		require.NoError(t, err)
	})
	t.Run("logo at decoded limit", func(t *testing.T) {
		logo := base64.StdEncoding.EncodeToString(make([]byte, maxLogoBytes))
		data := strings.Replace(validRuntimeYAML, "    license: apache-2.0", "    logo: data:image/png;base64,"+logo+"\n    license: apache-2.0", 1)
		_, err := parseServingRuntimesYAML([]byte(data))
		require.NoError(t, err)
	})
	t.Run("oversized decoded logo", func(t *testing.T) {
		logo := base64.StdEncoding.EncodeToString(make([]byte, maxLogoBytes+1))
		data := strings.Replace(validRuntimeYAML, "    license: apache-2.0", "    logo: data:image/png;base64,"+logo+"\n    license: apache-2.0", 1)
		_, err := parseServingRuntimesYAML([]byte(data))
		require.ErrorContains(t, err, "logo")
	})
}

func TestServingRuntimeValidationAcceptsTemplateJSON(t *testing.T) {
	data := strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        servingRuntimeTemplate: '{\"apiVersion\":\"serving.kserve.io/v1alpha1\",\"kind\":\"ServingRuntime\",\"spec\":{\"containers\":[{\"name\":\"runtime\",\"image\":\"registry.example.com/vllm:1\"}]}}'\n        supportLevel: supported", 1)
	data = "source: producer-label\n" + data
	_, err := parseServingRuntimesYAML([]byte(data))
	require.NoError(t, err)
}

func TestServingRuntimeValidationAcceptsOpenShiftTemplateWrapper(t *testing.T) {
	template := `{"apiVersion":"template.openshift.io/v1","kind":"Template","metadata":{"name":"vllm-template"},"objects":[{"apiVersion":"serving.kserve.io/v1alpha1","kind":"ServingRuntime","metadata":{"name":"vllm"},"spec":{"containers":[{"name":"kserve-container","image":"registry.example.com/vllm:1"}]}}],"parameters":[]}`
	data := strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        servingRuntimeTemplate: '"+template+"'\n        supportLevel: supported", 1)
	entries, err := parseServingRuntimesYAML([]byte(data))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NotNil(t, entries[0].Versions[0].ServingRuntimeTemplate)
	assert.JSONEq(t, template, *entries[0].Versions[0].ServingRuntimeTemplate, "the wrapper is stored unchanged")
}

func TestServingRuntimeValidationAcceptsMinimumRHOAIVersion(t *testing.T) {
	for _, value := range []string{"3.6", "3.6.1", "10.0", "v3.6", "3.6.0-ea.1", "v3.6.0-ea.1-1788535633", "3.6.0+rhaiv.8"} {
		t.Run(value, func(t *testing.T) {
			data := strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        minimumRHOAIVersion: '"+value+"'\n        supportLevel: supported", 1)
			entries, err := parseServingRuntimesYAML([]byte(data))
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.NotNil(t, entries[0].Versions[0].MinimumRHOAIVersion)
			assert.Equal(t, value, *entries[0].Versions[0].MinimumRHOAIVersion)
		})
	}
}

func TestServingRuntimeValidationRejectsMalformedMinimumRHOAIVersion(t *testing.T) {
	for _, value := range []string{"3", "3.x", "rhoai-3.6", "3.6.0.1", "3.6-", "3.6+", "3.6-ea..1", "V3.6"} {
		t.Run(value, func(t *testing.T) {
			data := strings.Replace(validRuntimeYAML, "        supportLevel: supported", "        minimumRHOAIVersion: '"+value+"'\n        supportLevel: supported", 1)
			_, err := parseServingRuntimesYAML([]byte(data))
			require.ErrorContains(t, err, "minimumRHOAIVersion")
		})
	}
}

func TestServingRuntimeTemplateSecretNameDetection(t *testing.T) {
	assert.True(t, looksLikeSecretName("HF_TOKEN"))
	assert.True(t, looksLikeSecretName("API_KEY"))
	assert.False(t, looksLikeSecretName("TOKENIZER_CONFIG"))
}

func TestServingRuntimeValidationTruncatesDiagnostics(t *testing.T) {
	var data strings.Builder
	data.WriteString("serving_runtimes:\n")
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&data, "  - name: runtime-%d\n    versions: [{version: '1', image: '???', supportLevel: unknown}]\n", i)
	}
	_, err := parseServingRuntimesYAML([]byte(data.String()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more error(s) omitted")
	assert.LessOrEqual(t, len(err.Error()), maxValidationTextBytes)
}

func TestServingRuntimeLoaderRejectsLaterInvalidEntryBeforeRepositoryAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtimes.yaml")
	data := `serving_runtimes:
  - name: alpha
    versions: [{version: "1", image: registry.example.com/alpha:new}]
  - name: beta
    customProperties:
      source_id: {metadataType: MetadataStringValue, string_value: other}
    versions: [{version: "1", image: registry.example.com/beta:new}]
`
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	repo := &preflightRuntimeRepository{}
	state := basecatalog.NewBaseLoader(nil)
	state.SetLeader(true)
	loader := NewServingRuntimeLoader(Services{ServingRuntimeRepository: repo}, state)
	err := loader.loadFromYAML(t.Context(), "first", basecatalog.PluginSource{Properties: map[string]any{yamlServingRuntimeCatalogPathKey: path}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `source="first"`)
	assert.Contains(t, err.Error(), `runtime="beta"`)
	assert.Zero(t, repo.calls)
}
