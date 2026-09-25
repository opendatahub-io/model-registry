package service

import (
	"testing"

	"github.com/kubeflow/hub/internal/platform/db/filter"
	"github.com/stretchr/testify/assert"
)

var expectedServingRuntimeProperties = map[string]filter.PropertyDefinition{
	"id":                       {Location: filter.EntityTable, ValueType: filter.IntValueType, Column: "id"},
	"name":                     {Location: filter.EntityTable, ValueType: filter.StringValueType, Column: "name"},
	"externalId":               {Location: filter.EntityTable, ValueType: filter.StringValueType, Column: "external_id"},
	"createTimeSinceEpoch":     {Location: filter.EntityTable, ValueType: filter.IntValueType, Column: "create_time_since_epoch"},
	"lastUpdateTimeSinceEpoch": {Location: filter.EntityTable, ValueType: filter.IntValueType, Column: "last_update_time_since_epoch"},
	"source_id":                {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "source_id"},
	"displayName":              {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "displayName"},
	"description":              {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "description"},
	"provider":                 {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "provider"},
	"readme":                   {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "readme"},
	"logo":                     {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "logo"},
	"license":                  {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "license"},
	"licenseLink":              {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "licenseLink"},
	"documentationUrl":         {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "documentationUrl"},
	"repositoryUrl":            {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "repositoryUrl"},
	"tags":                     {Location: filter.PropertyTable, ValueType: filter.ArrayValueType, Column: "tags"},
	"supportedModelFormats":    {Location: filter.PropertyTable, ValueType: filter.ArrayValueType, Column: "supportedModelFormats"},
	"capabilities":             {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "capabilities"},
	"versionCount":             {Location: filter.PropertyTable, ValueType: filter.IntValueType, Column: "versionCount"},
	"publishedDate":            {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "publishedDate"},
	"lastUpdated":              {Location: filter.PropertyTable, ValueType: filter.StringValueType, Column: "lastUpdated"},
}

func TestServingRuntimeEntityMappings(t *testing.T) {
	mappings := newServingRuntimeEntityMappings()
	assert.Equal(t, filter.EntityTypeContext, mappings.GetMLMDEntityType(""))

	for prop, expected := range expectedServingRuntimeProperties {
		t.Run(prop, func(t *testing.T) {
			got := mappings.GetPropertyDefinitionForRestEntity("", prop)
			assert.Equal(t, expected, got)
		})
	}

	got := mappings.GetPropertyDefinitionForRestEntity("", "unknownProp")
	assert.Equal(t, filter.Custom, got.Location)

	assert.False(t, mappings.IsChildEntity(""))
}
