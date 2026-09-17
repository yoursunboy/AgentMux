package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// This file is where a project's runtime lives on disk.
//
// Phase 2 gave every project a session on one shared tmux server. That server
// was a single point of failure with a single blast radius: when it went away,
// every project's terminal went away at once, and nothing in AgentMux could
// tell the difference between "one project's work ended" and "the runtime
// died". Phase 2.5 gives each project its own server, addressed by its own
// socket, so the blast radius of any runtime fault is one project.
//
// The socket is the whole mechanism. tmux identifies a server by its socket
// path, so two paths are two servers: separate processes, separate session
// trees, separate lifetimes. `tmux -S <path>` is that statement, and every
// command AgentMux issues carries it.

// socketFileSuffix is what a project's socket file is called.
const socketFileSuffix = ".sock"

// socketDirPerm is the mode of the directory holding the sockets.
//
// Owner-only, because a tmux socket is a control channel: anything that can
// open it can type into a project's terminal, read its output, and kill it.
// The default umask would leave the directory world-readable on many systems,
// so the mode is stated rather than inherited.
const socketDirPerm os.FileMode = 0o700

// SocketState is what a socket path turned out to be.
//
// The set exists because the safe action differs per case. Deleting a socket
// file is only ever correct for SocketStale, and SocketStale is only ever
// concluded from tmux saying so in as many words.
type SocketState string

const (
	// SocketAbsent means there is no file at the socket path. Nothing is
	// listening, and there is nothing to clean up.
	SocketAbsent SocketState = "ABSENT"

	// SocketLive means a tmux server answered on this socket. It is the
	// server's existence that is reported, not its contents: a live server
	// with no sessions is still a server, and one that has just had its last
	// session killed is exactly the moment a race would try to delete its
	// socket.
	SocketLive SocketState = "LIVE"

	// SocketStale means the file is there and no server is behind it. tmux
	// says this in as many words - "no server running on <path>" - and it is
	// the ordinary outcome rather than a fault: `kill-server` leaves the
	// socket file behind (measured on tmux 3.4 and 3.7c), so every destroyed
	// runtime leaves one.
	SocketStale SocketState = "STALE"

	// SocketUnknown means the path could not be classified: a permission
	// error, a connection refused, a timeout, a file that is not a socket.
	// Nothing is deleted on this answer, ever. A socket AgentMux cannot read
	// might be a server it cannot reach, and unlinking it would strand a live
	// server with no way back.
	SocketUnknown SocketState = "UNKNOWN"
)

// SocketFile is one entry in the socket directory.
type SocketFile struct {
	// Path is the socket path.
	Path string `json:"path"`

	// ProjectID is the identifier the file name encodes, whether or not it is
	// a well-formed one.
	ProjectID string `json:"projectId"`

	// WellFormed reports whether ProjectID has the shape AgentMux issues.
	//
	// A file whose name is not a project id is not AgentMux's, even though it
	// is in AgentMux's directory. It is reported and left alone: deriving a
	// project from an arbitrary file name is how a runtime ends up attached to
	// the wrong thing.
	WellFormed bool `json:"wellFormed"`
}

// SocketDir is the directory holding one tmux socket per project.
//
// The path is a function of two inputs and nothing else - the configured
// socket directory and the project id - which is what makes it recomputable
// after a restart instead of something that has to be stored and kept in sync.
// See the note in docs/RUNTIME.md about why no absolute socket path is written
// to the database.
type SocketDir struct {
	dir    string
	binary string
}

