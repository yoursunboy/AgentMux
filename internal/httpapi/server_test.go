package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/config"
	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/storage"
)

// harness is a fully wired API server backed by a temp data directory and a
// temp Projects Root.
//
// This is deliberately an integration harness rather than a mock: the routing,
// the request decoding, the service rules, and the SQLite statements are all
// exercised together, which is the only way a mis-wired handler is caught.
type harness struct {
	t       *testing.T
	server  *Server
	root    string
	dataDir string
	store   *storage.Store

	// backend is the session backend the server's runtime manager talks to. It
	// is a test double; see fakeBackend.
	backend *fakeBackend

	// runtime is the same manager the server holds, kept here so a test can
	// read the sequence numbers and states the handlers produced.
	runtime *session.Manager
}

// harnessOptions describes the environment a test wants its server to believe
// it is running in.
//
// The runtime environment is pinned rather than detected, so that the suite
// asserts the same things on Windows and inside WSL. A test that asked the real
// host adapter whether tmux is installed would pass or fail depending on the
// machine running it, which is the opposite of what a test is for. The real
// question - is tmux there, and does a session really survive a restart - is
// answered by the integration run, not here.
type harnessOptions struct {
	// prepare populates the Projects Root before the server is built.
	prepare func(root string)

	// runtimeAvailable says whether this server's environment can host a
	// terminal runtime. The default, false, is the Windows-native case: a
	// server that can manage projects and cannot host a session.
	//
	// When it is true the tmux probe is pinned as available too, because "this
	// environment can host a runtime" means both, and a test that wanted them
	// to disagree would be testing a state that does not exist.
	runtimeAvailable bool

	// tmuxMissing is the third case on its own: an environment that could host
	// a runtime with a tmux that is not installed in it.
	tmuxMissing bool

	// debugAPI turns on the diagnostic runtime endpoints.
	debugAPI bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessOpts(t, harnessOptions{})
}

// newHarnessWith builds a harness whose Projects Root is pre-populated by
// prepare, which receives the root path.
func newHarnessWith(t *testing.T, prepare func(root string)) *harness {
	t.Helper()
	return newHarnessOpts(t, harnessOptions{prepare: prepare})
}

func newHarnessOpts(t *testing.T, o harnessOptions) *harness {
	t.Helper()

	root := t.TempDir()
	dataDir := t.TempDir()
	if o.prepare != nil {
		o.prepare(root)
	}

	cfg, err := config.Load(config.LoadOptions{
		Defaults: config.Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
		// An empty environment, so a stray AGENTMUX_* variable in the
		// developer's shell cannot change what the tests assert.
		Environ: func(string) (string, bool) { return "", false },
		Overrides: config.Overrides{
			DebugAPI: o.debugAPI,
		},
	})
	if err != nil {
		t.Fatalf("config.Load returned an error: %v", err)
	}

	store, err := storage.Open(context.Background(), storage.OpenOptions{Path: cfg.SQLitePath()})
	if err != nil {
		t.Fatalf("storage.Open returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("closing the store failed: %v", err)
		}
	})
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate returned an error: %v", err)
	}
	// main.go creates the installation id on every start. Doing the same here
	// keeps the harness honest about what a running server's database contains,
	// which is what lets a test assert the value is stored and not returned.
	if _, err := store.Settings().GetOrCreate(context.Background(), storage.SettingInstallID,
		func() (string, error) { return "inst_test", nil }); err != nil {
		t.Fatalf("could not create the installation id: %v", err)
	}

	real, err := host.New(host.Options{Mode: config.RuntimeModeNative, Roots: []string{root}})
	if err != nil {
		t.Fatalf("host.New returned an error: %v", err)
	}
	// Everything about the host is real except the facts the tests choose:
	// whether a terminal runtime can run here, and whether tmux is installed.
	adapter := pinnedHost{
		Adapter:          real,
		runtimeAvailable: o.runtimeAvailable,
		tmuxAvailable:    o.runtimeAvailable && !o.tmuxMissing,
	}

	service, err := project.NewService(project.Options{
		Repository: store.Projects(),
		Host:       adapter,
		Logger:     discardLogger(),
	})
	if err != nil {
		t.Fatalf("project.NewService returned an error: %v", err)
	}
	discoverer, err := project.NewDiscoverer(project.DiscovererOptions{
		Host:   adapter,
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("project.NewDiscoverer returned an error: %v", err)
	}

	backend := newFakeBackend()
	manager, err := session.NewManager(session.ManagerOptions{
		Backend:  backend,
		Projects: service,
		Store:    store.Runtimes(),
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("session.NewManager returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("closing the runtime manager failed: %v", err)
		}
	})

	server, err := New(Options{
		Config:     cfg,
		Host:       adapter,
		Projects:   service,
		Discoverer: discoverer,
		Runtime:    manager,
		Logger:     discardLogger(),
		StartedAt:  time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC),
		WebDir:     "",
		Now:        func() time.Time { return time.Date(2026, time.September, 17, 12, 0, 30, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("httpapi.New returned an error: %v", err)
	}
	return &harness{
		t:       t,
		server:  server,
		root:    root,
		dataDir: dataDir,
		store:   store,
		backend: backend,
		runtime: manager,
	}
}

// discardLogger keeps request records out of the test output, so a failure is
// the only thing on screen.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// call performs a request against the server.
func (h *harness) call(method, target string, body string, headers ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	recorder := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(recorder, req)
	return recorder
}

// decode reads a JSON body and fails the test if it is not JSON.
func decode[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if ct := recorder.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON; body was %q", ct, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &out); err != nil {
		t.Fatalf("the response body is not valid JSON: %v; body was %q", err, recorder.Body.String())
	}
	return out
}

// errorResponse is the failing-response envelope.
type errorResponse struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func (h *harness) wantError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) errorResponse {
	t.Helper()
	if recorder.Code != status {
		t.Errorf("status = %d, want %d; body was %s", recorder.Code, status, recorder.Body.String())
	}
	body := decode[errorResponse](t, recorder)
	if body.Error.Code != code {
		t.Errorf("error code = %q, want %q", body.Error.Code, code)
	}
	if body.Error.Message == "" {
		t.Error("the error body carries no message for the user")
	}
	return body
}

