package service

import "github.com/kubeflow/hub/internal/platform/db/filter"

type servingRuntimeVersionEntityMappings struct{}

// Unexported: only used by the repository constructor in this package.
// Export (New...) if tests in a parent package need to call it directly.
func newServingRuntimeVersionEntityMappings() filter.EntityMappingFunctions {
	return &servingRuntimeVersionEntityMappings{}
}

func (m *servingRuntimeVersionEntityMappings) GetMLMDEntityType(_ filter.RestEntityType) filter.EntityType {
	return filter.EntityTypeArtifact
}

func (m *servingRuntimeVersionEntityMappings) GetPropertyDefinitionForRestEntity(_ filter.RestEntityType, propertyName string) filter.PropertyDefinition {
	if def, ok := servingRuntimeVersionProperties[propertyName]; ok {
		return def
	}
	return filter.PropertyDefinition{
		Location:  filter.Custom,
		ValueType: filter.StringValueType,
		Column:    propertyName,
	}
}

func (m *servingRuntimeVersionEntityMappings) IsChildEntity(_ filter.RestEntityType) bool {
	return false
}

var servingRuntimeVersionProperties = map[string]filter.PropertyDefinition{
	"id":                       {Location: filter.EntityTable, ValueType: filter.IntValueType, Column: "id"},
	"name":                     {Location: filter.EntityTable, ValueType: filter.StringValueType, Column: "name"},
	"externalId":               {Location: filter.EntityTable, ValueType: filter.StringValueType, Column: "external_id"},
	"createTimeSinceEpoch":     {Location: filter.EntityTable, ValueType: filter.IntValueType, Column: "create_time_since_epoch"},
	"lastUpdateTimeSinceEpoch": {Location: filter.EntityTable, ValueType: filter.IntValueType, Column: "last_update_time_since_epoch"},
	"uri":                      {Location: filter.EntityTable, ValueType: filter.StringValueType, Column: "uri"},
	"state":                    {Location: filter.EntityTable, ValueType: filter.StringValueType, Column: "state"},
	"source_id":                {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "source_id"},
	"artifactType":             {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "artifactType"},
	"version":                  {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "version"},
	"image":                    {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "image"},
	"supportLevel":             {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "supportLevel"},
	"supportedModelFormats":    {Location: filter.PropertyTable, ValueType: filter.ArrayValueType, Column: "supportedModelFormats"},
	"protocolVersions":         {Location: filter.PropertyTable, ValueType: filter.ArrayValueType, Column: "protocolVersions"},
	"recommendedResources":     {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "recommendedResources"},
	"defaultArgs":              {Location: filter.PropertyTable, ValueType: filter.ArrayValueType, Column: "defaultArgs"},
	"env":                      {Location: filter.PropertyTable, ValueType: filter.ArrayValueType, Column: "env"},
	"template":                 {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "template"},
	"deprecated":               {Location: filter.PropertyTable, ValueType: filter.BoolValueType, Column: "deprecated"},
	"publishedDate":            {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "publishedDate"},
}
