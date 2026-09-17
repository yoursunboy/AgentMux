package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/storage"
)

// This file holds the runtime half of the API tests, plus the two test doubles
// they need.
//
// The doubles exist because two facts have to be chosen rather than detected:
// whether this server's environment can host a terminal runtime, and what the
// session backend answers. Everything else - routing, decoding, validation,
// SQLite, and the manager's own decisions - is real, so a handler that is
// wired to the wrong collaborator still fails here.

// pinnedHost is a real host adapter with two facts chosen by the test.
//
// The embedded adapter answers every path and platform question for real, so
// project registration and discovery behave exactly as they do in production.
// Only SystemInfo and the dependency probe are overridden, because those are
// the two answers that would otherwise differ between the machine running the
// tests and the machine a user runs AgentMux on.
type pinnedHost struct {
	host.Adapter
	runtimeAvailable bool

	// tmuxAvailable pins the result of the tmux probe. Pinning it is the whole
	// point: a test that consulted the real PATH would assert something
	// different on every machine.
	tmuxAvailable bool
}

// testRuntimeBlocker is the reason a pinned-unavailable host gives. It is the
// message a Windows-native server produces, and the tests assert it reaches
// the client unchanged: a user who is told "tmux not found" when tmux is
// installed one command away has been misled.
const testRuntimeBlocker = "tmux runtime requires AgentMux Server to run inside WSL."

func (h pinnedHost) Info(ctx context.Context) host.SystemInfo {
	info := h.Adapter.Info(ctx)
	info.RuntimeAvailable = h.runtimeAvailable
	if h.runtimeAvailable {
		info.RuntimeUnavailableReason = ""
	} else {
		info.RuntimeUnavailableReason = testRuntimeBlocker
	}
	return info
}

func (h pinnedHost) RuntimeSupport() (bool, string) {
	if h.runtimeAvailable {
		return true, ""
	}
	return false, testRuntimeBlocker
}

func (h pinnedHost) CheckDependencies(ctx context.Context) []host.Dependency {
	deps := h.Adapter.CheckDependencies(ctx)
	for i, dep := range deps {
		if dep.Name != "tmux" {
			continue
		}
		deps[i].Available = h.tmuxAvailable
		if !h.tmuxAvailable {
			deps[i].Path = ""
		}
		return deps
	}
	// The real probe did not report tmux at all, which would make the override
	// a no-op and the test silently about something else. Say so.
	return append(deps, host.Dependency{
		Name:      "tmux",
		Available: h.tmuxAvailable,
		Required:  true,
		ProbedIn:  string(h.Environment()),
		Note:      "pinned by the test",
	})
}

// ---------------------------------------------------------------------------
// fakeBackend, and the per-project plumbing the runtime manager needs around it
// ---------------------------------------------------------------------------

// fakeFactory hands every project the same fake backend.
//
// The manager takes a factory rather than one backend because since Phase 2.5
// one project's runtime must not share a server with another's. The API layer
// has nothing to say about that - it is tested in the session package against
// real tmux - so the double answers every project with the one backend the
// assertions below are written against.
type fakeFactory struct{ backend *fakeBackend }

func (f *fakeFactory) Backend(string) (session.Backend, error) { return f.backend, nil }

func (f *fakeFactory) BackendName() string { return f.backend.Name() }

// statusFactory is a factory that can also describe the tmux installation its
// backends will run on.
//
// It is a separate type rather than a status field on fakeFactory because the
// ability to answer is what is under test: fakeFactory cannot, and the server
// has to leave the diagnostic out rather than invent one. One type with a nil
// status would put both cases through one code path and prove neither.
type statusFactory struct {
	*fakeFactory
	status session.TmuxStatus
}

func (f *statusFactory) Status(context.Context) session.TmuxStatus { return f.status }

// fakeSockets is the socket layout that goes with fakeBackend.
//
// It mirrors the real layout's contract - a path per project id, a probe that
// reports what is on a socket - without a filesystem. The distinction it does
// keep is the one the manager depends on: a path belongs to exactly one project
// id, so a probe of one project's socket reports that project's session and
// nothing else. A double that returned every session for every path would let a
// per-project bug pass here.
type fakeSockets struct {
	backend *fakeBackend

	// dir is the directory the paths are built under.
	dir string

	// absent, when set, makes every probe report that there is no socket there.
	// It stands in for a stopped runtime whose server is gone.
	absent bool

	// reclaimed records the socket paths Reclaim was asked about.
	reclaimed []string
}

func newFakeSockets(b *fakeBackend) *fakeSockets {
	return &fakeSockets{backend: b, dir: "/fake/tmux"}
}

func (s *fakeSockets) Dir() string { return s.dir }

func (s *fakeSockets) Path(projectID string) string {
	if !project.ValidID(projectID) {
		return ""
	}
	return filepath.Join(s.dir, projectID+".sock")
}