// write creates a file below the Projects Root.
func (h *harness) write(relPath, content string) string {
	h.t.Helper()
	full := filepath.Join(h.root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		h.t.Fatalf("could not create %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		h.t.Fatalf("could not create %s: %v", full, err)
	}
	return full
}

// mkdir creates a directory below the Projects Root.
func (h *harness) mkdir(relPath string) string {
	h.t.Helper()
	full := filepath.Join(h.root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(full, 0o755); err != nil {
		h.t.Fatalf("could not create %s: %v", full, err)
	}
	return full
}

// ---------------------------------------------------------------------------
// GET /api/server
// ---------------------------------------------------------------------------

func TestServerInfoReportsTheFoundation(t *testing.T) {
	h := newHarness(t)
	recorder := h.call(http.MethodGet, "/api/server", "")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	info := decode[serverInfoResponse](t, recorder)

	if info.AppName != "AgentMux" {
		t.Errorf("appName = %q, want %q", info.AppName, "AgentMux")
	}
	if info.Version == "" {
		t.Error("version is empty")
	}
	if info.Phase == "" {
		t.Error("phase is empty; a client must be able to tell which roadmap phase this server implements")
	}
	if info.Status != "online" {
		t.Errorf("status = %q, want %q", info.Status, "online")
	}
	if info.ProjectsRoot != h.root {
		t.Errorf("projectsRoot = %q, want %q", info.ProjectsRoot, h.root)
	}
	if len(info.ProjectsRoots) != 1 || info.ProjectsRoots[0] != h.root {
		t.Errorf("projectsRoots = %v, want [%q]", info.ProjectsRoots, h.root)
	}
	if info.DataDirectory != h.dataDir {
		t.Errorf("dataDirectory = %q, want %q", info.DataDirectory, h.dataDir)
	}
	if info.DatabasePath != filepath.Join(h.dataDir, "agentmux.db") {
		t.Errorf("databasePath = %q, want it inside the data directory", info.DatabasePath)
	}
	if info.UptimeSeconds != 30 {
		t.Errorf("uptimeSeconds = %d, want 30 from the injected clock", info.UptimeSeconds)
	}
	if info.StartedAt == "" {
		t.Error("startedAt is empty")
	}
}

// TestServerInfoDoesNotClaimATerminal is the honesty check. A build that
// contains the runtime still must not claim a terminal works on a host that
// cannot run one, and the provider switch must stay off until it is integrated.
func TestServerInfoDoesNotClaimATerminal(t *testing.T) {
	h := newHarness(t)
	info := decode[serverInfoResponse](t, h.call(http.MethodGet, "/api/server", ""))

	if !info.TerminalRuntimeImplemented {
		t.Error("terminalRuntimeImplemented is false, but Phase 2 contains the runtime")
	}
	if info.RuntimeAvailable {
		t.Error("runtimeAvailable is true on a host pinned to the Windows-native case")
	}
	if info.Features["terminal"] {
		t.Error("features.terminal is true, but this server cannot host a terminal")
	}
	if info.Provider.Integrated {
		t.Error("provider.integrated is true, but CC Switch is not integrated")
	}
	if info.Provider.Status == "" || info.Provider.Tool == "" {
		t.Error("the provider block must name the tool and its status so the UI can explain the disabled switch")
	}
	for _, feature := range []string{"projectRegistration", "projectCreation", "projectDiscovery"} {
		if !info.Features[feature] {
			t.Errorf("features.%s is false, but Phase 1 implements it", feature)
		}
	}
	// The controller features belong to a later phase and must stay off, so the
	// UI does not offer a control that has nothing behind it.
	for _, feature := range []string{"providerSwitch", "claudeHooks", "controllerTransfer"} {
		if info.Features[feature] {
			t.Errorf("features.%s is true, but it is not implemented yet", feature)
		}
	}
}

// TestServerInfoCarriesNoSecrets is the security check on the one endpoint
// that always answers. Nothing resembling a credential may appear in it.
func TestServerInfoCarriesNoSecrets(t *testing.T) {
	h := newHarness(t)
	recorder := h.call(http.MethodGet, "/api/server", "")

	// The harness's own temp paths are scaffolding, not server output, and
	// t.TempDir() embeds the test name - which contains the word this test
	// searches for. Redact them, in the escaped form they take inside a JSON
	// string, so the check is about the response rather than about where the
	// test ran.
	body := recorder.Body.String()
	for _, path := range []string{h.root, h.dataDir} {
		body = strings.ReplaceAll(body, escapedJSON(path), "<redacted>")
	}
	body = strings.ToLower(body)

	for _, forbidden := range []string{
		"apikey", "api_key", "api-key",
		"token", "secret", "password", "credential",
		"authorization", "bearer", "anthropic_api",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("GET /api/server mentions %q; this endpoint must carry no secrets", forbidden)
		}
	}
}

func TestServerInfoReportsTheRuntimeAndTheHostSeparately(t *testing.T) {
	h := newHarness(t)
	info := decode[serverInfoResponse](t, h.call(http.MethodGet, "/api/server", ""))

	if info.Host == "" || info.RuntimeMode == "" || info.RuntimeOS == "" {
		t.Errorf("host/runtime fields are incomplete: host=%q mode=%q runtimeOs=%q",
			info.Host, info.RuntimeMode, info.RuntimeOS)
	}
	if info.RuntimeMode != string(host.RuntimeNative) {
		t.Errorf("runtimeMode = %q, want %q for a server built in native mode", info.RuntimeMode, host.RuntimeNative)
	}
	if info.PathMapper == "" {
		t.Error("pathMapper is empty; a client needs to know how paths are translated")
	}
}

// TestServerInfoReportsDependenciesAsAList keeps the shape stable: the frontend
// iterates it, so nil would render as a crash rather than an empty list.
func TestServerInfoReportsDependenciesAsAList(t *testing.T) {
	h := newHarness(t)
	info := decode[serverInfoResponse](t, h.call(http.MethodGet, "/api/server", ""))

	if info.Dependencies == nil {
		t.Fatal("dependencies is null, want a list")
	}
	for _, dep := range info.Dependencies {
		if dep.Name == "" {
			t.Error("a dependency entry has no name")
		}
	}
}

func TestServerInfoReportsWarningsAsAList(t *testing.T) {
	h := newHarness(t)
	info := decode[serverInfoResponse](t, h.call(http.MethodGet, "/api/server", ""))

	if info.Warnings == nil {
		t.Error("warnings is null, want a list so the UI can iterate it")
	}
}

// ---------------------------------------------------------------------------
// GET /api/projects
// ---------------------------------------------------------------------------

func TestListProjectsOnAFreshServer(t *testing.T) {
	h := newHarness(t)
	recorder := h.call(http.MethodGet, "/api/projects", "")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	// An empty list, not null: the client maps over it unconditionally.
	if !strings.Contains(recorder.Body.String(), `"projects":[]`) {
		t.Errorf("body = %s, want an empty array rather than null", recorder.Body.String())
	}
	body := decode[projectListResponse](t, recorder)
	if body.Count != 0 {
		t.Errorf("count = %d, want 0", body.Count)
	}
}

func TestListProjectsHidesArchivedByDefault(t *testing.T) {
	h := newHarness(t)
	h.register("App")
	h.register("Hidden")

	view := decode[projectListResponse](t, h.call(http.MethodGet, "/api/projects", ""))
	if view.Count != 2 {
		t.Fatalf("count = %d, want 2", view.Count)
	}

	// Archive one project directly through the store, which is what the
	// service would do once an archive endpoint exists.
	p := view.Projects[0]
	p.Archived = true
	if err := h.store.Projects().Update(context.Background(), p); err != nil {
		t.Fatalf("archiving failed: %v", err)
	}

	visible := decode[projectListResponse](t, h.call(http.MethodGet, "/api/projects", ""))
	if visible.Count != 1 {
		t.Errorf("count = %d, want 1 after archiving one project", visible.Count)
	}

	all := decode[projectListResponse](t, h.call(http.MethodGet, "/api/projects?includeArchived=true", ""))
	if all.Count != 2 {
		t.Errorf("count = %d with includeArchived=true, want 2", all.Count)
	}
}

// ---------------------------------------------------------------------------
// POST /api/projects/register
// ---------------------------------------------------------------------------

// register registers an existing folder through the API and returns it.
func (h *harness) register(relPath string) *project.Project {
	h.t.Helper()
	dir := h.mkdir(relPath)
	recorder := h.call(http.MethodPost, "/api/projects/register",
		`{"hostPath":`+jsonString(dir)+`}`)
	if recorder.Code != http.StatusCreated {
		h.t.Fatalf("registering %q returned status %d: %s", dir, recorder.Code, recorder.Body.String())
	}
	return decode[projectResponse](h.t, recorder).Project
}

func TestRegisterCreatesAProject(t *testing.T) {
	h := newHarness(t)
	target := h.mkdir(filepath.FromSlash("2026 AgentMux/AgentMux"))

	recorder := h.call(http.MethodPost, "/api/projects/register",
		`{"hostPath":`+jsonString(target)+`}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body was %s", recorder.Code, recorder.Body.String())
	}

	p := decode[projectResponse](t, recorder).Project
	if p == nil {
		t.Fatal("the response carries no project")
	}
	if p.Name != "AgentMux" {
		t.Errorf("name = %q, want the directory name", p.Name)
	}
	if p.HostPath != target {
		t.Errorf("hostPath = %q, want %q", p.HostPath, target)
	}
	if p.CollectionPath != filepath.Dir(target) {
		t.Errorf("collectionPath = %q, want %q", p.CollectionPath, filepath.Dir(target))
	}
	if p.RuntimePath == "" {
		t.Error("runtimePath is empty; it must be resolved at registration time")
	}
	if p.Status != project.StatusStopped {
		t.Errorf("status = %q, want %q in Phase 1", p.Status, project.StatusStopped)
	}
	if !project.ValidID(p.ID) {
		t.Errorf("id = %q, which is not a valid project identifier", p.ID)
	}
}

func TestRegisterAcceptsAnExplicitName(t *testing.T) {
	h := newHarness(t)
	target := h.mkdir("checkout")

	recorder := h.call(http.MethodPost, "/api/projects/register",
		`{"hostPath":`+jsonString(target)+`,"name":"Readable"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body was %s", recorder.Code, recorder.Body.String())
	}
	if p := decode[projectResponse](t, recorder).Project; p.Name != "Readable" {
		t.Errorf("name = %q, want %q", p.Name, "Readable")
	}
}

// TestRegisterIsIdempotent is the rule that a second registration must not
// create a second project.
func TestRegisterIsIdempotent(t *testing.T) {
	h := newHarness(t)
	target := h.mkdir("App")

	first := h.call(http.MethodPost, "/api/projects/register", `{"hostPath":`+jsonString(target)+`}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("the first registration returned status %d: %s", first.Code, first.Body.String())
	}

	second := h.call(http.MethodPost, "/api/projects/register", `{"hostPath":`+jsonString(target)+`}`)
	body := h.wantError(t, second, http.StatusConflict, project.CodeAlreadyRegistered)
	if body.Error.Details["project"] == nil {
		t.Error("the conflict carries no details.project, so the UI cannot show what already exists")
	}
	// The client reads these two directly: the path to name in its message, and
	// the id to open what already exists.
	if got, want := body.Error.Details["hostPath"], target; got != want {
		t.Errorf("details.hostPath = %v, want %q", got, want)
	}
	if id, _ := body.Error.Details["projectId"].(string); !project.ValidID(id) {
		t.Errorf("details.projectId = %v, want a project id", body.Error.Details["projectId"])
	}
	// A status that is derived must be derived on every path out, including the
	// copy carried inside an error.
	embedded, ok := body.Error.Details["project"].(map[string]any)
	if !ok {
		t.Fatalf("details.project is %T, want an object", body.Error.Details["project"])
	}
	if embedded["status"] != project.StatusStopped {
		t.Errorf("details.project.status = %v, want %q", embedded["status"], project.StatusStopped)
	}

	list := decode[projectListResponse](t, h.call(http.MethodGet, "/api/projects", ""))
	if list.Count != 1 {
		t.Errorf("count = %d after registering the same folder twice, want 1", list.Count)
	}
}

// TestCreateExplainsAMissingCollection covers the one create failure a user is
// likely to hit: naming a collection that does not exist yet. AgentMux never
// invents parent folders, so the message has to say what to do instead.
func TestCreateExplainsAMissingCollection(t *testing.T) {
	h := newHarness(t)
	missing := filepath.Join(h.root, "Not Made Yet")

	recorder := h.call(http.MethodPost, "/api/projects",
		`{"name":"App","collectionPath":`+jsonString(missing)+`}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body was %s", recorder.Code, recorder.Body.String())
	}
	message := decode[errorResponse](t, recorder).Error.Message
	if !strings.Contains(message, "create that folder first") {
		t.Errorf("message = %q, want it to say how to proceed", message)
	}
}

func TestRegisterRejectsAnUnusablePath(t *testing.T) {
	h := newHarness(t)
	outside := t.TempDir()

	tests := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		// A missing path is a malformed request, not a missing directory: the
		// handler can tell the difference and says so.
		{"missing body", `{}`, http.StatusBadRequest, project.CodeInvalidInput},
		{"an empty path", `{"hostPath":""}`, http.StatusBadRequest, project.CodeInvalidInput},
		{"a blank path", `{"hostPath":"   "}`, http.StatusBadRequest, project.CodeInvalidInput},
		{"a path that does not exist",
			`{"hostPath":` + jsonString(filepath.Join(h.root, "nope")) + `}`,
			http.StatusNotFound, project.CodePathNotFound},
		{"a path outside every root",
			`{"hostPath":` + jsonString(outside) + `}`,
			http.StatusForbidden, project.CodeOutsideProjectsRoot},
		{"the Projects Root itself",
			`{"hostPath":` + jsonString(h.root) + `}`,
			http.StatusBadRequest, project.CodePathIsProjectsRoot},
		{"an illegal name",
			`{"hostPath":` + jsonString(h.mkdir("App")) + `,"name":"bad/name"}`,
			http.StatusBadRequest, project.CodeInvalidName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h.wantError(t, h.call(http.MethodPost, "/api/projects/register", tt.body), tt.status, tt.code)
		})
	}
}

