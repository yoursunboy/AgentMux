package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// envMap turns a map into the Environ hook Load expects.
func envMap(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

// baseLoadOptions returns options that need no config file and no environment,
// with real directories so validation produces no warnings.
func baseLoadOptions(t *testing.T) LoadOptions {
	t.Helper()
	dataDir := t.TempDir()
	root := t.TempDir()
	return LoadOptions{
		Defaults: Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
		Environ:  envMap(nil),
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	opts := baseLoadOptions(t)
	cfg, err := Load(opts)
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}

	if cfg.Server.Host != DefaultServerHost {
		t.Errorf("Server.Host = %q, want %q", cfg.Server.Host, DefaultServerHost)
	}
	if cfg.Server.Port != DefaultServerPort {
		t.Errorf("Server.Port = %d, want %d", cfg.Server.Port, DefaultServerPort)
	}
	if cfg.Projects.DiscoveryDepth != DefaultDiscoveryDepth {
		t.Errorf("DiscoveryDepth = %d, want %d", cfg.Projects.DiscoveryDepth, DefaultDiscoveryDepth)
	}
	if cfg.Logging.Level != DefaultLogLevel || cfg.Logging.Format != DefaultLogFormat {
		t.Errorf("Logging = %+v, want level %q format %q", cfg.Logging, DefaultLogLevel, DefaultLogFormat)
	}
	if cfg.Runtime.Mode != RuntimeModeAuto {
		t.Errorf("Runtime.Mode = %q, want %q", cfg.Runtime.Mode, RuntimeModeAuto)
	}
	if cfg.Runtime.WSLMountRoot != DefaultWSLMountRoot {
		t.Errorf("Runtime.WSLMountRoot = %q, want %q", cfg.Runtime.WSLMountRoot, DefaultWSLMountRoot)
	}
	if cfg.SourceFile != "" {
		t.Errorf("SourceFile = %q, want an empty string when no config file exists", cfg.SourceFile)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("Warnings = %q, want none", cfg.Warnings)
	}

	// The timeouts that would break the future WebSocket transport must stay
	// disabled by default.
	if cfg.Server.ReadTimeoutSec != 0 || cfg.Server.WriteTimeoutSec != 0 {
		t.Errorf("Read/WriteTimeoutSec = %d/%d, want 0/0: a write deadline would break hijacked connections",
			cfg.Server.ReadTimeoutSec, cfg.Server.WriteTimeoutSec)
	}
	if cfg.Server.ReadHeaderTimeoutSec <= 0 {
		t.Error("ReadHeaderTimeoutSec must be set even though the body timeouts are not")
	}
}

// TestLoadPrecedence pins the documented order: file, then environment, then
// command line.
func TestLoadPrecedence(t *testing.T) {
	dataDir := t.TempDir()
	root := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")

	fileBody, err := json.Marshal(map[string]any{
		"server":  map[string]any{"host": "10.0.0.1", "port": 1111},
		"logging": map[string]any{"level": "warn", "format": "json"},
	})
	if err != nil {
		t.Fatalf("could not encode the fixture: %v", err)
	}
	if err := os.WriteFile(configPath, fileBody, 0o644); err != nil {
		t.Fatalf("could not write the fixture: %v", err)
	}

	t.Run("the file overrides defaults", func(t *testing.T) {
		cfg, err := Load(LoadOptions{
			Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
			Overrides: Overrides{ConfigPath: configPath},
			Environ:   envMap(nil),
			ReadFile:  os.ReadFile,
		})
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if cfg.Server.Host != "10.0.0.1" || cfg.Server.Port != 1111 {
			t.Errorf("Server = %s:%d, want 10.0.0.1:1111", cfg.Server.Host, cfg.Server.Port)
		}
		if cfg.Logging.Level != "warn" || cfg.Logging.Format != "json" {
			t.Errorf("Logging = %+v, want warn/json", cfg.Logging)
		}
		if cfg.SourceFile != configPath {
			t.Errorf("SourceFile = %q, want %q", cfg.SourceFile, configPath)
		}
	})

	t.Run("the environment overrides the file", func(t *testing.T) {
		cfg, err := Load(LoadOptions{
			Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
			Overrides: Overrides{ConfigPath: configPath},
			Environ: envMap(map[string]string{
				EnvHost:     "192.168.1.5",
				EnvPort:     "2222",
				EnvLogLevel: "debug",
			}),
		})
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if cfg.Server.Host != "192.168.1.5" || cfg.Server.Port != 2222 {
			t.Errorf("Server = %s:%d, want 192.168.1.5:2222", cfg.Server.Host, cfg.Server.Port)
		}
		if cfg.Logging.Level != "debug" {
			t.Errorf("Logging.Level = %q, want debug", cfg.Logging.Level)
		}
		// The environment said nothing about the format, so the file still wins.
		if cfg.Logging.Format != "json" {
			t.Errorf("Logging.Format = %q, want json from the file", cfg.Logging.Format)
		}
	})

	t.Run("flags override both", func(t *testing.T) {
		cfg, err := Load(LoadOptions{
			Defaults: Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
			Overrides: Overrides{
				ConfigPath:  configPath,
				Host:        "127.0.0.1",
				Port:        3333,
				LogLevel:    "error",
				RuntimeMode: "native",
			},
			Environ: envMap(map[string]string{
				EnvHost:     "192.168.1.5",
				EnvPort:     "2222",
				EnvLogLevel: "debug",
			}),
		})
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if cfg.Server.Port != 3333 {
			t.Errorf("Server.Port = %d, want 3333 from the flag", cfg.Server.Port)
		}
		if cfg.Logging.Level != "error" {
			t.Errorf("Logging.Level = %q, want error from the flag", cfg.Logging.Level)
		}
		if cfg.Runtime.Mode != RuntimeModeNative {
			t.Errorf("Runtime.Mode = %q, want native from the flag", cfg.Runtime.Mode)
		}
	})
}

// TestConfigFileCannotMoveTheDataDirectory covers the circular case: the
// default config file lives inside the data directory, so the file must not be
// able to relocate it.
func TestConfigFileCannotMoveTheDataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	otherDir := t.TempDir()
	root := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")

	body, err := json.Marshal(map[string]any{"dataDir": otherDir})
	if err != nil {
		t.Fatalf("could not encode the fixture: %v", err)
	}
	if err := os.WriteFile(configPath, body, 0o644); err != nil {
		t.Fatalf("could not write the fixture: %v", err)
	}

	cfg, err := Load(LoadOptions{
		Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
		Overrides: Overrides{ConfigPath: configPath},
		Environ:   envMap(nil),
	})
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	if !samePath(cfg.DataDir, dataDir) {
		t.Errorf("DataDir = %q, want %q: the config file must not move it", cfg.DataDir, dataDir)
	}
	if len(cfg.Warnings) == 0 {
		t.Fatal("ignoring dataDir in the file must be reported, not silent")
	}
	if !strings.Contains(strings.Join(cfg.Warnings, " "), "dataDir") {
		t.Errorf("Warnings = %q, want one mentioning dataDir", cfg.Warnings)
	}
}

