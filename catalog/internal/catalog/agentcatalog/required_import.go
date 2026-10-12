package agentcatalog

import (
	"context"
	"fmt"
)

// ImportRequired imports the complete shipped source using the caller's transaction.
func ImportRequired(ctx context.Context, path, sourceID string, services Services) error {
	catalog, err := readYAMLAgentCatalog(path)
	if err != nil {
		return err
	}
	for _, entry := range catalog.Agents {
		if err := ctx.Err(); err != nil {
			return err
		}
		saved, err := services.AgentRepository.Save(yamlAgentToEntity(entry, sourceID))
		if err != nil {
			return err
		}
		if saved.GetID() == nil {
			return fmt.Errorf("agent %q has no persisted ID", entry.Name)
		}
		for _, template := range entry.Templates {
			if _, err := services.AgentTemplateArtifactRepository.Save(yamlTemplateToEntity(template, entry.Name, sourceID), saved.GetID()); err != nil {
				return fmt.Errorf("agent %q template: %w", entry.Name, err)
			}
		}
	}
	return nil
}
