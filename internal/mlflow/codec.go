package mlflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
)

// The codec projects typed Kubeflow resources onto MLflow tag maps.
// Descriptions map to the native description fields ( handled by the
// service, not the tag projection ); every other mapped field uses an
// adapter-owned "rhoai.*" key. User custom properties keep their
// original keys where valid and unreserved; anything else is escaped
// under "rhoai.custom.<sha256(key)>" with the original-key mapping
// retained in the SQL snapshot.

// maxTagValueLength is the adapter-owned bound on projected tag values.
// The verified MLflow 3.18.0 server does not bound tag values itself;
// the adapter rejects larger values rather than truncating silently.
const maxTagValueLength = maxTagValueRunes

// EncodeModel validates a registered model and returns its native tag
// projection: owner, externalId, state, and escaped custom properties.
// Incompatible names ( containing "/" or ":" ) are rejected before any
// native write.
func EncodeModel(model *openapi.RegisteredModel) (map[string]string, error) {
	if model == nil {
		return nil, fmt.Errorf("registered model is required: %w", api.ErrBadRequest)
	}
	if err := validateNativeName(model.Name); err != nil {
		return nil, err
	}

	tags := map[string]string{}
	setIfSet(tags, "rhoai.owner", model.Owner)
	setIfSet(tags, "rhoai.externalId", model.ExternalId)
	if model.State != nil {
		tags["rhoai.state"] = string(*model.State)
	}
	if err := encodeCustomProperties(tags, model.CustomProperties, ""); err != nil {
		return nil, err
	}
	return tags, nil
}

// EncodeVersion validates a model version and returns its native tag
// projection: versionName, author, externalId, state, and escaped
// custom properties.
func EncodeVersion(version *openapi.ModelVersion) (map[string]string, error) {
	if version == nil {
		return nil, fmt.Errorf("model version is required: %w", api.ErrBadRequest)
	}
	if err := validateLogicalVersionName(version.Name); err != nil {
		return nil, err
	}

	tags := map[string]string{
		"rhoai.versionName": version.Name,
	}
	setIfSet(tags, "rhoai.author", version.Author)
	setIfSet(tags, "rhoai.externalId", version.ExternalId)
	if version.State != nil {
		tags["rhoai.state"] = string(*version.State)
	}
	if err := encodeCustomProperties(tags, version.CustomProperties, ""); err != nil {
		return nil, err
	}
	return tags, nil
}

// EncodeArtifact validates a model artifact and returns its native tag
// projection. A primary artifact projects its fields under the
// unqualified "rhoai.*" keys ( source URI, format, connection, and
// source identification ) and its custom properties under
// "rhoai.artifact.<key>"; fields without a native counterpart are
// mirrored under "rhoai.artifactRecord.<field>". An additional
// ( non-primary ) artifact returns the same relative keys under the
// "rhoai.artifacts." prefix; the service qualifies them with the
// artifact ID because the codec does not know it.
func EncodeArtifact(artifact *openapi.ModelArtifact, primary bool) (map[string]string, error) {
	if artifact == nil {
		return nil, fmt.Errorf("model artifact is required: %w", api.ErrBadRequest)
	}

	tags := map[string]string{}
	setIfSet(tags, "rhoai.sourceUri", artifact.Uri)
	setIfSet(tags, "rhoai.modelFormat", artifact.ModelFormatName)
	setIfSet(tags, "rhoai.modelFormatVersion", artifact.ModelFormatVersion)
	setIfSet(tags, "rhoai.connection", artifact.StorageKey)
	setIfSet(tags, "rhoai.storagePath", artifact.StoragePath)
	setIfSet(tags, "rhoai.serviceAccountName", artifact.ServiceAccountName)
	setIfSet(tags, "rhoai.modelSourceKind", artifact.ModelSourceKind)
	setIfSet(tags, "rhoai.modelSourceClass", artifact.ModelSourceClass)
	setIfSet(tags, "rhoai.modelSourceGroup", artifact.ModelSourceGroup)
	setIfSet(tags, "rhoai.modelSourceName", artifact.ModelSourceName)
	setIfSet(tags, "rhoai.modelSourceId", artifact.ModelSourceId)

	// Fields without a native counterpart are mirrored so the native
	// view can reconstruct the compatibility record.
	setIfSet(tags, "rhoai.artifactRecord.name", artifact.Name)
	setIfSet(tags, "rhoai.artifactRecord.description", artifact.Description)
	setIfSet(tags, "rhoai.artifactRecord.externalId", artifact.ExternalId)
	setIfSet(tags, "rhoai.artifactRecord.artifactType", artifact.ArtifactType)
	setIfSet(tags, "rhoai.artifactRecord.experimentId", artifact.ExperimentId)
	setIfSet(tags, "rhoai.artifactRecord.experimentRunId", artifact.ExperimentRunId)
	if artifact.State != nil {
		tags["rhoai.artifactRecord.state"] = string(*artifact.State)
	}

	if err := encodeCustomProperties(tags, artifact.CustomProperties, artifactTagPrefix); err != nil {
		return nil, err
	}

	if !primary {
		return qualifyAdditionalArtifactTags(tags), nil
	}
	return tags, nil
}

