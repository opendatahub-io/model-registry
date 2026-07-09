package service

import (
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/agentcatalog/models"
	dbmodels "github.com/kubeflow/hub/internal/platform/db/entity"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentRepository(t *testing.T) {
	sharedDB, cleanup := testutils.SetupPostgresWithMigrations(t, testDatastoreSpec())
	defer cleanup()

	typeID := getAgentTypeID(t, sharedDB)
	repo := NewAgentRepository(sharedDB, typeID)

	t.Run("TestList_FilterByName", func(t *testing.T) {
		// Create agents with source-prefixed names (as the loader does: sourceID:agentName)
		agents := []struct {
			contextName string
			framework   string
		}{
			{"test-source:langgraph-react-agent", "langgraph"},
			{"test-source:langgraph-agentic-rag", "langgraph"},
			{"test-source:crewai-websearch-agent", "crewai"},
		}

		for _, agent := range agents {
			_, err := repo.Save(&models.AgentImpl{
				Attributes: &models.AgentAttributes{
					Name: &agent.contextName,
				},
				Properties: &[]dbmodels.Properties{
					{
						Name:        "source_id",
						StringValue: new("test-source"),
					},
					{
						Name:        "framework",
						StringValue: &agent.framework,
					},
				},
			})
			require.NoError(t, err)
		}

		// Filter by name with wildcard — should match via %:pattern
		nameFilter := "langgraph%"
		result, err := repo.List(&models.AgentListOptions{
			Name: &nameFilter,
		})
		require.NoError(t, err)
		assert.Equal(t, 2, len(result.Items), "Expected 2 langgraph agents")
		for _, item := range result.Items {
			assert.Contains(t, *item.GetAttributes().Name, "langgraph")
		}

		// Exact name match
		exactFilter := "crewai-websearch-agent"
		exactResult, err := repo.List(&models.AgentListOptions{
			Name: &exactFilter,
		})
		require.NoError(t, err)
		assert.Equal(t, 1, len(exactResult.Items), "Expected 1 crewai agent")

		// No match
		noMatchFilter := "nonexistent%"
		noMatchResult, err := repo.List(&models.AgentListOptions{
			Name: &noMatchFilter,
		})
		require.NoError(t, err)
		assert.Equal(t, 0, len(noMatchResult.Items), "Expected 0 agents for nonexistent name")
	})
}
