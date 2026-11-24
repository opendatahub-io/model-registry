package mlflow_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"gorm.io/gorm"
)

func TestStoreFixtureMySQLConnectionsStayBounded(t *testing.T) {
	observer, closeObserver := testutils.GetSharedMySQLDB(t)
	t.Cleanup(closeObserver)
	connections := func() int {
		var status struct {
			Value int `gorm:"column:Value"`
		}
		require.NoError(t, observer.Raw("SHOW STATUS LIKE 'Threads_connected'").Scan(&status).Error)
		return status.Value
	}
	t.Run("initialize package session", func(t *testing.T) { require.Len(t, newStoreFixtures(t), 2) })
	baseline := connections()
	for _, name := range []string{"first acquisition", "second acquisition"} {
		t.Run(name, func(t *testing.T) {
			fixtures := newStoreFixtures(t)
			require.Len(t, fixtures, 2)
		})
		require.Eventually(t, func() bool { return connections() == baseline }, 5*time.Second, 10*time.Millisecond,
			"repeated fixture acquisitions must not retain additional migration connections")
	}
}

// Replacing the package fixture with a per-test PostgreSQL container must
// fail this regression: the cluster identity comes from the real server.
func TestStoreFixtureReusesPostgres(t *testing.T) {
	var clusterID string
	for _, name := range []string{"first owner", "next owner"} {
		t.Run(name, func(t *testing.T) {
			fixtures := newStoreFixtures(t)
			require.Len(t, fixtures, 2, "both database engines are mandatory")
			var got string
			require.NoError(t, fixtures["postgres"].db.Raw(
				"SELECT system_identifier::text FROM pg_control_system()",
			).Scan(&got).Error)
			require.NotEmpty(t, got)
			if clusterID == "" {
				clusterID = got
			} else {
				require.Equal(t, clusterID, got, "serial tests must reuse one real PostgreSQL cluster")
			}
		})
	}
}

// Leaving rows behind on release or acquisition must fail this regression.
func TestStoreFixtureEmptyTables(t *testing.T) {
	for _, name := range []string{"write rows", "next owner"} {
		t.Run(name, func(t *testing.T) {
			for engine, fixture := range newStoreFixtures(t) {
				t.Run(engine, func(t *testing.T) {
					for _, model := range []any{
						&mlflow.Operation{}, &mlflow.VersionHistory{}, &mlflow.VersionBinding{},
						&mlflow.ArtifactLink{}, &mlflow.Object{}, &mlflow.Adapter{},
					} {
						var count int64
						require.NoError(t, fixture.db.Model(model).Count(&count).Error)
						require.Zero(t, count, "each owner must start with an empty %T", model)
					}
					model, err := fixture.store.ReserveObject(t.Context(), reservedModel("fixture-leftover"))
					require.NoError(t, err)
					version, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{
						Kind: mlflow.KindModelVersion, ParentID: model.ID, Name: "1",
					})
					require.NoError(t, err)
					artifact, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{
						Kind: mlflow.KindModelArtifact, Name: "fixture-artifact",
					})
					require.NoError(t, err)
					for _, row := range []any{
						&mlflow.Adapter{AdapterUUID: "fixture-adapter"},
						&mlflow.ArtifactLink{ArtifactID: artifact.ID, VersionID: version.ID},
						&mlflow.VersionBinding{VersionID: version.ID, ModelID: model.ID},
						&mlflow.VersionHistory{VersionID: version.ID, Generation: 1,
							NativeModelName: "fixture-model", NativeVersionNumber: "1"},
						&mlflow.Operation{OperationUUID: "fixture-operation", ResourceID: version.ID},
					} {
						require.NoError(t, fixture.db.Create(row).Error)
					}
				})
			}
		})
	}
}

type recordingPGContainer struct {
	*fakePGContainer
	events *[]string
	err    error
}

func (c *recordingPGContainer) Terminate(ctx context.Context, _ ...testcontainers.TerminateOption) error {
	*c.events = append(*c.events, "terminate container")
	if err := ctx.Err(); err != nil {
		return err
	}
	close(c.terminated)
	return c.err
}

func TestStoreFixtureExclusiveOwnership(t *testing.T) {
	var fixture *storeFixture
	t.Run("owner", func(t *testing.T) {
		fixture = sharedPostgresFixture(t)
		assertOwned := func(t *testing.T) {
			t.Helper()
			if sharedPostgresSession.use.TryLock() {
				sharedPostgresSession.use.Unlock()
				t.Fatal("another test could acquire the fixture before its owner completed cleanup")
			}
		}
		t.Run("child", assertOwned)
		t.Cleanup(func() { assertOwned(t) })
		_, err := fixture.store.ReserveObject(t.Context(), reservedModel("owner-leftover"))
		require.NoError(t, err)
	})
	// Cleanup released the lock, removed data, and retained the session's pool.
	require.True(t, sharedPostgresSession.use.TryLock(), "cleanup must release exclusive ownership")
	defer sharedPostgresSession.use.Unlock()
	var count int64
	require.NoError(t, fixture.db.Model(&mlflow.Object{}).Count(&count).Error)
	require.Zero(t, count, "release must clean rows before unlocking")
}

