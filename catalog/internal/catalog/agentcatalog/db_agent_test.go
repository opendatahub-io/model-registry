package agentcatalog

import (
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/agentcatalog/models"
	"github.com/stretchr/testify/assert"
)

func TestDisplayNameFromStoredName(t *testing.T) {
	tests := []struct {
		name     string
		stored   string
		expected string
	}{
		{"strips source prefix", "source_id:agent-name", "agent-name"},
		{"no prefix returns as-is", "agent-name", "agent-name"},
		{"empty string", "", ""},
		{"multiple colons strips first only", "source:agent:extra", "agent:extra"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, displayNameFromStoredName(tc.stored))
		})
	}
}

func TestTemplateDisplayNameFromStoredName(t *testing.T) {
	tests := []struct {
		name     string
		stored   string
		expected string
	}{
		{"three-segment qualified name", "test_source:agent-name:agent.yaml", "agent.yaml"},
		{"two-segment name", "source_id:template.yaml", "template.yaml"},
		{"no prefix returns as-is", "template.yaml", "template.yaml"},
		{"empty string", "", ""},
		{"trailing colon returns empty", "source:agent:", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, templateDisplayNameFromStoredName(tc.stored))
		})
	}
}

func TestMapDBTemplateArtifactToAPI_StripsNamePrefix(t *testing.T) {
	qualifiedName := "test_source:my-agent:agent.yaml"
	content := "template content"
	var id int32 = 42
	var typeID int32 = 7
	var createTime int64 = 1000
	var updateTime int64 = 2000

	artifact := &models.AgentTemplateArtifactImpl{
		ID:     &id,
		TypeID: &typeID,
		Attributes: &models.AgentTemplateArtifactAttributes{
			Name:                     &qualifiedName,
			Content:                  &content,
			CreateTimeSinceEpoch:     &createTime,
			LastUpdateTimeSinceEpoch: &updateTime,
		},
	}

	result := mapDBTemplateArtifactToAPI(artifact)

	assert.NotNil(t, result.Name)
	assert.Equal(t, "agent.yaml", *result.Name, "template artifact name should be stripped to just the template name")
	assert.Equal(t, "template content", result.Content)
	assert.Equal(t, "42", *result.Id)
}

func TestMapDBTemplateArtifactToAPI_NoColonInName(t *testing.T) {
	plainName := "agent.yaml"
	var id int32 = 1
	var typeID int32 = 7

	artifact := &models.AgentTemplateArtifactImpl{
		ID:     &id,
		TypeID: &typeID,
		Attributes: &models.AgentTemplateArtifactAttributes{
			Name: &plainName,
		},
	}

	result := mapDBTemplateArtifactToAPI(artifact)

	assert.NotNil(t, result.Name)
	assert.Equal(t, "agent.yaml", *result.Name, "name without colons should remain unchanged")
}
