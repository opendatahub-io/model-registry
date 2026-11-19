package mlflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// adapterSchemaVersion is the compatibility schema generation recorded in
// the adapter singleton. It advances only with a migration that changes
// what existing adapter rows mean.
const adapterSchemaVersion = 2

// RegistryTargetFingerprint canonicalizes a native registry target URL
// into a stable fingerprint binding one compatibility database to one
// MLflow registry. Canonicalization keeps the scheme, lowercased host,
// and cleaned base path; credentials, query strings, and fragments are
// rejected because they are not part of a registry's identity — and
// credentials are never recorded or logged. The fingerprint is the
// SHA-256 of the canonical form.
func RegistryTargetFingerprint(target string) (string, error) {
	parsed, err := client.ParseRegistryURL(target)
	if err != nil {
		return "", err
	}

	host := strings.ToLower(parsed.Hostname())
	if port := parsed.Port(); port != "" {
		isDefault := (parsed.Scheme == "http" && port == "80") ||
			(parsed.Scheme == "https" && port == "443")
		if !isDefault {
			host = host + ":" + port
		}
	}
	base := path.Clean("/" + parsed.Path)
	if base == "/" {
		base = ""
	}

	digest := sha256.Sum256([]byte(parsed.Scheme + "://" + host + base))
	return hex.EncodeToString(digest[:]), nil
}

// InitializeAdapter transactionally initializes — or verifies — the
// adapter singleton binding this database to one native registry
// target. Replicas serialize on the migration table's singleton
// version row, locked in the same transaction, before reading or
// inserting MLflowAdapter; two replicas therefore obtain the same
// identity, and a different registry target fails initialization
// without changing the recorded identity.
func (s *Store) InitializeAdapter(ctx context.Context, fingerprint string) (Adapter, error) {
	if fingerprint == "" {
		return Adapter{}, fmt.Errorf("registry target fingerprint is required: %w", api.ErrBadRequest)
	}

	var adapter Adapter
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Serialize replicas on the migration table's singleton version
		// row before touching the adapter identity. FOR UPDATE on the
		// one row makes concurrent initializers strictly ordered on both
		// engines.
		var singleton struct {
			Version int64
			Dirty   bool
		}
		if err := tx.Table("schema_migrations").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("version, dirty").Limit(1).Scan(&singleton).Error; err != nil {
			return fmt.Errorf("locking the migration singleton row: %w", err)
		}

		err := tx.Order("id").First(&adapter).Error
		if err == nil {
			if adapter.TargetFingerprint != fingerprint {
				return fmt.Errorf("compatibility store is already bound to a different mlflow registry target: %w",
					api.ErrConflict)
			}
			if adapter.SchemaVersion != adapterSchemaVersion || adapter.CodecVersion != candidateCodecVersion {
				return fmt.Errorf("incompatible mlflow adapter schema/codec %d/%d; expected %d/%d (mixed binaries unsupported): %w", adapter.SchemaVersion, adapter.CodecVersion, adapterSchemaVersion, candidateCodecVersion, api.ErrConflict)
			}
			return hydrateLegacyOperationGraph(tx)
		}
		if err != gorm.ErrRecordNotFound {
			return mapDBError(err)
		}

		adapter = Adapter{
			ID:                1,
			AdapterUUID:       uuid.NewString(),
			TargetFingerprint: fingerprint,
			SchemaVersion:     adapterSchemaVersion,
			CodecVersion:      candidateCodecVersion,
		}
		if !tx.Migrator().HasTable(&IntentResource{}) {
			return fmt.Errorf("mlflow operation graph migration is required: %w", api.ErrConflict)
		}
		if err := tx.Create(&adapter).Error; err != nil {
			return mapDBError(err)
		}
		return hydrateLegacyOperationGraph(tx)
	})
	if err != nil {
		return Adapter{}, err
	}
	return adapter, nil
}

// GetAdapter returns the initialized adapter singleton for read-only
// inspection. An uninitialized store reports not-found.
func (s *Store) GetAdapter(ctx context.Context) (Adapter, error) {
	var adapter Adapter
	err := s.db.WithContext(ctx).Order("id").First(&adapter).Error
	if err != nil {
		return Adapter{}, mapDBError(err)
	}
	return adapter, nil
}
