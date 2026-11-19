package mlflow_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/internal/mlflow/client"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/kubeflow/hub/pkg/openapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nativeStub is a protocol-faithful in-memory MLflow registry serving
// the registered-model and model-version endpoints the adapter uses,
// with request recording and create-failure injection.
type nativeStub struct {
	mu      sync.Mutex
	models  map[string]map[string]any // name -> {name, description, tags, aliases}
	request []string
	// versions holds model-version records per model name:
	// version number -> {name, version, source, description, status, tags}.
	versions    map[string]map[string]any
	nextVersion map[string]int
	failCreates int
	// createStatus is the registration status reported for freshly
	// created model versions. flipStatus makes the first status read
	// complete a PENDING_REGISTRATION, simulating async registration.
	// commitThenFail makes the next version create apply server-side
	// and still answer 500, simulating a gateway error after commit.
	// searchPageSize caps entries per search page ( 0 = unlimited ).
	// failNext queues deterministic 500s per request label, and gates
	// block labeled requests until released, simulating slow or stale
	// remote writes for concurrency tests.
	createStatus   string
	flipStatus     bool
	commitThenFail int
	searchPageSize int
	failNext       map[string]int
	badNext        map[string]int
	gates          map[string]chan struct{}
	entered        map[string]chan struct{}
	url            string
}

