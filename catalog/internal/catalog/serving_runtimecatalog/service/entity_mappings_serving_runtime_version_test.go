package service

import (
	"testing"

	"github.com/kubeflow/hub/internal/platform/db/filter"
	"github.com/stretchr/testify/assert"
)

var expectedServingRuntimeVersionProperties = map[string]filter.PropertyDefinition{
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

func TestServingRuntimeVersionEntityMappings(t *testing.T) {
	mappings := newServingRuntimeVersionEntityMappings()
	assert.Equal(t, filter.EntityTypeArtifact, mappings.GetMLMDEntityType(""))

	for prop, expected := range expectedServingRuntimeVersionProperties {
		t.Run(prop, func(t *testing.T) {
			got := mappings.GetPropertyDefinitionForRestEntity("", prop)
			assert.Equal(t, expected, got)
		})
	}

	got := mappings.GetPropertyDefinitionForRestEntity("", "unknownProp")
	assert.Equal(t, filter.Custom, got.Location)

	assert.False(t, mappings.IsChildEntity(""))
}
