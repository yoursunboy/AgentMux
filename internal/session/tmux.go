package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// Built-in tmux defaults. Every one of them can be overridden by configuration;
// none of them may be duplicated elsewhere.
const (
	// DefaultTmuxBinary is the tmux executable.
	//
	// A bare name is resolved on PATH; an absolute path is used as given. Both
	// are supported because both are real: a distribution install is a name,
	// and a tmux built side by side with the system one into a user prefix is
	// a path. Whatever this resolves to is the binary for every operation
	// AgentMux performs, versions and dependency probes included - a machine
	// with two tmux installations must not be measured with one and driven
	// with the other.
	DefaultTmuxBinary = "tmux"

	// DefaultTmuxSocketDirName is the directory under the data directory that
	// holds one socket per project. See socket.go.
	DefaultTmuxSocketDirName = "tmux"

	// DefaultTmuxConfig is the server configuration file.
	//
	// /dev/null means no configuration, which is the point: a user's
	// ~/.tmux.conf cannot change how AgentMux's sessions behave, and a bug
	// report about those sessions is reproducible. The cost is that a user's
	// tmux preferences do not apply here, which is a deliberate trade.
	DefaultTmuxConfig = "/dev/null"

	// DefaultTmuxTerminal is the TERM set inside AgentMux's sessions.
	//
	// 256 colours is the floor for a modern TUI. screen-256color is chosen
	// over tmux-256color because the latter's terminfo entry is not present on
	// every distribution AgentMux may be installed on, and a TERM whose
	// terminfo cannot be found degrades a program to its dumbest output.
	DefaultTmuxTerminal = "screen-256color"

	// DefaultTmuxHistoryLimit is the scrollback tmux itself keeps per pane.
	//
	// It is separate from the runtime's own output buffer, and it is what
	// makes a reattached client able to show what it missed.
	DefaultTmuxHistoryLimit = 50000

	// DefaultSnapshotHistory is how many lines above the visible pane a
	// snapshot carries.
	//
	// It is what makes a freshly connected terminal scrollable rather than a
	// single screenful: everything the agent said before this client arrived
	// is in tmux's scrollback and nowhere else - the runtime's own buffer is
	// bounded and belongs to the live stream, not to the screen.
	//
	// It is deliberately much smaller than DefaultTmuxHistoryLimit. A snapshot
	// is sent once per connection and re-sent on every resync, so its size is
	// a reconnect cost, and two hundred lines is already more than a person
	// scrolls back through on a tablet. Zero means the visible pane only.
	DefaultSnapshotHistory = 200

	// tmuxMinVersion is the oldest tmux AgentMux supports.
	//
	// It is a real floor rather than caution: the runtime resizes with
	// `resize-window -x -y` and depends on the control-mode `%output` escaping
	// described in control.go, both of which are 3.x behaviour.
	tmuxMinVersion = 3

	// tmuxCommandTimeout bounds a single tmux command.
	tmuxCommandTimeout = 15 * time.Second

	// sendKeysChunkBytes is how many bytes one send-keys invocation carries.
	//
	// Every byte becomes a two-character hex argument, so this is also a limit
	// on argv size. It is small enough to stay far from any command-line limit
	// and large enough that a paste is a handful of invocations.
	sendKeysChunkBytes = 256
)

// TmuxOptions configures a TmuxBackend. Zero values mean the defaults above.
type TmuxOptions struct {
	Binary string

	// SocketPath is the tmux socket this backend owns: one per project, so
	// that this project's server is this project's alone.
	//
	// It is a path rather than a socket name because a name is resolved
	// against a shared directory that the environment decides, and isolation
	// that depends on the environment is not isolation. An empty path is
	// refused by every operation that would touch a server - see run - so a
	// misconfigured backend fails loudly instead of quietly becoming a guest
	// in the user's own tmux.
	SocketPath string

	Config   string
	Terminal string

	// HistoryLimit is the per-pane scrollback tmux keeps.
	HistoryLimit int

	// SnapshotHistory is how many lines above the visible pane a Snapshot
	// carries. Zero means the defaults above; a negative value means the
	// visible pane only.
	SnapshotHistory int

	// Prefix is the session-name namespace this backend owns. Sessions named
	// otherwise are ignored by List and never destroyed.
	Prefix string

	// Install is the tmux binary and what it says about itself, shared with
	// every other backend running the same binary. Nil means this backend
	// probes on its own.
	Install *tmuxInstall

	Logger *slog.Logger
}

// tmuxInstall is one tmux binary and its self-reported version.
//
// The version is cached here rather than per backend because every project's
// backend runs the same binary: without this, a machine hosting twenty
// projects would run `tmux -V` twenty times to learn one fact. The cache is
// also what keeps the version honest - the answer reported by diagnostics is
// the answer of the binary that actually runs the commands.
type tmuxInstall struct {
	bin string

	once sync.Once
	text string
	err  error
}

// newTmuxInstall returns an installation whose version is already known, for
// callers that probed it themselves.
func newTmuxInstall(bin, version string, err error) *tmuxInstall {
	install := &tmuxInstall{bin: bin}
	install.once.Do(func() {
		install.text, install.err = version, err
	})
	return install
}

