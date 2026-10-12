package serving_runtimecatalog

import (
	"context"

	"github.com/kubeflow/hub/catalog/internal/catalog/basecatalog"
)

// ImportRequired participates in the caller's transaction across all asset types.
func ImportRequired(ctx context.Context, path, sourceID string, services Services) error {
	entries, err := loadServingRuntimesFromYAML(path)
	if err != nil {
		return err
	}
	state := basecatalog.NewBaseLoader(nil)
	state.SetLeader(true)
	loader := &ServingRuntimeLoader{state: state, services: services}
	return loader.persistEntries(ctx, sourceID, entries)
}
