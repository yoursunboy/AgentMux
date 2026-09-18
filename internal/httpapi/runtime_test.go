package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/storage"
	"github.com/kutonlagos/agentmux/internal/terminal"
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

// Snapshot returns the pane the terminal is showing.
//
// It answers with a Screen rather than with bytes because that is what a client
// draws from: the geometry, the buffer and the cursor are part of the answer,
// and a double that returned only text would let a handler drop all three and
// still pass. The text carries escape sequences for the same reason - a
// snapshot with its colour stripped is a different terminal from the one the
// user is looking at.
func (b *fakeBackend) Snapshot(_ context.Context, name string) (session.Screen, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	found, ok := b.sessions[name]
	if !ok {
		return session.Screen{}, session.ErrNoSuchSession
	}
	return session.Screen{
		Data:    []byte("\x1b[32mfake pane\x1b[0m"),
		Cols:    found.Cols,
		Rows:    found.Rows,
		CursorY: found.Rows - 1,
	}, nil
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

// ---------------------------------------------------------------------------
// The terminal socket
// ---------------------------------------------------------------------------

// The tests below are the HTTP half of the Web Terminal: that the endpoint
// exists, that it refuses what it must refuse, and that a browser which speaks
// the protocol really gets a terminal out of it.
//
// They go over a real listener and a real WebSocket handshake, because the part
// of a WebSocket server that a ResponseRecorder cannot test is exactly the part
// that goes wrong. The handshake hijacks the connection and everything after it
// is framed bytes rather than HTTP, so a test that used ServeHTTP with a
// recorder would check the routing and nothing else.
//
// The protocol's own edges - sequence gaps, re-synchronisation, a client that
// stops reading, the message limits - are tested in internal/terminal against a
// fake runtime, where they can be provoked deterministically and where the
// failure is read from the frames rather than inferred. What is here is the
// wiring: route to hub to manager to backend, and back.

// socketReadWait bounds a test's wait for something the server should send.
//
// It is generous because it bounds a failure rather than a success: a passing
// read returns as soon as the bytes arrive, and the only thing this deadline
// decides is how long a test sits there before reporting that nothing came.
const socketReadWait = 5 * time.Second

// terminalHello is the greeting a connection begins with.
//
// It is decoded into a struct of this package's own rather than into the
// terminal package's, which is deliberate: a test that decoded the server's
// greeting with the server's own type would pass even if the field names on the
// wire were wrong, and the wire is what a browser has to agree with.
type terminalHello struct {
	Type     string `json:"type"`
	Protocol int    `json:"protocol"`
	ClientID string `json:"clientId"`
	Server   string `json:"server"`
	Version  string `json:"version"`
}

// terminalConn is a test client for the one terminal socket.
//
// It reads messages rather than asserting on them, so a test can say which one
// it is waiting for and let the rest pass. A client that instead asserted "the
// next thing is the snapshot" would be asserting an ordering the protocol does
// not promise: output produced while the snapshot is being taken is allowed to
// arrive around it, and a test that forbade that would fail on a busy machine.
type terminalConn struct {
	t    *testing.T
	conn *websocket.Conn
}

// dialTerminal opens the terminal socket against this harness's server.
//
// The rest of the suite drives handlers through ServeHTTP with a recorder,
// which is the cheapest way to test an HTTP API. A WebSocket cannot be tested
// that way - the handshake hijacks the connection, and hijacking is exactly
// what a ResponseRecorder is not - so this opens a real listener on a loopback
// port and speaks the real protocol over it.
func (h *harness) dialTerminal(t *testing.T, headers http.Header) *terminalConn {
	t.Helper()

	srv := httptest.NewServer(h.server.Handler())
	socketURL := "ws" + strings.TrimPrefix(srv.URL, "http") + terminal.Endpoint

	conn, resp, err := websocket.DefaultDialer.Dial(socketURL, headers)
	if err != nil {
		srv.Close()
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dialing %s failed with status %d: %v", socketURL, status, err)
	}
	// One cleanup rather than two so that the order cannot be got wrong: the
	// client has to go first, because httptest.Server.Close waits for the
	// connections it is still serving and a hijacked socket is not one the
	// client's own Close reaches from the other side.
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Close()
	})
	return &terminalConn{t: t, conn: conn}
}

