package main

// Real Claude Code, end to end.
//
// These tests start the actual Claude Code CLI inside an actual project runtime
// and check what happened. They live in package main rather than beside the
// runtime for one reason: the translation from a resolved installation into a
// runnable agent is agentSpecs, and a test of any other translation would be a
// test of something the server does not run.
//
// They are opt-in, in two steps.
//
//	AGENTMUX_TEST_REAL_CLAUDE=1         start and stop a real process
//	AGENTMUX_TEST_REAL_CLAUDE_PROMPT=1  additionally type a prompt at it, which
//	                                    needs a signed-in CLI and spends tokens
//
// Off is the default, so `go test ./...` costs nothing, needs no Claude Code
// installation, and does not turn a build machine into a machine with failing
// tests. The second gate exists because "is this CLI signed in" cannot be
// answered from a terminal's text, and a test that guessed would be guessing
// about the user's credentials.
//
// What they deliberately do not do:
//
//   - They never pass --dangerously-skip-permissions, --permission-mode, or any
//     other flag that approves a tool call on the user's behalf. The permission
//     model is the thing being preserved; a test that disabled it would be
//     testing a program nobody runs.
//   - They never pass --model. The CLI's own configuration decides which model
//     it uses, and AgentMux has no opinion.
//   - They never decide anything from the terminal's text. A start is confirmed
//     by a process appearing in the kernel's process table, not by a screen
//     that looks ready.
//   - They never let the agent's working directory be this repository. The
//     fixture is a temporary directory, and assertOutsideSourceTree checks that
//     rather than trusting it: the program under test is a coding agent, and
//     pointing one at the code that is running it is how a test edits its own
//     source.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
)

// Environment variables that turn these tests on, and the settings a machine
// that keeps its tmux somewhere unusual can override.
const (
	realClaudeEnv       = "AGENTMUX_TEST_REAL_CLAUDE"
	realClaudePromptEnv = "AGENTMUX_TEST_REAL_CLAUDE_PROMPT"
	tmuxBinaryEnv       = "AGENTMUX_TEST_TMUX_BINARY"
	shellEnv            = "AGENTMUX_TEST_SHELL"
)

// requireRealClaude skips unless the caller has asked for the real CLI and this
// machine can host it.
func requireRealClaude(t *testing.T) {
	t.Helper()
	if os.Getenv(realClaudeEnv) != "1" {
		t.Skipf("set %s=1 to run the tests that start the real Claude Code CLI", realClaudeEnv)
	}
	if runtime.GOOS != "linux" {
		t.Skipf("the runtime is tmux and the agent is a Linux process; this host is %s", runtime.GOOS)
	}
	bin := tmuxBinary(t)
	if _, err := exec.LookPath(bin); err != nil {
		if _, statErr := os.Stat(bin); statErr != nil {
			t.Skipf("tmux (%s) is not installed, so there is no runtime to test", bin)
		}
	}
}

// requireRealClaudePrompt skips unless the prompt tests were asked for by name.
func requireRealClaudePrompt(t *testing.T) {
	t.Helper()
	requireRealClaude(t)
	if os.Getenv(realClaudePromptEnv) != "1" {
		t.Skipf("set %s=1 to run the tests that type at the real CLI; "+
			"they need a CLI that is signed in and they spend tokens", realClaudePromptEnv)
	}
}

// tmuxBinary is the tmux the test runtimes run.
func tmuxBinary(t *testing.T) string {
	t.Helper()
	if bin := strings.TrimSpace(os.Getenv(tmuxBinaryEnv)); bin != "" {
		return bin
	}
	return session.DefaultTmuxBinary
}

// runtimeShell is the shell a test runtime's session runs, and the name the
// runtime compares the terminal's foreground process against before it types a
// command at it. It has to be a shell that exists.
func runtimeShell(t *testing.T) string {
	t.Helper()
	if shell := strings.TrimSpace(os.Getenv(shellEnv)); shell != "" {
		return shell
	}
	for _, candidate := range []string{"/bin/bash", "/usr/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Skip("no shell was found to run the test runtime's session")
	return ""
}

// testLogger keeps the runtime's diagnostics out of a passing run and shows
// them in a failing one. `go test -v` is how a maintainer asks to see them.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// The process table
//
// Everything below reads /proc directly instead of asking the runtime. A test
// that asked the manager where the agent is and then asserted the manager's
// answer would pass for a manager that was wrong in both places, and "the agent
// runs in the project's directory" is exactly the claim that must not be
// self-certified.
// ---------------------------------------------------------------------------

// processRef is one process, as the kernel describes it.
type processRef struct {
	PID        int
	Executable string
	Dir        string
	Parent     int
}

func procPath(pid int, name string) string {
	return "/proc/" + strconv.Itoa(pid) + "/" + name
}

// describeProcess reads a process's executable and working directory. A process
// that has exited fails here, which is reported as absence rather than as an
// error, because that is what it is.
func describeProcess(pid int) (processRef, bool) {
	exe, err := os.Readlink(procPath(pid, "exe"))
	if err != nil {
		return processRef{}, false
	}
	ref := processRef{PID: pid, Executable: cleanProcPath(exe)}
	if dir, err := os.Readlink(procPath(pid, "cwd")); err == nil {
		ref.Dir = cleanProcPath(dir)
	}
	if parent, ok := processParent(pid); ok {
		ref.Parent = parent
	}
	return ref, true
}

// processParent reads a process's parent from /proc/<pid>/stat.
//
// The second field is the executable name in parentheses and may contain spaces
// and parentheses of its own, so the name is skipped by finding the last ')' and
// counting fields from there. Splitting on whitespace would misread every field
// after it.
func processParent(pid int) (int, bool) {
	data, err := os.ReadFile(procPath(pid, "stat"))
	if err != nil {
		return 0, false
	}
	line := string(data)
	closing := strings.LastIndexByte(line, ')')
	if closing < 0 || closing+2 > len(line) {
		return 0, false
	}
	// Fields after the name start at field 3 (state); ppid is field 4.
	fields := strings.Fields(line[closing+1:])
	if len(fields) < 2 {
		return 0, false
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return parent, true
}

// cleanProcPath trims the suffix the kernel appends to a path whose file no
// longer exists. Claude Code replaces its own versioned directory when it
// updates in place, so a running process's executable can be reported as
// deleted, and a test that did not trim it would fail after an update.
func cleanProcPath(path string) string {
	return filepath.Clean(strings.TrimSuffix(path, " (deleted)"))
}

// descendants returns every process descended from root, root included.
//
// Including root is deliberate: a session adopted from a previous server, or
// created by hand, may have been started with the agent as its leader.
func descendants(root int) map[int]bool {
	parent := map[int]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return map[int]bool{root: true}
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if ppid, ok := processParent(pid); ok {
			parent[pid] = ppid
		}
	}

	children := map[int][]int{}
	for pid, ppid := range parent {
		children[ppid] = append(children[ppid], pid)
	}

	seen := map[int]bool{root: true}
	for queue := []int{root}; len(queue) > 0; {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			if seen[child] {
				continue
			}
			seen[child] = true
			queue = append(queue, child)
		}
	}
	return seen
}

