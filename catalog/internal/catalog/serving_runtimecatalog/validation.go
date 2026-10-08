package serving_runtimecatalog

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/distribution/reference"
	openapi "github.com/kubeflow/hub/catalog/pkg/openapi"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	maxRuntimeCatalogBytes = 4 << 20
	maxReadmeBytes         = 64 << 10
	maxLogoBytes           = 256 << 10
	maxURLBytes            = 2 << 10
	maxTemplateBytes       = 256 << 10
	maxStringBytes         = 4 << 10
	maxRuntimeFamilies     = 256
	maxRuntimeVersions     = 64
	maxRuntimeListItems    = 128
	maxValidationIssues    = 100
	maxValidationTextBytes = 16 << 10
)

// rhoaiReleasePattern matches the minimumRHOAIVersion pattern in the OpenAPI spec.
var rhoaiReleasePattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

type runtimeValidationIssue struct {
	runtime string
	version string
	field   string
	reason  string
}

type runtimeValidationErrors struct {
	issues []runtimeValidationIssue
	total  int
}

func (e *runtimeValidationErrors) add(runtime, version, field, reason string) {
	e.total++
	if len(e.issues) < maxValidationIssues {
		e.issues = append(e.issues, runtimeValidationIssue{
			runtime: diagnosticText(runtime, 256),
			version: diagnosticText(version, 256),
			field:   diagnosticText(field, 512),
			reason:  reason,
		})
	}
}

func diagnosticText(value string, limit int) string {
	value = strings.NewReplacer("\n", `\n`, "\r", `\r`).Replace(value)
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "..."
}

func runtimeSourceStatusError(err error) string {
	message := err.Error()
	if len(message) <= maxValidationTextBytes {
		return message
	}
	const suffix = "... (truncated)"
	end := maxValidationTextBytes - len(suffix)
	for end > 0 && !utf8.RuneStart(message[end]) {
		end--
	}
	return message[:end] + suffix
}

func (e *runtimeValidationErrors) err() error {
	if e.total == 0 {
		return nil
	}
	return e
}

func (e *runtimeValidationErrors) appendIssues(other *runtimeValidationErrors) {
	e.total += other.total
	for _, issue := range other.issues {
		if len(e.issues) == maxValidationIssues {
			break
		}
		e.issues = append(e.issues, issue)
	}
}

func (e *runtimeValidationErrors) Error() string {
	var b strings.Builder
	b.WriteString("invalid serving_runtime data")
	shown := 0
	for _, issue := range e.issues {
		if shown == maxValidationIssues {
			break
		}
		line := fmt.Sprintf("; field=%s", issue.field)
		if issue.runtime != "" {
			line += fmt.Sprintf(" runtime=%q", issue.runtime)
		}
		if issue.version != "" {
			line += fmt.Sprintf(" version=%q", issue.version)
		}
		line += ": " + issue.reason
		if b.Len()+len(line)+64 > maxValidationTextBytes {
			break
		}
		b.WriteString(line)
		shown++
	}
	if shown < e.total {
		fmt.Fprintf(&b, "; %d more error(s) omitted", e.total-shown)
	}
	return b.String()
}

// validateServingRuntimeFamily validates the complete family, including all of
// its versions, without making a database write. Callers can reject this family
// while continuing to process unrelated families.
func validateServingRuntimeFamily(runtime yamlServingRuntime, index int, duplicate bool) *runtimeValidationErrors {
	issues := &runtimeValidationErrors{}
	path := fmt.Sprintf("serving_runtimes[%d]", index)
	validateIdentity(issues, runtime.Name, "", path+".name", runtime.Name)
	if duplicate && runtime.Name != "" {
		issues.add(runtime.Name, "", path+".name", "duplicate runtime name")
	}
	validateRuntime(issues, runtime, path)
	return issues
}

