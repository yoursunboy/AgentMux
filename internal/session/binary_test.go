package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kutonlagos/agentmux/internal/project"
)

// These tests are about `tmuxBinary`: the setting that says which tmux AgentMux
// runs.
//
// The property under test is not "the setting is read" but "the setting is the
// only thing that decides". A single code path that fell back to the PATH
// lookup would be invisible on a machine with one tmux - which is every machine
// until the one where it matters - so the tests use a binary that is not tmux and
// that leaves evidence of every call it receives.

// fakeTmuxVersion is what the fake binary reports for `-V`. It is not a version
// any real tmux reports, so a diagnostic that names it can only have asked the
// configured binary.
const fakeTmuxVersion = "9.9fake"

// fakeTmuxLogEnv is the variable naming the file the fake appends its arguments
// to. It is not AGENTMUX_-prefixed: it configures the test double, not AgentMux.
const fakeTmuxLogEnv = "AMX_TEST_FAKE_TMUX_LOG"

// fakeTmux writes an executable named tmux that records its arguments and then
// runs the real tmux, and returns its path.
//
// Both halves are needed. The recording is how a test sees which binary ran when
// both candidates would produce the same effect; handing the call to the real
// tmux is what lets one test drive a whole lifecycle through it and still assert
// on real sessions.
func fakeTmux(t *testing.T, dir string) string {
	t.Helper()
	if runningOnWindows() {
		t.Skip("a fake tmux is a shell script, and the tmux suite does not run on Windows")
	}
	real, err := exec.LookPath(DefaultTmuxBinary)
	if err != nil {
		t.Skipf("no tmux is installed to stand behind the fake one: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("could not make the fake's directory: %v", err)
	}

	path := filepath.Join(dir, "tmux")
	script := strings.Join([]string{
		"#!/bin/sh",
		`printf '%s\n' "$*" >> "$` + fakeTmuxLogEnv + `"`,
		`if [ "$1" = "-V" ]; then echo "tmux ` + fakeTmuxVersion + `"; exit 0; fi`,
		`exec "` + real + `" "$@"`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("could not write the fake tmux: %v", err)
	}
	return path
}

// fakeTmuxCalls reads back the arguments every invocation of the fake received.
func fakeTmuxCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("the fake tmux was never run: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

// TestTheConfiguredBinaryDrivesEveryRuntimePath is the requirement stated as a
// measurement.
//
// One lifecycle through the manager, then the log of what tmux was actually
// asked. Every shape of command the runtime issues has to appear - version,
// create, type, resize, read, list, attach, destroy - because the failure this
// guards against is one path among many quietly using a different binary, and a
// test that checked one path would not find it.
func TestTheConfiguredBinaryDrivesEveryRuntimePath(t *testing.T) {
	requireTmuxInstalled(t)

	ctx := context.Background()
	p := testProject("binary")
	p.RuntimePath = t.TempDir()

	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv(fakeTmuxLogEnv, logPath)

	fake := fakeTmux(t, t.TempDir())
	runtimes, err := NewProjectRuntimes(ProjectRuntimesOptions{
		Binary:    fake,
		SocketDir: uniqueSocketDir(t),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewProjectRuntimes returned an error: %v", err)
	}
	socketPath := runtimes.Sockets().Path(p.ID)

	// Diagnostics first: the version they report has to be the configured
	// binary's, or a compatibility matrix built from the output would be about
	// some other tmux.
	status := runtimes.Status(ctx)
	if !status.Available {
		t.Fatalf("the runtime reports tmux as unavailable: %s", status.Error)
	}
	if status.Version != fakeTmuxVersion {
		t.Errorf("the reported version is %q, want %q from the configured binary", status.Version, fakeTmuxVersion)
	}
	if status.Binary != fake {
		t.Errorf("the reported binary is %q, want %q", status.Binary, fake)
	}
	if status.SocketDir != runtimes.Sockets().Dir() {
		t.Errorf("the reported socket directory is %q, want %q", status.SocketDir, runtimes.Sockets().Dir())
	}

	m, err := NewManager(ManagerOptions{
		Backends: runtimes,
		Sockets:  runtimes.Sockets(),
		Projects: newFakeProjects(p),
		Store:    newFakeStore(),
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if err := m.Launch(ctx, p.ID, "echo amx-binary"); err != nil {
		t.Fatalf("Launch returned an error: %v", err)
	}
	if _, err := m.Resize(ctx, p.ID, 100, 30); err != nil {
		t.Fatalf("Resize returned an error: %v", err)
	}
	if _, err := m.Snapshot(ctx, p.ID); err != nil {
		t.Fatalf("Snapshot returned an error: %v", err)
	}
	if _, err := m.Sessions(ctx); err != nil {
		t.Fatalf("Sessions returned an error: %v", err)
	}
	if _, err := m.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if _, err := m.Stop(ctx, p.ID); err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}
	if err := m.Destroy(ctx, p.ID); err != nil {
		t.Fatalf("Destroy returned an error: %v", err)
	}

	calls := fakeTmuxCalls(t, logPath)
	for _, want := range []struct{ what, arg string }{
		{"the version probe", "-V"},
		{"creating the session", "new-session"},
		{"typing into the session", "send-keys"},
		{"resizing the window", "resize-window"},
		{"reading the screen", "capture-pane"},
		{"listing what is on a socket", "list-panes"},
		{"the control-mode attach", "attach-session"},
		{"the control-mode flag", "-C"},
		{"ending the session", "kill-session"},
	} {
		if !anyCallHas(calls, want.arg) {
			t.Errorf("nothing the runtime did reached the configured binary as %s (%q); "+
				"a path may be using the tmux on PATH instead", want.what, want.arg)
		}
	}

	// kill-server is deliberately not in that list. Destroying a project ends its
	// session, and tmux's own exit-empty then takes the server with it, so the
	// socket is already stale by the time the manager looks and there is nothing
	// left to stop. It is covered by TestTheConfiguredBinaryStopsAServer, which
	// can reach the case directly instead of hoping for it.

	// And every command that talks to a server named the project's own socket.
	// A path that used the right binary and the wrong socket would be the same
	// class of bug with the opposite symptom.
	for _, call := range calls {
		if call == "-V" {
			continue
		}
		if !strings.Contains(call, "-S "+socketPath) {
			t.Errorf("tmux was run without this project's socket: %q", call)
		}
	}
}

// TestTheConfiguredBinaryStopsAServer covers the one command the lifecycle test
// cannot reliably reach.
//
// Destroy ends a project's session, and tmux's exit-empty then takes the server
// with it - so by the time the manager checks whether the server is still up,
// it usually is not, and kill-server is never issued. The case it exists for is
// the other one: a server still running with nothing on it.
func TestTheConfiguredBinaryStopsAServer(t *testing.T) {
	requireTmuxInstalled(t)

	ctx := context.Background()
	p := testProject("killserver")
	p.RuntimePath = t.TempDir()

	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv(fakeTmuxLogEnv, logPath)

	runtimes, err := NewProjectRuntimes(ProjectRuntimesOptions{
		Binary:    fakeTmux(t, t.TempDir()),
		SocketDir: uniqueSocketDir(t),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewProjectRuntimes returned an error: %v", err)
	}

	backend, err := runtimes.Backend(p.ID)
	if err != nil {
		t.Fatalf("Backend returned an error: %v", err)
	}
	newTestSession(t, backend.(*TmuxBackend), p.ID, 80, 24)

	if err := backend.KillServer(ctx); err != nil {
		t.Fatalf("KillServer returned an error: %v", err)
	}
	calls := fakeTmuxCalls(t, logPath)
	if !anyCallHas(calls, "kill-server") {
		t.Error("stopping the server did not reach the configured binary")
	}
	for _, call := range calls {
		if call == "-V" {
			continue
		}
		if !strings.Contains(call, "-S "+runtimes.Sockets().Path(p.ID)) {
			t.Errorf("tmux was run without this project's socket: %q", call)
		}
	}
}

// TestTheConfiguredBinarySurvivesASpaceInItsPath is the quoting check.
//
// A configured path with a space in it - `C:\Program Files\...`, or a user's
// "My Tools" directory - must reach tmux as one argument. Nothing in this pack-
// age builds a command line as a string, and this is the test that says so.
func TestTheConfiguredBinarySurvivesASpaceInItsPath(t *testing.T) {
	requireTmuxInstalled(t)

	ctx := context.Background()
	p := testProject("spaced")
	p.RuntimePath = t.TempDir()

	logPath := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv(fakeTmuxLogEnv, logPath)

	fake := fakeTmux(t, filepath.Join(t.TempDir(), "with space"))
	runtimes, err := NewProjectRuntimes(ProjectRuntimesOptions{
		Binary:    fake,
		SocketDir: uniqueSocketDir(t),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewProjectRuntimes returned an error: %v", err)
	}

	status := runtimes.Status(ctx)
	if !status.Available {
		t.Fatalf("a tmux whose path contains a space is reported unavailable: %s", status.Error)
	}
	if status.Version != fakeTmuxVersion {
		t.Errorf("the reported version is %q, want %q", status.Version, fakeTmuxVersion)
	}

	backend, err := runtimes.Backend(p.ID)
	if err != nil {
		t.Fatalf("Backend returned an error: %v", err)
	}
	if _, err := backend.Create(ctx, SessionSpec{
		Name: project.SessionNameFor(p.ID), Dir: p.RuntimePath, Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatalf("creating a session through a binary with a space in its path failed: %v", err)
	}
	if calls := fakeTmuxCalls(t, logPath); !anyCallHas(calls, "new-session") {
		t.Error("the session was created without the configured binary being run")
	}
}

// TestAnUnusableBinaryIsReportedNotWorkedAround covers a configured path that is
// not a tmux.
//
// The failure has to arrive as a refusal. Falling back to the tmux on PATH would
// mean a user who pointed AgentMux at the wrong binary gets sessions on a server
// they did not choose, created by a version the diagnostics do not report.
func TestAnUnusableBinaryIsReportedNotWorkedAround(t *testing.T) {
	ctx := context.Background()
	p := testProject("nobin")
	p.RuntimePath = t.TempDir()

	missing := filepath.Join(t.TempDir(), "not-here", "tmux")
	runtimes, err := NewProjectRuntimes(ProjectRuntimesOptions{
		Binary:    missing,
		SocketDir: uniqueSocketDir(t),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewProjectRuntimes returned an error: %v", err)
	}

	status := runtimes.Status(ctx)
	if status.Available {
		t.Error("an installation whose binary does not exist is reported available")
	}
	if status.Error == "" {
		t.Error("the status does not say why the binary is unusable")
	}
	if status.Binary != missing {
		t.Errorf("the status names the binary %q, want the configured %q", status.Binary, missing)
	}

	backend, err := runtimes.Backend(p.ID)
	if err != nil {
		t.Fatalf("Backend returned an error: %v", err)
	}
	if err := backend.Available(ctx); !IsCode(err, CodeBackendUnavailable) {
		t.Errorf("Available returned %v, want %s", err, CodeBackendUnavailable)
	}
	if _, err := backend.Create(ctx, SessionSpec{
		Name: project.SessionNameFor(p.ID), Dir: p.RuntimePath, Cols: 80, Rows: 24,
	}); err == nil {
		t.Error("a session was created through a binary that does not exist")
	}
}

// TestABinaryThatIsNotTmuxIsRejected checks the other half: a file that exists
// and runs, but is not tmux. It must be reported, not used.
func TestABinaryThatIsNotTmuxIsRejected(t *testing.T) {
	if runningOnWindows() {
		t.Skip("the stand-in binary is a shell script")
	}

	dir := t.TempDir()
	impostor := filepath.Join(dir, "tmux")
	if err := os.WriteFile(impostor, []byte("#!/bin/sh\necho 'I am not tmux' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("could not write the stand-in binary: %v", err)
	}

	runtimes, err := NewProjectRuntimes(ProjectRuntimesOptions{
		Binary:    impostor,
		SocketDir: uniqueSocketDir(t),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewProjectRuntimes returned an error: %v", err)
	}

	status := runtimes.Status(context.Background())
	if status.Available {
		t.Error("a binary that fails every command is reported available")
	}
	if status.Version != "" {
		t.Errorf("the status reports the version %q from a binary that has none", status.Version)
	}
	if status.Error == "" {
		t.Error("the status does not say why the binary is unusable")
	}
}

// TestTheDefaultBinaryIsABareName keeps the default a PATH lookup.
//
// An absolute default would be wrong on every machine but the one it was written
// on, and the setting exists precisely because the right path differs per
// machine.
func TestTheDefaultBinaryIsABareName(t *testing.T) {
	if DefaultTmuxBinary != "tmux" {
		t.Errorf("the default tmux binary is %q, want a bare name resolved on PATH", DefaultTmuxBinary)
	}

	runtimes, err := NewProjectRuntimes(ProjectRuntimesOptions{
		SocketDir: uniqueSocketDir(t),
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewProjectRuntimes returned an error: %v", err)
	}
	if got := runtimes.install.bin; got != DefaultTmuxBinary {
		t.Errorf("an unset binary resolved to %q, want %q", got, DefaultTmuxBinary)
	}
}

// anyCallHas reports whether any recorded invocation contains want.
func anyCallHas(calls []string, want string) bool {
	for _, call := range calls {
		if strings.Contains(call, want) {
			return true
		}
	}
	return false
}