func TestRegisterRejectsAFilePath(t *testing.T) {
	h := newHarness(t)
	file := h.write("notes.txt", "not a directory")

	h.wantError(t, h.call(http.MethodPost, "/api/projects/register",
		`{"hostPath":`+jsonString(file)+`}`), http.StatusBadRequest, project.CodeNotADirectory)
}

func TestRegisterRejectsAMalformedBody(t *testing.T) {
	h := newHarness(t)

	tests := []struct {
		name string
		body string
	}{
		{"not JSON", `not json at all`},
		{"an unknown field", `{"hostPath":"x","initGti":true}`},
		{"a wrong type", `{"hostPath":42}`},
		{"two objects", `{"hostPath":"a"}{"hostPath":"b"}`},
		{"empty", ` `},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := h.call(http.MethodPost, "/api/projects/register", tt.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body was %s", recorder.Code, recorder.Body.String())
			}
			if code := decode[errorResponse](t, recorder).Error.Code; code != CodeInvalidRequest {
				t.Errorf("error code = %q, want %q", code, CodeInvalidRequest)
			}
		})
	}
}

// TestRegisterRejectsAnOversizedBody keeps a large request from being read into
// memory.
func TestRegisterRejectsAnOversizedBody(t *testing.T) {
	h := newHarness(t)

	big := `{"hostPath":"` + strings.Repeat("a", maxRequestBodyBytes+1024) + `"}`
	recorder := h.call(http.MethodPost, "/api/projects/register", big)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a body over the limit", recorder.Code)
	}
}