func validateRuntime(issues *runtimeValidationErrors, runtime yamlServingRuntime, path string) {
	name := runtime.Name
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"displayName", runtime.DisplayName}, {"provider", runtime.Provider},
		{"description", runtime.Description}, {"externalId", runtime.ExternalID},
	} {
		validateOptionalString(issues, name, "", path+"."+field.name, field.value, maxStringBytes)
	}
	validateOptionalString(issues, name, "", path+".readme", runtime.Readme, maxReadmeBytes)
	validateOptionalURL(issues, name, "", path+".licenseLink", runtime.LicenseLink)
	validateOptionalURL(issues, name, "", path+".documentationUrl", runtime.DocumentationURL)
	validateOptionalURL(issues, name, "", path+".repositoryUrl", runtime.RepositoryURL)
	validateTimestamp(issues, name, "", path+".publishedDate", runtime.PublishedDate)
	validateTimestamp(issues, name, "", path+".lastUpdated", runtime.LastUpdated)
	if runtime.License != nil {
		validateString(issues, name, "", path+".license", *runtime.License, maxStringBytes)
	}
	if runtime.Logo != nil {
		validateLogo(issues, name, path+".logo", *runtime.Logo)
	}
	validateStringList(issues, name, "", path+".tags", runtime.Tags, true)
	validateModelFormats(issues, name, "", path+".supportedModelFormats", runtime.SupportedModelFormats)
	if runtime.Capabilities != nil {
		validateAccelerators(issues, name, path+".capabilities.supportedAccelerators", runtime.Capabilities.SupportedAccelerators)
	}
	validateCustomProperties(issues, runtime, path)
	if len(runtime.Versions) > maxRuntimeVersions {
		issues.add(name, "", path+".versions", fmt.Sprintf("must have at most %d items", maxRuntimeVersions))
	}
	versions := make(map[string]bool, len(runtime.Versions))
	familyFormats := make(map[string]bool, len(runtime.SupportedModelFormats))
	for _, format := range runtime.SupportedModelFormats {
		familyFormats[format.Name] = true
	}
	for i, version := range runtime.Versions {
		versionPath := fmt.Sprintf("%s.versions[%d]", path, i)
		validateIdentity(issues, name, version.Version, versionPath+".version", version.Version)
		if version.Version != "" && versions[version.Version] {
			issues.add(name, version.Version, versionPath+".version", "duplicate version")
		}
		versions[version.Version] = true
		validateRuntimeVersion(issues, name, version, versionPath)
		if len(familyFormats) > 0 {
			for j, format := range version.SupportedModelFormats {
				if format.Name != "" && !familyFormats[format.Name] {
					issues.add(name, version.Version, fmt.Sprintf("%s.supportedModelFormats[%d].name", versionPath, j), "version format is not listed by the runtime family")
				}
			}
		}
	}
}

