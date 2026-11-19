package mysql

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sync"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	platformdb "github.com/kubeflow/hub/internal/platform/db"
	"github.com/kubeflow/hub/internal/platform/db/types"
	gorm_mysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

//go:embed migrations/*.sql
var migrations embed.FS

const (
	MigrationDir = "migrations"
)

func init() {
	platformdb.RegisterMigratorFactory(types.DatabaseTypeMySQL, func(db *gorm.DB) (platformdb.DBMigrator, error) {
		return NewMySQLMigrator(db)
	})
}

type MySQLMigrator struct {
	migrator *migrate.Migrate
	// conn is a dedicated connection owned by the migrator when the
	// caller's pool cannot run multi-statement migrations; nil when the
	// caller's pool is shared.
	conn      *sql.DB
	closeOnce sync.Once
	closeErr  error
}

// NewMySQLMigrator builds a migrator over an existing gorm connection. The
// golang-migrate MySQL driver executes each migration file as a single
// multi-statement ExecContext, which requires MultiStatements=true on the
// connection; gorm's pool does not set it. When the dialector's DSN is
// available, the migrator opens one dedicated multi-statement connection
// (reusing any registered TLS configuration) so guarded multi-statement
// migrations run; otherwise it shares the caller's pool, which supports
// single-statement migrations only.
func NewMySQLMigrator(db *gorm.DB) (*MySQLMigrator, error) {
	sqlDB, owned, err := migrationConnection(db)
	if err != nil {
		return nil, err
	}

	connection, err := sqlDB.Conn(context.Background())
	if err != nil {
		if owned {
			_ = sqlDB.Close()
		}
		return nil, err
	}
	driver, err := mysql.WithConnection(context.Background(), connection, &mysql.Config{})
	if err != nil {
		_ = connection.Close()
		if owned {
			_ = sqlDB.Close()
		}
		return nil, err
	}

	// Create a new source instance from the embedded files
	source, err := iofs.New(migrations, MigrationDir)
	if err != nil {
		_ = driver.Close()
		if owned {
			_ = sqlDB.Close()
		}
		return nil, err
	}

	m, err := migrate.NewWithInstance(
		"iofs",
		source,
		"mysql",
		driver,
	)

	if err != nil {
		_ = source.Close()
		_ = driver.Close()
		if owned {
			_ = sqlDB.Close()
		}
		return nil, err
	}

	migrator := &MySQLMigrator{
		migrator: m,
	}
	if owned {
		migrator.conn = sqlDB
	}
	return migrator, nil
}

// migrationConnection returns the database handle for migrations and
// whether the caller owns (and must eventually close) it.
func migrationConnection(db *gorm.DB) (*sql.DB, bool, error) {
	dialector, ok := db.Dialector.(*gorm_mysql.Dialector)
	if !ok || dialector.DSNConfig == nil {
		sqlDB, err := db.DB()
		if err != nil {
			return nil, false, err
		}
		return sqlDB, false, nil
	}

	// Clone the parsed DSN (preserving any registered TLS configuration)
	// and enable multi-statement execution for migration files only; the
	// production pool keeps its safer single-statement default.
	config := *dialector.DSNConfig
	config.MultiStatements = true
	conn, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return nil, false, err
	}
	return conn, true, nil
}

// Close returns the driver connection and closes any separately owned pool.
// The application pool remains open when it was borrowed.
func (m *MySQLMigrator) Close() error {
	m.closeOnce.Do(func() {
		sourceErr, driverErr := m.migrator.Close()
		m.closeErr = errors.Join(sourceErr, driverErr)
		if m.conn != nil {
			m.closeErr = errors.Join(m.closeErr, m.conn.Close())
		}
	})
	return m.closeErr
}

func (m *MySQLMigrator) Migrate() error {
	if err := m.Up(nil); err != nil && err != migrate.ErrNoChange {
		return err
	}

	return nil
}

func (m *MySQLMigrator) Up(steps *int) error {
	if steps == nil {
		return m.migrator.Up()
	}

	if *steps < 0 {
		return fmt.Errorf("steps cannot be negative")
	}

	return m.migrator.Steps(*steps)
}

func (m *MySQLMigrator) Down(steps *int) error {
	if steps == nil {
		return m.migrator.Down()
	}

	if *steps > 0 {
		return fmt.Errorf("steps cannot be positive")
	}

	return m.migrator.Steps(*steps)
}