func newNativeStub(t *testing.T) (*nativeStub, *client.Client) {
	t.Helper()
	stub := &nativeStub{
		models:       map[string]map[string]any{},
		versions:     map[string]map[string]any{},
		nextVersion:  map[string]int{},
		createStatus: "READY",
		failNext:     map[string]int{},
		badNext:      map[string]int{},
		gates:        map[string]chan struct{}{},
		entered:      map[string]chan struct{}{},
	}

	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		require.NoError(t, json.NewEncoder(w).Encode(body))
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
	stubTags := func(model map[string]any) []map[string]string {
		tags := []map[string]string{}
		for key, value := range model["tags"].(map[string]string) {
			tags = append(tags, map[string]string{"key": key, "value": value})
		}
		return tags
	}

	mux.HandleFunc("/api/2.0/mlflow/registered-models/create", func(w http.ResponseWriter, r *http.Request) {
		body := decode(r)
		name := body["name"].(string)
		stub.record("POST registered-models/create")
		if stub.consumeFailure() {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR")
			return
		}
		if _, exists := stub.model(name); exists {
			writeError(w, http.StatusBadRequest, "RESOURCE_ALREADY_EXISTS")
			return
		}
		description, _ := body["description"].(string)
		stub.put(name, map[string]any{
			"name": name, "description": description, "tags": tagMap(body),
			"aliases": map[string]string{},
		})
		writeJSON(w, http.StatusOK, map[string]any{
			"registered_model": map[string]any{"name": name},
		})
	})
	mux.HandleFunc("/api/2.0/mlflow/registered-models/get", func(w http.ResponseWriter, r *http.Request) {
		model, exists := stub.model(r.URL.Query().Get("name"))
		stub.record("GET registered-models/get")
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"registered_model": map[string]any{
				"name": model["name"], "description": model["description"], "tags": stubTags(model),
			},
		})
	})
	mux.HandleFunc("/api/2.0/mlflow/registered-models/search", func(w http.ResponseWriter, r *http.Request) {
		stub.record("GET registered-models/search")
		stub.hold("GET registered-models/search")
		if stub.failLabel("GET registered-models/search") {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR")
			return
		}
		pageToken := r.URL.Query().Get("page_token")
		stub.mu.Lock()
		defer stub.mu.Unlock()
		names := []string{}
		for name := range stub.models {
			names = append(names, name)
		}
		slices.Sort(names)
		entries := []any{}
		for _, name := range names {
			model := stub.models[name]
			entries = append(entries, map[string]any{
				"name": model["name"], "description": model["description"],
				"tags": stubTags(model),
			})
		}
		offset := 0
		if pageToken != "" {
			offset, _ = strconv.Atoi(pageToken)
		}
		if offset > len(entries) {
			offset = len(entries)
		}
		entries = entries[offset:]
		pageSize := stub.searchPageSize
		if pageSize <= 0 || pageSize > len(entries) {
			pageSize = len(entries)
		}
		page := entries[:pageSize]
		rest := entries[pageSize:]
		response := map[string]any{"registered_models": page}
		if len(rest) > 0 {
			response["next_page_token"] = fmt.Sprintf("%d", offset+pageSize)
		}
		writeJSON(w, http.StatusOK, response)
	})
	mux.HandleFunc("/api/2.0/mlflow/registered-models/update", func(w http.ResponseWriter, r *http.Request) {
		body := decode(r)
		model, exists := stub.model(body["name"].(string))
		stub.record("PATCH registered-models/update")
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		model["description"] = body["description"]
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	mux.HandleFunc("/api/2.0/mlflow/registered-models/set-tag", func(w http.ResponseWriter, r *http.Request) {
		body := decode(r)
		stub.hold("POST registered-models/set-tag")
		model, exists := stub.model(body["name"].(string))
		stub.record("POST registered-models/set-tag")
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		model["tags"].(map[string]string)[body["key"].(string)] = body["value"].(string)
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	mux.HandleFunc("/api/2.0/mlflow/registered-models/delete-tag", func(w http.ResponseWriter, r *http.Request) {
		body := decode(r)
		model, exists := stub.model(body["name"].(string))
		stub.record("DELETE registered-models/delete-tag")
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		delete(model["tags"].(map[string]string), body["key"].(string))
		writeJSON(w, http.StatusOK, map[string]any{})
	})

	// Model versions.
	versionOf := func(name, version string) (map[string]any, bool) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		model, exists := stub.versions[name]
		if !exists {
			return nil, false
		}
		record, exists := model[version]
		if !exists {
			return nil, false
		}
		return record.(map[string]any), true
	}
	mux.HandleFunc("/api/2.0/mlflow/model-versions/create", func(w http.ResponseWriter, r *http.Request) {
		body := decode(r)
		name := body["name"].(string)
		stub.record("POST model-versions/create")
		stub.hold("POST model-versions/create")
		if stub.consumeFailure() {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR")
			return
		}
		stub.mu.Lock()
		if _, exists := stub.models[name]; !exists {
			stub.mu.Unlock()
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		if stub.commitThenFail > 0 {
			stub.commitThenFail--
			stub.nextVersion[name]++
			number := fmt.Sprintf("%d", stub.nextVersion[name])
			source, _ := body["source"].(string)
			description, _ := body["description"].(string)
			record := map[string]any{
				"name": name, "version": number, "source": source,
				"description": description, "status": "READY", "tags": tagMap(body),
			}
			if stub.versions[name] == nil {
				stub.versions[name] = map[string]any{}
			}
			stub.versions[name][number] = record
			stub.mu.Unlock()
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR")
			return
		}
		stub.nextVersion[name]++
		number := fmt.Sprintf("%d", stub.nextVersion[name])
		source, _ := body["source"].(string)
		description, _ := body["description"].(string)
		record := map[string]any{
			"name": name, "version": number, "source": source,
			"description": description, "status": stub.createStatus, "tags": tagMap(body),
		}
		if stub.versions[name] == nil {
			stub.versions[name] = map[string]any{}
		}
		stub.versions[name][number] = record
		stub.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"model_version": map[string]any{"name": name, "version": number, "status": stub.createStatus},
		})
	})
	mux.HandleFunc("/api/2.0/mlflow/model-versions/get", func(w http.ResponseWriter, r *http.Request) {
		stub.record(fmt.Sprintf("GET model-versions/get [%s/%s]", r.URL.Query().Get("name"), r.URL.Query().Get("version")))
		stub.hold("GET model-versions/get")
		if stub.failLabel("GET model-versions/get") {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR")
			return
		}
		record, exists := versionOf(r.URL.Query().Get("name"), r.URL.Query().Get("version"))
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		stub.mu.Lock()
		if stub.flipStatus && record["status"] == "PENDING_REGISTRATION" {
			record["status"] = "READY"
		}
		stub.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"model_version": stub.versionBody(record)})
	})
	mux.HandleFunc("/api/2.0/mlflow/model-versions/search", func(w http.ResponseWriter, r *http.Request) {
		stub.record("GET model-versions/search")
		stub.hold("GET model-versions/search")
		if stub.failLabel("GET model-versions/search") {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR")
			return
		}
		filter := r.URL.Query().Get("filter")
		pageToken := r.URL.Query().Get("page_token")
		// Parse the filter exactly as the verified server does: the only
		// supported form is name='<value>' with no quote inside the value.
		// Anything else — including an unescaped quote — is a bad request,
		// so a malformed filter can never silently widen the result set.
		filterName, malformed := "", false
		if filter != "" {
			const prefix = "name='"
			if strings.HasPrefix(filter, prefix) && strings.HasSuffix(filter, "'") && len(filter) > len(prefix)+1 {
				value := filter[len(prefix) : len(filter)-1]
				if strings.Contains(value, "'") {
					malformed = true
				} else {
					filterName = value
				}
			} else {
				malformed = true
			}
		}
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if malformed {
			writeError(w, http.StatusBadRequest, "INVALID_PARAMETER_VALUE")
			return
		}
		// Deterministic order across pages: name, then version number.
		names := []string{}
		for name := range stub.versions {
			if filter == "" || name == filterName {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		entries := []any{}
		for _, name := range names {
			numbers := []string{}
			for number := range stub.versions[name] {
				numbers = append(numbers, number)
			}
			sort.Strings(numbers)
			for _, number := range numbers {
				entries = append(entries, stub.versionBody(stub.versions[name][number].(map[string]any)))
			}
		}
		// Resume at the page token, which is the offset of the next
		// entry.
		offset := 0
		if pageToken != "" {
			offset, _ = strconv.Atoi(pageToken)
		}
		if offset > len(entries) {
			offset = len(entries)
		}
		entries = entries[offset:]
		pageSize := stub.searchPageSize
		if pageSize <= 0 || pageSize > len(entries) {
			pageSize = len(entries)
		}
		page := entries[:pageSize]
		rest := entries[pageSize:]
		response := map[string]any{"model_versions": page}
		if len(rest) > 0 {
			response["next_page_token"] = fmt.Sprintf("%d", offset+pageSize)
		}
		writeJSON(w, http.StatusOK, response)
	})
	mux.HandleFunc("/api/2.0/mlflow/model-versions/update", func(w http.ResponseWriter, r *http.Request) {
		body := decode(r)
		stub.record("PATCH model-versions/update")
		record, exists := versionOf(body["name"].(string), body["version"].(string))
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		record["description"] = body["description"]
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	setVersionTag := func(w http.ResponseWriter, r *http.Request, record bool) {
		body := decode(r)
		if record {
			stub.record("POST model-versions/set-tag")
			if stub.badLabel("POST model-versions/set-tag") {
				writeError(w, http.StatusBadRequest, "INVALID_PARAMETER_VALUE")
				return
			}
			if stub.failLabel("POST model-versions/set-tag") {
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR")
				return
			}
			stub.hold("POST model-versions/set-tag")
		}
		versionRecord, exists := versionOf(body["name"].(string), body["version"].(string))
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		versionRecord["tags"].(map[string]string)[body["key"].(string)] = body["value"].(string)
		writeJSON(w, http.StatusOK, map[string]any{})
	}
	mux.HandleFunc("/api/2.0/mlflow/model-versions/set-tag", func(w http.ResponseWriter, r *http.Request) {
		setVersionTag(w, r, true)
	})
	mux.HandleFunc("/api/2.0/mlflow/model-versions/delete-tag", func(w http.ResponseWriter, r *http.Request) {
		body := decode(r)
		stub.record("DELETE model-versions/delete-tag")
		versionRecord, exists := versionOf(body["name"].(string), body["version"].(string))
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		delete(versionRecord["tags"].(map[string]string), body["key"].(string))
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	mux.HandleFunc("/api/2.0/mlflow/registered-models/alias", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			stub.record("GET registered-models/alias")
			stub.mu.Lock()
			model, exists := stub.models[r.URL.Query().Get("name")]
			var record map[string]any
			if exists {
				version, aliased := model["aliases"].(map[string]string)[r.URL.Query().Get("alias")]
				if aliased {
					record, _ = stub.versions[model["name"].(string)][version].(map[string]any)
				}
			}
			stub.mu.Unlock()
			if record == nil {
				writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"model_version": stub.versionBody(record)})
			return
		}
		body := decode(r)
		stub.record("POST registered-models/alias")
		if stub.failLabel("POST registered-models/alias") {
			writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR")
			return
		}
		stub.hold("POST registered-models/alias")
		stub.mu.Lock()
		model, exists := stub.models[body["name"].(string)]
		if exists {
			model["aliases"].(map[string]string)[body["alias"].(string)] = body["version"].(string)
		}
		stub.mu.Unlock()
		if !exists {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{})
	})

	// Every other registry path — notably tracking endpoints — must
	// never be requested by the adapter.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		stub.record("UNEXPECTED " + r.Method + " " + r.URL.Path)
		writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST")
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	stub.url = server.URL
	native, err := client.New(server.URL, server.Client())
	require.NoError(t, err)
	return stub, native
}

func (s *nativeStub) record(request string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.request = append(s.request, request)
}

func (s *nativeStub) consumeFailure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failCreates > 0 {
		s.failCreates--
		return true
	}
	return false
}

// failNextRequests queues count deterministic 500 responses for one
// request label, simulating a remote error at that exact protocol step.
func (s *nativeStub) failNextRequests(label string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[label] += count
}

// failLabel consumes one queued failure for the label, if any remains.
func (s *nativeStub) failLabel(label string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext[label] > 0 {
		s.failNext[label]--
		return true
	}
	return false
}

// failNextBadRequests queues count definitive 400 responses for one
// request label: an input the registry will always reject.
func (s *nativeStub) failNextBadRequests(label string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.badNext[label] += count
}

// badLabel consumes one queued bad-request failure for the label.
func (s *nativeStub) badLabel(label string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.badNext[label] > 0 {
		s.badNext[label]--
		return true
	}
	return false
}

// gateRequests blocks every labeled request until release is called,
// simulating a slow or stale remote write.
func (s *nativeStub) gateRequests(label string) (release func()) {
	s.mu.Lock()
	gate := make(chan struct{})
	s.gates[label] = gate
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.gates, label)
		s.mu.Unlock()
		close(gate)
	}
}