func (s *fakeSockets) List() ([]session.SocketFile, error) {
	return nil, nil
}

func (s *fakeSockets) Probe(_ context.Context, path string) session.SocketProbe {
	if s.absent {
		return session.SocketProbe{State: session.SocketAbsent}
	}
	id := strings.TrimSuffix(filepath.Base(path), ".sock")
	want := project.SessionNameFor(id)
	sessions, _ := s.backend.List(context.Background())
	out := make([]*session.Session, 0, 1)
	for _, sess := range sessions {
		if sess.Name == want {
			out = append(out, sess)
		}
	}
	return session.SocketProbe{State: session.SocketLive, Sessions: out}
}

func (s *fakeSockets) Reclaim(_ context.Context, path string) (bool, error) {
	s.reclaimed = append(s.reclaimed, path)
	return false, nil
}

// fakeBackend is an in-memory session.Backend.
//
// The session package already tests a real backend against real tmux, and that
// suite needs a POSIX host. What the HTTP layer needs is a backend whose
// answers the test chooses, so that a status code, a response shape, and a
// sequence number can be asserted anywhere - and so that a failure here
// localises to the API rather than to tmux.
//
// It records what it was asked to do, because the interesting assertions are
// about bytes arriving unchanged: an escape sequence, a control character, or
// text that is not ASCII must reach the backend exactly as the client sent it.
type fakeBackend struct {
	mu sync.Mutex

	// availableErr, when set, is what Available reports. It stands in for a
	// machine where the backend exists but cannot be used.
	availableErr error

	sessions  map[string]*session.Session
	created   []session.SessionSpec
	input     map[string][]byte
	resized   map[string][2]int
	launched  map[string][]string
	stopCalls int

	// serverKills counts KillServer calls. A project's Destroy may stop its own
	// server once nothing is left on it, and a test that asserted "destroying a
	// project stops one server" without this could not tell that from
	// "destroying a project stops every server".
	serverKills int

	destroyed []string
	subs      map[string][]*fakeSubscription
	closed    bool

	// pane is what the backend reports about a session's terminal, and
	// paneErr what it reports when it cannot say. Both exist for the agent
	// endpoints: they are the only callers that ask, and the answer they get
	// decides whether a launch is refused before it is attempted.
	pane    session.PaneProcess
	paneErr error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		sessions: make(map[string]*session.Session),
		input:    make(map[string][]byte),
		resized:  make(map[string][2]int),
		launched: make(map[string][]string),
		subs:     make(map[string][]*fakeSubscription),
	}
}

func (b *fakeBackend) Name() string { return "fake" }
func (b *fakeBackend) Available(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.availableErr
}

func (b *fakeBackend) Create(_ context.Context, spec session.SessionSpec) (*session.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.sessions[spec.Name]; exists {
		return nil, session.ErrSessionExists
	}
	created := &session.Session{
		Name:    spec.Name,
		Dir:     spec.Dir,
		Cols:    spec.Cols,
		Rows:    spec.Rows,
		Created: time.Date(2026, time.September, 17, 11, 0, 0, 0, time.UTC),
	}
	b.sessions[spec.Name] = created
	b.created = append(b.created, spec)
	return created, nil
}

func (b *fakeBackend) Exists(_ context.Context, name string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.sessions[name]
	return ok, nil
}

func (b *fakeBackend) Inspect(_ context.Context, name string) (*session.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	found, ok := b.sessions[name]
	if !ok {
		return nil, session.ErrNoSuchSession
	}
	return found, nil
}

func (b *fakeBackend) List(context.Context) ([]*session.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*session.Session, 0, len(b.sessions))
	for _, s := range b.sessions {
		out = append(out, s)
	}
	return out, nil
}

func (b *fakeBackend) Launch(_ context.Context, name, command string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.sessions[name]; !ok {
		return session.ErrNoSuchSession
	}
	b.launched[name] = append(b.launched[name], command)
	return nil
}

func (b *fakeBackend) SendInput(_ context.Context, name string, data []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.sessions[name]; !ok {
		return session.ErrNoSuchSession
	}
	b.input[name] = append(b.input[name], data...)
	return nil
}

func (b *fakeBackend) Resize(_ context.Context, name string, cols, rows int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	found, ok := b.sessions[name]
	if !ok {
		return session.ErrNoSuchSession
	}
	found.Cols, found.Rows = cols, rows
	b.resized[name] = [2]int{cols, rows}
	return nil
}

func (b *fakeBackend) Stop(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.sessions[name]; !ok {
		return session.ErrNoSuchSession
	}
	b.stopCalls++
	return nil
}

func (b *fakeBackend) Destroy(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, name)
	b.destroyed = append(b.destroyed, name)
	return nil
}

// KillServer ends every session, which is what stopping a tmux server does.
//
// It is recorded rather than only performed, because the assertion that matters
// about it is a negative one: destroying one project must not stop a server
// another project is still using.
func (b *fakeBackend) KillServer(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.serverKills++
	for name := range b.sessions {
		delete(b.sessions, name)
	}
	return nil
}