// socketURL is the address of this harness's terminal endpoint, for the tests
// that dial it themselves.
func (h *harness) socketURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + terminal.Endpoint
}

// send writes one client message.
func (c *terminalConn) send(v any) {
	c.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("could not encode a client message: %v", err)
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(socketReadWait))
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.t.Fatalf("sending %s failed: %v", data, err)
	}
}

// nextControl waits for the next control message of the given type, letting
// binary frames pass.
func (c *terminalConn) nextControl(want string) []byte {
	c.t.Helper()
	return c.nextControlMatch(want, nil)
}

// nextControlMatch waits for the next control message of the given type that
// also satisfies match, when one is given.
//
// It exists because a type is not always enough to identify the message a test
// is waiting for: a client that resizes twice receives two acknowledgements of
// the same type, and a test that read the first one would be asserting the
// wrong size.
func (c *terminalConn) nextControlMatch(want string, match func([]byte) bool) []byte {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(socketReadWait))
	for {
		kind, data, err := c.conn.ReadMessage()
		if err != nil {
			c.t.Fatalf("waiting for a %q message: %v", want, err)
		}
		if kind != websocket.TextMessage {
			continue
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			c.t.Fatalf("the server sent a text message that is not JSON: %q", data)
		}
		if envelope.Type == want && (match == nil || match(data)) {
			return data
		}
	}
}

// nextFrame waits for the next binary frame of the given type, letting control
// messages pass.
//
// It decodes with the protocol's own decoder rather than by hand, so the test
// asserts on a frame's meaning rather than on its byte layout; that the layout
// is what the documentation says is the internal/terminal frame tests' job.
func (c *terminalConn) nextFrame(want uint8) terminal.Frame {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(socketReadWait))
	for {
		kind, data, err := c.conn.ReadMessage()
		if err != nil {
			c.t.Fatalf("waiting for a binary frame of type %#x: %v", want, err)
		}
		if kind != websocket.BinaryMessage {
			continue
		}
		frame, err := terminal.DecodeFrame(data)
		if err != nil {
			c.t.Fatalf("the server sent a frame a client cannot decode: %v", err)
		}
		if frame.Type == want {
			return frame
		}
	}
}

// hello reads and decodes the greeting.
func (c *terminalConn) hello() terminalHello {
	c.t.Helper()
	var decoded terminalHello
	if err := json.Unmarshal(c.nextControl(terminal.MsgHello), &decoded); err != nil {
		c.t.Fatalf("the greeting did not decode: %v", err)
	}
	return decoded
}

// subscribe asks for a project's terminal.
func (c *terminalConn) subscribe(projectID string, cols, rows int) {
	c.t.Helper()
	c.send(map[string]any{
		"type":      terminal.MsgSubscribe,
		"projectId": projectID,
		"cols":      cols,
		"rows":      rows,
	})
}

// takeControl asks for a project's lease and waits until it is granted.
//
// It is what every client that types or resizes has to do first. A browser that
// has just connected is a viewer, whatever it intends to become.
func (c *terminalConn) takeControl(projectID string) {
	c.t.Helper()
	c.send(map[string]any{"type": terminal.MsgControlRequest, "projectId": projectID})
	var granted struct {
		Type    string `json:"type"`
		Reason  string `json:"reason"`
		Control struct {
			Controller *struct {
				ClientID string `json:"clientId"`
				Device   string `json:"device"`
			} `json:"controller"`
		} `json:"control"`
	}
	if err := json.Unmarshal(c.nextControl(terminal.MsgControlGranted), &granted); err != nil {
		c.t.Fatalf("the control grant did not decode: %v", err)
	}
	if granted.Reason != "available" {
		c.t.Fatalf("the grant gave reason %q, want %q", granted.Reason, "available")
	}
	if granted.Control.Controller == nil || granted.Control.Controller.ClientID == "" {
		c.t.Fatalf("the grant names no controller: %+v", granted.Control)
	}
}

