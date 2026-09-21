// Command server runs the AgentMux backend.
//
// Phase 2 scope: everything from Phase 1, plus the persistent terminal
// runtime. A project's session is a tmux session that outlives this process,
// named after the project id, started in the project's runtime path. There is
// still no Web Terminal: nothing streams a session's output to a browser, and
// the UI offers start and stop rather than a terminal it cannot fill.
//
// On Windows the server belongs inside WSL. The runtime is tmux and the
// programs it hosts are Linux processes, so a server started on the Windows
// side has no runtime at all; it says so through GET /api/server rather than
// reaching across the boundary one command at a time.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kutonlagos/agentmux/internal/agent"
	"github.com/kutonlagos/agentmux/internal/agentstate"
	"github.com/kutonlagos/agentmux/internal/attention"
	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/config"
	"github.com/kutonlagos/agentmux/internal/controller"
	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/httpapi"
	"github.com/kutonlagos/agentmux/internal/logging"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/storage"
	"github.com/kutonlagos/agentmux/internal/task"
	"github.com/kutonlagos/agentmux/internal/terminal"
	"github.com/kutonlagos/agentmux/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", version.AppName, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := loadConfig(args)
	if err != nil {
		return err
	}

	// Logs go to stderr so that stdout stays free for anything a future
	// command might print as its actual output.
	//
	// Every subsystem below is handed a logger tagged with its component, which
	// is what makes every record say where it came from. See the record shape in
	// internal/logging.
	logger := logging.New(cfg.Logging.Level, cfg.Logging.Format, os.Stderr)
	serverLog := logging.Component(logger, logging.ComponentServer)

	serverLog.Info("configuration loaded",
		"version", version.Version,
		"commit", orNone(version.ShortCommit()),
		"phase", version.Phase,
		"configFile", orNone(cfg.SourceFile),
		"dataDir", cfg.DataDir,
		"projectsRoots", cfg.Projects.Roots,
		"discoveryDepth", cfg.Projects.DiscoveryDepth,
		"runtimeMode", cfg.Runtime.Mode,
		"logLevel", cfg.Logging.Level,
		"debug", cfg.Server.Debug,
	)
	for _, warning := range cfg.Warnings {
		serverLog.Warn("configuration warning", "detail", warning)
	}

	// Leave a documented, editable configuration behind on first run, so that
	// "how do I change the Projects Root" has an answer in the file itself.
	//
	// Only when the configuration was not read from an explicit file: a server
	// started with -config /etc/agentmux/agentmux.yaml has been configured by
	// somebody who knows where the file is, and writing a second one inside the
	// data directory would leave two files that disagree, one of which is
	// ignored.
	configPath := cfg.SourceFile
	if configPath == "" {
		configPath = filepath.Join(cfg.DataDir, config.DefaultConfigFileName)
	}
	if created, err := config.WriteDefaultFile(configPath, cfg, 0o644); err != nil {
		serverLog.Warn("could not write a default config file", "path", configPath, "error", err)
	} else if created {
		serverLog.Info("wrote default config file", "path", configPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := storage.Open(ctx, storage.OpenOptions{Path: cfg.SQLitePath()})
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			serverLog.Warn("could not close the metadata database", "error", err)
		}
	}()
	storageLog := logging.Component(logger, logging.ComponentStorage)
	storageLog.Info("metadata database opened", "path", store.Path())

	migration, err := store.Migrate(ctx)
	if err != nil {
		return err
	}
	if len(migration.Applied) > 0 {
		storageLog.Info("schema migrations applied",
			"applied", migration.Applied, "schemaVersion", migration.Version)
	} else {
		storageLog.Info("schema is up to date", "schemaVersion", migration.Version)
	}

	// The identifier is created and kept in the settings table. It is not
	// returned by the API: an unauthenticated endpoint should not hand out a
	// value that is stable across every request from a machine.
	if _, err := store.Settings().GetOrCreate(ctx, storage.SettingInstallID, newInstallID); err != nil {
		return err
	}

	adapter, err := host.New(host.Options{
		Mode:         cfg.Runtime.Mode,
		Distro:       cfg.Runtime.Distro,
		WSLMountRoot: cfg.Runtime.WSLMountRoot,
		Roots:        cfg.Projects.Roots,
		Shell:        cfg.Terminal.Shell,
		TmuxBinary:   cfg.TmuxBinary(),
	})
	if err != nil {
		return err
	}
	systemInfo := adapter.Info(ctx)
	logging.Component(logger, logging.ComponentHost).Info("host adapter ready",
		"hostOs", systemInfo.HostOS,
		"runtimeMode", systemInfo.RuntimeMode,
		"runtimeOs", systemInfo.RuntimeOS,
		"distro", orNone(systemInfo.Distro),
		"pathMapper", systemInfo.PathMapper,
	)

	// The project service and the runtime manager each need the other: a
	// project's status comes from its runtime, and a runtime is resolved from a
	// project. The bridge is filled in on the next line, which is cheaper to
	// read than either a constructor that takes an unbuilt collaborator or a
	// setter that makes the wiring order a runtime precondition.
	bridge := &runtimeBridge{}
	projectService, err := project.NewService(project.Options{
		Repository: store.Projects(),
		Host:       adapter,
		Logger:     logging.Component(logger, logging.ComponentProject),
		Runtime:    bridge,
	})
	if err != nil {
		return err
	}

	// One project, one tmux server, one socket. That is the whole of the fault
	// isolation: a tmux server is identified by its socket, so a runtime fault
	// reaches exactly the projects whose sockets it holds, which is one.
	//
	// The socket directory defaults to a subdirectory of the data directory so
	// that it travels with the installation rather than with the shell the
	// server happened to be started from.
	socketDir := cfg.TmuxSocketDir()
	if socketDir == "" {
		socketDir = filepath.Join(cfg.DataDir, session.DefaultTmuxSocketDirName)
	}
	runtimeLog := logging.Component(logger, logging.ComponentRuntime)
	backends, err := session.NewProjectRuntimes(session.ProjectRuntimesOptions{
		Binary:    cfg.TmuxBinary(),
		SocketDir: socketDir,
		Logger:    runtimeLog,
	})
	if err != nil {
		return err
	}
	runtimeLog.Info("project runtimes",
		"tmuxBinary", backends.Status(ctx).Binary, "socketDir", backends.Sockets().Dir())

	// The coding agent a runtime hosts. Resolving it is a probe, not a
	// requirement: a machine with no Claude Code still runs AgentMux, still
	// manages projects, and still hosts terminals - it just has no agent to put
	// in one, and says so in the API rather than failing to start.
	agentLog := logging.Component(logger, logging.ComponentAgent)
	agents := claude.New(claude.Options{Binary: cfg.ClaudeBinary(), Logger: agentLog})
	installation := agents.Resolve(ctx)
	if installation.Available {
		agentLog.Info("coding agent ready",
			"agent", installation.Type,
			"version", installation.Version,
			"binary", installation.Path)
	} else {
		agentLog.Warn("no coding agent is available; runtimes will host terminals only",
			"agent", installation.Type, "reason", installation.Message)
	}

	// The event log. One service, handed to both the runtime manager that writes
	// it and the API that reads it, so that there is exactly one entry point
	// into the table and no path from an HTTP handler to a database.
	//
	// It is built before the runtime manager because the manager takes it as a
	// collaborator, and it is built after the store because that is what it
	// writes to. Nothing reads an event to decide anything: see
	// docs/AGENT_EVENTS.md §5.
	eventLog, err := event.NewService(event.Options{
		Repository: store.Events(),
		Logger:     logging.Component(logger, logging.ComponentEvent),
	})
	if err != nil {
		return err
	}

	// The projection of the event log into what is true about an agent now.
	//
	// It is built after the event log because it is told about every event that
	// log stores, and before everything else because everything else writes
	// events: a projection installed late would be a projection that had missed
	// whatever happened in the meantime.
	//
	// The two depend on each other - every stored event is projected, and a
	// projection is rebuilt from the log - so one of them has to exist first.
	// The event log does, and it is handed the projector on the next line.
	stateLog := logging.Component(logger, logging.ComponentAgentState)
	stateService, err := agentstate.NewService(agentstate.Options{
		Repository: store.AgentStates(),
		Events:     eventLog,
		Projects:   projectService,
		Logger:     stateLog,
	})
	if err != nil {
		return err
	}
	// Whether anybody needs to look, and what they might do about it.
	//
	// It is built after the state projection because it reads it: an `agent.*`
	// event names a runtime and no attempt, and the state is what says which
	// attempt a runtime belongs to. That is also why it is the second of the
	// two projectors rather than the first.
	attentionLog := logging.Component(logger, logging.ComponentAttention)
	attentionService, err := attention.NewService(attention.Options{
		Repository: store.Attention(),
		States:     stateService,
		Events:     eventLog,
		Projects:   projectService,
		Logger:     attentionLog,
	})
	if err != nil {
		return err
	}

	// The aggregation a console reads. It holds nothing and owns no table: it
	// asks the project service, the state projection and the attention
	// projection, and arranges what they say into one response. It is built
	// last because it depends on all of them.
	controllerLog := logging.Component(logger, logging.ComponentController)
	controllerService, err := controller.NewService(controller.Options{
		Projects:  projectService,
		Agents:    stateService,
		Attention: attentionService,
		Logger:    controllerLog,
	})
	if err != nil {
		return err
	}

	// The projectors, in the order they run, and the order is a dependency:
	// attention reads the state the first one keeps. Neither projection knows
	// the other exists; this line is the whole of what relates them.
	eventLog.SetProjectors(stateService, attentionService)

	// An empty projection over a log that has events in it is what a first run
	// after these phases leaves behind, so each is recomputed once, here, in
	// dependency order - the state first, because attention is folded from it.
	// A projection that already holds something is left alone: it is kept
	// current as events are written, and rebuilding on every start would pay the
	// whole history every time for nothing.
	//
	// The services report what they did; this only reports what they could not
	// do. A projection that could not be rebuilt is not a reason to refuse to
	// start: the event log is intact and is the record, and what is derived from
	// it can be recomputed on the next start.
	if _, _, err := stateService.RebuildIfNeeded(ctx); err != nil {
		stateLog.Error("could not rebuild the agent states", "error", err)
	}
	if _, _, err := attentionService.RebuildIfNeeded(ctx); err != nil {
		attentionLog.Error("could not rebuild attention", "error", err)
	}

	// The task model. It reads the project store to check that the project a
	// task names exists, and it writes to the event log the same way the runtime
	// manager does - through the service, never through the table.
	//
	// It is built after the event log because it records into it, and before the
	// API server because the API is handed the service rather than a repository.
	// Nothing here starts anything: creating a task records that somebody wants
	// work done, and the runtime API is still the only thing that launches a
	// process.
	taskLog := logging.Component(logger, logging.ComponentTask)
	taskService, err := task.NewService(task.Options{
		Repository: store.Tasks(),
		Projects:   projectService,
		Events:     eventLog,
		Logger:     taskLog,
	})
	if err != nil {
		return err
	}

	runtimes, err := session.NewManager(session.ManagerOptions{
		Backends:      backends,
		Sockets:       backends.Sockets(),
		Projects:      projectService,
		Store:         store.Runtimes(),
		Events:        eventLog,
		Shell:         cfg.Terminal.Shell,
		Cols:          cfg.Terminal.Cols,
		Rows:          cfg.Terminal.Rows,
		HistoryChunks: cfg.Terminal.HistoryChunks,
		HistoryBytes:  cfg.Terminal.HistoryBytes,
		Agent:         agentSpecs{agents},
		Logger:        runtimeLog,
	})
	if err != nil {
		return err
	}
	// The adapter manager. It owns one hook receiver per observed runtime, and
	// it is built here rather than after the coordinator because the coordinator
	// is handed it rather than allowed to make one: an observer that could be
	// constructed anywhere would be an observer nobody could close.
	//
	// Its recorder is the same event service everything else writes through, so
	// the `agent.*` rows land in the one log with the `runtime.*` rows beside
	// them and there is still exactly one door into the table.
	adapters := claude.NewManager(claude.ManagerOptions{
		Adapter: claude.AdapterOptions{
			Recorder: eventLog,
			Logger:   agentLog,
		},
		Logger: agentLog,
	})

	// The coordinator. It is the one thing in the build that decides which
	// Claude session is which attempt, and it decides it in memory - see
	// docs/AGENT_RUNTIME_BINDING.md §6. It starts nothing itself: every process
	// operation below it is the runtime manager's.
	agentService, err := agent.NewService(agent.Options{
		Runtimes: runtimes,
		Adapters: adapters,
		Sessions: taskService,
		Settings: agent.NewFileSettings(cfg.DataDir),
		Logger:   agentLog,
	})
	if err != nil {
		return err
	}
	// Registered after the runtime manager's own defer, so it runs before it:
	// every hook receiver is detached, and every settings document describing
	// one, before the runtime manager closes the terminals those sessions run
	// in. The sessions themselves are left alone, in both cases and for the
	// same reason - Claude is a child of the shell inside tmux and outlives this
	// process.
	defer func() {
		if err := adapters.Close(context.Background()); err != nil {
			agentLog.Warn("could not close the claude adapter manager", "error", err)
		}
	}()

	bridge.manager = runtimes

	// Closing the manager detaches every control client. It deliberately does
	// not touch the sessions: a terminal that ended because the server was
	// restarted would make the runtime pointless.
	defer func() {
		if err := runtimes.Close(); err != nil {
			runtimeLog.Warn("could not close the runtime manager", "error", err)
		}
	}()

	reconcileRuntimes(ctx, runtimes, runtimeLog)

	discoverer, err := project.NewDiscoverer(project.DiscovererOptions{
		Host:           adapter,
		Depth:          cfg.Projects.DiscoveryDepth,
		MaxCandidates:  cfg.Projects.MaxCandidates,
		MaxScannedDirs: cfg.Projects.MaxScanDirs,
		Timeout:        time.Duration(cfg.Projects.ScanTimeoutSec) * time.Second,
		Logger:         logging.Component(logger, logging.ComponentProject),
	})
	if err != nil {
		return err
	}

	workingDir, err := os.Getwd()
	if err != nil {
		workingDir = ""
	}
	webDir := cfg.ResolvedWebDir(workingDir)

	// The terminal hub is what a browser's socket is handed to. It is built
	// here rather than inside the API server because its lifetime is the
	// process's: it has to be closed before the runtime manager is, so that
	// every browser sees an ordinary close instead of a connection that dies
	// when the process does.
	hubOptions := terminal.HubOptions{Logger: logging.Component(logger, logging.ComponentTerminal)}
	// Zero is left as zero: the terminal package has its own default for how
	// long a disconnected controller keeps its lease, and restating the number
	// here would be a second place for it to be wrong.
	if cfg.Server.ControlGraceSec > 0 {
		hubOptions.ControlGrace = time.Duration(cfg.Server.ControlGraceSec) * time.Second
	}
	terminalHub, err := terminal.NewHub(runtimes, hubOptions)
	if err != nil {
		return err
	}

	api, err := httpapi.New(httpapi.Options{
		Config:      cfg,
		Host:        adapter,
		Projects:    projectService,
		Discoverer:  discoverer,
		Runtime:     runtimes,
		Events:      eventLog,
		Tasks:       taskService,
		Agent:       agents,
		Agents:      agentService,
		AgentStates: stateService,
		Attention:   attentionService,
		Controller:  controllerService,
		Terminal:    terminalHub,
		Logger:      logging.Component(logger, logging.ComponentAPI),
		WebDir:      webDir,
	})
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              cfg.Address(),
		Handler:           api.Handler(),
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderTimeoutSec) * time.Second,
		ReadTimeout:       time.Duration(cfg.Server.ReadTimeoutSec) * time.Second,
		WriteTimeout:      time.Duration(cfg.Server.WriteTimeoutSec) * time.Second,
		IdleTimeout:       time.Duration(cfg.Server.IdleTimeoutSec) * time.Second,
		ErrorLog:          slog.NewLogLogger(serverLog.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		serverLog.Info("AgentMux server listening",
			"address", cfg.Address(),
			"health", fmt.Sprintf("http://%s/health", displayAddress(cfg)),
			"serverInfo", fmt.Sprintf("http://%s/api/server", displayAddress(cfg)),
			"webDir", orNone(webDir),
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		serverLog.Info("shutdown signal received; stopping the server")
	}

	// Browser sockets are closed deliberately, and before the runtime manager
	// is. A hijacked connection is not one http.Server.Shutdown tracks, so
	// without this a browser would learn the server had stopped by its socket
	// dying, which is the one signal it cannot tell apart from a network that
	// went away.
	if err := terminalHub.Close(); err != nil {
		logging.Component(logger, logging.ComponentTerminal).Warn("could not close the terminal hub", "error", err)
	}

	// Browser disconnects must never affect project runtime, and a shutdown
	// must not leave requests half-answered. Draining is bounded so a stuck
	// client cannot hold the process open.
	shutdownCtx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(cfg.Server.ShutdownGraceSec)*time.Second,
	)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http server shutdown: %w", err)
	}
	serverLog.Info("AgentMux server stopped")
	return nil
}

