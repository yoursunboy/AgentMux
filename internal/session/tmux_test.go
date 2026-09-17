package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// These tests exercise the SessionBackend on a real tmux. Section names in the
// comments point at the requirement each one answers, because a test that
// cannot be traced back to a requirement is a test nobody dares delete.

// TestTmuxBackendReportsTheRuntimeVersion checks the availability probe wakes up
// and answers, rather than only that it compiles.
func TestTmuxBackendReportsTheRuntimeVersion(t *testing.T) {
	b := newTestBackend(t)

	if err := b.Available(context.Background()); err != nil {
		t.Fatalf("Available returned an error on a host where tmux runs: %v", err)
	}
	if b.Name() != "tmux" {
		t.Errorf("Name() = %q, want %q", b.Name(), "tmux")
	}
}

// TestTmuxBackendCreateInspectExists covers the create/exists/inspect triple and
// the geometry the session is created with.
func TestTmuxBackendCreateInspectExists(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, dir := newTestSession(t, b, "p_create", 80, 24)

	if session.Name != "amx-p_create" {
		t.Errorf("the session is named %q, want %q", session.Name, "amx-p_create")
	}
	if session.Dir != dir {
		t.Errorf("the session runs in %q, want %q", session.Dir, dir)
	}
	if session.Cols != 80 || session.Rows != 24 {
		t.Errorf("the session is %dx%d, want 80x24", session.Cols, session.Rows)
	}

	alive, err := b.Exists(ctx, session.Name)
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if !alive {
		t.Error("Exists says a session that was just created does not exist")
	}

	gone, err := b.Exists(ctx, "amx-p_never_created")
	if err != nil {
		t.Fatalf("Exists returned an error for an unknown name: %v", err)
	}
	if gone {
		t.Error("Exists says a session that was never created exists")
	}

	inspected, err := b.Inspect(ctx, session.Name)
	if err != nil {
		t.Fatalf("Inspect returned an error: %v", err)
	}
	if inspected.Dir != dir {
		t.Errorf("Inspect reports the directory as %q, want %q", inspected.Dir, dir)
	}
}

// TestTmuxBackendCreateRejectsADuplicateName is the property that stops a second
// start from adopting somebody else's terminal under the same name.
func TestTmuxBackendCreateRejectsADuplicateName(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	_, dir := newTestSession(t, b, "p_dup", 80, 24)

	_, err := b.Create(ctx, SessionSpec{Name: "amx-p_dup", Dir: dir, Cols: 80, Rows: 24})
	if err == nil {
		t.Fatal("creating a session under a name that is taken succeeded")
	}
	if !errors.Is(err, ErrSessionExists) {
		t.Errorf("the duplicate create failed with %v, want %v", err, ErrSessionExists)
	}
}

// TestTmuxBackendRefusesSessionsOutsideItsNamespace is what keeps AgentMux from
// interrupting, resizing, or destroying a session that is not its own.
func TestTmuxBackendRefusesSessionsOutsideItsNamespace(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	dir := t.TempDir()

	const foreign = "somebody-elses-session"

	if _, err := b.Create(ctx, SessionSpec{Name: foreign, Dir: dir, Cols: 80, Rows: 24}); !IsCode(err, CodeInvalidInput) {
		t.Errorf("Create on a foreign name failed with %v, want %s", err, CodeInvalidInput)
	}
	if err := b.Resize(ctx, foreign, 80, 24); !IsCode(err, CodeInvalidInput) {
		t.Errorf("Resize on a foreign name failed with %v, want %s", err, CodeInvalidInput)
	}
	if err := b.Stop(ctx, foreign); !IsCode(err, CodeInvalidInput) {
		t.Errorf("Stop on a foreign name failed with %v, want %s", err, CodeInvalidInput)
	}
	if err := b.Destroy(ctx, foreign); !IsCode(err, CodeInvalidInput) {
		t.Errorf("Destroy on a foreign name failed with %v, want %s", err, CodeInvalidInput)
	}
	if err := b.SendInput(ctx, foreign, []byte("x")); !IsCode(err, CodeInvalidInput) {
		t.Errorf("SendInput on a foreign name failed with %v, want %s", err, CodeInvalidInput)
	}
}

// TestTmuxBackendMissingSessionErrors checks that every call which needs a
// session says so in the same words, so a caller can tell "not there" from
// "broken".
func TestTmuxBackendMissingSessionErrors(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	const absent = "amx-p_absent"

	if _, err := b.Inspect(ctx, absent); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("Inspect on a missing session failed with %v, want %v", err, ErrNoSuchSession)
	}
	if err := b.Resize(ctx, absent, 80, 24); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("Resize on a missing session failed with %v, want %v", err, ErrNoSuchSession)
	}
	if err := b.Stop(ctx, absent); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("Stop on a missing session failed with %v, want %v", err, ErrNoSuchSession)
	}
	if err := b.SendInput(ctx, absent, []byte("x")); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("SendInput on a missing session failed with %v, want %v", err, ErrNoSuchSession)
	}
	if _, err := b.Attach(ctx, absent); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("Attach on a missing session failed with %v, want %v", err, ErrNoSuchSession)
	}
	if _, err := b.Snapshot(ctx, absent); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("Snapshot on a missing session failed with %v, want %v", err, ErrNoSuchSession)
	}
}

// TestTmuxBackendMissingSessionErrorsWithALiveServer is the second half of the
// test above, and the half that was missing.
//
// A socket with no server at all is the easy case: tmux cannot connect, and one
// message covers every command. A live server holding somebody else's session
// is the case that matters, because it is the normal state of a running
// AgentMux - and there tmux words its complaint by target kind: list-panes says
// "can't find window", capture-pane and send-keys say "can't find pane". A
// classifier that matched only "can't find session" turned every one of those
// into a backend failure, which is how creating a second session while a tmux
// server was already up failed with a 500 instead of starting.
func TestTmuxBackendMissingSessionErrorsWithALiveServer(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	// Somebody else's session, so the server is running and has state.
	newTestSession(t, b, "p_occupant", 80, 24)

	const absent = "amx-p_absent"

	if _, err := b.Inspect(ctx, absent); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("Inspect on a missing session with a live server failed with %v, want %v", err, ErrNoSuchSession)
	}
	if err := b.Resize(ctx, absent, 80, 24); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("Resize on a missing session with a live server failed with %v, want %v", err, ErrNoSuchSession)
	}
	if err := b.Stop(ctx, absent); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("Stop on a missing session with a live server failed with %v, want %v", err, ErrNoSuchSession)
	}
	if err := b.SendInput(ctx, absent, []byte("x")); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("SendInput on a missing session with a live server failed with %v, want %v", err, ErrNoSuchSession)
	}
	if _, err := b.Attach(ctx, absent); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("Attach on a missing session with a live server failed with %v, want %v", err, ErrNoSuchSession)
	}
	if _, err := b.Snapshot(ctx, absent); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("Snapshot on a missing session with a live server failed with %v, want %v", err, ErrNoSuchSession)
	}

	// Destroying what is already gone is the desired end state, not a failure -
	// and it too has to tell "gone" from "broken" with a live server present.
	if err := b.Destroy(ctx, absent); err != nil {
		t.Errorf("Destroy on a missing session with a live server failed with %v, want nil", err)
	}

	// Creating a second session while a server is already running is the case
	// that was broken end to end.
	second, err := b.Create(ctx, SessionSpec{Name: "amx-p_second", Dir: t.TempDir(), Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("creating a second session on a running tmux server failed: %v", err)
	}
	if second.Name != "amx-p_second" {
		t.Errorf("the second session is named %q", second.Name)
	}
}