// hold blocks while a gate is installed for the label.
func (s *nativeStub) hold(label string) {
	s.mu.Lock()
	gate, gated := s.gates[label]
	if entered := s.entered[label]; entered != nil {
		close(entered)
		delete(s.entered, label)
	}
	s.mu.Unlock()
	if gated {
		<-gate
	}
}

func (s *nativeStub) model(name string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	model, exists := s.models[name]
	return model, exists
}

func (s *nativeStub) put(name string, model map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models[name] = model
}

func (s *nativeStub) tags(name string) map[string]string {
	model, exists := s.model(name)
	if !exists {
		panic(fmt.Sprintf("native model %q must exist", name))
	}
	return model["tags"].(map[string]string)
}

// versionBody renders one model-version record as the wire JSON.
func (s *nativeStub) versionBody(record map[string]any) map[string]any {
	tags := []map[string]string{}
	for key, value := range record["tags"].(map[string]string) {
		tags = append(tags, map[string]string{"key": key, "value": value})
	}
	return map[string]any{
		"name": record["name"], "version": record["version"],
		"source": record["source"], "status": record["status"],
		"description": record["description"], "tags": tags,
	}
}

// versionRecord returns the stored model-version record.
func (s *nativeStub) versionRecord(name, version string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.versions[name][version]
	if !exists {
		panic(fmt.Sprintf("native model version %s/%s must exist", name, version))
	}
	return record.(map[string]any)
}

