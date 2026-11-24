package postgres_test

import (
	"strings"
	"testing"

	platformpostgres "github.com/kubeflow/hub/internal/platform/db/postgres"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gorm_postgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// newSecondaryPostgresDB creates a dedicated database on the shared
// PostgreSQL container so migration failure paths can leave dirty state
// without affecting other tests. The database is dropped on cleanup.
func newSecondaryPostgresDB(t *testing.T, name string) *gorm.DB {
	t.Helper()

	require.NoError(t, sharedDB.Exec(`CREATE DATABASE "`+name+`"`).Error)
	t.Cleanup(func() {
		_ = sharedDB.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`).Error
	})

	dsn := postgresContainer.MustConnectionString(t.Context(), "sslmode=disable")
	// Replace the database path in the URL-form connection string.
	const testDB = "/testdb?"
	require.Contains(t, dsn, testDB)
	db, err := gorm.Open(gorm_postgres.Open(strings.Replace(dsn, testDB, "/"+name+"?", 1)), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// TestMLflowArtifactLinkMigrationRejectsOrphans proves a PostgreSQL
// upgrade containing orphaned artifact links fails with a clear error
// instead of silently deleting them.
func TestMLflowArtifactLinkMigrationRejectsOrphans(t *testing.T) {
	db := newSecondaryPostgresDB(t, "mlflow_orphan_migration_test")

	migrator, err := platformpostgres.NewPostgresMigrator(db)
	require.NoError(t, err)
	require.NoError(t, migrator.Migrate())

	// Step back to version 29, before the link foreign keys, where orphaned
	// links are still insertable.
	for {
		var current int
		require.NoError(t, db.Raw("SELECT version FROM schema_migrations").Scan(&current).Error)
		if current <= 29 {
			require.Equal(t, 29, current)
			break
		}
		oneStep := -1
		require.NoError(t, migrator.Down(&oneStep))
	}

	require.NoError(t, db.Exec(
		`INSERT INTO "MLflowObject" (kind, parent_id, name, create_time_since_epoch, last_update_time_since_epoch, committed_json, revision, deleted) VALUES ('model-artifact', 0, 'orphan-artifact', 0, 0, '{}', 0, false)`).Error)
	require.NoError(t, db.Exec(
		`INSERT INTO "MLflowArtifactLink" (artifact_id, version_id) VALUES (999999, 999998)`).Error,
		"orphan links must be insertable at version 29 to set up the upgrade")

	err = migrator.Migrate()
	require.Error(t, err, "an upgrade containing orphaned links must fail")

	// The orphaned row survives: no migration ever deletes data.
	var count int64
	require.NoError(t, db.Raw(`SELECT COUNT(*) FROM "MLflowArtifactLink" WHERE artifact_id = 999999`).Scan(&count).Error)
	assert.Equal(t, int64(1), count, "the orphaned link must be preserved for operator repair")
}

// TestMLflowArtifactScopeMigration upgrades legacy identities without rewriting
// snapshots, operation evidence, timestamps, or the retained shared links.
func TestMLflowArtifactScopeMigration(t *testing.T) {
	db := newSecondaryPostgresDB(t, "mlflow_artifact_scope_upgrade")
	migrator, err := platformpostgres.NewPostgresMigrator(db)
	require.NoError(t, err)
	require.NoError(t, migrator.Migrate())
	for {
		var version int
		require.NoError(t, db.Raw("SELECT version FROM schema_migrations").Scan(&version).Error)
		if version == 31 {
			break
		}
		step := -1
		require.NoError(t, migrator.Down(&step))
	}
	require.NoError(t, db.Exec("INSERT INTO \"MLflowObject\" (id, kind, parent_id, name, create_time_since_epoch, last_update_time_since_epoch, committed_json, revision, deleted, visibility) VALUES\n (100, 'registered-model', 0, 'upgrade-model', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (101, 'model-version', 100, '1', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (102, 'model-version', 100, '2', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (110, 'model-artifact', 0, 'standalone', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (111, 'model-artifact', 0, 'one-parent', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (112, 'model-artifact', 0, 'shared', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (113, 'model-artifact', 0, 'native-upgrade-model-1', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (114, 'model-artifact', 0, 'reserved', 10, 20, '{}', 0, FALSE, 'RESERVED'),\n (115, 'model-artifact', 0, 'in-flight', 10, 20, '{}', 0, FALSE, 'RESERVED')").Error)
	require.NoError(t, db.Exec("INSERT INTO \"MLflowArtifactLink\" (artifact_id, version_id) VALUES (111,101),(112,101),(112,102),(113,101)").Error)
	require.NoError(t, db.Exec("INSERT INTO \"MLflowOperation\" (operation_uuid, resource_id, kind, expected_revision, candidate_json, payload_hash, generation, step, state, create_time_since_epoch, last_update_time_since_epoch) VALUES ('upgrade-in-flight',115,'version-materialize',0,'{\"codecVersion\":2,\"objects\":[{\"id\":115,\"expectedRevision\":0,\"snapshot\":{\"codecVersion\":2,\"kind\":\"model-artifact\",\"resource\":{\"name\":\"in-flight\",\"uri\":\"s3://bucket/in-flight.onnx\"}}}],\"links\":[{\"ArtifactID\":115,\"VersionID\":102}]}','preserved',1,'CREATE_DISPATCHED','RUNNING',10,20)").Error)

	require.NoError(t, db.Exec("INSERT INTO \"MLflowVersionBinding\" (version_id,model_id,primary_artifact_id,phase,native_model_name,native_version_number,active_generation,desired_generation,alias,revision) VALUES (101,100,113,'READY','upgrade-model','1',1,1,'v-1',4)").Error)
	require.NoError(t, db.Exec("INSERT INTO \"MLflowVersionHistory\" (version_id,generation,native_model_name,native_version_number,source,operation_uuid,superseded) VALUES (101,1,'upgrade-model','1','s3://bucket/legacy.onnx','import',FALSE)").Error)
	var oldCandidate string
	require.NoError(t, db.Raw("SELECT candidate_json FROM \"MLflowOperation\" WHERE operation_uuid = 'upgrade-in-flight'").Scan(&oldCandidate).Error)
	require.NoError(t, migrator.Migrate())
	var rows []struct {
		ID                   int
		ArtifactScopeID      int32
		Name                 string
		CreateTimeSinceEpoch int64
		Revision             int64
		CommittedJSON        string
	}
	require.NoError(t, db.Raw("SELECT id, artifact_scope_id, name, create_time_since_epoch, revision, committed_json FROM \"MLflowObject\" WHERE id >= 110 ORDER BY id").Scan(&rows).Error)
	require.Len(t, rows, 6)
	for i, expected := range []int32{0, 0, 0, 0, 0, 0} {
		require.Equal(t, 110+i, rows[i].ID)
		require.Equal(t, expected, rows[i].ArtifactScopeID)
		require.Equal(t, int64(10), rows[i].CreateTimeSinceEpoch)
		require.Equal(t, "{}", rows[i].CommittedJSON)
	}
	var links int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM \"MLflowArtifactLink\"").Scan(&links).Error)
	require.Equal(t, int64(4), links)
	var hash string
	require.NoError(t, db.Raw("SELECT payload_hash FROM \"MLflowOperation\" WHERE operation_uuid = 'upgrade-in-flight'").Scan(&hash).Error)
	require.Equal(t, "preserved", hash)

	var newCandidate string
	require.NoError(t, db.Raw("SELECT candidate_json FROM \"MLflowOperation\" WHERE operation_uuid = 'upgrade-in-flight'").Scan(&newCandidate).Error)
	require.Equal(t, oldCandidate, newCandidate)
	var primary int
	require.NoError(t, db.Raw("SELECT primary_artifact_id FROM \"MLflowVersionBinding\" WHERE version_id = 101").Scan(&primary).Error)
	require.Equal(t, 113, primary)
	var historyOperation string
	require.NoError(t, db.Raw("SELECT operation_uuid FROM \"MLflowVersionHistory\" WHERE version_id = 101").Scan(&historyOperation).Error)
	require.Equal(t, "import", historyOperation)
	// Target the artifact-scope migration even after newer migrations exist.
	for {
		var current int
		require.NoError(t, db.Raw("SELECT version FROM schema_migrations").Scan(&current).Error)
		if current == 32 {
			break
		}
		step := -1
		require.NoError(t, migrator.Down(&step))
	}
	// A nonempty database with globally unique legacy names can revert and
	// upgrade again without touching IDs, links, binding/history or evidence.
	reverse := -1
	require.NoError(t, migrator.Down(&reverse))
	require.NoError(t, migrator.Migrate())
	for {
		var current int
		require.NoError(t, db.Raw("SELECT version FROM schema_migrations").Scan(&current).Error)
		if current == 32 {
			break
		}
		step := -1
		require.NoError(t, migrator.Down(&step))
	}
	// Scope is independent of external-ID uniqueness and exact-name collation.
	require.NoError(t, db.Exec("INSERT INTO \"MLflowObject\" (id,kind,parent_id,artifact_scope_id,name,create_time_since_epoch,last_update_time_since_epoch,committed_json,revision,deleted) VALUES (120,'model-artifact',0,102,'one-parent',0,0,'{}',0,FALSE),(121,'model-artifact',0,102,'One-parent',0,0,'{}',0,FALSE),(122,'model-artifact',0,102,'one-parent ',0,0,'{}',0,FALSE)").Error)
	step := -1
	require.Error(t, migrator.Down(&step), "down must refuse identities that would collapse")
	var count int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM \"MLflowObject\" WHERE id >= 110").Scan(&count).Error)
	require.Equal(t, int64(9), count, "refusing down preserves every identity")
}
