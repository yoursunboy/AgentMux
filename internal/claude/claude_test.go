package claude

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// These tests are about the resolution rules, not about a Claude Code
// installation. Every one of them injects the three things that touch the
// machine - looking a name up on PATH, resolving a symlink, and running the
// version command - so the suite produces the same answers on a laptop with
// Claude Code installed, on a build server without it, and on Windows.

// fixedTime is a clock that does not advance, so a cache test can control time
// rather than wait for it.
type fixedTime struct {
	now   time.Time
	steps int
}

func (f *fixedTime) Now() time.Time {
	f.now = f.now.Add(time.Duration(f.steps) * time.Second)
	return f.now
}

// fakeMachine is a machine described entirely by what the test says is on it.
type fakeMachine struct {
	// paths maps a name to the path it resolves to.
	paths map[string]string
	// links maps a path to what it points at.
	links map[string]string
	// outputs maps a path to what its version command prints, verbatim.
	//
	// It is the raw output and not the version, because the fake parses it the
	// way runVersion does. A test can therefore give it the output a real Claude
	// Code prints - measured as "2.1.274 (Claude Code)" - and check the whole
	// path from the CLI's bytes to an Installation.
	outputs map[string]string
	// failures maps a path to the error its version command returns.
	failures map[string]error

	// probed records every path whose version was asked for, so a test can
	// check that a cache prevented a second probe.
	probed []string
}

func newFakeMachine() *fakeMachine {
	return &fakeMachine{
		paths:    map[string]string{},
		links:    map[string]string{},
		outputs:  map[string]string{},
		failures: map[string]error{},
	}
}

// launcher builds a launcher whose whole view of the machine is this fake.
func (m *fakeMachine) launcher(t *testing.T, binary string, now func() time.Time) *Launcher {
	t.Helper()
	return New(Options{
		Binary: binary,
		Now:    now,
		LookPath: func(name string) (string, error) {
			if path, ok := m.paths[name]; ok {
				return path, nil
			}
			return "", errors.New("executable file not found in $PATH")
		},
		EvalSymlinks: func(path string) (string, error) {
			if target, ok := m.links[path]; ok {
				return target, nil
			}
			return path, nil
		},
		// The probe's contract is runVersion's: it runs the command and answers
		// with the version, or with an error saying it did not get one. The fake
		// parses the output rather than being handed a version so that the two
		// cannot drift.
		ProbeVersion: func(_ context.Context, path string) (string, error) {
			m.probed = append(m.probed, path)
			if err, ok := m.failures[path]; ok {
				return "", err
			}
			output, ok := m.outputs[path]
			if !ok {
				return "", errors.New("it did not report a version")
			}
			version := parseVersion(output)
			if version == "" {
				return "", errors.New("it did not report a version")
			}
			return version, nil
		},
	})
}

// TestResolveReportsAnInstalledCLI is the ordinary case: a name on PATH, a
// symlink into a versioned directory, and a version command that answers.
func TestResolveReportsAnInstalledCLI(t *testing.T) {
	machine := newFakeMachine()
	machine.paths["claude"] = "/home/user/.local/bin/claude"
	machine.links["/home/user/.local/bin/claude"] = "/home/user/.local/share/claude/versions/2.1.274"
	machine.outputs["/home/user/.local/share/claude/versions/2.1.274"] = "2.1.274 (Claude Code)\n"

	got := machine.launcher(t, "", time.Now).Resolve(context.Background())

	if !got.Available {
		t.Fatalf("the CLI was reported unavailable: %s", got.Message)
	}
	if got.Type != Type {
		t.Errorf("Type = %q, want %q", got.Type, Type)
	}
	if got.Version != "2.1.274" {
		t.Errorf("Version = %q, want %q", got.Version, "2.1.274")
	}
	if got.Binary != DefaultBinary {
		t.Errorf("Binary = %q, want the default %q", got.Binary, DefaultBinary)
	}
	wantPath := "/home/user/.local/share/claude/versions/2.1.274"
	if got.Path != wantPath {
		t.Errorf("Path = %q, want the symlink resolved to %q", got.Path, wantPath)
	}
	if got.Command != Quote(wantPath) {
		t.Errorf("Command = %q, want the quoted resolved path %q", got.Command, Quote(wantPath))
	}
}