// ---------------------------------------------------------------------------
// POST /api/projects
// ---------------------------------------------------------------------------

func TestCreateProject(t *testing.T) {
	h := newHarness(t)
	collection := h.mkdir("2026 AgentMux")

	recorder := h.call(http.MethodPost, "/api/projects",
		`{"name":"NewApp","collectionPath":`+jsonString(collection)+`}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body was %s", recorder.Code, recorder.Body.String())
	}

	p := decode[projectResponse](t, recorder).Project
	want := filepath.Join(collection, "NewApp")
	if p.HostPath != want {
		t.Errorf("hostPath = %q, want %q", p.HostPath, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Errorf("the project directory was not created at %s: %v", want, err)
	}
	if p.CollectionPath != collection {
		t.Errorf("collectionPath = %q, want %q", p.CollectionPath, collection)
	}
}

func TestCreateProjectInTheRoot(t *testing.T) {
	h := newHarness(t)

	recorder := h.call(http.MethodPost, "/api/projects", `{"name":"Loose"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body was %s", recorder.Code, recorder.Body.String())
	}
	p := decode[projectResponse](t, recorder).Project
	if p.HostPath != filepath.Join(h.root, "Loose") {
		t.Errorf("hostPath = %q, want it directly under the first Projects Root", p.HostPath)
	}
	if p.CollectionPath != "" {
		t.Errorf("collectionPath = %q, want empty for a project directly under a root", p.CollectionPath)
	}
}

// TestCreateProjectRefusesToEscape is the security test for New Project.
func TestCreateProjectRefusesToEscape(t *testing.T) {
	h := newHarness(t)
	collection := h.mkdir("Collection")

	names := []string{
		"..", "../escape", `..\escape`, "../../etc/passwd",
		"a/../../escape", `a\..\..\escape`, "nested/child", "/absolute", `C:\absolute`,
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			recorder := h.call(http.MethodPost, "/api/projects",
				`{"name":`+jsonString(name)+`,"collectionPath":`+jsonString(collection)+`}`)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for the name %q; body was %s",
					recorder.Code, name, recorder.Body.String())
			}
			if code := decode[errorResponse](t, recorder).Error.Code; code != project.CodeInvalidName {
				t.Errorf("error code = %q, want %q", code, project.CodeInvalidName)
			}
		})
	}

	// Nothing may have appeared next to the collection.
	parent := filepath.Dir(collection)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("could not read %s: %v", parent, err)
	}
	for _, entry := range entries {
		if entry.Name() == "escape" || entry.Name() == "etc" || entry.Name() == "absolute" {
			t.Errorf("a rejected create left %q behind in %s", entry.Name(), parent)
		}
	}
}

