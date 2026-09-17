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
	DefaultTmuxBinary = "tmux"

	// DefaultTmuxSocket is the private socket AgentMux's sessions live on.
	//
	// A private socket is not tidiness, it is isolation. AgentMux creates,
	// resizes, interrupts, and destroys sessions; on the user's default socket
	// it would be a guest in somebody else's server, able to disturb their
	// work and be disturbed by it. The socket also makes the runtime
	// reproducible: a listing that shows AgentMux's sessions shows only those.
	DefaultTmuxSocket = "agentmux"

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
	Binary   string
	Socket   string
	Config   string
	Terminal string

	// HistoryLimit is the per-pane scrollback tmux keeps.
	HistoryLimit int

	// Prefix is the session-name namespace this backend owns. Sessions named
	// otherwise are ignored by List and never destroyed.
	Prefix string

	Logger *slog.Logger
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
	bin          string
	socket       string
	config       string
	terminal     string
	historyLimit int

	// prefix is the namespace AgentMux owns. It comes from project.SessionPrefix
	// rather than a literal here, so that the name a session is created with
	// and the name it is looked up by have one definition between them. A
	// second spelling of "amx-" is how a project ends up with two sessions.
	//
	// List filters on it and Destroy refuses names outside it, so a session
	// AgentMux did not create is never reported as its own and never killed.
	prefix string

	log *slog.Logger

	versionOnce sync.Once
	version     string
	versionErr  error

	// subs is every live subscription this process holds. Each one is its own
	// control client, so one caller closing its stream cannot interrupt
	// another's; the set exists so that Close and Destroy can detach them all.
	mu   sync.Mutex
	subs map[*tmuxSubscription]struct{}
}

// NewTmuxBackend builds the tmux backend.
func NewTmuxBackend(o TmuxOptions) *TmuxBackend {
	b := &TmuxBackend{
		bin:          orDefault(o.Binary, DefaultTmuxBinary),
		socket:       o.Socket,
		config:       orDefault(o.Config, DefaultTmuxConfig),
		terminal:     orDefault(o.Terminal, DefaultTmuxTerminal),
		historyLimit: o.HistoryLimit,
		prefix:       orDefault(o.Prefix, project.SessionPrefix),
		log:          o.Logger,
		subs:         make(map[*tmuxSubscription]struct{}),
	}
	if b.historyLimit <= 0 {
		b.historyLimit = DefaultTmuxHistoryLimit
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

// Socket reports the socket this backend uses, or "" when it uses the default
// one. It exists for diagnostics and for tests that need to tear down a tmux
// server they started.
func (b *TmuxBackend) Socket() string { return b.socket }

// Available implements Backend.
//
// The check runs tmux rather than only looking for it, because a binary on
// PATH that cannot execute is not an available runtime, and because the
// version decides whether control mode and resize behave as this backend
// assumes.
func (b *TmuxBackend) Available(ctx context.Context) error {
	path, err := exec.LookPath(b.bin)
	if err != nil {
		return wrapError(err, CodeBackendUnavailable,
			"tmux is not installed in the environment where the AgentMux server runs, "+
				"so no terminal session can be started")
	}
	version, err := b.tmuxVersion(ctx)
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

// tmuxVersion returns tmux's self-reported version, once per backend.
func (b *TmuxBackend) tmuxVersion(ctx context.Context) (string, error) {
	b.versionOnce.Do(func() {
		out, err := exec.CommandContext(ctx, b.bin, "-V").Output()
		if err != nil {
			b.versionErr = wrapError(err, CodeBackendUnavailable, "tmux is present but could not be run")
			return
		}
		// "tmux 3.4"
		b.version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "tmux "))
	})
	return b.version, b.versionErr
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
func (b *TmuxBackend) args(sub ...string) []string {
	args := []string{"-u"}
	if b.socket != "" {
		args = append(args, "-L", b.socket)
	}
	if b.config != "" {
		args = append(args, "-f", b.config)
	}
	return append(args, sub...)
}

// run executes one tmux command and returns its combined output.
func (b *TmuxBackend) run(ctx context.Context, sub ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tmuxCommandTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, b.bin, b.args(sub...)...)
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
	format := strings.Join([]string{
		"#{session_name}",
		"#{session_created}",
		"#{window_width}",
		"#{window_height}",
		"#{pane_current_path}",
	}, sessionFieldSeparator)

	out, err := b.run(ctx, "list-panes", "-t", name, "-F", format)
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

// List implements Backend.
func (b *TmuxBackend) List(ctx context.Context) ([]*Session, error) {
	format := strings.Join([]string{
		"#{session_name}",
		"#{session_created}",
		"#{window_width}",
		"#{window_height}",
		"#{pane_current_path}",
	}, sessionFieldSeparator)

	out, err := b.run(ctx, "list-panes", "-a", "-F", format)
	if err != nil {
		// No server is not a failure: it means no sessions exist, which is the
		// answer List was asked for.
		if isMissingTarget(out) {
			return nil, nil
		}
		// What tmux said is carried on the error, as runChecked does. "could not
		// list sessions: exit status 1" on its own names the symptom and hides
		// the cause, which is the one thing the operator cannot recover.
		trimmed := strings.TrimSpace(out)
		if trimmed == "" {
			return nil, wrapError(err, CodeBackendFailure, "could not list sessions")
		}
		return nil, wrapError(err, CodeBackendFailure, "could not list sessions: %s", trimmed)
	}

	seen := make(map[string]bool)
	var sessions []*Session
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		sess, ok := parseSessionLine(line)
		if !ok || !strings.HasPrefix(sess.Name, b.prefix) {
			continue
		}
		// One entry per session: a session with several panes is reported
		// once, by its first pane.
		if seen[sess.Name] {
			continue
		}
		seen[sess.Name] = true
		sessions = append(sessions, sess)
	}
	return sessions, nil
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

// Snapshot implements Backend.
//
// capture-pane is used here and only here. It answers "what is on the screen
// right now", which is exactly the question a client that has just connected
// needs answered once. It is not how live output is obtained - that is what
// control mode is for - because a snapshot cannot see between two calls.
func (b *TmuxBackend) Snapshot(ctx context.Context, name string) ([]byte, error) {
	if !b.sessionExists(ctx, name) {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchSession, name)
	}
	out, err := b.run(ctx, "capture-pane", "-p", "-e", "-t", name)
	if err != nil {
		return nil, wrapError(err, CodeBackendFailure, "could not capture session %q", name)
	}
	// The bytes are returned exactly as tmux produced them, escape sequences
	// included. Any tidying here would be the lossy step this whole design
	// exists to avoid.
	return []byte(out), nil
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
	args := b.args("-C", "attach-session", "-t", name)
	return startControlStream(ctx, b.bin, args, name, b.log)
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
// It is destructive, it is not reachable from the API, and nothing in AgentMux
// calls it during normal operation: Destroy removes one session, and Close
// deliberately leaves them all running. It exists so that a test can start
// from an empty runtime, and so that a future release has somewhere to put an
// explicit "stop everything" action, which is a decision for a user to make
// rather than a side effect of shutting a server down.
func (b *TmuxBackend) KillServer(ctx context.Context) error {
	b.closeSubscriptions("")
	// Nothing to kill is a success, so the result is deliberately discarded.
	_, _ = b.run(ctx, "kill-server")
	return nil
}
