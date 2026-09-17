// Command server runs the AgentMux backend.
//
// Phase 1 scope: configuration, the metadata store, the collection-aware
// project model, and the REST surface for managing projects. There is no
// terminal runtime yet, and the server deliberately does not pretend
// otherwise: GET /api/server reports terminalRuntimeImplemented false.
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

	"github.com/kutonlagos/agentmux/internal/config"
	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/httpapi"
	"github.com/kutonlagos/agentmux/internal/logging"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/storage"
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
	logger := logging.New(cfg.Logging.Level, cfg.Logging.Format, os.Stderr)

	logger.Info("configuration loaded",
		"version", version.Version,
		"phase", version.Phase,
		"configFile", orNone(cfg.SourceFile),
		"dataDir", cfg.DataDir,
		"projectsRoots", cfg.Projects.Roots,
		"discoveryDepth", cfg.Projects.DiscoveryDepth,
		"runtimeMode", cfg.Runtime.Mode,
		"logLevel", cfg.Logging.Level,
	)
	for _, warning := range cfg.Warnings {
		logger.Warn("configuration warning", "detail", warning)
	}

	// Leave a documented, editable configuration behind on first run, so that
	// "how do I change the Projects Root" has an answer in the file itself.
	configPath := cfg.SourceFile
	if configPath == "" {
		configPath = filepath.Join(cfg.DataDir, config.DefaultConfigFileName)
	}
	if created, err := config.WriteDefaultFile(configPath, cfg, 0o644); err != nil {
		logger.Warn("could not write a default config file", "path", configPath, "error", err)
	} else if created {
		logger.Info("wrote default config file", "path", configPath)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := storage.Open(ctx, storage.OpenOptions{Path: cfg.SQLitePath()})
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Warn("could not close the metadata database", "error", err)
		}
	}()
	logger.Info("metadata database opened", "path", store.Path())

	migration, err := store.Migrate(ctx)
	if err != nil {
		return err
	}
	if len(migration.Applied) > 0 {
		logger.Info("schema migrations applied",
			"applied", migration.Applied, "schemaVersion", migration.Version)
	} else {
		logger.Info("schema is up to date", "schemaVersion", migration.Version)
	}

	installID, err := store.Settings().GetOrCreate(ctx, storage.SettingInstallID, newInstallID)
	if err != nil {
		return err
	}

	adapter, err := host.New(host.Options{
		Mode:         cfg.Runtime.Mode,
		Distro:       cfg.Runtime.Distro,
		WSLMountRoot: cfg.Runtime.WSLMountRoot,
		Roots:        cfg.Projects.Roots,
	})
	if err != nil {
		return err
	}
	systemInfo := adapter.Info(ctx)
	logger.Info("host adapter ready",
		"hostOs", systemInfo.HostOS,
		"runtimeMode", systemInfo.RuntimeMode,
		"runtimeOs", systemInfo.RuntimeOS,
		"distro", orNone(systemInfo.Distro),
		"pathMapper", systemInfo.PathMapper,
	)

	projectService, err := project.NewService(project.Options{
		Repository: store.Projects(),
		Host:       adapter,
		Logger:     logger,
	})
	if err != nil {
		return err
	}

	discoverer, err := project.NewDiscoverer(project.DiscovererOptions{
		Host:           adapter,
		Depth:          cfg.Projects.DiscoveryDepth,
		MaxCandidates:  cfg.Projects.MaxCandidates,
		MaxScannedDirs: cfg.Projects.MaxScanDirs,
		Timeout:        time.Duration(cfg.Projects.ScanTimeoutSec) * time.Second,
		Logger:         logger,
	})
	if err != nil {
		return err
	}

	workingDir, err := os.Getwd()
	if err != nil {
		workingDir = ""
	}
	webDir := cfg.ResolvedWebDir(workingDir)

	api, err := httpapi.New(httpapi.Options{
		Config:     cfg,
		Host:       adapter,
		Projects:   projectService,
		Discoverer: discoverer,
		Logger:     logger,
		InstallID:  installID,
		WebDir:     webDir,
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
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("AgentMux server listening",
			"address", cfg.Address(),
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
		logger.Info("shutdown signal received; stopping the server")
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
	logger.Info("AgentMux server stopped")
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
		configPath    = flags.String("config", "", "JSON config file (default <data-dir>/config.json)")
		dataDir       = flags.String("data-dir", "", "directory for AgentMux state (default: platform config directory)")
		hostFlag      = flags.String("host", "", "HTTP listen address (default "+config.DefaultServerHost+")")
		port          = flags.Int("port", 0, "HTTP listen port (default 8787)")
		logLevel      = flags.String("log-level", "", "log level: debug, info, warn, error")
		logFormat     = flags.String("log-format", "", "log format: text or json")
		runtimeMode   = flags.String("runtime-mode", "", "runtime mode: auto, native, wsl")
		runtimeDistro = flags.String("runtime-distro", "", "WSL distribution name")
		webDir        = flags.String("web-dir", "", "directory holding the built frontend")
		showVersion   = flags.Bool("version", false, "print the version and exit")

		projectsRoots stringList
	)
	flags.Var(&projectsRoots, "projects-root",
		"Projects Root to scan and manage; repeat the flag for several roots")

	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if *showVersion {
		fmt.Printf("%s %s (%s)\n", version.AppName, version.Version, version.Phase)
		os.Exit(0)
	}
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s\n%s", strings.Join(flags.Args(), " "), flagUsage)
	}

	return config.Load(config.LoadOptions{
		Defaults: config.Defaults{
			ProjectsRoots: host.DefaultProjectsRoots(),
			DataDir:       host.DefaultDataDir(),
		},
		Overrides: config.Overrides{
			ConfigPath:    *configPath,
			DataDir:       *dataDir,
			Host:          *hostFlag,
			Port:          *port,
			ProjectsRoots: projectsRoots,
			LogLevel:      *logLevel,
			LogFormat:     *logFormat,
			RuntimeMode:   *runtimeMode,
			RuntimeDistro: *runtimeDistro,
			WebDir:        *webDir,
		},
	})
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
