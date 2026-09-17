package session

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
)

// This file builds one runtime per project.
//
// Phase 2 gave every project a session on a single shared tmux server. That is
// one process for the whole installation, and it made the fault domain the
// whole installation: a server that stopped took every project's terminal with
// it, and the AgentMux server could not tell "this project's work ended" from
// "the runtime died". Phase 2.5 makes the fault domain one project, because
// the blast radius of a runtime is exactly the set of things that share its
// server.

// ProjectRuntimesOptions configures the per-project runtime factory.
type ProjectRuntimesOptions struct {
	// Binary is the tmux executable every project's runtime runs. A bare name
	// is resolved on PATH; an absolute path is used as given.
	Binary string

	// SocketDir is the directory holding one socket per project.
	SocketDir string

	Config       string
	Terminal     string
	HistoryLimit int

	Logger *slog.Logger
}

// ProjectRuntimes builds and caches one backend per project.
//
// It is the only place that decides where a project's runtime lives, so the
// answer is the same for every caller: the project's socket path is a function
// of the socket directory and the project id, computed here and nowhere else.
//
// Backends are cached because a backend is the handle to a project's server,
// and two handles to one server that disagreed about anything - which
// subscriptions are open, which session is attached - would be a bug with no
// natural place to be fixed.
type ProjectRuntimes struct {
	install      *tmuxInstall
	sockets      *SocketDir
	config       string
	terminal     string
	historyLimit int
	log          *slog.Logger

	mu       sync.Mutex
	backends map[string]*TmuxBackend
}

// NewProjectRuntimes resolves the socket directory and prepares the factory.
//
// It fails when the socket directory cannot be made owner-only, and that is
// deliberate: a runtime whose control channel is readable by other local users
// is not the runtime this phase promises, and starting anyway would put a
// reassuring "running" in front of a weaker guarantee than the one documented.
func NewProjectRuntimes(o ProjectRuntimesOptions) (*ProjectRuntimes, error) {
	bin := orDefault(o.Binary, DefaultTmuxBinary)
	sockets, err := NewSocketDir(o.SocketDir, bin)
	if err != nil {
		return nil, err
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &ProjectRuntimes{
		install:      &tmuxInstall{bin: bin},
		sockets:      sockets,
		config:       orDefault(o.Config, DefaultTmuxConfig),
		terminal:     orDefault(o.Terminal, DefaultTmuxTerminal),
		historyLimit: o.HistoryLimit,
		log:          log,
		backends:     make(map[string]*TmuxBackend),
	}, nil
}

// Backend returns the backend that owns a project's runtime.
//
// An identifier AgentMux did not issue is refused rather than turned into a
// path. That is the whole of the "socket names come from the project id, never
// from the display name" rule, enforced where the path is built: there is no
// spelling of a project that reaches a different socket.
func (p *ProjectRuntimes) Backend(projectID string) (Backend, error) {
	path := p.sockets.Path(projectID)
	if path == "" {
		return nil, newError(CodeInvalidInput,
			"%q is not a project identifier AgentMux issued, so it has no runtime", projectID)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if backend, ok := p.backends[projectID]; ok {
		return backend, nil
	}
	backend := NewTmuxBackend(TmuxOptions{
		Install:      p.install,
		SocketPath:   path,
		Config:       p.config,
		Terminal:     p.terminal,
		HistoryLimit: p.historyLimit,
		Logger:       p.log,
	})
	p.backends[projectID] = backend
	return backend, nil
}

// BackendName names the kind of backend this factory builds.
func (p *ProjectRuntimes) BackendName() string { return "tmux" }

// Sockets returns the socket layout, for reconciliation and diagnostics.
func (p *ProjectRuntimes) Sockets() *SocketDir { return p.sockets }

// Status reports the tmux installation every project's runtime will use.
//
// It resolves the configured binary and reports the resolved path beside the
// version, because on a machine with more than one tmux installed the version
// alone does not identify what will run. It does not start anything and does
// not touch a socket: a diagnostic that changed the thing it was describing
// would be worse than no diagnostic.
func (p *ProjectRuntimes) Status(ctx context.Context) TmuxStatus {
	status := TmuxStatus{
		Binary:         p.install.bin,
		SocketDir:      p.sockets.Dir(),
		MinimumVersion: fmt.Sprintf("%d.0", tmuxMinVersion),
	}
	if resolved, err := exec.LookPath(p.install.bin); err == nil {
		status.Binary = resolved
	}

	version, err := p.install.version(ctx)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Version = version
	if major, ok := parseTmuxMajor(version); ok && major < tmuxMinVersion {
		status.Error = fmt.Sprintf("tmux %s is older than the required %d.0", version, tmuxMinVersion)
		return status
	}
	status.Available = true
	return status
}

// BackendFactory builds the backend that owns one project's runtime, and names
// the kind of backend it builds.
//
// The name is part of the interface rather than a constant the manager assumes,
// because the manager writes it into every runtime record: a record that says
// "tmux" for a session a different backend created is worse than no record.
type BackendFactory interface {
	Backend(projectID string) (Backend, error)
	BackendName() string
}

// StatusReporter is implemented by a factory that can describe the installation
// its backends will run on.
//
// It is deliberately not part of BackendFactory. The manager needs nothing from
// this during its working life - it exists so diagnostics can say which tmux
// will run, and a factory that cannot answer must still be a factory the
// manager accepts.
type StatusReporter interface {
	Status(ctx context.Context) TmuxStatus
}