func TestCreateProjectRefusesACollectionOutsideTheRoots(t *testing.T) {
	h := newHarness(t)
	outside := t.TempDir()

	h.wantError(t, h.call(http.MethodPost, "/api/projects",
		`{"name":"App","collectionPath":`+jsonString(outside)+`}`),
		http.StatusForbidden, project.CodeOutsideProjectsRoot)
}

func TestCreateProjectRefusesToOverwriteWork(t *testing.T) {
	h := newHarness(t)
	existing := h.mkdir("App")
	marker := filepath.Join(existing, "important.txt")
	if err := os.WriteFile(marker, []byte("do not lose me"), 0o644); err != nil {
		t.Fatalf("could not prepare the fixture: %v", err)
	}

	h.wantError(t, h.call(http.MethodPost, "/api/projects", `{"name":"App"}`),
		http.StatusConflict, project.CodeTargetExists)

	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "do not lose me" {
		t.Errorf("the existing directory was disturbed: %q, %v", content, err)
	}
}

func TestCreateProjectRejectsAnIllegalName(t *testing.T) {
	h := newHarness(t)

	for _, name := range []string{"", "  ", ".", "..", "CON", "bad:name", "bad*name"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS != "windows" && name == "CON" {
				t.Skip("reserved device names are a Windows rule")
			}
			h.wantError(t, h.call(http.MethodPost, "/api/projects", `{"name":`+jsonString(name)+`}`),
				http.StatusBadRequest, project.CodeInvalidName)
		})
	}
}

