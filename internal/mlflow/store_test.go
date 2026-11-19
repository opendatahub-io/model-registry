package mlflow_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/platform/db/mysql"
	"github.com/kubeflow/hub/internal/platform/db/postgres"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	contpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/gorm"
)

// storeFixture is a migrated compatibility database on one engine.
type storeFixture struct {
	store *mlflow.Store
	db    *gorm.DB
}

func (f *storeFixture) cleanup(t *testing.T) {
	t.Helper()
	cleanupMLflowTables(t, f.db)
}

func cleanupMLflowTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	// Later graph/disposition migrations reference journal/object rows.
	// Clean these dependants first when their migrations are present.
	for _, table := range []string{"MLflowNativeDisposition", "MLflowIntentResource"} {
		if db.Migrator().HasTable(table) {
			require.NoError(t, db.Table(table).Where("1 = 1").Delete(map[string]any{}).Error,
				"failed to clean table %s", table)
		}
	}
	// Deleted through the GORM models so each dialect quotes the mixed-
	// case table names correctly.
	for _, model := range []any{
		&mlflow.Operation{},
		&mlflow.VersionHistory{},
		&mlflow.VersionBinding{},
		&mlflow.ArtifactLink{},
		&mlflow.Object{},
		&mlflow.Adapter{},
	} {
		require.NoError(t, db.Where("1 = 1").Delete(model).Error, "failed to clean table for %T", model)
	}
}

// pgFixtureContainer is the subset of the PostgreSQL test container the
// fixture setup uses. Failure-path tests substitute fakes that record
// termination.
type pgFixtureContainer interface {
	testcontainers.Container
	ConnectionString(ctx context.Context, args ...string) (string, error)
}

// pgFixtureDeps are the fallible steps of PostgreSQL fixture setup. Each
// production default wraps the real step; failure-path tests inject errors
// to prove setup aborts loudly and every owned resource still cleans up.
type pgFixtureDeps struct {
	run     func(ctx context.Context) (pgFixtureContainer, error)
	dsn     func(ctx context.Context, container pgFixtureContainer) (string, error)
	connect func(dsn string) (*gorm.DB, func() error, error)
	migrate func(ctx context.Context, db *gorm.DB) error
}

func defaultPGFixtureDeps() pgFixtureDeps {
	return pgFixtureDeps{
		run: func(ctx context.Context) (pgFixtureContainer, error) {
			return contpostgres.Run(ctx, "postgres:16",
				contpostgres.WithDatabase("mlflow_store"),
				contpostgres.WithUsername("postgres"),
				contpostgres.WithPassword("postgres"),
				testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
			)
		},
		dsn: func(ctx context.Context, container pgFixtureContainer) (string, error) {
			return container.ConnectionString(ctx, "sslmode=disable")
		},
		connect: func(dsn string) (*gorm.DB, func() error, error) {
			db, err := postgres.NewPostgresDBConnector(dsn, nil).WithMaxRetries(1).Connect()
			if err != nil {
				return nil, nil, err
			}
			return db, func() error {
				sqlDB, err := db.DB()
				if err != nil {
					return err
				}
				return sqlDB.Close()
			}, nil
		},
		migrate: func(ctx context.Context, db *gorm.DB) error {
			return postgres.MigrateContext(ctx, db)
		},
	}
}

// setupPostgresFixture starts the PostgreSQL fixture. It returns an error
// rather than failing the test so failure-path tests can assert on it;
// newStoreFixtures turns any error into a test failure, so a missing
// "postgres" entry can never silently drop coverage. Cleanup for the
// container and connection is retained even when a later setup step fails;
// the connection closes before the container terminates.
func setupPostgresFixture(t *testing.T, deps pgFixtureDeps) (*storeFixture, error) {
	t.Helper()
	fixture, cleanup, err := openPostgresFixture(t.Context(), deps)
	t.Cleanup(func() { assert.NoError(t, cleanup(), "postgres fixture resources must close") })
	return fixture, err
}

// openPostgresFixture transfers every acquired resource to cleanup, including
// when a later setup step fails. Both the per-test failure probes and the
// package session use the same setup/teardown path.
func openPostgresFixture(ctx context.Context, deps pgFixtureDeps) (*storeFixture, func() error, error) {
	var cleanups []func() error
	cleanup := func() error {
		var errs []error
		for i := len(cleanups) - 1; i >= 0; i-- {
			errs = append(errs, cleanups[i]())
		}
		cleanups = nil
		return errors.Join(errs...)
	}
	container, err := deps.run(ctx)
	// A container run can return both a partially started container and an
	// error. Retain ownership even when startup did not finish.
	if container != nil {
		cleanups = append(cleanups, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return container.Terminate(ctx)
		})
	}
	if err != nil {
		return nil, cleanup, fmt.Errorf("starting postgres container: %w", err)
	}

	dsn, err := deps.dsn(ctx, container)
	if err != nil {
		return nil, cleanup, fmt.Errorf("resolving postgres connection string: %w", err)
	}

	db, closeDB, err := deps.connect(dsn)
	if closeDB != nil {
		cleanups = append(cleanups, closeDB)
	}
	if err != nil {
		return nil, cleanup, fmt.Errorf("connecting postgres fixture: %w", err)
	}

	if err := deps.migrate(ctx, db); err != nil {
		return nil, cleanup, fmt.Errorf("migrating postgres fixture: %w", err)
	}

	return &storeFixture{store: mlflow.NewStore(db), db: db}, cleanup, nil
}

// newStoreFixtures returns one fixture per supported database engine. Both
// engines are required: a PostgreSQL setup failure fails the test rather
// than returning a map that silently omits that engine's coverage. Each
// fixture's database has all migrations ( EmbedMD and MLflow adapter )
// applied, exercising coexistence with the existing schema.
func newStoreFixtures(t *testing.T) map[string]*storeFixture {
	t.Helper()

	fixtures := map[string]*storeFixture{}
	// Acquire PostgreSQL first: its test-lifetime lock also protects shared
	// MySQL tables while this test owns either fixture.
	fixtures["postgres"] = sharedPostgresFixture(t)

	mysqlFixture, err := sharedMySQLSession.start(testutils.GetSharedMySQLDSN(t))
	require.NoError(t, err, "MySQL is required for compatibility-store tests")
	cleanupMLflowTables(t, mysqlFixture.db)
	t.Cleanup(func() { cleanupMLflowTables(t, mysqlFixture.db) })
	fixtures["mysql"] = mysqlFixture

	return fixtures
}

// Migration ownership follows the package's shared MySQL container lifetime.
// The current production migrator retains a go-migrate sql.Conn even after
// Close, so repeated migrations would grow connections until MySQL refused
// new work. One session bounds it; TestMain closes the ordinary query pool
// and then terminates the owned container. The core reference fixture keeps
// its existing EmbedMD type initialization.
type mysqlFixtureSession struct {
	once     sync.Once
	fixture  *storeFixture
	migrator *mysql.MySQLMigrator
	err      error
}

var sharedMySQLSession mysqlFixtureSession

func (s *mysqlFixtureSession) start(dsn string) (*storeFixture, error) {
	s.once.Do(func() {
		db, err := mysql.NewMySQLDBConnector(dsn, nil).Connect()
		if err != nil {
			s.err = err
			return
		}
		s.fixture = &storeFixture{store: mlflow.NewStore(db), db: db}
		s.migrator, s.err = mysql.NewMySQLMigrator(db)
		if s.err == nil {
			s.err = s.migrator.Migrate()
		}
	})
	return s.fixture, s.err
}