func (b *fakeBackend) Snapshot(_ context.Context, name string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.sessions[name]; !ok {
		return nil, session.ErrNoSuchSession
	}
	return []byte("\x1b[32mfake pane\x1b[0m"), nil
}

func (b *fakeBackend) Attach(_ context.Context, name string) (session.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.sessions[name]; !ok {
		return nil, session.ErrNoSuchSession
	}
	sub := &fakeSubscription{name: name, out: make(chan []byte, 64), connected: true}
	b.subs[name] = append(b.subs[name], sub)
	return sub, nil
}

func (b *fakeBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

// PaneProcess implements session.ProcessInspector.
//
// It answers with what the test chose rather than with a process table, because
// what the agent endpoints do with the answer is the thing under test here. The
// real question - does the runtime recognise a running Claude, and does it
// report a cwd it read from the kernel - is answered in the session package
// against real tmux and in cmd/server against the real CLI.
func (b *fakeBackend) PaneProcess(_ context.Context, name string) (session.PaneProcess, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.sessions[name]; !ok {
		return session.PaneProcess{}, session.ErrNoSuchSession
	}
	if b.paneErr != nil {
		return session.PaneProcess{}, b.paneErr
	}
	return b.pane, nil
}

// setPane makes the backend report this as the terminal's foreground process.
func (b *fakeBackend) setPane(pane session.PaneProcess) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pane = pane
}

// failPane makes the backend report that it cannot describe the terminal, which
// is the "I cannot see" case the runtime must not confuse with "nothing is
// there".
func (b *fakeBackend) failPane(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.paneErr = err
}

// deliver pushes output to every live stream of a session, as the runtime
// would when a program inside the terminal writes something.
func (b *fakeBackend) deliver(t *testing.T, name string, data []byte) {
	t.Helper()
	b.mu.Lock()
	subs := append([]*fakeSubscription(nil), b.subs[name]...)
	b.mu.Unlock()
	if len(subs) == 0 {
		t.Fatalf("no output stream is open for session %q; nothing would receive %q", name, data)
	}
	for _, sub := range subs {
		sub.deliver(data)
	}
}

// failAvailable makes the backend report itself unusable, as a machine with no
// tmux on it does.
func (b *fakeBackend) failAvailable(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.availableErr = err
}

// receivedInput returns every byte the backend was given for a session.
func (b *fakeBackend) receivedInput(name string) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.input[name]...)
}

// sizeOf returns the last size the backend was told to apply.
func (b *fakeBackend) sizeOf(name string) (int, int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	size, ok := b.resized[name]
	return size[0], size[1], ok
}

// fakeSubscription is a live output stream a test can write to.
type fakeSubscription struct {
	name string
	out  chan []byte

	mu        sync.Mutex
	connected bool
	err       error
	closed    bool
}

func (s *fakeSubscription) Name() string          { return s.name }
func (s *fakeSubscription) Output() <-chan []byte { return s.out }

func (s *fakeSubscription) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected
}

func (s *fakeSubscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *fakeSubscription) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.connected = false
	close(s.out)
	return nil
}

func (s *fakeSubscription) deliver(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.out <- append([]byte(nil), data...)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// registerProject registers a project under the harness's Projects Root and
// returns its id and host path.
func (h *harness) registerProject(t *testing.T, name string) (string, string) {
	t.Helper()
	dir := h.mkdir(name)
	recorder := h.call(http.MethodPost, "/api/projects/register",
		fmt.Sprintf(`{"hostPath":%q,"name":%q}`, dir, name))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("registering %s: status = %d, body was %s", name, recorder.Code, recorder.Body.String())
	}
	body := decode[projectResponse](t, recorder)
	return body.Project.ID, dir
}

// startRuntime starts a project's runtime and returns the response.
func (h *harness) startRuntime(t *testing.T, projectID string) *session.Runtime {
	t.Helper()
	recorder := h.call(http.MethodPost, "/api/projects/"+projectID+"/runtime/start", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("starting the runtime: status = %d, body was %s", recorder.Code, recorder.Body.String())
	}
	return decode[runtimeResponse](t, recorder).Runtime
}

// waitForSequence waits until the manager has numbered at least want chunks.
//
// Numbering happens on the manager's own goroutine, so a test that delivered
// output and immediately read it back would be racing the pump.
func (h *harness) waitForSequence(t *testing.T, projectID string, want uint64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rt, err := h.runtime.Runtime(context.Background(), projectID)
		if err == nil && rt.Sequence >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the runtime never reached sequence %d", want)
}

// decodeChunks reads a debug output body, concatenating the chunk payloads so
// a test compares terminal bytes rather than their base64 encoding.
func decodeChunks(t *testing.T, recorder *httptest.ResponseRecorder) []byte {
	t.Helper()
	body := decode[debugOutputResponse](t, recorder)
	var out bytes.Buffer
	for _, chunk := range body.Chunks {
		out.Write(chunk.Data)
	}
	return out.Bytes()
}