// TestResolveUsesTheConfiguredBinary checks that the setting decides which
// program is resolved, and that nothing reaches PATH behind its back.
func TestResolveUsesTheConfiguredBinary(t *testing.T) {
	machine := newFakeMachine()
	machine.paths["/opt/claude/bin/claude"] = "/opt/claude/bin/claude"
	machine.outputs["/opt/claude/bin/claude"] = "1.2.3\n"
	// A different CLI is on PATH. It must not be the one that is found.
	machine.paths["claude"] = "/usr/bin/claude"
	machine.outputs["/usr/bin/claude"] = "9.9.9\n"

	machine.launcher(t, "", time.Now).Resolve(context.Background())
	if len(machine.probed) != 1 || machine.probed[0] != "/usr/bin/claude" {
		t.Fatalf("an unconfigured launcher probed %v, want the PATH claude", machine.probed)
	}

	// Note the path, not the name: this is a full path, and a full path is used
	// as given rather than searched for.
	got := machine.launcher(t, "/opt/claude/bin/claude", time.Now).Resolve(context.Background())
	if !got.Available {
		t.Fatalf("the configured CLI was reported unavailable: %s", got.Message)
	}
	if got.Version != "1.2.3" {
		t.Errorf("Version = %q, want the configured binary's %q", got.Version, "1.2.3")
	}
	if got.Binary != "/opt/claude/bin/claude" {
		t.Errorf("Binary = %q, want the setting as given", got.Binary)
	}
}

// TestResolveReportsAMissingCLIIsAnAnswer is the property the server depends
// on: a machine without Claude Code still starts, and says what is missing.
func TestResolveReportsAMissingCLIIsAnAnswer(t *testing.T) {
	got := newFakeMachine().launcher(t, "", time.Now).Resolve(context.Background())

	if got.Available {
		t.Fatal("a machine with nothing installed reported an available CLI")
	}
	if got.Message == "" {
		t.Error("an unavailable CLI must say why")
	}
	if !strings.Contains(got.Message, "terminal.claudeBinary") {
		t.Errorf("the message does not say how to fix it: %q", got.Message)
	}
	if got.Path != "" || got.Command != "" {
		t.Errorf("an unresolved CLI reported Path %q and Command %q", got.Path, got.Command)
	}
	// The raw LookPath error names a path, which is not what a user needs.
	if strings.Contains(got.Message, "$PATH") && strings.Contains(got.Message, "executable file not found") {
		t.Errorf("the message forwards the lookup error verbatim: %q", got.Message)
	}
}

// TestResolveReportsACLIThatCannotRun covers the case that matters most: the
// program is there and does not work. Reporting it available would move the
// failure to the moment a user pressed Start.
func TestResolveReportsACLIThatCannotRun(t *testing.T) {
	machine := newFakeMachine()
	machine.paths["claude"] = "/usr/local/bin/claude"
	machine.failures["/usr/local/bin/claude"] = errors.New("permission denied")

	got := machine.launcher(t, "", time.Now).Resolve(context.Background())

	if got.Available {
		t.Fatal("a CLI that cannot run was reported as available")
	}
	if !strings.Contains(got.Message, "permission denied") {
		t.Errorf("Message = %q, want it to carry the reason the CLI could not run", got.Message)
	}
}

// TestResolveRejectsOutputThatIsNotAVersion checks that a program which is not
// Claude Code is not reported as one.
//
// The program here is on PATH under the name claude and answers --version with
// something that is not a version, so the probe refuses it. The refusal is what
// makes this an unavailable installation rather than an available one with a
// version string of "usage: something-else [flags]".
func TestResolveRejectsOutputThatIsNotAVersion(t *testing.T) {
	machine := newFakeMachine()
	machine.paths["claude"] = "/usr/bin/something-else"
	machine.outputs["/usr/bin/something-else"] = "usage: something-else [flags]\n"

	got := machine.launcher(t, "", time.Now).Resolve(context.Background())
	if got.Available {
		t.Errorf("output that is not a version was accepted: Version = %q", got.Version)
	}
	if got.Version != "" {
		t.Errorf("Version = %q, want it empty for a program that reported none", got.Version)
	}
	if got.Command != "" {
		t.Errorf("Command = %q, want it empty: nothing was resolved to run", got.Command)
	}
}