func validateRuntimeVersion(issues *runtimeValidationErrors, runtime string, version yamlServingRuntimeVersion, path string) {
	name := version.Version
	if strings.TrimSpace(version.Image) == "" {
		issues.add(runtime, name, path+".image", "required image is missing")
	} else {
		validateString(issues, runtime, name, path+".image", version.Image, maxStringBytes)
		if err := validateImageReference(version.Image); err != nil {
			issues.add(runtime, name, path+".image", err.Error())
		}
	}
	if version.SupportLevel != nil {
		validateString(issues, runtime, name, path+".supportLevel", *version.SupportLevel, maxStringBytes)
		switch *version.SupportLevel {
		case "supported", "techPreview", "developerPreview", "community":
		default:
			issues.add(runtime, name, path+".supportLevel", "unsupported support level")
		}
	}
	if version.MinimumRHOAIVersion != nil && !rhoaiReleasePattern.MatchString(*version.MinimumRHOAIVersion) {
		issues.add(runtime, name, path+".minimumRHOAIVersion", "must be a release version such as 3.6")
	}
	validateModelFormats(issues, runtime, name, path+".supportedModelFormats", version.SupportedModelFormats)
	if len(version.ProtocolVersions) > maxRuntimeListItems {
		issues.add(runtime, name, path+".protocolVersions", fmt.Sprintf("must have at most %d items", maxRuntimeListItems))
	}
	seenProtocol := make(map[string]bool, len(version.ProtocolVersions))
	for i, protocol := range version.ProtocolVersions {
		field := fmt.Sprintf("%s.protocolVersions[%d]", path, i)
		validateString(issues, runtime, name, field, protocol, maxStringBytes)
		if protocol != "v1" && protocol != "v2" && protocol != "grpc-v2" {
			issues.add(runtime, name, field, "unsupported protocol version")
		}
		if seenProtocol[protocol] {
			issues.add(runtime, name, field, "duplicate protocol version")
		}
		seenProtocol[protocol] = true
	}
	validateStringList(issues, runtime, name, path+".defaultArgs", version.DefaultArgs, false)
	validateEnv(issues, runtime, name, path+".env", version.Env)
	validateResources(issues, runtime, name, path+".recommendedResources", version.RecommendedResources)
	validateOptionalString(issues, runtime, name, path+".externalId", version.ExternalID, maxStringBytes)
	validateTimestamp(issues, runtime, name, path+".publishedDate", version.PublishedDate)
	if version.ServingRuntimeTemplate != nil {
		validateString(issues, runtime, name, path+".servingRuntimeTemplate", *version.ServingRuntimeTemplate, maxTemplateBytes)
		validateTemplate(issues, runtime, name, path+".servingRuntimeTemplate", *version.ServingRuntimeTemplate, version.Image, version.Env)
	}
	if version.LlmInferenceServiceConfig != nil {
		validateString(issues, runtime, name, path+".llmInferenceServiceConfig", *version.LlmInferenceServiceConfig, maxTemplateBytes)
		if !json.Valid([]byte(*version.LlmInferenceServiceConfig)) {
			issues.add(runtime, name, path+".llmInferenceServiceConfig", "must be valid JSON")
		}
	}
}

func validateIdentity(issues *runtimeValidationErrors, runtime, version, field, value string) {
	if strings.TrimSpace(value) == "" {
		issues.add(runtime, version, field, "required value is missing")
	}
	if strings.Contains(value, ":") {
		issues.add(runtime, version, field, "must not contain ':'")
	}
	validateString(issues, runtime, version, field, value, maxStringBytes)
}

func validateOptionalString(issues *runtimeValidationErrors, runtime, version, field string, value *string, limit int) {
	if value != nil {
		validateString(issues, runtime, version, field, *value, limit)
	}
}

func validateString(issues *runtimeValidationErrors, runtime, version, field, value string, limit int) {
	if len(value) > limit {
		issues.add(runtime, version, field, fmt.Sprintf("must be at most %d bytes", limit))
	}
}

func validateStringList(issues *runtimeValidationErrors, runtime, version, field string, values []string, unique bool) {
	if len(values) > maxRuntimeListItems {
		issues.add(runtime, version, field, fmt.Sprintf("must have at most %d items", maxRuntimeListItems))
	}
	seen := make(map[string]bool, len(values))
	for i, value := range values {
		item := fmt.Sprintf("%s[%d]", field, i)
		if strings.TrimSpace(value) == "" {
			issues.add(runtime, version, item, "must not be empty")
		}
		validateString(issues, runtime, version, item, value, maxStringBytes)
		if unique && seen[value] {
			issues.add(runtime, version, item, "duplicate item")
		}
		seen[value] = true
	}
}

func validateModelFormats(issues *runtimeValidationErrors, runtime, version, field string, formats []openapi.SupportedModelFormat) {
	if len(formats) > maxRuntimeListItems {
		issues.add(runtime, version, field, fmt.Sprintf("must have at most %d items", maxRuntimeListItems))
	}
	seen := make(map[string]bool, len(formats))
	for i, format := range formats {
		item := fmt.Sprintf("%s[%d]", field, i)
		if strings.TrimSpace(format.Name) == "" {
			issues.add(runtime, version, item+".name", "required format name is missing")
		}
		validateString(issues, runtime, version, item+".name", format.Name, maxStringBytes)
		validateOptionalString(issues, runtime, version, item+".version", format.Version, maxStringBytes)
		key := format.Name
		if format.Version != nil {
			key += "\x00" + *format.Version
		}
		if seen[key] {
			issues.add(runtime, version, item+".name", "duplicate model format and version")
		}
		seen[key] = true
	}
}