// projectRuntimePath returns the path a project's session must run in.
func (h *harness) projectRuntimePath(t *testing.T, projectID string) string {
	t.Helper()
	p, err := h.server.projects.Get(context.Background(), projectID)
	if err != nil {
		t.Fatalf("reading back %s failed: %v", projectID, err)
	}
	return p.RuntimePath
}

// ---------------------------------------------------------------------------
// GET /api/server: runtime availability
// ---------------------------------------------------------------------------

// TestServerInfoReportsTheRuntimeAsUnavailable is the honesty check on the
// endpoint that always answers. A server that cannot host a terminal must say
// so, and must say what to do about it, rather than reporting tmux missing
// when the real problem is which side of the WSL boundary it was started on.
func TestServerInfoReportsTheRuntimeAsUnavailable(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: false})
	recorder := h.call(http.MethodGet, "/api/server", "")
	info := decode[serverInfoResponse](t, recorder)

	if !info.TerminalRuntimeImplemented {
		t.Error("terminalRuntimeImplemented is false, but this build contains the runtime")
	}
	if info.RuntimeAvailable {
		t.Error("runtimeAvailable is true, but the host was pinned to the Windows-native case")
	}
	if info.Features["terminal"] {
		t.Error("features.terminal is true, but this server cannot host a terminal")
	}
	if info.RuntimeUnavailableReason != testRuntimeBlocker {
		t.Errorf("runtimeUnavailableReason = %q, want the host adapter's explanation", info.RuntimeUnavailableReason)
	}

	// The reason has to reach the warnings list, because that is what the UI
	// renders as a banner. A client that had to reconstruct it would be a
	// second place the message is written.
	found := false
	for _, warning := range info.Warnings {
		if warning == testRuntimeBlocker {
			found = true
		}
	}
	if !found {
		t.Errorf("the warnings do not explain the unavailable runtime; got %v", info.Warnings)
	}
}

// TestServerInfoOffersTheTerminalWhenEverythingHolds is the other side: with a
// host that can run a runtime and a build that has one, the feature is on.
func TestServerInfoOffersTheTerminalWhenEverythingHolds(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	info := decode[serverInfoResponse](t, h.call(http.MethodGet, "/api/server", ""))

	if !info.RuntimeAvailable {
		t.Fatal("runtimeAvailable is false, but the host was pinned to a usable runtime")
	}
	if info.RuntimeUnavailableReason != "" {
		t.Errorf("runtimeUnavailableReason = %q, want it empty when the runtime is available",
			info.RuntimeUnavailableReason)
	}
	if !info.Features["terminal"] {
		t.Error("features.terminal is false, but the build and the host both support a runtime")
	}
	for _, warning := range info.Warnings {
		if strings.Contains(warning, "tmux") {
			t.Errorf("a server with a usable runtime warns about tmux: %q", warning)
		}
	}
}

// TestServerInfoNeverReturnsAnInstallID pins the Phase 1 leftover. The value is
// still stored, but it is not something an unauthenticated endpoint hands out:
// an identifier stable across every request from a machine is a tracking token
// whether or not it is called a credential.
func TestServerInfoNeverReturnsAnInstallID(t *testing.T) {
	h := newHarness(t)
	recorder := h.call(http.MethodGet, "/api/server", "")

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("the response is not a JSON object: %v", err)
	}
	for key := range raw {
		lowered := strings.ToLower(key)
		if strings.Contains(lowered, "install") {
			t.Errorf("GET /api/server returns the field %q; it must not", key)
		}
	}

	// The value is still created and kept, so the check above is about the
	// response rather than about the feature having been removed.
	if _, err := h.store.Settings().Get(context.Background(), storage.SettingInstallID); err != nil {
		t.Errorf("the installation id is no longer stored: %v", err)
	}
}

