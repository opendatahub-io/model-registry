// Package mlflow_test hosts backend-agnostic characterization contracts shared
// by the existing EmbedMD backend and the MLflow compatibility backend
// introduced by this work. The suite is database-backed, like internal/core's
// own tests, and runs with:
//
//	go test ./internal/mlflow/ -run Contract -count=1
package mlflow_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/kubeflow/hub/internal/core"
	"github.com/kubeflow/hub/internal/db/service"
	"github.com/kubeflow/hub/internal/defaults"
	"github.com/kubeflow/hub/internal/platform/db/schema"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const (
	httpConflict           = http.StatusConflict
	httpNotFound           = http.StatusNotFound
	httpBadRequest         = http.StatusBadRequest
	httpNotImplemented     = http.StatusNotImplemented
	httpServiceUnavailable = http.StatusServiceUnavailable
)

// parseID converts a decimal REST resource ID string to its numeric value.
func parseID(id string) (int32, error) {
	parsed, err := strconv.ParseInt(id, 10, 32)
	if err != nil {
		return 0, err
	}
	return int32(parsed), nil
}

// Metadata-value helpers shared by the contract suite.

func stringValue(v string) openapi.MetadataValue {
	return openapi.MetadataValue{
		MetadataStringValue: &openapi.MetadataStringValue{
			StringValue:  v,
			MetadataType: "MetadataStringValue",
		},
	}
}

func intValue(v string) openapi.MetadataValue {
	return openapi.MetadataValue{
		MetadataIntValue: &openapi.MetadataIntValue{
			IntValue:     v,
			MetadataType: "MetadataIntValue",
		},
	}
}

func doubleValue(v float64) openapi.MetadataValue {
	return openapi.MetadataValue{
		MetadataDoubleValue: &openapi.MetadataDoubleValue{
			DoubleValue:  v,
			MetadataType: "MetadataDoubleValue",
		},
	}
}

func boolValue(v bool) openapi.MetadataValue {
	return openapi.MetadataValue{
		MetadataBoolValue: &openapi.MetadataBoolValue{
			BoolValue:    v,
			MetadataType: "MetadataBoolValue",
		},
	}
}

// TestMain owns shared resources until every test and its cleanup completes.
func TestMain(m *testing.M) {
	runFixtureTests(testutils.SetupSharedMySQL, m.Run, func() error {
		err := errors.Join(closeSharedPostgresFixture(), sharedMySQLSession.close())
		testutils.CleanupSharedMySQL()
		return err
	}, os.Stderr, os.Exit)
}

// Keep exit last, including setup and test failures: os.Exit does not run
// deferred cleanup. A teardown failure must also produce a failing exit code.
func runFixtureTests(setup func() error, run func() int, cleanup func() error, output io.Writer, exit func(int)) {
	code := 1
	if err := setup(); err != nil {
		if _, writeErr := fmt.Fprintf(output, "Failed to setup shared MySQL container: %v\n", err); writeErr != nil {
			fmt.Printf("Failed to report fixture setup error: %v\n", writeErr)
		}
	} else {
		code = run()
	}
	if err := cleanup(); err != nil {
		if _, writeErr := fmt.Fprintf(output, "Failed to close shared fixtures: %v\n", err); writeErr != nil {
			fmt.Printf("Failed to report fixture cleanup error: %v\n", writeErr)
		}
		code = 1
	}
	exit(code)
}

type postgresFixtureSession struct {
	use     sync.Mutex
	once    sync.Once
	fixture *storeFixture
	cleanup func() error
	err     error
}

var sharedPostgresSession postgresFixtureSession

// acquire retains exclusive ownership until t and all its subtests finish.
// Setup uses a session context rather than the first borrower's lifetime.
func (s *postgresFixtureSession) acquire(t *testing.T, deps pgFixtureDeps) *storeFixture {
	t.Helper()
	s.use.Lock()
	t.Cleanup(s.use.Unlock)
	fixture, err := s.start(deps)
	require.NoError(t, err, "PostgreSQL is required for compatibility-store tests")
	cleanupMLflowTables(t, fixture.db)
	t.Cleanup(func() { cleanupMLflowTables(t, fixture.db) })
	return fixture
}

func (s *postgresFixtureSession) start(deps pgFixtureDeps) (*storeFixture, error) {
	s.once.Do(func() {
		s.fixture, s.cleanup, s.err = openPostgresFixture(context.Background(), deps)
	})
	return s.fixture, s.err
}

