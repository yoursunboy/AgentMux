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
		Runtime: RuntimeConfig{Mode: RuntimeModeAuto, WSLMountRoot: DefaultWSLMountRoot},
		Logging: LoggingConfig{Level: DefaultLogLevel, Format: DefaultLogFormat},
		Web:     WebConfig{Dir: DefaultWebDir},
		DataDir: d.DataDir,
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