// forgeVersion inserts a native model-version record directly,
// simulating a version created outside the adapter — for example a
// duplicate carrying a replayed operation identity tag.
func (s *nativeStub) forgeVersion(name, number string, tags map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions[name] == nil {
		s.versions[name] = map[string]any{}
	}
	s.versions[name][number] = map[string]any{
		"name": name, "version": number, "source": "s3://bucket/forged.tar.gz",
		"description": "", "status": "READY", "tags": tags,
	}
}

// versionTags returns the native tags of one model version.
func (s *nativeStub) versionTags(name, version string) map[string]string {
	return s.versionRecord(name, version)["tags"].(map[string]string)
}

// aliases returns the native aliases of one registered model.
func (s *nativeStub) aliases(name string) map[string]string {
	model, exists := s.model(name)
	if !exists {
		panic(fmt.Sprintf("native model %q must exist", name))
	}
	return model["aliases"].(map[string]string)
}

func (s *nativeStub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.request...)
}

func (s *nativeStub) countRequests(prefix string) int {
	count := 0
	for _, request := range s.requests() {
		if len(request) >= len(prefix) && request[:len(prefix)] == prefix {
			count++
		}
	}
	return count
}

// backendFixture combines a SQL store fixture, a protocol-faithful
// native registry stub, and an initialized service.
type backendFixture struct {
	*storeFixture
	stub    *nativeStub
	native  *client.Client
	service *mlflow.Service
}

