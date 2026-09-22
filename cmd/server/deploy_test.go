package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kutonlagos/agentmux/internal/config"
	"github.com/kutonlagos/agentmux/internal/host"
)

// This file is §十三's deployment test, and it is a contract test rather than a
// deployment test in the sense of "does it come up".
//
// Nothing here starts a server, opens a socket or needs Linux. What it checks is
// that the three artifacts a Linux deployment is made of - the systemd unit, the
// installer, and the example configuration - agree with the program they
// deploy. That is the class of mistake this phase can actually make: the code
// is exercised by the rest of the suite on every run, and the deployment files
// are exercised by nothing at all until somebody runs them on a server they
// cannot get back to easily.
//
// The specific failure it exists for is silent. install.sh installs the unit by
// substituting the paths in it with sed:
//
//	sed -e "s#/opt/agentmux#${PREFIX}#g" ...
//
// A sed pattern that stops matching substitutes nothing and exits successfully.
// Rename the default prefix in the script and the unit keeps pointing at
// /opt/agentmux - a directory nothing installed into - and the failure appears
// on the server as a service that cannot find its own binary. Every assertion
// below is a place where two files have to say the same thing, and none of them
// is checked by compiling anything.

// repoRoot is the repository root, two directories above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("could not resolve the repository root: %v", err)
	}
	return root
}

// readRepoFile reads a file from the repository, failing the test when it is
// missing rather than skpping: a deployment artifact that has been deleted is a
// broken deployment, not an untestable one.
func readRepoFile(t *testing.T, relPath string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(repoRoot(t), relPath))
	if err != nil {
		t.Fatalf("could not read %s: %v", relPath, err)
	}
	// The files are checked in with LF and are installed on Linux; a test run on
	// a Windows checkout with autocrlf on would otherwise compare lines that end
	// in \r against patterns that do not.
	return strings.ReplaceAll(string(content), "\r\n", "\n")
}

// unit is the part of a systemd unit file this package needs: sections,
// assignments, and the two keys that may appear more than once.
//
// It is a small reader rather than a dependency because the subset is genuinely
// small and a general parser would be more code than the thing it parses. What
// it does not implement - drop-ins, continuations, specifiers - is not used by
// the unit below, and a change that introduced one would show up here as a key
// that could not be found rather than as a wrong answer.
type unit struct {
	sections []string
	values   map[string]string
	environ  []string
}

