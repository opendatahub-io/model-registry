package mlflow

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	platformfilter "github.com/kubeflow/hub/internal/platform/db/filter"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
)

// Filterable property families. Built-in names follow the platform's
// REST entity property maps for the three supported kinds; names that
// belong to the serving or experiment families name relations the
// MLflow backend cannot evaluate, and relation references are rejected
// as unsupported rather than silently matching nothing.
var supportedBuiltins = map[ObjectKind]map[string]bool{
	KindRegisteredModel: {
		"id": true, "name": true, "externalId": true,
		"createTimeSinceEpoch": true, "lastUpdateTimeSinceEpoch": true,
		"state": true, "owner": true,
	},
	KindModelVersion: {
		"id": true, "name": true, "externalId": true,
		"createTimeSinceEpoch": true, "lastUpdateTimeSinceEpoch": true,
		"registeredModelId": true, "state": true, "author": true,
	},
	KindModelArtifact: {
		"id": true, "name": true, "externalId": true,
		"createTimeSinceEpoch": true, "lastUpdateTimeSinceEpoch": true,
		"uri": true, "state": true,
		"modelFormatName": true, "modelFormatVersion": true,
		"storageKey": true, "storagePath": true, "serviceAccountName": true,
		"modelSourceKind": true, "modelSourceClass": true, "modelSourceGroup": true,
		"modelSourceId": true, "modelSourceName": true,
	},
}

// unsupportedRelations names properties of the serving and experiment
// families: filters referencing them ask for a relation this backend
// has no representation of.
var unsupportedRelations = map[string]bool{
	"servingEnvironmentId": true, "runtime": true, "desiredState": true,
	"experimentId": true, "experimentRunId": true, "status": true,
	"startTimeSinceEpoch": true, "endTimeSinceEpoch": true,
}

// scalarKind retains the original metadata discriminator.
type scalarKind uint8

const (
	scalarString scalarKind = iota
	scalarInt
	scalarDouble
	scalarBool
)

// filterValue is one comparable property value with its type tag.
type filterValue struct {
	kind    scalarKind
	str     string
	integer int64
	num     float64
	bool    bool
}

func stringFilterValue(value string) filterValue  { return filterValue{kind: scalarString, str: value} }
func integerFilterValue(value int64) filterValue  { return filterValue{kind: scalarInt, integer: value} }
func numberFilterValue(value float64) filterValue { return filterValue{kind: scalarDouble, num: value} }
func boolFilterValue(value bool) filterValue      { return filterValue{kind: scalarBool, bool: value} }

// metadataFilterValue renders one typed custom property.
func metadataFilterValue(value openapi.MetadataValue) (filterValue, bool) {
	switch {
	case value.MetadataStringValue != nil:
		return stringFilterValue(value.MetadataStringValue.StringValue), true
	case value.MetadataIntValue != nil:
		parsed, err := strconv.ParseInt(value.MetadataIntValue.IntValue, 10, 64)
		if err != nil {
			return filterValue{}, false
		}
		return integerFilterValue(parsed), true
	case value.MetadataDoubleValue != nil:
		return numberFilterValue(value.MetadataDoubleValue.DoubleValue), true
	case value.MetadataBoolValue != nil:
		return boolFilterValue(value.MetadataBoolValue.BoolValue), true
	default:
		return filterValue{}, false
	}
}

// EvaluateFilter reports whether one committed object matches a parsed
// filter expression. The expression tree carries AND/OR precedence and
// grouping from the parser; leaves compare typed property values with
// SQL-like semantics. Names in the serving and experiment families are
// unsupported relations; comparisons against absent properties never
// match. The object list is the query's full candidate set, available
// for scope-sensitive evaluation.
func EvaluateFilter(expr *platformfilter.FilterExpression, object Object,
	all []Object) (bool, error) {
	if expr == nil {
		return true, nil
	}
	if expr.IsLeaf {
		return evaluateLeaf(expr, object)
	}
	left, err := EvaluateFilter(expr.Left, object, all)
	if err != nil {
		return false, err
	}
	switch expr.Operator {
	case "AND":
		if !left {
			return false, nil
		}
		return EvaluateFilter(expr.Right, object, all)
	case "OR":
		if left {
			return true, nil
		}
		return EvaluateFilter(expr.Right, object, all)
	default:
		return false, fmt.Errorf("filter operator %q is not supported: %w",
			expr.Operator, api.ErrBadRequest)
	}
}