func newBackendFixture(t *testing.T, fixture *storeFixture) *backendFixture {
	t.Helper()
	fixture.cleanup(t)
	stub, native := newNativeStub(t)

	store := mlflow.NewStore(fixture.db)
	fingerprint, err := mlflow.RegistryTargetFingerprint(stub.url)
	require.NoError(t, err)
	_, err = store.InitializeAdapter(t.Context(), fingerprint)
	require.NoError(t, err)

	service, err := mlflow.NewService(mlflow.Config{
		RegistryURI: stub.url, RequestTimeout: 5 * time.Second,
	}, store, native)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	return &backendFixture{storeFixture: fixture, stub: stub, native: native, service: service}
}

// newBackendService builds a second service and store over the same
// database, proving restart persistence.
func (f *backendFixture) newBackendService(t *testing.T) *mlflow.Service {
	t.Helper()
	service, err := mlflow.NewService(mlflow.Config{
		RegistryURI: f.stub.url, RequestTimeout: 5 * time.Second,
	}, mlflow.NewStore(f.db), f.native)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	return service
}

func liveState() *openapi.RegisteredModelState {
	state := openapi.REGISTEREDMODELSTATE_LIVE
	return &state
}

// TestAdapterIdentityFingerprint proves target canonicalization:
// scheme, host, and base path identify the registry; default ports,
// case, and trailing slashes do not; credentials, queries, and
// fragments are rejected.
func TestAdapterIdentityFingerprint(t *testing.T) {
	base, err := mlflow.RegistryTargetFingerprint("https://Registry.Example.com:443/mlflow/")
	require.NoError(t, err)

	for _, same := range []string{
		"https://registry.example.com/mlflow",
		"https://registry.example.com:443/mlflow/",
		"HTTPS://registry.example.com/mlflow",
	} {
		fingerprint, err := mlflow.RegistryTargetFingerprint(same)
		require.NoError(t, err, "%s", same)
		assert.Equal(t, base, fingerprint, "%s must canonicalize identically", same)
	}

	for _, differs := range []string{
		"https://registry.example.com/mlflow/v2",
		"http://registry.example.com/mlflow",
		"https://other.example.com/mlflow",
		"https://registry.example.com:8443/mlflow",
		"https://registry.example.com",
	} {
		fingerprint, err := mlflow.RegistryTargetFingerprint(differs)
		require.NoError(t, err, "%s", differs)
		assert.NotEqual(t, base, fingerprint, "%s must differ", differs)
	}

	for _, rejected := range []string{
		"https://user:secret@registry.example.com/mlflow",
		"https://registry.example.com/mlflow?token=x",
		"https://registry.example.com/mlflow#fragment",
		"ftp://registry.example.com",
		"not a url",
		"",
	} {
		_, err := mlflow.RegistryTargetFingerprint(rejected)
		require.ErrorIs(t, err, api.ErrBadRequest, "%q must be rejected", rejected)
	}
}

