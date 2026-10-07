package serving_runtimecatalog

import (
	"bytes"
	"fmt"
	"io"

	yamlv3 "gopkg.in/yaml.v3"
)

// yamlShape captures only the on-disk field layout. Strict typed decoding and
// validateServingRuntimeFamily still decide types and field semantics. This
// pass gives unknown and misplaced keys a runtime/version-aware field path.
type yamlShape struct {
	kind           yamlv3.Kind
	fields         map[string]*yamlShape
	items          *yamlShape
	additionalKeys *yamlShape
	identity       string
}

func runtimeYAMLShape() *yamlShape {
	stringValue := &yamlShape{kind: yamlv3.ScalarNode}
	stringList := &yamlShape{kind: yamlv3.SequenceNode, items: stringValue}
	format := &yamlShape{kind: yamlv3.MappingNode, fields: map[string]*yamlShape{
		"name": stringValue, "version": stringValue, "autoSelect": stringValue, "priority": stringValue,
	}}
	formats := &yamlShape{kind: yamlv3.SequenceNode, items: format}
	resourceTier := &yamlShape{kind: yamlv3.MappingNode, fields: map[string]*yamlShape{
		"cpu": stringValue, "memory": stringValue,
		"accelerator": {kind: yamlv3.MappingNode, additionalKeys: stringValue},
	}}
	resources := &yamlShape{kind: yamlv3.MappingNode, fields: map[string]*yamlShape{
		"minimal": resourceTier, "recommended": resourceTier, "high": resourceTier,
	}}
	env := &yamlShape{kind: yamlv3.MappingNode, fields: map[string]*yamlShape{
		"name": stringValue, "description": stringValue, "required": stringValue,
		"defaultValue": stringValue, "secret": stringValue,
	}}
	metadata := &yamlShape{kind: yamlv3.MappingNode, fields: map[string]*yamlShape{
		"metadataType": stringValue, "string_value": stringValue, "bool_value": stringValue,
		"int_value": stringValue, "double_value": stringValue, "type": stringValue,
		"proto_value": stringValue, "struct_value": stringValue,
	}}
	version := &yamlShape{kind: yamlv3.MappingNode, identity: "version", fields: map[string]*yamlShape{
		"version": stringValue, "image": stringValue, "supportLevel": stringValue,
		"supportedModelFormats": formats, "protocolVersions": stringList,
		"recommendedResources": resources, "defaultArgs": stringList,
		"env":                    {kind: yamlv3.SequenceNode, items: env},
		"servingRuntimeTemplate": stringValue, "llmInferenceServiceConfig": stringValue,
		"deprecated": stringValue, "publishedDate": stringValue, "externalId": stringValue,
	}}
	runtime := &yamlShape{kind: yamlv3.MappingNode, identity: "name", fields: map[string]*yamlShape{
		"name": stringValue, "displayName": stringValue, "provider": stringValue,
		"description": stringValue, "readme": stringValue, "logo": stringValue,
		"tags": stringList, "license": stringValue, "licenseLink": stringValue,
		"documentationUrl": stringValue, "repositoryUrl": stringValue,
		"supportedModelFormats": formats,
		"capabilities": {kind: yamlv3.MappingNode, fields: map[string]*yamlShape{
			"requiresGPU": stringValue, "supportedAccelerators": stringList, "multiModel": stringValue,
		}},
		"publishedDate": stringValue, "lastUpdated": stringValue, "externalId": stringValue,
		"customProperties": {kind: yamlv3.MappingNode, additionalKeys: metadata},
		"versions":         {kind: yamlv3.SequenceNode, items: version},
	}}
	return &yamlShape{kind: yamlv3.MappingNode, fields: map[string]*yamlShape{
		"source": stringValue, "serving_runtimes": {kind: yamlv3.SequenceNode, items: runtime},
	}}
}