func (s *mysqlFixtureSession) close() error {
	var errs []error
	if s.migrator != nil {
		errs = append(errs, s.migrator.Close())
		s.migrator = nil
	}
	if s.fixture != nil {
		db, err := s.fixture.db.DB()
		if err == nil {
			err = db.Close()
		}
		errs = append(errs, err)
		s.fixture = nil
	}
	return errors.Join(errs...)
}

// fakePGContainer records Terminate calls so failure-path tests can prove
// an owned container still cleans up when a later setup step aborts.
type fakePGContainer struct {
	testcontainers.Container
	terminated chan struct{}
}

func (f *fakePGContainer) Terminate(context.Context, ...testcontainers.TerminateOption) error {
	close(f.terminated)
	return nil
}

func (f *fakePGContainer) ConnectionString(context.Context, ...string) (string, error) {
	return "", nil
}

func newFakePGContainer() *fakePGContainer {
	return &fakePGContainer{terminated: make(chan struct{})}
}

// awaitTerminated reports a test failure unless the fake container was
// terminated. It must be registered with t.Cleanup before the fixture setup
// runs so it executes after the fixture's own cleanup ( LIFO ).
func awaitTerminated(t *testing.T, container *fakePGContainer) {
	t.Helper()
	select {
	case <-container.terminated:
	case <-time.After(30 * time.Second):
		t.Error("owned postgres container was not terminated after a setup failure")
	}
}

// awaitClosed reports a test failure unless the connection close function
// ran. Like awaitTerminated, it runs from cleanup registered before setup.
func awaitClosed(t *testing.T, closed chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Error("owned connection was not closed after a setup failure")
	}
}

// TestStoreFixturePostgresFailurePaths proves a PostgreSQL setup failure
// fails the fixture ( and therefore the test ) instead of dropping engine
// coverage, and that every started container and connection is cleaned up
// even when setup aborts partway.
func TestStoreFixturePostgresFailurePaths(t *testing.T) {
	injectFailure := func(name string, inject func(deps *pgFixtureDeps, container *fakePGContainer)) {
		t.Run(name, func(t *testing.T) {
			container := newFakePGContainer()
			deps := defaultPGFixtureDeps()
			deps.run = func(context.Context) (pgFixtureContainer, error) {
				return container, nil
			}
			inject(&deps, container)
			// Registered before setup so LIFO runs it after the fixture's
			// terminate/close cleanups.
			t.Cleanup(func() { awaitTerminated(t, container) })

			_, err := setupPostgresFixture(t, deps)
			require.Error(t, err, "a fixture step failure must fail setup, not skip the engine")
		})
	}

	t.Run("container creation failure", func(t *testing.T) {
		deps := defaultPGFixtureDeps()
		deps.run = func(context.Context) (pgFixtureContainer, error) {
			return nil, errors.New("no container runtime available")
		}
		_, err := setupPostgresFixture(t, deps)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "starting postgres container")
	})

	connectionClosed := make(chan struct{})
	injectFailure("dsn failure", func(deps *pgFixtureDeps, _ *fakePGContainer) {
		deps.dsn = func(context.Context, pgFixtureContainer) (string, error) {
			return "", errors.New("dsn resolution failed")
		}
		deps.connect = func(string) (*gorm.DB, func() error, error) {
			close(connectionClosed)
			return nil, nil, errors.New("connect must not run after dsn failure")
		}
	})
	select {
	case <-connectionClosed:
		t.Fatal("connect ran after dsn failure")
	default:
	}

	injectFailure("connect failure", func(deps *pgFixtureDeps, _ *fakePGContainer) {
		deps.connect = func(string) (*gorm.DB, func() error, error) {
			return nil, nil, errors.New("connection refused")
		}
	})

	injectFailure("migration failure", func(deps *pgFixtureDeps, _ *fakePGContainer) {
		closed := make(chan struct{})
		deps.connect = func(string) (*gorm.DB, func() error, error) {
			return nil, func() error { close(closed); return nil }, nil
		}
		deps.migrate = func(context.Context, *gorm.DB) error {
			return errors.New("migration failed")
		}
		t.Cleanup(func() { awaitClosed(t, closed) })
	})
}

func reservedModel(name string) mlflow.Object {
	return mlflow.Object{
		Kind:     mlflow.KindRegisteredModel,
		Name:     name,
		Revision: 1,
	}
}

func TestStoreObjects(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			created, err := fixture.store.ReserveObject(ctx, reservedModel("store-model"))
			require.NoError(t, err)
			assert.NotZero(t, created.ID)
			assert.False(t, created.Deleted)
			assert.NotZero(t, created.CreateTimeSinceEpoch)

			// Duplicate logical names conflict and allocate nothing.
			_, err = fixture.store.ReserveObject(ctx, reservedModel("store-model"))
			require.ErrorIs(t, err, api.ErrConflict)

			// Find by name, by external ID, and by ID.
			found, err := fixture.store.FindObject(ctx, mlflow.KindRegisteredModel, new("store-model"), nil, nil)
			require.NoError(t, err)
			require.Equal(t, created.ID, found.ID)

			ext := "store-ext-1"
			updated := found
			updated.ExternalID = &ext
			require.NoError(t, fixture.db.Save(&updated).Error)
			byExt, err := fixture.store.FindObject(ctx, mlflow.KindRegisteredModel, nil, nil, &ext)
			require.NoError(t, err)
			require.Equal(t, created.ID, byExt.ID)

			got, err := fixture.store.GetObject(ctx, created.ID)
			require.NoError(t, err)
			require.Equal(t, created.ID, got.ID)

			// Unknown IDs and names are not found.
			_, err = fixture.store.GetObject(ctx, 424242)
			require.ErrorIs(t, err, api.ErrNotFound)
			_, err = fixture.store.FindObject(ctx, mlflow.KindRegisteredModel, new("no-such-model"), nil, nil)
			require.ErrorIs(t, err, api.ErrNotFound)

			// Parent links: versions are scoped to their model.
			version, err := fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind:     mlflow.KindModelVersion,
				ParentID: created.ID,
				Name:     "1.0.0",
				Revision: 1,
			})
			require.NoError(t, err)
			require.Equal(t, created.ID, version.ParentID)

			// Version names are unique per parent but may repeat across
			// parents.
			other, err := fixture.store.ReserveObject(ctx, reservedModel("store-model-2"))
			require.NoError(t, err)
			_, err = fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind:     mlflow.KindModelVersion,
				ParentID: created.ID,
				Name:     "1.0.0",
				Revision: 1,
			})
			require.ErrorIs(t, err, api.ErrConflict)
			_, err = fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind:     mlflow.KindModelVersion,
				ParentID: other.ID,
				Name:     "1.0.0",
				Revision: 1,
			})
			require.NoError(t, err)

			versions, err := fixture.store.ListObjects(ctx, mlflow.KindModelVersion, &created.ID)
			require.NoError(t, err)
			require.Len(t, versions, 1)
			allVersions, err := fixture.store.ListObjects(ctx, mlflow.KindModelVersion, nil)
			require.NoError(t, err)
			require.Len(t, allVersions, 2)

			// A parentless version is rejected.
			_, err = fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelVersion, Name: "orphan", Revision: 1,
			})
			require.ErrorIs(t, err, api.ErrBadRequest)

			// Persistence: a brand-new Store over the same database sees
			// identical committed state.
			reopened := mlflow.NewStore(fixture.db)
			rgot, err := reopened.GetObject(ctx, created.ID)
			require.NoError(t, err)
			require.Equal(t, got, rgot)
		})
	}
}

