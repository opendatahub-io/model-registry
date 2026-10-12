package activation

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/kubeflow/hub/catalog/internal/db/service"
	_ "github.com/kubeflow/hub/catalog/internal/plugins/agent"
	_ "github.com/kubeflow/hub/catalog/internal/plugins/mcp"
	_ "github.com/kubeflow/hub/catalog/internal/plugins/model"
	_ "github.com/kubeflow/hub/catalog/internal/plugins/serving_runtime"
	"github.com/kubeflow/hub/internal/datastore/embedmd"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Real PostgreSQL is essential: repository Save methods use nested transactions,
// and the marker must share their rollback boundary across all source types.
func TestDatabaseActivationAtomicityAndRecovery(t *testing.T) {
	require.NoError(t, testutils.SetupSharedPostgres())
	defer testutils.CleanupSharedPostgres()
	dsn, err := url.Parse(testutils.GetSharedPostgresDSN(t))
	require.NoError(t, err)
	t.Setenv("PGHOST", dsn.Hostname())
	t.Setenv("PGPORT", dsn.Port())
	t.Setenv("PGUSER", dsn.User.Username())
	password, _ := dsn.User.Password()
	t.Setenv("PGPASSWORD", password)
	t.Setenv("PGDATABASE", dsn.Path[1:])
	t.Setenv("PGSSLMODE", "disable")
	ctx := context.Background()
	spec, err := service.DatastoreSpec()
	require.NoError(t, err)
	open := func(id Identity) *gorm.DB {
		db, err := Connect(ctx, id)
		require.NoError(t, err)
		pool, err := db.DB()
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, pool.Close()) })
		ds, err := embedmd.NewEmbedMDService(&embedmd.EmbedMDConfig{DB: db})
		require.NoError(t, err)
		_, err = ds.Initialize(ctx, spec)
		require.NoError(t, err)
		return db
	}
	root := t.TempDir()
	config := filepath.Join(root, "sources.yaml")
	writeFile(t, config, "model_catalogs:\n- id: models\n  type: yaml\n  properties:\n    yamlCatalogPath: models.yaml\nmcp_catalogs:\n- id: mcp\n  type: yaml\n  properties:\n    yamlCatalogPath: mcp.yaml\nagent_catalogs:\n- id: agents\n  type: yaml\n  properties:\n    yamlCatalogPath: agents.yaml\nserving_runtime_catalogs:\n- id: runtimes\n  type: yaml\n  properties:\n    yamlCatalogPath: runtimes.yaml\n")
	writeFile(t, filepath.Join(root, "models.yaml"), "models:\n- name: model\n  artifacts:\n  - uri: oci://registry/model:v1\n")
	writeFile(t, filepath.Join(root, "mcp.yaml"), "mcp_servers:\n- name: server\n  version: v1\n  tools:\n  - name: tool\n- name: server\n  version: v2\n")
	writeFile(t, filepath.Join(root, "agents.yaml"), "agents:\n- name: agent\n  templates:\n  - name: template.yaml\n    content: config\n")
	writeFile(t, filepath.Join(root, "runtimes.yaml"), "serving_runtimes:\n- name: vllm\n  versions:\n  - version: '1'\n    image: registry.example/vllm:1\n")
	benchmarks := filepath.Join(root, "benchmarks")
	writeFile(t, filepath.Join(benchmarks, "model", "metadata.json"), `{"id":"model"}`)
	writeFile(t, filepath.Join(benchmarks, "model", "performance.ndjson"), `{"id":"perf","model_id":"model","throughput":1}`)
	writeFile(t, filepath.Join(benchmarks, "model", "evaluations.ndjson"), `{"model_id":"model","benchmark":"accuracy","score":0.5}`)
	require.Nil(t, Preflight([]string{config}, []string{benchmarks}, true))
	id := testIdentity()
	db := open(id)
	calls := 0
	superseded := errors.New("superseded before commit")
	err = Activate(ctx, db, spec, id, []string{config}, []string{benchmarks}, func(context.Context) error {
		calls++
		if calls == 2 {
			return superseded
		}
		return nil
	})
	require.ErrorIs(t, err, superseded)
	var count int64
	require.NoError(t, db.Table(`"Context"`).Count(&count).Error)
	require.Zero(t, count)
	require.Error(t, Committed(ctx, db, id))
	current := func(context.Context) error { return nil }
	require.NoError(t, Activate(ctx, db, spec, id, []string{config}, []string{benchmarks}, current))
	require.NoError(t, Committed(ctx, db, id))
	require.NoError(t, db.Table(`"Context"`).Count(&count).Error)
	require.Greater(t, count, int64(3))
	before := count
	// Restart is idempotent and does not need to re-read the image content.
	require.NoError(t, Activate(ctx, db, spec, id, []string{"missing-on-restart"}, nil, current))
	require.NoError(t, db.Table(`"Context"`).Count(&count).Error)
	require.Equal(t, before, count)
	// Benchmark writes belong to the same transaction, including failures after
	// some model writes have already succeeded.
	broken := id
	broken.AttemptID = "broken-benchmark"
	brokenDB := open(broken)
	writeFile(t, filepath.Join(benchmarks, "model", "performance.ndjson"), "{broken")
	require.Error(t, Activate(ctx, brokenDB, spec, broken, []string{config}, []string{benchmarks}, current))
	require.NoError(t, brokenDB.Table(`"Context"`).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, brokenDB.Table(`"Artifact"`).Count(&count).Error)
	require.Zero(t, count)
	writeFile(t, filepath.Join(benchmarks, "model", "performance.ndjson"), `{"id":"perf","model_id":"model","throughput":1}`)
	// A failed replacement keeps all last-success data and rolls back its own writes.
	next := id
	next.AttemptID = "next"
	candidate := open(next)
	writeFile(t, filepath.Join(root, "mcp.yaml"), "mcp_servers:\n- name: broken\n  deploymentMode: remote\n")
	require.Error(t, Activate(ctx, candidate, spec, next, []string{config}, []string{benchmarks}, current))
	require.Error(t, Committed(ctx, candidate, next))
	require.NoError(t, candidate.Table(`"Context"`).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Table(`"Context"`).Count(&count).Error)
	require.Equal(t, before, count)
	require.NoError(t, Committed(ctx, db, id))
	// Replicas racing to activate one attempt share exactly one commit.
	writeFile(t, filepath.Join(root, "mcp.yaml"), "mcp_servers: []\n")
	parallel := id
	parallel.AttemptID = "parallel"
	replica1 := open(parallel)
	replica2 := open(parallel)
	outcomes := make(chan error, 2)
	for _, replica := range []*gorm.DB{replica1, replica2} {
		go func(db *gorm.DB) {
			outcomes <- Activate(ctx, db, spec, parallel, []string{config}, []string{benchmarks}, current)
		}(replica)
	}
	require.NoError(t, <-outcomes)
	require.NoError(t, <-outcomes)
	require.NoError(t, replica1.Table("catalog_activation").Count(&count).Error)
	require.Equal(t, int64(1), count)
	// Losing the database marker revokes serving, even with an old success report.
	require.NoError(t, db.Exec("DELETE FROM catalog_activation").Error)
	require.Error(t, Committed(ctx, db, id))
}