func validateAccelerators(issues *runtimeValidationErrors, runtime, field string, accelerators []string) {
	validateStringList(issues, runtime, "", field, accelerators, true)
	for i, name := range accelerators {
		if !strings.Contains(name, "/") || len(validation.IsQualifiedName(name)) > 0 {
			issues.add(runtime, "", fmt.Sprintf("%s[%d]", field, i), "must be a qualified accelerator resource name")
		}
	}
}

func validateEnv(issues *runtimeValidationErrors, runtime, version, field string, env []openapi.ServingRuntimeEnvVar) {
	if len(env) > maxRuntimeListItems {
		issues.add(runtime, version, field, fmt.Sprintf("must have at most %d items", maxRuntimeListItems))
	}
	seen := make(map[string]bool, len(env))
	for i, variable := range env {
		item := fmt.Sprintf("%s[%d]", field, i)
		if variable.Name == "" || len(validation.IsEnvVarName(variable.Name)) > 0 {
			issues.add(runtime, version, item+".name", "invalid environment variable name")
		}
		validateString(issues, runtime, version, item+".name", variable.Name, maxStringBytes)
		if seen[variable.Name] {
			issues.add(runtime, version, item+".name", "duplicate environment variable")
		}
		seen[variable.Name] = true
		validateOptionalString(issues, runtime, version, item+".description", variable.Description, maxStringBytes)
		if variable.DefaultValue != nil {
			validateString(issues, runtime, version, item+".defaultValue", *variable.DefaultValue, maxStringBytes)
			if variable.Secret != nil && *variable.Secret {
				issues.add(runtime, version, item+".defaultValue", "must be absent when secret=true")
			}
			if variable.Required != nil && *variable.Required {
				issues.add(runtime, version, item+".defaultValue", "must be absent when required=true")
			}
		}
	}
}