// TestAdapterIdentityInitialization proves the adapter singleton is
// transactionally initialized: replicas converge on one identity, a
// different target fails without changing it, and concurrent
// initialization is serialized.
func TestAdapterIdentityInitialization(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			fixture.cleanup(t)
			store := mlflow.NewStore(fixture.db)

			_, err := store.GetAdapter(t.Context())
			require.ErrorIs(t, err, api.ErrNotFound)

			first, err := store.InitializeAdapter(t.Context(), "fingerprint-a")
			require.NoError(t, err)
			require.NotEmpty(t, first.AdapterUUID)
			require.Equal(t, "fingerprint-a", first.TargetFingerprint)

			again, err := store.InitializeAdapter(t.Context(), "fingerprint-a")
			require.NoError(t, err)
			require.Equal(t, first.AdapterUUID, again.AdapterUUID)

			_, err = store.InitializeAdapter(t.Context(), "fingerprint-b")
			require.ErrorIs(t, err, api.ErrConflict)
			unchanged, err := store.GetAdapter(t.Context())
			require.NoError(t, err)
			require.Equal(t, first.AdapterUUID, unchanged.AdapterUUID)
			require.Equal(t, "fingerprint-a", unchanged.TargetFingerprint)

			// Concurrent replicas serialize on the migration singleton
			// row and obtain the same identity.
			second := mlflow.NewStore(fixture.db)
			var wg sync.WaitGroup
			identities := make([]string, 4)
			for i := range identities {
				wg.Go(func() {
					adapter, err := second.InitializeAdapter(t.Context(), "fingerprint-a")
					if err == nil {
						identities[i] = adapter.AdapterUUID
					}
				})
			}
			wg.Wait()
			for _, identity := range identities {
				require.Equal(t, first.AdapterUUID, identity)
			}
		})
	}
}

// TestRegisteredModelCreateNativeSync proves a create projects name,
// description, owner, external ID, state, and custom properties onto
// the native registry with adapter ownership tags, and that the
// committed view appears only after the native create succeeds.
func TestRegisteredModelCreateNativeSync(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)

			created, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name:        "fraud-detector",
				Description: new("detects fraud"),
				Owner:       new("data-science"),
				ExternalId:  new("ext-1"),
				State:       liveState(),
				CustomProperties: map[string]openapi.MetadataValue{
					"accuracy":   doubleValue(0.99),
					"tier":       stringValue("gold"),
					"rhoai.evil": stringValue("escaped"),
				},
			})
			require.NoError(t, err)
			require.NotNil(t, created.Id)
			require.Equal(t, "fraud-detector", created.Name)
			require.Equal(t, "detects fraud", *created.Description)
			require.Equal(t, "data-science", *created.Owner)
			require.Equal(t, "ext-1", *created.ExternalId)
			require.NotNil(t, created.CreateTimeSinceEpoch)
			require.NotNil(t, created.LastUpdateTimeSinceEpoch)
			require.Len(t, created.CustomProperties, 3)

			byID, err := backend.service.GetRegisteredModelById(*created.Id)
			require.NoError(t, err)
			require.Equal(t, created, byID)

			// The native model carries the projection and the creation
			// tags identifying adapter, operation, and logical ID.
			tags := backend.stub.tags("fraud-detector")
			assert.Equal(t, "data-science", tags["rhoai.owner"])
			assert.Equal(t, "ext-1", tags["rhoai.externalId"])
			assert.Equal(t, string(openapi.REGISTEREDMODELSTATE_LIVE), tags["rhoai.state"])
			assert.Equal(t, "0.99", tags["accuracy"])
			assert.Equal(t, "gold", tags["tier"])
			assert.NotContains(t, tags, "rhoai.evil", "reserved keys must be escaped")
			escaped := 0
			for key, value := range tags {
				if len(key) > len("rhoai.custom.") && key[:len("rhoai.custom.")] == "rhoai.custom." {
					escaped++
					assert.Equal(t, "escaped", value)
				}
			}
			assert.Equal(t, 1, escaped, "the reserved key is escaped exactly once")
			assert.NotEmpty(t, tags["rhoai.adapter"])
			assert.Equal(t, *created.Id, tags["rhoai.logicalId"])
			assert.NotEmpty(t, tags["rhoai.operation"])

			model, exists := backend.stub.model("fraud-detector")
			require.True(t, exists)
			assert.Equal(t, "detects fraud", model["description"])

			require.Equal(t, 1, backend.stub.countRequests("POST registered-models/create"))
		})
	}
}

