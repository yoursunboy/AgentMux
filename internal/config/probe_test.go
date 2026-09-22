package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfigFile writes a configuration file into a directory and fails the
// test if it cannot.
func writeConfigFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("could not write %s: %v", path, err)
	}
	return path
}

// loadFromDir resolves the configuration for a data directory that is expected
// to already hold whatever file the case is about.
func loadFromDir(t *testing.T, dataDir, root string) (*Config, error) {
	t.Helper()
	return Load(LoadOptions{
		Defaults: Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
		Environ:  envMap(nil),
		ReadFile: os.ReadFile,
	})
}

// TestTheImplicitConfigFilePrefersYAML pins the probe order.
//
// The order is the phase's whole configuration change: a person who follows the
// documentation and drops a config.yaml into the data directory must have it
// read, and an installation that already has a config.json must keep loading
// without being migrated. Both halves are asserted here because getting one
// right and the other wrong is exactly how this change could break somebody.
func TestTheImplicitConfigFilePrefersYAML(t *testing.T) {
	t.Run("a config.yaml is read", func(t *testing.T) {
		dataDir, root := t.TempDir(), t.TempDir()
		yamlPath := writeConfigFile(t, dataDir, "config.yaml", "server:\n  port: 9911\n")

		cfg, err := loadFromDir(t, dataDir, root)
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if cfg.Server.Port != 9911 {
			t.Errorf("Server.Port = %d, want 9911 from the YAML file", cfg.Server.Port)
		}
		if cfg.SourceFile != yamlPath {
			t.Errorf("SourceFile = %q, want %q", cfg.SourceFile, yamlPath)
		}
	})

	t.Run("a config.yml is read", func(t *testing.T) {
		dataDir, root := t.TempDir(), t.TempDir()
		ymlPath := writeConfigFile(t, dataDir, "config.yml", "server:\n  port: 9912\n")

		cfg, err := loadFromDir(t, dataDir, root)
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if cfg.Server.Port != 9912 {
			t.Errorf("Server.Port = %d, want 9912 from the .yml file", cfg.Server.Port)
		}
		if cfg.SourceFile != ymlPath {
			t.Errorf("SourceFile = %q, want %q", cfg.SourceFile, ymlPath)
		}
	})

	// The compatibility half. An installation made before this phase has a
	// config.json and nothing else, and it must not need a flag to keep working.
	t.Run("an existing config.json still loads", func(t *testing.T) {
		dataDir, root := t.TempDir(), t.TempDir()
		jsonPath := writeConfigFile(t, dataDir, "config.json", `{"server":{"port":9913}}`)

		cfg, err := loadFromDir(t, dataDir, root)
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if cfg.Server.Port != 9913 {
			t.Errorf("Server.Port = %d, want 9913 from the JSON file", cfg.Server.Port)
		}
		if cfg.SourceFile != jsonPath {
			t.Errorf("SourceFile = %q, want %q", cfg.SourceFile, jsonPath)
		}
	})

	t.Run("YAML wins when both are present, and says so", func(t *testing.T) {
		dataDir, root := t.TempDir(), t.TempDir()
		yamlPath := writeConfigFile(t, dataDir, "config.yaml", "server:\n  port: 9914\n")
		writeConfigFile(t, dataDir, "config.json", `{"server":{"port":9915}}`)

		cfg, err := loadFromDir(t, dataDir, root)
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if cfg.Server.Port != 9914 || cfg.SourceFile != yamlPath {
			t.Errorf("read %s (port %d), want %s", cfg.SourceFile, cfg.Server.Port, yamlPath)
		}
		// Silence is the failure mode this warning exists to prevent: the person
		// edits the file that is not read and nothing appears to happen.
		var found bool
		for _, w := range cfg.Warnings {
			if strings.Contains(w, "config.json") && strings.Contains(w, "config.yaml") {
				found = true
			}
		}
		if !found {
			t.Errorf("Warnings = %q, want one naming both files", cfg.Warnings)
		}
	})

	t.Run("no file at all is a first run, not an error", func(t *testing.T) {
		dataDir, root := t.TempDir(), t.TempDir()

		cfg, err := loadFromDir(t, dataDir, root)
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if cfg.SourceFile != "" {
			t.Errorf("SourceFile = %q, want empty", cfg.SourceFile)
		}
		if cfg.Server.Port != DefaultServerPort {
			t.Errorf("Server.Port = %d, want the default %d", cfg.Server.Port, DefaultServerPort)
		}
	})
}

// TestAnExplicitConfigPathIsNotProbed pins the other half of the rule: a caller
// who names a file gets that file. The probe is a convenience for the case where
// nobody has said anything, and it must never turn a named file into a guess.
func TestAnExplicitConfigPathIsNotProbed(t *testing.T) {
	dataDir, root := t.TempDir(), t.TempDir()
	writeConfigFile(t, dataDir, "config.yaml", "server:\n  port: 9916\n")
	jsonPath := writeConfigFile(t, dataDir, "config.json", `{"server":{"port":9917}}`)

	cfg, err := Load(LoadOptions{
		Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
		Overrides: Overrides{ConfigPath: jsonPath},
		Environ:   envMap(nil),
		ReadFile:  os.ReadFile,
	})
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	if cfg.Server.Port != 9917 || cfg.SourceFile != jsonPath {
		t.Errorf("read %s (port %d), want the named %s", cfg.SourceFile, cfg.Server.Port, jsonPath)
	}
	// The file that was not read is not a shadow, because it was never a
	// candidate: no warning, and no ambiguity to report.
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "config.yaml") {
			t.Errorf("Warnings = %q, want none about a file the caller did not name", w)
		}
	}

	// A named file that does not exist is an error rather than a fallback to
	// whatever else happens to be lying around.
	if _, err := Load(LoadOptions{
		Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
		Overrides: Overrides{ConfigPath: filepath.Join(dataDir, "absent.yaml")},
		Environ:   envMap(nil),
		ReadFile:  os.ReadFile,
	}); err == nil {
		t.Error("Load accepted a named config file that does not exist")
	}
}

