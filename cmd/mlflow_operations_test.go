package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/mlflow/client"
	platformdb "github.com/kubeflow/hub/internal/platform/db"
	"github.com/kubeflow/hub/internal/testutils"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// operationsStub is a lean protocol-faithful MLflow registry serving the
// endpoints the operator-resolution paths use, with request recording,
// pre-commit create failures, and commit-then-fail injection for
// uncertain creates.
type operationsStub struct {
	mu                 sync.Mutex
	url                string
	models             map[string]map[string]any // name -> {name, description, tags, aliases}
	versions           map[string]map[string]map[string]any
	nextVersion        map[string]int
	requests           []string
	failModelCreates   int
	failVersionCreates int
	commitThenFail     int
	failAliases        bool
}

func newOperationsStub(t *testing.T) *operationsStub {
	t.Helper()
	stub := &operationsStub{
		models:      map[string]map[string]any{},
		versions:    map[string]map[string]map[string]any{},
		nextVersion: map[string]int{},
	}

	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	writeError := func(w http.ResponseWriter, status int, code string) {
		writeJSON(w, status, map[string]any{"error_code": code, "message": code})
	}
	decode := func(r *http.Request) map[string]any {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		return body
	}
	tagMap := func(body map[string]any) map[string]string {
		tags := map[string]string{}
		if raw, ok := body["tags"].([]any); ok {
			for _, entry := range raw {
				tag := entry.(map[string]any)
				tags[tag["key"].(string)] = tag["value"].(string)
			}
		}
		return tags
	}
	stub.record("init")
	stub.requests = nil

	mux.HandleFunc("/api/2.0/mlflow/registered-models/create", func(w http.ResponseWriter, r *http.Request) {
		body := decode(r)
		name := body["name"].(string)
		stub.record("POST registered-models/create")
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if stub.failModelCreates > 0 {
			stub.failModelCreates--
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR")
			return
		}
		if _, exists := stub.models[name]; exists {
			writeError(w, http.StatusBadRequest, "RESOURCE_ALREADY_EXISTS")
			return
		}
		description, _ := body["description"].(string)
		stub.models[name] = map[string]any{
			"name": name, "description": description, "tags": tagMap(body),
			"aliases": map[string]string{},
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"registered_model": map[string]any{"name": name},
		})
	})
	mux.HandleFunc("/api/2.0/mlflow/registered-models/get", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		stub.record("GET registered-models/get")
		stub.mu.Lock()
		defer stub.mu.Unlock()
		model, exists := stub.models[name]
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		tags := []map[string]string{}
		for key, value := range model["tags"].(map[string]string) {
			tags = append(tags, map[string]string{"key": key, "value": value})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"registered_model": map[string]any{
				"name": model["name"], "description": model["description"], "tags": tags,
			},
		})
	})
	mux.HandleFunc("/api/2.0/mlflow/registered-models/search", func(w http.ResponseWriter, r *http.Request) {
		stub.record("GET registered-models/search")
		stub.mu.Lock()
		defer stub.mu.Unlock()
		entries := []any{}
		for _, model := range stub.models {
			tags := []map[string]string{}
			for key, value := range model["tags"].(map[string]string) {
				tags = append(tags, map[string]string{"key": key, "value": value})
			}
			entries = append(entries, map[string]any{"name": model["name"], "description": model["description"], "tags": tags})
		}
		writeJSON(w, http.StatusOK, map[string]any{"registered_models": entries})
	})
	versionBody := func(record map[string]any) map[string]any {
		tags := []map[string]string{}
		for key, value := range record["tags"].(map[string]string) {
			tags = append(tags, map[string]string{"key": key, "value": value})
		}
		return map[string]any{
			"name": record["name"], "version": record["version"],
			"source": record["source"], "status": record["status"], "tags": tags,
		}
	}
	mux.HandleFunc("/api/2.0/mlflow/model-versions/create", func(w http.ResponseWriter, r *http.Request) {
		body := decode(r)
		name := body["name"].(string)
		stub.record("POST model-versions/create")
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if _, exists := stub.models[name]; !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		if stub.failVersionCreates > 0 {
			stub.failVersionCreates--
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR")
			return
		}
		stub.nextVersion[name]++
		number := strconv.Itoa(stub.nextVersion[name])
		source, _ := body["source"].(string)
		description, _ := body["description"].(string)
		committed := stub.commitThenFail > 0
		if committed {
			stub.commitThenFail--
		}
		status := "READY"
		record := map[string]any{
			"name": name, "version": number, "source": source,
			"description": description, "status": status, "tags": tagMap(body),
		}
		if stub.versions[name] == nil {
			stub.versions[name] = map[string]map[string]any{}
		}
		stub.versions[name][number] = record
		if committed {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"model_version": map[string]any{"name": name, "version": number, "status": status},
		})
	})
	mux.HandleFunc("/api/2.0/mlflow/model-versions/get", func(w http.ResponseWriter, r *http.Request) {
		stub.record("GET model-versions/get")
		stub.mu.Lock()
		defer stub.mu.Unlock()
		record, exists := stub.versionRecord(r.URL.Query().Get("name"), r.URL.Query().Get("version"))
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"model_version": versionBody(record)})
	})
	mux.HandleFunc("/api/2.0/mlflow/model-versions/search", func(w http.ResponseWriter, r *http.Request) {
		stub.record("GET model-versions/search")
		filter := r.URL.Query().Get("filter")
		stub.mu.Lock()
		defer stub.mu.Unlock()
		entries := []any{}
		for name, versions := range stub.versions {
			if filter != "" && !strings.Contains(filter, name) {
				continue
			}
			numbers := []string{}
			for number := range versions {
				numbers = append(numbers, number)
			}
			for i := range numbers {
				entries = append(entries, versionBody(versions[numbers[i]]))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"model_versions": entries})
	})
	mux.HandleFunc("/api/2.0/mlflow/model-versions/set-tag", func(w http.ResponseWriter, r *http.Request) {
		body := decode(r)
		stub.record("POST model-versions/set-tag")
		stub.mu.Lock()
		defer stub.mu.Unlock()
		record, exists := stub.versionRecord(body["name"].(string), body["version"].(string))
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		record["tags"].(map[string]string)[body["key"].(string)] = body["value"].(string)
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	mux.HandleFunc("/api/2.0/mlflow/registered-models/alias", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			stub.record("GET registered-models/alias")
			stub.mu.Lock()
			defer stub.mu.Unlock()
			model, exists := stub.models[r.URL.Query().Get("name")]
			if !exists {
				writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
				return
			}
			version, aliased := model["aliases"].(map[string]string)[r.URL.Query().Get("alias")]
			if !aliased {
				writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
				return
			}
			record, _ := stub.versionRecord(model["name"].(string), version)
			writeJSON(w, http.StatusOK, map[string]any{"model_version": versionBody(record)})
			return
		}
		body := decode(r)
		stub.record("POST registered-models/alias")
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if stub.failAliases {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR")
			return
		}
		model, exists := stub.models[body["name"].(string)]
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		model["aliases"].(map[string]string)[body["alias"].(string)] = body["version"].(string)
		writeJSON(w, http.StatusOK, map[string]any{})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	stub.url = server.URL
	return stub
}

func (s *operationsStub) record(label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, label)
}

