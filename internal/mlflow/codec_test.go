package mlflow_test

import (
	"math"
	"strings"
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func protoValue(payload string) openapi.MetadataValue {
	return openapi.MetadataValue{MetadataProtoValue: &openapi.MetadataProtoValue{
		Type: "type.googleapis.com/foo.Bar", ProtoValue: payload, MetadataType: "MetadataProtoValue"}}
}

func structValue(payload string) openapi.MetadataValue {
	return openapi.MetadataValue{MetadataStructValue: &openapi.MetadataStructValue{
		StructValue: payload, MetadataType: "MetadataStructValue"}}
}

// TestCodecModelMapping covers every registered-model mapping row.
func TestCodecModelMapping(t *testing.T) {
	live := openapi.REGISTEREDMODELSTATE_LIVE
	model := &openapi.RegisteredModel{
		Name:        "owner-model",
		Owner:       new("alice"),
		ExternalId:  new("ext-1"),
		State:       &live,
		Description: new("native description"),
		CustomProperties: map[string]openapi.MetadataValue{
			"labels.env":             stringValue("prod"),
			"empty-label":            stringValue(""),
			"rhoai.owner":            stringValue("user attempt"),
			"rhoai.custom.x":         stringValue("escape namespace collision"),
			strings.Repeat("k", 251): stringValue("overlong key"),
			"":                       stringValue("empty key"),
		},
	}

	tags, err := mlflow.EncodeModel(model)
	require.NoError(t, err)

	assert.Equal(t, "alice", tags["rhoai.owner"])
	assert.Equal(t, "ext-1", tags["rhoai.externalId"])
	assert.Equal(t, "LIVE", tags["rhoai.state"])
	// Descriptions map to the native description field, never a tag.
	_, hasDescription := tags["rhoai.description"]
	assert.False(t, hasDescription)

	// Valid unreserved keys pass through, including empty-string labels.
	assert.Equal(t, "prod", tags["labels.env"])
	assert.Equal(t, "", tags["empty-label"])

	// Reserved and invalid keys escape under rhoai.custom.<sha256(key)>
	// with distinct digests; the original-key mapping stays in SQL.
	escapedOwner := tags[mlflow.EscapeTagKey("rhoai.owner")]
	assert.Equal(t, "user attempt", escapedOwner)
	escapedNamespace := tags[mlflow.EscapeTagKey("rhoai.custom.x")]
	assert.Equal(t, "escape namespace collision", escapedNamespace)
	assert.NotEqual(t, escapedOwner, escapedNamespace)
	overlongKey := strings.Repeat("k", 251)
	assert.Equal(t, "overlong key", tags[mlflow.EscapeTagKey(overlongKey)])
	assert.NotContains(t, tags, overlongKey)
	assert.NotContains(t, tags, "")
}

// TestCodecVersionMapping covers every model-version mapping row.
func TestCodecVersionMapping(t *testing.T) {
	live := openapi.MODELVERSIONSTATE_LIVE
	version := &openapi.ModelVersion{
		Name:        "1.0.0",
		Author:      new("bob"),
		ExternalId:  new("ext-v1"),
		State:       &live,
		Description: new("version description"),
		CustomProperties: map[string]openapi.MetadataValue{
			"accuracy": doubleValue(0.5),
		},
	}

	tags, err := mlflow.EncodeVersion(version)
	require.NoError(t, err)

	assert.Equal(t, "1.0.0", tags["rhoai.versionName"])
	assert.Equal(t, "bob", tags["rhoai.author"])
	assert.Equal(t, "ext-v1", tags["rhoai.externalId"])
	assert.Equal(t, "LIVE", tags["rhoai.state"])
	assert.Equal(t, "0.5", tags["accuracy"])
}

// TestCodecArtifactMapping covers the primary-artifact mapping rows and
// the additional-artifact namespace.
func TestCodecArtifactMapping(t *testing.T) {
	unknown := openapi.ARTIFACTSTATE_UNKNOWN
	artifact := &openapi.ModelArtifact{
		Name:               new("primary-artifact"),
		Description:        new("artifact description"),
		ExternalId:         new("ext-a"),
		Uri:                new("s3://bucket/model.onnx"),
		ModelFormatName:    new("onnx"),
		ModelFormatVersion: new("1.16"),
		StorageKey:         new("my-connection"),
		StoragePath:        new("models/"),
		ServiceAccountName: new("sa"),
		ModelSourceKind:    new("pipelines"),
		ModelSourceClass:   new("pipelinerun"),
		ModelSourceGroup:   new("namespace"),
		ModelSourceName:    new("run-42"),
		ModelSourceId:      new("run-id-42"),
		ArtifactType:       new("model-artifact"),
		State:              &unknown,
		CustomProperties: map[string]openapi.MetadataValue{
			"framework": stringValue("pytorch"),
		},
	}

	tags, err := mlflow.EncodeArtifact(artifact, true)
	require.NoError(t, err)

	assert.Equal(t, "s3://bucket/model.onnx", tags["rhoai.sourceUri"])
	assert.Equal(t, "onnx", tags["rhoai.modelFormat"])
	assert.Equal(t, "1.16", tags["rhoai.modelFormatVersion"])
	assert.Equal(t, "my-connection", tags["rhoai.connection"])
	assert.Equal(t, "models/", tags["rhoai.storagePath"])
	assert.Equal(t, "sa", tags["rhoai.serviceAccountName"])
	assert.Equal(t, "pipelines", tags["rhoai.modelSourceKind"])
	assert.Equal(t, "pipelinerun", tags["rhoai.modelSourceClass"])
	assert.Equal(t, "namespace", tags["rhoai.modelSourceGroup"])
	assert.Equal(t, "run-42", tags["rhoai.modelSourceName"])
	assert.Equal(t, "run-id-42", tags["rhoai.modelSourceId"])
	assert.Equal(t, "pytorch", tags["rhoai.artifact.framework"])

	// Fields without a native counterpart are mirrored.
	assert.Equal(t, "primary-artifact", tags["rhoai.artifactRecord.name"])
	assert.Equal(t, "artifact description", tags["rhoai.artifactRecord.description"])
	assert.Equal(t, "ext-a", tags["rhoai.artifactRecord.externalId"])
	assert.Equal(t, "model-artifact", tags["rhoai.artifactRecord.artifactType"])
	assert.Equal(t, "UNKNOWN", tags["rhoai.artifactRecord.state"])

	// An additional artifact projects under rhoai.artifacts.* and never
	// collides with the primary projection. Each sub-namespace keeps its
	// own suffix: direct field keys stay bare, mirrored fields keep
	// artifactRecord.*, and custom properties keep artifact.*.
	additional, err := mlflow.EncodeArtifact(artifact, false)
	require.NoError(t, err)
	assert.Equal(t, "s3://bucket/model.onnx", additional["rhoai.artifacts.sourceUri"])
	assert.Equal(t, "pytorch", additional["rhoai.artifacts.artifact.framework"])
	assert.Equal(t, "primary-artifact", additional["rhoai.artifacts.artifactRecord.name"])
	for key := range additional {
		assert.True(t, strings.HasPrefix(key, "rhoai.artifacts."),
			"additional artifact key %q must be namespaced", key)
	}
}

// TestCodecAdditionalArtifactNamespaceCollisions proves the additional
// artifact qualification is injective: fields mirrored under
// artifactRecord.*, custom properties under artifact.*, and direct field
// keys keep distinct names even when they share a suffix.
func TestCodecAdditionalArtifactNamespaceCollisions(t *testing.T) {
	unknown := openapi.ARTIFACTSTATE_UNKNOWN
	artifact := &openapi.ModelArtifact{
		Name:            new("record-name"),
		Description:     new("record-description"),
		Uri:             new("s3://bucket/model.onnx"),
		ModelFormatName: new("onnx"),
		State:           &unknown,
		CustomProperties: map[string]openapi.MetadataValue{
			"name":           stringValue("property-name"),
			"description":    stringValue("property-description"),
			"modelFormat":    stringValue("property-modelFormat"),
			"sourceUri":      stringValue("property-sourceUri"),
			"state":          stringValue("property-state"),
			"dotted.key":     stringValue("dotted-value"),
			"rhoai.custom.x": stringValue("reserved-value"),
		},
	}

	additional, err := mlflow.EncodeArtifact(artifact, false)
	require.NoError(t, err)

	escaped := strings.TrimPrefix("rhoai.artifact."+mlflow.EscapeTagKey("rhoai.custom.x"), "rhoai.")
	expected := map[string]string{
		// Direct field keys keep their bare names.
		"rhoai.artifacts.sourceUri":   "s3://bucket/model.onnx",
		"rhoai.artifacts.modelFormat": "onnx",
		// Mirrored fields stay under artifactRecord.*.
		"rhoai.artifacts.artifactRecord.name":        "record-name",
		"rhoai.artifacts.artifactRecord.description": "record-description",
		"rhoai.artifacts.artifactRecord.state":       "UNKNOWN",
		// Custom properties stay under artifact.*, including dotted and
		// escaped reserved keys.
		"rhoai.artifacts.artifact.name":        "property-name",
		"rhoai.artifacts.artifact.description": "property-description",
		"rhoai.artifacts.artifact.modelFormat": "property-modelFormat",
		"rhoai.artifacts.artifact.sourceUri":   "property-sourceUri",
		"rhoai.artifacts.artifact.state":       "property-state",
		"rhoai.artifacts.artifact.dotted.key":  "dotted-value",
		"rhoai.artifacts." + escaped:           "reserved-value",
	}
	assert.Equal(t, expected, additional,
		"every additional-artifact key must keep its full sub-namespace")
}

// TestCodecMetadataDiscriminators proves each typed property renders
// losslessly to its tag string and that invalid values are rejected.
func TestCodecMetadataDiscriminators(t *testing.T) {
	base := &openapi.RegisteredModel{Name: "discriminators"}
	with := func(properties map[string]openapi.MetadataValue) *openapi.RegisteredModel {
		model := *base
		model.CustomProperties = properties
		return &model
	}

	tags, err := mlflow.EncodeModel(with(map[string]openapi.MetadataValue{
		"b":  boolValue(true),
		"b2": boolValue(false),
		"i":  intValue("-42"),
		"i2": intValue("0"),
		"d":  doubleValue(0.1),
		"d2": doubleValue(1e21),
		"s":  stringValue("héllo"),
		"p":  protoValue("cHJvdG8="),
		"st": structValue("c3RydWN0"),
	}))
	require.NoError(t, err)
	assert.Equal(t, "true", tags["b"])
	assert.Equal(t, "false", tags["b2"])
	assert.Equal(t, "-42", tags["i"])
	assert.Equal(t, "0", tags["i2"])
	// Shortest round-trip formatting, not float64 printf artifacts.
	assert.Equal(t, "0.1", tags["d"])
	assert.Equal(t, "1e+21", tags["d2"])
	assert.Equal(t, "héllo", tags["s"])
	assert.Equal(t, "cHJvdG8=", tags["p"])
	assert.Equal(t, "c3RydWN0", tags["st"])

	// Nonfinite doubles are rejected before any native write.
	for name, value := range map[string]openapi.MetadataValue{
		"nan": doubleValue(math.NaN()),
		"inf": doubleValue(math.Inf(1)),
	} {
		_, err := mlflow.EncodeModel(with(map[string]openapi.MetadataValue{"v": value}))
		require.ErrorIs(t, err, api.ErrBadRequest, "%s must be rejected", name)
	}

	// Non-canonical integers and unset discriminators are malformed.
	_, err = mlflow.EncodeModel(with(map[string]openapi.MetadataValue{"v": intValue("1.5")}))
	require.ErrorIs(t, err, api.ErrBadRequest)
	_, err = mlflow.EncodeModel(with(map[string]openapi.MetadataValue{"v": {}}))
	require.ErrorIs(t, err, api.ErrBadRequest)

	// Large integers stay strings; they are never decoded through
	// float64.
	tags, err = mlflow.EncodeModel(with(map[string]openapi.MetadataValue{
		"big": intValue("999999999"),
	}))
	require.NoError(t, err)
	assert.Equal(t, "999999999", tags["big"])
}

// TestCodecTagValueBound proves the adapter-owned value bound is
// enforced by rejection at the documented boundary, never truncation.
func TestCodecTagValueBound(t *testing.T) {
	model := &openapi.RegisteredModel{
		Name: "bound-model",
		CustomProperties: map[string]openapi.MetadataValue{
			"at-bound": stringValue(strings.Repeat("a", 5000)),
		},
	}
	tags, err := mlflow.EncodeModel(model)
	require.NoError(t, err)
	assert.Len(t, tags["at-bound"], 5000)

	model.CustomProperties["over-bound"] = stringValue(strings.Repeat("a", 5001))
	_, err = mlflow.EncodeModel(model)
	require.ErrorIs(t, err, api.ErrBadRequest)
	assert.NotContains(t, err.Error(), strings.Repeat("a", 100),
		"the rejection must not echo the oversized value")
}

// TestCodecNameValidation proves incompatible names are rejected before
// any native write, and that omitted maps and nil fields are preserved.
func TestCodecNameValidation(t *testing.T) {
	for _, name := range []string{"", "model/with-slash", "model:with-colon"} {
		_, err := mlflow.EncodeModel(&openapi.RegisteredModel{Name: name})
		require.ErrorIs(t, err, api.ErrBadRequest, "name %q", name)
	}
	_, err := mlflow.EncodeVersion(&openapi.ModelVersion{Name: ""})
	require.ErrorIs(t, err, api.ErrBadRequest)
	for _, name := range []string{"release/2026", "release:2026"} {
		tags, err := mlflow.EncodeVersion(&openapi.ModelVersion{Name: name})
		require.NoError(t, err)
		assert.Equal(t, name, tags["rhoai.versionName"])
	}

	// Omitted maps and fields produce only the always-present keys.
	tags, err := mlflow.EncodeModel(&openapi.RegisteredModel{Name: "minimal"})
	require.NoError(t, err)
	assert.Empty(t, tags)

	tags, err = mlflow.EncodeVersion(&openapi.ModelVersion{Name: "1.0.0"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"rhoai.versionName": "1.0.0"}, tags)

	// Nil inputs are malformed.
	_, err = mlflow.EncodeModel(nil)
	require.ErrorIs(t, err, api.ErrBadRequest)
	_, err = mlflow.EncodeVersion(nil)
	require.ErrorIs(t, err, api.ErrBadRequest)
	_, err = mlflow.EncodeArtifact(nil, true)
	require.ErrorIs(t, err, api.ErrBadRequest)

	// A 250-character key is valid; 251 is escaped, matching the
	// verified server limit.
	validKey := strings.Repeat("k", 250)
	tags, err = mlflow.EncodeModel(&openapi.RegisteredModel{
		Name:             "key-length",
		CustomProperties: map[string]openapi.MetadataValue{validKey: stringValue("v")},
	})
	require.NoError(t, err)
	assert.Equal(t, "v", tags[validKey])

	// Unicode keys and values round-trip exactly when within limits.
	tags, err = mlflow.EncodeModel(&openapi.RegisteredModel{
		Name: "unicode",
		CustomProperties: map[string]openapi.MetadataValue{
			"héllo": stringValue("wörld"),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "wörld", tags["héllo"])
}
