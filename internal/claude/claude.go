// Package claude finds the Claude Code CLI and describes how to start it
// inside a project's persistent runtime.
//
// It owns as little as it can. It does not own a tmux socket, does not create
// or resize a terminal, does not transport a single byte of terminal output,
// and does not know what a browser is. Those belong to the session package and,
// from Phase 4, to the WebSocket layer. What is here is the one thing nothing
// else can answer: which Claude Code is installed, what version it is, and what
// command line starts it.
//
// # What this package will not do
//
// It will not read, print, or return a credential. It does not open Claude's
// configuration, does not read an API key out of the environment, does not
// touch the credential store, and reports nothing about whether the
// installation is signed in. Authentication is Claude's own business: it is
// decided when the program starts and it says so on its own terminal. A
// launcher that probed for a token would be a second place for a secret to
// exist, and the first place for one to leak into a log line or an API
// response.
//
// # Where it runs
//
// The Claude Code CLI is a Linux process. On Windows the AgentMux server runs
// inside WSL, so this package resolves and launches the CLI in the same
// environment as the tmux server that hosts it. A Windows-native server never
// reaches the point of launching anything: the runtime itself is unavailable
// there, and it says so.
package claude

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Type identifies this agent in the API and in a runtime's status.
const Type = "claude"

// DefaultBinary is the executable name resolved on PATH when the configuration
// names none.
//
// It is a bare name rather than an absolute path on purpose: the install
// location differs between a native install, a package manager, and a Node
// install, and a default that guessed one of them would be wrong on the other
// two. The setting exists so that a machine with more than one Claude Code can
// say which one AgentMux runs.
const DefaultBinary = "claude"

// versionTimeout bounds the version probe. Claude Code starts quickly, and a
// probe that hangs is a probe that would hold up every request that asks for
// server status.
const versionTimeout = 10 * time.Second

// versionCacheTTL bounds how long a resolved version is reused.
//
// The probe spawns the CLI, which is the most expensive thing this package
// does, and it is asked for on every server-status request and on every
// runtime status request. Claude Code updates itself in place, so the answer is
// allowed to change - it just is not allowed to cost a process per panel per
// poll.
const versionCacheTTL = 30 * time.Second

// Installation describes the Claude Code CLI AgentMux would launch.
//
// Every field is safe to log and safe to return over the API. There is
// deliberately no field describing authentication: this package does not look,
// and a boolean saying "signed in" would be a claim it cannot support.
type Installation struct {
	// Type is the agent's identity, always Type for this package.
	Type string `json:"type"`

	// Available reports whether a usable CLI was found. When it is false the
	// remaining fields describe what was looked for, and Message says why.
	Available bool `json:"available"`

	// Version is the version the CLI reported, for example "2.1.274". Empty
	// when it could not be read.
	Version string `json:"version,omitempty"`

	// Binary is the setting as given: a bare name, or the path that was
	// configured.
	Binary string `json:"binary"`

	// Path is the executable that setting resolved to, with symlinks followed.
	//
	// It is the canonical identity of the program, and it is what a running
	// process is matched against: the native Claude Code install is a symlink
	// into a versioned directory, so the path the CLI was started as and the
	// path the kernel reports for the process are not the same string.
	Path string `json:"path,omitempty"`

	// Command is the single line typed into a runtime's shell to start the
	// agent. It is shell-quoted.
	Command string `json:"command,omitempty"`

	// Message explains an unavailable installation, and is empty when it is
	// available.
	Message string `json:"message,omitempty"`
}

// Launcher resolves the Claude Code CLI.
//
// It is safe for concurrent use and caches the version probe. The function
// fields are injectable so the resolution rules can be tested without a Claude
// Code installation on the machine running the tests.
type Launcher struct {
	binary string
	log    *slog.Logger
	now    func() time.Time

	lookPath     func(string) (string, error)
	evalSymlinks func(string) (string, error)
	probeVersion func(ctx context.Context, path string) (string, error)

	mu       sync.Mutex
	cached   Installation
	cachedAt time.Time
}

// Options configures a Launcher. Every field is optional.
type Options struct {
	// Binary is the executable to resolve. Empty means DefaultBinary.
	Binary string

	// Logger receives diagnostics. Nil means slog.Default.
	Logger *slog.Logger

	// Now supplies the current time. Nil means time.Now.
	Now func() time.Time

	// LookPath resolves a bare name to a path. Nil means exec.LookPath.
	LookPath func(string) (string, error)

	// EvalSymlinks resolves a path to its canonical form. Nil means
	// filepath.EvalSymlinks.
	EvalSymlinks func(string) (string, error)

	// ProbeVersion runs the CLI's version command. Nil runs the real one.
	ProbeVersion func(ctx context.Context, path string) (string, error)
}