// versionRecord loads one version record; the caller holds the lock.
func (s *operationsStub) versionRecord(name, version string) (map[string]any, bool) {
	versions, exists := s.versions[name]
	if !exists {
		return nil, false
	}
	record, exists := versions[version]
	return record, exists
}

// countRequests counts recorded requests carrying the label prefix.
func (s *operationsStub) countRequests(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, label := range s.requests {
		if strings.HasPrefix(label, prefix) {
			count++
		}
	}
	return count
}

// mutatingRequests counts every recorded mutating request.
func (s *operationsStub) mutatingRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, label := range s.requests {
		if strings.HasPrefix(label, "POST") || strings.HasPrefix(label, "PATCH") ||
			strings.HasPrefix(label, "DELETE") {
			count++
		}
	}
	return count
}

// versionTags returns a copy of one native version's tags.
func (s *operationsStub) versionTags(name, version string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.versionRecord(name, version)
	if !exists {
		panic(fmt.Sprintf("version %s/%s does not exist", name, version))
	}
	tags := map[string]string{}
	maps.Copy(tags, record["tags"].(map[string]string))
	return tags
}

// setVersionTag overwrites one tag on a native version.
func (s *operationsStub) setVersionTag(name, version, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.versionRecord(name, version)
	if !exists {
		panic(fmt.Sprintf("version %s/%s does not exist", name, version))
	}
	record["tags"].(map[string]string)[key] = value
}

// stripVersionTag removes one tag from a native version, simulating a
// create whose identity tag was lost.
func (s *operationsStub) stripVersionTag(name, version, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.versionRecord(name, version)
	if !exists {
		panic(fmt.Sprintf("version %s/%s does not exist", name, version))
	}
	delete(record["tags"].(map[string]string), key)
}

// setVersionSource rewrites one native version's source.
func (s *operationsStub) setVersionSource(name, version, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.versionRecord(name, version)
	if !exists {
		panic(fmt.Sprintf("version %s/%s does not exist", name, version))
	}
	record["source"] = source
}