func TestStoreStandaloneArtifact(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			// A model artifact is a parentless object: it exists in the
			// compatibility store before ( and independently of ) any
			// version attachment.
			artifact, err := fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind:          mlflow.KindModelArtifact,
				Name:          "standalone",
				Revision:      1,
				CommittedJSON: `{"name":"standalone","uri":""}`,
			})
			require.NoError(t, err)
			require.Positive(t, artifact.ID)
			require.Zero(t, artifact.ParentID)

			found, err := fixture.store.FindObject(ctx, mlflow.KindModelArtifact, new("standalone"), nil, nil)
			require.NoError(t, err)
			require.Equal(t, artifact.ID, found.ID)

			// Links represent the zero-to-many version relationships of one
			// artifact; the object row itself never carries a parent.
			model, err := fixture.store.ReserveObject(ctx, reservedModel("link-model"))
			require.NoError(t, err)
			first, err := fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: "1.0.0", Revision: 1,
			})
			require.NoError(t, err)
			second, err := fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: "2.0.0", Revision: 1,
			})
			require.NoError(t, err)

			for _, versionID := range []int32{first.ID, second.ID} {
				require.NoError(t, fixture.db.Create(&mlflow.ArtifactLink{
					ArtifactID: artifact.ID, VersionID: versionID,
				}).Error)
			}
			var links []mlflow.ArtifactLink
			require.NoError(t, fixture.db.Where("artifact_id = ?", artifact.ID).Find(&links).Error)
			require.Len(t, links, 2)

			// The artifact/version pair is unique.
			err = fixture.db.Create(&mlflow.ArtifactLink{
				ArtifactID: artifact.ID, VersionID: first.ID,
			}).Error
			require.ErrorIs(t, err, gorm.ErrDuplicatedKey)

			// Artifacts never take a parent through ReserveObject.
			_, err = fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelArtifact, ParentID: model.ID, Name: "attached", Revision: 1,
			})
			require.ErrorIs(t, err, api.ErrBadRequest)
		})
	}
}

func TestStoreAdapterTimestamp(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			fixture.cleanup(t)

			artifact, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{
				Kind: mlflow.KindModelArtifact, Name: "standalone", Revision: 1,
				CommittedJSON: `{"name":"standalone","uri":""}`,
			})
			require.NoError(t, err)
			require.Positive(t, artifact.ID)
			stamp := time.Now().UnixMilli()
			adapter := mlflow.Adapter{
				AdapterUUID:       "d5fba25d-beb4-4fe7-bb37-6255da2dc395",
				TargetFingerprint: "test-target", SchemaVersion: 1, CodecVersion: 1,
			}
			require.NoError(t, fixture.db.Create(&adapter).Error)
			require.NoError(t, fixture.db.Model(&adapter).Update("last_refresh_time", stamp).Error)
			var restored mlflow.Adapter
			require.NoError(t, fixture.db.First(&restored, adapter.ID).Error)
			require.Equal(t, stamp, *restored.LastRefreshTime)

			// A unset refresh time stays NULL through a fresh connection.
			require.NoError(t, fixture.db.Model(&adapter).Update("last_refresh_time", nil).Error)
			var unset mlflow.Adapter
			require.NoError(t, fixture.db.First(&unset, adapter.ID).Error)
			require.Nil(t, unset.LastRefreshTime)
		})
	}
}

func TestStoreParentValidation(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			model, err := fixture.store.ReserveObject(ctx, reservedModel("parent-model"))
			require.NoError(t, err)
			artifact, err := fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelArtifact, Name: "parent-artifact", Revision: 1,
			})
			require.NoError(t, err)

			// Versions require a real registered-model parent.
			_, err = fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelVersion, Name: "orphan", Revision: 1,
			})
			require.ErrorIs(t, err, api.ErrBadRequest)
			_, err = fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelVersion, ParentID: 424242, Name: "unknown-parent", Revision: 1,
			})
			require.ErrorIs(t, err, api.ErrBadRequest)
			_, err = fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelVersion, ParentID: artifact.ID, Name: "wrong-kind-parent", Revision: 1,
			})
			require.ErrorIs(t, err, api.ErrBadRequest)
			_, err = fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: "valid", Revision: 1,
			})
			require.NoError(t, err)

			// Registered models never take a parent.
			_, err = fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindRegisteredModel, ParentID: model.ID, Name: "child-model", Revision: 1,
			})
			require.ErrorIs(t, err, api.ErrBadRequest)

			// Unknown kinds are rejected before any row is written.
			_, err = fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.ObjectKind("serving-runtime"), Name: "bogus", Revision: 1,
			})
			require.ErrorIs(t, err, api.ErrBadRequest)
			var count int64
			require.NoError(t, fixture.db.Model(&mlflow.Object{}).Where("name = ?", "bogus").Count(&count).Error)
			require.Zero(t, count)

			// A deleted parent is not a valid parent.
			deleted := model
			deleted.Deleted = true
			require.NoError(t, fixture.db.Save(&deleted).Error)
			_, err = fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: "deleted-parent", Revision: 1,
			})
			require.ErrorIs(t, err, api.ErrBadRequest)
		})
	}
}

// TestStoreExternalIDScope enforces the reference backend's external-ID
// uniqueness scope: unique across registered models and model versions
// jointly, separately unique within model artifacts, and unrestricted
// while unset.
func TestStoreExternalIDScope(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			modelA, err := fixture.store.ReserveObject(ctx, reservedModel("scope-model-a"))
			require.NoError(t, err)
			modelB, err := fixture.store.ReserveObject(ctx, reservedModel("scope-model-b"))
			require.NoError(t, err)

			// Unset external IDs never conflict.
			_, err = fixture.store.ReserveObject(ctx, reservedModel("scope-model-nil"))
			require.NoError(t, err)

			withExt := modelA
			withExt.ExternalID = new("scope-ext")
			require.NoError(t, fixture.db.Save(&withExt).Error)

			// A second registered model cannot reuse the external ID.
			dupModel := modelB
			dupModel.ExternalID = new("scope-ext")
			err = fixture.db.Create(&dupModel).Error
			require.ErrorIs(t, err, gorm.ErrDuplicatedKey)

			// Model versions share the registered-model scope.
			version, err := fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelVersion, ParentID: modelB.ID, Name: "1.0.0", Revision: 1,
			})
			require.NoError(t, err)
			version.ExternalID = new("scope-version-ext")
			require.NoError(t, fixture.db.Save(&version).Error)

			otherVersion, err := fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelVersion, ParentID: modelA.ID, Name: "1.0.0", Revision: 1,
			})
			require.NoError(t, err)
			otherVersion.ExternalID = new("scope-version-ext")
			err = fixture.db.Save(&otherVersion).Error
			require.ErrorIs(t, err, gorm.ErrDuplicatedKey)

			clashModel := reservedModel("scope-model-clash")
			clashModel.ExternalID = new("scope-version-ext")
			err = fixture.db.Create(&clashModel).Error
			require.ErrorIs(t, err, gorm.ErrDuplicatedKey)

			// Artifacts have their own scope and may reuse a context
			// external ID...
			artifact, err := fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelArtifact, Name: "scope-artifact", Revision: 1,
				CommittedJSON: `{"name":"scope-artifact","uri":""}`,
			})
			require.NoError(t, err)
			artifact.ExternalID = new("scope-ext")
			require.NoError(t, fixture.db.Save(&artifact).Error)

			// ...but not each other's.
			otherArtifact, err := fixture.store.ReserveObject(ctx, mlflow.Object{
				Kind: mlflow.KindModelArtifact, Name: "scope-artifact-2", Revision: 1,
				CommittedJSON: `{"name":"scope-artifact-2","uri":""}`,
			})
			require.NoError(t, err)
			otherArtifact.ExternalID = new("scope-ext")
			err = fixture.db.Save(&otherArtifact).Error
			require.ErrorIs(t, err, gorm.ErrDuplicatedKey)
		})
	}
}

