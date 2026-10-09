package agentcatalog

import (
	"fmt"
	"strings"
)

// ValidateRequiredYAML rejects malformed shipped agent content before ingestion.
func ValidateRequiredYAML(path string) error {
	catalog, err := readYAMLAgentCatalog(path)
	if err != nil {
		return err
	}
	if catalog.Agents == nil {
		return fmt.Errorf("%s: required agents list is missing", path)
	}
	names := map[string]bool{}
	for i, agent := range catalog.Agents {
		if strings.TrimSpace(agent.Name) == "" || names[agent.Name] {
			return fmt.Errorf("%s: agent %d has an empty or duplicate name", path, i)
		}
		names[agent.Name] = true
	}
	return nil
}