// TestRegisteredModelCreateConflicts proves external-ID duplicates
// conflict in SQL, and a native name owned by an unrelated model is
// never overwritten or adopted: the logical row stays invisible and
// the native model is untouched.
func TestRegisteredModelCreateConflicts(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)

			_, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name: "first", ExternalId: new("shared-ext"),
			})
			require.NoError(t, err)

			_, err = backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name: "second", ExternalId: new("shared-ext"),
			})
			require.ErrorIs(t, err, api.ErrConflict)

			// A native model with the requested name but no adapter
			// ownership tags is unrelated: refuse, never adopt.
			backend.stub.put("stranger", map[string]any{
				"name": "stranger", "description": "", "tags": map[string]string{"owner": "someone-else"},
			})
			_, err = backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "stranger"})
			require.ErrorIs(t, err, api.ErrConflict)
			tags := backend.stub.tags("stranger")
			assert.Equal(t, map[string]string{"owner": "someone-else"}, tags,
				"the unrelated native model must be untouched")

			// The refused create left no visible logical row.
			var reserved int64
			require.NoError(t, backend.db.Model(&mlflow.Object{}).
				Where("kind = ? AND name = ? AND visibility = ?",
					mlflow.KindRegisteredModel, "stranger", mlflow.VisibilityReserved).
				Count(&reserved).Error)
			require.Equal(t, int64(1), reserved, "the reserved row stays invisible until publication")
		})
	}
}

// TestRegisteredModelCreateNativeFailureVisibility proves a failed
// native create leaves no visible resource, and that a retry of the
// same create resumes the recorded intent and succeeds once the
// native registry recovers.
func TestRegisteredModelCreateNativeFailureVisibility(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)
			backend.stub.mu.Lock()
			backend.stub.failCreates = 1
			backend.stub.mu.Unlock()

			created, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name: "resilient", Owner: new("team"),
			})
			require.ErrorIs(t, err, api.ErrUnavailable)
			require.Nil(t, created)

			// Nothing is visible after the failed native create.
			var committed int64
			require.NoError(t, backend.db.Model(&mlflow.Object{}).
				Where("kind = ? AND name = ?", mlflow.KindRegisteredModel, "resilient").
				Count(&committed).Error)
			require.Equal(t, int64(1), committed, "the reserved row exists")
			require.NoError(t, backend.db.Model(&mlflow.Object{}).
				Where("kind = ? AND name = ? AND visibility = ?",
					mlflow.KindRegisteredModel, "resilient", mlflow.VisibilityCommitted).
				Count(&committed).Error)
			require.Zero(t, committed, "but stays invisible")

			// The retry resumes the recorded intent rather than
			// deadlocking on it.
			retried, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name: "resilient", Owner: new("team"),
			})
			require.NoError(t, err)
			require.Equal(t, "resilient", retried.Name)
			require.Equal(t, "team", *retried.Owner)
			require.Equal(t, 2, backend.stub.countRequests("POST registered-models/create"))
		})
	}
}