func validateResources(issues *runtimeValidationErrors, runtime, version, field string, resources *openapi.ServingRuntimeResourceRecommendation) {
	if resources == nil {
		return
	}
	for _, tier := range []struct {
		name  string
		value *openapi.ResourceTier
	}{
		{"minimal", resources.Minimal}, {"recommended", resources.Recommended}, {"high", resources.High},
	} {
		if tier.value == nil {
			continue
		}
		path := field + "." + tier.name
		for _, amount := range []struct {
			name  string
			value *string
		}{
			{"cpu", tier.value.Cpu}, {"memory", tier.value.Memory},
		} {
			if amount.value != nil {
				validateQuantity(issues, runtime, version, path+"."+amount.name, *amount.value)
			}
		}
		if tier.value.Accelerator != nil {
			keys := make([]string, 0, len(*tier.value.Accelerator))
			for key := range *tier.value.Accelerator {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if len(keys) > maxRuntimeListItems {
				issues.add(runtime, version, path+".accelerator", fmt.Sprintf("must have at most %d items", maxRuntimeListItems))
			}
			for _, key := range keys {
				item := fmt.Sprintf("%s.accelerator[%q]", path, key)
				if !strings.Contains(key, "/") || len(validation.IsQualifiedName(key)) > 0 {
					issues.add(runtime, version, item, "must be a qualified accelerator resource name")
				}
				amount := (*tier.value.Accelerator)[key]
				validateQuantity(issues, runtime, version, item, amount)
				if quantity, err := resource.ParseQuantity(amount); err == nil {
					if _, exact := quantity.AsScale(0); !exact {
						issues.add(runtime, version, item, "accelerator quantity must be a whole number")
					}
				}
			}
		}
	}
}

func validateQuantity(issues *runtimeValidationErrors, runtime, version, field, value string) {
	validateString(issues, runtime, version, field, value, maxStringBytes)
	quantity, err := resource.ParseQuantity(value)
	if err != nil || quantity.Sign() <= 0 {
		issues.add(runtime, version, field, "must be a positive Kubernetes resource quantity")
	}
}

func validateCustomProperties(issues *runtimeValidationErrors, runtime yamlServingRuntime, field string) {
	if runtime.CustomProperties == nil {
		return
	}
	keys := make([]string, 0, len(*runtime.CustomProperties))
	for key := range *runtime.CustomProperties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > maxRuntimeListItems {
		issues.add(runtime.Name, "", field+".customProperties", fmt.Sprintf("must have at most %d items", maxRuntimeListItems))
	}
	for _, key := range keys {
		item := fmt.Sprintf("%s.customProperties[%q]", field, key)
		if key == "" || len(key) > 255 {
			issues.add(runtime.Name, "", item, "property name must be 1 to 255 bytes")
		}
		if reservedServingRuntimeProperties[key] {
			issues.add(runtime.Name, "", item, "property name is reserved")
		}
		wrapped := (*runtime.CustomProperties)[key]
		if wrapped.Problem != "" {
			issues.add(runtime.Name, "", item, wrapped.Problem)
			continue
		}
		value := wrapped.Value
		if value.MetadataStringValue != nil {
			validateString(issues, runtime.Name, "", item+".string_value", value.MetadataStringValue.StringValue, maxStringBytes)
		}
		if value.MetadataIntValue != nil {
			validateString(issues, runtime.Name, "", item+".int_value", value.MetadataIntValue.IntValue, maxStringBytes)
			if _, err := strconv.ParseInt(value.MetadataIntValue.IntValue, 10, 32); err != nil {
				issues.add(runtime.Name, "", item+".int_value", "must be a valid int32")
			}
		}
		if value.MetadataProtoValue != nil {
			validateString(issues, runtime.Name, "", item+".type", value.MetadataProtoValue.Type, maxStringBytes)
			validateString(issues, runtime.Name, "", item+".proto_value", value.MetadataProtoValue.ProtoValue, maxStringBytes)
			if value.MetadataProtoValue.Type == "" {
				issues.add(runtime.Name, "", item+".type", "proto type is required")
			}
			if _, err := base64.StdEncoding.DecodeString(value.MetadataProtoValue.ProtoValue); err != nil {
				issues.add(runtime.Name, "", item+".proto_value", "must be base64 encoded")
			}
		}
		if value.MetadataStructValue != nil {
			validateString(issues, runtime.Name, "", item+".struct_value", value.MetadataStructValue.StructValue, maxStringBytes)
			if _, err := base64.StdEncoding.DecodeString(value.MetadataStructValue.StructValue); err != nil {
				issues.add(runtime.Name, "", item+".struct_value", "must be base64 encoded")
			}
		}
	}
}

func validateTimestamp(issues *runtimeValidationErrors, runtime, version, field string, value *string) {
	if value == nil {
		return
	}
	validateString(issues, runtime, version, field, *value, maxStringBytes)
	if _, err := time.Parse(time.RFC3339, *value); err != nil {
		issues.add(runtime, version, field, "must be an RFC3339 timestamp")
	}
}

func validateOptionalURL(issues *runtimeValidationErrors, runtime, version, field string, value *string) {
	if value != nil {
		validateHTTPURL(issues, runtime, version, field, *value, maxURLBytes)
	}
}

func validateHTTPURL(issues *runtimeValidationErrors, runtime, version, field, value string, limit int) {
	validateString(issues, runtime, version, field, value, limit)
	u, err := url.Parse(value)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		issues.add(runtime, version, field, "must be an absolute HTTP(S) URL without credentials")
	}
}