func TestLoadConfigFileErrors(t *testing.T) {
	dataDir := t.TempDir()
	root := t.TempDir()

	t.Run("an explicit path that does not exist is an error", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent.json")
		_, err := Load(LoadOptions{
			Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
			Overrides: Overrides{ConfigPath: missing},
			Environ:   envMap(nil),
		})
		if err == nil {
			t.Fatal("Load must fail when the config file the user named is missing")
		}
		if !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("error = %v, want it to say the file does not exist", err)
		}
	})

	t.Run("malformed JSON is an error", func(t *testing.T) {
		broken := filepath.Join(dataDir, "broken.json")
		if err := os.WriteFile(broken, []byte("{not json"), 0o644); err != nil {
			t.Fatalf("could not write the fixture: %v", err)
		}
		_, err := Load(LoadOptions{
			Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
			Overrides: Overrides{ConfigPath: broken},
			Environ:   envMap(nil),
		})
		if err == nil {
			t.Fatal("Load must fail on malformed JSON rather than silently using defaults")
		}
	})

	t.Run("an unreadable file is an error", func(t *testing.T) {
		_, err := Load(LoadOptions{
			Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
			Overrides: Overrides{ConfigPath: filepath.Join(dataDir, "config.json")},
			Environ:   envMap(nil),
			ReadFile: func(string) ([]byte, error) {
				return nil, os.ErrPermission
			},
		})
		if err == nil {
			t.Fatal("Load must report a read failure rather than continue with defaults")
		}
	})
}

