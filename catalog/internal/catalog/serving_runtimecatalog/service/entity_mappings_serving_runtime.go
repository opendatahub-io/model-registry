package service

import "github.com/kubeflow/hub/internal/platform/db/filter"

type servingRuntimeEntityMappings struct{}

// Unexported: only used by the repository constructor in this package.
// Export (New...) if tests in a parent package need to call it directly.
func newServingRuntimeEntityMappings() filter.EntityMappingFunctions {
	return &servingRuntimeEntityMappings{}
}

func (m *servingRuntimeEntityMappings) GetMLMDEntityType(_ filter.RestEntityType) filter.EntityType {
	return filter.EntityTypeContext
}

func (m *servingRuntimeEntityMappings) GetPropertyDefinitionForRestEntity(_ filter.RestEntityType, propertyName string) filter.PropertyDefinition {
	if def, ok := servingRuntimeProperties[propertyName]; ok {
		return def
	}
	return filter.PropertyDefinition{
		Location:  filter.Custom,
		ValueType: filter.StringValueType,
		Column:    propertyName,
	}
}

func (m *servingRuntimeEntityMappings) IsChildEntity(_ filter.RestEntityType) bool {
	return false
}

var servingRuntimeProperties = map[string]filter.PropertyDefinition{
	"id":                                 {Location: filter.EntityTable, ValueType: filter.IntValueType, Column: "id"},
	"name":                               {Location: filter.EntityTable, ValueType: filter.StringValueType, Column: "name"},
	"externalId":                         {Location: filter.EntityTable, ValueType: filter.StringValueType, Column: "external_id"},
	"createTimeSinceEpoch":               {Location: filter.EntityTable, ValueType: filter.IntValueType, Column: "create_time_since_epoch"},
	"lastUpdateTimeSinceEpoch":           {Location: filter.EntityTable, ValueType: filter.IntValueType, Column: "last_update_time_since_epoch"},
	"source_id":                          {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "source_id"},
	"displayName":                        {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "displayName"},
	"description":                        {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "description"},
	"provider":                           {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "provider"},
	"readme":                             {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "readme"},
	"logo":                               {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "logo"},
	"license":                            {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "license"},
	"licenseLink":                        {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "licenseLink"},
	"documentationUrl":                   {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "documentationUrl"},
	"repositoryUrl":                      {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "repositoryUrl"},
	"tags":                               {Location: filter.PropertyTable, ValueType: filter.ArrayValueType, Column: "tags"},
	"supportedModelFormats":              {Location: filter.PropertyTable, ValueType: filter.ArrayValueType, Column: "supportedModelFormats"},
	"capabilities":                       {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "capabilities"},
	"capabilities.requiresGPU":           {Location: filter.PropertyTable, ValueType: filter.BoolValueType, Column: "capabilities.requiresGPU"},
	"capabilities.multiModel":            {Location: filter.PropertyTable, ValueType: filter.BoolValueType, Column: "capabilities.multiModel"},
	"capabilities.supportedAccelerators": {Location: filter.PropertyTable, ValueType: filter.ArrayValueType, Column: "capabilities.supportedAccelerators"},
	"versionCount":                       {Location: filter.PropertyTable, ValueType: filter.IntValueType, Column: "versionCount"},
	"publishedDate":                      {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "publishedDate"},
	"lastUpdated":                        {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "lastUpdated"},
}