func TestStoreFixtureExitCleanup(t *testing.T) {
	for _, tc := range []struct {
		name         string
		setupErr     error
		testCode     int
		cleanupErr   error
		terminateErr error
		wantCode     int
		wantEvents   []string
	}{
		{name: "pass", wantEvents: []string{"setup", "run", "cleanup", "close database", "terminate container", "exit"}},
		{name: "test failure", testCode: 7, wantCode: 7, wantEvents: []string{"setup", "run", "cleanup", "close database", "terminate container", "exit"}},
		{name: "setup failure", setupErr: errors.New("setup failed"), wantCode: 1, wantEvents: []string{"setup", "cleanup", "exit"}},
		{name: "cleanup failure", cleanupErr: errors.New("cleanup failed"), wantCode: 1, wantEvents: []string{"setup", "run", "cleanup", "close database", "terminate container", "exit"}},
		{name: "both resources fail cleanup", cleanupErr: errors.New("database close failed"), terminateErr: errors.New("termination failed"), wantCode: 1, wantEvents: []string{"setup", "run", "cleanup", "close database", "terminate container", "exit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			var output bytes.Buffer
			var session postgresFixtureSession
			deps := defaultPGFixtureDeps()
			container := &recordingPGContainer{fakePGContainer: newFakePGContainer(), events: &events, err: tc.terminateErr}
			deps.run = func(context.Context) (pgFixtureContainer, error) { return container, nil }
			deps.dsn = func(context.Context, pgFixtureContainer) (string, error) { return "fake-dsn", nil }
			deps.connect = func(string) (*gorm.DB, func() error, error) {
				return nil, func() error {
					events = append(events, "close database")
					return tc.cleanupErr
				}, nil
			}
			deps.migrate = func(context.Context, *gorm.DB) error { return nil }
			runFixtureTests(func() error {
				events = append(events, "setup")
				return tc.setupErr
			}, func() int {
				events = append(events, "run")
				_, err := session.start(deps)
				require.NoError(t, err)
				return tc.testCode
			}, func() error {
				events = append(events, "cleanup")
				return session.close()
			}, &output, func(code int) {
				events = append(events, "exit")
				require.Equal(t, tc.wantEvents, events, "owned resources must close before os.Exit")
				require.Equal(t, tc.wantCode, code)
			})
			if tc.cleanupErr != nil {
				require.Contains(t, output.String(), tc.cleanupErr.Error())
			}
			if tc.terminateErr != nil {
				require.Contains(t, output.String(), tc.terminateErr.Error())
			}
		})
	}
}

func TestStoreFixtureSessionFailureCleanup(t *testing.T) {
	container := newFakePGContainer()
	deps := defaultPGFixtureDeps()
	deps.run = func(context.Context) (pgFixtureContainer, error) { return container, nil }
	deps.dsn = func(context.Context, pgFixtureContainer) (string, error) {
		return "", errors.New("PostgreSQL unavailable")
	}
	var session postgresFixtureSession
	fixture, err := session.start(deps)
	require.Nil(t, fixture)
	require.ErrorContains(t, err, "PostgreSQL unavailable")
	// A second borrower must see the same setup error, rather than using a
	// nil fixture or silently omitting the engine after sync.Once completed.
	fixture, err = session.start(deps)
	require.Nil(t, fixture)
	require.ErrorContains(t, err, "PostgreSQL unavailable")
	require.NoError(t, session.close())
	awaitTerminated(t, container)
	require.NoError(t, session.close(), "session teardown must be idempotent")
}

// Fatal fixture setup is exercised in a subprocess so an absent engine must
// produce a failing test process, rather than quietly reduce the fixture map.
func TestStoreFixtureRequiredPostgres(t *testing.T) {
	const childEnv = "MLFLOW_FIXTURE_REQUIRED_POSTGRES_CHILD"
	if os.Getenv(childEnv) == "1" {
		deps := defaultPGFixtureDeps()
		deps.run = func(context.Context) (pgFixtureContainer, error) {
			return nil, errors.New("injected missing PostgreSQL")
		}
		_, err := sharedPostgresSession.start(deps)
		require.Error(t, err)
		fixtures := newStoreFixtures(t)
		t.Logf("fixture unexpectedly returned %d engines", len(fixtures))
		return
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStoreFixtureRequiredPostgres$", "-test.v")
	command.Env = append(os.Environ(), childEnv+"=1")
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "%s", output)
	require.Equal(t, 1, exitErr.ExitCode(), "%s", output)
	require.Contains(t, string(output), "PostgreSQL is required for compatibility-store tests")
	require.Contains(t, string(output), "injected missing PostgreSQL")
	require.NotContains(t, string(output), "fixture unexpectedly returned")
}

func TestStoreFixturePartialStartupCleanup(t *testing.T) {
	container := newFakePGContainer()
	deps := defaultPGFixtureDeps()
	deps.run = func(context.Context) (pgFixtureContainer, error) {
		return container, errors.New("startup readiness failed")
	}
	t.Cleanup(func() { awaitTerminated(t, container) })
	_, err := setupPostgresFixture(t, deps)
	require.ErrorContains(t, err, "startup readiness failed")
}