func validateLogo(issues *runtimeValidationErrors, runtime, field, value string) {
	if !strings.HasPrefix(strings.ToLower(value), "data:") {
		validateHTTPURL(issues, runtime, "", field, value, maxURLBytes)
		return
	}
	rest := value[len("data:"):]
	meta, payload, found := strings.Cut(rest, ",")
	if !found {
		issues.add(runtime, "", field, "malformed data URI")
		return
	}
	parts := strings.Split(meta, ";")
	allowedType := map[string]bool{
		"image/png": true, "image/jpeg": true, "image/gif": true,
		"image/webp": true, "image/svg+xml": true, "image/x-icon": true,
		"image/vnd.microsoft.icon": true,
	}
	if !allowedType[strings.ToLower(parts[0])] {
		issues.add(runtime, "", field, "unsupported logo media type")
	}
	isBase64 := false
	for _, part := range parts[1:] {
		if part == "base64" {
			isBase64 = true
		} else if strings.EqualFold(part, "base64") {
			issues.add(runtime, "", field, "unsupported logo data URI encoding")
			return
		}
	}
	var decoded []byte
	var err error
	if isBase64 {
		if base64.StdEncoding.DecodedLen(len(payload)) > maxLogoBytes+2 {
			issues.add(runtime, "", field, fmt.Sprintf("decoded logo must be at most %d bytes", maxLogoBytes))
			return
		}
		decoded, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(payload)
		}
	} else {
		if len(payload) > maxLogoBytes {
			issues.add(runtime, "", field, fmt.Sprintf("decoded logo must be at most %d bytes", maxLogoBytes))
			return
		}
		var unescaped string
		unescaped, err = url.PathUnescape(payload)
		decoded = []byte(unescaped)
	}
	if err != nil {
		issues.add(runtime, "", field, "invalid logo data URI encoding")
	} else if len(decoded) > maxLogoBytes {
		issues.add(runtime, "", field, fmt.Sprintf("decoded logo must be at most %d bytes", maxLogoBytes))
	}
}

func validateImageReference(value string) error {
	first, _, found := strings.Cut(value, "/")
	if !found || (!strings.Contains(first, ".") && !strings.Contains(first, ":") && first != "localhost") {
		return fmt.Errorf("must include a registry host")
	}
	named, err := reference.ParseNormalizedNamed(value)
	if err != nil {
		return fmt.Errorf("invalid container image reference")
	}
	if _, tagged := named.(reference.Tagged); !tagged {
		if _, digested := named.(reference.Digested); !digested {
			return fmt.Errorf("image must use a fixed tag or digest")
		}
	}
	if tagged, ok := named.(reference.Tagged); ok && strings.EqualFold(tagged.Tag(), "latest") {
		return fmt.Errorf("image tag must not be latest")
	}
	return nil
}

// unwrapServingRuntimeTemplate returns the ServingRuntime to validate and its
// field path. A bare ServingRuntime is returned as is; an OpenShift Template
// must contain exactly one ServingRuntime in its objects.
func unwrapServingRuntimeTemplate(issues *runtimeValidationErrors, runtime, version, field, value string) (json.RawMessage, string, bool) {
	var wrapper struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Objects    []json.RawMessage `json:"objects"`
	}
	if err := json.Unmarshal([]byte(value), &wrapper); err != nil {
		issues.add(runtime, version, field, "must be a JSON-encoded ServingRuntime or Template object")
		return nil, field, false
	}
	if wrapper.Kind != "Template" {
		return json.RawMessage(value), field, true
	}
	if wrapper.APIVersion != "template.openshift.io/v1" {
		issues.add(runtime, version, field+".apiVersion", "unsupported OpenShift Template API version")
	}
	var found json.RawMessage
	foundField := field
	count := 0
	for i, object := range wrapper.Objects {
		var header struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(object, &header); err != nil {
			issues.add(runtime, version, fmt.Sprintf("%s.objects[%d]", field, i), "must be a JSON object")
			continue
		}
		if header.Kind == "ServingRuntime" {
			found, foundField = object, fmt.Sprintf("%s.objects[%d]", field, i)
			count++
		}
	}
	if count != 1 {
		issues.add(runtime, version, field+".objects", "must contain exactly one ServingRuntime")
		return nil, field, false
	}
	return found, foundField, true
}

