package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"testing"

	"github.com/kutonlagos/agentmux/internal/project"
)

// These tests are about the socket directory: the one place that decides where
// a project's runtime lives. Everything Phase 2.5 promises - one project, one
// server, one blast radius - is a consequence of what is in this file, so the
// cases where it could quietly do the wrong thing are the cases tested.

// testSocketDir returns a socket layout over a fresh directory.
func testSocketDir(t *testing.T) (*SocketDir, string) {
	t.Helper()
	dir := uniqueSocketDir(t)
	layout, err := NewSocketDir(dir, DefaultTmuxBinary)
	if err != nil {
		t.Fatalf("NewSocketDir returned an error: %v", err)
	}
	return layout, layout.Dir()
}

func TestSocketDirRefusesAnEmptyPath(t *testing.T) {
	// There is no safe default: falling back to tmux's own socket directory
	// would put every project back on one server, which is the arrangement this
	// phase exists to remove.
	for _, dir := range []string{"", "   "} {
		if _, err := NewSocketDir(dir, DefaultTmuxBinary); err == nil {
			t.Errorf("NewSocketDir(%q) succeeded, want a refusal", dir)
		}
	}
}

// TestSocketDirIsOwnerOnly checks the mode a control channel has to have.
//
// A tmux socket is not a file handle: anything that can open it can type into a
// project's terminal, read everything it prints, and kill it. The directory is
// created with an explicit mode rather than left to the umask because the umask
// is 0022 on both platforms AgentMux runs on, which would make it world
// readable.
func TestSocketDirIsOwnerOnly(t *testing.T) {
	if runningOnWindows() {
		t.Skip("Windows has no POSIX permission bits to check")
	}

	_, dir := testSocketDir(t)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("could not stat the socket directory: %v", err)
	}
	if got := info.Mode().Perm(); got != socketDirPerm {
		t.Errorf("the socket directory is %04o, want %04o", got, socketDirPerm)
	}
}

// TestSocketDirTightensAnExistingDirectory is the upgrade case.
//
// A directory made by an older release - or by hand - keeps whatever mode it
// had, and nothing would ever report it. Every start narrows it instead.
func TestSocketDirTightensAnExistingDirectory(t *testing.T) {
	if runningOnWindows() {
		t.Skip("Windows has no POSIX permission bits to check")
	}

	dir := filepath.Join(t.TempDir(), "tmux")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("could not make the directory: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("could not widen the directory: %v", err)
	}

	if _, err := NewSocketDir(dir, DefaultTmuxBinary); err != nil {
		t.Fatalf("NewSocketDir returned an error: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("could not stat the socket directory: %v", err)
	}
	if got := info.Mode().Perm(); got != socketDirPerm {
		t.Errorf("the socket directory is %04o after NewSocketDir, want %04o", got, socketDirPerm)
	}
}

// TestSocketPathComesFromTheProjectID is the rule that a socket name is never
// derived from anything a user can type.
//
// A path built from a display name would move a project's runtime whenever the
// project was renamed, and would let a crafted name address a path outside the
// socket directory. Both are refused by returning nothing, which callers must
// treat as "this project has no runtime" rather than repairing.
func TestSocketPathComesFromTheProjectID(t *testing.T) {
	layout, dir := testSocketDir(t)

	id := testProjectID("socket-path")
	want := filepath.Join(dir, id+socketFileSuffix)
	if got := layout.Path(id); got != want {
		t.Errorf("Path(%q) = %q, want %q", id, got, want)
	}

	for _, bad := range []string{
		"",
		"My Project",                     // a display name
		"p_short",                        // a label, the shape this suite used to use
		"../../etc/passwd",               // traversal
		"p_0123456789abcdef0123456789ab", // one character too long
		"p_0123456789ABCDEF0123",         // upper case hex
		id + "/../" + id,                 // a valid id with something appended
	} {
		if got := layout.Path(bad); got != "" {
			t.Errorf("Path(%q) = %q, want a refusal", bad, got)
		}
	}
}

// TestSocketPathIsStableAcrossRestarts is the property that makes storing it
// unnecessary: the same id and the same directory give the same path, so a
// restart recomputes where a runtime lives instead of reading it back.
func TestSocketPathIsStableAcrossRestarts(t *testing.T) {
	dir := uniqueSocketDir(t)
	first, err := NewSocketDir(dir, DefaultTmuxBinary)
	if err != nil {
		t.Fatalf("NewSocketDir returned an error: %v", err)
	}
	second, err := NewSocketDir(dir, DefaultTmuxBinary)
	if err != nil {
		t.Fatalf("NewSocketDir returned an error on the second call: %v", err)
	}

	id := testProjectID("stable")
	if got, want := first.Path(id), second.Path(id); got != want {
		t.Errorf("the path changed between two resolutions: %q then %q", got, want)
	}
}

func TestSocketListReportsSocketsAndFlagsForeignNames(t *testing.T) {
	layout, dir := testSocketDir(t)

	mine := testProjectID("listed")
	writeFile(t, filepath.Join(dir, mine+socketFileSuffix))
	writeFile(t, filepath.Join(dir, "notes.txt"))    // not a socket name at all
	writeFile(t, filepath.Join(dir, "someone.sock")) // a socket name that is not an id

	files, err := layout.List()
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("List returned %d entries, want the two .sock files: %v", len(files), files)
	}
	if files[0].Path != filepath.Join(dir, mine+socketFileSuffix) {
		t.Errorf("the first entry is %q, want the project's socket", files[0].Path)
	}
	if files[0].ProjectID != mine || !files[0].WellFormed {
		t.Errorf("the project's socket is reported as %+v, want a well-formed id", files[0])
	}
	if files[1].WellFormed {
		t.Errorf("%q is reported as a well-formed project id; a foreign name must be flagged, not adopted", files[1].Path)
	}
}