// TestTmuxBackendListOnAServerThatHasNeverRun is a regression test.
//
// The first version of List asked only about "no server running", which is what
// tmux says when a server has exited. A socket that never had a server at all
// produces a different sentence - "error connecting to <socket> (No such file or
// directory)" - so a fresh installation reported a failure where the honest
// answer was "no sessions". This asserts the honest answer.
func TestTmuxBackendListOnAServerThatHasNeverRun(t *testing.T) {
	b := newTestBackend(t)

	sessions, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("List on a socket with no server returned an error: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("List on a socket with no server returned %d sessions, want none", len(sessions))
	}
}

// TestTmuxBackendListShowsOnlyItsOwnNamespace checks both halves of List: that
// it finds AgentMux's sessions, and that it does not claim anybody else's.
func TestTmuxBackendListShowsOnlyItsOwnNamespace(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	dir := t.TempDir()

	newTestSession(t, b, "p_a", 80, 24)
	newTestSession(t, b, "p_b", 80, 24)

	// A session on the same server that AgentMux did not create.
	if _, err := b.run(ctx, "new-session", "-d", "-s", "somebody-elses", "-c", dir); err != nil {
		t.Fatalf("could not create the foreign session: %v", err)
	}

	sessions, err := b.List(ctx)
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}

	names := make([]string, 0, len(sessions))
	for _, s := range sessions {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "amx-p_a,amx-p_b" {
		t.Errorf("List returned [%s], want [amx-p_a,amx-p_b]", got)
	}

	// The foreign session really is there; it is List's filter that excludes it.
	if exists := b.sessionExists(ctx, "somebody-elses"); !exists {
		t.Fatal("the foreign session was not created, so this test proves nothing")
	}
}

// TestTmuxBackendDestroyRemovesTheSession checks that Destroy ends the session
// and that destroying what is already gone is a success rather than an error.
func TestTmuxBackendDestroyRemovesTheSession(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, _ := newTestSession(t, b, "p_destroy", 80, 24)

	if err := b.Destroy(ctx, session.Name); err != nil {
		t.Fatalf("Destroy returned an error: %v", err)
	}
	alive, err := b.Exists(ctx, session.Name)
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if alive {
		t.Error("the session still exists after Destroy")
	}

	// Idempotent: the caller asked for it to be gone, and it is gone.
	if err := b.Destroy(ctx, session.Name); err != nil {
		t.Errorf("Destroying a session that is already gone failed with %v", err)
	}
}

// TestTmuxBackendStopInterruptsAndKeepsTheSession pins the difference between
// Stop and Destroy. Stop ends the work; it does not end the terminal. If this
// ever changes, every "the scrollback is still there" promise changes with it.
func TestTmuxBackendStopInterruptsAndKeepsTheSession(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, _ := newTestSession(t, b, "p_stop", 100, 30)
	shell := shellCommand(t, b, session.Name)

	if err := b.Launch(ctx, session.Name, "sleep 300"); err != nil {
		t.Fatalf("could not start a long-running command: %v", err)
	}
	waitForPaneCommand(t, b, session.Name, "sleep")

	if err := b.Stop(ctx, session.Name); err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}
	waitForPaneCommand(t, b, session.Name, shell)

	alive, err := b.Exists(ctx, session.Name)
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if !alive {
		t.Fatal("Stop destroyed the session; it must only interrupt what is running in it")
	}

	// The shell is still usable, which is what "the terminal survived" means.
	if err := b.Launch(ctx, session.Name, "echo amx-alive-after-stop"); err != nil {
		t.Fatalf("the session did not accept input after Stop: %v", err)
	}
	snapshot := screenRows(t, b, session.Name)
	if !bytes.Contains(snapshot, []byte("amx-alive-after-stop")) {
		t.Errorf("the shell did not run a command after Stop; the pane holds:\n%s", snapshot)
	}
}

// TestTmuxBackendSessionSurvivesTheBackendThatMadeIt is the persistence
// requirement, and it is the whole reason tmux was chosen.
//
// It closes the backend - the thing that owns the control clients - and then
// looks at the session through a different backend on the same socket, checking
// that the work is still running rather than only that a name is still listed. A
// file the session keeps appending to is the evidence: a session that exists but
// whose process had died would satisfy every other check here.
func TestTmuxBackendSessionSurvivesTheBackendThatMadeIt(t *testing.T) {
	ctx := context.Background()
	socket := uniqueSocketPath(t)

	first := newTestBackendOn(t, socket)
	session, dir := newTestSession(t, first, "p_persist", 100, 30)

	ticks := filepath.Join(dir, "ticks")
	if err := first.Launch(ctx, session.Name,
		"for i in $(seq 1 400); do echo tick >> "+ticks+"; sleep 0.05; done"); err != nil {
		t.Fatalf("could not start the long-running task: %v", err)
	}

	// Wait for the task to be visibly under way before the backend goes away,
	// so that "it kept going" means something.
	before := countLines(waitForFile(t, ticks, readinessWait, func(d []byte) bool {
		return bytes.Count(d, []byte("tick")) >= 3
	}))

	// The server process running AgentMux goes away. The work must not.
	if err := first.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}

	second := newTestBackendOn(t, socket)
	alive, err := second.Exists(ctx, session.Name)
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if !alive {
		t.Fatal("the session did not survive the server that created it")
	}

	rediscovered, err := second.Inspect(ctx, session.Name)
	if err != nil {
		t.Fatalf("the surviving session could not be inspected: %v", err)
	}
	if rediscovered.Dir != dir {
		t.Errorf("the surviving session runs in %q, want %q", rediscovered.Dir, dir)
	}

	// The process, not just the session, is still alive.
	after := countLines(waitForFile(t, ticks, readinessWait, func(d []byte) bool {
		return countLines(d) > before
	}))
	if after <= before {
		t.Errorf("the task stopped producing when the backend closed (%d lines, was %d)", after, before)
	}

	// And the rediscovered session accepts input.
	if err := second.Launch(ctx, session.Name, "echo amx-resumed"); err != nil {
		t.Fatalf("the rediscovered session did not accept input: %v", err)
	}
}

// countLines counts the lines in data.
func countLines(data []byte) int {
	return bytes.Count(data, []byte("\n"))
}