// cloneVersionWithTag duplicates one native version under a new number,
// carrying the given operation tag — the multiple-matches conflict.
func (s *operationsStub) cloneVersionWithTag(name, version, newNumber, tagKey, tagValue string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.versionRecord(name, version)
	if !exists {
		panic(fmt.Sprintf("version %s/%s does not exist", name, version))
	}
	clone := map[string]any{}
	maps.Copy(clone, record)
	tags := map[string]string{}
	maps.Copy(tags, record["tags"].(map[string]string))
	tags[tagKey] = tagValue
	clone["tags"] = tags
	clone["version"] = newNumber
	s.versions[name][newNumber] = clone
}

// countVersions returns the number of native versions of one model.
func (s *operationsStub) countVersions(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.versions[name])
}

// modelAliasVersion returns the version an alias points at.
func (s *operationsStub) modelAliasVersion(name, alias string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	model, exists := s.models[name]
	if !exists {
		return "", false
	}
	version, aliased := model["aliases"].(map[string]string)[alias]
	return version, aliased
}

// operationsFixture couples a migrated compatibility store, an
// initialized adapter, a native stub, and a request-path service used
// to arrange journal state.
type operationsFixture struct {
	engine  string
	stub    *operationsStub
	native  *client.Client
	store   *mlflow.Store
	service *mlflow.Service
}

// newOperationsFixture migrates the shared compatibility database,
// initializes the adapter against the stub's registry target, and
// builds the request-path service.
func newOperationsFixture(t *testing.T, engine string) *operationsFixture {
	t.Helper()
	fixture := &operationsFixture{engine: engine}
	fixture.reopen(t, true)
	return fixture
}

// reopen resets the compatibility store and, when asked, reinitializes
// the adapter identity over a fresh stub.
func (f *operationsFixture) reopen(t *testing.T, initialize bool) {
	t.Helper()
	var dsn string
	if f.engine == "mysql" {
		dsn = testutils.GetSharedMySQLDSN(t)
	} else {
		dsn = testutils.GetSharedPostgresDSN(t)
	}
	resetMLflowCompatibilityStore(t, f.engine, dsn)
	db := openCompatibilityDB(t, f.engine, dsn)

	// The schema must exist even when the adapter row is deliberately
	// absent for the uninitialized-store test.
	migrator, err := platformdb.NewDBMigrator(db)
	require.NoError(t, err)
	require.NoError(t, migrator.Migrate())
	if closer, ok := migrator.(interface{ Close() error }); ok {
		_ = closer.Close()
	}

	f.stub = newOperationsStub(t)
	f.native, err = client.New(f.stub.url, nil)
	require.NoError(t, err)
	f.store = mlflow.NewStore(db)

	if initialize {
		fingerprint, err := mlflow.RegistryTargetFingerprint(f.stub.url)
		require.NoError(t, err)
		_, err = f.store.InitializeAdapter(t.Context(), fingerprint)
		require.NoError(t, err)
	}

	f.service, err = mlflow.NewService(mlflow.Config{
		RegistryURI: f.stub.url, RequestTimeout: 5 * time.Second,
	}, f.store, f.native)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.service.Close() })
}

// mustModel creates a committed registered model through the public API.
func (f *operationsFixture) mustModel(t *testing.T, name string) *openapi.RegisteredModel {
	t.Helper()
	model, err := f.service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: name})
	require.NoError(t, err)
	return model
}

// mustVersion creates a committed pending model version.
func (f *operationsFixture) mustVersion(t *testing.T,
	model *openapi.RegisteredModel, name string) *openapi.ModelVersion {
	t.Helper()
	version, err := f.service.UpsertModelVersion(&openapi.ModelVersion{Name: name}, model.Id)
	require.NoError(t, err)
	return version
}

// uncertainOperation attaches a source-bearing artifact whose native
// create leaves an uncertain outcome: the operation is left running at
// CREATE_DISPATCHED. When commit is set the create applies server-side
// before answering with a gateway error, leaving one committed native
// version carrying its tags; otherwise the create fails cleanly and
// nothing commits.
func (f *operationsFixture) uncertainOperation(t *testing.T,
	modelName, uri string, commit bool) mlflow.Operation {
	t.Helper()
	model := f.mustModel(t, modelName)
	version := f.mustVersion(t, model, "1.0.0")

	f.stub.mu.Lock()
	if commit {
		f.stub.commitThenFail = 1
	} else {
		f.stub.failVersionCreates = 1
	}
	f.stub.mu.Unlock()

	_, err := f.service.UpsertModelVersionArtifact(&openapi.Artifact{
		ModelArtifact: &openapi.ModelArtifact{Name: new(modelName + "-primary"), Uri: new(uri)},
	}, *version.Id)
	require.ErrorIs(t, err, api.ErrUnavailable)

	// The crashed replica's lease is expired: a restarted worker or the
	// operator may reclaim the work.
	f.expireOperationLeases(t)

	var operation mlflow.Operation
	f.firstOperation(t, mlflow.OperationKindVersionMaterialize, &operation)
	require.Equal(t, mlflow.OperationRunning, operation.State)
	require.Equal(t, mlflow.StepCreateDispatched, operation.Step)
	return operation
}

