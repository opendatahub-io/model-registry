package agentcatalog

import (
	"context"
	"errors"
	"testing"

	"github.com/kubeflow/hub/catalog/internal/catalog/agentcatalog/models"
	agentservice "github.com/kubeflow/hub/catalog/internal/catalog/agentcatalog/service"
	openapi "github.com/kubeflow/hub/catalog/pkg/openapi"
	dbmodels "github.com/kubeflow/hub/internal/platform/db/entity"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mock repositories ---

type mockAgentRepo struct {
	getResult models.Agent
	getErr    error
}

func (m *mockAgentRepo) GetByID(_ int32) (models.Agent, error) {
	if m.getResult != nil || m.getErr != nil {
		return m.getResult, m.getErr
	}
	return nil, errors.New("not implemented")
}
func (m *mockAgentRepo) GetByName(_ string) (models.Agent, error) {
	return nil, errors.New("not implemented")
}
func (m *mockAgentRepo) List(_ *models.AgentListOptions) (*dbmodels.ListWrapper[models.Agent], error) {
	return nil, errors.New("not implemented")
}
func (m *mockAgentRepo) Save(_ models.Agent) (models.Agent, error) {
	return nil, errors.New("not implemented")
}
func (m *mockAgentRepo) DeleteBySource(_ string) error { return errors.New("not implemented") }
func (m *mockAgentRepo) DeleteByID(_ int32) error      { return errors.New("not implemented") }
func (m *mockAgentRepo) GetDistinctSourceIDs() ([]string, error) {
	return nil, errors.New("not implemented")
}
func (m *mockAgentRepo) GetTypeID() int32 { return 0 }

type mockAgentTemplateArtifactRepo struct {
	listResult *dbmodels.ListWrapper[models.AgentTemplateArtifact]
	listErr    error
	// capturedListOptions stores the last options passed to List, nil if List was never called.
	capturedListOptions *models.AgentTemplateArtifactListOptions
}

func (m *mockAgentTemplateArtifactRepo) GetByID(_ int32) (models.AgentTemplateArtifact, error) {
	return nil, errors.New("not implemented")
}
func (m *mockAgentTemplateArtifactRepo) List(opts models.AgentTemplateArtifactListOptions) (*dbmodels.ListWrapper[models.AgentTemplateArtifact], error) {
	m.capturedListOptions = &opts
	return m.listResult, m.listErr
}
func (m *mockAgentTemplateArtifactRepo) Save(_ models.AgentTemplateArtifact, _ *int32) (models.AgentTemplateArtifact, error) {
	return nil, errors.New("not implemented")
}
func (m *mockAgentTemplateArtifactRepo) DeleteByParentID(_ int32) error {
	return errors.New("not implemented")
}

// --- helpers ---

func newTestAgentCatalog(agentRepo models.AgentRepository, templateRepo models.AgentTemplateArtifactRepository) *DBAgentCatalog {
	return &DBAgentCatalog{
		agentRepo:                 agentRepo,
		agentTemplateArtifactRepo: templateRepo,
	}
}

func agentEntity(id int32) models.Agent {
	name := "test-source:test-agent"
	return &models.AgentImpl{
		ID:         &id,
		Attributes: &models.AgentAttributes{Name: &name},
	}
}

func makeTemplateArtifact(id int32, name string, content string) models.AgentTemplateArtifact {
	return &models.AgentTemplateArtifactImpl{
		ID: &id,
		Attributes: &models.AgentTemplateArtifactAttributes{
			Name:    &name,
			Content: &content,
		},
	}
}

func templateArtifactList(items ...models.AgentTemplateArtifact) *dbmodels.ListWrapper[models.AgentTemplateArtifact] {
	return &dbmodels.ListWrapper[models.AgentTemplateArtifact]{Items: items}
}

// --- GetAgentArtifacts tests ---

func TestGetAgentArtifacts_InvalidAgentID(t *testing.T) {
	cat := newTestAgentCatalog(&mockAgentRepo{}, &mockAgentTemplateArtifactRepo{})

	_, err := cat.GetAgentArtifacts(context.Background(), "not-a-number", nil, 10, "", "", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, api.ErrBadRequest)
}

func TestGetAgentArtifacts_AgentNotFound(t *testing.T) {
	agentRepo := &mockAgentRepo{getErr: agentservice.ErrAgentNotFound}
	cat := newTestAgentCatalog(agentRepo, &mockAgentTemplateArtifactRepo{})

	_, err := cat.GetAgentArtifacts(context.Background(), "1", nil, 10, "", "", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, api.ErrNotFound)
}

func TestGetAgentArtifacts_NoFilterFetchesTemplates(t *testing.T) {
	agentRepo := &mockAgentRepo{getResult: agentEntity(1)}
	templateRepo := &mockAgentTemplateArtifactRepo{
		listResult: templateArtifactList(makeTemplateArtifact(1, "agent.yaml", "content-a")),
	}
	cat := newTestAgentCatalog(agentRepo, templateRepo)

	result, err := cat.GetAgentArtifacts(context.Background(), "1", nil, 10, "", "", nil)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	require.NotNil(t, result.Items[0].Name)
	assert.Equal(t, "agent.yaml", *result.Items[0].Name, "template name should be the base name, not the qualified stored name")

	require.NotNil(t, templateRepo.capturedListOptions, "List should be called on the template repo")
	require.NotNil(t, templateRepo.capturedListOptions.ParentResourceID)
	assert.Equal(t, int32(1), *templateRepo.capturedListOptions.ParentResourceID)
}

func TestGetAgentArtifacts_ExplicitTemplateArtifactFilterFetchesTemplates(t *testing.T) {
	agentRepo := &mockAgentRepo{getResult: agentEntity(1)}
	templateRepo := &mockAgentTemplateArtifactRepo{
		listResult: templateArtifactList(makeTemplateArtifact(1, "agent.yaml", "content-a")),
	}
	cat := newTestAgentCatalog(agentRepo, templateRepo)

	result, err := cat.GetAgentArtifacts(context.Background(), "1",
		[]openapi.AgentArtifactTypeQueryParam{openapi.AGENTARTIFACTTYPEQUERYPARAM_TEMPLATE_ARTIFACT}, 10, "", "", nil)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	require.NotNil(t, templateRepo.capturedListOptions, "List should be called when filter explicitly includes template-artifact")
}

func TestGetAgentArtifacts_NonTemplateFilterSkipsFetch(t *testing.T) {
	agentRepo := &mockAgentRepo{getResult: agentEntity(1)}
	templateRepo := &mockAgentTemplateArtifactRepo{
		listResult: templateArtifactList(makeTemplateArtifact(1, "agent.yaml", "content-a")),
	}
	cat := newTestAgentCatalog(agentRepo, templateRepo)

	result, err := cat.GetAgentArtifacts(context.Background(), "1",
		[]openapi.AgentArtifactTypeQueryParam{"image-artifact"}, 10, "", "", nil)
	require.NoError(t, err)
	assert.Empty(t, result.Items)
	assert.Nil(t, templateRepo.capturedListOptions, "template repo List should not be called when the filter excludes template-artifact")
}

func TestGetAgentArtifacts_MixedFilterTypesIncludesTemplates(t *testing.T) {
	agentRepo := &mockAgentRepo{getResult: agentEntity(1)}
	templateRepo := &mockAgentTemplateArtifactRepo{
		listResult: templateArtifactList(makeTemplateArtifact(1, "agent.yaml", "content-a")),
	}
	cat := newTestAgentCatalog(agentRepo, templateRepo)

	result, err := cat.GetAgentArtifacts(context.Background(), "1",
		[]openapi.AgentArtifactTypeQueryParam{"image-artifact", openapi.AGENTARTIFACTTYPEQUERYPARAM_TEMPLATE_ARTIFACT}, 10, "", "", nil)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
}

func TestGetAgentArtifacts_EmptyResultReturnsEmptySliceNotNil(t *testing.T) {
	agentRepo := &mockAgentRepo{getResult: agentEntity(1)}
	templateRepo := &mockAgentTemplateArtifactRepo{listResult: templateArtifactList()}
	cat := newTestAgentCatalog(agentRepo, templateRepo)

	result, err := cat.GetAgentArtifacts(context.Background(), "1", nil, 10, "", "", nil)
	require.NoError(t, err)
	assert.NotNil(t, result.Items)
	assert.Empty(t, result.Items)
	assert.Equal(t, int32(0), result.Size)
}

func TestGetAgentArtifacts_NilTemplateRepoSkipsFetch(t *testing.T) {
	agentRepo := &mockAgentRepo{getResult: agentEntity(1)}
	cat := newTestAgentCatalog(agentRepo, nil)

	result, err := cat.GetAgentArtifacts(context.Background(), "1", nil, 10, "", "", nil)
	require.NoError(t, err)
	assert.Empty(t, result.Items)
}

func TestGetAgentArtifacts_RepoErrorPropagates(t *testing.T) {
	agentRepo := &mockAgentRepo{getResult: agentEntity(1)}
	templateRepo := &mockAgentTemplateArtifactRepo{listErr: errors.New("db error")}
	cat := newTestAgentCatalog(agentRepo, templateRepo)

	_, err := cat.GetAgentArtifacts(context.Background(), "1", nil, 10, "", "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db error")
}

func TestGetAgentArtifacts_NextPageTokenPassthrough(t *testing.T) {
	agentRepo := &mockAgentRepo{getResult: agentEntity(1)}
	templateRepo := &mockAgentTemplateArtifactRepo{
		listResult: &dbmodels.ListWrapper[models.AgentTemplateArtifact]{
			Items:         []models.AgentTemplateArtifact{makeTemplateArtifact(1, "agent.yaml", "content-a")},
			NextPageToken: "next-token",
		},
	}
	cat := newTestAgentCatalog(agentRepo, templateRepo)

	result, err := cat.GetAgentArtifacts(context.Background(), "1", nil, 10, "", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "next-token", result.NextPageToken)
}

// --- mapDBTemplateArtifactToAPI tests ---

func TestMapDBTemplateArtifactToAPI_AllFieldsPresent(t *testing.T) {
	id := int32(42)
	name := "src:agent:agent.yaml"
	content := "template content here"
	createTime := int64(1000)
	updateTime := int64(2000)
	externalID := "ext-123"

	dbArtifact := &models.AgentTemplateArtifactImpl{
		ID: &id,
		Attributes: &models.AgentTemplateArtifactAttributes{
			Name:                     &name,
			Content:                  &content,
			CreateTimeSinceEpoch:     &createTime,
			LastUpdateTimeSinceEpoch: &updateTime,
			ExternalID:               &externalID,
		},
	}

	result := mapDBTemplateArtifactToAPI(dbArtifact)

	assert.Equal(t, "template-artifact", result.ArtifactType)
	require.NotNil(t, result.Id)
	assert.Equal(t, "42", *result.Id)
	require.NotNil(t, result.Name)
	assert.Equal(t, "agent.yaml", *result.Name, "should return base name, not qualified stored name")
	assert.Equal(t, content, result.Content)
	require.NotNil(t, result.CreateTimeSinceEpoch)
	assert.Equal(t, "1000", *result.CreateTimeSinceEpoch)
	require.NotNil(t, result.LastUpdateTimeSinceEpoch)
	assert.Equal(t, "2000", *result.LastUpdateTimeSinceEpoch)
	require.NotNil(t, result.ExternalId)
	assert.Equal(t, externalID, *result.ExternalId)
}

func TestMapDBTemplateArtifactToAPI_NilID(t *testing.T) {
	name := "agent.yaml"
	content := "x"
	dbArtifact := &models.AgentTemplateArtifactImpl{
		Attributes: &models.AgentTemplateArtifactAttributes{Name: &name, Content: &content},
	}

	result := mapDBTemplateArtifactToAPI(dbArtifact)
	assert.Nil(t, result.Id)
}

func TestMapDBTemplateArtifactToAPI_NilAttributes(t *testing.T) {
	dbArtifact := &models.AgentTemplateArtifactImpl{}

	result := mapDBTemplateArtifactToAPI(dbArtifact)

	assert.Equal(t, "template-artifact", result.ArtifactType)
	assert.Nil(t, result.Id)
	assert.Nil(t, result.Name)
	assert.Equal(t, "", result.Content)
	assert.Nil(t, result.CreateTimeSinceEpoch)
	assert.Nil(t, result.LastUpdateTimeSinceEpoch)
	assert.Nil(t, result.ExternalId)
}

func TestMapDBTemplateArtifactToAPI_NilContentPointer(t *testing.T) {
	name := "agent.yaml"
	dbArtifact := &models.AgentTemplateArtifactImpl{
		Attributes: &models.AgentTemplateArtifactAttributes{Name: &name},
	}

	result := mapDBTemplateArtifactToAPI(dbArtifact)

	assert.Equal(t, "", result.Content)
	require.NotNil(t, result.Name)
	assert.Equal(t, name, *result.Name)
}

func TestTemplateBaseNameFromStoredName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "qualified name", input: "source:agent:deploy.yaml", expected: "deploy.yaml"},
		{name: "no prefix", input: "agent.yaml", expected: "agent.yaml"},
		{name: "single prefix", input: "source:agent.yaml", expected: "agent.yaml"},
		{name: "empty string", input: "", expected: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, templateBaseNameFromStoredName(tc.input))
		})
	}
}