// TestServerInfoNamesTheEnvironmentMissingTmux is §二十一: when the host can
// run a runtime but tmux is not installed in it, the server says which
// environment the probe looked in and how to fix it, and does not offer to
// install anything itself.
//
// Naming the environment is the whole point. On a Windows host the probe looks
// inside WSL, so "tmux is not installed" on its own sends a user to install
// tmux on a machine that is not the one that needs it.
func TestServerInfoNamesTheEnvironmentMissingTmux(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, tmuxMissing: true})
	info := decode[serverInfoResponse](t, h.call(http.MethodGet, "/api/server", ""))

	if !info.RuntimeAvailable {
		t.Error("runtimeAvailable is false, but the host can host a runtime")
	}
	if info.Features["terminal"] {
		t.Error("features.terminal is true while tmux is missing")
	}

	var blocker string
	for _, warning := range info.Warnings {
		if strings.Contains(warning, "tmux") {
			blocker = warning
		}
	}
	if blocker == "" {
		t.Fatalf("no warning explains the missing tmux; warnings were %v", info.Warnings)
	}
	if !strings.Contains(blocker, "not installed in") {
		t.Errorf("the warning does not name the environment: %q", blocker)
	}
	// The environment named must be the one the probe actually looked in, and
	// that is not necessarily the one the server runs in: on Windows the server
	// is on the host while tmux lives inside WSL, and naming the wrong one
	// sends a user to install tmux on a machine that does not need it.
	var probedIn string
	for _, dep := range info.Dependencies {
		if dep.Name == "tmux" {
			probedIn = dep.ProbedIn
		}
	}
	if probedIn == "" {
		t.Fatal("the response does not say which environment tmux was probed for")
	}
	if !strings.Contains(blocker, probedIn) {
		t.Errorf("the warning names %q, want the environment the probe looked in (%q)", blocker, probedIn)
	}
	if info.Environment == "" {
		t.Error("the response does not say where the server itself is running")
	}
	if !strings.Contains(blocker, "sudo apt install tmux") {
		t.Errorf("the warning gives no way forward: %q", blocker)
	}

	// The runtime endpoints refuse for the same reason rather than reporting a
	// start failure. The refusal comes from the backend, which is the layer
	// that knows tmux is missing, and the HTTP layer maps it to 503 rather than
	// 500 because the service is genuinely unavailable rather than broken.
	h.backend.failAvailable(&session.Error{
		Code: session.CodeBackendUnavailable,
		Message: "tmux is not installed in the environment where the AgentMux server runs, " +
			"so no terminal session can be started",
	})
	id, _ := h.registerProject(t, "app")
	body := h.wantError(t, h.call(http.MethodPost, "/api/projects/"+id+"/runtime/start", ""),
		http.StatusServiceUnavailable, session.CodeBackendUnavailable)
	if strings.TrimSpace(body.Error.Message) == "" {
		t.Error("the refusal gives the user nothing to act on")
	}
}

// ---------------------------------------------------------------------------
// The runtime endpoints
// ---------------------------------------------------------------------------

// TestRuntimeEndpointsRefuseOnAHostThatCannotRunOne is §二十三 at the API
// boundary: a Windows-native server does not silently proxy to tmux inside
// WSL, it refuses and explains. The refusal is 503 rather than 500 because the
// service is genuinely unavailable rather than broken, and the message is the
// host adapter's because that is the part of AgentMux that knows the
// difference.
func TestRuntimeEndpointsRefuseOnAHostThatCannotRunOne(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: false})
	id, _ := h.registerProject(t, "app")

	for _, tc := range []struct {
		method string
		target string
	}{
		{http.MethodGet, "/api/projects/" + id + "/runtime"},
		{http.MethodPost, "/api/projects/" + id + "/runtime/start"},
		{http.MethodPost, "/api/projects/" + id + "/runtime/stop"},
		{http.MethodDelete, "/api/projects/" + id + "/runtime"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			recorder := h.call(tc.method, tc.target, "")
			body := h.wantError(t, recorder, http.StatusServiceUnavailable, session.CodeUnavailable)
			if body.Error.Message != testRuntimeBlocker {
				t.Errorf("message = %q, want the host adapter's explanation %q",
					body.Error.Message, testRuntimeBlocker)
			}
		})
	}
}