// TestTmuxBackendSessionRunsInTheDirectoryItWasGiven is the working-directory
// requirement. The directory is asserted by a program inside the session, not
// by the API that was asked to use it.
func TestTmuxBackendSessionRunsInTheDirectoryItWasGiven(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, dir := newTestSession(t, b, "p_cwd", 100, 30)

	// t.TempDir may hand back a path with a symlink in it, and the shell reports
	// the resolved one. Comparing resolved paths compares what was asked for
	// with what happened instead of with how the temporary directory is spelled.
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("could not resolve the test directory: %v", err)
	}

	report := filepath.Join(dir, "pwd.txt")
	if err := b.Launch(ctx, session.Name, "pwd > "+report); err != nil {
		t.Fatalf("could not ask the session for its directory: %v", err)
	}

	got := strings.TrimSpace(string(waitForFile(t, report, readinessWait, func(d []byte) bool {
		return len(bytes.TrimSpace(d)) > 0
	})))
	if got != want {
		t.Errorf("the session runs in %q, want %q", got, want)
	}
}

// sizePattern matches the terminal size a shell reports, and cannot match the
// command that asked for it: the echoed command contains "%s", not digits.
var sizePattern = regexp.MustCompile(`AMXSIZE:(\d+):(\d+)`)

// readTerminalSize asks the session itself how wide it is.
//
// stty and tput answer from the pty, so the answer is the size the process
// really has rather than a number AgentMux remembers asking for. Anything that
// only recorded the request would pass a test written against stored state and
// fail the first time a program drew itself.
func readTerminalSize(t *testing.T, b *TmuxBackend, sub Subscription, name string) (int, int) {
	t.Helper()

	if err := b.Launch(context.Background(), name,
		`printf 'AMXSIZE:%s:%s\n' "$(tput cols)" "$(tput lines)"`); err != nil {
		t.Fatalf("could not ask for the terminal size: %v", err)
	}
	out := drain(sub, outputWait, func(d []byte) bool { return sizePattern.Match(d) })

	matches := sizePattern.FindAllSubmatch(out, -1)
	if len(matches) == 0 {
		t.Fatalf("the session never reported its size; it produced:\n%q", out)
	}
	last := matches[len(matches)-1]
	cols, _ := strconv.Atoi(string(last[1]))
	rows, _ := strconv.Atoi(string(last[2]))
	return cols, rows
}

// TestTmuxBackendResizeChangesTheTerminal is the resize requirement, measured
// inside the session.
func TestTmuxBackendResizeChangesTheTerminal(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, _ := newTestSession(t, b, "p_resize", 80, 24)

	sub, err := b.Attach(ctx, session.Name)
	if err != nil {
		t.Fatalf("Attach returned an error: %v", err)
	}
	defer sub.Close()
	syncStream(t, b, sub, session.Name)

	if cols, rows := readTerminalSize(t, b, sub, session.Name); cols != 80 || rows != 24 {
		t.Fatalf("the session started %dx%d, want 80x24", cols, rows)
	}

	if err := b.Resize(ctx, session.Name, 100, 30); err != nil {
		t.Fatalf("Resize returned an error: %v", err)
	}
	if cols, rows := readTerminalSize(t, b, sub, session.Name); cols != 100 || rows != 30 {
		t.Errorf("after Resize the session is %dx%d, want 100x30", cols, rows)
	}

	// The geometry tmux reports must agree with the geometry the program sees,
	// or a client would draw to a different size than the program assumed.
	inspected, err := b.Inspect(ctx, session.Name)
	if err != nil {
		t.Fatalf("Inspect returned an error: %v", err)
	}
	if inspected.Cols != 100 || inspected.Rows != 30 {
		t.Errorf("tmux reports %dx%d, want 100x30", inspected.Cols, inspected.Rows)
	}
}

// TestTmuxBackendResizeRejectsAnUnusableSize checks the guard rather than
// trusting the API layer to be the only caller.
func TestTmuxBackendResizeRejectsAnUnusableSize(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, _ := newTestSession(t, b, "p_badsize", 80, 24)

	for _, size := range [][2]int{{0, 24}, {80, 0}, {-1, 24}, {80, -1}} {
		err := b.Resize(ctx, session.Name, size[0], size[1])
		if !IsCode(err, CodeInvalidSize) {
			t.Errorf("Resize to %dx%d failed with %v, want %s", size[0], size[1], err, CodeInvalidSize)
		}
	}
}

// bytePayload is the input the transparency test delivers.
//
// Every byte in it is one a terminal's line discipline passes through
// untouched. The exceptions are not oversights and they are not hidden: see
// TestTmuxBackendLineDisciplineInterpretsErase, and the note in
// docs/RUNTIME.md, for the small set of bytes a pty is specified to act on -
// which is the same mechanism that makes Ctrl-C work at all.
var bytePayload = append(
	[]byte("raw-\x01\x02-\x1b[A\x1b[31m-\x09-\xe4\xb8\xad\xe6\x96\x87-\x80\xc3\xa9\xfe\xff\n"),
	[]byte("second-\x1b[0m-\x7e\n")...,
)

// TestTmuxBackendInputIsByteTransparent is the input-fidelity requirement.
//
// The bytes are compared against what a program inside the session read, so the
// assertion covers the whole path - the API, tmux's send-keys, the pty, and the
// shell's line editor - and it cannot be satisfied by bytes that only appear
// because the terminal echoed the typed command back.
func TestTmuxBackendInputIsByteTransparent(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, dir := newTestSession(t, b, "p_input", 120, 30)

	captured := filepath.Join(dir, "captured")
	if err := b.Launch(ctx, session.Name, "cat > "+captured); err != nil {
		t.Fatalf("could not start the reader: %v", err)
	}
	waitForPaneCommand(t, b, session.Name, "cat")

	if err := b.SendInput(ctx, session.Name, bytePayload); err != nil {
		t.Fatalf("SendInput returned an error: %v", err)
	}
	// End of file, so the reader finishes and the file is complete before it is
	// compared. Waiting for a length would be guessing; EOT is the answer.
	if err := b.SendInput(ctx, session.Name, []byte{0x04}); err != nil {
		t.Fatalf("could not end the input: %v", err)
	}

	got := waitForFile(t, captured, readinessWait, func(d []byte) bool { return len(d) >= len(bytePayload) })
	if !bytes.Equal(got, bytePayload) {
		t.Errorf("the session read %d bytes, want %d\n got: %q\nwant: %q",
			len(got), len(bytePayload), got, bytePayload)
	}
}