func validateTemplate(issues *runtimeValidationErrors, runtime, version, field, value, image string, declaredEnv []openapi.ServingRuntimeEnvVar) {
	if len(value) > maxTemplateBytes {
		return
	}
	var document struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Spec       struct {
			Containers []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
				Env   []struct {
					Name  string  `json:"name"`
					Value *string `json:"value"`
				} `json:"env"`
			} `json:"containers"`
		} `json:"spec"`
	}
	servingRuntime, field, ok := unwrapServingRuntimeTemplate(issues, runtime, version, field, value)
	if !ok {
		return
	}
	if err := json.Unmarshal(servingRuntime, &document); err != nil {
		issues.add(runtime, version, field, "must be a JSON-encoded ServingRuntime object")
		return
	}
	if document.APIVersion != "serving.kserve.io/v1alpha1" {
		issues.add(runtime, version, field+".apiVersion", "unsupported KServe API version")
	}
	if document.Kind != "ServingRuntime" {
		issues.add(runtime, version, field+".kind", "must be ServingRuntime")
	}
	if len(document.Spec.Containers) == 0 {
		issues.add(runtime, version, field+".spec.containers", "at least one container is required")
	}
	if len(document.Spec.Containers) > maxRuntimeListItems {
		issues.add(runtime, version, field+".spec.containers", fmt.Sprintf("must have at most %d items", maxRuntimeListItems))
	}
	secretNames := make(map[string]bool, len(declaredEnv))
	for _, variable := range declaredEnv {
		if variable.Secret != nil && *variable.Secret {
			secretNames[variable.Name] = true
		}
	}
	matched := false
	seenContainers := make(map[string]bool, len(document.Spec.Containers))
	for i, container := range document.Spec.Containers {
		containerPath := fmt.Sprintf("%s.spec.containers[%d]", field, i)
		validateString(issues, runtime, version, containerPath+".name", container.Name, maxStringBytes)
		validateString(issues, runtime, version, containerPath+".image", container.Image, maxStringBytes)
		if container.Name == "" {
			issues.add(runtime, version, containerPath+".name", "container name is required")
		}
		if container.Name != "" && seenContainers[container.Name] {
			issues.add(runtime, version, containerPath+".name", "duplicate container name")
		}
		seenContainers[container.Name] = true
		if container.Image == image {
			matched = true
		}
		if err := validateImageReference(container.Image); err != nil {
			issues.add(runtime, version, containerPath+".image", err.Error())
		}
		if len(container.Env) > maxRuntimeListItems {
			issues.add(runtime, version, containerPath+".env", fmt.Sprintf("must have at most %d items", maxRuntimeListItems))
		}
		seenEnv := make(map[string]bool, len(container.Env))
		for j, variable := range container.Env {
			envPath := fmt.Sprintf("%s.env[%d]", containerPath, j)
			validateString(issues, runtime, version, envPath+".name", variable.Name, maxStringBytes)
			if variable.Name == "" || len(validation.IsEnvVarName(variable.Name)) > 0 {
				issues.add(runtime, version, envPath+".name", "invalid environment variable name")
			}
			if seenEnv[variable.Name] {
				issues.add(runtime, version, envPath+".name", "duplicate environment variable")
			}
			seenEnv[variable.Name] = true
			validateOptionalString(issues, runtime, version, envPath+".value", variable.Value, maxStringBytes)
			if variable.Value != nil && (secretNames[variable.Name] || looksLikeSecretName(variable.Name)) {
				issues.add(runtime, version, envPath+".value", "literal value is not allowed for a secret environment variable")
			}
		}
	}
	if len(document.Spec.Containers) > 0 && !matched {
		issues.add(runtime, version, field+".spec.containers", "at least one container image must match version.image")
	}
}

func looksLikeSecretName(name string) bool {
	lower := strings.ToLower(name)
	for _, part := range strings.FieldsFunc(lower, func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
		switch part {
		case "token", "secret", "password", "passwd", "apikey", "credential", "credentials":
			return true
		}
	}
	return strings.Contains(lower, "api_key")
}