// TestRuntimeLifecycle walks the four endpoints in order and asserts what each
// one leaves behind, because the difference between them is the whole point of
// having four.
func TestRuntimeLifecycle(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	id, dir := h.registerProject(t, "app")

	// A project that has never had a runtime is stopped, not missing. One shape
	// for the client to render beats two.
	before := decode[runtimeResponse](t,
		h.call(http.MethodGet, "/api/projects/"+id+"/runtime", "")).Runtime
	if before.State != session.StateStopped || before.SessionAlive {
		t.Errorf("a fresh runtime reports state=%s alive=%v, want STOPPED and not alive",
			before.State, before.SessionAlive)
	}

	started := h.startRuntime(t, id)
	if started.State != session.StateRunning || !started.SessionAlive {
		t.Errorf("after start: state=%s alive=%v, want RUNNING and alive", started.State, started.SessionAlive)
	}
	if started.Session != project.SessionNameFor(id) {
		t.Errorf("session = %q, want %q: the session is named after the project id",
			started.Session, project.SessionNameFor(id))
	}
	if started.Cols != session.DefaultCols || started.Rows != session.DefaultRows {
		t.Errorf("size = %dx%d, want the canonical default %dx%d",
			started.Cols, started.Rows, session.DefaultCols, session.DefaultRows)
	}

	// The session's working directory is the project's runtime path. A session
	// started at a Projects Root or a collection would put the agent in the
	// wrong repository while looking perfectly healthy.
	h.backend.mu.Lock()
	specs := append([]session.SessionSpec(nil), h.backend.created...)
	h.backend.mu.Unlock()
	if len(specs) != 1 {
		t.Fatalf("the backend was asked to create %d sessions, want 1", len(specs))
	}
	if specs[0].Dir == dir {
		// On a native host the runtime path and the host path are the same
		// string, so this is the expected case - but the assertion below is
		// still the one that matters.
		t.Logf("native host: runtime path equals the host path (%s)", dir)
	}
	if specs[0].Dir != h.projectRuntimePath(t, id) {
		t.Errorf("the session was created in %q, want the project's runtime path %q",
			specs[0].Dir, h.projectRuntimePath(t, id))
	}

	// Stop ends the work and keeps the terminal.
	stopped := decode[runtimeResponse](t,
		h.call(http.MethodPost, "/api/projects/"+id+"/runtime/stop", "")).Runtime
	if stopped.State != session.StateStopped {
		t.Errorf("after stop: state = %s, want STOPPED", stopped.State)
	}
	if !stopped.SessionAlive {
		t.Error("after stop: the session is gone, but stop must keep the terminal so its scrollback survives")
	}
	if h.backend.stopCalls != 1 {
		t.Errorf("the backend saw %d stop calls, want 1", h.backend.stopCalls)
	}

	// Destroy ends the session.
	destroyed := decode[runtimeResponse](t,
		h.call(http.MethodDelete, "/api/projects/"+id+"/runtime", "")).Runtime
	if destroyed.State != session.StateStopped {
		t.Errorf("after destroy: state = %s, want STOPPED", destroyed.State)
	}
	if destroyed.SessionAlive {
		t.Error("after destroy: the session is still alive, but destroy removes it")
	}
	if len(h.backend.destroyed) != 1 || h.backend.destroyed[0] != project.SessionNameFor(id) {
		t.Errorf("the backend destroyed %v, want [%s]", h.backend.destroyed, project.SessionNameFor(id))
	}
}

// TestStartRuntimeIsIdempotent keeps a double click from producing two
// terminals writing to the same repository.
func TestStartRuntimeIsIdempotent(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	id, _ := h.registerProject(t, "app")

	h.startRuntime(t, id)
	h.startRuntime(t, id)

	h.backend.mu.Lock()
	created := len(h.backend.created)
	h.backend.mu.Unlock()
	if created != 1 {
		t.Errorf("the backend was asked to create %d sessions, want 1", created)
	}
}

// TestRuntimeEndpointsReportAMissingProject keeps the two failure modes apart:
// a project that does not exist is not a runtime that is stopped.
func TestRuntimeEndpointsReportAMissingProject(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})

	for _, tc := range []struct {
		method string
		target string
	}{
		{http.MethodGet, "/api/projects/p_missing/runtime"},
		{http.MethodPost, "/api/projects/p_missing/runtime/start"},
		{http.MethodPost, "/api/projects/p_missing/runtime/stop"},
		{http.MethodDelete, "/api/projects/p_missing/runtime"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			recorder := h.call(tc.method, tc.target, "")
			h.wantError(t, recorder, http.StatusNotFound, project.CodeNotFound)
		})
	}
}

// TestRuntimeResizeRejectsAnImpossibleSize checks the boundary between a bad
// request and a conflict with the current state. A size of zero is the
// client's mistake; resizing a stopped runtime is not.
func TestRuntimeResizeRejectsAnImpossibleSize(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, debugAPI: true})
	id, _ := h.registerProject(t, "app")

	recorder := h.call(http.MethodPost, "/api/debug/projects/"+id+"/runtime/resize", `{"cols":0,"rows":0}`)
	h.wantError(t, recorder, http.StatusBadRequest, session.CodeInvalidSize)

	// Nothing is running, so a well-formed size is a conflict rather than a
	// bad request.
	recorder = h.call(http.MethodPost, "/api/debug/projects/"+id+"/runtime/resize", `{"cols":100,"rows":30}`)
	h.wantError(t, recorder, http.StatusConflict, session.CodeNotRunning)
}

// TestRuntimeResizeIsKeptAndApplied checks that the resize reaches the backend
// and comes back as the runtime's canonical size.
func TestRuntimeResizeIsKeptAndApplied(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, debugAPI: true})
	id, _ := h.registerProject(t, "app")
	h.startRuntime(t, id)

	resized := decode[runtimeResponse](t, h.call(http.MethodPost,
		"/api/debug/projects/"+id+"/runtime/resize", `{"cols":100,"rows":30}`)).Runtime
	if resized.Cols != 100 || resized.Rows != 30 {
		t.Errorf("size = %dx%d, want 100x30", resized.Cols, resized.Rows)
	}

	cols, rows, ok := h.backend.sizeOf(project.SessionNameFor(id))
	if !ok || cols != 100 || rows != 30 {
		t.Errorf("the backend was asked for %dx%d (set=%v), want 100x30", cols, rows, ok)
	}
}

// ---------------------------------------------------------------------------
// Input
// ---------------------------------------------------------------------------

