package mysql_test

import (
	"testing"

	sqlmysql "github.com/go-sql-driver/mysql"
	"github.com/kubeflow/hub/internal/platform/db/mysql"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gorm_mysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// newSecondaryMySQLDB creates a dedicated database on the shared MySQL
// container so migration failure paths can leave dirty state without
// affecting other tests. The database is dropped on cleanup.
func newSecondaryMySQLDB(t *testing.T, name string) *gorm.DB {
	t.Helper()

	sharedDB, cleanup := testutils.GetSharedMySQLDB(t)
	t.Cleanup(cleanup)

	require.NoError(t, sharedDB.Exec("CREATE DATABASE IF NOT EXISTS `"+name+"`").Error)
	t.Cleanup(func() {
		_ = sharedDB.Exec("DROP DATABASE IF EXISTS `" + name + "`").Error
	})

	cfg, err := sqlmysql.ParseDSN(testutils.GetSharedMySQLDSN(t))
	require.NoError(t, err)
	cfg.DBName = name

	db, err := gorm.Open(gorm_mysql.Open(cfg.FormatDSN()), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// migrationVersion reads the recorded schema version and dirty flag.
func migrationVersion(t *testing.T, db *gorm.DB) (int, bool) {
	t.Helper()
	var row struct {
		Version int  `gorm:"column:version"`
		Dirty   bool `gorm:"column:dirty"`
	}
	require.NoError(t, db.Raw("SELECT version, dirty FROM schema_migrations LIMIT 1").Scan(&row).Error)
	return row.Version, row.Dirty
}

// stepDownTo steps the database down one migration at a time until the
// recorded version reaches target.
func stepDownTo(t *testing.T, db *gorm.DB, target int) {
	t.Helper()
	migrator, err := mysql.NewMySQLMigrator(db)
	require.NoError(t, err)
	for {
		version, dirty := migrationVersion(t, db)
		require.False(t, dirty)
		if version <= target {
			require.Equal(t, target, version)
			return
		}
		oneStep := -1
		require.NoError(t, migrator.Down(&oneStep))
	}
}

// TestMLflowIdentityCollationIsNoPad characterizes the identity columns'
// collation: utf8mb4_0900_bin must be in use and must be NO PAD, while the
// previous utf8mb4_bin collation is PAD SPACE ( the defect F7 closed ).
func TestMLflowIdentityCollationIsNoPad(t *testing.T) {
	sharedDB, cleanup := testutils.GetSharedMySQLDB(t)
	defer cleanup()

	migrator, err := mysql.NewMySQLMigrator(sharedDB)
	require.NoError(t, err)
	require.NoError(t, migrator.Migrate())

	var padAttribute string
	require.NoError(t, sharedDB.Raw(
		"SELECT PAD_ATTRIBUTE FROM information_schema.COLLATIONS WHERE COLLATION_NAME = 'utf8mb4_0900_bin'").Scan(&padAttribute).Error)
	assert.Equal(t, "NO PAD", padAttribute, "utf8mb4_0900_bin must compare trailing spaces")

	require.NoError(t, sharedDB.Raw(
		"SELECT PAD_ATTRIBUTE FROM information_schema.COLLATIONS WHERE COLLATION_NAME = 'utf8mb4_bin'").Scan(&padAttribute).Error)
	assert.Equal(t, "PAD SPACE", padAttribute, "the previous collation ignored trailing spaces")

	for _, column := range []string{"name", "external_id", "native_model_name"} {
		var collation string
		require.NoError(t, sharedDB.Raw(
			"SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'MLflowObject' AND COLUMN_NAME = ?", column).Scan(&collation).Error)
		assert.Equal(t, "utf8mb4_0900_bin", collation, "MLflowObject.%s must use the NO PAD collation", column)
	}
}

// TestMLflowArtifactLinkMigrationRejectsOrphans proves an upgrade
// containing orphaned artifact links fails with a clear error instead of
// silently deleting them, and documents MySQL's partial-application
// behavior: migration 000034 ( collation ) stays applied when 000035
// ( foreign keys ) fails, and the version row is left dirty.
func TestMLflowArtifactLinkMigrationRejectsOrphans(t *testing.T) {
	db := newSecondaryMySQLDB(t, "mlflow_orphan_migration_test")

	migrator, err := mysql.NewMySQLMigrator(db)
	require.NoError(t, err)
	require.NoError(t, migrator.Migrate())

	// Return to version 33, before the identity and link migrations, where
	// orphaned links are still insertable.
	stepDownTo(t, db, 33)

	require.NoError(t, db.Exec(
		"INSERT INTO MLflowObject (kind, parent_id, name, create_time_since_epoch, last_update_time_since_epoch, committed_json, revision, deleted) VALUES ('model-artifact', 0, 'orphan-artifact', 0, 0, '{}', 0, 0)").Error)
	require.NoError(t, db.Exec(
		"INSERT INTO MLflowArtifactLink (artifact_id, version_id) VALUES (999999, 999998)").Error,
		"orphan links must be insertable at version 33 to set up the upgrade")

	err = migrator.Migrate()
	require.Error(t, err, "an upgrade containing orphaned links must fail")

	// The failed migration leaves the version row dirty; the operator must
	// repair the data and resolve the dirty version explicitly.
	version, dirty := migrationVersion(t, db)
	assert.Equal(t, 35, version)
	assert.True(t, dirty, "the failed migration must leave a dirty version for the operator to resolve")

	// Earlier applied DDL remains in effect: the collation change from
	// migration 34 survives migration 35's failure.
	var collation string
	require.NoError(t, db.Raw(
		"SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'MLflowObject' AND COLUMN_NAME = 'name'").Scan(&collation).Error)
	assert.Equal(t, "utf8mb4_0900_bin", collation,
		"a failed later migration must not roll back earlier applied DDL")

	// The orphaned row survives: no migration ever deletes data.
	var count int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM MLflowArtifactLink WHERE artifact_id = 999999").Scan(&count).Error)
	assert.Equal(t, int64(1), count, "the orphaned link must be preserved for operator repair")
}

// TestMLflowIdentityCollationDownRefusesCollapsingRows proves the down
// migration for the identity collation refuses to revert to PAD SPACE when
// rows would collapse, losing no data.
func TestMLflowIdentityCollationDownRefusesCollapsingRows(t *testing.T) {
	db := newSecondaryMySQLDB(t, "mlflow_collation_down_test")

	migrator, err := mysql.NewMySQLMigrator(db)
	require.NoError(t, err)
	require.NoError(t, migrator.Migrate())

	// Two rows distinct only by a trailing space: legal under NO PAD.
	require.NoError(t, db.Exec(
		"INSERT INTO MLflowObject (kind, parent_id, name, create_time_since_epoch, last_update_time_since_epoch, committed_json, revision, deleted) VALUES ('registered-model', 0, 'collapse', 0, 0, '{}', 0, 0)").Error)
	require.NoError(t, db.Exec(
		"INSERT INTO MLflowObject (kind, parent_id, name, create_time_since_epoch, last_update_time_since_epoch, committed_json, revision, deleted) VALUES ('registered-model', 0, 'collapse ', 0, 0, '{}', 0, 0)").Error)

	stepDownTo(t, db, 36)

	// Drop the visibility column ( 36.down ) and the link foreign keys
	// ( 35.down ), then attempt the collation revert ( 34.down ): it must
	// fail rather than merge identities.
	oneStep := -1
	require.NoError(t, migrator.Down(&oneStep))
	require.NoError(t, migrator.Down(&oneStep))
	err = migrator.Down(&oneStep)
	require.Error(t, err, "reverting to a PAD SPACE collation must fail when identities would collapse")

	// Both rows survive untouched.
	var count int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM MLflowObject WHERE name IN ('collapse', 'collapse ')").Scan(&count).Error)
	assert.Equal(t, int64(2), count, "the refused down migration must not delete or merge rows")
}

// TestMLflowArtifactScopeMigration upgrades legacy identities without rewriting
// snapshots, operation evidence, timestamps, or the retained shared links.
func TestMLflowArtifactScopeMigration(t *testing.T) {
	db := newSecondaryMySQLDB(t, "mlflow_artifact_scope_upgrade")
	migrator, err := mysql.NewMySQLMigrator(db)
	require.NoError(t, err)
	require.NoError(t, migrator.Migrate())
	for {
		var version int
		require.NoError(t, db.Raw("SELECT version FROM schema_migrations").Scan(&version).Error)
		if version == 36 {
			break
		}
		step := -1
		require.NoError(t, migrator.Down(&step))
	}
	require.NoError(t, db.Exec("INSERT INTO `MLflowObject` (id, kind, parent_id, name, create_time_since_epoch, last_update_time_since_epoch, committed_json, revision, deleted, visibility) VALUES\n (100, 'registered-model', 0, 'upgrade-model', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (101, 'model-version', 100, '1', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (102, 'model-version', 100, '2', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (110, 'model-artifact', 0, 'standalone', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (111, 'model-artifact', 0, 'one-parent', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (112, 'model-artifact', 0, 'shared', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (113, 'model-artifact', 0, 'native-upgrade-model-1', 10, 20, '{}', 3, FALSE, 'COMMITTED'),\n (114, 'model-artifact', 0, 'reserved', 10, 20, '{}', 0, FALSE, 'RESERVED'),\n (115, 'model-artifact', 0, 'in-flight', 10, 20, '{}', 0, FALSE, 'RESERVED')").Error)
	require.NoError(t, db.Exec("INSERT INTO `MLflowArtifactLink` (artifact_id, version_id) VALUES (111,101),(112,101),(112,102),(113,101)").Error)
	require.NoError(t, db.Exec("INSERT INTO `MLflowOperation` (operation_uuid, resource_id, kind, expected_revision, candidate_json, payload_hash, generation, step, state, create_time_since_epoch, last_update_time_since_epoch) VALUES ('upgrade-in-flight',115,'version-materialize',0,'{\"codecVersion\":2,\"objects\":[{\"id\":115,\"expectedRevision\":0,\"snapshot\":{\"codecVersion\":2,\"kind\":\"model-artifact\",\"resource\":{\"name\":\"in-flight\",\"uri\":\"s3://bucket/in-flight.onnx\"}}}],\"links\":[{\"ArtifactID\":115,\"VersionID\":102}]}','preserved',1,'CREATE_DISPATCHED','RUNNING',10,20)").Error)

	require.NoError(t, db.Exec("INSERT INTO `MLflowVersionBinding` (version_id,model_id,primary_artifact_id,phase,native_model_name,native_version_number,active_generation,desired_generation,alias,revision) VALUES (101,100,113,'READY','upgrade-model','1',1,1,'v-1',4)").Error)
	require.NoError(t, db.Exec("INSERT INTO `MLflowVersionHistory` (version_id,generation,native_model_name,native_version_number,source,operation_uuid,superseded) VALUES (101,1,'upgrade-model','1','s3://bucket/legacy.onnx','import',FALSE)").Error)
	var oldCandidate string
	require.NoError(t, db.Raw("SELECT candidate_json FROM `MLflowOperation` WHERE operation_uuid = 'upgrade-in-flight'").Scan(&oldCandidate).Error)
	require.NoError(t, migrator.Migrate())
	var rows []struct {
		ID                   int
		ArtifactScopeID      int32
		Name                 string
		CreateTimeSinceEpoch int64
		Revision             int64
		CommittedJSON        string
	}
	require.NoError(t, db.Raw("SELECT id, artifact_scope_id, name, create_time_since_epoch, revision, committed_json FROM `MLflowObject` WHERE id >= 110 ORDER BY id").Scan(&rows).Error)
	require.Len(t, rows, 6)
	for i, expected := range []int32{0, 0, 0, 0, 0, 0} {
		require.Equal(t, 110+i, rows[i].ID)
		require.Equal(t, expected, rows[i].ArtifactScopeID)
		require.Equal(t, int64(10), rows[i].CreateTimeSinceEpoch)
		require.Equal(t, "{}", rows[i].CommittedJSON)
	}
	var links int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM `MLflowArtifactLink`").Scan(&links).Error)
	require.Equal(t, int64(4), links)
	var hash string
	require.NoError(t, db.Raw("SELECT payload_hash FROM `MLflowOperation` WHERE operation_uuid = 'upgrade-in-flight'").Scan(&hash).Error)
	require.Equal(t, "preserved", hash)

	var newCandidate string
	require.NoError(t, db.Raw("SELECT candidate_json FROM `MLflowOperation` WHERE operation_uuid = 'upgrade-in-flight'").Scan(&newCandidate).Error)
	require.Equal(t, oldCandidate, newCandidate)
	var primary int
	require.NoError(t, db.Raw("SELECT primary_artifact_id FROM `MLflowVersionBinding` WHERE version_id = 101").Scan(&primary).Error)
	require.Equal(t, 113, primary)
	var historyOperation string
	require.NoError(t, db.Raw("SELECT operation_uuid FROM `MLflowVersionHistory` WHERE version_id = 101").Scan(&historyOperation).Error)
	require.Equal(t, "import", historyOperation)
	// Target the artifact-scope migration even after newer migrations exist.
	for {
		var current int
		require.NoError(t, db.Raw("SELECT version FROM schema_migrations").Scan(&current).Error)
		if current == 37 {
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
		if current == 37 {
			break
		}
		step := -1
		require.NoError(t, migrator.Down(&step))
	}
	// Scope is independent of external-ID uniqueness and exact-name collation.
	require.NoError(t, db.Exec("INSERT INTO `MLflowObject` (id,kind,parent_id,artifact_scope_id,name,create_time_since_epoch,last_update_time_since_epoch,committed_json,revision,deleted) VALUES (120,'model-artifact',0,102,'one-parent',0,0,'{}',0,FALSE),(121,'model-artifact',0,102,'One-parent',0,0,'{}',0,FALSE),(122,'model-artifact',0,102,'one-parent ',0,0,'{}',0,FALSE)").Error)
	step := -1
	require.Error(t, migrator.Down(&step), "down must refuse identities that would collapse")
	var count int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM `MLflowObject` WHERE id >= 110").Scan(&count).Error)
	require.Equal(t, int64(9), count, "refusing down preserves every identity")
}