// parseUnit reads a unit file.
func parseUnit(t *testing.T, source string) *unit {
	t.Helper()
	u := &unit{values: map[string]string{}}
	for i, line := range strings.Split(source, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			if name := strings.Trim(line, "[]"); name != "" {
				u.sections = append(u.sections, name)
			} else {
				t.Fatalf("line %d is an empty section header: %q", i+1, line)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("line %d is neither a section nor an assignment: %q", i+1, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "Environment" {
			u.environ = append(u.environ, value)
			continue
		}
		u.values[key] = value
	}
	if len(u.sections) == 0 {
		t.Fatal("the unit file has no sections, so it is not a unit file")
	}
	return u
}

// value returns an assignment, failing the test when it is absent.
func (u *unit) value(t *testing.T, key string) string {
	t.Helper()
	value, ok := u.values[key]
	if !ok {
		t.Fatalf("the unit file has no %s= line", key)
	}
	return value
}

// environment returns the value of an Environment= assignment, failing the test
// when it is absent.
func (u *unit) environment(t *testing.T, name string) string {
	t.Helper()
	for _, assignment := range u.environ {
		if key, value, ok := strings.Cut(assignment, "="); ok && strings.TrimSpace(key) == name {
			return strings.TrimSpace(value)
		}
	}
	t.Fatalf("the unit file sets no %s in an Environment= line", name)
	return ""
}

// installDefaults are the installer's own defaults, read out of the script.
//
// They are read rather than restated because restating them here would be a
// third copy of the same fact: the assertion is that the script and the unit
// agree, and a literal in this file would make the test agree with itself.
type installDefaults map[string]string

// shellAssignment reads `NAME=value` out of a shell script.
func shellAssignment(t *testing.T, script, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `=([^\n]*)`)
	match := pattern.FindStringSubmatch(script)
	if match == nil {
		t.Fatalf("install.sh declares no %s= default", name)
	}
	return strings.TrimSpace(match[1])
}

// installerDefaults reads the five defaults the unit is generated from.
func installerDefaults(t *testing.T) installDefaults {
	t.Helper()
	script := readRepoFile(t, "deploy/linux/install.sh")
	defaults := installDefaults{}
	for _, name := range []string{"PREFIX", "DATA_DIR", "CONFIG_DIR", "SERVICE_USER", "SERVICE_GROUP", "UNIT_NAME", "PORT"} {
		defaults[name] = shellAssignment(t, script, name)
	}
	return defaults
}

// TestTheUnitAndTheInstallerAgree is the central assertion of this file.
//
// Every path in the shipped unit is a default that install.sh rewrites. The two
// therefore have to start out identical, and this checks each pair: the binary
// the unit runs, the directory it runs in, the two environment values it sets,
// and the account it runs as.
func TestTheUnitAndTheInstallerAgree(t *testing.T) {
	defaults := installerDefaults(t)
	u := parseUnit(t, readRepoFile(t, "deploy/linux/agentmux.service"))

	prefix := defaults["PREFIX"]
	execStart := u.value(t, "ExecStart")
	if want := prefix + "/agentmux-server"; execStart != want {
		t.Errorf("ExecStart = %q, want %q", execStart, want)
	}
	if got := u.value(t, "WorkingDirectory"); got != prefix {
		t.Errorf("WorkingDirectory = %q, want %q", got, prefix)
	}
	// The binary has to be inside the working directory, because the frontend's
	// default path is relative to it and is resolved against it. A unit whose
	// ExecStart pointed somewhere else would start a server that could not find
	// its own bundle.
	if !strings.HasPrefix(execStart, prefix+"/") {
		t.Errorf("ExecStart %q is not inside WorkingDirectory %q", execStart, prefix)
	}

	if got := u.environment(t, "AGENTMUX_CONFIG"); got != defaults["CONFIG_DIR"]+"/agentmux.yaml" {
		t.Errorf("AGENTMUX_CONFIG = %q, want the file install.sh writes under %q",
			got, defaults["CONFIG_DIR"])
	}
	if got := u.environment(t, "AGENTMUX_DATA_DIR"); got != defaults["DATA_DIR"] {
		t.Errorf("AGENTMUX_DATA_DIR = %q, want %q", got, defaults["DATA_DIR"])
	}

	if got := u.value(t, "User"); got != defaults["SERVICE_USER"] {
		t.Errorf("User = %q, want %q", got, defaults["SERVICE_USER"])
	}
	if got := u.value(t, "Group"); got != defaults["SERVICE_GROUP"] {
		t.Errorf("Group = %q, want %q", got, defaults["SERVICE_GROUP"])
	}
}

// TestTheInstallerSubstitutionsMatchTheUnit is the silent-failure guard.
//
// install.sh rewrites the unit with one sed expression per default. A sed
// expression whose search pattern matches nothing substitutes nothing and exits
// successfully, so grepping the script for the paths it mentions proves
// nothing: what has to hold is that each expression's *search side* still finds
// a line in the unit file. That is what this runs - every expression, applied
// to the unit as a pattern, line-anchored the way sed applies it.
//
// The failure it is written for: somebody renames the unit's User to
// `agentmux-svc` and updates the installer's SERVICE_USER default to match. The
// script then looks correct, installs cleanly, and produces a unit that still
// says `User=agentmux` with no such account on the machine - because
// `s#^User=.*#User=agentmux-svc#` replaced the text it found with the text it
// found. The service fails to start with "Failed to determine user credentials".
func TestTheInstallerSubstitutionsMatchTheUnit(t *testing.T) {
	script := readRepoFile(t, "deploy/linux/install.sh")
	source := readRepoFile(t, "deploy/linux/agentmux.service")

	// The sed invocation that installs the unit, up to the redirection into it.
	// Matched from the script rather than restated so that the expressions
	// checked are the ones that run.
	block := regexp.MustCompile(`(?s)sed\s+((?:-e\s+"[^"]*"\s*\\?\s*)+)"\$unit_src"\s*>\s*"\$unit_dst"`)
	match := block.FindStringSubmatch(script)
	if match == nil {
		t.Fatal("install.sh no longer contains the sed invocation this test checks; " +
			"read it and update this test rather than deleting it")
	}

	expressions := regexp.MustCompile(`-e\s+"([^"]*)"`).FindAllStringSubmatch(match[1], -1)
	// Five defaults are rewritten into the unit: the three paths and the two
	// credentials. A sixth added to the script is checked too - this only fails
	// if one disappears, which would mean a default is no longer substituted.
	if len(expressions) < 5 {
		t.Fatalf("the installer rewrites the unit with %d expressions, want at least 5: %v",
			len(expressions), expressions)
	}

	for _, expression := range expressions {
		program := expression[1]
		search, ok := sedSearch(program)
		if !ok {
			t.Errorf("could not read %q as a sed substitution, so nothing here checks it", program)
			continue
		}
		// (?m) because sed's ^ is per-line, which is also what the unit's own
		// assignments are.
		pattern, err := regexp.Compile("(?m)" + search)
		if err != nil {
			t.Errorf("the search pattern in %q is not one this test can apply: %v", program, err)
			continue
		}
		if !pattern.MatchString(source) {
			t.Errorf("the unit file has no line matching the search pattern in %q, so that "+
				"substitution replaces nothing and still succeeds", program)
		}
	}
}

// sedSearch returns the search side of an `s#...#...#` program.
//
// The delimiter is taken from the program rather than assumed to be '#', because
// sed's is whatever character follows the s, and an expression written with a
// different one would otherwise be read as an empty pattern and pass.
func sedSearch(program string) (string, bool) {
	if len(program) < 3 || program[0] != 's' {
		return "", false
	}
	delimiter := program[1]
	body := program[2:]
	parts := strings.Split(body, string(delimiter))
	if len(parts) < 2 {
		return "", false
	}
	return parts[0], true
}

// TestTheUnitKeepsTerminalsThroughARestart is §十一 scenario 5's precondition,
// asserted on the file rather than on a running server.
//
// KillMode is the line that decides whether `systemctl restart agentmux`
// preserves the tmux servers the runtimes live in. The systemd default,
// control-group, sends the stop signal to every process in the unit's cgroup -
// and a project's tmux server is one of those. With the default, a restart
// would destroy every session and everything running in one, and the server
// would come back up and correctly report every runtime as stopped, having done
// it itself.
//
// That is a one-word change away from being true at all times, and nothing else
// in this repository would notice.
func TestTheUnitKeepsTerminalsThroughARestart(t *testing.T) {
	u := parseUnit(t, readRepoFile(t, "deploy/linux/agentmux.service"))

	if got := u.value(t, "KillMode"); got != "process" {
		t.Errorf("KillMode = %q, want %q: a restart would end every project's tmux "+
			"server and every session in it", got, "process")
	}
	if got := u.value(t, "Restart"); got != "always" {
		t.Errorf("Restart = %q, want %q", got, "always")
	}
	if got := u.value(t, "Type"); got != "simple" {
		t.Errorf("Type = %q, want %q", got, "simple")
	}
	for _, section := range []string{"Unit", "Service", "Install"} {
		if !contains(u.sections, section) {
			t.Errorf("the unit file has no [%s] section", section)
		}
	}
	if got := u.value(t, "WantedBy"); got != "multi-user.target" {
		t.Errorf("WantedBy = %q, want %q, which is what `systemctl enable` installs",
			got, "multi-user.target")
	}
}

// TestTheUnitSetsOnlyTheTwoValuesItDocuments is the same "exact set" discipline
// the health endpoints are held to.
//
// The unit's own comment says there are two Environment lines and that the
// shortness is the design: anything set here wins over the configuration file,
// so a setting that belongs in /etc/agentmux/agentmux.yaml must not also appear
// here. A third line added later would be a setting an operator could not
// change from the file they were told to edit.
func TestTheUnitSetsOnlyTheTwoValuesItDocuments(t *testing.T) {
	u := parseUnit(t, readRepoFile(t, "deploy/linux/agentmux.service"))

	if len(u.environ) != 2 {
		t.Errorf("the unit sets %d environment values, want exactly 2: %v", len(u.environ), u.environ)
	}
	want := map[string]bool{"AGENTMUX_CONFIG": true, "AGENTMUX_DATA_DIR": true}
	for _, assignment := range u.environ {
		key, _, ok := strings.Cut(assignment, "=")
		if !ok || !want[strings.TrimSpace(key)] {
			t.Errorf("the unit sets %q in the environment; the file cannot carry AGENTMUX_CONFIG "+
				"and must not carry AGENTMUX_DATA_DIR, and nothing else belongs there", assignment)
		}
	}
}

// TestTheInstallerIsValidShell runs the shell's own parser over the script.
//
// It is the only check here that is about the installer's syntax rather than
// its contents, and it is the difference between a script that fails on line
// 120 of an installation and one that fails in a terminal before anything has
// been changed. bash -n parses without executing, so nothing on the machine
// running the tests is touched.
//
// It skips where bash is not installed, which is the honest answer on a Windows
// machine with no Git Bash: the script is for Linux, and a test that could not
// check it is not a test that should fail.
func TestTheInstallerIsValidShell(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed; the installer is for Linux with systemd")
	}

	for _, script := range []string{"deploy/linux/install.sh", "deploy/linux/recovery-test.sh"} {
		t.Run(script, func(t *testing.T) {
			path := filepath.Join(repoRoot(t), filepath.FromSlash(script))
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("could not find %s: %v", script, err)
			}
			out, err := exec.Command(bash, "-n", path).CombinedOutput()
			if err != nil {
				t.Fatalf("bash -n %s failed: %v\n%s", script, err, out)
			}
			if len(strings.TrimSpace(string(out))) > 0 {
				t.Errorf("bash -n %s printed %q", script, out)
			}
		})
	}
}

