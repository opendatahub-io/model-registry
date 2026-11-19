package mlflow_test

import (
	"fmt"
	"testing"

	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/require"
)

// TestInlineWritesRaceOwnSweep proves inline request paths and this
// replica's own Run sweep can drive the journal concurrently: the sweep
// must skip operations an inline application is driving instead of
// re-claiming their lease, which would invalidate the in-flight
// fencing token and surface a spurious conflict to the client.
func TestInlineWritesRaceOwnSweep(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			go func() { _ = backend.service.Run(t.Context()) }()

			for i := range 10 {
				model, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
					Name: fmt.Sprintf("race-model-%d", i),
				})
				require.NoError(t, err, "inline create %d must not fail while the sweep runs", i)
				require.NotNil(t, model.Id)
			}
		})
	}
}
