package mcpcatalog

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/mcpcatalog/models"
	sharedmodels "github.com/kubeflow/hub/catalog/internal/db/models"
	dbmodels "github.com/kubeflow/hub/internal/platform/db/entity"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetFilterOptions_TagsAboveFiftyDistinctValues is a regression test for
// the tags filter disappearing from filter_options once the MCP catalog held
// 50 or more distinct tags (the context_property_options view dropped the
// whole field instead of offering its values).
func TestGetFilterOptions_TagsAboveFiftyDistinctValues(t *testing.T) {
	db, services, cleanup := setupMCPLoaderTest(t)
	defer cleanup()
	testutils.CleanupPostgresTestData(t, db)

	// 11 servers x 5 tags = 55 distinct tags, matching the catalog that
	// triggered the bug.
	var want []any
	for s := range 11 {
		tags := make([]string, 5)
		for i := range tags {
			tags[i] = fmt.Sprintf("tag-%02d", s*5+i)
			want = append(want, tags[i])
		}
		encoded, err := json.Marshal(tags)
		require.NoError(t, err)

		name := fmt.Sprintf("tags-server-%02d", s)
		_, err = services.MCPServerRepository.Save(&models.MCPServerImpl{
			Attributes: &models.MCPServerAttributes{Name: &name},
			Properties: &[]dbmodels.Properties{
				dbmodels.NewStringProperty("source_id", "tags-cardinality-test", false),
				dbmodels.NewStringProperty("tags", string(encoded), false),
			},
		})
		require.NoError(t, err)
	}

	catalog := NewDBMCPCatalog(services, nil, nil)
	require.NoError(t, services.PropertyOptionsRepository.Refresh(sharedmodels.ContextPropertyOptionType))
	options, err := catalog.GetFilterOptions(t.Context())
	require.NoError(t, err)
	require.NotNil(t, options.Filters)

	require.Contains(t, *options.Filters, "tags")
	tagsOption := (*options.Filters)["tags"]
	assert.Equal(t, "string", tagsOption.Type)
	assert.ElementsMatch(t, want, tagsOption.Values)
}