// ---------------------------------------------------------------------------
// GET /api/projects/discover
// ---------------------------------------------------------------------------

func TestDiscoverFindsTheNestedProject(t *testing.T) {
	h := newHarnessWith(t, func(root string) {
		mustWrite(t, filepath.Join(root, "2026 AgentMux", "README.md"), "# collection")
		mustWrite(t, filepath.Join(root, "2026 AgentMux", "AgentMux", "CLAUDE.md"), "# AgentMux")
		mustWrite(t, filepath.Join(root, "2026 AgentMux", "AgentMux", "go.mod"), "module app")
	})

	recorder := h.call(http.MethodGet, "/api/projects/discover", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	result := decode[project.DiscoveryResult](t, recorder)

	if len(result.Candidates) != 1 {
		t.Fatalf("candidates = %d, want only the nested project: %+v", len(result.Candidates), result.Candidates)
	}
	candidate := result.Candidates[0]
	if candidate.Name != "AgentMux" {
		t.Errorf("name = %q, want %q", candidate.Name, "AgentMux")
	}
	if candidate.Registered {
		t.Error("an unregistered candidate came back marked as registered")
	}
	if candidate.RuntimePath == "" {
		t.Error("runtimePath is empty; a discovered project must carry both paths")
	}
	if len(result.Roots) != 1 || !result.Roots[0].Exists {
		t.Errorf("roots = %+v, want the configured root to be reported as existing", result.Roots)
	}
}

// TestDiscoverDoesNotRegisterAnything is the safety property: discovery
// suggests, and only an explicit request creates a project.
func TestDiscoverDoesNotRegisterAnything(t *testing.T) {
	h := newHarnessWith(t, func(root string) {
		mustWrite(t, filepath.Join(root, "App", "go.mod"), "module app")
	})

	h.call(http.MethodGet, "/api/projects/discover", "")

	list := decode[projectListResponse](t, h.call(http.MethodGet, "/api/projects", ""))
	if list.Count != 0 {
		t.Errorf("a discovery scan registered %d projects, want none", list.Count)
	}
}

func TestDiscoverMarksRegisteredProjects(t *testing.T) {
	h := newHarness(t)
	target := h.mkdir(filepath.FromSlash("Collection/App"))
	if err := os.WriteFile(filepath.Join(target, "go.mod"), []byte("module app"), 0o644); err != nil {
		t.Fatalf("could not prepare the fixture: %v", err)
	}
	registered := h.register(filepath.FromSlash("Collection/App"))

	result := decode[project.DiscoveryResult](t, h.call(http.MethodGet, "/api/projects/discover", ""))
	if len(result.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(result.Candidates))
	}
	if !result.Candidates[0].Registered {
		t.Error("a project the user already has must be reported as registered, not hidden")
	}
	if result.Candidates[0].ProjectID != registered.ID {
		t.Errorf("projectId = %q, want %q", result.Candidates[0].ProjectID, registered.ID)
	}
}

