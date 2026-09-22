package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
// TestYAMLAndJSONProduceTheSameConfiguration is the reason the YAML reader is
// a front end to the JSON one rather than a second decoder.
//
// The same settings are written in both formats, in YAML by hand so that the
// test proves YAML syntax is read rather than that a marshaller can round-trip
// its own output, and the two resolved configurations are compared whole. A key
// that one format accepted and the other ignored, a nested section that landed
// in the wrong place, or a list that was read as a string would all show up
// here as a difference - and would otherwise show up in production as a setting
// that works in one file and is silently dropped in the other.
func TestYAMLAndJSONProduceTheSameConfiguration(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()

	jsonPath := filepath.Join(dir, "config.json")
	jsonBody, err := json.Marshal(map[string]any{
		"server": map[string]any{
			"host": "10.0.0.1", "port": 1111, "debug": true, "controlGraceSeconds": 8,
		},
		"storage":  map[string]any{"sqlitePath": "state.db"},
		"runtime":  map[string]any{"mode": "native", "distro": "Ubuntu-24.04"},
		"logging":  map[string]any{"level": "warn", "format": "json"},
		"projects": map[string]any{"roots": []string{root}, "discoveryDepth": 2},
		"web":      map[string]any{"dir": "public"},
	})
	if err != nil {
		t.Fatalf("could not encode the JSON fixture: %v", err)
	}
	if err := os.WriteFile(jsonPath, jsonBody, 0o644); err != nil {
		t.Fatalf("could not write the JSON fixture: %v", err)
	}

	// A comment, a blank line and a nested list, which is what the checked-in
	// example in config/ is made of and what a JSON reader could not have.
	yamlPath := filepath.Join(dir, "agentmux.yaml")
	yamlBody := "# AgentMux configuration, in the format the deployment docs use.\n" +
		"server:\n" +
		"  host: 10.0.0.1\n" +
		"\n" +
		"  port: 1111\n" +
		"  debug: true\n" +
		"  controlGraceSeconds: 8\n" +
		"storage:\n" +
		"  sqlitePath: state.db\n" +
		"runtime:\n" +
		"  mode: native\n" +
		"  distro: Ubuntu-24.04\n" +
		"logging:\n" +
		"  level: warn\n" +
		"  format: json\n" +
		"projects:\n" +
		"  roots:\n" +
		"    - " + root + "\n" +
		"  discoveryDepth: 2\n" +
		"web:\n" +
		"  dir: public\n"
	if err := os.WriteFile(yamlPath, []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("could not write the YAML fixture: %v", err)
	}

	load := func(path string) *Config {
		t.Helper()
		cfg, err := Load(LoadOptions{
			Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dir},
			Overrides: Overrides{ConfigPath: path},
			Environ:   envMap(nil),
		})
		if err != nil {
			t.Fatalf("Load(%s) returned an error: %v", filepath.Base(path), err)
		}
		// The file it came from is the one thing that is meant to differ.
		cfg.SourceFile = ""
		return cfg
	}

	fromJSON := load(jsonPath)
	fromYAML := load(yamlPath)

	// Spot checks first, so a failure says which setting is wrong before a
	// whole-struct diff says only that something is.
	for _, tc := range []struct {
		name string
		got  any
		want any
	}{
		{"server.host", fromYAML.Server.Host, "10.0.0.1"},
		{"server.port", fromYAML.Server.Port, 1111},
		{"server.debug", fromYAML.Server.Debug, true},
		{"server.controlGraceSeconds", fromYAML.Server.ControlGraceSec, 8},
		{"storage.sqlitePath", fromYAML.Storage.SQLitePath, "state.db"},
		{"runtime.mode", fromYAML.Runtime.Mode, RuntimeModeNative},
		{"runtime.distro", fromYAML.Runtime.Distro, "Ubuntu-24.04"},
		{"logging.level", fromYAML.Logging.Level, "warn"},
		{"logging.format", fromYAML.Logging.Format, "json"},
		{"projects.discoveryDepth", fromYAML.Projects.DiscoveryDepth, 2},
		{"web.dir", fromYAML.Web.Dir, "public"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s from YAML = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	if len(fromYAML.Projects.Roots) != 1 || fromYAML.Projects.Roots[0] != root {
		t.Errorf("projects.roots from YAML = %v, want [%q]", fromYAML.Projects.Roots, root)
	}

	if !reflect.DeepEqual(fromJSON, fromYAML) {
		t.Errorf("the two formats produced different configurations:\nJSON: %+v\nYAML: %+v",
			fromJSON, fromYAML)
	}
}

// TestYAMLFilesAreRecognisedByName pins which names ask for the YAML reader, so
// that a file called config.yaml is never handed to the JSON decoder and
// reported as a syntax error.
func TestYAMLFilesAreRecognisedByName(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"agentmux.yaml", true},
		{"agentmux.yml", true},
		{"AGENTMUX.YAML", true},
		{"config.json", false},
		{"config", false},
		{"config.yaml.bak", false},
	} {
		if got := isYAMLPath(tc.path); got != tc.want {
			t.Errorf("isYAMLPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestEmptyYAMLFileIsAnEmptyConfiguration covers the file a person leaves
// behind after emptying it to start again: comments and nothing else. It is an
// empty configuration, not a parse failure, and the defaults apply.
func TestEmptyYAMLFileIsAnEmptyConfiguration(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(dir, "agentmux.yaml")

	body := "# Everything here has been removed. The defaults apply.\n\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("could not write the fixture: %v", err)
	}

	cfg, err := Load(LoadOptions{
		Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dir},
		Overrides: Overrides{ConfigPath: path},
		Environ:   envMap(nil),
	})
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	if cfg.Server.Port != DefaultServerPort {
		t.Errorf("Server.Port = %d, want the default %d", cfg.Server.Port, DefaultServerPort)
	}
	if cfg.SourceFile != path {
		t.Errorf("SourceFile = %q, want %q; an empty file was still read", cfg.SourceFile, path)
	}
}

// TestYAMLSyntaxErrorNamesTheFile checks the failure a person is most likely to
// meet: a typo in a file they are editing.
func TestYAMLSyntaxErrorNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(dir, "agentmux.yaml")

	// A tab where the indentation needs spaces. This is the classic mistake and
	// the reason the parser's own message, with its line number, is passed
	// through rather than replaced with something the config package wrote.
	if err := os.WriteFile(path, []byte("server:\n\tport: 1111\n"), 0o644); err != nil {
		t.Fatalf("could not write the fixture: %v", err)
	}

	_, err := Load(LoadOptions{
		Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dir},
		Overrides: Overrides{ConfigPath: path},
		Environ:   envMap(nil),
	})
	if err == nil {
		t.Fatal("Load accepted a file that is not valid YAML")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file it came from", err)
	}
}