// TestTmuxBackendInputDeliversControlCharacters covers the control input the
// terminal is expected to obey. Ctrl-C is the one that matters most: it is how a
// user stops an agent that will not stop by itself.
func TestTmuxBackendInputDeliversControlCharacters(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, _ := newTestSession(t, b, "p_control", 120, 30)
	shell := shellCommand(t, b, session.Name)

	if err := b.Launch(ctx, session.Name, "sleep 300"); err != nil {
		t.Fatalf("could not start a long-running command: %v", err)
	}
	waitForPaneCommand(t, b, session.Name, "sleep")

	// 0x03 is Ctrl-C.
	if err := b.SendInput(ctx, session.Name, []byte{0x03}); err != nil {
		t.Fatalf("SendInput returned an error: %v", err)
	}
	waitForPaneCommand(t, b, session.Name, shell)

	alive, err := b.Exists(ctx, session.Name)
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if !alive {
		t.Fatal("Ctrl-C destroyed the session; it must only interrupt the command")
	}

	// Enter is 0x0d, and it has to submit a line rather than be inserted.
	if err := b.SendInput(ctx, session.Name, []byte("echo amx-enter-works\r")); err != nil {
		t.Fatalf("SendInput returned an error: %v", err)
	}
	snapshot := screenRows(t, b, session.Name)
	if !bytes.Contains(snapshot, []byte("amx-enter-works")) {
		t.Errorf("Enter did not submit the line; the pane holds:\n%s", snapshot)
	}
}

// TestTmuxBackendInputDeliversEscapeSequences checks the arrow keys, which arrive
// as escape sequences and must reach the shell intact rather than as literal
// bracket characters.
//
// The check is behavioural: in bash, up-arrow recalls the previous command from
// the history, so a second appearance of a marker that was only typed once is
// the sequence having been understood.
func TestTmuxBackendInputDeliversEscapeSequences(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	dir := t.TempDir()
	name := project.SessionNameFor("p_arrows")
	if _, err := b.Create(ctx, SessionSpec{Name: name, Dir: dir, Cols: 120, Rows: 30, Command: []string{"bash"}}); err != nil {
		t.Fatalf("could not create the session: %v", err)
	}
	waitForPaneCommand(t, b, name, "bash")

	const marker = "amx-history-marker"
	if err := b.Launch(ctx, name, "echo "+marker); err != nil {
		t.Fatalf("could not run the marker command: %v", err)
	}

	// Up arrow, then Enter: the shell should run the previous command again.
	if err := b.SendInput(ctx, name, []byte("\x1b[A\r")); err != nil {
		t.Fatalf("could not send the up-arrow sequence: %v", err)
	}

	deadline := time.Now().Add(outputWait)
	for time.Now().Before(deadline) {
		snapshot := screenRows(t, b, name)
		if bytes.Count(snapshot, []byte(marker)) >= 3 {
			// Once in the recalled command line, once in each of the two runs.
			return
		}
		time.Sleep(pollInterval)
	}
	snapshot, _ := b.Snapshot(ctx, name)
	t.Errorf("up-arrow did not recall the previous command; the pane holds:\n%s", snapshot.Data)
}

// TestTmuxBackendLineDisciplineInterpretsErase records a property of ptys that
// this design does not fight.
//
// A terminal's line discipline acts on a handful of control bytes - erase,
// kill, interrupt, end-of-file, flow control - before any program sees them.
// That is not a gap in the input channel: it is the same mechanism that makes
// Ctrl-C stop a runaway command, so a runtime that delivered 0x7f literally
// would be a runtime whose backspace key did not work. The test exists so the
// behaviour is recorded rather than discovered, and so that a change to it is a
// failure somebody has to think about.
func TestTmuxBackendLineDisciplineInterpretsErase(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, dir := newTestSession(t, b, "p_erase", 120, 30)

	captured := filepath.Join(dir, "captured")
	if err := b.Launch(ctx, session.Name, "cat > "+captured); err != nil {
		t.Fatalf("could not start the reader: %v", err)
	}
	waitForPaneCommand(t, b, session.Name, "cat")

	// "ab", erase, newline. 0x7f is the terminal's erase character.
	if err := b.SendInput(ctx, session.Name, []byte("ab\x7f\n")); err != nil {
		t.Fatalf("SendInput returned an error: %v", err)
	}
	if err := b.SendInput(ctx, session.Name, []byte{0x04}); err != nil {
		t.Fatalf("could not end the input: %v", err)
	}

	got := waitForFile(t, captured, readinessWait, func(d []byte) bool { return len(d) > 0 })
	if string(got) != "a\n" {
		t.Errorf("the session read %q, want %q; if this changed, docs/RUNTIME.md is now wrong",
			got, "a\n")
	}
}

// catFile types "cat <path>" into a session and returns what came back.
//
// The bytes to be checked are written to a file first, so the program's output
// is exactly the bytes under test and cannot be confused with the terminal
// echoing the command that produced them.
func catFile(t *testing.T, b *TmuxBackend, sub Subscription, name, path string, ready func([]byte) bool) []byte {
	t.Helper()
	if err := b.Launch(context.Background(), name, "cat "+path); err != nil {
		t.Fatalf("could not run cat: %v", err)
	}
	return drain(sub, outputWait, ready)
}

// TestTmuxBackendOutputKeepsAnsiAndUnicode is the output-fidelity requirement,
// stated as a property of bytes: colour, cursor control, and multibyte
// characters arrive exactly as the program wrote them.
func TestTmuxBackendOutputKeepsAnsiAndUnicode(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, dir := newTestSession(t, b, "p_output", 120, 30)

	sub, err := b.Attach(ctx, session.Name)
	if err != nil {
		t.Fatalf("Attach returned an error: %v", err)
	}
	defer sub.Close()
	syncStream(t, b, sub, session.Name)

	// Colour: ESC [ 3 1 m ... ESC [ 0 m. A pipeline that stripped ANSI, or that
	// re-encoded the screen as text, would lose the ESC bytes.
	colour := []byte("\x1b[31mRED\x1b[0m\n")
	colourFile := filepath.Join(dir, "colour")
	if err := os.WriteFile(colourFile, colour, 0o644); err != nil {
		t.Fatalf("could not write the test file: %v", err)
	}
	got := catFile(t, b, sub, session.Name, colourFile, func(d []byte) bool {
		return bytes.Contains(d, []byte("\x1b[31mRED\x1b[0m"))
	})
	if !bytes.Contains(got, []byte("\x1b[31mRED\x1b[0m")) {
		t.Errorf("the colour sequence did not survive; the stream carried:\n%q", got)
	}

	// Cursor control: ESC [ 2 J, erase display.
	cursor := []byte("\x1b[2J\x1b[Hhome\n")
	cursorFile := filepath.Join(dir, "cursor")
	if err := os.WriteFile(cursorFile, cursor, 0o644); err != nil {
		t.Fatalf("could not write the test file: %v", err)
	}
	got = catFile(t, b, sub, session.Name, cursorFile, func(d []byte) bool {
		return bytes.Contains(d, []byte("\x1b[Hhome"))
	})
	if !bytes.Contains(got, []byte("\x1b[2J\x1b[H")) {
		t.Errorf("the cursor-control sequence did not survive; the stream carried:\n%q", got)
	}

	// Multibyte: Chinese, as UTF-8 bytes rather than as replacement characters.
	unicode := []byte("amx-unicode-\xe4\xb8\xad\xe6\x96\x87-\xe2\x9c\x93\n")
	unicodeFile := filepath.Join(dir, "unicode")
	if err := os.WriteFile(unicodeFile, unicode, 0o644); err != nil {
		t.Fatalf("could not write the test file: %v", err)
	}
	got = catFile(t, b, sub, session.Name, unicodeFile, func(d []byte) bool {
		return bytes.Contains(d, []byte("amx-unicode-\xe4\xb8\xad\xe6\x96\x87"))
	})
	if !bytes.Contains(got, []byte("-中文-")) {
		t.Errorf("the multibyte characters did not survive; the stream carried:\n%q", got)
	}
	if bytes.Contains(got, []byte("\xef\xbf\xbd")) {
		t.Error("a UTF-8 replacement character appeared, so the output was decoded as text somewhere")
	}
}

