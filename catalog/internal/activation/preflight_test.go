package activation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
}

func TestPreflightRejectsEntireRequiredCandidate(t *testing.T) {
	for _, tc := range []struct {
		name, section, data string
	}{
		{"model syntax", "model_catalogs", "models: ["},
		{"model shape", "model_catalogs", "not_models: []"},
		{"model missing name", "model_catalogs", "models:\n- description: unnamed\n"},
		{"duplicate models", "model_catalogs", "models:\n- name: a\n- name: a\n"},
		{"mcp syntax", "mcp_catalogs", "mcp_servers: ["},
		{"mcp null", "mcp_catalogs", "mcp_servers: [null]"},
		{"mcp shape", "mcp_catalogs", "models: []"},
		{"agent syntax", "agent_catalogs", "agents: ["},
		{"agent unnamed", "agent_catalogs", "agents:\n- description: unnamed\n"},
		{"runtime shape", "serving_runtime_catalogs", "models: []"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			config := filepath.Join(root, "sources.yaml")
			writeFile(t, filepath.Join(root, "good.yaml"), "models:\n- name: good\n")
			writeFile(t, filepath.Join(root, "bad.yaml"), tc.data)
			writeFile(t, config, "model_catalogs:\n- id: good\n  name: Good\n  type: yaml\n  properties:\n    yamlCatalogPath: good.yaml\n")
			badConfig := filepath.Join(root, "bad-sources.yaml")
			writeFile(t, badConfig, tc.section+":\n- id: bad\n  name: Bad\n  type: yaml\n  properties:\n    yamlCatalogPath: bad.yaml\n")
			failure := Preflight([]string{config, badConfig}, nil, false)
			require.NotNil(t, failure)
			require.Equal(t, "Loading", failure.Stage)
			require.Equal(t, "CatalogContentInvalid", failure.Reason)
			require.Contains(t, failure.Message, "bad")
		})
	}
}

func TestPreflightPreservesAdministratorFilesAndRecovers(t *testing.T) {
	root := t.TempDir()
	shipped := filepath.Join(root, "shipped.yaml")
	config := filepath.Join(root, "sources.yaml")
	admin := filepath.Join(root, "admin.yaml")
	adminData := "model_catalogs: [invalid admin source\n"
	writeFile(t, admin, adminData)
	writeFile(t, config, "model_catalogs:\n- id: shipped\n  name: Shipped\n  type: yaml\n  properties:\n    yamlCatalogPath: shipped.yaml\n")
	// Missing required content rejects startup; a correction permits preflight.
	require.NotNil(t, Preflight([]string{config}, nil, false))
	writeFile(t, shipped, "models:\n- name: valid\n")
	require.Nil(t, Preflight([]string{config}, nil, false))
	data, err := os.ReadFile(admin)
	require.NoError(t, err)
	require.Equal(t, adminData, string(data))
	// Explicitly empty lists are permitted; missing lists are not.
	writeFile(t, shipped, "models: []\n")
	require.Nil(t, Preflight([]string{config}, nil, false))
}

func TestPreflightValidPluginBundle(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "sources.yaml")
	writeFile(t, config, `model_catalogs:
- id: models
  name: Models
  type: yaml
  properties:
    yamlCatalogPath: models.yaml
mcp_catalogs:
- id: mcp
  name: MCP
  type: yaml
  properties:
    yamlCatalogPath: mcp.yaml
agent_catalogs:
- id: agents
  name: Agents
  type: yaml
  properties:
    yamlCatalogPath: agents.yaml
serving_runtime_catalogs:
- id: runtimes
  name: Runtimes
  type: yaml
  properties:
    yamlCatalogPath: runtimes.yaml
`)
	writeFile(t, filepath.Join(root, "models.yaml"), "models:\n- name: valid\n")
	writeFile(t, filepath.Join(root, "mcp.yaml"), "mcp_servers:\n- name: valid\n")
	writeFile(t, filepath.Join(root, "agents.yaml"), "agents:\n- name: valid\n")
	writeFile(t, filepath.Join(root, "runtimes.yaml"), "serving_runtimes: []\n")
	require.Nil(t, Preflight([]string{config}, nil, false))
}