// resize states the size the client's viewport has.
func (c *terminalConn) resize(projectID string, cols, rows int) {
	c.t.Helper()
	c.send(map[string]any{
		"type":      terminal.MsgResize,
		"projectId": projectID,
		"cols":      cols,
		"rows":      rows,
	})
}

// close ends the connection, as closing a browser tab does.
func (c *terminalConn) close() { _ = c.conn.Close() }

// TestTheTerminalSocketGreetsAndIdentifiesItself checks the endpoint answers
// with the protocol rather than with a route that happens to exist.
//
// A client that cannot tell what it is talking to has to guess, and a client
// that guesses draws the wrong thing rather than refusing.
func TestTheTerminalSocketGreetsAndIdentifiesItself(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})

	conn := h.dialTerminal(t, nil)
	greeting := conn.hello()

	if greeting.Protocol != terminal.ProtocolVersion {
		t.Errorf("the server greeted with protocol %d, want %d",
			greeting.Protocol, terminal.ProtocolVersion)
	}
	if greeting.ClientID == "" {
		t.Error("the greeting carries no client id, so a browser cannot be identified in a log")
	}
	if greeting.Server == "" {
		t.Error("the greeting does not say which server answered")
	}
}

// TestTheTerminalSocketAnswersTheSubprotocolABrowserOffers is the handshake a
// browser needs and a program does not.
//
// A browser that offers a subprotocol and is answered without one fails the
// connection outright, so a client offering agentmux.terminal.v1 against a
// server that does not list it does not open a socket at all. The second half
// of the test is the half that is easy to lose while fixing the first: a client
// that offers nothing must still be answered, which is every non-browser client
// and every other test in this package.
func TestTheTerminalSocketAnswersTheSubprotocolABrowserOffers(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})

	t.Run("offered", func(t *testing.T) {
		srv := httptest.NewServer(h.server.Handler())
		defer srv.Close()

		dialer := websocket.Dialer{Subprotocols: []string{terminal.Subprotocol}}
		conn, resp, err := dialer.Dial(h.socketURL(srv), nil)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Fatalf("dialing with the terminal subprotocol failed with status %d: %v", status, err)
		}
		defer conn.Close()

		if got := conn.Subprotocol(); got != terminal.Subprotocol {
			t.Errorf("negotiated subprotocol %q, want %q", got, terminal.Subprotocol)
		}
	})

	t.Run("not offered", func(t *testing.T) {
		conn := h.dialTerminal(t, nil)
		if got := conn.conn.Subprotocol(); got != "" {
			t.Errorf("negotiated subprotocol %q with a client that asked for none", got)
		}
	})
}

// TestTheTerminalSocketRefusesAProtocolItDoesNotSpeak is the other half of
// versioning.
//
// An absent version is allowed - a client that has not stated one is told what
// this server speaks - but a stated version that is wrong is a deliberate
// question with a knowable answer, and the answer is not "guess". A stale tab
// left open across a server upgrade has to be told it is stale rather than fed
// bytes it will mis-draw.
//
// Version 1 is in the list rather than the current version, and it is the
// interesting one: it is the version a tab left open across the Phase 6 upgrade
// is speaking, and it is refused because a version 1 client would be shown a
// terminal it believes it can type into. See ProtocolVersion.
func TestTheTerminalSocketRefusesAProtocolItDoesNotSpeak(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})

	for _, version := range []string{"0", "1", "99", "latest"} {
		t.Run(version, func(t *testing.T) {
			h.wantError(t,
				h.call(http.MethodGet, terminal.Endpoint+"?"+terminal.ProtocolParam+"="+version, ""),
				http.StatusBadRequest, CodeInvalidRequest)
		})
	}
}