// TestTmuxBackendOutputKeepsProgressInPlace is the fidelity requirement that
// polling cannot meet.
//
// A carriage return that overwrites a line and a backspace that steps back
// inside one are the two writes that a screen-scraping pipeline destroys: both
// become plain lines, and a progress indicator becomes a list of every state it
// passed through. Keeping the bytes is the only way to keep the meaning.
func TestTmuxBackendOutputKeepsProgressInPlace(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, dir := newTestSession(t, b, "p_progress", 120, 30)

	sub, err := b.Attach(ctx, session.Name)
	if err != nil {
		t.Fatalf("Attach returned an error: %v", err)
	}
	defer sub.Close()
	syncStream(t, b, sub, session.Name)

	carriage := []byte("step-1\rstep-2\rstep-3\n")
	carriageFile := filepath.Join(dir, "carriage")
	if err := os.WriteFile(carriageFile, carriage, 0o644); err != nil {
		t.Fatalf("could not write the test file: %v", err)
	}
	got := catFile(t, b, sub, session.Name, carriageFile, func(d []byte) bool {
		return bytes.Contains(d, []byte("step-3"))
	})
	if !bytes.Contains(got, []byte("step-1\rstep-2\rstep-3")) {
		t.Errorf("the carriage returns did not survive as bytes; the stream carried:\n%q", got)
	}

	backspace := []byte("spin-|\bspin-/\bspin-\n")
	backspaceFile := filepath.Join(dir, "backspace")
	if err := os.WriteFile(backspaceFile, backspace, 0o644); err != nil {
		t.Fatalf("could not write the test file: %v", err)
	}
	got = catFile(t, b, sub, session.Name, backspaceFile, func(d []byte) bool {
		return bytes.Contains(d, []byte("spin-/"))
	})
	if !bytes.Contains(got, []byte("spin-|\bspin-/")) {
		t.Errorf("the backspaces did not survive as bytes; the stream carried:\n%q", got)
	}
}

// TestTmuxBackendSnapshotCarriesEscapeSequences checks the one place a screen is
// turned into bytes. The snapshot is what a client that has just connected is
// shown, and a snapshot without colour would repaint the screen wrongly.
func TestTmuxBackendSnapshotCarriesEscapeSequences(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, dir := newTestSession(t, b, "p_snapshot", 120, 30)

	colourFile := filepath.Join(dir, "colour")
	if err := os.WriteFile(colourFile, []byte("\x1b[32mGREEN\x1b[0m\n"), 0o644); err != nil {
		t.Fatalf("could not write the test file: %v", err)
	}
	if err := b.Launch(ctx, session.Name, "cat "+colourFile); err != nil {
		t.Fatalf("could not run cat: %v", err)
	}

	deadline := time.Now().Add(outputWait)
	for time.Now().Before(deadline) {
		snapshot, err := b.Snapshot(ctx, session.Name)
		if err != nil {
			t.Fatalf("Snapshot returned an error: %v", err)
		}
		if bytes.Contains(snapshot.Data, []byte("GREEN")) {
			if !bytes.Contains(snapshot.Data, []byte("\x1b[32m")) {
				t.Errorf("the snapshot lost the colour escapes; it holds:\n%q", snapshot.Data)
			}
			// A screen is not only its text. The geometry has to describe the
			// pane the rows were captured from, or a client sizes its terminal
			// wrongly and wraps them in the wrong places.
			if snapshot.Cols != 120 || snapshot.Rows != 30 {
				t.Errorf("the screen is %dx%d, want the pane's 120x30", snapshot.Cols, snapshot.Rows)
			}
			if snapshot.CursorY < 0 || snapshot.CursorY >= snapshot.Rows {
				t.Errorf("the cursor row is %d, which is outside a %d-row screen",
					snapshot.CursorY, snapshot.Rows)
			}
			if snapshot.Alternate {
				t.Error("the screen reports the alternate buffer, but the session is at a shell prompt")
			}
			// Render is what a client actually draws, and it has to place the
			// cursor: capture-pane does not emit one.
			if rendered := snapshot.Render(); !bytes.Contains(rendered, []byte("\x1b[")) {
				t.Errorf("the rendered screen carries no escape sequences: %q", rendered)
			}
			return
		}
		time.Sleep(pollInterval)
	}
	t.Error("the snapshot never showed the command's output")
}

// TestTmuxBackendSessionsDoNotShareOutput is the isolation requirement.
//
// Three sessions is the smallest number that can tell "output goes to the wrong
// session" apart from "output goes to the other session": with two, a swap and a
// duplicate look the same.
func TestTmuxBackendSessionsDoNotShareOutput(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	ids := []string{"p_one", "p_two", "p_three"}
	subs := make(map[string]Subscription, len(ids))
	for _, id := range ids {
		session, _ := newTestSession(t, b, id, 120, 30)
		sub, err := b.Attach(ctx, session.Name)
		if err != nil {
			t.Fatalf("Attach for %s returned an error: %v", id, err)
		}
		defer sub.Close()
		syncStream(t, b, sub, session.Name)
		subs[id] = sub
	}

	// A distinct marker per session, typed into each in turn.
	for _, id := range ids {
		if err := b.Launch(ctx, project.SessionNameFor(id), "echo amx-only-"+id); err != nil {
			t.Fatalf("could not type into %s: %v", id, err)
		}
	}

	for _, id := range ids {
		got := drainFor(subs[id], []byte("amx-only-"+id), outputWait)
		if !bytes.Contains(got, []byte("amx-only-"+id)) {
			t.Errorf("%s never received its own output", id)
		}
		for _, other := range ids {
			if other == id {
				continue
			}
			if bytes.Contains(got, []byte("amx-only-"+other)) {
				t.Errorf("output meant for %s arrived at %s", other, id)
			}
		}
	}

	// Bytes typed into one session must not reach the others either. The marker
	// is multibyte as well, so a mis-routed UTF-8 chunk has nowhere to hide.
	one := project.SessionNameFor("p_one")
	if err := b.SendInput(ctx, one, []byte("echo amx-routed-\xe4\xb8\xad\xe6\x96\x87\r")); err != nil {
		t.Fatalf("could not type into p_one: %v", err)
	}

	if got := drain(subs["p_three"], 500*time.Millisecond, nil); bytes.Contains(got, []byte("amx-routed-")) {
		t.Errorf("input typed into p_one reached p_three:\n%q", got)
	}
	if got := drain(subs["p_two"], 500*time.Millisecond, nil); bytes.Contains(got, []byte("amx-routed-")) {
		t.Errorf("input typed into p_one reached p_two:\n%q", got)
	}
}

