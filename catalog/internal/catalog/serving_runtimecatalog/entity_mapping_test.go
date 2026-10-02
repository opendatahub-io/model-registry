package serving_runtimecatalog

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/serving_runtimecatalog/models"
	openapi "github.com/kubeflow/hub/catalog/pkg/openapi"
	dbmodels "github.com/kubeflow/hub/internal/platform/db/entity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildServingRuntimeEntityRejectsInvalidCustomProperties(t *testing.T) {
	for _, tt := range []struct {
		name      string
		key       string
		value     openapi.MetadataValue
		parseCode error
	}{
		{name: "reserved source identity", key: "source_id", value: openapi.MetadataValue{MetadataStringValue: &openapi.MetadataStringValue{StringValue: "another-source"}}},
		{name: "reserved runtime identity", key: "base_name", value: openapi.MetadataValue{MetadataStringValue: &openapi.MetadataStringValue{StringValue: "another-runtime"}}},
		{name: "non-numeric integer", key: "priority", value: openapi.MetadataValue{MetadataIntValue: &openapi.MetadataIntValue{IntValue: "high"}}, parseCode: strconv.ErrSyntax},
		{name: "integer exceeds int32", key: "priority", value: openapi.MetadataValue{MetadataIntValue: &openapi.MetadataIntValue{IntValue: "2147483648"}}, parseCode: strconv.ErrRange},
		{name: "integer below int32", key: "priority", value: openapi.MetadataValue{MetadataIntValue: &openapi.MetadataIntValue{IntValue: "-2147483649"}}, parseCode: strconv.ErrRange},
	} {
		t.Run(tt.name, func(t *testing.T) {
			loader := &ServingRuntimeLoader{}
			entity, err := loader.buildServingRuntimeEntity("community", yamlServingRuntime{
				Name: "vllm", CustomProperties: &map[string]openapi.MetadataValue{tt.key: tt.value},
			})
			require.Error(t, err, "community:vllm must reject invalid custom property %q", tt.key)
			assert.Nil(t, entity, "community:vllm must not return a partially built entity for property %q", tt.key)
			assert.ErrorContains(t, err, "vllm", "entity construction failure must identify runtime")
			assert.ErrorContains(t, err, tt.key, "entity construction failure must identify custom property")
			if tt.parseCode != nil {
				assert.ErrorIs(t, err, tt.parseCode, "community:vllm custom property %q must preserve integer conversion cause", tt.key)
			}
		})
	}
}

func TestBuildServingRuntimeEntityAcceptsInt32Boundaries(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  int32
	}{
		{value: "2147483647", want: 2147483647},
		{value: "-2147483648", want: -2147483648},
	} {
		t.Run(tt.value, func(t *testing.T) {
			loader := &ServingRuntimeLoader{}
			entity, err := loader.buildServingRuntimeEntity("community", yamlServingRuntime{
				Name: "vllm", CustomProperties: &map[string]openapi.MetadataValue{
					"priority": {MetadataIntValue: &openapi.MetadataIntValue{IntValue: tt.value}},
				},
			})
			require.NoError(t, err, "community:vllm must accept int32 boundary priority=%s", tt.value)
			require.NotNil(t, entity, "community:vllm must produce an entity for valid custom properties")
			require.NotNil(t, entity.GetCustomProperties(), "community:vllm must retain custom properties")
			require.Len(t, *entity.GetCustomProperties(), 1, "community:vllm must retain the priority property")
			property := (*entity.GetCustomProperties())[0]
			assert.Equal(t, "priority", property.Name, "community:vllm custom property name")
			require.NotNil(t, property.IntValue, "community:vllm priority must retain its integer type")
			assert.Equal(t, tt.want, *property.IntValue, "community:vllm priority must retain the int32 boundary value")
		})
	}
}

func TestServingRuntimeAPIMappingPreservesStructuredProperties(t *testing.T) {
	entity := &models.ServingRuntimeImpl{
		Attributes: &models.ServingRuntimeAttributes{Name: new("community:vllm")},
		Properties: &[]dbmodels.Properties{
			dbmodels.NewStringProperty("tags", `["gpu"]`, false),
			dbmodels.NewStringProperty("supportedModelFormatsDetails", `[{"name":"onnx","version":"1"}]`, false),
			dbmodels.NewStringProperty("capabilities", `{"requiresGPU":true}`, false),
		},
	}
	got, err := mapDBServingRuntimeToAPI(entity)
	require.NoError(t, err, "community:vllm must map valid structured properties")
	assert.Equal(t, []string{"gpu"}, got.Tags, "community:vllm tags must survive API mapping")
	require.Len(t, got.SupportedModelFormats, 1, "community:vllm supported format must survive API mapping")
	assert.Equal(t, "onnx", got.SupportedModelFormats[0].Name, "community:vllm format name")
	assert.Equal(t, "1", got.SupportedModelFormats[0].GetVersion(), "community:vllm format version")
	require.NotNil(t, got.Capabilities, "community:vllm capabilities must survive API mapping")
	assert.True(t, got.Capabilities.GetRequiresGPU(), "community:vllm requiresGPU must survive API mapping")
}