// TestDiscoverClampsTheDepthParameter keeps a client from asking for a scan
// deep enough to walk an entire drive.
func TestDiscoverClampsTheDepthParameter(t *testing.T) {
	h := newHarnessWith(t, func(root string) {
		// Reachable at the deepest permitted level, 8.
		mustWrite(t, filepath.Join(root, "a", "b", "c", "d", "e", "f", "g", "Reachable", "go.mod"), "module a")
		// One level past it, so it is out of reach however large depth is.
		mustWrite(t, filepath.Join(root, "a", "b", "c", "d", "e", "f", "g", "h", "TooDeep", "go.mod"), "module b")
	})

	candidates := func(query string) []string {
		t.Helper()
		recorder := h.call(http.MethodGet, "/api/projects/discover"+query, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d for %q, want 200; body was %s", recorder.Code, query, recorder.Body.String())
		}
		result := decode[project.DiscoveryResult](t, recorder)
		names := make([]string, 0, len(result.Candidates))
		for _, c := range result.Candidates {
			names = append(names, c.Name)
		}
		return names
	}

	if got := candidates("?depth=1"); len(got) != 0 {
		t.Errorf("depth=1 offered %v, want nothing at that depth", got)
	}

	// An out-of-range value is clamped to the maximum rather than rejected...
	atMax := candidates("?depth=8")
	clamped := candidates("?depth=999")
	if strings.Join(clamped, ",") != strings.Join(atMax, ",") {
		t.Errorf("depth=999 offered %v and depth=8 offered %v; the parameter must be clamped", clamped, atMax)
	}
	// ...and the clamp is a real ceiling, not an unbounded scan.
	if strings.Join(clamped, ",") != "Reachable" {
		t.Errorf("depth=999 offered %v, want only the project within the maximum depth", clamped)
	}

	// A nonsensical value falls back to the configured depth rather than
	// failing the request.
	fallback := candidates("?depth=abc")
	if fallback == nil {
		t.Error("a non-numeric depth produced a null candidate list, want an empty one")
	}
}

// ---------------------------------------------------------------------------
// GET /api/projects/{id}
// ---------------------------------------------------------------------------

func TestGetProjectByID(t *testing.T) {
	h := newHarness(t)
	created := h.register("App")

	recorder := h.call(http.MethodGet, "/api/projects/"+created.ID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	got := decode[projectResponse](t, recorder).Project
	if got.ID != created.ID {
		t.Errorf("id = %q, want %q", got.ID, created.ID)
	}
}

func TestGetProjectRejectsAnUnknownID(t *testing.T) {
	h := newHarness(t)

	for _, id := range []string{"p_0123456789abcdef0123", "not-an-id", "p_short", "%20"} {
		t.Run(id, func(t *testing.T) {
			h.wantError(t, h.call(http.MethodGet, "/api/projects/"+id, ""),
				http.StatusNotFound, project.CodeNotFound)
		})
	}
}

// TestProjectIDNeverReachesTheFilesystem records how a traversal attempt in the
// identifier is handled. The router cleans the path before matching, so the
// request is redirected rather than reaching the handler; either way it can
// never resolve to a project.
func TestProjectIDNeverReachesTheFilesystem(t *testing.T) {
	h := newHarness(t)

	recorder := h.call(http.MethodGet, "/api/projects/../../etc/passwd", "")
	switch {
	case recorder.Code >= 300 && recorder.Code < 400:
		// The router cleaned the path and redirected. Nothing was resolved.
	case recorder.Code == http.StatusNotFound:
		// The cleaned path matched no route.
	default:
		t.Errorf("status = %d for a traversal identifier, want a redirect or 404", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), `"project"`) {
		t.Errorf("a traversal identifier returned a project: %s", recorder.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Routing, recovery, CORS, and static serving
// ---------------------------------------------------------------------------

// TestUnknownAPIPathReturnsJSON is what keeps a mistyped endpoint from being
// answered with an HTML page and a confusing 200.
func TestUnknownAPIPathReturnsJSON(t *testing.T) {
	h := newHarness(t)

	for _, target := range []string{"/api/", "/api/nope", "/api/projects/x/y/z"} {
		t.Run(target, func(t *testing.T) {
			recorder := h.call(http.MethodGet, target, "")
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body was %s", recorder.Code, recorder.Body.String())
			}
			if ct := recorder.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want JSON, not HTML", ct)
			}
			if code := decode[errorResponse](t, recorder).Error.Code; code != CodeNotFound {
				t.Errorf("error code = %q, want %q", code, CodeNotFound)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newHarness(t)

	for _, tt := range []struct{ method, target string }{
		{http.MethodPost, "/api/server"},
		{http.MethodDelete, "/api/projects"},
		{http.MethodPut, "/api/projects/register"},
	} {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			recorder := h.call(tt.method, tt.target, "")
			if recorder.Code != http.StatusMethodNotAllowed && recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 405 or 404", recorder.Code)
			}
		})
	}
}

func TestCORSPermitsLoopbackOrigins(t *testing.T) {
	h := newHarness(t)

	for _, origin := range []string{
		"http://localhost:5173",
		"http://127.0.0.1:5173",
		"http://[::1]:5173",
	} {
		t.Run(origin, func(t *testing.T) {
			recorder := h.call(http.MethodGet, "/api/server", "", "Origin", origin)
			if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != origin {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, origin)
			}
		})
	}
}

// TestCORSRejectsRemoteOrigins is the security half: a page on the public
// internet must not be able to reach a local AgentMux from a browser.
func TestCORSRejectsRemoteOrigins(t *testing.T) {
	h := newHarness(t)

	for _, origin := range []string{
		"https://evil.example.com",
		"http://192.168.1.10:8080",
		"null",
		"file://",
	} {
		t.Run(origin, func(t *testing.T) {
			recorder := h.call(http.MethodGet, "/api/server", "", "Origin", origin)
			if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("Access-Control-Allow-Origin = %q for a remote origin, want it absent", got)
			}
		})
	}
}

func TestPreflightIsAnswered(t *testing.T) {
	h := newHarness(t)

	recorder := h.call(http.MethodOptions, "/api/projects", "",
		"Origin", "http://localhost:5173",
		"Access-Control-Request-Method", "POST")
	if recorder.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("Access-Control-Allow-Methods = %q, want it to include POST", got)
	}
}

