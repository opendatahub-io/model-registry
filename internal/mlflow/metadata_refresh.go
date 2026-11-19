package mlflow

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/kubeflow/hub/pkg/openapi"
)

// mergeExternalValue preserves the discriminator and auxiliary fields when
// the native representation is unchanged. Changed values keep their prior
// type only when valid; otherwise the exact external text becomes a string.
func mergeExternalValue(prior openapi.MetadataValue, text string) (openapi.MetadataValue, error) {
	encoded, err := encodeMetadataValue(prior)
	if err != nil {
		return openapi.MetadataValue{}, err
	}
	if text == encoded {
		return prior, nil
	}
	merged := prior
	switch {
	case prior.MetadataIntValue != nil:
		if _, err := strconv.ParseInt(text, 10, 64); err == nil {
			value := *prior.MetadataIntValue
			value.IntValue = text
			merged.MetadataIntValue = &value
			return merged, nil
		}
	case prior.MetadataDoubleValue != nil:
		if parsed, err := strconv.ParseFloat(text, 64); err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
			value := *prior.MetadataDoubleValue
			value.DoubleValue = parsed
			merged.MetadataDoubleValue = &value
			return merged, nil
		}
	case prior.MetadataBoolValue != nil:
		if parsed, err := strconv.ParseBool(text); err == nil {
			value := *prior.MetadataBoolValue
			value.BoolValue = parsed
			merged.MetadataBoolValue = &value
			return merged, nil
		}
	case prior.MetadataProtoValue != nil:
		if _, err := base64.StdEncoding.DecodeString(text); err == nil {
			value := *prior.MetadataProtoValue
			value.ProtoValue = text
			merged.MetadataProtoValue = &value
			return merged, nil
		}
	case prior.MetadataStructValue != nil:
		if _, err := base64.StdEncoding.DecodeString(text); err == nil {
			value := *prior.MetadataStructValue
			value.StructValue = text
			merged.MetadataStructValue = &value
			return merged, nil
		}
	}
	return openapi.MetadataValue{MetadataStringValue: &openapi.MetadataStringValue{
		StringValue: text, MetadataType: "MetadataStringValue",
	}}, nil
}

// marshalImportedResource checks the projection and JSON encoding before
// publishing native-derived data. A failure rolls back the whole observation.
func marshalImportedResource(resource any) (json.RawMessage, error) {
	var err error
	switch value := resource.(type) {
	case *openapi.RegisteredModel:
		_, err = EncodeModel(value)
	case *openapi.ModelVersion:
		_, err = EncodeVersion(value)
	case *openapi.ModelArtifact:
		_, err = EncodeArtifact(value, true)
	}
	if err != nil {
		return nil, fmt.Errorf("validating imported resource: %w", err)
	}
	encoded, err := json.Marshal(resource)
	if err != nil {
		return nil, fmt.Errorf("serializing imported resource: %w", err)
	}
	return encoded, nil
}