// firstOperation loads the most recently created operation row of one
// kind.
func (f *operationsFixture) firstOperation(t *testing.T, kind string, dest *mlflow.Operation) {
	t.Helper()
	require.NoError(t, f.store.DB().Where("kind = ?", kind).
		Order("create_time_since_epoch DESC").First(dest).Error)
}

// bindingOfVersion loads the committed binding of a logical version.
func (f *operationsFixture) bindingOfVersion(t *testing.T, versionID string) mlflow.VersionBinding {
	t.Helper()
	var binding mlflow.VersionBinding
	parsed, err := strconv.ParseInt(versionID, 10, 32)
	require.NoError(t, err)
	require.NoError(t, f.store.DB().Where("version_id = ?", int32(parsed)).First(&binding).Error)
	return binding
}

// command builds the operations command over this fixture's resources.
func (f *operationsFixture) command(t *testing.T) *cobra.Command {
	t.Helper()
	open := func(context.Context, MLflowBackendConfig) (*mlflow.Store, *client.Client, func() error, error) {
		return f.store, f.native, func() error { return nil }, nil
	}
	return newMLflowOperationsCommand(open)
}

// runOperationCommand executes the command with the fixture's registry
// target and returns its output and error.
func (f *operationsFixture) runOperationCommand(t *testing.T,
	args ...string) (string, error) {
	t.Helper()
	command := f.command(t)
	command.SetArgs(append([]string{"--mlflow-registry-uri", f.stub.url}, args...))
	var output strings.Builder
	command.SetOut(&output)
	command.SetErr(&output)
	err := command.ExecuteContext(t.Context())
	return output.String(), err
}

// expireOperationLeases forces every running operation's lease into the
// past so a restarted worker or the operator may reclaim the work.
func (f *operationsFixture) expireOperationLeases(t *testing.T) {
	t.Helper()
	require.NoError(t, f.store.DB().Model(&mlflow.Operation{}).
		Where("state = ?", mlflow.OperationRunning).
		Update("lease_expires_since_epoch", 0).Error)
}

// operation loads one operation by UUID.
func (f *operationsFixture) operation(t *testing.T, operationID string) mlflow.Operation {
	t.Helper()
	operation, err := f.store.GetOperation(t.Context(), operationID)
	require.NoError(t, err)
	return operation
}

// committedArtifacts counts committed model-artifact rows.
func (f *operationsFixture) committedArtifacts(t *testing.T) int64 {
	t.Helper()
	var count int64
	require.NoError(t, f.store.DB().Model(&mlflow.Object{}).
		Where("kind = ? AND visibility = ?", mlflow.KindModelArtifact, mlflow.VisibilityCommitted).
		Count(&count).Error)
	return count
}

// stubOperationTag returns the operation identity tag one native
// version carries, if any.
func (f *operationsFixture) stubOperationTag(modelName, version string) string {
	return f.stub.versionTags(modelName, version)["rhoai.operation"]
}

// TestMLflowOperationsListIsSanitizedReadOnly proves the list command
// reports state, checkpoint, lease, and retry details without
// credentials or registry URLs, and performs no mutation on either
// side.
func TestMLflowOperationsListIsSanitizedReadOnly(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			fixture := newOperationsFixture(t, engine)
			operation := fixture.uncertainOperation(t, "list-model", "s3://bucket/weights.tar.gz", true)

			mutations := fixture.stub.mutatingRequests()
			output, err := fixture.runOperationCommand(t, "list")
			require.NoError(t, err)

			assert.Contains(t, output, operation.OperationUUID)
			assert.Contains(t, output, "kind:             version-materialize")
			assert.Contains(t, output, "state:            RUNNING")
			assert.Contains(t, output, "step:             CREATE_DISPATCHED")
			assert.Contains(t, output, "attempts:")
			assert.Contains(t, output, "lease owner:")
			assert.Contains(t, output, "next retry:")
			assert.NotContains(t, output, fixture.stub.url, "the registry URL must not appear")
			assert.NotContains(t, output, "dsn", "the DSN must not appear")

			assert.Equal(t, mutations, fixture.stub.mutatingRequests(),
				"inspection must issue no mutating native request")
			assert.Equal(t, operation, fixture.operation(t, operation.OperationUUID),
				"inspection must change no row")

			// An empty journal lists cleanly.
			other := newOperationsFixture(t, engine)
			output, err = other.runOperationCommand(t, "list")
			require.NoError(t, err)
			assert.Contains(t, output, "no operations")
		})
	}
}