// New builds a Launcher.
func New(o Options) *Launcher {
	l := &Launcher{
		binary:       strings.TrimSpace(o.Binary),
		log:          o.Logger,
		now:          o.Now,
		lookPath:     o.LookPath,
		evalSymlinks: o.EvalSymlinks,
		probeVersion: o.ProbeVersion,
	}
	if l.binary == "" {
		l.binary = DefaultBinary
	}
	if l.log == nil {
		l.log = slog.Default()
	}
	if l.now == nil {
		l.now = time.Now
	}
	if l.lookPath == nil {
		l.lookPath = exec.LookPath
	}
	if l.evalSymlinks == nil {
		l.evalSymlinks = filepath.EvalSymlinks
	}
	if l.probeVersion == nil {
		l.probeVersion = runVersion
	}
	return l
}

// Name identifies the agent this launcher starts.
func (l *Launcher) Name() string { return Type }

// Binary reports the configured executable, as given.
func (l *Launcher) Binary() string { return l.binary }

// Resolve reports the Claude Code CLI this launcher would start.
//
// It never returns an error. A missing CLI is an answer, not a failure: the
// server has to be able to start and say that it cannot host an agent, which is
// a different thing from refusing to start.
func (l *Launcher) Resolve(ctx context.Context) Installation {
	l.mu.Lock()
	cached, at := l.cached, l.cachedAt
	l.mu.Unlock()
	if at != (time.Time{}) && l.now().Sub(at) < versionCacheTTL {
		return cached
	}

	inst := l.resolve(ctx)

	l.mu.Lock()
	l.cached, l.cachedAt = inst, l.now()
	l.mu.Unlock()
	return inst
}

// resolve does the work Resolve caches.
func (l *Launcher) resolve(ctx context.Context) Installation {
	inst := Installation{Type: Type, Binary: l.binary}

	path, err := l.lookPath(l.binary)
	if err != nil {
		inst.Message = fmt.Sprintf("%s was not found on PATH; set terminal.claudeBinary to its full path", l.binary)
		return inst
	}
	// The canonical path, because a native install is a symlink into a
	// versioned directory: the configured name and the running process are the
	// same program under two different strings.
	if resolved, err := l.evalSymlinks(path); err == nil {
		path = resolved
	}
	inst.Path = path

	version, err := l.probeVersion(ctx, path)
	if err != nil {
		// Found but unusable is still not available: a CLI that cannot answer
		// --version is one that would not start either, and reporting it as
		// available would move the failure to the moment a user pressed Start.
		inst.Message = fmt.Sprintf("%s could not be run: %v", path, err)
		return inst
	}
	inst.Version = version
	inst.Command = Quote(path)
	inst.Available = true
	return inst
}

// runVersion runs the CLI's version command and returns the version.
//
// Only the version is kept. Whatever else the CLI printed is discarded rather
// than logged, because this package does not know what the CLI prints and a
// diagnostic that forwarded it would be forwarding something nobody vetted.
func runVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("it did not answer within %s", versionTimeout)
		}
		return "", err
	}
	version := parseVersion(string(out))
	if version == "" {
		return "", fmt.Errorf("it did not report a version")
	}
	return version, nil
}

// parseVersion pulls the version out of the version command's output.
//
// The CLI answers with `2.1.274 (Claude Code)`. The first whitespace-separated
// token is taken if it looks like a version, so that a build whose output grows
// a suffix is still read correctly, and output that is not a version at all is
// reported as missing rather than returned as one.
func parseVersion(out string) string {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return ""
	}
	candidate := strings.TrimPrefix(fields[0], "v")
	if !looksLikeVersion(candidate) {
		return ""
	}
	return candidate
}

// looksLikeVersion reports whether s is dotted digits, optionally with a
// suffix. It is deliberately narrow: "2.1.274", "1.0.0-beta.1", "3".
func looksLikeVersion(s string) bool {
	digits := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '.':
			// A dot must not lead, trail, or double up.
			if i == 0 || i == len(s)-1 || s[i-1] == '.' {
				return false
			}
		case c == '-' || c == '+':
			// A suffix starts here; everything before it must be digits and
			// dots, and there must be something before it.
			return digits > 0 && i > 0
		default:
			return false
		}
	}
	return digits > 0
}

// Quote renders a path as a single shell word.
//
// The command is typed into a shell, so a path containing a space - which every
// path under /mnt/c is one directory away from - would otherwise arrive as two
// arguments. Single quotes make every byte literal, and the closing-quote
// escape is the POSIX idiom for a path that contains one.
func Quote(path string) string {
	if path == "" {
		return ""
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}
