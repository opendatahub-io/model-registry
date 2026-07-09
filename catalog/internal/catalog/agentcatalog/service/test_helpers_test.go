package service

import (
	"errors"
	"os"
	"testing"

	"github.com/kubeflow/hub/internal/platform/datastore"
	"github.com/kubeflow/hub/internal/platform/db/schema"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	os.Exit(testutils.TestMainPostgresHelper(m))
}

const testAgentTypeName = "kf.Agent"

func testDatastoreSpec() *datastore.Spec {
	return datastore.NewSpec().
		AddContext(testAgentTypeName, datastore.NewSpecType(NewAgentRepository).
			AddString("source_id").
			AddString("displayName").
			AddString("description").
			AddString("readme").
			AddString("framework").
			AddStruct("labels").
			AddString("logo").
			AddString("repositoryUrl").
			AddStruct("env").
			AddStruct("artifacts"),
		)
}

func getAgentTypeID(t *testing.T, db *gorm.DB) int32 {
	var typeRecord schema.Type
	err := db.Where("name = ?", testAgentTypeName).First(&typeRecord).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			typeRecord = schema.Type{
				Name: testAgentTypeName,
			}
			err = db.Create(&typeRecord).Error
			require.NoError(t, err)
		} else {
			require.NoError(t, err)
		}
	}
	return typeRecord.ID
}
