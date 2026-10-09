package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kubeflow/hub/catalog/internal/activation"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestRequiredContentFailsBeforeDatabaseStartup(t *testing.T) {
	previous := catalogCfg
	t.Cleanup(func() { catalogCfg = previous })
	root := t.TempDir()
	catalogCfg.RequiredConfigPath = []string{filepath.Join(root, "missing-sources.yaml")}
	report := filepath.Join(root, "termination-log")
	t.Setenv("CATALOG_ACTIVATION_FAILURE_PATH", report)
	// A content error must be returned without trying to connect to PostgreSQL.
	t.Setenv("PGHOST", "invalid.database.example")
	command := &cobra.Command{}
	command.SetContext(context.Background())
	err := runCatalogServer(command, nil)
	var failure *activation.Failure
	require.True(t, errors.As(err, &failure), "expected preflight error, got %v", err)
	require.Equal(t, "CatalogContentInvalid", failure.Reason)
	data, readErr := os.ReadFile(report)
	require.NoError(t, readErr)
	require.Contains(t, string(data), "CatalogContentInvalid")
}