// evaluateLeaf compares one property against one filter value.
func evaluateLeaf(expr *platformfilter.FilterExpression, object Object) (bool, error) {
	name := expr.Property
	explicitType := ""
	for _, suffix := range []string{
		platformfilter.StringValueType, platformfilter.DoubleValueType,
		platformfilter.IntValueType, platformfilter.BoolValueType,
	} {
		if strings.HasSuffix(name, "."+suffix) {
			explicitType = suffix
			name = strings.TrimSuffix(name, "."+suffix)
			break
		}
	}
	if unsupportedRelations[name] {
		return false, fmt.Errorf("filtering by %q requires a relation the mlflow backend does not support: %w",
			name, api.ErrNotImplemented)
	}

	value, exists, err := objectPropertyValue(object, name)
	if err != nil || !exists {
		// A absent property matches nothing — including inequality, as
		// in the SQL property-join semantics this mirrors.
		return false, err
	}
	if explicitType != "" && valueTypeName(value) != explicitType {
		return false, nil
	}

	// SQL permits quoted numeric identifiers as well as numeric literals.
	// Limit this coercion to identifier built-ins; metadata strings stay typed.
	literal := expr.Value
	if name == "id" || name == "registeredModelId" {
		literal = identifierLiteral(literal)
	}
	switch expr.Operator {
	case "=", "!=", "<>":
		equal, comparable := filterEquals(value, literal)
		if !comparable {
			return false, nil
		}
		if expr.Operator == "=" {
			return equal, nil
		}
		return !equal, nil
	case ">", ">=", "<", "<=":
		return filterCompare(value, literal, expr.Operator)
	case "LIKE", "ILIKE":
		pattern, ok := expr.Value.(string)
		if !ok || value.kind != scalarString {
			return false, nil
		}
		return matchLike(pattern, value.str, expr.Operator == "ILIKE"), nil
	case "IN":
		return filterIn(value, literal)
	default:
		return false, fmt.Errorf("filter operator %q is not supported: %w",
			expr.Operator, api.ErrBadRequest)
	}
}

func identifierLiteral(value any) any {
	switch typed := value.(type) {
	case string:
		if parsed, err := strconv.ParseInt(typed, 10, 64); err == nil {
			return parsed
		}
	case []any:
		values := make([]any, len(typed))
		for i, item := range typed {
			values[i] = identifierLiteral(item)
		}
		return values
	}
	return value
}

// objectPropertyValue extracts one built-in or custom property.
func objectPropertyValue(object Object, name string) (filterValue, bool, error) {
	if supportedBuiltins[object.Kind][name] {
		value, ok := builtinPropertyValue(object, name)
		return value, ok, nil
	}
	// Everything else is a custom property on the stored resource.
	properties, err := objectCustomProperties(object)
	if err != nil {
		return filterValue{}, false, err
	}
	metadata, ok := properties[name]
	if !ok {
		return filterValue{}, false, nil
	}
	value, ok := metadataFilterValue(metadata)
	return value, ok, nil
}