// TestTheTerminalSocketRefusesAForeignOrigin is §五十二.
//
// The check exists because a WebSocket is not subject to the same-origin policy
// once a server has accepted it. Without it, any page in any browser on this
// machine - and, on an installation reachable from a LAN, any page in any
// browser on that LAN - could open a terminal on this project and type into it,
// including into a Claude Code session that is mid-run.
func TestTheTerminalSocketRefusesAForeignOrigin(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})

	for _, origin := range []string{
		"http://evil.example",
		"https://evil.example",
		// The shape the check could plausibly get wrong: a host that merely
		// starts with this server's address.
		"http://127.0.0.1.evil.example",
	} {
		t.Run(origin, func(t *testing.T) {
			h.wantError(t,
				h.call(http.MethodGet, terminal.Endpoint, "", "Origin", origin),
				http.StatusForbidden, CodeForbidden)
		})
	}
}

// TestTheTerminalSocketAcceptsItsOwnOrigin is the case the policy exists to
// permit.
//
// A page served by this server may open a terminal on it, wherever the server
// is reachable. That is what makes AgentMux usable from a tablet on a LAN
// without configuring anything, and it is safe for the obvious reason: the page
// came from here.
func TestTheTerminalSocketAcceptsItsOwnOrigin(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})

	srv := httptest.NewServer(h.server.Handler())
	defer srv.Close()

	origin := "http://" + strings.TrimPrefix(srv.URL, "http://")
	conn, resp, err := websocket.DefaultDialer.Dial(
		h.socketURL(srv), http.Header{"Origin": []string{origin}})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("a page served by this server was refused a terminal (status %d, origin %s): %v",
			status, origin, err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(socketReadWait))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("the connection was accepted but says nothing: %v", err)
	}
}

// TestTheTerminalSocketOnlyServesRegisteredProjects is §五十.
//
// A client names a project; it never names a path, a session, or a command.
// The identifier is resolved against what this server has registered, so a
// socket cannot reach a terminal the server was not told about, and there is no
// message in the protocol that would let it try.
func TestTheTerminalSocketOnlyServesRegisteredProjects(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	id, _ := h.registerProject(t, "app")
	h.startRuntime(t, id)

	conn := h.dialTerminal(t, nil)
	conn.hello()

	for _, tc := range []struct {
		name      string
		projectID string
		code      string
	}{
		{
			// A well-formed identifier for a project this server does not have.
			// It is refused by the project lookup rather than by the syntax
			// check, which is why it carries the project code.
			name:      "a project that is not registered",
			projectID: "p_00000000000000000000",
			code:      session.CodeProjectNotFound,
		},
		{
			// A path, dressed as an identifier. This is the case the syntax
			// check exists for, and it is checked before anything is resolved.
			name:      "a filesystem path",
			projectID: "../../etc",
			code:      terminal.CodeBadProject,
		},
		{
			// The session name rather than the project id: the two are related
			// by a prefix, and a client that confused them would otherwise reach
			// a terminal by the wrong name.
			name:      "a session name",
			projectID: project.SessionNameFor(id),
			code:      terminal.CodeBadProject,
		},
		{
			name:      "an empty identifier",
			projectID: "",
			code:      terminal.CodeBadProject,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn.subscribe(tc.projectID, 0, 0)

			var refusal struct {
				Type      string `json:"type"`
				Code      string `json:"code"`
				Message   string `json:"message"`
				ProjectID string `json:"projectId"`
			}
			if err := json.Unmarshal(conn.nextControl(terminal.MsgError), &refusal); err != nil {
				t.Fatalf("the refusal did not decode: %v", err)
			}
			if refusal.Code != tc.code {
				t.Errorf("subscribing to %q was refused with %q (%s), want %q",
					tc.projectID, refusal.Code, refusal.Message, tc.code)
			}
			if refusal.Message == "" {
				t.Error("the refusal does not say what was wrong")
			}
		})
	}

	// Nothing was subscribed, so the connection still holds nothing - and the
	// registered project it could have subscribed to is still available, which
	// is what says the refusals did not consume anything.
	if got := h.server.terminal.Stats().Subscriptions; got != 0 {
		t.Errorf("the server holds %d subscriptions after four refusals, want 0", got)
	}
	conn.subscribe(id, 80, 24)
	if frame := conn.nextFrame(terminal.FrameSnapshot); frame.ProjectID != id {
		t.Errorf("the snapshot names %q, want %q", frame.ProjectID, id)
	}
}

