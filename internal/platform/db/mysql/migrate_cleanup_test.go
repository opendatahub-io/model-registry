package mysql

import (
	"context"
	"github.com/stretchr/testify/require"
	contmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"testing"
)

func TestMigratorCleanupOwnedAndBorrowed(t *testing.T) {
	container, err := contmysql.Run(t.Context(), "mysql:8.3", contmysql.WithDatabase("cleanup"), contmysql.WithUsername("root"), contmysql.WithPassword("root"))
	require.NoError(t, err)
	defer func() { require.NoError(t, container.Terminate(context.Background())) }()
	dsn, err := container.ConnectionString(t.Context())
	require.NoError(t, err)
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	defer func() { require.NoError(t, pool.Close()) }()
	for _, owned := range []bool{true, false} {
		t.Run(map[bool]string{true: "owned", false: "borrowed"}[owned], func(t *testing.T) {
			handle := db.Session(&gorm.Session{NewDB: true})
			if !owned {
				dialector := *db.Dialector.(*gormmysql.Dialector)
				config := *dialector.Config
				config.DSNConfig = nil
				dialector.Config = &config
				handle.Dialector = &dialector
			}
			baseline := pool.Stats().InUse
			migrator, err := NewMySQLMigrator(handle)
			require.NoError(t, err)
			dedicated := migrator.conn
			if owned {
				require.NotNil(t, dedicated)
				require.Equal(t, 1, dedicated.Stats().InUse)
			} else {
				require.Equal(t, baseline+1, pool.Stats().InUse)
			}
			require.NoError(t, migrator.Close())
			require.NoError(t, migrator.Close())
			if owned {
				require.Equal(t, 0, dedicated.Stats().InUse)
				require.Error(t, dedicated.Ping())
			} else {
				require.Equal(t, baseline, pool.Stats().InUse)
			}
			require.NoError(t, pool.Ping(), "borrowed application pool must remain open")
		})
	}
}