// TestMLflowOperationsBindRejections proves every unverified or
// unresolvable selection is rejected before any row changes, on both
// engines.
func TestMLflowOperationsBindRejections(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			fixture := newOperationsFixture(t, engine)

			// Missing explicit operation selection.
			_, err := fixture.runOperationCommand(t, "bind",
				"--model", "list-model", "--version", "1")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "operation")

			// Unknown operation.
			_, err = fixture.runOperationCommand(t, "bind",
				"--operation", "deadbeef-dead-dead-dead-deaddeadbeef",
				"--model", "m", "--version", "1")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "no operation found")

			cases := []struct {
				name     string
				arrange  func(t *testing.T) (operationID, model, version string)
				message  string
				noChange bool
				selected bool
			}{
				{
					name: "terminal operation",
					arrange: func(t *testing.T) (string, string, string) {
						model := fixture.mustModel(t, "done-model")
						version := fixture.mustVersion(t, model, "1.0.0")
						_, err := fixture.service.UpsertModelVersionArtifact(&openapi.Artifact{
							ModelArtifact: &openapi.ModelArtifact{
								Name: new("done-model-primary"), Uri: new("s3://bucket/done.tar.gz"),
							},
						}, *version.Id)
						require.NoError(t, err)
						var operation mlflow.Operation
						fixture.firstOperation(t, mlflow.OperationKindVersionMaterialize, &operation)
						return operation.OperationUUID, "done-model", "1"
					},
					message: "already SUCCEEDED",
				},
				{
					name: "wrong logical parent",
					arrange: func(t *testing.T) (string, string, string) {
						operation := fixture.uncertainOperation(t, "parent-model", "s3://bucket/X.tar.gz", true)
						fixture.mustModel(t, "other-model")
						return operation.OperationUUID, "other-model", "1"
					},
					message: "logical parent",
				},
				{
					name: "wrong generation",
					arrange: func(t *testing.T) (string, string, string) {
						operation := fixture.uncertainOperation(t, "generation-model", "s3://bucket/X.tar.gz", true)
						tamperCandidateGeneration(t, fixture, operation, 99)
						return operation.OperationUUID, "generation-model", "1"
					},
					message: "generation",
				},
				{
					name: "wrong ownership",
					arrange: func(t *testing.T) (string, string, string) {
						operation := fixture.uncertainOperation(t, "owner-model", "s3://bucket/X.tar.gz", true)
						fixture.stub.setVersionTag("owner-model", "1", "rhoai.adapter", "another-adapter")
						return operation.OperationUUID, "owner-model", "1"
					},
					message: "ownership or identity",
				},
				{
					name: "claimed by another operation",
					arrange: func(t *testing.T) (string, string, string) {
						operation := fixture.uncertainOperation(t, "claimed-model", "s3://bucket/X.tar.gz", true)
						fixture.stub.setVersionTag("claimed-model", "1",
							"rhoai.operation", "11111111-2222-3333-4444-555555555555")
						return operation.OperationUUID, "claimed-model", "1"
					},
					message: "ownership or identity",
				},
				{
					name: "wrong source",
					arrange: func(t *testing.T) (string, string, string) {
						operation := fixture.uncertainOperation(t, "source-model", "s3://bucket/X.tar.gz", true)
						fixture.stub.setVersionSource("source-model", "1", "s3://bucket/other.tar.gz")
						return operation.OperationUUID, "source-model", "1"
					},
					message: "source",
				},
				{
					name: "invalid native identity",
					arrange: func(t *testing.T) (string, string, string) {
						operation := fixture.uncertainOperation(t, "missing-model", "s3://bucket/X.tar.gz", true)
						return operation.OperationUUID, "missing-model", "99"
					},
					message: "no native model version",
				},
				{
					name: "multiple identity-tag matches",
					arrange: func(t *testing.T) (string, string, string) {
						operation := fixture.uncertainOperation(t, "multi-model", "s3://bucket/X.tar.gz", true)
						opTag := fixture.stubOperationTag("multi-model", "1")
						fixture.stub.cloneVersionWithTag("multi-model", "1", "2", "rhoai.operation", opTag)
						return operation.OperationUUID, "multi-model", "1"
					},
					selected: true,
				},
				{
					name: "identity tag on another version",
					arrange: func(t *testing.T) (string, string, string) {
						operation := fixture.uncertainOperation(t, "tagged-model", "s3://bucket/X.tar.gz", true)
						// The operator selects a version that exists but
						// is not the one carrying the operation's tag.
						fixture.stub.cloneVersionWithTag("tagged-model", "1", "2",
							"rhoai.operation", "unrelated")
						return operation.OperationUUID, "tagged-model", "2"
					},
					message: "ownership or identity",
				},
				{
					name: "not a native version create",
					arrange: func(t *testing.T) (string, string, string) {
						fixture.stub.mu.Lock()
						fixture.stub.failModelCreates = 1
						fixture.stub.mu.Unlock()
						_, err := fixture.service.UpsertRegisteredModel(&openapi.RegisteredModel{
							Name: "stuck-model",
						})
						require.Error(t, err)
						var operation mlflow.Operation
						fixture.firstOperation(t, mlflow.OperationKindModelCreate, &operation)
						return operation.OperationUUID, "stuck-model", "1"
					},
					message: "applies to native version creates",
				},
			}

			for _, testCase := range cases {
				t.Run(testCase.name, func(t *testing.T) {
					operationID, model, version := testCase.arrange(t)
					before := fixture.operation(t, operationID)

					_, err := fixture.runOperationCommand(t, "bind",
						"--operation", operationID, "--model", model, "--version", version)
					if testCase.selected {
						require.NoError(t, err)
						assert.Equal(t, mlflow.OperationSucceeded, fixture.operation(t, operationID).State)
						var retained []mlflow.NativeDisposition
						require.NoError(t, fixture.store.DB().Where("operation_uuid = ?", operationID).Find(&retained).Error)
						require.Len(t, retained, 1)
						assert.Equal(t, "2", retained[0].NativeVersionNumber)
						return
					}
					require.Error(t, err)
					assert.Contains(t, err.Error(), testCase.message)

					assert.Equal(t, before, fixture.operation(t, operationID),
						"a rejected bind must change no row")
				})
			}
		})
	}
}