// version returns the binary's version, probing at most once.
func (i *tmuxInstall) version(ctx context.Context) (string, error) {
	i.once.Do(func() {
		out, err := exec.CommandContext(ctx, i.bin, "-V").Output()
		if err != nil {
			i.err = wrapError(err, CodeBackendUnavailable, "tmux is present but could not be run")
			return
		}
		// "tmux 3.4"
		i.text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "tmux "))
	})
	return i.text, i.err
}

// TmuxStatus describes the tmux installation AgentMux will use. It is a
// diagnostic, and it reports the binary rather than assuming one: on a machine
// with two tmux installations "3.4" is not a measurement, "3.4 at /usr/bin/tmux"
// is.
type TmuxStatus struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Binary    string `json:"binary,omitempty"`
	SocketDir string `json:"socketDir,omitempty"`

	// MinimumVersion is the oldest tmux AgentMux will run on. It is a
	// requirement and it is enforced, which is what makes it different from a
	// recommendation.
	//
	// There is deliberately no RecommendedVersion field. Both versions
	// measured on both platforms were completely stable (see the compatibility
	// matrix in docs/RUNTIME.md), so naming a preferred one would be a
	// preference dressed up as a finding - and a field that exists is a field
	// a UI will render and a user will act on.
	MinimumVersion string `json:"minimumVersion,omitempty"`

	// Error explains why tmux is not usable, when it is not.
	Error string `json:"error,omitempty"`
}

// TmuxBackend is the SessionBackend implemented on tmux.
//
// tmux was chosen because the persistence AgentMux needs is exactly the
// property tmux already has: a server process that owns PTYs and outlives
// every client, with a control protocol for reading them. Reimplementing that
// on forkpty would mean owning process groups, terminal modes, and reattach
// for a result that already exists.
//
// # How commands are sent
//
// Output travels over a long-lived control-mode client (control.go). Commands
// - create, send-keys, resize, kill - are one-off tmux invocations instead.
//
// That split is deliberate. A command issued through the control client would
// have to be matched to its response by parsing `%begin`/`%end` blocks, and
// its failure would arrive asynchronously, long after the HTTP request that
// caused it returned. A one-off invocation has an exit code and a stderr, so a
// failure to resize is reported to the caller that asked for it. The cost is
// one short-lived process per command, which interactive typing does not
// notice and which buys a much simpler correctness story.
type TmuxBackend struct {
	install         *tmuxInstall
	socketPath      string
	config          string
	terminal        string
	historyLimit    int
	snapshotHistory int

	// prefix is the namespace AgentMux owns. It comes from project.SessionPrefix
	// rather than a literal here, so that the name a session is created with
	// and the name it is looked up by have one definition between them. A
	// second spelling of "amx-" is how a project ends up with two sessions.
	//
	// List filters on it and Destroy refuses names outside it, so a session
	// AgentMux did not create is never reported as its own and never killed.
	prefix string

	log *slog.Logger

	// subs is every live subscription this process holds. Each one is its own
	// control client, so one caller closing its stream cannot interrupt
	// another's; the set exists so that Close and Destroy can detach them all.
	mu   sync.Mutex
	subs map[*tmuxSubscription]struct{}
}

// NewTmuxBackend builds the tmux backend.
func NewTmuxBackend(o TmuxOptions) *TmuxBackend {
	install := o.Install
	if install == nil {
		install = &tmuxInstall{bin: orDefault(o.Binary, DefaultTmuxBinary)}
	}
	b := &TmuxBackend{
		install:         install,
		socketPath:      strings.TrimSpace(o.SocketPath),
		config:          orDefault(o.Config, DefaultTmuxConfig),
		terminal:        orDefault(o.Terminal, DefaultTmuxTerminal),
		historyLimit:    o.HistoryLimit,
		snapshotHistory: o.SnapshotHistory,
		prefix:          orDefault(o.Prefix, project.SessionPrefix),
		log:             o.Logger,
		subs:            make(map[*tmuxSubscription]struct{}),
	}
	if b.historyLimit <= 0 {
		b.historyLimit = DefaultTmuxHistoryLimit
	}
	// Zero means "unset" and a negative value means "none", which are different
	// answers: the first is a caller that did not say, the second is one that
	// did.
	if b.snapshotHistory == 0 {
		b.snapshotHistory = DefaultSnapshotHistory
	}
	if b.log == nil {
		b.log = slog.New(slog.DiscardHandler)
	}
	return b
}

// orDefault returns value, or fallback when value is blank.
func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// Name implements Backend.
func (b *TmuxBackend) Name() string { return "tmux" }

// SocketPath reports the socket this backend owns.
//
// It exists for diagnostics and for tests that need to reach the same server
// directly. Every one of AgentMux's own operations goes through args, which
// uses it, so this is a reader and not a second source of truth.
func (b *TmuxBackend) SocketPath() string { return b.socketPath }

// Binary reports the tmux executable this backend runs, with a bare name
// resolved against PATH.
//
// It reports the configured value unchanged when nothing resolves, because the
// useful answer there is the name that failed rather than an empty string that
// hides what was asked for. Diagnostics and the stability harness both record
// this: a measurement that says "tmux 3.4" without saying which tmux is a
// measurement that cannot be reproduced on a machine with two installed.
func (b *TmuxBackend) Binary() string {
	if path, err := exec.LookPath(b.install.bin); err == nil {
		return path
	}
	return b.install.bin
}