// TestResolveCachesTheVersionProbe checks the cost property: the probe spawns
// the CLI, and the API asks for this on every server-status request.
func TestResolveCachesTheVersionProbe(t *testing.T) {
	machine := newFakeMachine()
	machine.paths["claude"] = "/usr/bin/claude"
	machine.outputs["/usr/bin/claude"] = "2.1.274 (Claude Code)\n"

	clock := &fixedTime{now: time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)}
	launcher := machine.launcher(t, "", clock.Now)

	for i := 0; i < 3; i++ {
		if got := launcher.Resolve(context.Background()); !got.Available {
			t.Fatalf("resolve %d reported the CLI unavailable: %s", i, got.Message)
		}
	}
	if len(machine.probed) != 1 {
		t.Errorf("the version command ran %d times within the cache window, want 1", len(machine.probed))
	}

	// Past the window the CLI is asked again: Claude Code updates itself in
	// place, so a cached version is a claim that expires.
	clock.steps = int(versionCacheTTL/time.Second) + 1
	launcher.Resolve(context.Background())
	if len(machine.probed) != 2 {
		t.Errorf("the version command ran %d times after the cache expired, want 2", len(machine.probed))
	}
}

// TestResolveCarriesNoCredential is the security property, written as a test so
// that a later change has to break a test rather than pass review.
func TestResolveCarriesNoCredential(t *testing.T) {
	machine := newFakeMachine()
	machine.paths["claude"] = "/usr/bin/claude"
	machine.outputs["/usr/bin/claude"] = "2.1.274 (Claude Code)\n"

	got := machine.launcher(t, "", time.Now).Resolve(context.Background())

	// Every field is rendered and searched. The point is not that a particular
	// secret is absent; it is that there is no field for one to arrive in.
	rendered := strings.Join([]string{
		got.Type, got.Version, got.Binary, got.Path, got.Command, got.Message,
	}, " ")
	for _, forbidden := range []string{
		"sk-ant", "api_key", "apiKey", "ANTHROPIC", "token", "Bearer", "oauth", "credentials", "signed in",
	} {
		if strings.Contains(strings.ToLower(rendered), strings.ToLower(forbidden)) {
			t.Errorf("the installation report contains %q: %q", forbidden, rendered)
		}
	}
}

// TestQuoteMakesOneShellWord checks the property the launch depends on: a path
// with a space in it arrives as one argument.
func TestQuoteMakesOneShellWord(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"an ordinary path", "/usr/local/bin/claude", "'/usr/local/bin/claude'"},
		{"a path with a space", "/mnt/c/Program Files/claude", "'/mnt/c/Program Files/claude'"},
		{"a bare name", "claude", "'claude'"},
		{"a path with a quote", "/opt/it's here/claude", `'/opt/it'\''s here/claude'`},
		{"an empty path", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Quote(tc.path); got != tc.want {
				t.Errorf("Quote(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestParseVersion reads the CLI's own answer, measured on 2.1.274:
// `2.1.274 (Claude Code)`.
func TestParseVersion(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"the measured output", "2.1.274 (Claude Code)\n", "2.1.274"},
		{"a v prefix", "v1.2.3\n", "1.2.3"},
		{"a prerelease suffix", "1.0.0-beta.1", "1.0.0-beta.1"},
		{"extra whitespace", "  3.4.5  \n\n", "3.4.5"},
		{"nothing at all", "", ""},
		{"prose", "Claude Code, version two", ""},
		{"a trailing dot", "2.1.", ""},
		{"a leading dot", ".1.2", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseVersion(tc.out); got != tc.want {
				t.Errorf("parseVersion(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}
