package serving_runtimecatalog

import (
	"encoding/json"
	"fmt"

	openapi "github.com/kubeflow/hub/catalog/pkg/openapi"
)

// yamlMetadataValue decodes the discriminated custom-property value without
// silently accepting an unknown discriminator or keys. The generated
// openapi.MetadataValue.UnmarshalJSON accepts an unknown metadataType as an
// empty union, which is unsafe for a strict catalog source.
type yamlMetadataValue struct {
	Value   openapi.MetadataValue
	Problem string
}

func (v *yamlMetadataValue) UnmarshalJSON(data []byte) error {
	if err := v.decode(data); err != nil {
		v.Problem = err.Error()
	}
	return nil
}

func (v *yamlMetadataValue) decode(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return fmt.Errorf("custom property must be an object")
	}
	var metadataType string
	if err := json.Unmarshal(fields["metadataType"], &metadataType); err != nil || metadataType == "" {
		return fmt.Errorf("custom property requires a metadataType string")
	}

	var required []string
	switch metadataType {
	case "MetadataStringValue":
		required = []string{"string_value"}
	case "MetadataBoolValue":
		required = []string{"bool_value"}
	case "MetadataIntValue":
		required = []string{"int_value"}
	case "MetadataDoubleValue":
		required = []string{"double_value"}
	case "MetadataProtoValue":
		required = []string{"type", "proto_value"}
	case "MetadataStructValue":
		required = []string{"struct_value"}
	default:
		return fmt.Errorf("custom property has unsupported metadataType")
	}
	allowed := map[string]bool{"metadataType": true}
	for _, key := range required {
		allowed[key] = true
		if _, ok := fields[key]; !ok {
			return fmt.Errorf("custom property %s requires %s", metadataType, key)
		}
	}
	for key := range fields {
		if !allowed[key] {
			return fmt.Errorf("custom property %s has unknown field %q", metadataType, key)
		}
	}

	switch metadataType {
	case "MetadataStringValue":
		var value openapi.MetadataStringValue
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("invalid MetadataStringValue")
		}
		v.Value = openapi.MetadataStringValueAsMetadataValue(&value)
	case "MetadataBoolValue":
		var value openapi.MetadataBoolValue
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("invalid MetadataBoolValue")
		}
		v.Value = openapi.MetadataBoolValueAsMetadataValue(&value)
	case "MetadataIntValue":
		var value openapi.MetadataIntValue
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("invalid MetadataIntValue")
		}
		v.Value = openapi.MetadataIntValueAsMetadataValue(&value)
	case "MetadataDoubleValue":
		var value openapi.MetadataDoubleValue
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("invalid MetadataDoubleValue")
		}
		v.Value = openapi.MetadataDoubleValueAsMetadataValue(&value)
	case "MetadataProtoValue":
		var value openapi.MetadataProtoValue
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("invalid MetadataProtoValue")
		}
		v.Value = openapi.MetadataProtoValueAsMetadataValue(&value)
	case "MetadataStructValue":
		var value openapi.MetadataStructValue
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("invalid MetadataStructValue")
		}
		v.Value = openapi.MetadataStructValueAsMetadataValue(&value)
	}
	return nil
}