// tamperCandidateGeneration rewrites the history generation inside an
// operation's candidate so the recorded generations disagree.
func tamperCandidateGeneration(t *testing.T, fixture *operationsFixture,
	operation mlflow.Operation, generation int64) {
	t.Helper()
	var candidate mlflow.MutationCandidate
	require.NoError(t, json.Unmarshal([]byte(operation.CandidateJSON), &candidate))
	require.Len(t, candidate.History, 1)
	candidate.History[0].Value.Generation = generation
	encoded, err := json.Marshal(candidate)
	require.NoError(t, err)
	require.NoError(t, fixture.store.DB().Model(&mlflow.Operation{}).
		Where("operation_uuid = ?", operation.OperationUUID).
		Update("candidate_json", string(encoded)).Error)
}

// TestMLflowOperationsBindResumesThroughApplyOperation proves a
// verified bind checkpoints the adopted identity and resumes through
// the normal executor: the operation publishes with its verified identity,
// the alias moves, and no second native version
// appears.
func TestMLflowOperationsBindResumesThroughApplyOperation(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			fixture := newOperationsFixture(t, engine)
			operation := fixture.uncertainOperation(t, "bind-model", "s3://bucket/bind.tar.gz", true)

			// The create response was lost, leaving the journal uncertain
			// while the actual native result retains the complete identity.
			fixture.expireOperationLeases(t)

			_, err := fixture.runOperationCommand(t, "bind",
				"--operation", operation.OperationUUID,
				"--model", "bind-model", "--version", "1")
			require.NoError(t, err)

			resolved := fixture.operation(t, operation.OperationUUID)
			assert.Equal(t, mlflow.OperationSucceeded, resolved.State)
			assert.Equal(t, mlflow.StepPublished, resolved.Step)

			binding := fixture.bindingOfVersion(t, versionIDOf(t, fixture, "bind-model"))
			assert.Equal(t, mlflow.PhaseReady, binding.Phase)
			assert.Equal(t, int64(1), binding.ActiveGeneration)
			assert.Equal(t, "bind-model", *binding.NativeModelName)
			assert.Equal(t, "1", *binding.NativeVersionNumber)
			assert.Equal(t, int64(1), fixture.committedArtifacts(t))
			assert.Equal(t, 1, fixture.stub.countVersions("bind-model"),
				"a verified bind must not create a second native version")

			// The resumed executor preserves the verified operation identity.
			assert.Equal(t, operation.OperationUUID, fixture.stubOperationTag("bind-model", "1"))
			aliasVersion, aliased := fixture.stub.modelAliasVersion("bind-model", binding.Alias)
			assert.True(t, aliased, "the adapter-owned alias must point at the bound version")
			assert.Equal(t, "1", aliasVersion)
		})
	}
}