// Available implements Backend.
//
// The check runs tmux rather than only looking for it, because a binary on
// PATH that cannot execute is not an available runtime, and because the
// version decides whether control mode and resize behave as this backend
// assumes.
func (b *TmuxBackend) Available(ctx context.Context) error {
	if err := b.requireSocket(); err != nil {
		return err
	}
	path, err := exec.LookPath(b.install.bin)
	if err != nil {
		return wrapError(err, CodeBackendUnavailable,
			"tmux is not installed in the environment where the AgentMux server runs, "+
				"so no terminal session can be started")
	}
	version, err := b.install.version(ctx)
	if err != nil {
		return err
	}
	major, ok := parseTmuxMajor(version)
	if ok && major < tmuxMinVersion {
		return newError(CodeBackendUnavailable,
			"tmux %s at %s is too old; AgentMux needs tmux %d.0 or newer",
			version, path, tmuxMinVersion)
	}
	// A version that cannot be read is not evidence of being too old, so it is
	// allowed through. Refusing here would take the runtime away from a user
	// whose tmux is merely unusual, and the operations this backend depends on
	// fail loudly on their own if they are genuinely missing.
	return nil
}

// requireSocket refuses an operation that would otherwise address a tmux
// server AgentMux does not own.
//
// An empty socket path does not mean "no socket"; to tmux it means the user's
// default one, where their own sessions live. Creating, resizing, typing into,
// or destroying on that socket would make AgentMux a guest in somebody else's
// server with the ability to disturb their work - and it would silently break
// the isolation this whole phase is built on. So it is refused here, once, at
// the single point every server-addressing command passes through.
func (b *TmuxBackend) requireSocket() error {
	if b.socketPath == "" {
		return newError(CodeBackendUnavailable,
			"the tmux backend has no socket path; every AgentMux runtime owns its own "+
				"tmux server, and an empty socket path would address the user's own")
	}
	return nil
}

// parseTmuxMajor reads the major version from a version like "3.4" or "3.4a".
//
// The digits are searched for rather than assumed to be at the front, because a
// tmux built from its development branch reports a name rather than a number -
// "next-3.5" - and refusing to read that would disable the runtime on exactly
// the installations most likely to be current.
func parseTmuxMajor(version string) (int, bool) {
	start := strings.IndexFunc(version, func(r rune) bool { return r >= '0' && r <= '9' })
	if start < 0 {
		return 0, false
	}
	rest := version[start:]
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(rest)
	}
	major, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return major, true
}

// args builds a tmux command line with this backend's global flags.
//
// -u forces UTF-8 handling regardless of the locale the server was started
// with, which is what makes multibyte output survive. It is set here, on every
// invocation, rather than left to the environment: terminal fidelity must not
// depend on how somebody happened to launch the server.
//
// -S names the socket, and it is the flag that makes one project one server.
// It appears on every invocation for the same reason -u does: an operation
// that forgot it would address a different server than the one it was meant
// for, and nothing about the result would look wrong.
func (b *TmuxBackend) args(sub ...string) []string {
	args := []string{"-u"}
	if b.socketPath != "" {
		args = append(args, "-S", b.socketPath)
	}
	if b.config != "" {
		args = append(args, "-f", b.config)
	}
	return append(args, sub...)
}

// run executes one tmux command and returns its combined output.
//
// It is the single choke point for every command that talks to a server, and
// it refuses to run at all without a socket path. Putting the check here
// rather than in each method is what makes "every path uses this project's own
// socket" a property of the code instead of a rule somebody has to remember:
// there is no way to spell a tmux invocation in this package that skips it.
func (b *TmuxBackend) run(ctx context.Context, sub ...string) (string, error) {
	if err := b.requireSocket(); err != nil {
		return "", err
	}
	return runTmux(ctx, b.install.bin, b.args(sub...)...)
}

// runTmux executes one tmux command line and returns its combined output.
//
// It is a free function because two callers need it and neither owns the
// other: a backend runs commands for one project, and a socket probe runs a
// read-only listing for a socket that may belong to no live runtime at all.
func runTmux(ctx context.Context, bin string, args ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tmuxCommandTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	cmd.Env = tmuxEnv()
	err := cmd.Run()
	return combined.String(), err
}

// runChecked executes a command and turns a failure into a coded error.
func (b *TmuxBackend) runChecked(ctx context.Context, what string, sub ...string) (string, error) {
	out, err := b.run(ctx, sub...)
	if err == nil {
		return out, nil
	}
	trimmed := strings.TrimSpace(out)
	switch {
	case isMissingTarget(trimmed):
		return "", fmt.Errorf("%w: %s", ErrNoSuchSession, trimmed)
	case isDuplicateSession(trimmed):
		return "", fmt.Errorf("%w: %s", ErrSessionExists, trimmed)
	}
	if trimmed == "" {
		return "", wrapError(err, CodeBackendFailure, "tmux %s failed", what)
	}
	return "", wrapError(err, CodeBackendFailure, "tmux %s failed: %s", what, trimmed)
}

// tmuxEnv is the environment tmux commands run with.
//
// C.UTF-8 is set explicitly because tmux decides whether to treat a terminal as
// UTF-8 from the locale, and a server started from a service manager or a
// minimal shell often has none. Without it, multibyte output is mangled before
// it ever reaches the control-mode stream.
func tmuxEnv() []string {
	env := os.Environ()
	env = append(env, "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TERM=xterm-256color")
	return env
}