// runningExecutable returns the processes in a pane's tree that are running the
// given executable. It is the kernel's answer to "is the agent in there", asked
// without the runtime's help.
func runningExecutable(root int, executable string) []processRef {
	target := cleanProcPath(executable)
	var found []processRef
	for pid := range descendants(root) {
		ref, ok := describeProcess(pid)
		if !ok {
			continue
		}
		if ref.Executable == target {
			found = append(found, ref)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].PID < found[j].PID })
	return found
}

// outermostRunning returns the processes in a pane's tree that are running the
// given executable and are not inside another process that is running it.
//
// Measured on Claude Code 2.1.274: it starts helpers from its own binary, so
// "how many processes in this terminal are running Claude" has an answer greater
// than one, and the agent is the outermost of them. The runtime picks the
// shallowest match for the same reason - its own comment says "the agent itself
// rather than a helper it spawned later from a copy of its own executable" - and
// this is what checks that it did.
func outermostRunning(root int, executable string) []processRef {
	refs := runningExecutable(root, executable)
	var out []processRef
	for _, ref := range refs {
		nested := false
		for _, other := range refs {
			if other.PID != ref.PID && descendants(other.PID)[ref.PID] {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, ref)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The environment and the server
// ---------------------------------------------------------------------------

// claudeEnv is the state that outlives one server process: which projects are
// registered and what was written about their runtimes. Building a second
// server over one environment is what a server restart is, and it is the only
// way to test that an agent outlives the process that started it.
type claudeEnv struct {
	mu       sync.Mutex
	dataDir  string
	store    *memoryRuntimeStore
	projects map[string]*project.Project
}

func newClaudeEnv(t *testing.T) *claudeEnv {
	t.Helper()
	return &claudeEnv{
		dataDir:  t.TempDir(),
		store:    newMemoryRuntimeStore(),
		projects: map[string]*project.Project{},
	}
}

// Register creates a project whose directory is a fresh temporary one.
func (e *claudeEnv) Register(t *testing.T, name string) *project.Project {
	t.Helper()
	dir := claudeProjectDir(t, name)
	id, err := project.NewID()
	if err != nil {
		t.Fatalf("could not mint a project id: %v", err)
	}
	now := time.Now().UTC()
	p := &project.Project{
		ID:          id,
		Name:        name,
		HostPath:    dir,
		RuntimePath: dir,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	e.mu.Lock()
	e.projects[id] = p
	e.mu.Unlock()
	return p
}

// Server builds one server's worth of runtime machinery over this environment,
// wired the way cmd/server wires it.
func (e *claudeEnv) Server(t *testing.T) *claudeHarness {
	t.Helper()
	logger := testLogger(t)

	runtimes, err := session.NewProjectRuntimes(session.ProjectRuntimesOptions{
		Binary:    tmuxBinary(t),
		SocketDir: filepath.Join(e.dataDir, session.DefaultTmuxSocketDirName),
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("could not build the project runtimes: %v", err)
	}

	launcher := claude.New(claude.Options{Logger: logger})
	manager, err := session.NewManager(session.ManagerOptions{
		Backends:          runtimes,
		Sockets:           runtimes.Sockets(),
		Projects:          &memoryProjects{env: e},
		Store:             e.store,
		Shell:             runtimeShell(t),
		Cols:              session.DefaultCols,
		Rows:              session.DefaultRows,
		Agent:             agentSpecs{launcher},
		AgentPoll:         100 * time.Millisecond,
		AgentStartTimeout: 30 * time.Second,
		AgentStopGrace:    3 * time.Second,
		Logger:            logger,
	})
	if err != nil {
		t.Fatalf("could not build the runtime manager: %v", err)
	}

	h := &claudeHarness{t: t, env: e, runtimes: runtimes, manager: manager, launcher: launcher}
	t.Cleanup(h.Close)
	return h
}

// claudeHarness is one server.
type claudeHarness struct {
	t        *testing.T
	env      *claudeEnv
	runtimes *session.ProjectRuntimes
	manager  *session.Manager
	launcher *claude.Launcher

	mu       sync.Mutex
	detached bool
}

// Shutdown closes the manager and leaves the sessions alone.
//
// It is what the product does when the server stops, and it is what a restart
// test has to do: the runtimes must outlive this server, or the next one would
// have nothing to adopt. A harness that has been shut down this way does not
// destroy anything during cleanup, because its manager is gone and the runtimes
// now belong to whichever server adopted them.
func (h *claudeHarness) Shutdown() error {
	h.mu.Lock()
	h.detached = true
	h.mu.Unlock()
	return h.manager.Close()
}

// Close destroys every runtime this server built, then closes the manager.
//
// Destroying is deliberate here even though it is not what the product does: a
// manager's Close leaves sessions running, which is exactly why an agent
// survives a restart, but a test that left them running would leave a Claude
// Code process behind on every run.
func (h *claudeHarness) Close() {
	h.mu.Lock()
	detached := h.detached
	h.mu.Unlock()

	if !detached {
		h.env.mu.Lock()
		ids := make([]string, 0, len(h.env.projects))
		for id := range h.env.projects {
			ids = append(ids, id)
		}
		h.env.mu.Unlock()

		for _, id := range ids {
			if err := h.manager.Destroy(context.Background(), id); err != nil {
				h.t.Logf("could not destroy the runtime for %s during cleanup: %v", id, err)
			}
		}
	}
	if err := h.manager.Close(); err != nil {
		h.t.Logf("could not close the runtime manager during cleanup: %v", err)
	}
}

// Start runs a project's runtime.
func (h *claudeHarness) Start(t *testing.T, p *project.Project) {
	t.Helper()
	if _, err := h.manager.Start(context.Background(), p.ID); err != nil {
		t.Fatalf("could not start the runtime for %s: %v", p.ID, err)
	}
}

// paneProcess reads the project's terminal through the backend rather than
// through the manager, so that the agent's process is checked against the thing
// it is supposed to be inside of and not against the runtime's view of itself.
func (h *claudeHarness) paneProcess(t *testing.T, p *project.Project) session.PaneProcess {
	t.Helper()
	backend := session.NewTmuxBackend(session.TmuxOptions{
		Binary:     tmuxBinary(t),
		SocketPath: h.runtimes.Sockets().Path(p.ID),
		Prefix:     project.SessionPrefix,
		Logger:     testLogger(t),
	})
	pane, err := backend.PaneProcess(context.Background(), p.SessionName())
	if err != nil {
		t.Fatalf("could not read the terminal for %s: %v", p.ID, err)
	}
	return pane
}

// spec resolves the agent the way the server resolves it.
func (h *claudeHarness) spec(t *testing.T) session.AgentSpec {
	t.Helper()
	spec, err := agentSpecs{h.launcher}.Spec(context.Background())
	if err != nil {
		t.Skipf("no Claude Code can be started on this machine: %v", err)
	}
	return spec
}

// startRealClaude starts the agent and waits until its process can be seen.
//
// The CLI is a network client, and on a machine that cannot reach its service it
// can print an error and exit within a second of starting. That is an event in
// the environment rather than a defect in the runtime, so a start that was not
// observable is retried before it is called a failure - and when it is called a
// failure the last status is reported, because "the agent never appeared" and
// "the agent appeared and immediately went away" are different answers and the
// runtime distinguishes them.
func (h *claudeHarness) startRealClaude(t *testing.T, p *project.Project) (session.AgentStatus, processRef) {
	t.Helper()
	const attempts = 3

	var lastStatus session.AgentStatus
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		status, err := h.manager.StartAgent(context.Background(), p.ID)
		switch {
		case err != nil:
			lastErr = err
		case !status.Running || status.PID == 0:
			lastStatus = status
			lastErr = fmt.Errorf("the runtime reports %s with pid %d", status.State, status.PID)
		default:
			if ref, ok := describeProcess(status.PID); ok {
				return status, ref
			}
			lastStatus = status
			lastErr = fmt.Errorf("pid %d is not in the process table", status.PID)
		}
		if attempt < attempts {
			t.Logf("attempt %d of %d could not observe the agent in %s (%v); trying again",
				attempt, attempts, p.ID, lastErr)
			time.Sleep(500 * time.Millisecond)
		}
	}

	t.Fatalf("Claude Code could not be started and observed in %s after %d attempts: status=%+v error=%v",
		p.RuntimePath, attempts, lastStatus, lastErr)
	return session.AgentStatus{}, processRef{}
}

// ---------------------------------------------------------------------------
// The project fixture
// ---------------------------------------------------------------------------

// claudeProjectDir creates the project directory the agent runs in.
//
// It is a fresh directory under the system temporary directory, on purpose
// rather than for convenience. The program under test is a coding agent that
// reads, writes, and runs commands in its working directory; pointing one at the
// AgentMux repository would let a test modify the code that is running it.
// assertOutsideSourceTree turns that rule into a check.
func claudeProjectDir(t *testing.T, label string) string {
	t.Helper()
	base := filepath.Join(os.TempDir(), "agentmux-claude-test", safeName(label))
	dir := filepath.Join(base, "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("could not create the test project directory %s: %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Logf("could not remove the test project directory %s: %v", base, err)
		}
	})

	// A directory that looks like a project, with the file names a coding agent
	// already knows how to read, so that nothing about the fixture is the reason
	// the agent behaves oddly.
	files := map[string]string{
		"README.md":  "# AgentMux Claude runtime test\n\nA temporary project. Nothing in it is real.\n",
		"CLAUDE.md":  "# Instructions\n\nThis directory exists only for an AgentMux test run.\n",
		"sample.txt": "alpha\nbeta\ngamma\n",
	}
	for file, content := range files {
		path := filepath.Join(dir, file)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("could not write the fixture file %s: %v", path, err)
		}
	}

	assertOutsideSourceTree(t, dir)
	return dir
}

// assertOutsideSourceTree fails when path is inside the AgentMux source tree.
func assertOutsideSourceTree(t *testing.T, path string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return
	}
	root = resolvePath(root)
	got := resolvePath(path)
	if got == root || strings.HasPrefix(got, root+string(filepath.Separator)) {
		t.Fatalf("the test project %s is inside the AgentMux source tree %s; "+
			"the agent under test would be able to modify the code that is running it", got, root)
	}
}