func TestLoadValidation(t *testing.T) {
	root := t.TempDir()
	dataDir := t.TempDir()
	good := func() LoadOptions {
		return LoadOptions{
			Defaults: Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
			Environ:  envMap(nil),
		}
	}

	tests := []struct {
		name   string
		mutate func(*LoadOptions)
		want   string
	}{
		{
			name:   "port below the valid range",
			mutate: func(o *LoadOptions) { o.Overrides.Port = 70000 },
			want:   "port",
		},
		{
			name:   "unknown runtime mode",
			mutate: func(o *LoadOptions) { o.Overrides.RuntimeMode = "docker" },
			want:   "runtime.mode",
		},
		{
			name:   "unknown log level",
			mutate: func(o *LoadOptions) { o.Overrides.LogLevel = "verbose" },
			want:   "logging.level",
		},
		{
			name:   "unknown log format",
			mutate: func(o *LoadOptions) { o.Overrides.LogFormat = "xml" },
			want:   "logging.format",
		},
		{
			name:   "no projects root",
			mutate: func(o *LoadOptions) { o.Defaults.ProjectsRoots = nil },
			want:   "Projects Root",
		},
		{
			name:   "a relative projects root",
			mutate: func(o *LoadOptions) { o.Defaults.ProjectsRoots = []string{"relative/root"} },
			want:   "absolute",
		},
		{
			name:   "no data directory",
			mutate: func(o *LoadOptions) { o.Defaults.DataDir = "" },
			want:   "data directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := good()
			tt.mutate(&opts)
			_, err := Load(opts)
			if err == nil {
				t.Fatalf("Load accepted an invalid configuration (expected an error mentioning %q)", tt.want)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.want)) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestMissingProjectsRootIsAWarningNotAFailure records the deliberate choice:
// a drive that is not mounted yet must not stop AgentMux from starting.
func TestMissingProjectsRootIsAWarningNotAFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-mounted")

	cfg, err := Load(LoadOptions{
		Defaults: Defaults{ProjectsRoots: []string{missing}, DataDir: t.TempDir()},
		Environ:  envMap(nil),
	})
	if err != nil {
		t.Fatalf("Load failed on a missing Projects Root: %v", err)
	}
	if len(cfg.Warnings) == 0 {
		t.Fatal("a missing Projects Root must be reported as a warning")
	}
	if !strings.Contains(strings.Join(cfg.Warnings, " "), "not-mounted") {
		t.Errorf("Warnings = %q, want one naming the missing root", cfg.Warnings)
	}
}

func TestProjectsRootsFromTheEnvironment(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	sep := string(filepath.ListSeparator)

	cfg, err := Load(LoadOptions{
		Defaults: Defaults{DataDir: t.TempDir()},
		Environ:  envMap(map[string]string{EnvProjectsRoots: first + sep + second}),
	})
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	if len(cfg.Projects.Roots) != 2 {
		t.Fatalf("Projects.Roots = %q, want two entries", cfg.Projects.Roots)
	}
	if !samePath(cfg.Projects.Roots[0], first) || !samePath(cfg.Projects.Roots[1], second) {
		t.Errorf("Projects.Roots = %q, want [%q %q]", cfg.Projects.Roots, first, second)
	}
}

// TestProjectsRootsAreDeduplicated matters because a duplicated root would
// appear twice in the UI and be scanned twice.
func TestProjectsRootsAreDeduplicated(t *testing.T) {
	root := t.TempDir()

	cfg, err := Load(LoadOptions{
		Defaults: Defaults{
			ProjectsRoots: []string{root, root + string(filepath.Separator), "", "  "},
			DataDir:       t.TempDir(),
		},
		Environ: envMap(nil),
	})
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	if len(cfg.Projects.Roots) != 1 {
		t.Errorf("Projects.Roots = %q, want a single entry", cfg.Projects.Roots)
	}
}

func TestInvalidEnvironmentIntegersAreIgnored(t *testing.T) {
	cfg, err := Load(LoadOptions{
		Defaults: Defaults{ProjectsRoots: []string{t.TempDir()}, DataDir: t.TempDir()},
		Environ:  envMap(map[string]string{EnvPort: "not-a-number"}),
	})
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	if cfg.Server.Port != DefaultServerPort {
		t.Errorf("Server.Port = %d, want the default %d when the environment value is unusable",
			cfg.Server.Port, DefaultServerPort)
	}
}

func TestNormalisation(t *testing.T) {
	cfg, err := Load(LoadOptions{
		Defaults: Defaults{ProjectsRoots: []string{t.TempDir()}, DataDir: t.TempDir()},
		Overrides: Overrides{
			Host:        "  127.0.0.1  ",
			LogLevel:    "  INFO  ",
			RuntimeMode: "  NATIVE  ",
		},
		Environ: envMap(map[string]string{EnvWSLMountRoot: "mnt/"}),
	})
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("Server.Host = %q, want it trimmed", cfg.Server.Host)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("Logging.Level = %q, want it lower-cased and trimmed", cfg.Logging.Level)
	}
	if cfg.Runtime.Mode != RuntimeModeNative {
		t.Errorf("Runtime.Mode = %q, want it lower-cased and trimmed", cfg.Runtime.Mode)
	}
	if cfg.Runtime.WSLMountRoot != "/mnt" {
		t.Errorf("WSLMountRoot = %q, want %q", cfg.Runtime.WSLMountRoot, "/mnt")
	}
}

func TestAddress(t *testing.T) {
	cfg := &Config{Server: ServerConfig{Host: "127.0.0.1", Port: 8787}}
	if got := cfg.Address(); got != "127.0.0.1:8787" {
		t.Errorf("Address() = %q, want %q", got, "127.0.0.1:8787")
	}
	// A wildcard bind must still produce a well-formed address.
	cfg.Server.Host = "::"
	if got := cfg.Address(); got != "[::]:8787" {
		t.Errorf("Address() = %q, want %q", got, "[::]:8787")
	}
}