func TestDisplayNameFromStoredName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "qualified name", input: "source:agent-name", expected: "agent-name"},
		{name: "no prefix", input: "agent-name", expected: "agent-name"},
		{name: "empty string", input: "", expected: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, displayNameFromStoredName(tc.input))
		})
	}
}

func TestMapDBAgentToAPI_ResolvesReadmeLinks(t *testing.T) {
	id := int32(1)
	name := "test-source:test-agent"
	readmeContent := `# Agent

<img src="/images/arch.png" alt="architecture">

![diagram](/images/diagram.png)`
	repoURL := "https://github.com/owner/repo/tree/main/agents/test-agent"

	props := []dbmodels.Properties{
		dbmodels.NewStringProperty("readme", readmeContent, false),
		dbmodels.NewStringProperty("repositoryUrl", repoURL, false),
	}
	agent := &models.AgentImpl{
		ID:         &id,
		Attributes: &models.AgentAttributes{Name: &name},
		Properties: &props,
	}

	result := mapDBAgentToAPI(agent)

	require.NotNil(t, result.Readme)
	assert.Contains(t, *result.Readme, "https://raw.githubusercontent.com/owner/repo/main/images/arch.png")
	assert.Contains(t, *result.Readme, "https://raw.githubusercontent.com/owner/repo/main/images/diagram.png")
	assert.NotContains(t, *result.Readme, `src="/images/arch.png"`)
	assert.NotContains(t, *result.Readme, `](/images/diagram.png)`)
}

func TestMapDBAgentToAPI_NoRepoURLLeavesReadmeUnchanged(t *testing.T) {
	id := int32(1)
	name := "test-source:test-agent"
	readmeContent := `<img src="/images/arch.png" alt="architecture">`

	props := []dbmodels.Properties{
		dbmodels.NewStringProperty("readme", readmeContent, false),
	}
	agent := &models.AgentImpl{
		ID:         &id,
		Attributes: &models.AgentAttributes{Name: &name},
		Properties: &props,
	}

	result := mapDBAgentToAPI(agent)

	require.NotNil(t, result.Readme)
	assert.Equal(t, readmeContent, *result.Readme)
}