// TestTmuxBackendAttachGivesEachCallerItsOwnStream checks the contract that a
// subscription is not shared: one caller closing its stream must not interrupt
// another's.
func TestTmuxBackendAttachGivesEachCallerItsOwnStream(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, _ := newTestSession(t, b, "p_two_streams", 120, 30)

	first, err := b.Attach(ctx, session.Name)
	if err != nil {
		t.Fatalf("the first Attach returned an error: %v", err)
	}
	second, err := b.Attach(ctx, session.Name)
	if err != nil {
		t.Fatalf("the second Attach returned an error: %v", err)
	}
	defer second.Close()
	syncStream(t, b, second, session.Name)

	if err := first.Close(); err != nil {
		t.Fatalf("closing the first stream returned an error: %v", err)
	}

	// The second stream is unaffected by the first one going away.
	if err := b.Launch(ctx, session.Name, "echo amx-second-stream"); err != nil {
		t.Fatalf("could not type into the session: %v", err)
	}
	if got := drainFor(second, []byte("amx-second-stream"), outputWait); !bytes.Contains(got, []byte("amx-second-stream")) {
		t.Errorf("closing one subscription interrupted another; the second carried:\n%q", got)
	}
}

// TestTmuxBackendCreateRejectsAUsableButUnusableDirectory covers the guard that
// keeps a session from starting somewhere that does not exist.
func TestTmuxBackendCreateRejectsAUnusableDirectory(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	missing := filepath.Join(t.TempDir(), "not-created")
	if _, err := b.Create(ctx, SessionSpec{Name: "amx-p_nodir", Dir: missing, Cols: 80, Rows: 24}); !IsCode(err, CodeStartFailed) {
		t.Errorf("Create in a directory that does not exist failed with %v, want %s", err, CodeStartFailed)
	}

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("could not create the test file: %v", err)
	}
	if _, err := b.Create(ctx, SessionSpec{Name: "amx-p_notdir", Dir: file, Cols: 80, Rows: 24}); !IsCode(err, CodeStartFailed) {
		t.Errorf("Create in a path that is a file failed with %v, want %s", err, CodeStartFailed)
	}

	if _, err := b.Create(ctx, SessionSpec{Name: "amx-p_nodir", Dir: "  ", Cols: 80, Rows: 24}); !IsCode(err, CodeStartFailed) {
		t.Errorf("Create with no directory failed with %v, want %s", err, CodeStartFailed)
	}
}

// TestTmuxBackendLaunchRefusesAMultiLineCommand keeps a launch a launch: a
// command that runs several lines is a script, and typing one into a shell is
// how a "start the agent" button quietly becomes an arbitrary command runner.
func TestTmuxBackendLaunchRefusesAMultiLineCommand(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, _ := newTestSession(t, b, "p_multiline", 80, 24)

	for _, command := range []string{"echo one\necho two", "echo one\recho two"} {
		if err := b.Launch(ctx, session.Name, command); !IsCode(err, CodeInvalidInput) {
			t.Errorf("Launch(%q) failed with %v, want %s", command, err, CodeInvalidInput)
		}
	}
}

// TestTmuxBackendSendInputAcceptsEmptyData checks the boundary: nothing to send
// is not an error, because a caller assembling input in pieces will produce it.
func TestTmuxBackendSendInputAcceptsEmptyData(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, _ := newTestSession(t, b, "p_empty_input", 80, 24)

	if err := b.SendInput(ctx, session.Name, nil); err != nil {
		t.Errorf("SendInput with no data failed with %v", err)
	}
}

// TestTmuxBackendSendInputCarriesALargePaste covers the chunking: a paste larger
// than one send-keys invocation must arrive in order and complete.
func TestTmuxBackendSendInputCarriesALargePaste(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, dir := newTestSession(t, b, "p_paste", 120, 30)

	captured := filepath.Join(dir, "captured")
	if err := b.Launch(ctx, session.Name, "cat > "+captured); err != nil {
		t.Fatalf("could not start the reader: %v", err)
	}
	waitForPaneCommand(t, b, session.Name, "cat")

	// Four times the chunk size, so the chunk boundaries are exercised, and all
	// on one line so the line discipline delivers it as one piece.
	line := bytes.Repeat([]byte("paste-0123456789-"), 100)
	paste := append(append([]byte{}, line...), '\n')
	if err := b.SendInput(ctx, session.Name, paste); err != nil {
		t.Fatalf("SendInput returned an error: %v", err)
	}
	if err := b.SendInput(ctx, session.Name, []byte{0x04}); err != nil {
		t.Fatalf("could not end the input: %v", err)
	}

	got := waitForFile(t, captured, readinessWait, func(d []byte) bool { return len(d) >= len(paste) })
	if !bytes.Equal(got, paste) {
		t.Errorf("the paste arrived as %d bytes, want %d", len(got), len(paste))
	}
}