// resolvePath follows symlinks where it can, so that two spellings of one
// directory compare equal. /tmp is a symlink on some systems and not on others.
func resolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func safeName(label string) string {
	return strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_").Replace(label)
}

// ---------------------------------------------------------------------------
// The collaborators the server would supply
// ---------------------------------------------------------------------------

// memoryProjects is the slice of the project model the runtime asks for.
type memoryProjects struct{ env *claudeEnv }

func (m *memoryProjects) Get(_ context.Context, id string) (*project.Project, error) {
	m.env.mu.Lock()
	defer m.env.mu.Unlock()
	p, ok := m.env.projects[id]
	if !ok {
		return nil, fmt.Errorf("no project %s", id)
	}
	return p.Clone(), nil
}

func (m *memoryProjects) List(_ context.Context, _ project.ListFilter) ([]*project.Project, error) {
	m.env.mu.Lock()
	defer m.env.mu.Unlock()
	out := make([]*project.Project, 0, len(m.env.projects))
	for _, p := range m.env.projects {
		out = append(out, p.Clone())
	}
	return out, nil
}

// memoryRuntimeStore is the runtime metadata a server persists, in memory. It is
// shared by every server built over one environment, which is what lets a
// restarted server adopt its predecessor's runtimes: the metadata it reads is
// the metadata that was written.
type memoryRuntimeStore struct {
	mu      sync.Mutex
	records map[string]session.Record
}