// TestYAMLScalarDocumentIsRefused covers a file that parses but is not a
// configuration: a bare string, a list, a number. It must be an error rather
// than a silent no-op, because a person who wrote one believes they configured
// something.
func TestYAMLScalarDocumentIsRefused(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()

	for _, body := range []string{"just a string\n", "- one\n- two\n", "42\n"} {
		path := filepath.Join(dir, "agentmux.yaml")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("could not write the fixture: %v", err)
		}
		_, err := Load(LoadOptions{
			Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dir},
			Overrides: Overrides{ConfigPath: path},
			Environ:   envMap(nil),
		})
		if err == nil {
			t.Errorf("Load accepted %q as a configuration file", strings.TrimSpace(body))
		}
	}
}

// TestDebugPrecedence pins server.debug to the documented order, and pins the
// direction it can be moved in.
//
// The flag can only turn debug on. That is deliberate and is asserted here
// rather than left to a comment: a configuration file that turned debug on can
// be turned off again by editing the file, and a test that expected
// "-debug=false" to beat the file would be asserting a behaviour the flag does
// not have.
func TestDebugPrecedence(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	path := filepath.Join(dir, "agentmux.yaml")

	if err := os.WriteFile(path, []byte("server:\n  debug: true\n"), 0o644); err != nil {
		t.Fatalf("could not write the fixture: %v", err)
	}
	base := func() LoadOptions {
		return LoadOptions{
			Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dir},
			Overrides: Overrides{ConfigPath: path},
			Environ:   envMap(nil),
		}
	}

	t.Run("off by default", func(t *testing.T) {
		cfg, err := Load(LoadOptions{
			Defaults: Defaults{ProjectsRoots: []string{root}, DataDir: dir},
			Environ:  envMap(nil),
		})
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if cfg.Server.Debug {
			t.Error("Server.Debug is on with nothing asking for it; it must be opt-in")
		}
	})

	t.Run("the file turns it on", func(t *testing.T) {
		cfg, err := Load(base())
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if !cfg.Server.Debug {
			t.Error("Server.Debug is off though the config file set it")
		}
	})

	t.Run("the environment overrides the file", func(t *testing.T) {
		opts := base()
		opts.Environ = envMap(map[string]string{EnvDebug: "false"})
		cfg, err := Load(opts)
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if cfg.Server.Debug {
			t.Errorf("Server.Debug is on though %s said false", EnvDebug)
		}
	})

	t.Run("a flag turns it on where the environment said no", func(t *testing.T) {
		opts := base()
		opts.Environ = envMap(map[string]string{EnvDebug: "false"})
		opts.Overrides.Debug = true
		cfg, err := Load(opts)
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if !cfg.Server.Debug {
			t.Error("Server.Debug is off though the flag asked for it")
		}
	})
}

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

// TestSplitRoots pins the two ways a list of roots may be written: the
// platform's list separator, and a comma.
//
// The separator is taken from filepath.ListSeparator rather than spelled as a
// literal ";" in the fixture. On Windows ";" separates and the fixture below
// asks for three roots; on Unix it does not and the same fixture asks for two.
// The test was written on Windows and had never run anywhere else, so it was
// asserting a Windows-only reading of a portable function.
func TestSplitRoots(t *testing.T) {
	sep := string(filepath.ListSeparator)
	got := splitRoots(" /a , /b " + sep + sep + " /c ,, ")
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

// TestSplitRootsKeepsAPathThatIsNotASeparatorHere is the other half of the rule
// above, stated rather than left to be inferred: only this platform's separator
// and the comma split, so the character the other platform uses is ordinary
// path content and a root containing one survives whole.
func TestSplitRootsKeepsAPathThatIsNotASeparatorHere(t *testing.T) {
	other := ";"
	if filepath.ListSeparator == ';' {
		other = ":"
	}
	root := "/a" + other + "b"
	got := splitRoots(root)
	if len(got) != 1 || got[0] != root {
		t.Errorf("splitRoots(%q) = %q, want [%q]: %q does not separate on this platform",
			root, got, root, other)
	}
}

// samePath compares paths the way the host filesystem does.
func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