// loadConfig parses the command line and resolves the configuration.
//
// Host-derived defaults are injected here rather than read inside the config
// package, which is what keeps a drive letter out of the configuration code.
func loadConfig(args []string) (*config.Config, error) {
	const flagUsage = "usage: server [flags]"

	flags := flag.NewFlagSet(version.AppName, flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "%s %s - %s\n\n%s\n\nFlags:\n",
			version.AppName, version.Version, version.Phase, flagUsage)
		flags.PrintDefaults()
	}

	var (
		configPath    = flags.String("config", "", "config file, JSON or (with a .yaml/.yml name) YAML (default <data-dir>/config.json)")
		dataDir       = flags.String("data-dir", "", "directory for AgentMux state (default: platform config directory)")
		hostFlag      = flags.String("host", "", "HTTP listen address (default "+config.DefaultServerHost+")")
		port          = flags.Int("port", 0, "HTTP listen port (default 8787)")
		debug         = flags.Bool("debug", false, "include this machine's filesystem layout in GET /api/server; see docs/SECURITY.md")
		logLevel      = flags.String("log-level", "", "log level: debug, info, warn, error")
		logFormat     = flags.String("log-format", "", "log format: text or json")
		runtimeMode   = flags.String("runtime-mode", "", "runtime mode: auto, native, wsl")
		runtimeDistro = flags.String("runtime-distro", "", "WSL distribution name")
		webDir        = flags.String("web-dir", "", "directory holding the built frontend")
		shell         = flags.String("shell", "", "shell a terminal session runs (default: the host's)")
		tmuxSocket    = flags.String("tmux-socket", "", "deprecated: the shared tmux socket name used before Phase 2.5; each project now has its own tmux server and this setting has no effect")
		tmuxBinary    = flags.String("tmux-binary", "", "tmux executable every project's runtime runs (default: tmux on PATH)")
		tmuxSocketDir = flags.String("tmux-socket-dir", "", "directory holding one tmux socket per project (default: <data-dir>/tmux)")
		claudeBinary  = flags.String("claude-binary", "", "Claude Code CLI a project's runtime starts (default: claude on PATH)")
		showVersion   = flags.Bool("version", false, "print the version and exit")

		projectsRoots stringList
	)
	flags.Var(&projectsRoots, "projects-root",
		"Projects Root to scan and manage; repeat the flag for several roots")

	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if *showVersion {
		// The commit is printed when the binary carries one, which a release
		// build does and a plain `go build` does not. See internal/version.
		fmt.Printf("%s (%s)\n", version.StringWithCommit(), version.Phase)
		if version.BuildDate != "" {
			fmt.Printf("built %s\n", version.BuildDate)
		}
		os.Exit(0)
	}
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s\n%s", strings.Join(flags.Args(), " "), flagUsage)
	}

	return config.Load(config.LoadOptions{
		Defaults: config.Defaults{
			ProjectsRoots: host.DefaultProjectsRoots(),
			DataDir:       host.DefaultDataDir(),
			TerminalShell: host.DefaultShell(),
		},
		Overrides: config.Overrides{
			ConfigPath:    *configPath,
			DataDir:       *dataDir,
			Host:          *hostFlag,
			Port:          *port,
			Debug:         *debug,
			ProjectsRoots: projectsRoots,
			LogLevel:      *logLevel,
			LogFormat:     *logFormat,
			RuntimeMode:   *runtimeMode,
			RuntimeDistro: *runtimeDistro,
			WebDir:        *webDir,
			TerminalShell: *shell,
			TmuxSocket:    *tmuxSocket,
			TmuxBinary:    *tmuxBinary,
			TmuxSocketDir: *tmuxSocketDir,
			ClaudeBinary:  *claudeBinary,
		},
	})
}