func newMemoryRuntimeStore() *memoryRuntimeStore {
	return &memoryRuntimeStore{records: map[string]session.Record{}}
}

func (s *memoryRuntimeStore) Save(_ context.Context, rec session.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[rec.ProjectID] = rec
	return nil
}

func (s *memoryRuntimeStore) Get(_ context.Context, projectID string) (session.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[projectID]
	if !ok {
		return session.Record{}, session.ErrRecordNotFound
	}
	return rec, nil
}

func (s *memoryRuntimeStore) List(_ context.Context) ([]session.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]session.Record, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, rec)
	}
	return out, nil
}

func (s *memoryRuntimeStore) Delete(_ context.Context, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, projectID)
	return nil
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// looksLikeAVersion reports whether a string could be a version rather than a
// line of prose that happened to be printed by something else on PATH.
func looksLikeAVersion(v string) bool {
	if !strings.Contains(v, ".") {
		return false
	}
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '.', r == '-', r == '+':
		default:
			return false
		}
	}
	return true
}

func truncate(data []byte, n int) string {
	if len(data) <= n {
		return string(data)
	}
	return string(data[:n]) + "..."
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

// TestRealClaudeIsResolvedByTheProductionAdapter checks the first link in the
// chain: the adapter the server actually uses turns this machine's Claude Code
// installation into a command line the runtime can type.
func TestRealClaudeIsResolvedByTheProductionAdapter(t *testing.T) {
	requireRealClaude(t)

	launcher := claude.New(claude.Options{Logger: testLogger(t)})
	installation := launcher.Resolve(context.Background())
	if !installation.Available {
		t.Skipf("no Claude Code on this machine: %s", installation.Message)
	}
	spec, err := agentSpecs{launcher}.Spec(context.Background())
	if err != nil {
		t.Fatalf("the production adapter could not produce an agent spec: %v", err)
	}

	if spec.Type != claude.Type {
		t.Errorf("Type = %q, want %q", spec.Type, claude.Type)
	}
	if spec.Version == "" {
		t.Error("no version was reported")
	} else if !looksLikeAVersion(spec.Version) {
		t.Errorf("Version = %q, which is not a version", spec.Version)
	}
	if !filepath.IsAbs(spec.Executable) {
		t.Errorf("Executable = %q, want an absolute path: it is what a running process is matched against", spec.Executable)
	}
	if resolved := resolvePath(spec.Executable); resolved != spec.Executable {
		t.Errorf("Executable = %q, but it resolves to %q; a process's executable is the resolved path", spec.Executable, resolved)
	}
	if _, err := os.Stat(spec.Executable); err != nil {
		t.Errorf("Executable = %q cannot be stat'd: %v", spec.Executable, err)
	}
	if want := claude.Quote(spec.Executable); spec.Command != want {
		t.Errorf("Command = %q, want the quoted resolved path %q", spec.Command, want)
	}
	if strings.ContainsAny(spec.Command, "\r\n") {
		t.Errorf("Command = %q contains a line break; it is delivered as terminal input", spec.Command)
	}

	// The permission model is the CLI's own, and nothing may approve on the
	// user's behalf or override the model the CLI would otherwise choose.
	for _, forbidden := range []string{
		"--dangerously-skip-permissions",
		"--permission-mode",
		"--allowedTools",
		"--allowed-tools",
		"--model",
	} {
		if strings.Contains(spec.Command, forbidden) {
			t.Errorf("Command = %q carries %q; the CLI's own settings decide", spec.Command, forbidden)
		}
	}

	// And nothing here is a credential. The runtime is handed a program to run,
	// not a way to authenticate it: authentication belongs to the CLI's own
	// configuration, inside the runtime, where the user put it.
	rendered := strings.ToLower(strings.Join(
		[]string{spec.Type, spec.Version, spec.Command, spec.Executable}, " "))
	for _, forbidden := range []string{"sk-ant", "api_key", "apikey", "anthropic_api", "bearer ", "oauth"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("the agent spec contains %q: %s", forbidden, rendered)
		}
	}

	t.Logf("resolved Claude Code %s at %s", spec.Version, spec.Executable)
}