// TestRegisteredModelUpdateNativeSync proves metadata updates
// synchronize the native description and tag projection — deleting
// removed keys, setting changed ones — while preserving creation tags
// and the protected name.
func TestRegisteredModelUpdateNativeSync(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)

			created, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name:        "updatable",
				Description: new("before"),
				Owner:       new("team-a"),
				CustomProperties: map[string]openapi.MetadataValue{
					"keep": stringValue("yes"), "drop": stringValue("no"),
				},
			})
			require.NoError(t, err)

			updated, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Id:          created.Id,
				Name:        "updatable",
				Description: new("after"),
				Owner:       new("team-b"),
				CustomProperties: map[string]openapi.MetadataValue{
					"keep": stringValue("yes"), "added": intValue("7"),
				},
			})
			require.NoError(t, err)
			require.Equal(t, *created.Id, *updated.Id)
			require.Equal(t, "after", *updated.Description)
			require.Equal(t, "team-b", *updated.Owner)
			require.Len(t, updated.CustomProperties, 2)

			tags := backend.stub.tags("updatable")
			assert.Equal(t, "after", backendModelDescription(backend, "updatable"), "description synchronized")
			assert.Equal(t, "team-b", tags["rhoai.owner"])
			assert.Equal(t, "yes", tags["keep"])
			assert.Equal(t, "7", tags["added"])
			assert.NotContains(t, tags, "drop", "removed keys are deleted natively")
			assert.NotEmpty(t, tags["rhoai.adapter"], "creation tags survive updates")
			assert.Equal(t, *created.Id, tags["rhoai.logicalId"])

			// The name is a protected identity: a supplied rename is
			// advisory and the stored name is kept.
			renamed, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{
				Name: "renamed", Id: created.Id,
			})
			require.NoError(t, err)
			require.Equal(t, "updatable", renamed.Name)

			// A second service over the same database sees the update.
			other := backend.newBackendService(t)
			restarted, err := other.GetRegisteredModelById(*created.Id)
			require.NoError(t, err)
			require.Equal(t, "after", *restarted.Description)
		})
	}
}

func backendModelDescription(f *backendFixture, name string) string {
	model, _ := f.stub.model(name)
	return model["description"].(string)
}

// TestRegisteredModelProtectedAndOmittedMaps proves omitted property
// maps behave like empty ones, and unknown-id lookups fail cleanly.
func TestRegisteredModelProtectedAndOmittedMaps(t *testing.T) {
	for engine, fixture := range newStoreFixtures(t) {
		t.Run(engine, func(t *testing.T) {
			backend := newBackendFixture(t, fixture)

			bare, err := backend.service.UpsertRegisteredModel(&openapi.RegisteredModel{Name: "bare"})
			require.NoError(t, err)
			require.Empty(t, bare.CustomProperties)
			tags := backend.stub.tags("bare")
			assert.NotEmpty(t, tags["rhoai.adapter"])

			_, err = backend.service.GetRegisteredModelById("999999999")
			require.ErrorIs(t, err, api.ErrNotFound)
			_, err = backend.service.GetRegisteredModelById("not-an-id")
			require.ErrorIs(t, err, api.ErrBadRequest)
			_, err = backend.service.GetRegisteredModelById(fmt.Sprintf("%d", 0))
			require.ErrorIs(t, err, api.ErrBadRequest)
		})
	}
}

// requestBarrier signals actual dispatch before blocking, without a timing poll.
func (s *nativeStub) requestBarrier(label string) (<-chan struct{}, func()) {
	release := s.gateRequests(label)
	entered := make(chan struct{})
	s.mu.Lock()
	s.entered[label] = entered
	s.mu.Unlock()
	var once sync.Once
	return entered, func() { once.Do(release) }
}