// TestTheHealthCheckTheInstallerRunsIsTheHealthCheckThatExists ties the
// installer's verification step to the routes the server registers.
//
// install.sh starts the service and then curls it, and that curl is the only
// thing standing between an installation and an operator who believes it
// worked. The paths and the port it uses have to be the ones the server
// answers on, and the server's defaults are the authority for the port.
//
// The two routes are registered in internal/httpapi and are asserted to answer
// there; what is checked here is that the script it curls is spelled the same.
func TestTheHealthCheckTheInstallerRunsIsTheHealthCheckThatExists(t *testing.T) {
	script := readRepoFile(t, "deploy/linux/install.sh")
	defaults := installerDefaults(t)

	if defaults["PORT"] != fmt.Sprint(config.DefaultServerPort) {
		t.Errorf("install.sh writes port %s into a new config file and the server's "+
			"default is %d", defaults["PORT"], config.DefaultServerPort)
	}

	for _, route := range []string{"/health", "/api/health"} {
		if !strings.Contains(script, route) {
			t.Errorf("install.sh never checks %s, so an installation could be reported "+
				"healthy by a server that does not answer it", route)
		}
	}
	if !strings.Contains(script, "127.0.0.1:${PORT}") {
		t.Error("install.sh does not build its health URL from 127.0.0.1 and ${PORT}, " +
			"so it could be checking an address the server does not bind")
	}
}