// ---------------------------------------------------------------------------
// The lifecycle
// ---------------------------------------------------------------------------

// TestRealClaudeRunsInTheProjectDirectory is the requirement this phase exists
// to meet: the agent runs in the project's directory, and the runtime knows
// where it is because it looked rather than because it assumed.
func TestRealClaudeRunsInTheProjectDirectory(t *testing.T) {
	requireRealClaude(t)
	env := newClaudeEnv(t)
	server := env.Server(t)
	p := env.Register(t, "claude-cwd")
	spec := server.spec(t)

	server.Start(t, p)
	status, ref := server.startRealClaude(t, p)

	// What the runtime reports.
	if !status.Running {
		t.Fatalf("the agent is not reported as running: %+v", status)
	}
	if status.PID != ref.PID {
		t.Errorf("the runtime reports pid %d; the process table has %d", status.PID, ref.PID)
	}
	if status.Executable != spec.Executable {
		t.Errorf("the status reports executable %q, want %q", status.Executable, spec.Executable)
	}
	if status.StartedAt == nil {
		t.Error("the agent's start time was not reported")
	}
	want := resolvePath(p.RuntimePath)
	if got := resolvePath(status.Dir); got != want {
		t.Errorf("the runtime reports the agent's directory as %q, want %q", got, want)
	}

	// What the kernel says, read here rather than asked of the runtime.
	if ref.Executable != spec.Executable {
		t.Errorf("the process is running %s, but the runtime resolved %s", ref.Executable, spec.Executable)
	}
	if ref.Dir != want {
		t.Errorf("Claude Code runs in %s, not in the project's directory %s", ref.Dir, want)
	}

	// It is inside this project's terminal, and it is the only one there.
	pane := server.paneProcess(t, p)
	if ref.PID == pane.PID {
		t.Errorf("the agent's process is the terminal's own leader (%d); a runtime AgentMux created runs a shell there", pane.PID)
	}
	if !descendants(pane.PID)[ref.PID] {
		t.Errorf("pid %d is not a descendant of the terminal's leader %d, so it is not running inside this project's runtime",
			ref.PID, pane.PID)
	}
	if found := outermostRunning(pane.PID, spec.Executable); len(found) != 1 || found[0].PID != ref.PID {
		t.Errorf("the outermost process running %s in the terminal is %v, want only pid %d",
			spec.Executable, found, ref.PID)
	}
	if all := runningExecutable(pane.PID, spec.Executable); len(all) > 1 {
		t.Logf("Claude Code is running as pid %d with %d further process(es) started from its own binary: %v",
			ref.PID, len(all)-1, all)
	}

	// The terminal is a terminal: it has produced output, and what it produced
	// is text. Claude Code draws a full-screen interface, so a screen with
	// nothing on it after a successful start would mean the process was running
	// with nowhere to write.
	snapshot, err := server.manager.Snapshot(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("could not read the terminal's contents: %v", err)
	}
	if len(bytes.TrimSpace(snapshot)) == 0 {
		t.Error("the terminal is empty after Claude Code started")
	}
	if !utf8.Valid(snapshot) {
		t.Errorf("the terminal's output is not valid UTF-8: %q", truncate(snapshot, 200))
	}

	if got := server.manager.ProjectStatus(p.ID); got != project.StatusRunning {
		t.Errorf("the project's runtime is %s with an agent running in it, want %s", got, project.StatusRunning)
	}

	t.Logf("Claude Code %s is running as pid %d in %s, inside the terminal led by pid %d",
		spec.Version, ref.PID, ref.Dir, pane.PID)
}