func TestStoreOperations(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			object, err := fixture.store.ReserveObject(ctx, reservedModel("op-model"))
			require.NoError(t, err)

			operation, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "b6589fc6-ab04-42cc-9f8f-5f6a2c0e0f01",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: object.Revision,
				CandidateJSON:    `{"name":"op-model","description":"updated"}`,
				PayloadHash:      "hash-1",
				Generation:       1,
			})
			require.NoError(t, err)
			require.Equal(t, mlflow.OperationPending, operation.State)
			require.Equal(t, mlflow.StepPrepared, operation.Step)
			require.Zero(t, operation.FencingToken)

			// A second intent for the same resource/revision/kind conflicts.
			_, err = fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "b6589fc6-ab04-42cc-9f8f-5f6a2c0e0f02",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: object.Revision,
				CandidateJSON:    `{}`,
				PayloadHash:      "hash-2",
				Generation:       1,
			})
			require.ErrorIs(t, err, api.ErrConflict)

			// Claim mints a fencing token.
			claimed, err := fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-1", 30*time.Second)
			require.NoError(t, err)
			require.Equal(t, mlflow.OperationRunning, claimed.State)
			require.Equal(t, int64(1), claimed.FencingToken)
			require.Equal(t, 1, claimed.Attempts)

			// A live lease held by another worker conflicts.
			_, err = fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-2", 30*time.Second)
			require.ErrorIs(t, err, api.ErrConflict)

			// Checkpoints require the current fencing token.
			err = fixture.store.CheckpointOperation(ctx, operation.OperationUUID, 999, mlflow.StepCreateDispatched, nil)
			require.ErrorIs(t, err, api.ErrConflict)

			require.NoError(t, fixture.store.CheckpointOperation(ctx, operation.OperationUUID, claimed.FencingToken, mlflow.StepCreateDispatched, new("12")))

			// Publication is conditional on the expected revision.
			require.NoError(t, fixture.db.Model(&mlflow.Object{}).
				Where("id = ?", object.ID).
				Update("revision", object.Revision+1).Error)
			err = fixture.store.PublishOperation(ctx, operation.OperationUUID, claimed.FencingToken)
			require.ErrorIs(t, err, api.ErrConflict)

			// The failed publication changed neither object nor operation.
			unchanged, err := fixture.store.GetObject(ctx, object.ID)
			require.NoError(t, err)
			require.Equal(t, object.Revision+1, unchanged.Revision)
			require.NotEqual(t, operation.CandidateJSON, unchanged.CommittedJSON)
			still, err := fixture.store.GetOperation(ctx, operation.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, mlflow.OperationRunning, still.State)

			// A corrupt/stale dispatched graph retains ownership until explicit
			// resolution. A different resource can still publish normally.
			_, err = fixture.store.PrepareOperation(ctx, mlflow.Operation{OperationUUID: "stale-reservation-intruder", ResourceID: object.ID, ExpectedRevision: unchanged.Revision, CandidateJSON: "{}"})
			require.ErrorIs(t, err, api.ErrConflict)
			object, err = fixture.store.ReserveObject(ctx, reservedModel("op-model-independent"))
			require.NoError(t, err)
			unchanged = object
			second, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "b6589fc6-ab04-42cc-9f8f-5f6a2c0e0f03",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update-2",
				ExpectedRevision: unchanged.Revision,
				CandidateJSON:    `{"name":"op-model","description":"updated"}`,
				PayloadHash:      "hash-1",
				Generation:       2,
			})
			require.NoError(t, err)
			secondClaim, err := fixture.store.ClaimOperation(ctx, second.OperationUUID, "worker-1", 30*time.Second)
			require.NoError(t, err)
			require.NoError(t, fixture.store.PublishOperation(ctx, second.OperationUUID, secondClaim.FencingToken))

			published, err := fixture.store.GetObject(ctx, object.ID)
			require.NoError(t, err)
			require.Equal(t, second.CandidateJSON, published.CommittedJSON)
			require.Equal(t, unchanged.Revision+1, published.Revision)

			done, err := fixture.store.GetOperation(ctx, second.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, mlflow.OperationSucceeded, done.State)
			require.Equal(t, mlflow.StepPublished, done.Step)

			// Completed operations cannot be re-claimed.
			_, err = fixture.store.ClaimOperation(ctx, second.OperationUUID, "worker-1", 30*time.Second)
			require.ErrorIs(t, err, api.ErrConflict)
		})
	}
}

// TestStoreReclaimPreservesCheckpoint proves reclaiming an expired lease
// never erases execution progress: the persisted step, native result,
// immutable candidate, and generation survive reclamation, while the
// fencing token increases so the previous worker is fenced out.
func TestStoreReclaimPreservesCheckpoint(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			steps := []string{
				mlflow.StepCreateDispatched,
				mlflow.StepNativeCreated,
				mlflow.StepTagsApplied,
				mlflow.StepAliasMoved,
			}
			for i, step := range steps {
				object, err := fixture.store.ReserveObject(ctx, reservedModel(fmt.Sprintf("reclaim-model-%d", i)))
				require.NoError(t, err)

				op, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
					OperationUUID:    fmt.Sprintf("e6589fc6-ab04-42cc-9f8f-5f6a2c0e0f0%d", i),
					ResourceID:       object.ID,
					Kind:             "version-materialize",
					ExpectedRevision: object.Revision,
					CandidateJSON:    `{"name":"reclaim-model","uri":"s3://bucket/model.onnx"}`,
					PayloadHash:      "hash-1",
					Generation:       3,
				})
				require.NoError(t, err)

				first, err := fixture.store.ClaimOperation(ctx, op.OperationUUID, "worker-1", time.Minute)
				require.NoError(t, err)
				require.NoError(t, fixture.store.CheckpointOperation(ctx, op.OperationUUID,
					first.FencingToken, step, new("17")))

				// Expire the lease directly and reclaim from a fresh store,
				// as a restarted worker would.
				require.NoError(t, fixture.db.Model(&mlflow.Operation{}).
					Where("operation_uuid = ?", op.OperationUUID).
					Update("lease_expires_since_epoch", int64(0)).Error)
				reopened := mlflow.NewStore(fixture.db)
				reclaimed, err := reopened.ClaimOperation(ctx, op.OperationUUID, "worker-2", time.Minute)
				require.NoError(t, err)
				require.Equal(t, step, reclaimed.Step)
				require.NotNil(t, reclaimed.NativeResult)
				require.Equal(t, "17", *reclaimed.NativeResult)
				require.Equal(t, op.CandidateJSON, reclaimed.CandidateJSON)
				require.Equal(t, int64(3), reclaimed.Generation)
				require.Greater(t, reclaimed.FencingToken, first.FencingToken)
				require.Equal(t, 2, reclaimed.Attempts)
				require.Equal(t, "worker-2", *reclaimed.LeaseOwner)
			}
		})
	}
}