func TestSocketListOnAMissingDirectory(t *testing.T) {
	layout := &SocketDir{dir: filepath.Join(t.TempDir(), "gone"), binary: DefaultTmuxBinary}
	files, err := layout.List()
	if err != nil {
		t.Fatalf("List on a missing directory returned an error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("List on a missing directory returned %d entries, want none", len(files))
	}
}

func TestSocketProbeOnAPathThatIsNotThere(t *testing.T) {
	layout, dir := testSocketDir(t)
	probe := layout.Probe(context.Background(), filepath.Join(dir, "nothing.sock"))
	if probe.State != SocketAbsent {
		t.Errorf("the state is %q, want %q", probe.State, SocketAbsent)
	}
}

func TestSocketProbeWithNoPath(t *testing.T) {
	layout, _ := testSocketDir(t)
	if probe := layout.Probe(context.Background(), "  "); probe.State != SocketUnknown {
		t.Errorf("the state is %q, want %q: a probe with no path cannot conclude anything", probe.State, SocketUnknown)
	}
}

// TestSocketProbeFindsALiveServer covers the state that everything else is
// compared against.
//
// A server with nothing on it is still a server, and it is the state a
// reconciliation is most likely to meet by accident: `kill-session` on a
// project's last session leaves the server alive and on its way out, and a
// probe that read "nothing here" during that window would delete the socket of
// a server that is still using it. `exit-empty off` is what makes the state
// last long enough to measure - with tmux's default the server is gone the
// instant its last session is, which is why a destroyed runtime leaves a stale
// socket file behind.
func TestSocketProbeFindsALiveServer(t *testing.T) {
	layout, dir := testSocketDir(t)
	id := testProjectID("live")
	path := layout.Path(id)

	backend := newTestBackendOn(t, path)

	// One command line, because a server with no sessions exits at once and a
	// separate second invocation would be talking to a socket that is already
	// dead - the same reason Create sends its bootstrap as one line.
	if _, err := backend.run(context.Background(), "start-server", ";",
		"set-option", "-g", "exit-empty", "off"); err != nil {
		t.Fatalf("could not start a server with no sessions: %v", err)
	}

	probe := layout.Probe(context.Background(), path)
	if probe.State != SocketLive {
		t.Fatalf("the state is %q, want %q (tmux said: %s)", probe.State, SocketLive, probe.Detail)
	}
	if len(probe.Sessions) != 0 {
		t.Errorf("the server has %d sessions, want none before anything was created", len(probe.Sessions))
	}

	newTestSession(t, backend, id, 80, 24)
	probe = layout.Probe(context.Background(), path)
	if probe.State != SocketLive {
		t.Fatalf("the state is %q, want %q", probe.State, SocketLive)
	}
	if len(probe.Sessions) != 1 || probe.Sessions[0].Name != project.SessionNameFor(id) {
		t.Errorf("the probe reported %v, want just %q", probe.Sessions, project.SessionNameFor(id))
	}

	// The directory the socket lives in is the one it belongs to, which is what
	// lets a reconciliation map a socket back to a project.
	if filepath.Dir(path) != dir {
		t.Errorf("the socket is in %q, want the socket directory %q", filepath.Dir(path), dir)
	}
}

// TestSocketProbeOnAStaleSocket pins the ordinary outcome of a destroyed
// runtime.
//
// `kill-server` leaves the socket file behind - measured on tmux 3.4 and 3.7c -
// so a stale socket is not a fault, it is what every finished runtime leaves.
// Telling it apart from a live one is the difference between cleaning up and
// destroying somebody's work.
func TestSocketProbeOnAStaleSocket(t *testing.T) {
	layout, dir := testSocketDir(t)
	id := testProjectID("stale")
	path := layout.Path(id)

	backend := newTestBackendOn(t, path)
	newTestSession(t, backend, id, 80, 24)
	if err := backend.KillServer(context.Background()); err != nil {
		t.Fatalf("could not stop the server: %v", err)
	}

	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("tmux removed the socket file when the server was stopped, so this test is measuring nothing: %v", err)
	}

	probe := layout.Probe(context.Background(), path)
	if probe.State != SocketStale {
		t.Errorf("the state is %q, want %q (tmux said: %s)", probe.State, SocketStale, probe.Detail)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("the socket is in %q, want %q", filepath.Dir(path), dir)
	}
}