func TestRequiredBenchmarkPreflight(t *testing.T) {
	for _, tc := range []struct {
		name, filename, data string
	}{
		{"bad metadata", "metadata.json", "{broken"},
		{"missing metadata ID", "metadata.json", "{}"},
		{"bad evaluations", "evaluations.ndjson", "{\"model_id\":\"a\",\"benchmark\":\"b\"}\n{broken\n"},
		{"bad performance", "performance.ndjson", "{broken\n"},
		{"incomplete performance", "performance.ndjson", "{\"model_id\":\"a\"}\n"},
		{"bad security", "security-evaluations.ndjson", "{broken\n"},
		{"trailing JSON", "performance.ndjson", "{\"model_id\":\"a\",\"id\":\"b\"} {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			modelDir := filepath.Join(root, "model-not-in-catalog")
			writeFile(t, filepath.Join(modelDir, "metadata.json"), "{\"id\":\"a\"}")
			writeFile(t, filepath.Join(modelDir, tc.filename), tc.data)
			failure := Preflight(nil, []string{root}, true)
			require.NotNil(t, failure)
			require.Equal(t, "BenchmarkContentInvalid", failure.Reason)
			require.Contains(t, failure.Message, tc.filename)
			// Optional standalone benchmark policy remains unchanged.
			require.Nil(t, Preflight(nil, []string{root}, false))
		})
	}
	t.Run("missing and empty paths", func(t *testing.T) {
		require.NotNil(t, Preflight(nil, nil, true))
		root := t.TempDir()
		require.NotNil(t, Preflight(nil, []string{root}, true))
		require.NotNil(t, Preflight(nil, []string{filepath.Join(root, "missing")}, true))
		writeFile(t, filepath.Join(root, "not-dir"), "data")
		require.NotNil(t, Preflight(nil, []string{filepath.Join(root, "not-dir")}, true))
	})
	t.Run("valid benchmarks and config_id compatibility", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "m", "metadata.json"), "{\"id\":\"a\"}")
		writeFile(t, filepath.Join(root, "m", "performance.ndjson"), "\n{\"config_id\":\"p\",\"model_id\":\"a\",\"throughput\":1}\n")
		writeFile(t, filepath.Join(root, "m", "evaluations.ndjson"), "{\"model_id\":\"a\",\"benchmark\":\"b\",\"score\":0.5}\n")
		writeFile(t, filepath.Join(root, "m", "security-evaluations.ndjson"), "{\"id\":\"s\",\"model_id\":\"a\"}\n")
		require.Nil(t, Preflight(nil, []string{root}, true))
	})
	t.Run("orphan metric and duplicate model IDs", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "m", "performance.ndjson"), "{\"model_id\":\"a\",\"id\":\"p\"}\n")
		require.NotNil(t, Preflight(nil, []string{root}, true))
		writeFile(t, filepath.Join(root, "m", "metadata.json"), "{\"id\":\"a\"}")
		require.Nil(t, Preflight(nil, []string{root}, true))
		writeFile(t, filepath.Join(root, "duplicate", "metadata.json"), "{\"id\":\"A\"}")
		require.NotNil(t, Preflight(nil, []string{root}, true))
	})
}

func TestTerminationReportIsBoundedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termination-log")
	failure := &Failure{Stage: "Loading", Reason: "BenchmarkContentInvalid", Message: string(make([]byte, 10000))}
	require.NoError(t, failure.WriteTerminationMessage(path))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var report Failure
	require.NoError(t, json.Unmarshal(data, &report))
	require.Equal(t, failure.Reason, report.Reason)
	require.LessOrEqual(t, len(data), 4096)
	require.NoError(t, failure.WriteTerminationMessage(""))
}
