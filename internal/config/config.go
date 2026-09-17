// Package config resolves the AgentMux server configuration.
//
// Sources, in order of increasing precedence:
//
//	built-in defaults -> JSON config file -> AGENTMUX_* environment -> CLI flags
//
// The package deliberately contains no OS-specific knowledge. Anything that
// depends on the host platform (the default Projects Root, the default data
// directory) is injected by the caller through Defaults. This is what keeps
// "D:\AI\Projects" and "/mnt/d/AI/Projects" out of business logic.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kutonlagos/agentmux/internal/pathutil"
)

// Environment variables understood by AgentMux. Every one has a matching CLI
// flag; flags win.
const (
	EnvPrefix        = "AGENTMUX_"
	EnvConfigPath    = EnvPrefix + "CONFIG"
	EnvDataDir       = EnvPrefix + "DATA_DIR"
	EnvHost          = EnvPrefix + "HOST"
	EnvPort          = EnvPrefix + "PORT"
	EnvProjectsRoots = EnvPrefix + "PROJECTS_ROOTS"
	EnvLogLevel      = EnvPrefix + "LOG_LEVEL"
	EnvLogFormat     = EnvPrefix + "LOG_FORMAT"
	EnvRuntimeMode   = EnvPrefix + "RUNTIME_MODE"
	EnvRuntimeDistro = EnvPrefix + "RUNTIME_DISTRO"
	EnvWSLMountRoot  = EnvPrefix + "WSL_MOUNT_ROOT"
	EnvWebDir        = EnvPrefix + "WEB_DIR"
	EnvTerminalShell = EnvPrefix + "TERMINAL_SHELL"
	EnvTmuxSocket    = EnvPrefix + "TMUX_SOCKET"
	EnvTmuxBinary    = EnvPrefix + "TMUX_BINARY"
	EnvTmuxSocketDir = EnvPrefix + "TMUX_SOCKET_DIR"
	EnvDebugAPI      = EnvPrefix + "DEBUG_API"
)

// Runtime modes. "auto" lets the HostAdapter decide from the host platform.
const (
	RuntimeModeAuto   = "auto"
	RuntimeModeNative = "native"
	RuntimeModeWSL    = "wsl"
)

// Built-in defaults.
const (
	DefaultServerHost     = "127.0.0.1"
	DefaultServerPort     = 8787
	DefaultDiscoveryDepth = 3
	DefaultMaxCandidates  = 500
	DefaultMaxScanDirs    = 20000
	DefaultScanTimeoutSec = 20
	DefaultLogLevel       = "info"
	DefaultLogFormat      = "text"
	DefaultWSLMountRoot   = "/mnt"
	DefaultWebDir         = "web/dist"
	DefaultSQLiteFileName = "agentmux.db"
	DefaultConfigFileName = "config.json"
)

// Config is the fully resolved AgentMux server configuration.
type Config struct {
	Server   ServerConfig   `json:"server"`
	Storage  StorageConfig  `json:"storage"`
	Projects ProjectsConfig `json:"projects"`
	Runtime  RuntimeConfig  `json:"runtime"`
	Terminal TerminalConfig `json:"terminal"`
	Logging  LoggingConfig  `json:"logging"`
	Web      WebConfig      `json:"web"`

	// DataDir is where AgentMux keeps its own state (database, config file).
	// AgentMux never writes runtime state into a managed project repository.
	//
	// DataDir is resolved from -data-dir / AGENTMUX_DATA_DIR / the built-in
	// default, and is intentionally NOT settable from the config file: the
	// default config file lives inside the data directory, so allowing the
	// file to move it would be circular. A dataDir key in the file is ignored
	// and reported as a warning.
	DataDir string `json:"dataDir"`

	// SourceFile is the config file that was read, or "" when no file existed.
	SourceFile string `json:"-"`

	// Warnings collects non-fatal problems, such as a Projects Root that does
	// not exist on this machine. They are surfaced through GET /api/server so
	// the UI can display them instead of silently pretending everything is
	// fine.
	Warnings []string `json:"-"`
}

