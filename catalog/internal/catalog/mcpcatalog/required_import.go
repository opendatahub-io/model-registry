package mcpcatalog

import (
	"context"
	"fmt"
	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
)

// ImportRequired uses transaction-bound services and never starts file watchers.
func ImportRequired(ctx context.Context, path string, source *basecatalog.MCPSource, services Services) error {
	catalog, err := (&yamlMCPProvider{}).read(path)
	if err != nil {
		return err
	}
	filter, err := NewServerFilterFromSource(source)
	if err != nil {
		return err
	}
	loader := &MCPLoader{}
	for _, entry := range catalog.MCPServers {
		if err := ctx.Err(); err != nil {
			return err
		}
		record := entry.ToMCPServerProviderRecord()
		if record.Error != nil {
			return record.Error
		}
		loader.setServerSourceID(record.Server, source.ID)
		if !filter.Allows(*record.Server.GetAttributes().Name) {
			continue
		}
		saved, err := services.MCPServerRepository.Save(record.Server)
		if err != nil {
			return err
		}
		if saved.GetID() == nil {
			return fmt.Errorf("MCP server %q has no persisted ID", entry.Name)
		}
		for _, tool := range record.Tools {
			if _, err := services.MCPServerToolRepository.Save(buildMCPServerTool(saved, tool), saved.GetID()); err != nil {
				return fmt.Errorf("MCP server %q tool %q: %w", entry.Name, tool.Name, err)
			}
		}
	}
	return nil
}