// TestRuntimeInputCarriesEveryByteKind is the input half of the byte contract.
//
// The cases are the ones a terminal actually receives and that a naive
// implementation loses: multi-byte UTF-8, a control character, an escape
// sequence, and a carriage return. The runtime has no concept of a prompt, so
// none of these may be filtered, translated, or refused.
func TestRuntimeInputCarriesEveryByteKind(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []byte
	}{
		{
			name: "ascii",
			body: `{"text":"ls -la","enter":true}`,
			want: []byte("ls -la\r"),
		},
		{
			name: "chinese",
			body: `{"text":"echo 中文测试","enter":true}`,
			want: []byte("echo 中文测试\r"),
		},
		{
			name: "control character",
			// Ctrl-C: the byte that interrupts whatever is running.
			body: `{"bytes":"` + base64.StdEncoding.EncodeToString([]byte{0x03}) + `"}`,
			want: []byte{0x03},
		},
		{
			name: "arrow key",
			// Up, as a terminal actually sends it.
			body: `{"bytes":"` + base64.StdEncoding.EncodeToString([]byte("\x1b[A")) + `"}`,
			want: []byte("\x1b[A"),
		},
		{
			name: "bytes then enter",
			body: `{"bytes":"` + base64.StdEncoding.EncodeToString([]byte("echo hi")) + `","enter":true}`,
			want: []byte("echo hi\r"),
		},
		{
			name: "a byte that is not valid UTF-8",
			// A lone 0xff cannot appear in a JSON string, so this is the case
			// that a text-only input channel would have to refuse.
			body: `{"bytes":"` + base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}) + `"}`,
			want: []byte{0xff, 0xfe},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, debugAPI: true})
			id, _ := h.registerProject(t, "app")
			h.startRuntime(t, id)

			recorder := h.call(http.MethodPost, "/api/debug/projects/"+id+"/runtime/input", tc.body)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body was %s", recorder.Code, recorder.Body.String())
			}

			got := h.backend.receivedInput(project.SessionNameFor(id))
			if !bytes.Equal(got, tc.want) {
				t.Errorf("the backend received %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRuntimeInputRefusesAmbiguousText checks the one shape the input endpoint
// will not guess at. Text and bytes are two fields rather than a sequence, so a
// body carrying both has no defined order; picking one silently would send the
// author of a failing test looking in the wrong place.
func TestRuntimeInputRefusesAmbiguousText(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, debugAPI: true})
	id, _ := h.registerProject(t, "app")
	h.startRuntime(t, id)

	body := `{"text":"echo hi","bytes":"` + base64.StdEncoding.EncodeToString([]byte{0x0d}) + `"}`
	recorder := h.call(http.MethodPost, "/api/debug/projects/"+id+"/runtime/input", body)
	h.wantError(t, recorder, http.StatusBadRequest, CodeInvalidRequest)

	if got := h.backend.receivedInput(project.SessionNameFor(id)); len(got) != 0 {
		t.Errorf("a rejected input still reached the backend: %q", got)
	}
}