func TestMLflowOperationsBindMissingOperationTagChangesNothing(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			fixture := newOperationsFixture(t, engine)
			operation := fixture.uncertainOperation(t, "missing-operation", "s3://bucket/original", true)
			fixture.stub.stripVersionTag("missing-operation", "1", "rhoai.operation")
			fixture.expireOperationLeases(t)
			snapshot := func() string {
				var state struct {
					Adapters     []mlflow.Adapter
					Objects      []mlflow.Object
					Bindings     []mlflow.VersionBinding
					Links        []mlflow.ArtifactLink
					Histories    []mlflow.VersionHistory
					Operations   []mlflow.Operation
					Reservations []mlflow.IntentResource
				}
				for _, table := range []struct {
					rows  any
					order string
				}{
					{&state.Adapters, "id"}, {&state.Objects, "id"},
					{&state.Bindings, "id"}, {&state.Links, "artifact_id, version_id"},
					{&state.Histories, "version_id, generation"}, {&state.Operations, "operation_uuid"},
					{&state.Reservations, "resource_id"},
				} {
					require.NoError(t, fixture.store.DB().Order(table.order).Find(table.rows).Error)
				}
				encoded, err := json.Marshal(state)
				require.NoError(t, err)
				return string(encoded)
			}
			before := snapshot()
			writes := fixture.stub.mutatingRequests()
			_, err := fixture.runOperationCommand(t, "bind", "--operation", operation.OperationUUID, "--model", "missing-operation", "--version", "1")
			require.ErrorIs(t, err, api.ErrConflict)
			assert.Equal(t, before, snapshot(), "rejected bind changes no SQL or journal evidence")
			assert.Equal(t, writes, fixture.stub.mutatingRequests(), "rejected bind performs no native mutations")
			assert.NotContains(t, fixture.stub.versionTags("missing-operation", "1"), "rhoai.operation")
		})
	}
}

// versionIDOf loads the logical ID of the model version named under a
// model.
func versionIDOf(t *testing.T, fixture *operationsFixture, modelName string) string {
	t.Helper()
	var model, version mlflow.Object
	require.NoError(t, fixture.store.DB().
		Where("kind = ? AND name = ?", mlflow.KindRegisteredModel, modelName).First(&model).Error)
	require.NoError(t, fixture.store.DB().
		Where("kind = ? AND parent_id = ?", mlflow.KindModelVersion, model.ID).First(&version).Error)
	return strconv.Itoa(int(version.ID))
}

// TestMLflowOperationsRetryCreate proves the retry requires the
// duplicate-risk acknowledgment, supersedes the original without
// erasing evidence, links a fresh auditable attempt, and completes it
// through the normal executor.
func TestMLflowOperationsRetryCreate(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			fixture := newOperationsFixture(t, engine)
			operation := fixture.uncertainOperation(t, "retry-model", "s3://bucket/retry.tar.gz", true)

			// The acknowledgment is required and its omission changes
			// nothing.
			_, err := fixture.runOperationCommand(t, "retry-create",
				"--operation", operation.OperationUUID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "accept-duplicate-risk")
			assert.Equal(t, operation, fixture.operation(t, operation.OperationUUID),
				"a rejected retry must change no row")

			// Missing explicit operation selection.
			_, err = fixture.runOperationCommand(t, "retry-create", "--accept-duplicate-risk")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "operation")

			// A terminal operation cannot be retried.
			done := fixture.uncertainOperation(t, "done-retry-model", "s3://bucket/done.tar.gz", true)
			fixture.expireOperationLeases(t)
			require.NoError(t, fixture.service.ApplyOperation(t.Context(), done.OperationUUID))
			_, err = fixture.runOperationCommand(t, "retry-create",
				"--operation", done.OperationUUID, "--accept-duplicate-risk")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "already SUCCEEDED")

			// The create never committed server-side: the retry
			// supersedes the original, links a fresh attempt, and
			// completes it.
			neverCommitted := fixture.uncertainOperation(t, "clean-model", "s3://bucket/clean.tar.gz", false)
			require.Equal(t, 0, fixture.stub.countVersions("clean-model"),
				"the failed create must not have committed")

			_, err = fixture.runOperationCommand(t, "retry-create",
				"--operation", neverCommitted.OperationUUID, "--accept-duplicate-risk")
			require.NoError(t, err)

			superseded := fixture.operation(t, neverCommitted.OperationUUID)
			assert.Equal(t, mlflow.OperationFailed, superseded.State)
			assert.Equal(t, mlflow.StepCreateDispatched, superseded.Step,
				"the dispatch checkpoint must not be erased")
			assert.Nil(t, superseded.NativeResult)
			require.NotNil(t, superseded.FailureCode)
			assert.Equal(t, mlflow.FailureCodeOperatorRetry, *superseded.FailureCode)

			var retry mlflow.Operation
			require.NoError(t, fixture.store.DB().
				Where("parent_operation_uuid = ?", neverCommitted.OperationUUID).First(&retry).Error)
			assert.Equal(t, mlflow.OperationSucceeded, retry.State)
			assert.Equal(t, 1, fixture.stub.countVersions("clean-model"))
			// The completed done-retry artifact and the retried clean
			// artifact are both committed.
			assert.Equal(t, int64(2), fixture.committedArtifacts(t))
			assert.Equal(t, mlflow.PhaseReady, fixture.bindingOfVersion(
				t, versionIDOf(t, fixture, "clean-model")).Phase)
		})
	}
}