// isMissingTarget reports whether tmux's complaint means "there is no such
// session" rather than "something went wrong".
//
// The complaint is not one shape, and it does not depend only on the state of
// the server - it depends on what kind of target the command asked for. All of
// the following were observed on tmux 3.4 with a live server and no session
// called amx-missing:
//
//	has-session -t       can't find session: amx-missing
//	kill-session -t      can't find session: amx-missing
//	list-panes -t        can't find window: amx-missing
//	resize-window -t     can't find window: amx-missing
//	capture-pane -t      can't find pane: amx-missing
//	send-keys -t         can't find pane: amx-missing
//
// and, with no server at all, "error connecting to <socket> (No such file or
// directory)", or "no server running on <socket>" for a socket left behind by a
// server that has exited.
//
// A bare name is parsed as a window first and only then as a session, which is
// why list-panes complains about a window. That ambiguity cannot bite here:
// every target this backend builds is a whole session name carrying the
// amx- prefix, so "there is no window by that name" and "there is no session by
// that name" are the same statement about the same string.
//
// This function has been wrong twice for the same reason - matching only the
// messages a convenient test produced. The first version missed the missing
// socket; the version after it missed the window and pane wordings, which meant
// that creating a second session while a tmux server was already running failed
// with a backend error, because Inspect could not tell "not there yet" from
// "broken". Both are covered by tests now: one against a socket with no server,
// one against a live server holding somebody else's session.
//
// The errno is matched together with the phrase so that a socket the server can
// see but not open - a permission problem, worded differently - still surfaces
// as a failure. Telling a user their session does not exist when the real
// problem is that AgentMux cannot reach the runtime would hide the one fact
// they need.
func isMissingTarget(text string) bool {
	if strings.Contains(text, "error connecting to") &&
		strings.Contains(text, "No such file or directory") {
		return true
	}
	for _, marker := range []string{
		"can't find session",
		"can't find window",
		"can't find pane",
		"session not found",
		"no such session",
		"no server running",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func isDuplicateSession(text string) bool {
	return strings.Contains(text, "duplicate session")
}

// Create implements Backend.
func (b *TmuxBackend) Create(ctx context.Context, spec SessionSpec) (*Session, error) {
	if err := b.validateName(spec.Name); err != nil {
		return nil, err
	}
	if err := b.Available(ctx); err != nil {
		return nil, err
	}
	if err := ensureDirectory(spec.Dir); err != nil {
		return nil, err
	}

	cols, rows := normalizedSize(spec.Cols, spec.Rows)

	// Server bootstrap, global options, and the new session are one tmux command
	// line, and they have to be.
	//
	// `set-option -g` cannot start a server, and `start-server` on its own does
	// not help: a server with no sessions exits at once (exit-empty), so the
	// options in the probe ran against a socket that was already dead and were
	// silently lost. Sent as one line, a single client holds the server open
	// across the whole sequence.
	//
	// The order is not cosmetic either. default-terminal and history-limit are
	// read when a pane is created, so options applied after new-session would
	// leave the session's first pane - the only pane AgentMux uses - with tmux's
	// own defaults. Applied before it, the probe read TERM back from inside the
	// pane as screen-256color.
	sub := []string{"start-server", ";"}
	sub = append(sub, configCommand(b.terminal, b.historyLimit)...)
	sub = append(sub, ";", "new-session", "-d", "-s", spec.Name, "-c", spec.Dir,
		"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows))
	sub = append(sub, spec.Command...)
	if _, err := b.runChecked(ctx, "new-session", sub...); err != nil {
		if errors.Is(err, ErrSessionExists) {
			return nil, err
		}
		return nil, wrapError(err, CodeStartFailed, "could not create session %q", spec.Name)
	}

	// The canonical size is AgentMux's decision and must not move when a client
	// attaches. `window-size manual` is what stops tmux resizing the window to
	// fit whoever connected; the resize that follows states the size rather than
	// relying on the size new-session asked for, which a client attaching
	// between the two lines could already have changed.
	if _, err := b.runChecked(ctx, "resize-window", resizeCommand(spec.Name, cols, rows)...); err != nil {
		// Leaving a session behind that no caller knows about would be worse
		// than failing loudly.
		_, _ = b.run(ctx, "kill-session", "-t", spec.Name)
		return nil, err
	}

	return b.Inspect(ctx, spec.Name)
}

// configCommand is the server-wide option setup, sent as one tmux command line.
// The ";" arguments are tmux's own command separator: without a shell in the
// middle they reach tmux literally.
func configCommand(terminal string, historyLimit int) []string {
	return []string{
		"set-option", "-g", "history-limit", strconv.Itoa(historyLimit), ";",
		"set-option", "-g", "default-terminal", terminal, ";",
		// A session must not die because its last client detached. It is
		// already off by default with -f /dev/null, and stating it means a
		// future configuration change cannot silently break persistence.
		"set-option", "-g", "destroy-unattached", "off",
	}
}

// resizeCommand takes ownership of a session's geometry and sets it.
//
// Both halves are one command line for the same reason the bootstrap is: the
// window-size option is the precondition that makes the resize stick, and a
// resize that silently does nothing is the worst possible outcome for this
// operation.
func resizeCommand(name string, cols, rows int) []string {
	return []string{
		"set-option", "-t", name, "window-size", "manual", ";",
		"resize-window", "-t", name, "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows),
	}
}

// validateName refuses to touch a session AgentMux does not own.
func (b *TmuxBackend) validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return newError(CodeInvalidInput, "session name is empty")
	}
	if !strings.HasPrefix(name, b.prefix) {
		return newError(CodeInvalidInput,
			"session name %q is outside the %q namespace", name, b.prefix)
	}
	return nil
}

// ensureDirectory checks the session's working directory before tmux is asked
// to use it, so the error names the path instead of tmux's phrasing.
func ensureDirectory(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return newError(CodeStartFailed,
			"the session has no working directory; a project's runtime path is required")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return wrapError(err, CodeStartFailed, "the session working directory %q is not usable", dir)
	}
	if !info.IsDir() {
		return newError(CodeStartFailed, "the session working directory %q is not a directory", dir)
	}
	return nil
}