func TestStoreHeartbeat(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			object, err := fixture.store.ReserveObject(ctx, reservedModel("heartbeat-model"))
			require.NoError(t, err)
			op, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "f6589fc6-ab04-42cc-9f8f-5f6a2c0e0f01",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: object.Revision,
				CandidateJSON:    `{"name":"heartbeat-model"}`,
				PayloadHash:      "hash-1",
				Generation:       1,
			})
			require.NoError(t, err)

			// Nonpositive lease durations are rejected for both claim and
			// heartbeat.
			_, err = fixture.store.ClaimOperation(ctx, op.OperationUUID, "worker-1", 0)
			require.ErrorIs(t, err, api.ErrBadRequest)
			err = fixture.store.HeartbeatOperation(ctx, op.OperationUUID, 1, -time.Second)
			require.ErrorIs(t, err, api.ErrBadRequest)

			claimed, err := fixture.store.ClaimOperation(ctx, op.OperationUUID, "worker-1", time.Second)
			require.NoError(t, err)

			// A valid heartbeat extends the lease.
			before, err := fixture.store.GetOperation(ctx, op.OperationUUID)
			require.NoError(t, err)
			require.NoError(t, fixture.store.HeartbeatOperation(ctx, op.OperationUUID, claimed.FencingToken, time.Minute))
			after, err := fixture.store.GetOperation(ctx, op.OperationUUID)
			require.NoError(t, err)
			require.Greater(t, *after.LeaseExpiresSinceEpoch, *before.LeaseExpiresSinceEpoch)

			// A stale fencing token cannot heartbeat.
			err = fixture.store.HeartbeatOperation(ctx, op.OperationUUID, claimed.FencingToken+100, time.Minute)
			require.ErrorIs(t, err, api.ErrConflict)

			// An expired lease cannot be revived by heartbeat, even with
			// the current fencing token.
			require.NoError(t, fixture.db.Model(&mlflow.Operation{}).
				Where("operation_uuid = ?", op.OperationUUID).
				Update("lease_expires_since_epoch", int64(0)).Error)
			err = fixture.store.HeartbeatOperation(ctx, op.OperationUUID, claimed.FencingToken, time.Minute)
			require.ErrorIs(t, err, api.ErrConflict)

			// An expired lease also blocks checkpointing and publication
			// with the correct fencing token.
			err = fixture.store.CheckpointOperation(ctx, op.OperationUUID, claimed.FencingToken, mlflow.StepTagsApplied, nil)
			require.ErrorIs(t, err, api.ErrConflict)
			err = fixture.store.PublishOperation(ctx, op.OperationUUID, claimed.FencingToken)
			require.ErrorIs(t, err, api.ErrConflict)
			still, err := fixture.store.GetOperation(ctx, op.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, mlflow.OperationRunning, still.State)
		})
	}
}

// TestStorePrepareRevisionPrecondition proves a stale expected revision is
// rejected at prepare time, before any remote work can start.
func TestStorePrepareRevisionPrecondition(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			object, err := fixture.store.ReserveObject(ctx, reservedModel("precondition-model"))
			require.NoError(t, err)

			_, err = fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "a7589fc6-ab04-42cc-9f8f-5f6a2c0e0f01",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: object.Revision + 5,
				CandidateJSON:    `{"name":"precondition-model"}`,
				PayloadHash:      "hash-1",
				Generation:       1,
			})
			require.ErrorIs(t, err, api.ErrConflict)

			// The rejected intent left no operation row.
			_, err = fixture.store.GetOperation(ctx, "a7589fc6-ab04-42cc-9f8f-5f6a2c0e0f01")
			require.ErrorIs(t, err, api.ErrNotFound)

			// Preparing against a missing resource also fails.
			_, err = fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "a7589fc6-ab04-42cc-9f8f-5f6a2c0e0f02",
				ResourceID:       424242,
				Kind:             "model-metadata-update",
				ExpectedRevision: 1,
				CandidateJSON:    `{}`,
				PayloadHash:      "hash-1",
				Generation:       1,
			})
			require.ErrorIs(t, err, api.ErrNotFound)
		})
	}
}

// TestStoreLeaseRace proves exactly one of two concurrent claimants wins
// a live lease.
func TestStoreLeaseRace(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			object, err := fixture.store.ReserveObject(ctx, reservedModel("race-model"))
			require.NoError(t, err)
			op, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "b7589fc6-ab04-42cc-9f8f-5f6a2c0e0f01",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: object.Revision,
				CandidateJSON:    `{"name":"race-model"}`,
				PayloadHash:      "hash-1",
				Generation:       1,
			})
			require.NoError(t, err)

			start := make(chan struct{})
			results := make(chan error, 2)
			for _, worker := range []string{"worker-1", "worker-2"} {
				go func(worker string) {
					<-start
					_, err := fixture.store.ClaimOperation(ctx, op.OperationUUID, worker, time.Minute)
					results <- err
				}(worker)
			}
			close(start)
			var successes int
			for range 2 {
				if err := <-results; err == nil {
					successes++
				} else {
					require.ErrorIs(t, err, api.ErrConflict)
				}
			}
			require.Equal(t, 1, successes, "exactly one claimant must win the lease")

			winner, err := fixture.store.GetOperation(ctx, op.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, mlflow.OperationRunning, winner.State)
			require.NotNil(t, winner.LeaseOwner)
		})
	}
}

// TestStorePublishSynchronizesColumns proves publication updates the
// queryable mutable columns, not only committed_json, so indexed lookups
// cannot serve stale values.
func TestStorePublishSynchronizesColumns(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			oldExt := "ext-old"
			object, err := fixture.store.ReserveObject(ctx, reservedModel("publish-ext-model"))
			require.NoError(t, err)
			object.ExternalID = &oldExt
			require.NoError(t, fixture.db.Save(&object).Error)

			op, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "c7589fc6-ab04-42cc-9f8f-5f6a2c0e0f01",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: object.Revision,
				CandidateJSON:    `{"name":"publish-ext-model","externalId":"ext-new"}`,
				PayloadHash:      "hash-1",
				Generation:       1,
			})
			require.NoError(t, err)
			claimed, err := fixture.store.ClaimOperation(ctx, op.OperationUUID, "worker-1", time.Minute)
			require.NoError(t, err)
			require.NoError(t, fixture.store.PublishOperation(ctx, op.OperationUUID, claimed.FencingToken))

			// The new external ID is queryable immediately after
			// publication...
			byNew, err := fixture.store.FindObject(ctx, mlflow.KindRegisteredModel, nil, nil, new("ext-new"))
			require.NoError(t, err)
			require.Equal(t, object.ID, byNew.ID)
			require.Equal(t, "ext-new", *byNew.ExternalID)

			// ...and the old value no longer matches.
			_, err = fixture.store.FindObject(ctx, mlflow.KindRegisteredModel, nil, nil, &oldExt)
			require.ErrorIs(t, err, api.ErrNotFound)
		})
	}
}

func TestStoreLeaseExpiryAndFencing(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			object, err := fixture.store.ReserveObject(ctx, reservedModel("lease-model"))
			require.NoError(t, err)

			operation, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "c6589fc6-ab04-42cc-9f8f-5f6a2c0e0f01",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: object.Revision,
				CandidateJSON:    `{"name":"lease-model"}`,
				PayloadHash:      "hash-1",
				Generation:       1,
			})
			require.NoError(t, err)

			// Expire the lease of a claimed operation directly.
			_, err = fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-1", 30*time.Second)
			require.NoError(t, err)
			require.NoError(t, fixture.db.Model(&mlflow.Operation{}).
				Where("operation_uuid = ?", operation.OperationUUID).
				Update("lease_expires_since_epoch", time.Now().Add(-time.Minute).UnixMilli()).Error)

			// Another worker reclaims, minting a fresh fencing token.
			reclaimed, err := fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-2", 30*time.Second)
			require.NoError(t, err)
			require.Equal(t, "worker-2", *reclaimed.LeaseOwner)
			require.Equal(t, int64(2), reclaimed.FencingToken)

			// The stale worker's write is fenced out and changes nothing.
			err = fixture.store.CheckpointOperation(ctx, operation.OperationUUID, 1, mlflow.StepTagsApplied, nil)
			require.ErrorIs(t, err, api.ErrConflict)
			still, err := fixture.store.GetOperation(ctx, operation.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, mlflow.StepClaimed, still.Step)
			require.NotEqual(t, mlflow.StepTagsApplied, still.Step)

			err = fixture.store.PublishOperation(ctx, operation.OperationUUID, 1)
			require.ErrorIs(t, err, api.ErrConflict)
			objectAfter, err := fixture.store.GetObject(ctx, object.ID)
			require.NoError(t, err)
			require.Equal(t, object.CommittedJSON, objectAfter.CommittedJSON)
		})
	}
}