// TestTmuxBackendAvailableRejectsAnOldVersion checks the version floor, since the
// resize and control-mode behaviour the backend depends on is 3.x.
func TestTmuxBackendAvailableRejectsAnOldVersion(t *testing.T) {
	if major, ok := parseTmuxMajor("2.9a"); !ok || major != 2 {
		t.Errorf("parseTmuxMajor(%q) = %d, %v, want 2, true", "2.9a", major, ok)
	}
	if major, ok := parseTmuxMajor("3.4"); !ok || major != 3 {
		t.Errorf("parseTmuxMajor(%q) = %d, %v, want 3, true", "3.4", major, ok)
	}
	// tmux from its development branch names itself rather than numbering
	// itself, so the digits have to be found rather than assumed to be first.
	if major, ok := parseTmuxMajor("next-3.5"); !ok || major != 3 {
		t.Errorf("parseTmuxMajor(%q) = %d, %v, want 3, true", "next-3.5", major, ok)
	}
	if major, ok := parseTmuxMajor("openbsd"); ok {
		t.Errorf("parseTmuxMajor(%q) = %d, %v, want a refusal", "openbsd", major, ok)
	}

	b := NewTmuxBackend(TmuxOptions{
		Binary:     "tmux-that-does-not-exist",
		SocketPath: uniqueSocketPath(t),
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err := b.Available(context.Background()); !IsCode(err, CodeBackendUnavailable) {
		t.Errorf("Available with a missing binary failed with %v, want %s", err, CodeBackendUnavailable)
	}
}

// TestDecodeControlEscapes pins the wire format the whole output path rests on.
//
// It is a unit test rather than an integration test because the escaping is a
// documented contract of tmux's control mode, and the case that matters most -
// bytes above 0x7f passing through unescaped - is easy to "fix" by decoding the
// payload as text, which turns every Chinese character into a replacement
// character while the tests still pass on ASCII.
func TestDecodeControlEscapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text", "hello", "hello"},
		{"a literal backslash", `\\`, `\`},
		{"carriage return", `\015`, "\r"},
		{"escape", `\033[31m`, "\x1b[31m"},
		{"delete", `\177`, "\x7f"},
		{"high bytes pass through", "\xe4\xb8\xad\xe6\x96\x87", "中文"},
		{"an invalid high byte passes through", "\x80\xfe\xff", "\x80\xfe\xff"},
		{"a lone backslash is data", `\`, `\`},
		{"a short octal run is not an escape", `\01`, `\01`},
		// tmux escapes control bytes and nothing else. `\xNN` is not part of the
		// format, so a decoder that handled it would be inventing an escape tmux
		// never sends - and the bytes above 0x7f, which is where all the
		// non-ASCII text lives, would be at risk of being rewritten.
		{"a backslash-x run is not an escape", `\xe4\xb8\xad`, `\xe4\xb8\xad`},
		{"mixed", `a\015b中\033[0m\177`, "a\rb中\x1b[0m\x7f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(decodeControlEscapes(nil, []byte(tc.in))); got != tc.want {
				t.Errorf("decodeControlEscapes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestControlPayloadParsing covers the record shapes the reader has to tell
// apart, including the " : " that only the extended form has.
func TestControlPayloadParsing(t *testing.T) {
	if got, ok := controlPayload([]byte("%0 hello there")); !ok || string(got) != "hello there" {
		t.Errorf("controlPayload = %q, %v, want %q, true", got, ok, "hello there")
	}
	if _, ok := controlPayload([]byte("no pane id")); ok {
		t.Error("controlPayload accepted a record with no pane id")
	}
	if got, ok := extendedOutputPayload([]byte("%0 12 : hello")); !ok || string(got) != "hello" {
		t.Errorf("extendedOutputPayload = %q, %v, want %q, true", got, ok, "hello")
	}
	if name, rest := splitControlRecord([]byte("%output %0 x")); name != "output" || string(rest) != "%0 x" {
		t.Errorf("splitControlRecord = %q, %q", name, rest)
	}
	if name, rest := splitControlRecord([]byte("%exit")); name != "exit" || rest != nil {
		t.Errorf("splitControlRecord(%q) = %q, %q, want exit, nil", "%exit", name, rest)
	}
}

// TestSessionNamingIsOneDefinition is the naming requirement: one function, and
// the name derived from the id rather than the display name.
func TestSessionNamingIsOneDefinition(t *testing.T) {
	if got := project.SessionNameFor("p_abc"); got != "amx-p_abc" {
		t.Errorf("SessionNameFor(%q) = %q, want %q", "p_abc", got, "amx-p_abc")
	}

	p := &project.Project{ID: "p_abc", Name: "A Project"}
	if got := p.SessionName(); got != "amx-p_abc" {
		t.Errorf("SessionName() = %q, want %q", got, "amx-p_abc")
	}

	// Renaming must not move the session. This is the assertion that fails if
	// anybody ever derives the name from the display name.
	p.Name = "A Renamed Project"
	if got := p.SessionName(); got != "amx-p_abc" {
		t.Errorf("after a rename SessionName() = %q, want the name to be unchanged", got)
	}
}

// TestSessionDescriptionRoundTrips pins the encoding of one session
// description, which is the format string handed to tmux and the parser that
// reads it back.
//
// The separator has to be printable. tmux escapes non-printable bytes in the
// output of a -F format, so a control-character separator is returned as the
// text `\037` and the description silently fails to split - every session then
// looks malformed, and the failure appears at the parser rather than at the
// format string where it belongs. The unit test is here so that changing the
// separator to something clever fails immediately and in one place.
func TestSessionDescriptionRoundTrips(t *testing.T) {
	for i := 0; i < len(sessionFieldSeparator); i++ {
		if c := sessionFieldSeparator[i]; c < 0x20 || c == 0x7f {
			t.Fatalf("the field separator %q contains a non-printable byte, which tmux will escape",
				sessionFieldSeparator)
		}
	}

	// A path that contains the separator still arrives whole, because the split
	// is bounded and the path is the last field.
	awkward := "/tmp/a" + sessionFieldSeparator + "b"
	line := strings.Join([]string{"amx-p_x", "1700000000", "100", "30", awkward}, sessionFieldSeparator)

	sess, ok := parseSessionLine(line)
	if !ok {
		t.Fatalf("parseSessionLine refused a description it produced itself: %q", line)
	}
	if sess.Name != "amx-p_x" {
		t.Errorf("the parsed name is %q, want %q", sess.Name, "amx-p_x")
	}
	if sess.Cols != 100 || sess.Rows != 30 {
		t.Errorf("the parsed size is %dx%d, want 100x30", sess.Cols, sess.Rows)
	}
	if sess.Dir != awkward {
		t.Errorf("the parsed directory is %q, want %q", sess.Dir, awkward)
	}

	// And a description that is not one is refused rather than half-read.
	if _, ok := parseSessionLine("amx-p_x|not-a-number|100|30|/tmp"); ok {
		t.Error("parseSessionLine accepted a description with an unparseable timestamp")
	}
}

// TestChunkBufferBounds and its neighbours cover the history the sequence
// numbers index into.
func TestChunkBufferBounds(t *testing.T) {
	buf := newChunkBuffer(3, 1<<20)
	for i := 1; i <= 5; i++ {
		buf.append(Chunk{Sequence: uint64(i), Data: []byte{byte('0' + i)}})
	}
	got := buf.since(0)
	if len(got) != 3 {
		t.Fatalf("the buffer kept %d chunks, want 3", len(got))
	}
	if got[0].Sequence != 3 || got[2].Sequence != 5 {
		t.Errorf("the buffer kept sequences %d..%d, want 3..5", got[0].Sequence, got[2].Sequence)
	}
	if latest := buf.latest(); latest != 5 {
		t.Errorf("latest() = %d, want 5", latest)
	}
	if after := buf.since(4); len(after) != 1 || after[0].Sequence != 5 {
		t.Errorf("since(4) returned %d chunks, want just sequence 5", len(after))
	}
	if none := buf.since(5); len(none) != 0 {
		t.Errorf("since(5) returned %d chunks, want none", len(none))
	}
}

// TestChunkBufferByteLimit covers the other bound: a session that prints one
// enormous line must not be able to grow the buffer without limit.
func TestChunkBufferByteLimit(t *testing.T) {
	buf := newChunkBuffer(1000, 100)
	for i := 1; i <= 10; i++ {
		buf.append(Chunk{Sequence: uint64(i), Data: bytes.Repeat([]byte("x"), 30)})
	}
	total := 0
	for _, c := range buf.since(0) {
		total += len(c.Data)
	}
	if total > 100 {
		t.Errorf("the buffer holds %d bytes, want at most 100", total)
	}
	if len(buf.since(0)) == 0 {
		t.Error("the byte limit discarded everything, including the newest chunk")
	}
}

// TestTmuxBackendOptionsDefault checks that a zero value is a working backend
// rather than one that fails on its first call.
func TestTmuxBackendOptionsDefault(t *testing.T) {
	b := NewTmuxBackend(TmuxOptions{})
	if b.install.bin != DefaultTmuxBinary {
		t.Errorf("the binary is %q, want %q", b.install.bin, DefaultTmuxBinary)
	}
	if b.config != DefaultTmuxConfig {
		t.Errorf("the config is %q, want %q", b.config, DefaultTmuxConfig)
	}
	if b.terminal != DefaultTmuxTerminal {
		t.Errorf("the terminal is %q, want %q", b.terminal, DefaultTmuxTerminal)
	}
	if b.historyLimit != DefaultTmuxHistoryLimit {
		t.Errorf("the history limit is %d, want %d", b.historyLimit, DefaultTmuxHistoryLimit)
	}
	if b.prefix != project.SessionPrefix {
		t.Errorf("the prefix is %q, want %q", b.prefix, project.SessionPrefix)
	}
	if b.log == nil {
		t.Error("the logger is nil, so a log call would panic")
	}
	// The socket path is empty here, and since Phase 2.5 that is a refusal
	// rather than a fallback. Phase 2 read an empty socket as "the user's
	// default socket", which meant a backend that had lost its socket path
	// would quietly drive whatever server the user happened to own. There is no
	// safe default now: every backend owns one project's server, so a backend
	// without a socket has nothing to address.
	if b.socketPath != "" {
		t.Errorf("the socket path is %q, want the empty default", b.socketPath)
	}
	if err := b.Available(context.Background()); !IsCode(err, CodeBackendUnavailable) {
		t.Errorf("Available without a socket path failed with %v, want %s", err, CodeBackendUnavailable)
	}
}

// TestTmuxBackendConfigCommandIsOneLine pins the shape that makes the global
// options apply at all. Sent as separate invocations they are lost, because a
// tmux server with no sessions exits immediately and the socket is gone.
func TestTmuxBackendConfigCommandIsOneLine(t *testing.T) {
	got := strings.Join(configCommand("screen-256color", 1000), " ")
	want := "set-option -g history-limit 1000 ; set-option -g default-terminal screen-256color ; " +
		"set-option -g destroy-unattached off"
	if got != want {
		t.Errorf("configCommand = %q, want %q", got, want)
	}
}

// TestTmuxBackendCreateAppliesItsGlobalOptions is the regression test for the
// bootstrap bug: the server does not exist yet when Create runs, and the options
// have to be in place before the first pane is.
func TestTmuxBackendCreateAppliesItsGlobalOptions(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, _ := newTestSession(t, b, "p_options", 80, 24)

	out, err := b.run(ctx, "show-options", "-g", "default-terminal")
	if err != nil {
		t.Fatalf("could not read the server's terminal option: %v", err)
	}
	if !strings.Contains(out, DefaultTmuxTerminal) {
		t.Errorf("default-terminal is %q, want it to name %q", strings.TrimSpace(out), DefaultTmuxTerminal)
	}

	out, err = b.run(ctx, "show-options", "-g", "history-limit")
	if err != nil {
		t.Fatalf("could not read the server's history limit: %v", err)
	}
	if !strings.Contains(out, strconv.Itoa(DefaultTmuxHistoryLimit)) {
		t.Errorf("history-limit is %q, want %d", strings.TrimSpace(out), DefaultTmuxHistoryLimit)
	}

	// The pane got the terminal the option named, which is the part that fails
	// if the options are applied after the session is created.
	report := filepath.Join(t.TempDir(), "term.txt")
	if err := b.Launch(ctx, session.Name, `printf 'AMXTERM:%s\n' "$TERM" > `+report); err != nil {
		t.Fatalf("could not ask the session for its TERM: %v", err)
	}
	got := strings.TrimSpace(string(waitForFile(t, report, readinessWait, func(d []byte) bool {
		return bytes.Contains(d, []byte("AMXTERM:"))
	})))
	if got != "AMXTERM:"+DefaultTmuxTerminal {
		t.Errorf("the pane reports %q, want %q", got, "AMXTERM:"+DefaultTmuxTerminal)
	}
}

// TestTmuxBackendWindowSizeIsManual checks the setting that stops a client from
// resizing the session it attaches to. Without it the canonical size would be
// whatever the last client's window happened to be.
func TestTmuxBackendWindowSizeIsManual(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()

	session, _ := newTestSession(t, b, "p_manual", 90, 25)

	out, err := b.run(ctx, "show-options", "-t", session.Name, "window-size")
	if err != nil {
		t.Fatalf("could not read the window-size option: %v", err)
	}
	if !strings.Contains(out, "manual") {
		t.Errorf("window-size is %q, want manual", strings.TrimSpace(out))
	}

	// And the size survives a client attaching and detaching, which is the point.
	sub, err := b.Attach(ctx, session.Name)
	if err != nil {
		t.Fatalf("Attach returned an error: %v", err)
	}
	syncStream(t, b, sub, session.Name)
	if err := sub.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}

	inspected, err := b.Inspect(ctx, session.Name)
	if err != nil {
		t.Fatalf("Inspect returned an error: %v", err)
	}
	if inspected.Cols != 90 || inspected.Rows != 25 {
		t.Errorf("the session is %dx%d after a client came and went, want 90x25",
			inspected.Cols, inspected.Rows)
	}
}

// TestTmuxBackendNameIsTheBackendName is a small check with a real purpose: the
// value is persisted in every runtime record, so changing it silently would make
// stored records describe a backend that no longer answers to that name.
func TestTmuxBackendNameIsTheBackendName(t *testing.T) {
	b := NewTmuxBackend(TmuxOptions{})
	if b.Name() != "tmux" {
		t.Errorf("Name() = %q, want %q", b.Name(), "tmux")
	}
}

// errString renders an error for a message, so a nil never prints as a panic.
func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// TestTmuxBackendErrorsCarryCodedDetail is a small check that the coded errors
// keep their cause, since the HTTP layer reports the code and the log reports
// the cause.
func TestTmuxBackendErrorsCarryCodedDetail(t *testing.T) {
	inner := errors.New("the cause")
	wrapped := wrapError(inner, CodeStartFailed, "could not start %s", "x")

	if !errors.Is(wrapped, inner) {
		t.Error("a wrapped error does not unwrap to its cause")
	}
	if !IsCode(wrapped, CodeStartFailed) {
		t.Errorf("the wrapped error's code is %q, want %q", CodeOf(wrapped), CodeStartFailed)
	}
	if got, want := errString(wrapped), "runtime_start_failed: could not start x: the cause"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if CodeOf(inner) != "" {
		t.Errorf("an uncoded error reports the code %q, want none", CodeOf(inner))
	}
	if !strings.Contains(newError(CodeInvalidSize, "%dx%d", 0, 0).Error(), "0x0") {
		t.Error("a formatted coded error did not interpolate its arguments")
	}
}
