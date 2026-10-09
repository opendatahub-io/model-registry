package mcpcatalog

import (
	"fmt"
	"strings"
)

// ValidateRequiredYAML rejects malformed shipped MCP content before ingestion.
func ValidateRequiredYAML(path string) error {
	catalog, err := (&yamlMCPProvider{}).read(path)
	if err != nil {
		return err
	}
	if catalog.MCPServers == nil {
		return fmt.Errorf("%s: required mcp_servers list is missing", path)
	}
	names := map[string]bool{}
	for i, server := range catalog.MCPServers {
		if server == nil || strings.TrimSpace(server.Name) == "" || names[server.Name] {
			return fmt.Errorf("%s: MCP server %d is null or has an empty or duplicate name", path, i)
		}
		names[server.Name] = true
	}
	return nil
}