func TestSQLitePath(t *testing.T) {
	dataDir := t.TempDir()

	t.Run("a relative name is placed in the data directory", func(t *testing.T) {
		cfg := &Config{DataDir: dataDir, Storage: StorageConfig{SQLitePath: DefaultSQLiteFileName}}
		want := filepath.Join(dataDir, DefaultSQLiteFileName)
		if got := cfg.SQLitePath(); got != want {
			t.Errorf("SQLitePath() = %q, want %q", got, want)
		}
	})

	t.Run("an absolute path is used as-is", func(t *testing.T) {
		absolute := filepath.Join(t.TempDir(), "elsewhere.db")
		cfg := &Config{DataDir: dataDir, Storage: StorageConfig{SQLitePath: absolute}}
		if got := cfg.SQLitePath(); got != absolute {
			t.Errorf("SQLitePath() = %q, want %q", got, absolute)
		}
	})

	t.Run("an empty path falls back to the default name", func(t *testing.T) {
		cfg := &Config{DataDir: dataDir}
		want := filepath.Join(dataDir, DefaultSQLiteFileName)
		if got := cfg.SQLitePath(); got != want {
			t.Errorf("SQLitePath() = %q, want %q", got, want)
		}
	})
}

func TestResolvedWebDir(t *testing.T) {
	workingDir := t.TempDir()

	cfg := &Config{Web: WebConfig{Dir: DefaultWebDir}}
	if got, want := cfg.ResolvedWebDir(workingDir), filepath.Join(workingDir, "web", "dist"); got != want {
		t.Errorf("ResolvedWebDir() = %q, want %q", got, want)
	}

	absolute := t.TempDir()
	cfg.Web.Dir = absolute
	if got := cfg.ResolvedWebDir(workingDir); got != absolute {
		t.Errorf("ResolvedWebDir() = %q, want %q", got, absolute)
	}

	cfg.Web.Dir = "web/dist"
	if got := cfg.ResolvedWebDir(""); got != filepath.Clean("web/dist") {
		t.Errorf("ResolvedWebDir(\"\") = %q, want a relative result when the working directory is unknown", got)
	}

	cfg.Web.Dir = "  "
	if got := cfg.ResolvedWebDir(workingDir); got != "" {
		t.Errorf("ResolvedWebDir() = %q, want an empty string when static serving is disabled", got)
	}
}

func TestWriteDefaultFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")
	cfg := &Config{
		Server:  ServerConfig{Host: DefaultServerHost, Port: DefaultServerPort},
		Storage: StorageConfig{SQLitePath: DefaultSQLiteFileName},
		Projects: ProjectsConfig{
			Roots:          []string{t.TempDir()},
			DiscoveryDepth: DefaultDiscoveryDepth,
		},
		Logging: LoggingConfig{Level: DefaultLogLevel, Format: DefaultLogFormat},
		Web:     WebConfig{Dir: DefaultWebDir},
		DataDir: t.TempDir(),
	}

	created, err := WriteDefaultFile(path, cfg, 0o644)
	if err != nil {
		t.Fatalf("WriteDefaultFile returned an error: %v", err)
	}
	if !created {
		t.Fatal("WriteDefaultFile reported it created nothing on a fresh path")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the file was reported created but cannot be read: %v", err)
	}
	var roundTripped Config
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("the written file is not valid JSON: %v", err)
	}
	if roundTripped.Server.Port != DefaultServerPort {
		t.Errorf("the written file has port %d, want %d", roundTripped.Server.Port, DefaultServerPort)
	}

	// A second call must not clobber a file the user may have edited.
	if err := os.WriteFile(path, []byte(`{"server":{"port":9999}}`), 0o644); err != nil {
		t.Fatalf("could not overwrite the fixture: %v", err)
	}
	created, err = WriteDefaultFile(path, cfg, 0o644)
	if err != nil {
		t.Fatalf("WriteDefaultFile returned an error on an existing file: %v", err)
	}
	if created {
		t.Error("WriteDefaultFile must not overwrite an existing configuration file")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not re-read the file: %v", err)
	}
	if !strings.Contains(string(after), "9999") {
		t.Error("WriteDefaultFile overwrote user content")
	}
}

func TestSplitRoots(t *testing.T) {
	got := splitRoots(" /a , /b ;; /c ,, ")
	want := []string{"/a", "/b", "/c"}
	if len(got) != len(want) {
		t.Fatalf("splitRoots returned %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitRoots()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if len(splitRoots("")) != 0 {
		t.Error("splitRoots(\"\") must return nothing")
	}
}

// samePath compares paths the way the host filesystem does.
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