func sharedPostgresFixture(t *testing.T) *storeFixture {
	t.Helper()
	return sharedPostgresSession.acquire(t, defaultPGFixtureDeps())
}

func (s *postgresFixtureSession) close() error {
	s.use.Lock()
	defer s.use.Unlock()
	if s.cleanup == nil {
		return nil
	}
	return s.cleanup()
}

func closeSharedPostgresFixture() error {
	return sharedPostgresSession.close()
}

// setupEmbedMDService builds the current EmbedMD-backed ModelRegistryApi
// against a disposable MySQL database, mirroring internal/core's own test
// setup so the contract characterizes production behavior.
func setupEmbedMDService(t *testing.T) (api.ModelRegistryApi, func()) {
	t.Helper()
	sharedPostgresSession.use.Lock()
	t.Cleanup(sharedPostgresSession.use.Unlock)

	db, dbCleanup := testutils.SetupMySQLWithMigrations(t, service.DatastoreSpec())
	testutils.CleanupTestData(t, db)

	cleanup := func() {
		testutils.CleanupTestData(t, db)
		dbCleanup()
	}

	return createModelRegistryService(t, db), cleanup
}

// createModelRegistryService mirrors core_test.go's construction of the
// concrete EmbedMD service from an initialized database connection.
func createModelRegistryService(t *testing.T, db *gorm.DB) *core.ModelRegistryService {
	t.Helper()

	typeNames := []string{
		defaults.RegisteredModelTypeName,
		defaults.ModelVersionTypeName,
		defaults.DocArtifactTypeName,
		defaults.ModelArtifactTypeName,
		defaults.ServingEnvironmentTypeName,
		defaults.InferenceServiceTypeName,
		defaults.ServeModelTypeName,
		defaults.ExperimentTypeName,
		defaults.ExperimentRunTypeName,
		defaults.DataSetTypeName,
		defaults.MetricTypeName,
		defaults.MetricHistoryTypeName,
		defaults.ParameterTypeName,
	}

	typesMap := map[string]int32{}
	for _, typeName := range typeNames {
		var typeRecord schema.Type
		err := db.Where("name = ?", typeName).First(&typeRecord).Error
		require.NoError(t, err, "Failed to find type: %s", typeName)
		typesMap[typeName] = typeRecord.ID
	}

	return core.NewModelRegistryService(
		service.NewArtifactRepository(db, map[string]int32{
			defaults.ModelArtifactTypeName: typesMap[defaults.ModelArtifactTypeName],
			defaults.DocArtifactTypeName:   typesMap[defaults.DocArtifactTypeName],
			defaults.DataSetTypeName:       typesMap[defaults.DataSetTypeName],
			defaults.MetricTypeName:        typesMap[defaults.MetricTypeName],
			defaults.ParameterTypeName:     typesMap[defaults.ParameterTypeName],
			defaults.MetricHistoryTypeName: typesMap[defaults.MetricHistoryTypeName],
		}),
		service.NewModelArtifactRepository(db, typesMap[defaults.ModelArtifactTypeName]),
		service.NewDocArtifactRepository(db, typesMap[defaults.DocArtifactTypeName]),
		service.NewRegisteredModelRepository(db, typesMap[defaults.RegisteredModelTypeName]),
		service.NewModelVersionRepository(db, typesMap[defaults.ModelVersionTypeName]),
		service.NewServingEnvironmentRepository(db, typesMap[defaults.ServingEnvironmentTypeName]),
		service.NewInferenceServiceRepository(db, typesMap[defaults.InferenceServiceTypeName]),
		service.NewServeModelRepository(db, typesMap[defaults.ServeModelTypeName]),
		service.NewExperimentRepository(db, typesMap[defaults.ExperimentTypeName]),
		service.NewExperimentRunRepository(db, typesMap[defaults.ExperimentRunTypeName]),
		service.NewDataSetRepository(db, typesMap[defaults.DataSetTypeName]),
		service.NewMetricRepository(db, typesMap[defaults.MetricTypeName]),
		service.NewParameterRepository(db, typesMap[defaults.ParameterTypeName]),
		service.NewMetricHistoryRepository(db, typesMap[defaults.MetricHistoryTypeName]),
		typesMap,
	)
}
