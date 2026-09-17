package session

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// This file holds the helpers shared by the tmux backend tests and the runtime
// manager tests.
//
// Those tests drive a real tmux rather than a fake, and that is a deliberate
// cost. The properties they check - a pty that is really 100 columns wide, bytes
// that really survive a pty, a process that really outlives the server that
// started it - are properties of tmux and of the kernel. A fake backend would
// assert that the fake behaves the way it was written, which is a statement
// about the test, not about the runtime. Where tmux is not installed the tests
// skip rather than fail, so the suite still runs on a machine that only manages
// projects.

// Timeouts. They are generous because they bound a failure, not a success: a
// passing test returns as soon as the bytes arrive.
const (
	// streamSyncTimeout bounds the wait for a control stream to start
	// delivering.
	streamSyncTimeout = 20 * time.Second

	// outputWait bounds how long a test waits for output it expects.
	outputWait = 8 * time.Second

	// paneCommandWait bounds the wait for a session's foreground process to
	// change.
	paneCommandWait = 10 * time.Second

	// readinessWait bounds the wait for a file another process is writing.
	readinessWait = 10 * time.Second

	// pollInterval is how often a waiting helper re-checks its condition.
	pollInterval = 20 * time.Millisecond

	// outputQuiet is how long a live output stream must produce nothing before
	// a test treats it as settled. Output a session is still producing is not
	// something a test can call "all of it".
	outputQuiet = 300 * time.Millisecond
)

// testSocketCounter gives every test its own tmux socket.
//
// Sharing one socket between tests would let one test's list, kill, or crash
// decide another test's outcome, which is how an integration suite acquires
// failures that move when you run it twice.
var testSocketCounter atomic.Int64

// uniqueSocketDir returns a directory no other test is using, for sockets to
// live in. It is removed when the test ends.
//
// It is a directory rather than a path because a project's runtime is addressed
// by a socket *path* since Phase 2.5, and the layout that turns a project id
// into one is what the tests have to exercise. A test that named a socket the
// old way would be testing a server it does not own.
//
// The directory is made under the system temporary directory rather than with
// t.TempDir() because a socket path is bounded - about a hundred bytes on Linux
// - and a test name descriptive enough to be worth reading is long enough to
// exceed that. A too-long path fails with "File name too long" from somewhere
// that says nothing about the test name being the cause.
func uniqueSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", fmt.Sprintf("amx-%d-", testSocketCounter.Add(1)))
	if err != nil {
		t.Fatalf("could not make a socket directory: %v", err)
	}
	// Registered here rather than by the caller so that it runs after every
	// cleanup the test registers later: t.Cleanup is last-in-first-out, and the
	// server has to be gone before its directory can be removed.
	//
	// Removing the directory is not enough on its own. Several tests deliberately
	// leave a server running - a stopped runtime keeps its session, and Close is
	// supposed to leave sessions alone - so the server outlives the test that
	// made it, and unlinking a socket file does not stop the process behind it.
	// Without the sweep below the suite leaked one tmux server per socket
	// directory, which on a machine that runs it repeatedly is dozens of
	// processes holding dozens of sockets, all of them AgentMux's and none of
	// them wanted.
	//
	// The sweep is scoped to this directory, so it can only reach servers this
	// test created.
	t.Cleanup(func() {
		killServersIn(t, dir)
		_ = os.RemoveAll(dir)
	})
	return dir
}

// killServersIn ends every tmux server holding a socket inside dir.
//
// It is deliberately narrow: it lists the directory rather than pattern-matching
// process names, so it cannot reach a server the test did not create. A test
// suite that kills tmux by name is a suite that eventually kills somebody's
// editor session.
func killServersIn(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), socketFileSuffix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		b := NewTmuxBackend(TmuxOptions{
			SocketPath: path,
			Logger:     slog.New(slog.DiscardHandler),
		})
		// Errors are ignored on purpose. The socket may already be stale, or the
		// server may already be gone, and a cleanup that fails the test it is
		// cleaning up after is worse than a cleanup that does nothing.
		_ = b.KillServer(context.Background())
	}
}