func TestStoreTransactionRollback(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			// A failure midway through a transaction leaves no partial
			// public state.
			err := fixture.store.WithinTx(ctx, func(tx *mlflow.Store) error {
				_, err := tx.ReserveObject(ctx, reservedModel("rollback-model"))
				if err != nil {
					return err
				}
				return fmt.Errorf("simulated failure midway: %w", api.ErrConflict)
			})
			require.Error(t, err)

			_, err = fixture.store.FindObject(ctx, mlflow.KindRegisteredModel, new("rollback-model"), nil, nil)
			require.ErrorIs(t, err, api.ErrNotFound)

			// Publication rolled back midway also leaves the previous
			// committed state intact.
			object, err := fixture.store.ReserveObject(ctx, reservedModel("rollback-publish"))
			require.NoError(t, err)
			operation, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "d6589fc6-ab04-42cc-9f8f-5f6a2c0e0f01",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: object.Revision,
				CandidateJSON:    `{"name":"rollback-publish","description":"updated"}`,
				PayloadHash:      "hash-1",
				Generation:       1,
			})
			require.NoError(t, err)
			claimed, err := fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-1", 30*time.Second)
			require.NoError(t, err)

			err = fixture.store.WithinTx(ctx, func(tx *mlflow.Store) error {
				var inner mlflow.Operation
				require.NoError(t, tx.DB().Where("operation_uuid = ?", operation.OperationUUID).First(&inner).Error)
				inner.State = mlflow.OperationSucceeded
				require.NoError(t, tx.DB().Save(&inner).Error)
				return fmt.Errorf("fail before object update")
			})
			require.Error(t, err)
			failed, err := fixture.store.GetOperation(ctx, operation.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, mlflow.OperationRunning, failed.State)

			require.NoError(t, fixture.store.PublishOperation(ctx, operation.OperationUUID, claimed.FencingToken))
		})
	}
}

func TestStoreIDRangeGuard(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			// Push the allocator past the REST-valid range; allocation must
			// fail explicitly rather than return an unusable ID. The helper
			// restores the allocator afterward so later tests sharing this
			// database are not left with an exhausted id space.
			pushAllocatorBeyondLimit(t, engine, fixture.db)

			// Allocation beyond the range fails explicitly; callers hold
			// the reservation inside a transaction so the overflowed row
			// rolls back with it.
			err := fixture.store.WithinTx(ctx, func(tx *mlflow.Store) error {
				_, err := tx.ReserveObject(ctx, reservedModel("overflow-model"))
				return err
			})
			require.ErrorIs(t, err, api.ErrConflict)

			var count int64
			require.NoError(t, fixture.db.Model(&mlflow.Object{}).Where("name = ?", "overflow-model").Count(&count).Error)
			require.Zero(t, count)

			// A direct ( non-transactional ) reservation must also fail
			// without leaving a visible overflowed row behind.
			_, err = fixture.store.ReserveObject(ctx, reservedModel("overflow-direct"))
			require.ErrorIs(t, err, api.ErrConflict)
			require.NoError(t, fixture.db.Model(&mlflow.Object{}).Where("name = ?", "overflow-direct").Count(&count).Error)
			require.Zero(t, count)

			// Explicit out-of-range or nonpositive IDs are rejected before
			// any row is written.
			for _, explicit := range []int32{-1, 0, mlflow.MaxKubeflowID + 1} {
				if explicit == 0 {
					continue // zero means auto-allocation
				}
				reservation := reservedModel("explicit-id-model")
				reservation.ID = explicit
				_, err = fixture.store.ReserveObject(ctx, reservation)
				require.ErrorIs(t, err, api.ErrBadRequest, "explicit ID %d", explicit)
			}
			require.NoError(t, fixture.db.Model(&mlflow.Object{}).Where("name = ?", "explicit-id-model").Count(&count).Error)
			require.Zero(t, count)
		})
	}
}

// pushAllocatorBeyondLimit advances the MLflowObject id allocator past the
// REST-valid range so the next allocation overflows. The allocator is
// restored afterward so later tests sharing the database are not left with
// an exhausted id space.
func pushAllocatorBeyondLimit(t *testing.T, engine string, db *gorm.DB) {
	t.Helper()
	switch engine {
	case "mysql":
		require.NoError(t, db.Exec(
			fmt.Sprintf("ALTER TABLE MLflowObject AUTO_INCREMENT = %d", mlflow.MaxKubeflowID+1),
		).Error)
	case "postgres":
		require.NoError(t, db.Exec(
			fmt.Sprintf(`SELECT setval(pg_get_serial_sequence('"MLflowObject"', 'id'), %d)`, mlflow.MaxKubeflowID),
		).Error)
	}
	t.Cleanup(func() {
		switch engine {
		case "mysql":
			require.NoError(t, db.Exec("ALTER TABLE MLflowObject AUTO_INCREMENT = 1").Error)
		case "postgres":
			require.NoError(t, db.Exec(
				`SELECT setval(pg_get_serial_sequence('"MLflowObject"', 'id'), COALESCE((SELECT MAX(id) FROM "MLflowObject"), 0) + 1, false)`).Error)
		}
	})
}

// TestStoreReservationCancellationIsAtomic proves F8: a context canceled
// immediately after the reserved row's INSERT still leaves no row behind.
// The compensating delete of the old implementation ran with the canceled
// context and failed, persisting the overflowed row on both engines.
func TestStoreReservationCancellationIsAtomic(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			fixture.cleanup(t)
			pushAllocatorBeyondLimit(t, engine, fixture.db)

			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)

			// Cancel the context the moment the INSERT has executed.
			require.NoError(t, fixture.db.Callback().Create().After("gorm:create").
				Register("test:cancel_after_insert", func(tx *gorm.DB) {
					if _, ok := tx.Statement.Dest.(*mlflow.Object); ok {
						cancel()
					}
				}))
			t.Cleanup(func() {
				_ = fixture.db.Callback().Create().Remove("test:cancel_after_insert")
			})

			_, err := fixture.store.ReserveObject(ctx, reservedModel("canceled-overflow"))
			require.Error(t, err)

			var count int64
			require.NoError(t, fixture.db.Model(&mlflow.Object{}).Where("name = ?", "canceled-overflow").Count(&count).Error)
			require.Zero(t, count, "a canceled reservation must leave no persisted row")
		})
	}
}