// normalizedSize clamps a requested size into something tmux will accept.
func normalizedSize(cols, rows int) (int, int) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return cols, rows
}

// Exists implements Backend.
func (b *TmuxBackend) Exists(ctx context.Context, name string) (bool, error) {
	return b.sessionExists(ctx, name), nil
}

// sessionExists is the internal form used where an error cannot be handled.
//
// The exit code alone is the answer. Every way tmux has of saying "no" here -
// no server, no such socket, no session by that name - means the same thing to
// the caller, which is why this asks for a boolean and does not classify the
// message.
func (b *TmuxBackend) sessionExists(ctx context.Context, name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	_, err := b.run(ctx, "has-session", "-t", name)
	return err == nil
}

// Inspect implements Backend.
func (b *TmuxBackend) Inspect(ctx context.Context, name string) (*Session, error) {
	out, err := b.run(ctx, "list-panes", "-t", name, "-F", sessionListFormat)
	if err != nil {
		if isMissingTarget(out) {
			return nil, fmt.Errorf("%w: %s", ErrNoSuchSession, name)
		}
		return nil, wrapError(err, CodeBackendFailure, "could not inspect session %q", name)
	}
	line := firstLine(out)
	if line == "" {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchSession, name)
	}
	sess, ok := parseSessionLine(line)
	if !ok {
		return nil, newError(CodeBackendFailure, "unexpected session description for %q: %q", name, line)
	}
	return sess, nil
}

// sessionFieldSeparator joins the fields of one session description.
//
// The obvious choice is a control character, on the grounds that the last field
// is a filesystem path and a path may contain any printable character. tmux
// rules that out: it escapes non-printable bytes in the output of a -F format,
// so a 0x1f separator in the format string comes back as the four characters
// `\037` and the description never splits. Measured on tmux 3.4, the output
// encoding is `\ooo` for most non-printables, the C names `\t \v \f \r` for
// those four, and a literal backslash left alone - which is not reversible, so
// decoding it would corrupt a path that genuinely contains a backslash.
//
// A printable separator is safe here for a different reason: the fields before
// the path are a session name this package generates and three integers, so no
// path can ever contain one. The path is the final field and the split is
// bounded, so a path containing the separator still arrives intact.
const sessionFieldSeparator = "|"

// sessionListFormat is the -F format that describes one session per line.
const sessionListFormat = "#{session_name}" + sessionFieldSeparator +
	"#{session_created}" + sessionFieldSeparator +
	"#{window_width}" + sessionFieldSeparator +
	"#{window_height}" + sessionFieldSeparator +
	"#{pane_current_path}"

// List implements Backend.
func (b *TmuxBackend) List(ctx context.Context) ([]*Session, error) {
	if err := b.requireSocket(); err != nil {
		return nil, err
	}
	sessions, detail, err := listSessionsOn(ctx, b.install.bin, b.socketPath)
	if err != nil {
		// No server is not a failure: it means no sessions exist, which is the
		// answer List was asked for.
		if isMissingTarget(detail) {
			return nil, nil
		}
		// What tmux said is carried on the error, as runChecked does. "could not
		// list sessions: exit status 1" on its own names the symptom and hides
		// the cause, which is the one thing the operator cannot recover.
		if detail == "" {
			return nil, wrapError(err, CodeBackendFailure, "could not list sessions")
		}
		return nil, wrapError(err, CodeBackendFailure, "could not list sessions: %s", detail)
	}

	seen := make(map[string]bool)
	var owned []*Session
	for _, sess := range sessions {
		if !strings.HasPrefix(sess.Name, b.prefix) {
			continue
		}
		// One entry per session: a session with several panes is reported
		// once, by its first pane.
		if seen[sess.Name] {
			continue
		}
		seen[sess.Name] = true
		owned = append(owned, sess)
	}
	return owned, nil
}