// uniqueSocketPath returns a socket path no other test is using, for a backend
// that is not part of a manager's layout.
func uniqueSocketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(uniqueSocketDir(t), "tmux.sock")
}

// testRuntimes builds the per-project runtime factory the manager will use.
//
// The tmux probe is here rather than in each caller because every caller is a
// test that starts real servers, and a few of them reach Start without ever
// building a backend of their own - the isolation suite builds its manager and
// its three projects and goes straight to Start. Without this they failed on a
// machine with no tmux on the path, which is every Windows machine, and the
// failure read as AgentMux being broken rather than as tmux being absent.
func testRuntimes(t *testing.T, dir string) *ProjectRuntimes {
	t.Helper()
	requireTmuxInstalled(t)
	runtimes, err := NewProjectRuntimes(ProjectRuntimesOptions{
		SocketDir: dir,
		Logger:    discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewProjectRuntimes returned an error: %v", err)
	}
	return runtimes
}

// runningOnWindows reports whether the suite is on Windows, for the handful of
// assertions that are about POSIX behaviour.
//
// It reads the path separator rather than importing "runtime", because this
// package has a type called runtime and Go will not have both names in one file.
func runningOnWindows() bool { return os.PathSeparator == '\\' }

// requireTmuxInstalled skips a test that needs tmux to be installed but does not
// need a server of its own - a test about how tmux words a refusal, for example,
// which is the evidence the socket states are classified from.
func requireTmuxInstalled(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the tmux integration tests in short mode")
	}
	probe := NewTmuxBackend(TmuxOptions{
		SocketPath: filepath.Join(os.TempDir(), "amx-tmux-probe.sock"),
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err := probe.Available(context.Background()); err != nil {
		t.Skipf("tmux is not usable here, so the runtime cannot be tested: %v", err)
	}
}

// requireTmux skips a test where tmux cannot run.
func requireTmux(t *testing.T, b *TmuxBackend) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the tmux integration tests in short mode")
	}
	if err := b.Available(context.Background()); err != nil {
		t.Skipf("tmux is not usable here, so the runtime cannot be tested: %v", err)
	}
}

// newTestBackend returns a backend on a socket of its own.
func newTestBackend(t *testing.T) *TmuxBackend {
	t.Helper()
	return newTestBackendOn(t, uniqueSocketPath(t))
}

// newTestBackendOn returns a backend on a given socket path, for the tests that
// need two backends to look at the same tmux server in turn.
func newTestBackendOn(t *testing.T, socketPath string) *TmuxBackend {
	t.Helper()
	// An empty socket path is a defect in the test, never a property of the
	// machine, so it must not reach requireTmux and become a skip. It did once:
	// the manager tests were passing socket paths derived from label-shaped
	// project ids, which Phase 2.5 turns into the empty string, and the whole
	// suite reported success by not running.
	if strings.TrimSpace(socketPath) == "" {
		t.Fatalf("a test asked for a backend with no socket path; the project id it used is not one AgentMux would issue")
	}
	b := NewTmuxBackend(TmuxOptions{
		SocketPath: socketPath,
		Logger:     slog.New(slog.DiscardHandler),
	})
	requireTmux(t, b)
	t.Cleanup(func() {
		// KillServer also detaches every subscription, so a test that fails
		// half-way cannot leave a control client behind holding the socket.
		_ = b.KillServer(context.Background())
	})
	return b
}

// newTestSession creates a session for a project id in a fresh directory and
// returns it with that directory.
func newTestSession(t *testing.T, b *TmuxBackend, projectID string, cols, rows int) (*Session, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := b.Create(context.Background(), SessionSpec{
		Name: project.SessionNameFor(projectID),
		Dir:  dir,
		Cols: cols,
		Rows: rows,
	})
	if err != nil {
		t.Fatalf("could not create the session for %s: %v", projectID, err)
	}
	return s, dir
}