// TestStaticHandlerWithoutABuildExplainsItself records that a missing frontend
// is reported as a state, not as a bare 404 that looks like a bug.
func TestStaticHandlerWithoutABuildExplainsItself(t *testing.T) {
	h := newHarness(t)

	for _, target := range []string{"/", "/projects"} {
		t.Run(target, func(t *testing.T) {
			recorder := h.call(http.MethodGet, target, "")
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", recorder.Code)
			}
			if !strings.Contains(recorder.Body.String(), "/api/server") {
				t.Errorf("body = %q, want it to point at the API", recorder.Body.String())
			}
		})
	}
}

func TestStaticHandlerServesTheBuildAndFallsBackToIndex(t *testing.T) {
	webDir := t.TempDir()
	mustWrite(t, filepath.Join(webDir, "index.html"), "<!doctype html><title>AgentMux</title>")
	mustWrite(t, filepath.Join(webDir, "assets", "app.js"), "console.log('app')")

	h := newHarness(t)
	server, err := New(Options{
		Config:     h.server.cfg,
		Host:       h.server.host,
		Projects:   h.server.projects,
		Discoverer: h.server.discoverer,
		Runtime:    h.runtime,
		Logger:     discardLogger(),
		WebDir:     webDir,
	})
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	call := func(target string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		return recorder
	}

	// A real file is served as itself.
	if body := call("/assets/app.js").Body.String(); !strings.Contains(body, "console.log") {
		t.Errorf("a static asset was not served: %q", body)
	}
	// An unknown route falls back to the entry point, which is what makes
	// client-side routing work.
	index := call("/projects/p_0123456789abcdef0123")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "<!doctype html>") {
		t.Errorf("the SPA fallback returned %d: %q", index.Code, index.Body.String())
	}
	if cache := index.Header().Get("Cache-Control"); cache != "no-cache" {
		t.Errorf("Cache-Control = %q for index.html, want no-cache", cache)
	}
	// A hidden segment is refused rather than served.
	if hidden := call("/.env"); hidden.Code != http.StatusNotFound {
		t.Errorf("status = %d for a hidden path, want 404", hidden.Code)
	}
	// The API still wins over the fallback.
	if api := call("/api/server"); !strings.Contains(api.Header().Get("Content-Type"), "application/json") {
		t.Errorf("the API path was answered by the static handler: %q", api.Body.String())
	}
}

// TestPanicBecomesA500 proves a handler panic cannot drop a connection.
func TestPanicBecomesA500(t *testing.T) {
	h := newHarness(t)
	// A handler that panics, wrapped in the server's middleware.
	handler := h.server.withRecovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/server", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if code := decode[errorResponse](t, recorder).Error.Code; code != CodeInternal {
		t.Errorf("error code = %q, want %q", code, CodeInternal)
	}
	if strings.Contains(recorder.Body.String(), "boom") {
		t.Error("the panic message leaked into the response body")
	}
}

func TestNewRequiresItsCollaborators(t *testing.T) {
	h := newHarness(t)
	base := Options{
		Config:     h.server.cfg,
		Host:       h.server.host,
		Projects:   h.server.projects,
		Discoverer: h.server.discoverer,
		Runtime:    h.runtime,
	}

	tests := []struct {
		name string
		mut  func(*Options)
	}{
		{"no config", func(o *Options) { o.Config = nil }},
		{"no host adapter", func(o *Options) { o.Host = nil }},
		{"no project service", func(o *Options) { o.Projects = nil }},
		{"no discoverer", func(o *Options) { o.Discoverer = nil }},
		// A server without a runtime manager cannot answer a single runtime
		// request, so it must be refused at construction rather than turning
		// into a panic inside a handler.
		{"no runtime manager", func(o *Options) { o.Runtime = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := base
			tt.mut(&options)
			if server, err := New(options); err == nil {
				t.Errorf("New succeeded with %s, want a rejection (server %p)", tt.name, server)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// jsonString encodes a Go string as a JSON string literal, so a Windows path
// with backslashes survives the round trip.
func jsonString(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// escapedJSON is the body of a JSON string literal: the form a value takes
// inside a JSON document.
func escapedJSON(value string) string {
	encoded := jsonString(value)
	return encoded[1 : len(encoded)-1]
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("could not create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("could not create %s: %v", path, err)
	}
}