// listSessionsOn asks one tmux socket for every session on it.
//
// It is a free function rather than a method because a socket probe needs it
// too, and a socket being probed may belong to no live runtime at all - there
// is no backend to ask.
//
// It reports tmux's own words alongside the error. The difference between
// "there is no server here" and "something is wrong here" is a difference in
// wording and not in exit code, and one of those two answers leads to deleting
// a file.
func listSessionsOn(ctx context.Context, bin, socketPath string) ([]*Session, string, error) {
	args := []string{"-u", "-S", socketPath, "-f", DefaultTmuxConfig,
		"list-panes", "-a", "-F", sessionListFormat}
	out, err := runTmux(ctx, bin, args...)
	detail := strings.TrimSpace(out)
	if err != nil {
		// A live server with no sessions is neither a failure nor an absence.
		// tmux says "no current target" and exits non-zero. Reading that as an
		// error would make a project's socket look broken in the window
		// between its last session being killed and its server exiting - which
		// is precisely the window a reconciliation runs in.
		if isNoSessions(detail) {
			return nil, detail, nil
		}
		return nil, detail, err
	}
	return parseSessionList(out), detail, nil
}

// isNoSessions reports whether tmux's complaint means "there is a server and
// it has nothing on it".
//
// Measured on tmux 3.4: `list-sessions` against a server with exit-empty off
// and no sessions prints nothing and exits 0, while `list-panes -a` prints
// "no current target" and exits 1.
func isNoSessions(text string) bool {
	return strings.Contains(text, "no current target") ||
		strings.Contains(text, "no sessions")
}

// parseSessionList reads every line of a listing.
func parseSessionList(out string) []*Session {
	var sessions []*Session
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if sess, ok := parseSessionLine(line); ok {
			sessions = append(sessions, sess)
		}
	}
	return sessions
}

// parseSessionLine reads one description produced by List or Inspect.
func parseSessionLine(line string) (*Session, bool) {
	// The split is limited so that the last field - a filesystem path - keeps
	// any separator characters it happens to contain.
	parts := strings.SplitN(line, sessionFieldSeparator, 5)
	if len(parts) < 5 {
		return nil, false
	}
	created, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return nil, false
	}
	cols, err := strconv.Atoi(strings.TrimSpace(parts[2]))
	if err != nil {
		return nil, false
	}
	rows, err := strconv.Atoi(strings.TrimSpace(parts[3]))
	if err != nil {
		return nil, false
	}
	return &Session{
		Name:    parts[0],
		Created: time.Unix(created, 0),
		Cols:    cols,
		Rows:    rows,
		Dir:     parts[4],
	}, true
}

// firstLine returns the first non-empty line of out.
func firstLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

// Launch implements Backend.
//
// The command is typed into the session's shell rather than executed. That is
// what makes it visible, interruptible, and part of the scrollback - an agent
// the user cannot see or Ctrl-C is not a terminal session.
func (b *TmuxBackend) Launch(ctx context.Context, name string, command string) error {
	if strings.ContainsAny(command, "\r\n") {
		return newError(CodeInvalidInput, "a launch command must be a single line")
	}
	return b.SendInput(ctx, name, []byte(command+"\r"))
}

// SendInput implements Backend.
//
// Input travels as hex (`send-keys -H`) rather than as text. tmux's own
// argument syntax would otherwise have to be quoted correctly for arbitrary
// bytes - and it cannot be, for a byte sequence that is not text at all.
// Hex sidesteps quoting entirely: every byte becomes two characters from a
// sixteen-letter alphabet, so an escape sequence, a UTF-8 character, and a
// Ctrl-C are all delivered by exactly the same mechanism.
func (b *TmuxBackend) SendInput(ctx context.Context, name string, data []byte) error {
	if err := b.validateName(name); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if !b.sessionExists(ctx, name) {
		return fmt.Errorf("%w: %s", ErrNoSuchSession, name)
	}

	for start := 0; start < len(data); start += sendKeysChunkBytes {
		end := min(start+sendKeysChunkBytes, len(data))
		sub := make([]string, 0, 5+(end-start))
		sub = append(sub, "send-keys", "-H", "-t", name)
		sub = append(sub, hexArgs(data[start:end])...)
		if _, err := b.runChecked(ctx, "send-keys", sub...); err != nil {
			return wrapError(err, CodeInputFailed, "could not deliver input to session %q", name)
		}
	}
	return nil
}

// hexArgs renders bytes as the two-character hex arguments send-keys -H wants.
func hexArgs(data []byte) []string {
	const digits = "0123456789abcdef"
	out := make([]string, len(data))
	for i, c := range data {
		out[i] = string([]byte{digits[c>>4], digits[c&0x0f]})
	}
	return out
}

// Resize implements Backend.
func (b *TmuxBackend) Resize(ctx context.Context, name string, cols, rows int) error {
	if err := b.validateName(name); err != nil {
		return err
	}
	if cols <= 0 || rows <= 0 {
		return newError(CodeInvalidSize, "terminal size %dx%d is not usable", cols, rows)
	}
	if !b.sessionExists(ctx, name) {
		return fmt.Errorf("%w: %s", ErrNoSuchSession, name)
	}
	if _, err := b.runChecked(ctx, "resize-window", resizeCommand(name, cols, rows)...); err != nil {
		return wrapError(err, CodeResizeFailed, "could not resize session %q to %dx%d", name, cols, rows)
	}
	return nil
}

// Stop implements Backend.
//
// It interrupts the foreground process - the equivalent of Ctrl-C - and leaves
// the session, its shell, and its scrollback in place. "Stopped" therefore
// means nothing is running, not that the terminal is gone; Destroy is the call
// that ends a session.
func (b *TmuxBackend) Stop(ctx context.Context, name string) error {
	if err := b.validateName(name); err != nil {
		return err
	}
	if !b.sessionExists(ctx, name) {
		return fmt.Errorf("%w: %s", ErrNoSuchSession, name)
	}
	if _, err := b.runChecked(ctx, "send-keys", "send-keys", "-t", name, "C-c"); err != nil {
		return wrapError(err, CodeStopFailed, "could not interrupt session %q", name)
	}
	return nil
}