// qualifyAdditionalArtifactTags moves every key of a non-primary
// artifact projection under the "rhoai.artifacts." prefix so additional
// artifacts never collide with the primary projection. The rule is
// injective: the entire suffix after "rhoai." is preserved, so mirrored
// fields ("rhoai.artifacts.artifactRecord.*"), custom properties
// ("rhoai.artifacts.artifact.*"), and direct field keys keep distinct
// names even when they share a suffix. Keys outside the adapter-owned
// namespace (none are produced by EncodeArtifact) qualify verbatim,
// which cannot collide with any owned sub-namespace because every owned
// key retains its "rhoai." suffix content.
func qualifyAdditionalArtifactTags(tags map[string]string) map[string]string {
	qualified := make(map[string]string, len(tags))
	for key, value := range tags {
		qualified[additionalTagPrefix+strings.TrimPrefix(key, reservedTagPrefix)] = value
	}
	return qualified
}

// legacyQualifyAdditionalArtifactTags reproduces the pre-v4 projection,
// which flattened the artifactRecord.*, artifact.*, and direct field
// sub-namespaces into bare suffixes. It is used only to diff and retire
// keys written by stored codec-2/3 snapshots; new writes never produce
// these keys.
func legacyQualifyAdditionalArtifactTags(tags map[string]string) map[string]string {
	qualified := make(map[string]string, len(tags))
	for key, value := range tags {
		suffix := key
		switch {
		case len(key) >= len(artifactRecordPrefix) && key[:len(artifactRecordPrefix)] == artifactRecordPrefix:
			suffix = key[len(artifactRecordPrefix):]
		case len(key) >= len(artifactTagPrefix) && key[:len(artifactTagPrefix)] == artifactTagPrefix:
			suffix = key[len(artifactTagPrefix):]
		case strings.HasPrefix(key, reservedTagPrefix):
			suffix = key[len(reservedTagPrefix):]
		}
		qualified[additionalTagPrefix+suffix] = value
	}
	return qualified
}

// validateNativeName rejects names MLflow cannot represent before any
// native write: the verified 3.18.0 server refuses registered-model
// names containing "/" or ":".
func validateNativeName(name string) error {
	if err := validateLogicalVersionName(name); err != nil {
		return err
	}
	for _, r := range name {
		if r == '/' || r == ':' {
			return fmt.Errorf("name %q contains %q, which the native registry rejects: %w",
				name, string(r), api.ErrBadRequest)
		}
	}
	return nil
}