// TestRuntimeLaunchTypesACommandLine checks the second input path: a whole
// command line typed into the shell, which is how work is started.
func TestRuntimeLaunchTypesACommandLine(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, debugAPI: true})
	id, _ := h.registerProject(t, "app")
	h.startRuntime(t, id)

	recorder := h.call(http.MethodPost, "/api/debug/projects/"+id+"/runtime/input",
		`{"keys":["printf 'hello\n'","sleep 5"]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body was %s", recorder.Code, recorder.Body.String())
	}

	sessionName := project.SessionNameFor(id)
	h.backend.mu.Lock()
	launched := append([]string(nil), h.backend.launched[sessionName]...)
	h.backend.mu.Unlock()

	want := []string{"printf 'hello\n'", "sleep 5"}
	if len(launched) != len(want) {
		t.Fatalf("the backend was asked to run %q, want %q", launched, want)
	}
	for i := range want {
		if launched[i] != want[i] {
			t.Errorf("command %d = %q, want %q", i, launched[i], want[i])
		}
	}
}

// TestRuntimeInputRequiresARunningRuntime keeps a client from believing input
// was delivered to a terminal that is not there.
func TestRuntimeInputRequiresARunningRuntime(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, debugAPI: true})
	id, _ := h.registerProject(t, "app")

	recorder := h.call(http.MethodPost, "/api/debug/projects/"+id+"/runtime/input", `{"text":"ls"}`)
	h.wantError(t, recorder, http.StatusConflict, session.CodeNotRunning)
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

// TestRuntimeOutputIsSequencedAndByteExact is §十四 and §十三 at the API
// boundary: every chunk carries a gap-free sequence number, and the bytes
// arrive as they were produced - escape sequences, non-ASCII text, and control
// characters included.
func TestRuntimeOutputIsSequencedAndByteExact(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, debugAPI: true})
	id, _ := h.registerProject(t, "app")
	h.startRuntime(t, id)

	// Three chunks of the kinds a terminal really produces: a coloured line, a
	// progress update that overwrites itself on one line, and Chinese text.
	sessionName := project.SessionNameFor(id)
	h.backend.deliver(t, sessionName, []byte("\x1b[32mok\x1b[0m\r\n"))
	h.backend.deliver(t, sessionName, []byte("progress 10%\rprogress 20%\r"))
	h.backend.deliver(t, sessionName, []byte("中文输出\n"))
	h.waitForSequence(t, id, 3)

	whole := decodeChunks(t, h.call(http.MethodGet, "/api/debug/projects/"+id+"/runtime/output", ""))
	want := "\x1b[32mok\x1b[0m\r\nprogress 10%\rprogress 20%\r中文输出\n"
	if string(whole) != want {
		t.Errorf("the output pipeline changed the bytes:\n got %q\nwant %q", whole, want)
	}

	// The resume point returns exactly what came after it.
	body := decode[debugOutputResponse](t,
		h.call(http.MethodGet, "/api/debug/projects/"+id+"/runtime/output?since=2", ""))
	if len(body.Chunks) != 1 {
		t.Fatalf("?since=2 returned %d chunks, want 1 (sequences: %v)", len(body.Chunks), sequencesOf(body.Chunks))
	}
	if body.Chunks[0].Sequence != 3 {
		t.Errorf("the chunk after sequence 2 is numbered %d, want 3", body.Chunks[0].Sequence)
	}
	if body.Sequence != 3 {
		t.Errorf("the reported sequence is %d, want 3", body.Sequence)
	}

	// A malformed resume point is the client's mistake and is reported as one.
	h.wantError(t,
		h.call(http.MethodGet, "/api/debug/projects/"+id+"/runtime/output?since=-1", ""),
		http.StatusBadRequest, CodeInvalidRequest)
}

// TestRuntimeSnapshotKeepsEscapeSequences checks the diagnostic that a future
// client will draw before live output arrives. It is a redraw, not a stream,
// but it must not be stripped on the way out either.
func TestRuntimeSnapshotKeepsEscapeSequences(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, debugAPI: true})
	id, _ := h.registerProject(t, "app")
	h.startRuntime(t, id)

	body := decode[debugOutputResponse](t,
		h.call(http.MethodGet, "/api/debug/projects/"+id+"/runtime/output?snapshot=1", ""))
	if body.Session == nil {
		t.Fatal("?snapshot=1 returned no session description")
	}
	if !bytes.Contains(body.Snapshot, []byte("\x1b[")) {
		t.Errorf("the snapshot lost its escape sequences: %q", body.Snapshot)
	}
}

// ---------------------------------------------------------------------------
// The diagnostic surface
// ---------------------------------------------------------------------------

// TestDebugEndpointsDoNotExistByDefault is §十九. The diagnostics exist so the
// runtime can be exercised before there is a Web Terminal to exercise it with;
// they are not a product API, and an ordinary installation must not have a
// route that accepts raw terminal input.
func TestDebugEndpointsDoNotExistByDefault(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	id, _ := h.registerProject(t, "app")

	for _, tc := range []struct {
		method string
		target string
	}{
		{http.MethodGet, "/api/debug/runtimes"},
		{http.MethodPost, "/api/debug/reconcile"},
		{http.MethodGet, "/api/debug/projects/" + id + "/runtime/output"},
		{http.MethodPost, "/api/debug/projects/" + id + "/runtime/input"},
		{http.MethodPost, "/api/debug/projects/" + id + "/runtime/resize"},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			recorder := h.call(tc.method, tc.target, "")
			h.wantError(t, recorder, http.StatusNotFound, CodeNotFound)
		})
	}
}

// TestDebugRuntimesListsWhatTheBackendHas checks the diagnostic that answers
// "what does the runtime actually hold", which is the question a user asks
// after a restart surprises them.
func TestDebugRuntimesListsWhatTheBackendHas(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, debugAPI: true})
	id, _ := h.registerProject(t, "app")
	h.startRuntime(t, id)

	body := decode[debugRuntimesResponse](t, h.call(http.MethodGet, "/api/debug/runtimes", ""))
	if body.Backend != "fake" {
		t.Errorf("backend = %q, want the backend in use", body.Backend)
	}
	if len(body.Sessions) != 1 || body.Sessions[0].Name != project.SessionNameFor(id) {
		t.Errorf("sessions = %v, want the one session for %s", sessionNamesOf(body.Sessions), id)
	}
}

// sequencesOf renders chunk sequence numbers for a failure message.
func sequencesOf(chunks []session.Chunk) []uint64 {
	out := make([]uint64, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, chunk.Sequence)
	}
	return out
}

// sessionNamesOf renders session names for a failure message.
func sessionNamesOf(sessions []session.SessionRef) []string {
	out := make([]string, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.Name)
	}
	return out
}