// Destroy implements Backend.
func (b *TmuxBackend) Destroy(ctx context.Context, name string) error {
	if err := b.validateName(name); err != nil {
		return err
	}
	// Any subscription this process holds is closed first: a control client
	// attached to a session that is being killed would otherwise spend its
	// reconnect loop rediscovering that the session is gone.
	b.closeSubscriptions(name)

	out, err := b.run(ctx, "kill-session", "-t", name)
	if err != nil {
		if isMissingTarget(out) {
			// Already gone is the desired end state.
			return nil
		}
		return wrapError(err, CodeDestroyFailed, "could not destroy session %q", name)
	}
	return nil
}

// paneScreenFormat is the list-panes format that describes a screen.
//
// It is read in one call so the four facts cannot disagree with each other: a
// cursor read separately from the geometry could be a cursor position in a
// pane that has since been resized.
const paneScreenFormat = "#{cursor_x}" + sessionFieldSeparator +
	"#{cursor_y}" + sessionFieldSeparator +
	"#{pane_width}" + sessionFieldSeparator +
	"#{pane_height}" + sessionFieldSeparator +
	"#{alternate_on}"

// Snapshot implements Backend.
//
// capture-pane is used here and only here. It answers "what is on the screen
// right now", which is exactly the question a client that has just connected
// needs answered once. It is not how live output is obtained - that is what
// control mode is for - because a snapshot cannot see between two calls.
//
// # The flags, and why each one
//
//	-p   print to stdout instead of writing to a buffer
//	-e   include escape sequences, so colours and attributes survive
//	-N   preserve trailing spaces, so a coloured region reaches the edge of
//	     the pane instead of stopping at the last character on the line
//	-S   start this many lines above the visible pane, so the client receives
//	     the recent scrollback it could not otherwise get
//
// -J is deliberately absent. It joins wrapped lines, which is the opposite of
// what a terminal needs: a row that wrapped is two rows on the screen, and
// joining them would produce one line that no longer fits the pane it came
// from.
//
// The result is not written to a client unchanged - rowsToCRLF explains why -
// and it is not the whole of a terminal's state. What it does not carry is
// listed in docs/TERMINAL.md.
func (b *TmuxBackend) Snapshot(ctx context.Context, name string) (Screen, error) {
	if !b.sessionExists(ctx, name) {
		return Screen{}, fmt.Errorf("%w: %s", ErrNoSuchSession, name)
	}

	// The geometry and the cursor are read first so that they describe a pane
	// at or before the moment the content was captured. A cursor read after
	// the content could belong to a screen the content does not show.
	described, err := b.run(ctx, "list-panes", "-t", name, "-F", paneScreenFormat)
	if err != nil {
		if isMissingTarget(described) {
			return Screen{}, fmt.Errorf("%w: %s", ErrNoSuchSession, name)
		}
		return Screen{}, wrapError(err, CodeBackendFailure,
			"could not read the screen geometry of session %q", name)
	}
	cols, rows, cursorX, cursorY, alt, err := parsePaneScreen(firstLine(described))
	if err != nil {
		return Screen{}, err
	}

	args := []string{"capture-pane", "-p", "-e", "-N", "-t", name}
	if b.snapshotHistory > 0 {
		args = append(args, "-S", "-"+strconv.Itoa(b.snapshotHistory))
	}
	captured, err := b.run(ctx, args...)
	if err != nil {
		if isMissingTarget(captured) {
			return Screen{}, fmt.Errorf("%w: %s", ErrNoSuchSession, name)
		}
		return Screen{}, wrapError(err, CodeBackendFailure, "could not capture session %q", name)
	}

	return Screen{
		Data:      rowsToCRLF([]byte(captured)),
		Cols:      cols,
		Rows:      rows,
		CursorX:   cursorX,
		CursorY:   cursorY,
		Alternate: alt,
	}, nil
}

// parsePaneScreen decodes the paneScreenFormat line.
//
// Every field is parsed rather than assumed. A tmux that answers a format
// question with something unexpected is a tmux whose screen should not be
// drawn from a guess, and the error names the line it could not read.
func parsePaneScreen(line string) (cols, rows, cursorX, cursorY int, alternate bool, err error) {
	parts := strings.Split(line, sessionFieldSeparator)
	if len(parts) != 5 {
		return 0, 0, 0, 0, false, newError(CodeBackendFailure,
			"unexpected pane screen description: %q", line)
	}
	numbers := make([]int, 4)
	for i, field := range []string{parts[0], parts[1], parts[2], parts[3]} {
		value, convErr := strconv.Atoi(strings.TrimSpace(field))
		if convErr != nil {
			return 0, 0, 0, 0, false, newError(CodeBackendFailure,
				"unexpected value %q in pane screen description %q", field, line)
		}
		numbers[i] = value
	}
	cursorX, cursorY, cols, rows = numbers[0], numbers[1], numbers[2], numbers[3]
	alternate = strings.TrimSpace(parts[4]) == "1"

	if cols <= 0 || rows <= 0 {
		return 0, 0, 0, 0, false, newError(CodeBackendFailure,
			"pane screen description reports a %dx%d pane: %q", cols, rows, line)
	}
	// A cursor outside the pane would place the client's cursor off-screen.
	// Clamping is not the same as correcting: tmux reports the cursor of the
	// visible pane, and a value outside it means the two calls disagreed about
	// which pane they were describing.
	if cursorX < 0 || cursorX >= cols || cursorY < 0 || cursorY >= rows {
		return 0, 0, 0, 0, false, newError(CodeBackendFailure,
			"pane screen description places the cursor at %d,%d in a %dx%d pane: %q",
			cursorX, cursorY, cols, rows, line)
	}
	return cols, rows, cursorX, cursorY, alternate, nil
}

