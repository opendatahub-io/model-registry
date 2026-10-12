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
	names := map[[2]string]bool{}
	for i, server := range catalog.MCPServers {
		if server == nil {
			return fmt.Errorf("%s: MCP server %d is null", path, i)
		}
		if err := server.validate(); err != nil {
			return fmt.Errorf("%s: MCP server %d: %w", path, i, err)
		}
		version := ""
		if server.Version != nil {
			version = *server.Version
		}
		identity := [2]string{strings.TrimSpace(server.Name), version}
		if names[identity] {
			return fmt.Errorf("%s: duplicate MCP server name/version %q/%q", path, identity[0], identity[1])
		}
		names[identity] = true
	}
	return nil
}