// builtinPropertyValue extracts one built-in field.
func builtinPropertyValue(object Object, name string) (filterValue, bool) {
	switch name {
	case "id":
		return integerFilterValue(int64(object.ID)), true
	case "name":
		return stringFilterValue(object.Name), true
	case "externalId":
		if object.ExternalID == nil {
			return filterValue{}, false
		}
		return stringFilterValue(*object.ExternalID), true
	case "createTimeSinceEpoch":
		return integerFilterValue(object.CreateTimeSinceEpoch), true
	case "lastUpdateTimeSinceEpoch":
		return integerFilterValue(object.LastUpdateTimeSinceEpoch), true
	case "registeredModelId":
		// The parent is authoritative; the stored resource may not
		// carry the id the server assigned.
		if object.ParentID == 0 {
			return filterValue{}, false
		}
		return integerFilterValue(int64(object.ParentID)), true
	}
	properties, err := objectResource(object)
	if err != nil {
		return filterValue{}, false
	}
	switch object.Kind {
	case KindRegisteredModel:
		model := properties.(*openapi.RegisteredModel)
		switch name {
		case "state":
			if model.State == nil {
				return filterValue{}, false
			}
			return stringFilterValue(string(*model.State)), true
		case "owner":
			if model.Owner == nil {
				return filterValue{}, false
			}
			return stringFilterValue(*model.Owner), true
		}
	case KindModelVersion:
		version := properties.(*openapi.ModelVersion)
		switch name {
		case "state":
			if version.State == nil {
				return filterValue{}, false
			}
			return stringFilterValue(string(*version.State)), true
		case "author":
			if version.Author == nil {
				return filterValue{}, false
			}
			return stringFilterValue(*version.Author), true
		}
	case KindModelArtifact:
		artifact := properties.(*openapi.ModelArtifact)
		switch name {
		case "uri":
			if artifact.Uri == nil {
				return filterValue{}, false
			}
			return stringFilterValue(*artifact.Uri), true
		case "state":
			if artifact.State == nil {
				return filterValue{}, false
			}
			return stringFilterValue(string(*artifact.State)), true
		case "modelFormatName":
			return optionalString(artifact.ModelFormatName)
		case "modelFormatVersion":
			return optionalString(artifact.ModelFormatVersion)
		case "storageKey":
			return optionalString(artifact.StorageKey)
		case "storagePath":
			return optionalString(artifact.StoragePath)
		case "serviceAccountName":
			return optionalString(artifact.ServiceAccountName)
		case "modelSourceKind":
			return optionalString(artifact.ModelSourceKind)
		case "modelSourceClass":
			return optionalString(artifact.ModelSourceClass)
		case "modelSourceGroup":
			return optionalString(artifact.ModelSourceGroup)
		case "modelSourceId":
			return optionalString(artifact.ModelSourceId)
		case "modelSourceName":
			return optionalString(artifact.ModelSourceName)
		}
	}
	return filterValue{}, false
}

func optionalString(value *string) (filterValue, bool) {
	if value == nil {
		return filterValue{}, false
	}
	return stringFilterValue(*value), true
}

// objectResource decodes the stored resource by kind.
func objectResource(object Object) (any, error) {
	snapshot, err := decodeStoredSnapshot(object.CommittedJSON)
	if err != nil {
		return nil, err
	}
	switch object.Kind {
	case KindRegisteredModel:
		var model openapi.RegisteredModel
		if err := json.Unmarshal(snapshot.Resource, &model); err != nil {
			return nil, fmt.Errorf("decoding model %d snapshot: %w", object.ID, api.ErrBadRequest)
		}
		return &model, nil
	case KindModelVersion:
		var version openapi.ModelVersion
		if err := json.Unmarshal(snapshot.Resource, &version); err != nil {
			return nil, fmt.Errorf("decoding version %d snapshot: %w", object.ID, api.ErrBadRequest)
		}
		return &version, nil
	case KindModelArtifact:
		var artifact openapi.ModelArtifact
		if err := json.Unmarshal(snapshot.Resource, &artifact); err != nil {
			return nil, fmt.Errorf("decoding artifact %d snapshot: %w", object.ID, api.ErrBadRequest)
		}
		return &artifact, nil
	default:
		return nil, fmt.Errorf("object %d has unsupported kind %q: %w",
			object.ID, object.Kind, api.ErrBadRequest)
	}
}

// objectCustomProperties decodes the resource's custom properties.
func objectCustomProperties(object Object) (map[string]openapi.MetadataValue, error) {
	properties, err := objectResource(object)
	if err != nil {
		return nil, err
	}
	switch typed := properties.(type) {
	case *openapi.RegisteredModel:
		return typed.CustomProperties, nil
	case *openapi.ModelVersion:
		return typed.CustomProperties, nil
	case *openapi.ModelArtifact:
		return typed.CustomProperties, nil
	default:
		return nil, fmt.Errorf("object %d has unsupported kind %q: %w",
			object.ID, object.Kind, api.ErrBadRequest)
	}
}

// valueTypeName reports the metadata type name of one value.
func valueTypeName(value filterValue) string {
	switch value.kind {
	case scalarString:
		return platformfilter.StringValueType
	case scalarBool:
		return platformfilter.BoolValueType
	case scalarInt:
		return platformfilter.IntValueType
	case scalarDouble:
		return platformfilter.DoubleValueType
	default:
		return ""
	}
}