// PaneProcess implements ProcessInspector.
//
// It reports the process the pane's terminal is running, which is how the
// runtime finds and watches a coding agent without reading a byte of what the
// terminal displays.
//
// The three fields are read in one call, and the path is last. That ordering is
// what makes the parse safe: a process id cannot contain the separator, and a
// mis-split caused by an unusual process name can only corrupt the final field.
// The failure mode is therefore a directory comparison that does not match,
// which refuses an agent start - it never lets one run in a directory nobody
// chose.
func (b *TmuxBackend) PaneProcess(ctx context.Context, name string) (PaneProcess, error) {
	if err := b.validateName(name); err != nil {
		return PaneProcess{}, err
	}
	if !b.sessionExists(ctx, name) {
		return PaneProcess{}, fmt.Errorf("%w: %s", ErrNoSuchSession, name)
	}

	format := "#{pane_pid}" + sessionFieldSeparator +
		"#{pane_current_command}" + sessionFieldSeparator +
		"#{pane_current_path}"
	out, err := b.run(ctx, "list-panes", "-t", name, "-F", format)
	if err != nil {
		if isMissingTarget(out) {
			return PaneProcess{}, fmt.Errorf("%w: %s", ErrNoSuchSession, name)
		}
		return PaneProcess{}, wrapError(err, CodeBackendFailure,
			"could not read the process of session %q", name)
	}

	line := firstLine(out)
	parts := strings.SplitN(line, sessionFieldSeparator, 3)
	if len(parts) < 3 {
		return PaneProcess{}, newError(CodeBackendFailure,
			"unexpected pane description for %q: %q", name, line)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return PaneProcess{}, newError(CodeBackendFailure,
			"unexpected process id in the pane description for %q: %q", name, parts[0])
	}
	return PaneProcess{
		PID:     pid,
		Command: strings.TrimSpace(parts[1]),
		Dir:     strings.TrimSpace(parts[2]),
	}, nil
}

// Attach implements Backend.
//
// Each caller gets its own control client. Sharing one stream between callers
// would make one of them closing it an outage for the others, and a
// subscription is cheap: it is a pipe and a goroutine, not a session.
func (b *TmuxBackend) Attach(ctx context.Context, name string) (Subscription, error) {
	if err := b.validateName(name); err != nil {
		return nil, err
	}
	if !b.sessionExists(ctx, name) {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchSession, name)
	}
	sub := newTmuxSubscription(ctx, b, name, b.log)
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()
	return sub, nil
}

// startControl launches one control-mode client for a session.
func (b *TmuxBackend) startControl(ctx context.Context, name string) (*controlStream, error) {
	if err := b.requireSocket(); err != nil {
		return nil, err
	}
	args := b.args("-C", "attach-session", "-t", name)
	return startControlStream(ctx, b.install.bin, args, name, b.log)
}

// Close implements Backend.
//
// It detaches every subscription and nothing else. Sessions are the point of
// the runtime and must survive the server that created them, so shutting the
// backend down is not allowed to be a shutdown of the work.
func (b *TmuxBackend) Close() error {
	for _, sub := range b.takeSubscriptions("") {
		_ = sub.Close()
	}
	return nil
}

// closeSubscriptions detaches this process's streams for one session. An empty
// name detaches all of them.
func (b *TmuxBackend) closeSubscriptions(name string) {
	for _, sub := range b.takeSubscriptions(name) {
		_ = sub.Close()
	}
}

// takeSubscriptions removes and returns the matching subscriptions.
func (b *TmuxBackend) takeSubscriptions(name string) []*tmuxSubscription {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*tmuxSubscription
	for sub := range b.subs {
		if name != "" && sub.Name() != name {
			continue
		}
		out = append(out, sub)
		delete(b.subs, sub)
	}
	return out
}

// KillServer stops the tmux server this backend talks to, destroying every
// session on it.
//
// Since Phase 2.5 this is a project-scoped operation rather than an
// installation-wide one: the backend owns one project's socket, so killing the
// server reaches that project's sessions and nothing else. It is still
// destructive and still not reachable from the API as a user action - Destroy
// removes one session, and Close deliberately leaves them all running - but it
// is now what makes Destroy complete, since a project's server is a project's
// resource.
func (b *TmuxBackend) KillServer(ctx context.Context) error {
	if err := b.requireSocket(); err != nil {
		return err
	}
	b.closeSubscriptions("")
	// Nothing to kill is a success, so the result is deliberately discarded.
	_, _ = b.run(ctx, "kill-server")
	return nil
}