// runtimeBridge lets the project service ask the runtime manager for a
// project's status while the runtime manager asks the project service to
// resolve a project.
//
// The cycle is real and belongs to the domain, not to a mistake in the wiring:
// a project's status is a fact about its runtime. Breaking it with a pointer
// that is filled in immediately keeps both constructors honest - each takes
// exactly what it needs - and keeps the manager's nil case safe, so a status
// read before the manager exists answers "nothing to say" rather than panicking.
type runtimeBridge struct {
	manager *session.Manager
}

// agentSpecs adapts the Claude Code launcher to the runtime's view of an agent.
//
// The runtime is handed a command line and the executable that command starts,
// and nothing else: which program that is, what version, and whether it is
// authenticated are the launcher's business. This is the one place the two
// vocabularies meet, and keeping the translation here is what stops the runtime
// package from depending on any particular agent - or on any particular vendor.
type agentSpecs struct {
	launcher *claude.Launcher
}

// Spec implements session.AgentProvider.
//
// The launch is translated rather than passed on: the runtime's description of
// what the caller asked for is not the launcher's description of a command
// line, and this is the one place the two meet. What comes back is a single
// line the runtime types into a shell, with the binary and every argument
// quoted for it.
func (a agentSpecs) Spec(ctx context.Context, launch session.AgentLaunch) (session.AgentSpec, error) {
	installation := a.launcher.Resolve(ctx)
	if !installation.Available {
		return session.AgentSpec{}, errors.New(installation.Message)
	}
	return session.AgentSpec{
		Type:    installation.Type,
		Version: installation.Version,
		Command: claude.LaunchCommand(installation, claude.LaunchOptions{
			SessionID:    launch.SessionID,
			SettingsPath: launch.SettingsPath,
		}),
		Executable: installation.Path,
	}, nil
}