func literalFilterValue(value any) (filterValue, bool) {
	switch typed := value.(type) {
	case string:
		return stringFilterValue(typed), true
	case bool:
		return boolFilterValue(typed), true
	case int64:
		return integerFilterValue(typed), true
	case float64:
		return numberFilterValue(typed), true
	default:
		return filterValue{}, false
	}
}

// filterEquals preserves scalar kinds while allowing exact numeric comparison.
func filterEquals(left filterValue, right any) (bool, bool) {
	value, ok := literalFilterValue(right)
	if !ok {
		return false, false
	}
	if left.kind == scalarString && value.kind == scalarString {
		return left.str == value.str, true
	}
	if left.kind == scalarBool && value.kind == scalarBool {
		return left.bool == value.bool, true
	}
	comparison, comparable := compareNumbers(left, value)
	return comparison == 0 && comparable, comparable
}

func filterCompare(left filterValue, right any, operator string) (bool, error) {
	value, ok := literalFilterValue(right)
	if !ok {
		return false, nil
	}
	var comparison int
	if left.kind == scalarString && value.kind == scalarString {
		comparison = strings.Compare(left.str, value.str)
	} else {
		var comparable bool
		comparison, comparable = compareNumbers(left, value)
		if !comparable {
			return false, nil
		}
	}
	switch operator {
	case ">":
		return comparison > 0, nil
	case ">=":
		return comparison >= 0, nil
	case "<":
		return comparison < 0, nil
	case "<=":
		return comparison <= 0, nil
	default:
		return false, fmt.Errorf("filter operator %q is not supported: %w", operator, api.ErrBadRequest)
	}
}

// compareNumbers never rounds an integer through float64. Rational conversion
// represents finite doubles exactly, including their binary fractional value.
func compareNumbers(left, right filterValue) (int, bool) {
	numeric := func(value filterValue) bool { return value.kind == scalarInt || value.kind == scalarDouble }
	if !numeric(left) || !numeric(right) {
		return 0, false
	}
	if left.kind == scalarInt && right.kind == scalarInt {
		switch {
		case left.integer < right.integer:
			return -1, true
		case left.integer > right.integer:
			return 1, true
		default:
			return 0, true
		}
	}
	// Nonfinite values cannot be emitted in a stored JSON resource.
	if (left.kind == scalarDouble && (math.IsNaN(left.num) || math.IsInf(left.num, 0))) ||
		(right.kind == scalarDouble && (math.IsNaN(right.num) || math.IsInf(right.num, 0))) {
		return 0, false
	}
	rational := func(value filterValue) *big.Rat {
		if value.kind == scalarInt {
			return new(big.Rat).SetInt64(value.integer)
		}
		return new(big.Rat).SetFloat64(value.num)
	}
	return rational(left).Cmp(rational(right)), true
}

// filterIn applies an IN membership test.
func filterIn(left filterValue, right any) (bool, error) {
	values, ok := right.([]any)
	if !ok {
		return false, fmt.Errorf("the IN operator requires a value list: %w", api.ErrBadRequest)
	}
	for _, candidate := range values {
		if equal, comparable := filterEquals(left, candidate); comparable && equal {
			return true, nil
		}
	}
	return false, nil
}

// matchLike applies a SQL LIKE pattern with % and _ wildcards.
func matchLike(pattern, value string, caseInsensitive bool) bool {
	if caseInsensitive {
		pattern = strings.ToLower(pattern)
		value = strings.ToLower(value)
	}
	return likeMatch(pattern, value)
}

func likeMatch(patternText, valueText string) bool {
	pattern, value := []rune(patternText), []rune(valueText)
	// Dynamic programming over pattern prefixes; % matches any run, _
	// matches one character, everything else matches literally.
	matches := make([]bool, len(value)+1)
	matches[0] = true
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '%':
			for j := 1; j <= len(value); j++ {
				matches[j] = matches[j] || matches[j-1]
			}
		case '_':
			copy(matches[1:], matches[:len(matches)-1])
			matches[0] = false
		default:
			for j := len(value); j >= 1; j-- {
				matches[j] = matches[j-1] && value[j-1] == pattern[i]
			}
			matches[0] = false
		}
	}
	return matches[len(value)]
}
