package service

import (
	"errors"
	"testing"

	"github.com/kubeflow/hub/internal/platform/datastore"
	"github.com/kubeflow/hub/internal/platform/db/schema"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const (
	testServingRuntimeTypeName        = "kf.ServingRuntime"
	testServingRuntimeVersionTypeName = "kf.ServingRuntimeVersion"
)

// testDatastoreSpec returns a minimal datastore spec for serving_runtime catalog
// tests. This avoids importing catalog/internal/db/service which would cause an
// import cycle.
func testDatastoreSpec() *datastore.Spec {
	return datastore.NewSpec().
		AddContext(testServingRuntimeTypeName, datastore.NewSpecType(NewServingRuntimeRepository).
			AddString("source_id").
			AddString("base_name").
			AddString("displayName").
			AddString("description").
			AddString("provider").
			AddString("readme").
			AddString("logo").
			AddString("license").
			AddString("licenseLink").
			AddString("documentationUrl").
			AddString("repositoryUrl").
			AddStruct("tags").
			AddStruct("supportedModelFormats").
			AddStruct("capabilities").
			AddInt("versionCount").
			AddString("publishedDate").
			AddString("lastUpdated"),
		).
		AddArtifact(testServingRuntimeVersionTypeName, datastore.NewSpecType(NewServingRuntimeVersionRepository).
			AddString("source_id").
			AddString("artifactType").
			AddString("version").
			AddString("image").
			AddString("supportLevel").
			AddStruct("supportedModelFormats").
			AddStruct("protocolVersions").
			AddStruct("recommendedResources").
			AddStruct("defaultArgs").
			AddStruct("env").
			AddString("template").
			AddBoolean("deprecated").
			AddString("publishedDate"),
		)
}

// getServingRuntimeTypeID gets the ServingRuntime type ID from the database.
func getServingRuntimeTypeID(t *testing.T, db *gorm.DB) int32 {
	return getOrCreateTypeID(t, db, testServingRuntimeTypeName)
}

// getServingRuntimeVersionTypeID gets the ServingRuntimeVersion type ID from the database.
func getServingRuntimeVersionTypeID(t *testing.T, db *gorm.DB) int32 {
	return getOrCreateTypeID(t, db, testServingRuntimeVersionTypeName)
}

func getOrCreateTypeID(t *testing.T, db *gorm.DB, name string) int32 {
	var typeRecord schema.Type
	err := db.Where("name = ?", name).First(&typeRecord).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			typeRecord = schema.Type{
				Name: name,
			}
			err = db.Create(&typeRecord).Error
			require.NoError(t, err)
		} else {
			require.NoError(t, err)
		}
	}
	return typeRecord.ID
}