// linuxProjectsRoot is the root the example names, which is the root
// deploy/linux/README.md and deploy/linux/install.sh both default to.
const linuxProjectsRoot = "/srv/projects"

// hostAcceptsLinuxRoot reports whether this host's loader would accept a Linux
// path as a projects root.
//
// It asks filepath.IsAbs, which is the exact expression internal/config's
// validate uses, so this test and the loader cannot come to different
// conclusions about which hosts can load the file as written.
func hostAcceptsLinuxRoot() bool {
	return filepath.IsAbs(linuxProjectsRoot)
}

// TestTheExampleConfigLoads is the one assertion about configuration that
// matters to a deployment.
//
// config/agentmux.example.yaml is a reference, not a file the server reads, and
// nothing would fail if a key in it were misspelled - an operator would copy it,
// the loader would ignore the key, and the setting would silently be the
// default. Loading it through the real loader is what makes the example a claim
// rather than prose.
//
// It loads the way main.go loads, platform defaults and all. The one thing that
// cannot be asked of every host is the projects root: the example names a Linux
// path, and filepath.IsAbs answers false for it on Windows. So on a host that
// cannot accept it the root is supplied as an override - which is what an
// operator there would have to do anyway - and the file's own spelling of it is
// checked against the text instead. On Linux, which is the environment this file
// is for, the file's own root is what loads.
//
// The values checked are the ones install.sh writes for a default installation,
// so this also pins that the example and the written file agree about what a
// default deployment looks like.
func TestTheExampleConfigLoads(t *testing.T) {
	path := filepath.Join(repoRoot(t), "config", "agentmux.example.yaml")
	example := readRepoFile(t, "config/agentmux.example.yaml")

	overrides := config.Overrides{ConfigPath: path}
	if !hostAcceptsLinuxRoot() {
		overrides.ProjectsRoots = host.DefaultProjectsRoots()
	}

	cfg, err := config.Load(config.LoadOptions{
		Defaults: config.Defaults{
			ProjectsRoots: host.DefaultProjectsRoots(),
			DataDir:       host.DefaultDataDir(),
			TerminalShell: host.DefaultShell(),
		},
		Overrides: overrides,
		// An empty environment, so a stray AGENTMUX_* variable in the
		// developer's shell cannot change what this asserts. The systemd unit
		// sets two of them, and what it sets is checked against the file it
		// points at by TestTheUnitAndTheInstallerAgree above.
		Environ: func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatalf("the example configuration does not load: %v", err)
	}

	// A warning is a key the loader did not understand or a setting it had to
	// correct, and either one in the file that documents every key is a mistake
	// in the documentation.
	for _, warning := range cfg.Warnings {
		t.Errorf("loading the example configuration produced a warning: %s", warning)
	}

	if cfg.SourceFile != path {
		t.Errorf("SourceFile = %q, want %q", cfg.SourceFile, path)
	}
	if cfg.Server.Host != config.DefaultServerHost {
		t.Errorf("the example binds %q; the documented default is %q, which is the one "+
			"install.sh writes and the one docs/SECURITY.md §1 argues for",
			cfg.Server.Host, config.DefaultServerHost)
	}
	if cfg.Server.Port != config.DefaultServerPort {
		t.Errorf("the example names port %d, want %d", cfg.Server.Port, config.DefaultServerPort)
	}
	if cfg.Logging.Level != "info" || cfg.Logging.Format != "text" {
		t.Errorf("the example configures logging as %s/%s, want info/text",
			cfg.Logging.Level, cfg.Logging.Format)
	}

	// The root, from the text when the load could not carry it: the same claim
	// either way, since the value is a literal in the file and not a default
	// resolved from anywhere.
	if hostAcceptsLinuxRoot() {
		if len(cfg.Projects.Roots) != 1 || cfg.Projects.Roots[0] != linuxProjectsRoot {
			t.Errorf("the example names Projects Roots %v, want [%s] - the root "+
				"deploy/linux/README.md and install.sh both default to",
				cfg.Projects.Roots, linuxProjectsRoot)
		}
	} else if !hasYAMLListItem(example, linuxProjectsRoot) {
		t.Errorf("the example no longer names %s as a projects root, which is the root "+
			"deploy/linux/README.md and deploy/linux/install.sh both default to",
			linuxProjectsRoot)
	}
}

// hasYAMLListItem reports whether a block sequence in a YAML file names a value.
//
// It matches on the item's own line rather than on the surrounding indentation,
// so reformatting the file does not fail this: what is being read is that the
// value is still written down, and the loader checked everything else about the
// line already.
func hasYAMLListItem(source, value string) bool {
	for _, line := range strings.Split(source, "\n") {
		if strings.TrimSpace(line) == "- "+value {
			return true
		}
	}
	return false
}

// TestTheExampleConfigIsNotTheFileTheServerWrites pins a decision worth stating
// where somebody would change it.
//
// The loader reads YAML first and writes JSON. That asymmetry is deliberate:
// the probe accepts a .yaml file because that is what the documentation and the
// installer use, while the file a first run writes stays JSON because the
// writer is the same encoder every other part of this program uses, and a YAML
// writer would be a second one that could disagree with the reader about a key.
//
// The consequence is visible here: the shipped example is a hand-written YAML
// reference named agentmux.example.yaml, and the file config.WriteDefaultFile
// produces is config.json. This asserts the two names are different, so that a
// later change that made the writer emit YAML has to come and delete this test
// and read the reason.
func TestTheExampleConfigIsNotTheFileTheServerWrites(t *testing.T) {
	if filepath.Ext(config.DefaultConfigFileName) != ".json" {
		t.Errorf("DefaultConfigFileName is %q; a first run writes JSON on purpose - "+
			"see internal/config, decodeConfigFile and WriteDefaultFile",
			config.DefaultConfigFileName)
	}
	if config.DefaultConfigFileName == "config.yaml" {
		t.Error("the written default file has become config.yaml, which is the first name " +
			"the probe looks for: the two would then be the same file")
	}

	// The probe's order is the other half of the decision, and it is what makes
	// a YAML file left by hand the one that is read.
	if len(config.DefaultConfigFileNames) == 0 || config.DefaultConfigFileNames[0] != "config.yaml" {
		t.Errorf("the implicit config file is probed in the order %v; YAML is first "+
			"because it is what the documentation and install.sh write",
			config.DefaultConfigFileNames)
	}
}