// Logical version names travel in a tag and a normalized alias, so native
// registered-model punctuation restrictions do not apply to them.
func validateLogicalVersionName(name string) error {
	if name == "" {
		return fmt.Errorf("name must not be empty: %w", api.ErrBadRequest)
	}
	return nil
}

// encodeCustomProperties projects custom properties into tags. Valid
// unreserved keys are used verbatim; reserved or invalid keys are
// escaped under "rhoai.custom.<sha256(key)>". prefix selects the
// namespace for artifact-owned properties ( "" for model and version
// properties, "rhoai.artifact." for the primary artifact ).
func encodeCustomProperties(tags map[string]string, properties map[string]openapi.MetadataValue, namespace string) error {
	for key, value := range properties {
		valueStr, err := encodeMetadataValue(value)
		if err != nil {
			return fmt.Errorf("custom property %q: %w", key, err)
		}
		tags[namespace+escapeTagKey(key)] = valueStr
	}
	return nil
}

// escapeTagKey maps a user property key to its tag key. Keys are valid
// when non-empty, at most 250 characters ( the verified server's limit ),
// and outside the adapter-owned "rhoai." namespace; everything else —
// including keys that collide with the escape namespace itself — is
// escaped under "rhoai.custom.<sha256(key)>".
// EscapeTagKey exposes the tag-key escaping so SQL snapshots can retain
// the original-key mapping alongside the escaped projection.
func EscapeTagKey(key string) string {
	return escapeTagKey(key)
}

func escapeTagKey(key string) string {
	if isValidTagKey(key) {
		return key
	}
	digest := sha256.Sum256([]byte(key))
	return escapedTagPrefix + hex.EncodeToString(digest[:])
}

func isValidTagKey(key string) bool {
	if key == "" || utf8.RuneCountInString(key) > maxTagKeyRunes {
		return false
	}
	return !strings.HasPrefix(key, reservedTagPrefix)
}

// encodeMetadataValue renders one typed property value as its tag
// string: bools as "true"/"false", integers as their canonical decimal
// string, doubles with lossless shortest round-trip formatting, strings
// unchanged, and proto/struct values as their existing base64 payload
// strings ( the proto type URL and every metadata discriminator are
// preserved in the SQL snapshot ). Nonfinite doubles and unset
// discriminators are rejected before any native write; values beyond
// the adapter-owned bound are rejected, never truncated.
func encodeMetadataValue(value openapi.MetadataValue) (string, error) {
	var rendered string
	switch {
	case value.MetadataBoolValue != nil:
		rendered = strconv.FormatBool(value.MetadataBoolValue.BoolValue)
	case value.MetadataIntValue != nil:
		rendered = value.MetadataIntValue.IntValue
		if _, err := strconv.ParseInt(rendered, 10, 64); err != nil {
			return "", fmt.Errorf("int value %q is not a canonical decimal: %w", rendered, api.ErrBadRequest)
		}
	case value.MetadataDoubleValue != nil:
		d := value.MetadataDoubleValue.DoubleValue
		if math.IsNaN(d) || math.IsInf(d, 0) {
			return "", fmt.Errorf("double value %v is not finite: %w", d, api.ErrBadRequest)
		}
		rendered = strconv.FormatFloat(d, 'g', -1, 64)
	case value.MetadataStringValue != nil:
		rendered = value.MetadataStringValue.StringValue
	case value.MetadataProtoValue != nil:
		rendered = value.MetadataProtoValue.ProtoValue
	case value.MetadataStructValue != nil:
		rendered = value.MetadataStructValue.StructValue
	default:
		return "", fmt.Errorf("metadata value has no discriminator set: %w", api.ErrBadRequest)
	}

	if utf8.RuneCountInString(rendered) > maxTagValueLength {
		return "", fmt.Errorf("value exceeds the adapter's %d-character tag limit: %w",
			maxTagValueLength, api.ErrBadRequest)
	}
	return rendered, nil
}

func setIfSet(tags map[string]string, key string, value *string) {
	if value != nil {
		tags[key] = *value
	}
}