// TestStoreObjectVisibility proves reserved rows are invisible to public
// reads while still consuming identity uniqueness, and that publication
// flips them to committed atomically.
func TestStoreObjectVisibility(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			// Existing callers create committed objects by default.
			committed, err := fixture.store.ReserveObject(ctx, reservedModel("visible-model"))
			require.NoError(t, err)
			require.Equal(t, mlflow.VisibilityCommitted, committed.Visibility)
			_, err = fixture.store.GetObject(ctx, committed.ID)
			require.NoError(t, err)

			// Native-backed creates reserve invisible rows.
			reservation := reservedModel("reserved-model")
			reservation.Visibility = mlflow.VisibilityReserved
			reserved, err := fixture.store.ReserveObject(ctx, reservation)
			require.NoError(t, err)
			require.Equal(t, mlflow.VisibilityReserved, reserved.Visibility)

			// Reserved rows are absent from every public read.
			_, err = fixture.store.GetObject(ctx, reserved.ID)
			require.ErrorIs(t, err, api.ErrNotFound)
			_, err = fixture.store.FindObject(ctx, mlflow.KindRegisteredModel, new("reserved-model"), nil, nil)
			require.ErrorIs(t, err, api.ErrNotFound)
			listed, err := fixture.store.ListObjects(ctx, mlflow.KindRegisteredModel, nil)
			require.NoError(t, err)
			for _, object := range listed {
				require.NotEqual(t, reserved.ID, object.ID, "a reserved row must not appear in public lists")
			}

			// The reserved row still consumes identity and external-ID
			// uniqueness, so a second create of the same name conflicts.
			_, err = fixture.store.ReserveObject(ctx, reservedModel("reserved-model"))
			require.ErrorIs(t, err, api.ErrConflict)

			// Publication flips reserved to committed in the same
			// transaction that applies the candidate.
			operation, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "d6589fc6-ab04-42cc-9f8f-5f6a2c0e0f10",
				ResourceID:       reserved.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: reserved.Revision,
				CandidateJSON:    `{"name":"reserved-model","description":"published"}`,
				PayloadHash:      "hash-visibility",
				Generation:       1,
			})
			require.NoError(t, err)
			claimed, err := fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-1", 30*time.Second)
			require.NoError(t, err)
			require.NoError(t, fixture.store.PublishOperation(ctx, operation.OperationUUID, claimed.FencingToken))

			published, err := fixture.store.GetObject(ctx, reserved.ID)
			require.NoError(t, err, "publication must make the reserved row publicly visible")
			require.Equal(t, mlflow.VisibilityCommitted, published.Visibility)

			// Unknown visibility values are rejected before any row is
			// written.
			bad := reservedModel("bad-visibility")
			bad.Visibility = mlflow.ObjectVisibility("HYPOTHETICAL")
			_, err = fixture.store.ReserveObject(ctx, bad)
			require.ErrorIs(t, err, api.ErrBadRequest)
			var count int64
			require.NoError(t, fixture.db.Model(&mlflow.Object{}).Where("name = ?", "bad-visibility").Count(&count).Error)
			require.Zero(t, count)
		})
	}
}

// TestStaleIntentRejectedAfterSiblingPublication is the permanent F3
// reproduction: an intent that lost its race — because a sibling mutation
// on the same resource published first — is rejected by every journal
// call, before any remote dispatch, and never applies its stale candidate.
func TestStaleIntentRejectedAfterSiblingPublication(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			object, err := fixture.store.ReserveObject(ctx, reservedModel("stale-intent-model"))
			require.NoError(t, err)

			winner, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "e7589fc6-ab04-42cc-9f8f-5f6a2c0e0f01",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: object.Revision,
				CandidateJSON:    `{"name":"stale-intent-model","description":"winner"}`,
				PayloadHash:      "hash-winner",
				Generation:       1,
			})
			require.NoError(t, err)

			// A differing intent at the same revision is rejected during
			// preparation, across kinds.
			_, err = fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "e7589fc6-ab04-42cc-9f8f-5f6a2c0e0f02",
				ResourceID:       object.ID,
				Kind:             "version-materialize",
				ExpectedRevision: object.Revision,
				CandidateJSON:    `{"name":"stale-intent-model","uri":"s3://bucket/model.onnx"}`,
				PayloadHash:      "hash-loser",
				Generation:       1,
			})
			require.ErrorIs(t, err, api.ErrConflict, "a differing intent at the same revision must be rejected at prepare time")

			// Seed a legacy obsolete intent directly, as rows written
			// before this serialization existed would appear, and claim it
			// while the resource is still at the expected revision.
			legacy := mlflow.Operation{
				OperationUUID:            "e7589fc6-ab04-42cc-9f8f-5f6a2c0e0f03",
				ResourceID:               object.ID,
				Kind:                     "version-materialize",
				ExpectedRevision:         object.Revision,
				CandidateJSON:            `{"name":"stale-intent-model","uri":"s3://bucket/model.onnx"}`,
				PayloadHash:              "hash-loser",
				Generation:               1,
				Step:                     mlflow.StepClaimed,
				State:                    mlflow.OperationRunning,
				LeaseOwner:               new("worker-legacy"),
				LeaseExpiresSinceEpoch:   new(time.Now().Add(time.Minute).UnixMilli()),
				FencingToken:             1,
				Attempts:                 1,
				CreateTimeSinceEpoch:     time.Now().Add(-time.Second).UnixMilli(),
				LastUpdateTimeSinceEpoch: time.Now().Add(-time.Second).UnixMilli(),
			}
			require.NoError(t, fixture.db.Create(&legacy).Error)

			// The sibling publishes first, moving the resource revision.
			claimed, err := fixture.store.ClaimOperation(ctx, winner.OperationUUID, "worker-1", 30*time.Second)
			require.NoError(t, err)
			require.NoError(t, fixture.store.PublishOperation(ctx, winner.OperationUUID, claimed.FencingToken))

			published, err := fixture.store.GetObject(ctx, object.ID)
			require.NoError(t, err)
			require.Equal(t, winner.CandidateJSON, published.CommittedJSON)
			require.Equal(t, object.Revision+1, published.Revision)

			// Every journal call on the obsolete intent is rejected before
			// any remote dispatch, and changes nothing.
			_, err = fixture.store.ClaimOperation(ctx, legacy.OperationUUID, "worker-2", 30*time.Second)
			require.ErrorIs(t, err, api.ErrConflict, "claiming an obsolete intent must be rejected")
			err = fixture.store.HeartbeatOperation(ctx, legacy.OperationUUID, 1, 30*time.Second)
			require.ErrorIs(t, err, api.ErrConflict, "heartbeating an obsolete intent must be rejected")
			err = fixture.store.CheckpointOperation(ctx, legacy.OperationUUID, 1, mlflow.StepCreateDispatched, nil)
			require.ErrorIs(t, err, api.ErrConflict, "checkpointing an obsolete intent must be rejected")
			err = fixture.store.PublishOperation(ctx, legacy.OperationUUID, 1)
			require.ErrorIs(t, err, api.ErrConflict, "publishing an obsolete intent must be rejected")

			// The rejected calls left the published state and the legacy
			// row untouched.
			after, err := fixture.store.GetObject(ctx, object.ID)
			require.NoError(t, err)
			require.Equal(t, published, after)
			legacyAfter, err := fixture.store.GetOperation(ctx, legacy.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, mlflow.OperationRunning, legacyAfter.State)
			require.Equal(t, mlflow.StepClaimed, legacyAfter.Step)

			// The obsolete intent no longer blocks a fresh writer at the
			// resource's current revision.
			fresh, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "e7589fc6-ab04-42cc-9f8f-5f6a2c0e0f04",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: published.Revision,
				CandidateJSON:    `{"name":"stale-intent-model","description":"fresh"}`,
				PayloadHash:      "hash-fresh",
				Generation:       2,
			})
			require.NoError(t, err)
			freshClaim, err := fixture.store.ClaimOperation(ctx, fresh.OperationUUID, "worker-1", 30*time.Second)
			require.NoError(t, err)
			require.NoError(t, fixture.store.PublishOperation(ctx, fresh.OperationUUID, freshClaim.FencingToken))
		})
	}
}