// NewSocketDir resolves the socket directory and makes sure it exists with the
// right permissions.
//
// Tightening an existing directory is deliberate rather than incidental: an
// installation upgraded from a release that created it with the umask's
// permissions would otherwise keep a world-readable runtime control channel
// forever, and nothing would ever report it.
func NewSocketDir(dir, binary string) (*SocketDir, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("session: the tmux socket directory is empty; " +
			"each project's runtime needs its own socket, so there is no safe default")
	}
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return nil, fmt.Errorf("session: resolve tmux socket directory %q: %w", dir, err)
	}
	abs = filepath.Clean(abs)

	if err := os.MkdirAll(abs, socketDirPerm); err != nil {
		return nil, fmt.Errorf("session: create tmux socket directory %q: %w", abs, err)
	}
	if err := os.Chmod(abs, socketDirPerm); err != nil {
		return nil, fmt.Errorf("session: set the tmux socket directory %q to owner-only: %w", abs, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("session: inspect tmux socket directory %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("session: the tmux socket path %q is not a directory", abs)
	}

	return &SocketDir{dir: abs, binary: strings.TrimSpace(binary)}, nil
}

// Dir is the socket directory.
func (d *SocketDir) Dir() string { return d.dir }

// Path is where a project's tmux server listens, or "" when the identifier is
// not one AgentMux issued.
//
// The empty result is a refusal rather than a fallback. A socket path derived
// from anything mutable - a display name, a title, a folder name - would move
// a project's runtime whenever that string changed, and would let a crafted
// name address a path outside the socket directory. Callers that get "" must
// treat the runtime as unavailable; substituting a name is the bug this
// function exists to prevent.
func (d *SocketDir) Path(projectID string) string {
	if !project.ValidID(projectID) {
		return ""
	}
	return filepath.Join(d.dir, projectID+socketFileSuffix)
}

// List returns the socket files present, sorted by path. A missing directory
// is an empty listing rather than an error.
func (d *SocketDir) List() ([]SocketFile, error) {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, wrapError(err, CodeBackendFailure, "could not read the tmux socket directory %q", d.dir)
	}

	out := make([]SocketFile, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, socketFileSuffix) {
			continue
		}
		id := strings.TrimSuffix(name, socketFileSuffix)
		out = append(out, SocketFile{
			Path:       filepath.Join(d.dir, name),
			ProjectID:  id,
			WellFormed: project.ValidID(id),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// SocketProbe is what one socket answered.
type SocketProbe struct {
	State SocketState

	// Sessions are the sessions the server reported. Only a live server has
	// any, and a live server may legitimately have none.
	Sessions []*Session

	// Detail carries tmux's own words when the state is not live. It is for
	// logging and diagnostics: the classification is the contract, the words
	// are the evidence.
	Detail string
}

// Probe asks what is at a socket path.
//
// It runs `list-panes -a` rather than `list-sessions` so that one invocation
// answers both questions a reconciliation has - is a server there, and what is
// on it - instead of two. It never starts a server: measured on tmux 3.4 and
// 3.7c, a read-only command against a socket with no server behind it reports
// the absence and exits, leaving no socket file behind.
//
// One complaint is asked about twice. tmux's client can reach a server that is
// in its last moments - it accepts the connection and then goes - and the
// message for that, "server exited unexpectedly", is not an answer about what is
// at the path now. Returning it as one would classify an ordinary dead socket as
// something AgentMux could not read, which is the answer that cleans up nothing.
func (d *SocketDir) Probe(ctx context.Context, path string) SocketProbe {
	if strings.TrimSpace(path) == "" {
		return SocketProbe{State: SocketUnknown, Detail: "no socket path"}
	}
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SocketProbe{State: SocketAbsent}
		}
		return SocketProbe{State: SocketUnknown, Detail: err.Error()}
	}

	probe := d.probeOnce(ctx, path)
	if !serverVanished(probe.Detail) {
		return probe
	}

	// The server that was there has gone. The wait is for the exit to finish,
	// so that the second look is a question about the path rather than about a
	// process that is still shutting down.
	if !sleepContext(ctx, socketProbeRetrySettle) {
		return probe
	}
	return d.probeOnce(ctx, path)
}

// probeOnce is one question to tmux about one path.
func (d *SocketDir) probeOnce(ctx context.Context, path string) SocketProbe {
	sessions, detail, err := listSessionsOn(ctx, d.binary, path)
	if err == nil {
		return SocketProbe{State: SocketLive, Sessions: sessions}
	}
	return SocketProbe{State: classifySocketFailure(detail), Detail: detail}
}

// serverVanished reports whether tmux's complaint is the one a client makes
// when it reached a server and the server then left.
//
// It is not a socket state and it is deliberately not one: a server that has
// just gone may be one that is about to come back, and the honest response to
// "there was a server here a moment ago" is to ask again rather than to
// conclude anything. Measured on tmux 3.4, where this is what forces the second
// look:
//
//	the server was killed mid-probe   server exited unexpectedly
//
// It happens more often than "mid-probe" suggests, because tmux's own control
// client starts a server when there is none - see startControl in control.go -
// so a reconnect attempt that loses the race can create a server that finds no
// sessions, exits, and leaves its socket file behind. The probe racing with that
// is the case this exists for.
func serverVanished(detail string) bool {
	return strings.Contains(detail, "server exited unexpectedly")
}

// socketProbeRetrySettle is how long Probe waits before asking again about a
// server that left mid-question.
//
// It is short because the window it waits for is short: the server has already
// been asked to stop by the time this is reached, and what remains is the exit
// itself.
const socketProbeRetrySettle = 150 * time.Millisecond

// classifySocketFailure reads tmux's complaint as one of the socket states.
//
// The wording is matched rather than the exit code, because the exit code is 1
// for every one of these and the difference between them is the difference
// between deleting a file and leaving it alone. Measured on tmux 3.4:
//
//	no file at all      error connecting to <path> (No such file or directory)
//	file, no server     no server running on <path>
//	a regular file      no server running on <path>
//	permission denied   error connecting to <path> (Permission denied)
//
// Only the two wordings that mean "there is definitely no server here" are
// classified stale. Everything else - a refusal, a timeout, a socket that
// something other than tmux is holding open - is unknown, which is the answer
// that does nothing.
func classifySocketFailure(detail string) SocketState {
	switch {
	case strings.Contains(detail, "no server running"):
		return SocketStale
	case strings.Contains(detail, "error connecting to") &&
		strings.Contains(detail, "No such file or directory"):
		return SocketStale
	default:
		return SocketUnknown
	}
}

// Reclaim removes a socket file that has been confirmed to have no server
// behind it.
//
// The confirmation is the point, and it is deliberately repeated. A single
// failed connection is not evidence: a server that is starting up, a machine
// under load, or a socket that was replaced between two calls all look like
// one. So the sequence is probe, wait, probe again, and only then unlink - and
// even then only if the path is still a socket file, because a path that has
// become something else is something else's problem.
//
// It reports whether a file was removed.
func (d *SocketDir) Reclaim(ctx context.Context, path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, nil
	}

	first := d.Probe(ctx, path)
	if first.State == SocketLive {
		return false, nil
	}
	if first.State == SocketUnknown {
		// Could not be classified, so it is not touched. An unreadable socket
		// may be a live server AgentMux cannot reach; unlinking it would not
		// stop that server, it would only make it unreachable forever.
		return false, nil
	}
	if first.State == SocketAbsent {
		return false, nil
	}

	// A settle before the second look. It is short because the race it guards
	// against - a server that has just been asked to start and has not yet
	// bound its socket - is measured in milliseconds, and because a
	// reconciliation that stalls on every project is worse than one that
	// leaves a socket for the next pass.
	if !sleepContext(ctx, socketReclaimSettle) {
		return false, nil
	}

	second := d.Probe(ctx, path)
	if second.State != SocketStale {
		return false, nil
	}

	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		// Something that is not a socket is sitting on the name. It is in a
		// directory only AgentMux writes to, so this is worth a human looking
		// at rather than a silent unlink.
		return false, wrapError(nil, CodeBackendFailure,
			"the tmux socket path %q exists but is not a socket (%s); leaving it alone", path, info.Mode())
	}

	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, wrapError(err, CodeBackendFailure, "could not remove the stale tmux socket %q", path)
	}
	return true, nil
}

// socketReclaimSettle is how long a stale socket is left alone before it is
// confirmed stale and removed.
const socketReclaimSettle = 250 * time.Millisecond

// SocketLayout is where project runtimes live, as the runtime manager sees it.
//
// It is an interface so that the manager's reconciliation can be tested
// against the interesting cases - a live server, a missing one, a stale
// socket, an orphan - without a tmux installation or a filesystem that has
// them.
type SocketLayout interface {
	// Dir is the socket directory.
	Dir() string

	// Path is a project's socket path, or "" when the identifier is not one
	// AgentMux issued.
	Path(projectID string) string

	// List returns the socket files present.
	List() ([]SocketFile, error)

	// Probe asks what is at a socket path.
	Probe(ctx context.Context, path string) SocketProbe

	// Reclaim removes a socket file that has been confirmed to have no server
	// behind it, reporting whether it removed one.
	Reclaim(ctx context.Context, path string) (bool, error)
}