func TestSocketReclaimRemovesAStaleSocket(t *testing.T) {
	layout, _ := testSocketDir(t)
	id := testProjectID("reclaim")
	path := layout.Path(id)

	backend := newTestBackendOn(t, path)
	newTestSession(t, backend, id, 80, 24)
	if err := backend.KillServer(context.Background()); err != nil {
		t.Fatalf("could not stop the server: %v", err)
	}

	removed, err := layout.Reclaim(context.Background(), path)
	if err != nil {
		t.Fatalf("Reclaim returned an error: %v", err)
	}
	if !removed {
		t.Fatal("Reclaim left a stale socket behind")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket path still exists after Reclaim: %v", err)
	}

	// Removing it twice, or removing one that was never there, is not an error.
	removed, err = layout.Reclaim(context.Background(), path)
	if err != nil || removed {
		t.Errorf("Reclaim on a path with no file returned (%v, %v), want (false, nil)", removed, err)
	}
}

// TestSocketReclaimLeavesALiveServerAlone is the safety property. Unlinking the
// socket of a running server does not stop the server, it only makes it
// permanently unreachable - the session keeps running with no way back to it.
func TestSocketReclaimLeavesALiveServerAlone(t *testing.T) {
	layout, _ := testSocketDir(t)
	id := testProjectID("alive")
	path := layout.Path(id)

	backend := newTestBackendOn(t, path)
	newTestSession(t, backend, id, 80, 24)

	removed, err := layout.Reclaim(context.Background(), path)
	if err != nil {
		t.Fatalf("Reclaim returned an error: %v", err)
	}
	if removed {
		t.Fatal("Reclaim removed the socket of a running server")
	}
	if probe := layout.Probe(context.Background(), path); probe.State != SocketLive {
		t.Errorf("the server is %q after Reclaim, want %q", probe.State, SocketLive)
	}
	alive, err := backend.Exists(context.Background(), project.SessionNameFor(id))
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if !alive {
		t.Error("Reclaim ended the session on a live server")
	}
}

// TestSocketReclaimLeavesSomethingThatIsNotASocket covers the case where a name
// in the socket directory is not a socket at all.
//
// A directory is the interesting one: os.Remove would happily delete an empty
// one, so without the mode check this would be a silent deletion of something
// AgentMux did not create.
func TestSocketReclaimLeavesSomethingThatIsNotASocket(t *testing.T) {
	// The classification under test is tmux's own wording for "there is a file
	// here and no server behind it", so this needs tmux rather than a fake.
	requireTmuxInstalled(t)

	layout, dir := testSocketDir(t)

	for _, tc := range []struct {
		name string
		make func(t *testing.T, path string)
	}{
		{"a regular file", func(t *testing.T, path string) { writeFile(t, path) }},
		{"a directory", func(t *testing.T, path string) {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatalf("could not make the directory: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, testProjectID("not-a-socket "+tc.name)+socketFileSuffix)
			tc.make(t, path)

			removed, err := layout.Reclaim(context.Background(), path)
			if err == nil {
				t.Error("Reclaim reported no problem with a path that is not a socket; a human should be told")
			}
			if removed {
				t.Error("Reclaim removed something that is not a socket")
			}
			if _, statErr := os.Lstat(path); statErr != nil {
				t.Errorf("the path was removed anyway: %v", statErr)
			}
		})
	}
}

// TestClassifySocketFailure pins tmux's own wording.
//
// The wording is matched rather than the exit code, because every one of these
// exits 1 and the difference between them is the difference between deleting a
// file and leaving it alone. These strings were read off tmux 3.4 and 3.7c; a
// version that words its refusal differently must not be read as "there is
// definitely no server here".
func TestClassifySocketFailure(t *testing.T) {
	for _, tc := range []struct {
		detail string
		want   SocketState
	}{
		{"no server running on /tmp/x/tmux.sock", SocketStale},
		{"error connecting to /tmp/x/tmux.sock (No such file or directory)", SocketStale},
		{"error connecting to /tmp/x/tmux.sock (Permission denied)", SocketUnknown},
		{"error connecting to /tmp/x/tmux.sock (Connection refused)", SocketUnknown},
		{"", SocketUnknown},
		{"lost server", SocketUnknown},
		{"something a future tmux says", SocketUnknown},
	} {
		if got := classifySocketFailure(tc.detail); got != tc.want {
			t.Errorf("classifySocketFailure(%q) = %q, want %q", tc.detail, got, tc.want)
		}
	}
}

// writeFile creates an empty file, failing the test if it cannot.
func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("could not write %s: %v", path, err)
	}
}