// drain reads from a subscription until ready is satisfied, or until wait
// passes, and returns everything it read.
//
// It never fails the test on timeout: what "not seeing it" means differs by
// test, and a helper that decided would take that judgement away from the
// assertion that actually knows.
func drain(sub Subscription, wait time.Duration, ready func([]byte) bool) []byte {
	var out []byte
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if ready != nil && ready(out) {
			return out
		}
		select {
		case chunk, ok := <-sub.Output():
			if !ok {
				return out
			}
			out = append(out, chunk...)
		case <-time.After(pollInterval):
		}
	}
	return out
}

// drainFor reads until the subscription has produced want.
func drainFor(sub Subscription, want []byte, wait time.Duration) []byte {
	return drain(sub, wait, func(out []byte) bool { return bytes.Contains(out, want) })
}

// syncStream blocks until the subscription is genuinely delivering output.
//
// A control-mode client attaches asynchronously, and tmux does not replay what a
// pane produced before the attach. The first command typed after Attach can
// therefore land in a session nobody is watching yet, and a test that typed once
// and waited would report a missing byte as an output-fidelity bug. Typing until
// something comes back removes the guess: the echoed command line is itself
// proof that bytes are flowing.
func syncStream(t *testing.T, b *TmuxBackend, sub Subscription, name string) {
	t.Helper()

	ctx := context.Background()
	token := fmt.Sprintf("amx-sync-%d", testSocketCounter.Add(1))
	deadline := time.Now().Add(streamSyncTimeout)
	for time.Now().Before(deadline) {
		if !sub.Connected() {
			time.Sleep(pollInterval)
			continue
		}
		if err := b.Launch(ctx, name, "printf '"+token+"\\n'"); err != nil {
			t.Fatalf("could not type into session %q: %v", name, err)
		}
		if bytes.Contains(drainFor(sub, []byte(token), 250*time.Millisecond), []byte(token)) {
			return
		}
	}
	t.Fatalf("the control-mode stream for %q never delivered output", name)
}

// waitForPaneCommand waits until a session's foreground process is want.
//
// It is how a test knows a command has started or been interrupted, rather than
// sleeping for long enough that it usually has.
func waitForPaneCommand(t *testing.T, b *TmuxBackend, name, want string) {
	t.Helper()

	deadline := time.Now().Add(paneCommandWait)
	last := ""
	for time.Now().Before(deadline) {
		last = paneCommand(t, b, name)
		if last == want {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("the foreground process of %q is %q, want %q", name, last, want)
}

// paneCommand reads a session's foreground process name.
func paneCommand(t *testing.T, b *TmuxBackend, name string) string {
	t.Helper()
	out, err := b.run(context.Background(), "list-panes", "-t", name, "-F", "#{pane_current_command}")
	if err != nil {
		t.Fatalf("could not read the foreground process of %q: %v", name, err)
	}
	return strings.TrimSpace(firstLine(out))
}

// waitForFile waits until a file exists and its contents satisfy ready, and
// returns the contents.
//
// Reading a file the session wrote is how the input tests assert byte fidelity.
// The alternative - matching against the output stream - cannot separate a byte
// the program produced from the same byte echoed back as part of the typed
// command, and that ambiguity is exactly where a false pass would hide.
func waitForFile(t *testing.T, path string, wait time.Duration, ready func([]byte) bool) []byte {
	t.Helper()

	deadline := time.Now().Add(wait)
	var last []byte
	var lastErr error
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		switch {
		case err == nil && ready(data):
			return data
		case err == nil:
			last, lastErr = data, nil
		default:
			lastErr = err
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("the file %s never reached the expected contents (last read %d bytes, err %v)",
		path, len(last), lastErr)
	return nil
}

// shellCommand is the foreground process a freshly created session has, which is
// the shell it runs when no command is given.
func shellCommand(t *testing.T, b *TmuxBackend, name string) string {
	t.Helper()
	return paneCommand(t, b, name)
}