// TestMLflowOperationsRetryLeavesOrphanVisible proves that when the
// earlier dispatched create did commit — but without a verifiable
// identity — the retried attempt publishes against its own native
// version while the orphaned duplicate remains visible and undeleted.
func TestMLflowOperationsRetryLeavesOrphanVisible(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			fixture := newOperationsFixture(t, engine)
			operation := fixture.uncertainOperation(t, "orphan-model", "s3://bucket/orphan.tar.gz", true)

			// The committed version lost its operation identity tag, so
			// neither reconciliation nor bind can adopt it.
			fixture.stub.stripVersionTag("orphan-model", "1", "rhoai.operation")

			_, err := fixture.runOperationCommand(t, "retry-create",
				"--operation", operation.OperationUUID, "--accept-duplicate-risk")
			require.NoError(t, err)

			// The original stays superseded — never erased — while the
			// linked retry publishes against its own native version.
			superseded := fixture.operation(t, operation.OperationUUID)
			assert.Equal(t, mlflow.OperationFailed, superseded.State)
			require.NotNil(t, superseded.FailureCode)
			assert.Equal(t, mlflow.FailureCodeOperatorRetry, *superseded.FailureCode)
			var retry mlflow.Operation
			require.NoError(t, fixture.store.DB().
				Where("parent_operation_uuid = ?", operation.OperationUUID).First(&retry).Error)
			assert.Equal(t, mlflow.OperationSucceeded, retry.State)
			assert.Equal(t, 2, fixture.stub.countVersions("orphan-model"),
				"the orphaned native duplicate must remain visible")
			assert.Zero(t, fixture.stub.countRequests("DELETE"),
				"the adapter must never delete a native duplicate")
			assert.Equal(t, int64(1), fixture.committedArtifacts(t))
		})
	}
}

// TestMLflowOperationsWorkerRace proves operator resolution and an
// automatic worker — separate service instances contending through the
// same SQL leases and fencing tokens — converge on one published
// native version without corrupting the journal.
func TestMLflowOperationsWorkerRace(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			fixture := newOperationsFixture(t, engine)
			operation := fixture.uncertainOperation(t, "race-model", "s3://bucket/race.tar.gz", true)
			fixture.expireOperationLeases(t)

			worker, err := mlflow.NewService(mlflow.Config{
				RegistryURI: fixture.stub.url, RequestTimeout: 5 * time.Second,
			}, fixture.store, fixture.native)
			require.NoError(t, err)
			runCtx, cancel := context.WithCancel(t.Context())
			go func() { _ = worker.Run(runCtx) }()
			t.Cleanup(func() {
				cancel()
				require.NoError(t, worker.Close())
			})

			// The operator resolves while the sweep may claim the same
			// operation; whichever wins, exactly one native version is
			// published.
			_, _ = fixture.runOperationCommand(t, "bind",
				"--operation", operation.OperationUUID,
				"--model", "race-model", "--version", "1")

			require.Eventually(t, func() bool {
				return fixture.operation(t, operation.OperationUUID).State == mlflow.OperationSucceeded
			}, 10*time.Second, 50*time.Millisecond, "the operation must complete")
			assert.Equal(t, 1, fixture.stub.countVersions("race-model"),
				"worker and operator must not publish two native versions")
			assert.Equal(t, int64(1), fixture.committedArtifacts(t))
		})
	}
}

// TestMLflowOperationsStoreGuards proves the commands require an
// initialized store bound to the configured registry target.
func TestMLflowOperationsStoreGuards(t *testing.T) {
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			// An uninitialized store is refused with setup guidance.
			fixture := &operationsFixture{engine: engine}
			fixture.reopen(t, false)
			_, err := fixture.runOperationCommand(t, "list")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not initialized")

			// A different registry target is refused before any action.
			initialized := newOperationsFixture(t, engine)
			command := initialized.command(t)
			command.SetArgs([]string{
				"--mlflow-registry-uri", "http://127.0.0.1:1", "list",
			})
			err = command.ExecuteContext(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "different mlflow registry target")
		})
	}
}