// runtimeYAMLFamilyNodes checks document-level structure before any family is
// decoded. Family nodes are returned separately so one malformed family does
// not hide the other families' validation results.
func runtimeYAMLFamilyNodes(data []byte) ([]*yamlv3.Node, error) {
	decoder := yamlv3.NewDecoder(bytes.NewReader(data))
	var document yamlv3.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	var second yamlv3.Node
	if err := decoder.Decode(&second); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("failed to parse YAML: %w", err)
		}
		return nil, fmt.Errorf("runtime-data must contain one YAML document")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yamlv3.MappingNode {
		return nil, fmt.Errorf("runtime-data must be a YAML object")
	}
	root := document.Content[0]
	issues := &runtimeValidationErrors{}
	seen := make(map[string]bool, len(root.Content)/2)
	var families []*yamlv3.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Kind != yamlv3.ScalarNode || key.Tag != "!!str" {
			issues.add("", "", "", "mapping keys must be strings")
			continue
		}
		if seen[key.Value] {
			issues.add("", "", key.Value, "duplicate YAML key")
		}
		seen[key.Value] = true
		switch key.Value {
		case "source":
			inspectRuntimeYAMLNode(value, runtimeYAMLShape().fields["source"], "source", "", "", issues)
			if value.Kind == yamlv3.ScalarNode && value.Tag != "!!null" {
				validateString(issues, "", "", "source", value.Value, maxStringBytes)
			}
		case "serving_runtimes":
			if value.Kind != yamlv3.SequenceNode {
				issues.add("", "", "serving_runtimes", "expected a list")
				continue
			}
			families = value.Content
		default:
			issues.add("", "", key.Value, "unknown or misplaced field")
		}
	}
	if !seen["serving_runtimes"] {
		issues.add("", "", "serving_runtimes", "required list is missing")
	}
	if len(families) > maxRuntimeFamilies {
		issues.add("", "", "serving_runtimes", fmt.Sprintf("must have at most %d items", maxRuntimeFamilies))
	}
	if err := issues.err(); err != nil {
		return nil, err
	}
	return families, nil
}

func runtimeYAMLFamilyName(node *yamlv3.Node) string {
	if node.Kind != yamlv3.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "name" && node.Content[i+1].Kind == yamlv3.ScalarNode {
			return node.Content[i+1].Value
		}
	}
	return ""
}

func inspectRuntimeYAMLNode(node *yamlv3.Node, shape *yamlShape, path, runtime, version string, issues *runtimeValidationErrors) {
	if node.Kind == yamlv3.AliasNode {
		issues.add(runtime, version, path, "YAML aliases are not supported")
		return
	}
	if node.Tag == "!!null" {
		return // Required values are checked by typed semantic validation.
	}
	if node.Kind != shape.kind {
		issues.add(runtime, version, path, "unexpected YAML value type")
		return
	}
	switch shape.kind {
	case yamlv3.SequenceNode:
		for i, child := range node.Content {
			inspectRuntimeYAMLNode(child, shape.items, fmt.Sprintf("%s[%d]", path, i), runtime, version, issues)
		}
	case yamlv3.MappingNode:
		if shape.identity != "" {
			for i := 0; i+1 < len(node.Content); i += 2 {
				if node.Content[i].Value == shape.identity && node.Content[i+1].Kind == yamlv3.ScalarNode {
					if shape.identity == "name" {
						runtime = node.Content[i+1].Value
					} else {
						version = node.Content[i+1].Value
					}
					break
				}
			}
		}
		seen := make(map[string]bool, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind != yamlv3.ScalarNode || key.Tag != "!!str" {
				issues.add(runtime, version, path, "mapping keys must be strings")
				continue
			}
			childPath := path + "." + key.Value
			if path == "" {
				childPath = key.Value
			}
			if shape.additionalKeys != nil {
				childPath = fmt.Sprintf("%s[%q]", path, key.Value)
			}
			if seen[key.Value] {
				issues.add(runtime, version, childPath, "duplicate YAML key")
			}
			seen[key.Value] = true
			childShape := shape.additionalKeys
			if childShape == nil {
				childShape = shape.fields[key.Value]
			}
			if childShape == nil {
				issues.add(runtime, version, childPath, "unknown or misplaced field")
				continue
			}
			inspectRuntimeYAMLNode(value, childShape, childPath, runtime, version, issues)
		}
	}
}