// TestABrowserGetsAWorkingTerminal is the phase's acceptance criterion stated
// as one exchange.
//
// A browser connects, subscribes to a project, is sent the screen that is
// already there, takes control of it, states its viewport size, is sent what
// the terminal produces next, and types into it. Each step is checked against
// the backend the manager is driving, so a frame that arrived without the
// corresponding input reaching the terminal - or the reverse - fails here.
func TestABrowserGetsAWorkingTerminal(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	id, _ := h.registerProject(t, "app")
	h.startRuntime(t, id)
	name := project.SessionNameFor(id)

	conn := h.dialTerminal(t, nil)
	conn.hello()

	// Watching comes first, and control after it. A browser that has just
	// connected is a viewer, and a client can only ask to control a terminal it
	// is already watching - which is the ordering the whole of this phase is,
	// and the reason this test now takes control instead of simply typing.
	conn.subscribe(id, 0, 0)

	// The screen. A client that started from an empty terminal would show a
	// blank window until something happened to print, which on a Claude Code
	// session is a long and alarming blank.
	snapshot := conn.nextFrame(terminal.FrameSnapshot)
	if snapshot.ProjectID != id {
		t.Errorf("the snapshot names project %q, want %q", snapshot.ProjectID, id)
	}
	// The screen is drawn at the size the terminal actually is, which is the
	// size the program inside is drawing for.
	if cols, rows, ok := h.backend.sizeOf(name); ok && (snapshot.Cols != cols || snapshot.Rows != rows) {
		t.Errorf("the snapshot is %dx%d, want the terminal's %dx%d",
			snapshot.Cols, snapshot.Rows, cols, rows)
	}
	if !bytes.Contains(snapshot.Payload, []byte("\x1b[")) {
		t.Errorf("the snapshot stripped the pane's escape sequences: %q", snapshot.Payload)
	}
	if snapshot.FirstSequence != snapshot.LastSequence {
		t.Errorf("the snapshot's boundary is %d..%d, want a single number: a screen is "+
			"a statement about the whole terminal, not a range of chunks",
			snapshot.FirstSequence, snapshot.LastSequence)
	}

	// Control. A viewer draws a terminal it cannot type into; this is the
	// browser asking to stop being one, and being granted it because nobody
	// else had it.
	conn.takeControl(id)

	// Now that it holds the lease, the browser states the size its viewport
	// has. The size reaches the terminal, and the acknowledgement comes back to
	// the client that asked as well as to any other viewer: there is one pty, so
	// there is one size, and a second tab rendering its own idea of it would
	// draw a terminal that does not match the program inside.
	var ack struct {
		Type string `json:"type"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}
	conn.resize(id, 100, 30)
	if err := json.Unmarshal(conn.nextControl(terminal.MsgResized), &ack); err != nil {
		t.Fatalf("the resize acknowledgement did not decode: %v", err)
	}
	if ack.Cols != 100 || ack.Rows != 30 {
		t.Errorf("the terminal was resized to %dx%d, want the 100x30 the browser asked for", ack.Cols, ack.Rows)
	}
	if cols, rows, ok := h.backend.sizeOf(name); !ok || cols != 100 || rows != 30 {
		t.Errorf("the backend was asked for %dx%d (set=%v), want 100x30", cols, rows, ok)
	}

	// What the terminal produces next arrives as output, byte for byte. The
	// bytes are the kinds a real session makes: a colour escape, a Chinese
	// character, and a carriage return.
	live := []byte("\x1b[32mclaude\x1b[0m\r\n中文\r\n")
	h.backend.deliver(t, name, live)

	output := conn.nextFrame(terminal.FrameOutput)
	if !bytes.Equal(output.Payload, live) {
		t.Errorf("the live frame carries %q, want %q", output.Payload, live)
	}
	// This is the rule a client applies to decide whether it missed anything,
	// asserted where it is produced rather than only where it is consumed.
	if !output.Accounts(snapshot.LastSequence) {
		t.Errorf("the output frame covers %d..%d, which does not follow the snapshot's %d: "+
			"a client applying its own gap rule would reject it",
			output.FirstSequence, output.LastSequence, snapshot.LastSequence)
	}

	// Typing. A keystroke is bytes rather than text, and Ctrl-C is the case
	// that proves it: it is the byte that interrupts whatever is running.
	conn.send(map[string]any{
		"type":      terminal.MsgInput,
		"projectId": id,
		"data":      []byte{0x03},
	})
	h.waitForInput(t, id, []byte{0x03})

	// An arrow key and multi-byte text in one message, which is what a paste
	// and a key held down both look like. The terminal sorts input from output
	// by nothing at all, so the only thing a test can check is that what was
	// typed is what the terminal received, in order and with nothing added.
	conn.send(map[string]any{
		"type":      terminal.MsgInput,
		"projectId": id,
		"data":      append([]byte("\x1b[A"), []byte("中文")...),
	})
	h.waitForInput(t, id, append([]byte{0x03, 0x1b, '[', 'A'}, []byte("中文")...))

	// A resize reaches the terminal, and comes back to this subscriber too.
	conn.send(map[string]any{
		"type":      terminal.MsgResize,
		"projectId": id,
		"cols":      120,
		"rows":      40,
	})
	if err := json.Unmarshal(conn.nextControlMatch(terminal.MsgResized, func(raw []byte) bool {
		var m struct {
			Cols int `json:"cols"`
			Rows int `json:"rows"`
		}
		return json.Unmarshal(raw, &m) == nil && m.Cols == 120 && m.Rows == 40
	}), &ack); err != nil {
		t.Fatalf("the resize acknowledgement did not decode: %v", err)
	}
	h.waitForSize(t, id, 120, 40)

	// Unsubscribing is acknowledged, so a client tearing a terminal down can
	// tell that the server has stopped sending for it rather than guess from
	// silence.
	conn.send(map[string]any{"type": terminal.MsgUnsubscribe, "projectId": id})
	var unsubscribed struct {
		Type      string `json:"type"`
		ProjectID string `json:"projectId"`
	}
	if err := json.Unmarshal(conn.nextControl(terminal.MsgUnsubscribed), &unsubscribed); err != nil {
		t.Fatalf("the unsubscribe acknowledgement did not decode: %v", err)
	}
	if unsubscribed.ProjectID != id {
		t.Errorf("the acknowledgement names %q, want %q", unsubscribed.ProjectID, id)
	}

	// And input for a project this connection is no longer watching is refused.
	// This is §五十一 at the protocol level: the socket types into a terminal
	// the user already started, and into nothing else.
	conn.send(map[string]any{
		"type":      terminal.MsgInput,
		"projectId": id,
		"data":      []byte("rm -rf /"),
	})
	var refusal struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(conn.nextControl(terminal.MsgError), &refusal); err != nil {
		t.Fatalf("the refusal did not decode: %v", err)
	}
	if refusal.Code != terminal.CodeNotSubscribed {
		t.Errorf("input after unsubscribing was refused with %q, want %q",
			refusal.Code, terminal.CodeNotSubscribed)
	}
	h.waitForInput(t, id, append([]byte{0x03, 0x1b, '[', 'A'}, []byte("中文")...))
}

// TestClosingTheBrowserLeavesTheTerminalAlone is §一 and §三十八: the browser
// is a viewer, not the owner.
//
// A runtime that ended when its last viewer left would be a runtime that ends
// whenever somebody's laptop sleeps. The subscription is the browser's; the
// session belongs to the project, and the control connection that reads it
// belongs to the manager.
func TestClosingTheBrowserLeavesTheTerminalAlone(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	id, _ := h.registerProject(t, "app")
	h.startRuntime(t, id)
	name := project.SessionNameFor(id)

	conn := h.dialTerminal(t, nil)
	conn.hello()
	conn.subscribe(id, 80, 24)
	conn.nextFrame(terminal.FrameSnapshot)

	before := h.runtimeState(t, id).Sequence

	conn.close()
	h.waitForSubscriptions(t, 0)

	state := h.runtimeState(t, id)
	if state.State != session.StateRunning {
		t.Errorf("the runtime is %s after the browser left, want %s", state.State, session.StateRunning)
	}
	if !state.SessionAlive {
		t.Error("the session is gone after the browser left; the terminal outlives its viewers")
	}

	// And the terminal still works with nobody watching. Output produced
	// between a disconnect and a reconnect is still numbered, which is what the
	// client that reconnects reads its snapshot's boundary against.
	h.backend.deliver(t, name, []byte("still here\n"))
	h.waitForSequence(t, id, before+1)
}

// TestTheDiagnosticSurfaceIsGone is §七十六 and §九十二's criterion that
// /api/debug is deleted rather than switched off.
//
// Those endpoints existed so the runtime could be exercised before there was a
// Web Terminal to exercise it with. There is one now, and a route that accepts
// raw terminal input - or lists every session on the machine - is a liability
// rather than a convenience: it is unauthenticated, it is not part of the
// product, and a build that had it disabled still had it.
//
// The paths are enumerated rather than sampled, because the failure this guards
// against is one handler left registered, and a test that checked the two
// endpoints somebody remembered would not find it.
func TestTheDiagnosticSurfaceIsGone(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	id, _ := h.registerProject(t, "app")
	h.startRuntime(t, id)

	paths := []string{
		"/api/debug",
		"/api/debug/",
		"/api/debug/runtimes",
		"/api/debug/reconcile",
		"/api/debug/projects/" + id + "/runtime",
		"/api/debug/projects/" + id + "/runtime/start",
		"/api/debug/projects/" + id + "/runtime/stop",
		"/api/debug/projects/" + id + "/runtime/output",
		"/api/debug/projects/" + id + "/runtime/input",
		"/api/debug/projects/" + id + "/runtime/resize",
		"/api/debug/projects/" + id + "/terminal",
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, path := range paths {
			t.Run(method+" "+path, func(t *testing.T) {
				h.wantError(t, h.call(method, path, ""), http.StatusNotFound, CodeNotFound)
			})
		}
	}
}

// runtimeState reads a project's runtime as the API would describe it.
func (h *harness) runtimeState(t *testing.T, projectID string) *session.Runtime {
	t.Helper()
	rt, err := h.runtime.Runtime(context.Background(), projectID)
	if err != nil {
		t.Fatalf("reading the runtime of %s failed: %v", projectID, err)
	}
	return rt
}

// waitForInput waits until the terminal has received exactly want.
//
// The assertion is equality rather than containment on purpose. A terminal has
// no way to tell input from output, so the only thing a test can check about
// typing is that what was typed is what arrived, in that order, with nothing
// added or lost. Input crosses two goroutines on its way - the socket's reader
// and the manager's - so a test that read it back immediately would be racing
// one of them.
func (h *harness) waitForInput(t *testing.T, projectID string, want []byte) {
	t.Helper()
	name := project.SessionNameFor(projectID)

	deadline := time.Now().Add(5 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		if got = h.backend.receivedInput(name); bytes.Equal(got, want) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the terminal received %q, want %q", got, want)
}

// waitForSize waits until the terminal has been resized to exactly cols x rows.
func (h *harness) waitForSize(t *testing.T, projectID string, cols, rows int) {
	t.Helper()
	name := project.SessionNameFor(projectID)

	deadline := time.Now().Add(5 * time.Second)
	var gotCols, gotRows int
	for time.Now().Before(deadline) {
		if gotCols, gotRows, _ = h.backend.sizeOf(name); gotCols == cols && gotRows == rows {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the terminal was resized to %dx%d, want %dx%d", gotCols, gotRows, cols, rows)
}

// waitForSubscriptions waits until the server holds exactly want browser
// subscriptions.
//
// The manager's own control subscription is not counted here: this is the
// hub's count, which is the browsers'.
func (h *harness) waitForSubscriptions(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		if got = h.server.terminal.Stats().Subscriptions; got == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the server holds %d browser subscriptions, want %d", got, want)
}
