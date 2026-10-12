package activation

import (
	"context"
	"fmt"

	"github.com/kubeflow/hub/internal/datastore/embedmd"
	"github.com/kubeflow/hub/internal/platform/datastore"
	"github.com/kubeflow/hub/internal/platform/db/postgres"
	"gorm.io/gorm"
)

// Connect stages a candidate in a private schema. Both connection pools use the
// existing libpq environment/TLS configuration; search_path has no public fallback.
func Connect(ctx context.Context, id Identity) (*gorm.DB, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	bootstrap, err := postgres.NewPostgresDBConnector("", nil).ConnectContext(ctx)
	if err != nil {
		return nil, err
	}
	pool, err := bootstrap.DB()
	if err != nil {
		return nil, err
	}
	defer func() { _ = pool.Close() }()
	if err := bootstrap.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", id.SchemaName()).Error; err != nil {
			return err
		}
		return tx.Exec("CREATE SCHEMA IF NOT EXISTS " + id.SchemaName()).Error
	}); err != nil {
		return nil, err
	}
	return postgres.NewPostgresDBConnector("search_path="+id.SchemaName(), nil).ConnectContext(ctx)
}

// Activate commits the complete required dataset and its success marker in one
// transaction. A process crash, failed write, cancellation or superseded intent
// rolls back the entire candidate. Concurrent replicas serialize on this schema.
func Activate(ctx context.Context, db *gorm.DB, spec *datastore.Spec, id Identity, configs, benchmarks []string, current func(context.Context) error) error {
	if err := id.Validate(); err != nil {
		return err
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", id.SchemaName()).Error; err != nil {
			return err
		}
		// IF NOT EXISTS alone does not serialize PostgreSQL catalog writes.
		// Create the marker table only after acquiring the attempt lock.
		if err := tx.Exec(`CREATE TABLE IF NOT EXISTS catalog_activation (attempt_id text PRIMARY KEY, activated_at timestamptz NOT NULL DEFAULT now())`).Error; err != nil {
			return err
		}
		if err := current(ctx); err != nil {
			return err
		}
		var count int64
		if err := tx.Table("catalog_activation").Where("attempt_id = ?", id.AttemptID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return nil
		}
		repos, err := embedmd.Repositories(tx, spec)
		if err != nil {
			return err
		}
		if err := ImportRequired(ctx, configs, benchmarks, repos); err != nil {
			return err
		}
		if err := current(ctx); err != nil {
			return err
		}
		return tx.Exec("INSERT INTO catalog_activation (attempt_id) VALUES (?)", id.AttemptID).Error
	})
}

// Committed is checked for every data request: a stale success report cannot
// authorize an emptyDir database which has since disappeared or been recreated.
func Committed(ctx context.Context, db *gorm.DB, id Identity) error {
	var count int64
	if err := db.WithContext(ctx).Table("catalog_activation").Where("attempt_id = ?", id.AttemptID).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("current dataset has no committed activation")
	}
	return nil
}