// ProjectStatus implements project.RuntimeState.
func (b *runtimeBridge) ProjectStatus(projectID string) string {
	if b.manager == nil {
		return ""
	}
	return b.manager.ProjectStatus(projectID)
}

// reconcileRuntimes matches what the store remembers against what is actually
// running, and reports the difference.
//
// It never starts anything. A session that survived is adopted so its output
// can be read again; a project whose session is gone is reported stopped, and
// resuming it is the user's decision rather than a side effect of a restart.
// A session with no project behind it is reported and left alone, because it
// may hold work somebody needs and killing it would be a guess.
func reconcileRuntimes(ctx context.Context, runtimes *session.Manager, logger *slog.Logger) {
	report, err := runtimes.Reconcile(ctx)
	if err != nil {
		// A failure here means the runtime state could not be established, not
		// that the server cannot run: project management does not depend on a
		// terminal, so this is reported and the server carries on.
		logger.Error("could not reconcile the terminal runtimes", "error", err)
		return
	}
	logger.Info("terminal runtimes reconciled",
		"running", report.Running,
		"stopped", report.Stopped,
		"orphans", len(report.Orphans),
	)
	for _, orphan := range report.Orphans {
		logger.Warn("a terminal session belongs to no registered project; it is left running",
			"session", orphan.Session,
			"projectId", orphan.ProjectID,
			"dir", orphan.Dir,
		)
	}
}

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string {
	return strings.Join(*s, string(filepath.ListSeparator))
}

func (s *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value must not be empty")
	}
	*s = append(*s, value)
	return nil
}

// newInstallID generates the identifier stored in the settings table.
func newInstallID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate installation id: %w", err)
	}
	return "inst_" + hex.EncodeToString(buf), nil
}

// displayAddress renders a browsable address, mapping a wildcard bind to
// loopback so the printed URL is one a user can actually open.
func displayAddress(cfg *config.Config) string {
	host := cfg.Server.Host
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", host, cfg.Server.Port)
}

func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(none)"
	}
	return value
}

// orDefault returns value, or fallback when value is blank.
//
// It exists because the session package treats an empty socket as "the user's
// own tmux socket", which is right for a library and wrong for a product: an
// AgentMux session must not appear in an unrelated tmux session list. The
// product default is applied here, at the one place that decides.
func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