func TestServingRuntimeVersionAPIMappingPreservesStructuredProperties(t *testing.T) {
	entity := &models.ServingRuntimeVersionImpl{
		Attributes: &models.ServingRuntimeVersionAttributes{Name: new("community:vllm:1.0")},
		Properties: &[]dbmodels.Properties{
			dbmodels.NewStringProperty("supportedModelFormatsDetails", `[{"name":"onnx"}]`, false),
			dbmodels.NewStringProperty("protocolVersions", `["v2"]`, false),
			dbmodels.NewStringProperty("recommendedResources", `{"minimal":{"cpu":"2"}}`, false),
			dbmodels.NewStringProperty("defaultArgs", `["--port=8080"]`, false),
			dbmodels.NewStringProperty("envDetails", `[{"name":"MODE","defaultValue":"production"}]`, false),
		},
	}
	got, err := mapDBServingRuntimeVersionToAPI(entity)
	require.NoError(t, err, "community:vllm:1.0 must map valid structured properties")
	require.Len(t, got.SupportedModelFormats, 1, "community:vllm:1.0 supported format must survive API mapping")
	assert.Equal(t, "onnx", got.SupportedModelFormats[0].Name, "community:vllm:1.0 format name")
	assert.Equal(t, []string{"v2"}, got.ProtocolVersions, "community:vllm:1.0 protocols must survive API mapping")
	require.NotNil(t, got.RecommendedResources, "community:vllm:1.0 resources must survive API mapping")
	require.NotNil(t, got.RecommendedResources.Minimal, "community:vllm:1.0 minimal resources must survive API mapping")
	assert.Equal(t, "2", got.RecommendedResources.Minimal.GetCpu(), "community:vllm:1.0 CPU recommendation")
	assert.Equal(t, []string{"--port=8080"}, got.DefaultArgs, "community:vllm:1.0 arguments must survive API mapping")
	require.Len(t, got.Env, 1, "community:vllm:1.0 environment must survive API mapping")
	assert.Equal(t, "MODE", got.Env[0].Name, "community:vllm:1.0 environment name")
	assert.Equal(t, "production", got.Env[0].GetDefaultValue(), "community:vllm:1.0 environment default value")
}

func TestServingRuntimeAPIMappingRejectsMalformedStructuredProperties(t *testing.T) {
	for _, property := range []string{"tags", "supportedModelFormatsDetails", "capabilities"} {
		t.Run(property, func(t *testing.T) {
			for _, payload := range []string{"{broken", "42"} {
				t.Run(payload, func(t *testing.T) {
					entity := &models.ServingRuntimeImpl{
						Attributes: &models.ServingRuntimeAttributes{Name: new("community:vllm")},
						Properties: &[]dbmodels.Properties{dbmodels.NewStringProperty(property, payload, false)},
					}
					_, err := mapDBServingRuntimeToAPI(entity)
					require.Error(t, err, "community:vllm API mapping must reject malformed %s=%q", property, payload)
					assertMappingJSONError(t, err, "vllm", property, payload)
				})
			}
		})
	}
}

func TestServingRuntimeVersionAPIMappingRejectsMalformedStructuredProperties(t *testing.T) {
	for _, property := range []string{"supportedModelFormatsDetails", "protocolVersions", "recommendedResources", "defaultArgs", "envDetails"} {
		t.Run(property, func(t *testing.T) {
			for _, payload := range []string{"{broken", "42"} {
				t.Run(payload, func(t *testing.T) {
					entity := &models.ServingRuntimeVersionImpl{
						Attributes: &models.ServingRuntimeVersionAttributes{Name: new("community:vllm:1.0")},
						Properties: &[]dbmodels.Properties{dbmodels.NewStringProperty(property, payload, false)},
					}
					_, err := mapDBServingRuntimeVersionToAPI(entity)
					require.Error(t, err, "community:vllm:1.0 API mapping must reject malformed %s=%q", property, payload)
					assertMappingJSONError(t, err, "vllm:1.0", property, payload)
				})
			}
		})
	}
}

func assertMappingJSONError(t *testing.T, err error, name, property, payload string) {
	t.Helper()
	assert.ErrorContains(t, err, name, "API mapping failure must identify runtime/version %q", name)
	assert.ErrorContains(t, err, property, "API mapping failure for %q must identify property %q", name, property)
	if payload == "{broken" {
		var syntaxError *json.SyntaxError
		assert.ErrorAs(t, err, &syntaxError, "API mapping failure for %s.%s must preserve JSON syntax cause", name, property)
	} else {
		var typeError *json.UnmarshalTypeError
		assert.ErrorAs(t, err, &typeError, "API mapping failure for %s.%s must preserve JSON type cause", name, property)
	}
}