// ServerConfig configures the HTTP listener.
type ServerConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`

	// ReadHeaderTimeoutSec guards against slow-header clients.
	ReadHeaderTimeoutSec int `json:"readHeaderTimeoutSeconds"`

	// ReadTimeoutSec and WriteTimeoutSec default to 0 (disabled). A non-zero
	// WriteTimeout would apply its deadline to hijacked connections and would
	// break the WebSocket transport added in Phase 4. Keep them disabled and
	// enforce per-request limits in handlers instead.
	ReadTimeoutSec  int `json:"readTimeoutSeconds"`
	WriteTimeoutSec int `json:"writeTimeoutSeconds"`

	IdleTimeoutSec   int `json:"idleTimeoutSeconds"`
	ShutdownGraceSec int `json:"shutdownGraceSeconds"`

	// AllowedOrigins lists extra browser origins permitted by CORS, on top of
	// the always-allowed loopback origins. Empty means loopback only.
	AllowedOrigins []string `json:"allowedOrigins"`

	// DebugAPI enables the diagnostic endpoints under /api/debug.
	//
	// It is off by default and is not a product API: those endpoints exist so
	// that the terminal runtime can be exercised end to end before there is a
	// Web Terminal to exercise it with, and they are expected to be deleted
	// once Phase 4 provides a real one. Leaving them on in an ordinary
	// installation would expose raw terminal input and output over HTTP.
	DebugAPI bool `json:"debugApi"`
}

// StorageConfig configures the metadata store.
type StorageConfig struct {
	// SQLitePath is the database file. A relative path is resolved against
	// Config.DataDir.
	SQLitePath string `json:"sqlitePath"`
}

// ProjectsConfig configures the project model.
type ProjectsConfig struct {
	// Roots are the broad user-controlled roots that may contain collection
	// folders and projects. Order matters: the first root is the default
	// target for New Project.
	Roots []string `json:"roots"`

	// DiscoveryDepth bounds how deep candidate discovery walks below a root.
	// Depth 2 reaches "Root/Collection/Project"; 3 leaves one level of margin.
	DiscoveryDepth int `json:"discoveryDepth"`

	MaxCandidates  int `json:"maxCandidates"`
	MaxScanDirs    int `json:"maxScanDirectories"`
	ScanTimeoutSec int `json:"scanTimeoutSeconds"`
}

// RuntimeConfig selects the execution environment for terminal sessions.
type RuntimeConfig struct {
	// Mode is one of "auto", "native", or "wsl".
	Mode string `json:"mode"`

	// Distro is the WSL distribution name. Empty means auto-detect on Windows.
	Distro string `json:"distro"`

	// WSLMountRoot is the mount point under which WSL exposes Windows drives.
	WSLMountRoot string `json:"wslMountRoot"`
}

// TerminalConfig configures the terminal runtime itself, as opposed to where it
// executes.
//
// Every field is optional. An empty or zero value means the runtime's own
// default, which is applied by the session package rather than duplicated here:
// a socket directory or a canonical size written down twice is a socket
// directory or a canonical size that will eventually disagree with itself.
type TerminalConfig struct {
	// Socket is the tmux socket *name* sessions were created on, on one shared
	// tmux server.
	//
	// Deprecated, and retained only so that an existing configuration file
	// keeps loading. Since Phase 2.5 every project has its own server and its
	// own socket, addressed by SocketDir and the project id; a single shared
	// socket name no longer describes anything AgentMux creates. Setting it
	// produces a warning and has no effect - see the note in docs/RUNTIME.md.
	//
	// It is not silently reinterpreted as a path or as a socket directory
	// because both readings would be wrong in a way the user could not see:
	// a socket name is not a path, and a name that used to hold every project
	// is not the directory that now holds one socket per project.
	Socket string `json:"socket"`

	// Binary is the tmux executable every project's runtime runs. A bare name
	// is resolved on PATH; an absolute path is used as given.
	//
	// It is configurable because a machine can have more than one tmux, and a
	// runtime that measures one and drives another is worse than one that
	// measures nothing: the version report would describe a binary that never
	// ran. Every path that touches a server resolves the binary through this
	// value, so the answer to "which tmux is this" is the same everywhere.
	Binary string `json:"tmuxBinary"`

	// SocketDir holds one socket per project, named after the project id.
	//
	// Empty means a directory under the data directory. It is the whole of the
	// fault isolation: a tmux server is identified by its socket, so two socket
	// paths are two servers, and two servers cannot take each other down.
	SocketDir string `json:"tmuxSocketDir"`

	// Shell is the shell a session runs when no command is given. Empty means
	// the host's default shell, which is injected by the caller because it is
	// the one platform-dependent value here.
	Shell string `json:"shell"`

	// Cols and Rows are the canonical terminal geometry. Zero means the
	// runtime's default.
	//
	// A session needs a size before any client exists. Letting the first client
	// to attach decide would make the terminal's size a side effect of who
	// clicked first, and would resize a program that had already drawn itself.
	Cols int `json:"canonicalCols"`
	Rows int `json:"canonicalRows"`

	// HistoryChunks and HistoryBytes bound the output AgentMux keeps in memory
	// per runtime, so that a reconnecting client can be given what it missed.
	// Zero means the runtime's default. This is not the session's scrollback:
	// that lives in tmux and is bounded by the tmux history limit.
	HistoryChunks int `json:"historyChunks"`
	HistoryBytes  int `json:"historyBytes"`
}

// LoggingConfig configures structured logging.
type LoggingConfig struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

// WebConfig configures serving of the built frontend.
type WebConfig struct {
	// Dir holds the built frontend. A relative path is resolved against the
	// process working directory. When the directory is missing the server
	// still runs; it simply does not serve the UI.
	Dir string `json:"dir"`
}

// Defaults are host-derived values injected by the caller.
type Defaults struct {
	ProjectsRoots []string
	DataDir       string
	ServerHost    string
	ServerPort    int

	// TerminalShell is the shell a session runs when the configuration names
	// none. It is injected rather than read inside this package because it is
	// the one value here that depends on the platform.
	TerminalShell string
}

// Overrides are values supplied on the command line. Zero values are ignored.
type Overrides struct {
	ConfigPath    string
	DataDir       string
	Host          string
	Port          int
	ProjectsRoots []string
	LogLevel      string
	LogFormat     string
	RuntimeMode   string
	RuntimeDistro string
	WebDir        string
	TerminalShell string

	// TmuxSocket is the deprecated shared socket name. It is accepted and
	// warned about rather than rejected, so that an existing invocation keeps
	// starting.
	TmuxSocket    string
	TmuxBinary    string
	TmuxSocketDir string
	DebugAPI      bool
}

// LoadOptions controls Load. The function hooks are injectable so the loader
// can be tested without touching the real filesystem or environment.
type LoadOptions struct {
	Defaults  Defaults
	Overrides Overrides

	// Environ supplies environment variables. Nil means os.LookupEnv.
	Environ func(string) (string, bool)

	// ReadFile reads a config file. Nil means os.ReadFile.
	ReadFile func(string) ([]byte, error)

	// Stat reports filesystem metadata. Nil means os.Stat.
	Stat func(string) (os.FileInfo, error)
}

// Load resolves the configuration.
func Load(o LoadOptions) (*Config, error) {
	env := o.Environ
	if env == nil {
		env = os.LookupEnv
	}
	readFile := o.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	stat := o.Stat
	if stat == nil {
		stat = os.Stat
	}

	cfg := defaultsFrom(o.Defaults)

	// 1. Data directory, resolved before the config file is located because
	//    the default config file lives inside it.
	if v, ok := env(EnvDataDir); ok && strings.TrimSpace(v) != "" {
		cfg.DataDir = strings.TrimSpace(v)
	}
	if v := strings.TrimSpace(o.Overrides.DataDir); v != "" {
		cfg.DataDir = v
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, fmt.Errorf("config: no data directory resolved: set -data-dir, %s, or supply a default", EnvDataDir)
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("config: resolve data directory %q: %w", cfg.DataDir, err)
	}
	cfg.DataDir = filepath.Clean(dataDir)

	// 2. Config file: flag, then environment, then <dataDir>/config.json.
	path := strings.TrimSpace(o.Overrides.ConfigPath)
	if path == "" {
		if v, ok := env(EnvConfigPath); ok {
			path = strings.TrimSpace(v)
		}
	}
	explicit := path != ""
	if path == "" {
		path = filepath.Join(cfg.DataDir, DefaultConfigFileName)
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = filepath.Clean(abs)
	}

	data, err := readFile(path)
	switch {
	case err == nil:
		dataDirBefore := cfg.DataDir
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
		if !pathutil.Same(cfg.DataDir, dataDirBefore) {
			cfg.addWarning(fmt.Sprintf(
				"config file %s sets dataDir=%q, which is ignored; use -data-dir or %s instead",
				path, cfg.DataDir, EnvDataDir))
			cfg.DataDir = dataDirBefore
		}
		cfg.SourceFile = path
	case errors.Is(err, os.ErrNotExist) && !explicit:
		// First run: no config file yet, defaults apply.
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("config: config file %s does not exist", path)
	default:
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	// 3. Environment overrides.
	applyEnv(cfg, env)

	// 4. Flag overrides.
	applyOverrides(cfg, o.Overrides)

	normalize(cfg)
	if err := validate(cfg, stat); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaultsFrom(d Defaults) *Config {
	host := strings.TrimSpace(d.ServerHost)
	if host == "" {
		host = DefaultServerHost
	}
	port := d.ServerPort
	if port == 0 {
		port = DefaultServerPort
	}
	return &Config{
		Server: ServerConfig{
			Host:                 host,
			Port:                 port,
			ReadHeaderTimeoutSec: 10,
			IdleTimeoutSec:       120,
			ShutdownGraceSec:     10,
		},
		Storage: StorageConfig{SQLitePath: DefaultSQLiteFileName},
		Projects: ProjectsConfig{
			Roots:          append([]string(nil), d.ProjectsRoots...),
			DiscoveryDepth: DefaultDiscoveryDepth,
			MaxCandidates:  DefaultMaxCandidates,
			MaxScanDirs:    DefaultMaxScanDirs,
			ScanTimeoutSec: DefaultScanTimeoutSec,
		},
		Runtime:  RuntimeConfig{Mode: RuntimeModeAuto, WSLMountRoot: DefaultWSLMountRoot},
		Terminal: TerminalConfig{Shell: strings.TrimSpace(d.TerminalShell)},
		Logging:  LoggingConfig{Level: DefaultLogLevel, Format: DefaultLogFormat},
		Web:      WebConfig{Dir: DefaultWebDir},
		DataDir:  d.DataDir,
	}
}

func applyEnv(cfg *Config, env func(string) (string, bool)) {
	if v, ok := env(EnvHost); ok && strings.TrimSpace(v) != "" {
		cfg.Server.Host = strings.TrimSpace(v)
	}
	if v, ok := envInt(env, EnvPort); ok {
		cfg.Server.Port = v
	}
	if v, ok := env(EnvProjectsRoots); ok {
		if roots := splitRoots(v); len(roots) > 0 {
			cfg.Projects.Roots = roots
		}
	}
	if v, ok := env(EnvLogLevel); ok && strings.TrimSpace(v) != "" {
		cfg.Logging.Level = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := env(EnvLogFormat); ok && strings.TrimSpace(v) != "" {
		cfg.Logging.Format = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := env(EnvRuntimeMode); ok && strings.TrimSpace(v) != "" {
		cfg.Runtime.Mode = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := env(EnvRuntimeDistro); ok && strings.TrimSpace(v) != "" {
		cfg.Runtime.Distro = strings.TrimSpace(v)
	}
	if v, ok := env(EnvWSLMountRoot); ok && strings.TrimSpace(v) != "" {
		cfg.Runtime.WSLMountRoot = strings.TrimSpace(v)
	}
	if v, ok := env(EnvWebDir); ok && strings.TrimSpace(v) != "" {
		cfg.Web.Dir = strings.TrimSpace(v)
	}
	if v, ok := env(EnvTerminalShell); ok && strings.TrimSpace(v) != "" {
		cfg.Terminal.Shell = strings.TrimSpace(v)
	}
	if v, ok := env(EnvTmuxSocket); ok && strings.TrimSpace(v) != "" {
		cfg.Terminal.Socket = strings.TrimSpace(v)
	}
	if v, ok := env(EnvTmuxBinary); ok && strings.TrimSpace(v) != "" {
		cfg.Terminal.Binary = strings.TrimSpace(v)
	}
	if v, ok := env(EnvTmuxSocketDir); ok && strings.TrimSpace(v) != "" {
		cfg.Terminal.SocketDir = strings.TrimSpace(v)
	}
	if v, ok := env(EnvDebugAPI); ok {
		cfg.Server.DebugAPI = parseBool(v)
	}
}

func applyOverrides(cfg *Config, o Overrides) {
	if v := strings.TrimSpace(o.Host); v != "" {
		cfg.Server.Host = v
	}
	if o.Port != 0 {
		cfg.Server.Port = o.Port
	}
	if len(o.ProjectsRoots) > 0 {
		cfg.Projects.Roots = append([]string(nil), o.ProjectsRoots...)
	}
	if v := strings.TrimSpace(o.LogLevel); v != "" {
		cfg.Logging.Level = strings.ToLower(v)
	}
	if v := strings.TrimSpace(o.LogFormat); v != "" {
		cfg.Logging.Format = strings.ToLower(v)
	}
	if v := strings.TrimSpace(o.RuntimeMode); v != "" {
		cfg.Runtime.Mode = strings.ToLower(v)
	}
	if v := strings.TrimSpace(o.RuntimeDistro); v != "" {
		// Pinning the distribution matters when several are installed and the
		// automatic choice is not the one the projects live in.
		cfg.Runtime.Distro = v
	}
	if v := strings.TrimSpace(o.WebDir); v != "" {
		cfg.Web.Dir = v
	}
	if v := strings.TrimSpace(o.TerminalShell); v != "" {
		cfg.Terminal.Shell = v
	}
	if v := strings.TrimSpace(o.TmuxSocket); v != "" {
		cfg.Terminal.Socket = v
	}
	if v := strings.TrimSpace(o.TmuxBinary); v != "" {
		cfg.Terminal.Binary = v
	}
	if v := strings.TrimSpace(o.TmuxSocketDir); v != "" {
		cfg.Terminal.SocketDir = v
	}
	// A boolean flag can only turn the diagnostic endpoints on, never off: they
	// are off unless something asks for them, so there is nothing to override.
	if o.DebugAPI {
		cfg.Server.DebugAPI = true
	}
}

// parseBool reads a boolean environment value. Anything unrecognised is false,
// so a typo disables a feature rather than silently enabling one.
func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// normalize cleans up values that are valid in several spellings.
func normalize(cfg *Config) {
	cfg.Projects.Roots = dedupePaths(cfg.Projects.Roots)
	if strings.TrimSpace(cfg.Storage.SQLitePath) == "" {
		cfg.Storage.SQLitePath = DefaultSQLiteFileName
	}
	if strings.TrimSpace(cfg.Runtime.WSLMountRoot) == "" {
		cfg.Runtime.WSLMountRoot = DefaultWSLMountRoot
	}
	mount := strings.TrimSpace(cfg.Runtime.WSLMountRoot)
	if !strings.HasPrefix(mount, "/") {
		mount = "/" + mount
	}
	cfg.Runtime.WSLMountRoot = strings.TrimRight(mount, "/")
	if cfg.Runtime.WSLMountRoot == "" {
		cfg.Runtime.WSLMountRoot = DefaultWSLMountRoot
	}
	cfg.Server.Host = strings.TrimSpace(cfg.Server.Host)
	cfg.Logging.Level = strings.ToLower(strings.TrimSpace(cfg.Logging.Level))
	cfg.Logging.Format = strings.ToLower(strings.TrimSpace(cfg.Logging.Format))
	cfg.Runtime.Mode = strings.ToLower(strings.TrimSpace(cfg.Runtime.Mode))
	if cfg.Runtime.Mode == "" {
		cfg.Runtime.Mode = RuntimeModeAuto
	}
	if cfg.Projects.DiscoveryDepth <= 0 {
		cfg.Projects.DiscoveryDepth = DefaultDiscoveryDepth
	}
	if cfg.Projects.MaxCandidates <= 0 {
		cfg.Projects.MaxCandidates = DefaultMaxCandidates
	}
	if cfg.Projects.MaxScanDirs <= 0 {
		cfg.Projects.MaxScanDirs = DefaultMaxScanDirs
	}
	if cfg.Projects.ScanTimeoutSec <= 0 {
		cfg.Projects.ScanTimeoutSec = DefaultScanTimeoutSec
	}
	if cfg.Server.ReadHeaderTimeoutSec <= 0 {
		cfg.Server.ReadHeaderTimeoutSec = 10
	}
	if cfg.Server.IdleTimeoutSec <= 0 {
		cfg.Server.IdleTimeoutSec = 120
	}
	if cfg.Server.ShutdownGraceSec <= 0 {
		cfg.Server.ShutdownGraceSec = 10
	}
	if strings.TrimSpace(cfg.Web.Dir) == "" {
		cfg.Web.Dir = DefaultWebDir
	}
	// The terminal's own defaults are applied by the runtime, so only trimming
	// happens here. A value that survives as "" means "the runtime decides".
	cfg.Terminal.Socket = strings.TrimSpace(cfg.Terminal.Socket)
	cfg.Terminal.Shell = strings.TrimSpace(cfg.Terminal.Shell)
	if cfg.Terminal.Cols < 0 {
		cfg.Terminal.Cols = 0
	}
	if cfg.Terminal.Rows < 0 {
		cfg.Terminal.Rows = 0
	}
}

// validate reports fatal configuration problems and records non-fatal ones as
// warnings.
func validate(cfg *Config, stat func(string) (os.FileInfo, error)) error {
	if cfg.Server.Host == "" {
		return errors.New("config: server.host must not be empty")
	}
	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return fmt.Errorf("config: server.port %d is out of range 1-65535", cfg.Server.Port)
	}
	switch cfg.Runtime.Mode {
	case RuntimeModeAuto, RuntimeModeNative, RuntimeModeWSL:
	default:
		return fmt.Errorf("config: runtime.mode %q is not one of auto, native, wsl", cfg.Runtime.Mode)
	}
	if !loggingLevels[cfg.Logging.Level] {
		return fmt.Errorf("config: logging.level %q is not one of debug, info, warn, error", cfg.Logging.Level)
	}
	if !loggingFormats[cfg.Logging.Format] {
		return fmt.Errorf("config: logging.format %q is not one of text, json", cfg.Logging.Format)
	}
	if cfg.Projects.DiscoveryDepth < 1 || cfg.Projects.DiscoveryDepth > 8 {
		return fmt.Errorf("config: projects.discoveryDepth %d is out of range 1-8", cfg.Projects.DiscoveryDepth)
	}
	if len(cfg.Projects.Roots) == 0 {
		return errors.New("config: projects.roots is empty; AgentMux needs at least one Projects Root")
	}
	for _, root := range cfg.Projects.Roots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("config: projects root %q must be an absolute path", root)
		}
	}
	// A missing root is a warning, not a failure: discovery reports it per
	// root, and the user may mount the drive later.
	for _, root := range cfg.Projects.Roots {
		info, err := stat(root)
		switch {
		case err != nil:
			cfg.addWarning(fmt.Sprintf("projects root %s is not accessible: %v", root, err))
		case !info.IsDir():
			cfg.addWarning(fmt.Sprintf("projects root %s is not a directory", root))
		}
	}

	// The deprecated shared socket name. It is warned about rather than
	// rejected, because refusing to start would break an installation that is
	// otherwise fine, and it is warned about rather than ignored because a user
	// who set it believes it is doing something.
	//
	// What it is *not* is reinterpreted. Neither reading is safe: a socket name
	// is not a directory, and a name that used to hold every project at once is
	// not the directory that now holds one socket per project. Treating it as
	// either would move every runtime to a path the user never named, silently.
	if cfg.Terminal.Socket != "" {
		where := cfg.TmuxSocketDir()
		if where == "" {
			where = "the default directory under the data directory"
		}
		cfg.addWarning(fmt.Sprintf(
			"terminal.socket (%q) is deprecated and has no effect: since Phase 2.5 each project "+
				"runs on its own tmux server, addressed by terminal.tmuxSocketDir (%s) plus the "+
				"project id. Remove the setting.", cfg.Terminal.Socket, where))
	}
	return nil
}

var (
	loggingLevels  = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	loggingFormats = map[string]bool{"text": true, "json": true}
)

func (c *Config) addWarning(msg string) {
	c.Warnings = append(c.Warnings, msg)
}

// Address is the host:port the HTTP server binds to.
func (c *Config) Address() string {
	return net.JoinHostPort(c.Server.Host, strconv.Itoa(c.Server.Port))
}

// SQLitePath returns the absolute path of the SQLite database file.
func (c *Config) SQLitePath() string {
	p := strings.TrimSpace(c.Storage.SQLitePath)
	if p == "" {
		p = DefaultSQLiteFileName
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(c.DataDir, p))
}

// TmuxSocketDir returns the absolute directory holding one tmux socket per
// project, or "" when none was configured.
//
// A relative path is resolved against the data directory rather than the
// working directory, because the sockets are runtime state and belong beside
// the database and the logs - the same rule SQLitePath follows. A directory
// chosen by the process's working directory would silently change when the
// server was started from somewhere else, and every project's runtime would
// move with it.
//
// The empty result is left empty rather than filled with a default, because the
// default's name is the runtime's business: this package decides where under
// the data directory things live, and the session package decides what the
// runtime's own directories are called. See the note on TerminalConfig.
func (c *Config) TmuxSocketDir() string {
	p := strings.TrimSpace(c.Terminal.SocketDir)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if c.DataDir == "" {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(c.DataDir, p))
}

// TmuxBinary returns the tmux executable every project's runtime runs.
//
// Empty means a bare "tmux", resolved on PATH by the session package. It is not
// resolved here: resolving is a filesystem lookup, and a configuration accessor
// that touched the filesystem would make reading the configuration a thing that
// can fail.
func (c *Config) TmuxBinary() string {
	return strings.TrimSpace(c.Terminal.Binary)
}

// ResolvedWebDir returns the absolute path of the built frontend directory.
// A relative path is resolved against workingDir.
func (c *Config) ResolvedWebDir(workingDir string) string {
	p := strings.TrimSpace(c.Web.Dir)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if workingDir == "" {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(workingDir, p))
}

// WriteDefaultFile writes cfg to path as JSON when path does not exist yet.
// It returns true when a file was created.
//
// The file is written so that a first run leaves a documented, editable
// configuration behind, which is what makes "how do I change the Projects
// Root" answerable without reading the source.
func WriteDefaultFile(path string, cfg *Config, perm os.FileMode) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("config: inspect %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("config: create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, fmt.Errorf("config: encode default config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, perm); err != nil {
		return false, fmt.Errorf("config: write %s: %w", path, err)
	}
	return true, nil
}

// splitRoots splits a path-list environment value. It accepts the platform
// path list separator plus a comma, so a value written on one platform is
// still usable on the other.
func splitRoots(v string) []string {
	fields := strings.FieldsFunc(v, func(r rune) bool {
		return r == filepath.ListSeparator || r == ','
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// dedupePaths cleans the list and removes duplicates, comparing
// case-insensitively on platforms with case-insensitive filesystems.
func dedupePaths(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		key := pathutil.Key(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

func envInt(env func(string) (string, bool), name string) (int, bool) {
	v, ok := env(name)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, false
	}
	return n, true
}