// TestRealClaudeTerminalCarriesUnicode checks the terminal Claude Code runs in
// in both directions: a multi-byte string typed into it arrives intact, and
// what the terminal hands back is text.
//
// The string is written to a file through the shell that owns the terminal and
// then read back, which compares exact bytes rather than something that looks
// right on a screen. The agent is started afterwards, in the same session, so
// the terminal that was measured is the terminal Claude Code runs in.
func TestRealClaudeTerminalCarriesUnicode(t *testing.T) {
	requireRealClaude(t)
	env := newClaudeEnv(t)
	server := env.Server(t)
	p := env.Register(t, "claude-unicode")
	spec := server.spec(t)

	server.Start(t, p)

	// No single quote and no backslash: the payload is delivered as a shell
	// word, and the subject of this test is the bytes rather than the quoting.
	const payload = "你好，世界 — ünïcødé 🚀 ✓ Ω"
	if strings.ContainsAny(payload, `'\`) {
		t.Fatal("the payload contains a character that would need quoting")
	}

	out := filepath.Join(p.RuntimePath, "unicode.txt")
	command := "printf '%s\\n' '" + payload + "' > " + filepath.Base(out)
	if err := server.manager.Input(context.Background(), p.ID, []byte(command+"\r")); err != nil {
		t.Fatalf("could not type into the terminal: %v", err)
	}

	var got []byte
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(out); err == nil {
			got = data
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got == nil {
		t.Fatalf("the terminal did not produce %s; the command typed into it was %q", out, command)
	}
	if want := payload + "\n"; string(got) != want {
		t.Errorf("the file contains %q, want %q", got, want)
	}
	if !utf8.Valid(got) {
		t.Errorf("what the terminal delivered is not valid UTF-8: %q", got)
	}

	// The same session now hosts the real Claude Code.
	status, ref := server.startRealClaude(t, p)
	if !status.Running {
		t.Fatalf("the agent is not running in the session that carried the text: %+v", status)
	}
	if want := resolvePath(p.RuntimePath); ref.Dir != want {
		t.Errorf("Claude Code runs in %s, not in %s", ref.Dir, want)
	}
	if ref.Executable != spec.Executable {
		t.Errorf("the process in that terminal is running %s, want %s", ref.Executable, spec.Executable)
	}

	t.Logf("a %d-byte UTF-8 line crossed the terminal Claude Code runs in, unchanged", len(got))
}

// TestRealClaudeIsIsolatedBetweenProjects checks the boundary this design
// exists to draw: one project's agent is in one project's terminal, and
// destroying that project's runtime does not reach the other's.
func TestRealClaudeIsIsolatedBetweenProjects(t *testing.T) {
	requireRealClaude(t)
	env := newClaudeEnv(t)
	server := env.Server(t)
	one := env.Register(t, "claude-isolation-one")
	two := env.Register(t, "claude-isolation-two")
	spec := server.spec(t)

	server.Start(t, one)
	server.Start(t, two)

	first, firstRef := server.startRealClaude(t, one)
	second, secondRef := server.startRealClaude(t, two)

	if first.PID == second.PID {
		t.Fatalf("both projects report the same agent process %d", first.PID)
	}
	if want := resolvePath(one.RuntimePath); firstRef.Dir != want {
		t.Errorf("the first agent is in %s, want %s", firstRef.Dir, want)
	}
	if want := resolvePath(two.RuntimePath); secondRef.Dir != want {
		t.Errorf("the second agent is in %s, want %s", secondRef.Dir, want)
	}
	if resolvePath(firstRef.Dir) == resolvePath(secondRef.Dir) {
		t.Fatalf("both agents are in %s", firstRef.Dir)
	}

	// Each agent is inside its own project's terminal and in no other's.
	paneOne := server.paneProcess(t, one)
	paneTwo := server.paneProcess(t, two)
	if paneOne.PID == paneTwo.PID {
		t.Fatalf("both projects share one terminal (%d)", paneOne.PID)
	}
	if descendants(paneOne.PID)[second.PID] {
		t.Errorf("the second project's agent (%d) is inside the first project's terminal (%d)", second.PID, paneOne.PID)
	}
	if descendants(paneTwo.PID)[first.PID] {
		t.Errorf("the first project's agent (%d) is inside the second project's terminal (%d)", first.PID, paneTwo.PID)
	}
	if found := outermostRunning(paneOne.PID, spec.Executable); len(found) != 1 || found[0].PID != first.PID {
		t.Errorf("the outermost process running %s in the first project's terminal is %v, want only pid %d",
			spec.Executable, found, first.PID)
	}
	if found := outermostRunning(paneTwo.PID, spec.Executable); len(found) != 1 || found[0].PID != second.PID {
		t.Errorf("the outermost process running %s in the second project's terminal is %v, want only pid %d",
			spec.Executable, found, second.PID)
	}

	// One socket each: a tmux server is identified by its socket, so this is
	// the whole of the fault isolation.
	pathOne := server.runtimes.Sockets().Path(one.ID)
	pathTwo := server.runtimes.Sockets().Path(two.ID)
	if pathOne == "" || pathTwo == "" || pathOne == pathTwo {
		t.Fatalf("the projects do not have distinct sockets: %q and %q", pathOne, pathTwo)
	}

	// Destroying one project's runtime must not reach the other's agent.
	if err := server.manager.Destroy(context.Background(), one.ID); err != nil {
		t.Fatalf("could not destroy the first project's runtime: %v", err)
	}
	if _, ok := describeProcess(second.PID); !ok {
		t.Errorf("destroying the first project's runtime ended the second project's agent (%d)", second.PID)
	}
	if status, err := server.manager.Agent(context.Background(), two.ID); err != nil {
		t.Errorf("could not read the second project's agent: %v", err)
	} else if !status.Running {
		t.Errorf("the second project's agent is reported as %s after the first project's runtime was destroyed", status.State)
	}

	t.Logf("two projects, two sockets, two agents: %d in %s and %d in %s",
		first.PID, firstRef.Dir, second.PID, secondRef.Dir)
}

// TestRealClaudeSurvivesAServerRestart checks the property the whole runtime
// exists for: the agent's parent is the shell inside tmux, not the server, so
// restarting the server does not restart the work.
func TestRealClaudeSurvivesAServerRestart(t *testing.T) {
	requireRealClaude(t)
	env := newClaudeEnv(t)
	first := env.Server(t)
	p := env.Register(t, "claude-restart")

	first.Start(t, p)
	before, ref := first.startRealClaude(t, p)
	if before.StartedAt == nil {
		t.Fatal("the agent's start time was not reported before the restart")
	}

	// A restart. Shutting the server down detaches every control client and
	// deliberately does not touch the sessions, which is why the agent is still
	// there to be found afterwards.
	if err := first.Shutdown(); err != nil {
		t.Fatalf("could not shut the first server down: %v", err)
	}
	if _, ok := describeProcess(ref.PID); !ok {
		t.Fatalf("the agent (%d) ended when the server closed; it should have outlived it", ref.PID)
	}

	second := env.Server(t)
	report, err := second.manager.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("could not reconcile after the restart: %v", err)
	}
	if !contains(report.Running, p.ID) {
		t.Errorf("the restarted server did not adopt the project's running runtime: running=%v stopped=%v orphans=%d",
			report.Running, report.Stopped, len(report.Orphans))
	}

	after, err := second.manager.Agent(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("could not read the agent after the restart: %v", err)
	}
	if !after.Running {
		t.Fatalf("the agent is reported as %s after the restart, and it never stopped", after.State)
	}
	if after.PID != before.PID {
		t.Errorf("the agent's pid changed across the restart: %d then %d", before.PID, after.PID)
	}
	if after.StartedAt == nil {
		t.Fatal("the agent's start time was lost across the restart")
	}
	// The start time is the kernel's, derived from the same start ticks both
	// times, so it survives the restart to within the resolution of the reading
	// it comes from: /proc/uptime has two decimal places, so a boot time
	// computed from it is quantised to ten milliseconds.
	if delta := after.StartedAt.Sub(*before.StartedAt); delta > 20*time.Millisecond || delta < -20*time.Millisecond {
		t.Errorf("the agent's start time moved by %s across the restart: %s then %s",
			delta, before.StartedAt.Format(time.RFC3339Nano), after.StartedAt.Format(time.RFC3339Nano))
	}
	if want := resolvePath(p.RuntimePath); resolvePath(after.Dir) != want {
		t.Errorf("after the restart the agent is reported in %q, want %q", after.Dir, want)
	}

	t.Logf("Claude Code kept running as pid %d across a server restart, with its start time intact", after.PID)
}

// TestRealClaudeReportedStateMatchesTheProcessTable checks the invariant that
// makes every other answer trustworthy: whatever the agent does, the runtime
// says so.
//
// It does not assert that the agent stays up. On a machine that cannot reach
// Anthropic's service the CLI prints an error and exits on its own, and a test
// that demanded otherwise would be asserting a property of the network. What it
// asserts is that the runtime's answer and the kernel's answer never disagree,
// and that an ending nobody asked for is reported as one.
func TestRealClaudeReportedStateMatchesTheProcessTable(t *testing.T) {
	requireRealClaude(t)
	env := newClaudeEnv(t)
	server := env.Server(t)
	p := env.Register(t, "claude-state")
	spec := server.spec(t)

	server.Start(t, p)
	if _, ref := server.startRealClaude(t, p); ref.PID == 0 {
		t.Fatal("no agent process to observe")
	}
	leader := server.paneProcess(t, p).PID

	const window = 12 * time.Second
	var last session.AgentStatus
	endedOnItsOwn := false

	for deadline := time.Now().Add(window); time.Now().Before(deadline); {
		status, err := server.manager.Agent(context.Background(), p.ID)
		if err != nil {
			t.Fatalf("could not read the agent: %v", err)
		}
		last = status

		present := len(runningExecutable(leader, spec.Executable)) > 0
		if status.Running != present {
			t.Fatalf("the runtime reports running=%v while the process table says present=%v: %+v",
				status.Running, present, status)
		}
		if !present {
			endedOnItsOwn = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	if endedOnItsOwn {
		if last.State != session.AgentExited {
			t.Errorf("the agent ended and the runtime reports %s, want %s", last.State, session.AgentExited)
		}
		if last.Requested {
			t.Error("the agent ended without being asked to, but the status records that a stop was requested")
		}
		if last.ExitedAt == nil {
			t.Error("the agent ended and no time was recorded for it")
		}
		t.Log("the CLI ended on its own during the window, and the runtime reported an unrequested exit")
	} else {
		if last.State != session.AgentRunning || !last.Running {
			t.Errorf("the agent is still in the process table but the runtime reports %s (running=%v)",
				last.State, last.Running)
		}
		t.Logf("the CLI stayed up for the whole %s window; the runtime reported RUNNING throughout and never disagreed with the process table",
			window)
	}
}

// TestRealClaudeStopIsReportedHonestly checks what a stop does and what it says.
//
// A stop is a Ctrl-C typed into the terminal, which is what a person would do
// and the only interruption AgentMux has. Claude Code holds its terminal in raw
// mode, so that Ctrl-C arrives as a keystroke rather than as a signal - which
// means the CLI decides what it means, and a CLI waiting for the user to answer
// something can decline it. Measured on 2.1.274 sitting on its first-run screen:
// it is declined. The runtime reports that instead of pretending the agent
// stopped, and this test accepts either outcome while requiring the report to
// match the process table.
//
// The CLI is given a few seconds to settle before it is interrupted. A stop
// delivered while the process is still starting is a different measurement - it
// ends the process before it has installed its terminal handling, which is how a
// stop appears to succeed for a reason that would not hold a moment later.
func TestRealClaudeStopIsReportedHonestly(t *testing.T) {
	requireRealClaude(t)
	env := newClaudeEnv(t)
	server := env.Server(t)
	p := env.Register(t, "claude-stop")
	spec := server.spec(t)

	server.Start(t, p)
	before, ref := server.startRealClaude(t, p)
	leader := server.paneProcess(t, p).PID

	const settle = 3 * time.Second
	time.Sleep(settle)

	after, err := server.manager.StopAgent(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("a stop failed rather than reporting an answer: %v", err)
	}

	present := len(runningExecutable(leader, spec.Executable)) > 0
	if after.Running != present {
		t.Fatalf("after the stop the runtime reports running=%v while the process table says present=%v: %+v",
			after.Running, present, after)
	}
	if !after.Requested {
		t.Error("a stop was asked for and the status does not record that it was")
	}

	if present {
		if after.State != session.AgentRunning {
			t.Errorf("the agent is still running but the runtime reports %s", after.State)
		}
		if after.Message == "" {
			t.Error("the agent did not take the interrupt and the status does not say so")
		}
		if after.PID != before.PID {
			t.Errorf("the reported pid changed from %d to %d while the process never ended", before.PID, after.PID)
		}
		t.Logf("the CLI did not take the interrupt after %s and is still running as %d; the runtime reported: %s",
			settle, ref.PID, after.Message)
	} else {
		if after.State != session.AgentStopped {
			t.Errorf("the agent ended when it was asked to and the runtime reports %s, want %s",
				after.State, session.AgentStopped)
		}
		t.Logf("the CLI ended when it was asked to, having been running for %s", settle)
	}

	// Either way the terminal is still there. A stop that ended the terminal
	// would take the scrollback with it, and the scrollback is the record of
	// what the agent did.
	if status := server.manager.ProjectStatus(p.ID); status != project.StatusRunning {
		t.Errorf("the project's runtime is %s after its agent was stopped, want %s", status, project.StatusRunning)
	}
	if _, err := server.manager.Snapshot(context.Background(), p.ID); err != nil {
		t.Errorf("the terminal could not be read after the stop: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The prompt, which needs a signed-in CLI
// ---------------------------------------------------------------------------

// TestRealClaudeAnswersAPrompt types a prompt at the running CLI and checks that
// it answers.
//
// It is behind the second gate because it needs something the first one does not:
// a CLI that is signed in. An unauthenticated CLI sits on its sign-in screen,
// where a prompt is not a request it can carry out, so the test would fail for a
// reason that has nothing to do with AgentMux. It also spends tokens.
//
// It asserts the input-output loop and nothing about the permission model beyond
// observing it: no approval is ever typed at a dialog, no flag is passed to skip
// one, and what the CLI does with a file-creating request is recorded rather than
// asserted, because whether a tool call needs approving is the user's
// configuration and not this test's business.
func TestRealClaudeAnswersAPrompt(t *testing.T) {
	requireRealClaude(t)
	requireRealClaudePrompt(t)
	env := newClaudeEnv(t)
	server := env.Server(t)
	p := env.Register(t, "claude-prompt")
	spec := server.spec(t)

	server.Start(t, p)
	if _, ref := server.startRealClaude(t, p); ref.PID == 0 {
		t.Fatal("no agent process to prompt")
	}

	before, err := server.manager.Snapshot(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("could not read the terminal before prompting: %v", err)
	}

	// One line, because it is delivered as terminal input: a newline in the
	// middle of it would submit half a prompt.
	const prompt = "Reply with exactly: agentmux-ready"
	if strings.ContainsAny(prompt, "\r\n") {
		t.Fatal("the prompt contains a line break")
	}
	if err := server.manager.Input(context.Background(), p.ID, []byte(prompt+"\r")); err != nil {
		t.Fatalf("could not type the prompt into the terminal: %v", err)
	}

	// The terminal is watched for growth rather than for a phrase. Looking for a
	// particular word would be reading the screen to decide what the CLI is
	// doing, which is the guess this phase forbids; that more was written after
	// the prompt than before it is a fact about bytes.
	const window = 60 * time.Second
	grew := false
	var after []byte
	for deadline := time.Now().Add(window); time.Now().Before(deadline); {
		after, err = server.manager.Snapshot(context.Background(), p.ID)
		if err != nil {
			t.Fatalf("could not read the terminal after prompting: %v", err)
		}
		if len(after) > len(before) {
			grew = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !grew {
		t.Fatalf("the terminal produced nothing in response to a prompt within %s (it held %d bytes before)",
			window, len(before))
	}

	status, err := server.manager.Agent(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("could not read the agent after prompting: %v", err)
	}
	if !status.Running {
		t.Errorf("the agent is %s after being prompted, so it could not have answered", status.State)
	}
	if status.Executable != spec.Executable {
		t.Errorf("the agent is running %q, want %q", status.Executable, spec.Executable)
	}

	// Recorded, not asserted: the CLI was asked only to reply, so whether a file
	// exists is a statement about the user's own settings rather than about
	// AgentMux. It is logged because it is the observation a maintainer wants
	// when they read this test's output.
	gate2 := filepath.Join(p.RuntimePath, "gate2.txt")
	if _, err := os.Stat(gate2); err == nil {
		t.Log("a file appeared in the project directory; this CLI is configured to run tools without asking")
	} else {
		t.Log("no file appeared in the project directory; this CLI asks before it runs tools")
	}

	t.Logf("the terminal grew from %d to %d bytes after a prompt, and the agent is still running as pid %d",
		len(before), len(after), status.PID)
}