// TestStoreOperationIdenticalRetryJoins proves a retry carrying the same
// immutable payload joins the recorded intent instead of conflicting with
// it, while any payload difference — including the native generation —
// still conflicts.
func TestStoreOperationIdenticalRetryJoins(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			object, err := fixture.store.ReserveObject(ctx, reservedModel("retry-join-model"))
			require.NoError(t, err)

			intent := mlflow.Operation{
				OperationUUID:    "f7589fc6-ab04-42cc-9f8f-5f6a2c0e0f01",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: object.Revision,
				CandidateJSON:    `{"name":"retry-join-model","description":"once"}`,
				PayloadHash:      "hash-once",
				Generation:       1,
			}
			first, err := fixture.store.PrepareOperation(ctx, intent)
			require.NoError(t, err)

			// The same payload under a fresh UUID joins the same intent.
			retry := intent
			retry.OperationUUID = "f7589fc6-ab04-42cc-9f8f-5f6a2c0e0f02"
			joined, err := fixture.store.PrepareOperation(ctx, retry)
			require.NoError(t, err)
			require.Equal(t, first.OperationUUID, joined.OperationUUID, "an identical retry must join the recorded intent")

			var count int64
			require.NoError(t, fixture.db.Model(&mlflow.Operation{}).
				Where("resource_id = ?", object.ID).Count(&count).Error)
			require.Equal(t, int64(1), count, "an identical retry must not record a second intent")

			// Request identity is independent of the work-order payload hash.
			// Joining must retain the recorded immutable evidence.
			retry.PayloadHash = "a-new-encoding-hash"
			joined, err = fixture.store.PrepareOperation(ctx, retry)
			require.NoError(t, err)
			require.Equal(t, first.PayloadHash, joined.PayloadHash)
			// Different desired state, kind, generation or preconditions conflict.
			for name, mutation := range map[string]func(*mlflow.Operation){
				"candidate":  func(op *mlflow.Operation) { op.CandidateJSON = `{"name":"retry-join-model"}` },
				"kind":       func(op *mlflow.Operation) { op.Kind = "version-materialize" },
				"generation": func(op *mlflow.Operation) { op.Generation = 2 },
				// A stale expected revision fails the compare-and-swap
				// precondition before the active-intent check.
				"revision": func(op *mlflow.Operation) { op.ExpectedRevision = object.Revision + 1 },
			} {
				differing := intent
				differing.OperationUUID = "f7589fc6-ab04-42cc-9f8f-5f6a2c0e0f03"
				mutation(&differing)
				_, err := fixture.store.PrepareOperation(ctx, differing)
				require.ErrorIs(t, err, api.ErrConflict, "a differing %s must conflict", name)
			}
		})
	}
}

// TestPublishConflictDuplicateExternalID is the permanent F6
// reproduction: publishing a candidate whose external ID collides with
// another object's maps to a 409 conflict and changes neither object.
func TestPublishConflictDuplicateExternalID(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			owner, err := fixture.store.ReserveObject(ctx, reservedModel("f6-owner-model"))
			require.NoError(t, err)
			owner.ExternalID = new("f6-shared-ext")
			require.NoError(t, fixture.db.Save(&owner).Error)

			candidate, err := fixture.store.ReserveObject(ctx, reservedModel("f6-candidate-model"))
			require.NoError(t, err)

			operation, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "07589fc6-ab04-42cc-9f8f-5f6a2c0e0f01",
				ResourceID:       candidate.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: candidate.Revision,
				CandidateJSON:    `{"name":"f6-candidate-model","externalId":"f6-shared-ext"}`,
				PayloadHash:      "hash-f6",
				Generation:       1,
			})
			require.NoError(t, err)
			claimed, err := fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-1", 30*time.Second)
			require.NoError(t, err)

			before, err := fixture.store.GetObject(ctx, candidate.ID)
			require.NoError(t, err)

			err = fixture.store.PublishOperation(ctx, operation.OperationUUID, claimed.FencingToken)
			require.ErrorIs(t, err, api.ErrConflict, "a duplicate external ID must be a conflict, not an internal error")
			require.Equal(t, http.StatusConflict, api.ErrToStatus(err))

			// The refused publication changed neither object nor the
			// operation's completion.
			after, err := fixture.store.GetObject(ctx, candidate.ID)
			require.NoError(t, err)
			require.Equal(t, before.CommittedJSON, after.CommittedJSON)
			require.Equal(t, before.Revision, after.Revision)
			require.Nil(t, after.ExternalID)
			ownerAfter, err := fixture.store.GetObject(ctx, owner.ID)
			require.NoError(t, err)
			require.Equal(t, "f6-shared-ext", *ownerAfter.ExternalID)
			still, err := fixture.store.GetOperation(ctx, operation.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, mlflow.OperationRunning, still.State)
			require.Equal(t, int64(1), still.FencingToken)
		})
	}
}

// TestStoreOperationLockOrdering proves concurrent prepare, claim, and
// publish calls on one resource serialize through the database with a
// consistent resource-before-operation lock order: no call deadlocks, the
// live publication wins exactly once, and the losers receive conflicts.
func TestStoreOperationLockOrdering(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			ctx := t.Context()
			fixture.cleanup(t)

			object, err := fixture.store.ReserveObject(ctx, reservedModel("lock-order-model"))
			require.NoError(t, err)
			operation, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "17589fc6-ab04-42cc-9f8f-5f6a2c0e0f01",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: object.Revision,
				CandidateJSON:    `{"name":"lock-order-model","description":"updated"}`,
				PayloadHash:      "hash-lock",
				Generation:       1,
			})
			require.NoError(t, err)
			claimed, err := fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-1", time.Minute)
			require.NoError(t, err)

			start := make(chan struct{})
			type outcome struct {
				name string
				err  error
			}
			outcomes := make(chan outcome, 3)
			run := func(name string, call func() error) {
				<-start
				outcomes <- outcome{name: name, err: call()}
			}
			go run("prepare", func() error {
				_, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
					OperationUUID:    "17589fc6-ab04-42cc-9f8f-5f6a2c0e0f02",
					ResourceID:       object.ID,
					Kind:             "version-materialize",
					ExpectedRevision: object.Revision,
					CandidateJSON:    `{"name":"lock-order-model","uri":"s3://bucket/model.onnx"}`,
					PayloadHash:      "hash-lock-other",
					Generation:       1,
				})
				return err
			})
			go run("claim", func() error {
				_, err := fixture.store.ClaimOperation(ctx, operation.OperationUUID, "worker-2", time.Minute)
				return err
			})
			go run("publish", func() error {
				return fixture.store.PublishOperation(ctx, operation.OperationUUID, claimed.FencingToken)
			})
			close(start)

			for range 3 {
				select {
				case result := <-outcomes:
					switch result.name {
					case "publish":
						require.NoError(t, result.err, "the live publication must win")
					default:
						require.ErrorIs(t, result.err, api.ErrConflict,
							"the %s call must conflict, not deadlock or succeed", result.name)
					}
				case <-time.After(30 * time.Second):
					t.Fatal("concurrent journal calls deadlocked on lock ordering")
				}
			}

			// The winner published exactly once and the resource is not
			// wedged: a fresh intent at the new revision prepares, claims,
			// and publishes normally.
			published, err := fixture.store.GetObject(ctx, object.ID)
			require.NoError(t, err)
			require.Equal(t, object.Revision+1, published.Revision)
			done, err := fixture.store.GetOperation(ctx, operation.OperationUUID)
			require.NoError(t, err)
			require.Equal(t, mlflow.OperationSucceeded, done.State)

			fresh, err := fixture.store.PrepareOperation(ctx, mlflow.Operation{
				OperationUUID:    "17589fc6-ab04-42cc-9f8f-5f6a2c0e0f03",
				ResourceID:       object.ID,
				Kind:             "model-metadata-update",
				ExpectedRevision: published.Revision,
				CandidateJSON:    `{"name":"lock-order-model","description":"fresh"}`,
				PayloadHash:      "hash-lock-fresh",
				Generation:       2,
			})
			require.NoError(t, err)
			freshClaim, err := fixture.store.ClaimOperation(ctx, fresh.OperationUUID, "worker-1", 30*time.Second)
			require.NoError(t, err)
			require.NoError(t, fixture.store.PublishOperation(ctx, fresh.OperationUUID, freshClaim.FencingToken))
		})
	}
}