// TestBetaRecordingOffByDefaultAndTurnedOnDeliberately covers the beta switch
// through the three sources that can set it, plus the precedence between them.
func TestBetaRecordingOffByDefaultAndTurnedOnDeliberately(t *testing.T) {
	root := t.TempDir()

	t.Run("off unless somebody says otherwise", func(t *testing.T) {
		cfg, err := Load(LoadOptions{
			Defaults: Defaults{ProjectsRoots: []string{root}, DataDir: t.TempDir()},
			Environ:  envMap(nil),
		})
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if cfg.Beta.Enabled {
			t.Error("Beta.Enabled is on with nothing asking for it")
		}
	})

	t.Run("the file turns it on", func(t *testing.T) {
		dataDir := t.TempDir()
		writeConfigFile(t, dataDir, "config.yaml", "beta:\n  enabled: true\n")

		cfg, err := loadFromDir(t, dataDir, root)
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if !cfg.Beta.Enabled {
			t.Error("Beta.Enabled is off though the file asked for it")
		}
	})

	t.Run("the environment turns it on", func(t *testing.T) {
		cfg, err := Load(LoadOptions{
			Defaults: Defaults{ProjectsRoots: []string{root}, DataDir: t.TempDir()},
			Environ:  envMap(map[string]string{"AGENTMUX_BETA": "true"}),
		})
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if !cfg.Beta.Enabled {
			t.Error("Beta.Enabled is off though the environment asked for it")
		}
	})

	t.Run("the flag turns it on", func(t *testing.T) {
		cfg, err := Load(LoadOptions{
			Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: t.TempDir()},
			Overrides: Overrides{Beta: true},
			Environ:   envMap(nil),
		})
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if !cfg.Beta.Enabled {
			t.Error("Beta.Enabled is off though the flag asked for it")
		}
	})

	t.Run("the flag beats a file that leaves it off", func(t *testing.T) {
		dataDir := t.TempDir()
		writeConfigFile(t, dataDir, "config.yaml", "beta:\n  enabled: false\n")

		cfg, err := Load(LoadOptions{
			Defaults:  Defaults{ProjectsRoots: []string{root}, DataDir: dataDir},
			Overrides: Overrides{Beta: true},
			Environ:   envMap(nil),
			ReadFile:  os.ReadFile,
		})
		if err != nil {
			t.Fatalf("Load returned an error: %v", err)
		}
		if !cfg.Beta.Enabled {
			t.Error("Beta.Enabled is off though the flag asked for it and the file only declined")
		}
	})

	t.Run("an unparsable value is ignored rather than fatal", func(t *testing.T) {
		cfg, err := Load(LoadOptions{
			Defaults: Defaults{ProjectsRoots: []string{root}, DataDir: t.TempDir()},
			Environ:  envMap(map[string]string{"AGENTMUX_BETA": "yes-please"}),
		})
		if err != nil {
			t.Fatalf("Load returned an error for an unparsable boolean: %v", err)
		}
		if cfg.Beta.Enabled {
			t.Error("Beta.Enabled is on from a value that is not a boolean")
		}
	})
}

// TestTheDefaultFileIsStillWrittenAsJSON pins the decision not to move the
// written default.
//
// The file a first run writes is named by DefaultConfigFileName and encoded as
// JSON, and it is deliberately the last name the probe tries. Writing YAML would
// mean a second encoder over structs that carry only json tags, which is the
// second set of matching rules decodeConfigFile's comment rejects; and it would
// give two installations two different default file names for one behaviour. The
// probe reads YAML first, which is what the phase needed; it does not write it.
func TestTheDefaultFileIsStillWrittenAsJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultConfigFileName)

	cfg, err := Load(LoadOptions{
		Defaults: Defaults{ProjectsRoots: []string{dir}, DataDir: dir},
		Environ:  envMap(nil),
	})
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	created, err := WriteDefaultFile(path, cfg, 0o644)
	if err != nil {
		t.Fatalf("WriteDefaultFile returned an error: %v", err)
	}
	if !created {
		t.Fatal("WriteDefaultFile reported no file was created")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read the file back: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		t.Errorf("the written file does not look like JSON:\n%s", data)
	}

	// And it round-trips: the file the server wrote is a file the server reads.
	again, err := Load(LoadOptions{
		Defaults: Defaults{ProjectsRoots: []string{dir}, DataDir: dir},
		Environ:  envMap(nil),
		ReadFile: os.ReadFile,
	})
	if err != nil {
		t.Fatalf("Load could not read back the file it wrote: %v", err)
	}
	if again.SourceFile != path {
		t.Errorf("SourceFile = %q, want the written %q", again.SourceFile, path)
	}
}
