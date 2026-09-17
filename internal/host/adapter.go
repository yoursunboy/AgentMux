package host

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kutonlagos/agentmux/internal/pathutil"
)

// Built-in host paths. These are defaults only: they are injected into the
// configuration as defaults and can always be overridden. No other package
// may contain them.
const (
	// DefaultWindowsProjectsRoot is the documented Windows Projects Root.
	DefaultWindowsProjectsRoot = `D:\AI\Projects`

	// DefaultWSLProjectsRoot is the documented Projects Root as seen inside
	// WSL2, used as the default when AgentMux itself runs on Linux under WSL.
	DefaultWSLProjectsRoot = "/mnt/d/AI/Projects"

	// wslDistroTimeout bounds the one-off WSL distribution probe.
	wslDistroTimeout = 3 * time.Second
)

// Options configures New.
type Options struct {
	// Mode is "auto", "native", or "wsl". Empty means "auto".
	Mode string

	// Distro is the WSL distribution name. Empty means auto-detect.
	Distro string

	// WSLMountRoot is the mount point for Windows drives inside WSL.
	WSLMountRoot string

	// Roots are the configured Projects Roots, in priority order.
	Roots []string

	// Shell is the shell a terminal session runs. Empty means DefaultShell.
	// It is only used to report whether the runtime has it.
	Shell string

	// TmuxBinary is the tmux executable the terminal runtime runs. Empty means
	// a bare "tmux", resolved on the runtime's PATH.
	//
	// It is here so that the dependency report describes the binary AgentMux
	// will actually run. A machine can have more than one tmux - a distribution
	// package and a newer one built into a user prefix - and a report that
	// checked PATH while the runtime used a configured path would answer a
	// question nobody asked, in the reassuring direction.
	TmuxBinary string
}

// New builds the Adapter for the platform AgentMux is running on.
func New(o Options) (Adapter, error) {
	kind := currentKind()
	env, detectedDistro := DetectEnvironment()
	mode, err := resolveMode(o.Mode, kind, env)
	if err != nil {
		return nil, err
	}
	distro := strings.TrimSpace(o.Distro)
	if distro == "" {
		distro = detectedDistro
	}
	shell := strings.TrimSpace(o.Shell)
	if shell == "" {
		shell = DefaultShell()
	}
	a := &adapter{
		kind:   kind,
		env:    env,
		mode:   mode,
		roots:  normalizeRoots(o.Roots),
		distro: distro,
		shell:  shell,
		tmux:   strings.TrimSpace(o.TmuxBinary),
	}
	if kind == KindWindows && mode == RuntimeWSL {
		a.mapper = NewWSLPathMapper(o.WSLMountRoot)
	} else {
		a.mapper = NewNativePathMapper()
	}
	a.supported, a.unsupportedReason = runtimeSupport(env, mode)
	return a, nil
}

// DefaultProjectsRoots returns the built-in Projects Root for this host.
func DefaultProjectsRoots() []string {
	if currentKind() == KindWindows {
		return []string{DefaultWindowsProjectsRoot}
	}
	if info, err := os.Stat(DefaultWSLProjectsRoot); err == nil && info.IsDir() {
		return []string{DefaultWSLProjectsRoot}
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return []string{filepath.Join(home, "Projects")}
	}
	return []string{string(filepath.Separator) + "Projects"}
}

// DefaultDataDir returns the built-in AgentMux data directory for this host.
//
// AgentMux state lives here - never inside a managed project repository.
func DefaultDataDir() string {
	if dir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(dir) != "" {
		return filepath.Join(dir, "AgentMux")
	}
	return filepath.Join(".", "data")
}

// currentKind maps runtime.GOOS to a Kind.
func currentKind() Kind {
	switch runtime.GOOS {
	case "windows":
		return KindWindows
	case "darwin":
		return KindDarwin
	default:
		return KindLinux
	}
}

// resolveMode turns a configured mode into a concrete runtime mode.
func resolveMode(mode string, kind Kind, env Environment) (RuntimeMode, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "auto":
		if kind == KindWindows && wslAvailable() {
			return RuntimeWSL, nil
		}
		return RuntimeNative, nil
	case "wsl":
		if kind == KindWindows {
			return RuntimeWSL, nil
		}
		if env == EnvWSL {
			// The server is already inside the distribution, so its own paths
			// are the runtime's paths and "wsl" simply names the runtime it is
			// standing in. Accepting it means one configuration file can be
			// shared between the Windows and the WSL run instead of the WSL run
			// refusing to start.
			return RuntimeNative, nil
		}
		// Asking for a WSL runtime on a host that is neither Windows nor WSL is
		// a real misconfiguration, not a spelling difference.
		return "", fmt.Errorf(
			"host: runtime mode %q requires a Windows host or a process running inside WSL; "+
				"this process is running on %s, where host and runtime paths are already identical, "+
				"so use %q",
			mode, env, "native")
	case "native":
		return RuntimeNative, nil
	default:
		return "", fmt.Errorf("host: unknown runtime mode %q", mode)
	}
}

// runtimeSupport reports whether a persistent terminal runtime can execute in
// this environment, and why not when it cannot.
func runtimeSupport(env Environment, mode RuntimeMode) (bool, string) {
	if env.Posix() {
		return true, ""
	}
	return false, runtimeUnsupportedReason(env, mode)
}

// wslAvailable reports whether the wsl.exe launcher exists on this machine.
func wslAvailable() bool {
	_, err := exec.LookPath("wsl.exe")
	return err == nil
}

// normalizeRoots cleans, de-duplicates, and orders roots by specificity so
// that a nested root always wins over the broader root containing it.
func normalizeRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	seen := make(map[string]bool, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		r = filepath.Clean(r)
		key := pathutil.Key(r)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// adapter is the concrete Adapter used for every platform.
//
// One implementation covers all hosts because the only genuinely
// platform-shaped decisions - which PathMapper to use, and whether a runtime
// can execute here - are made once in New.
type adapter struct {
	kind   Kind
	env    Environment
	mode   RuntimeMode
	roots  []string
	mapper PathMapper
	shell  string
	tmux   string

	supported         bool
	unsupportedReason string

	distroOnce sync.Once
	distro     string

	// The runtime probe crosses a process boundary on Windows, so its result
	// is cached briefly rather than re-run on every GET /api/server.
	runtimeProbeMu sync.Mutex
	runtimeProbe   []Dependency
	runtimeProbeAt time.Time
}

// Kind implements Adapter.
func (a *adapter) Kind() Kind { return a.kind }

// Mode implements Adapter.
func (a *adapter) Mode() RuntimeMode { return a.mode }

// Environment implements Adapter.
func (a *adapter) Environment() Environment { return a.env }

// RuntimeSupport implements Adapter.
func (a *adapter) RuntimeSupport() (bool, string) { return a.supported, a.unsupportedReason }

// PathMapper implements Adapter.
func (a *adapter) PathMapper() PathMapper { return a.mapper }

// ProjectsRoots implements Adapter.
func (a *adapter) ProjectsRoots() []string {
	return append([]string(nil), a.roots...)
}

// Info implements Adapter.
func (a *adapter) Info(ctx context.Context) SystemInfo {
	info := SystemInfo{
		HostOS:                   a.kind,
		HostArch:                 runtime.GOARCH,
		RuntimeMode:              a.mode,
		RuntimeOS:                a.kind,
		PathMapper:               a.mapper.Describe(),
		Environment:              a.env,
		RuntimeAvailable:         a.supported,
		RuntimeUnavailableReason: a.unsupportedReason,
	}
	if a.mode == RuntimeWSL {
		info.RuntimeOS = KindLinux
		info.Distro = a.detectDistro(ctx)
	} else if a.env == EnvWSL {
		// Running inside the distribution: the runtime is this machine, so the
		// distribution name describes both.
		info.Distro = a.distro
	}
	return info
}

// detectDistro resolves the WSL distribution name once per process.
//
// Failure is not an error: an undetected distribution only means the UI shows
// "WSL" instead of "WSL: Ubuntu-24.04".
func (a *adapter) detectDistro(ctx context.Context) string {
	a.distroOnce.Do(func() {
		if a.distro != "" {
			return
		}
		a.distro = probeWSLDistro(ctx)
	})
	return a.distro
}

func probeWSLDistro(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, wslDistroTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "wsl.exe", "-l", "-q")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// wsl.exe writes UTF-16LE, so ASCII names arrive with NUL bytes between
	// characters. Dropping NULs is enough for distribution names.
	text := strings.ReplaceAll(string(out), "\x00", "")
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// ToRuntimePath implements Adapter.
func (a *adapter) ToRuntimePath(hostPath string) (string, error) {
	return a.mapper.ToRuntimePath(hostPath)
}

// ToHostPath implements Adapter.
func (a *adapter) ToHostPath(runtimePath string) (string, error) {
	return a.mapper.ToHostPath(runtimePath)
}

// OwningRoot implements Adapter. The most specific matching root wins.
func (a *adapter) OwningRoot(hostPath string) string {
	if strings.TrimSpace(hostPath) == "" {
		return ""
	}
	candidate := filepath.Clean(hostPath)
	best := ""
	for _, root := range a.roots {
		if !pathutil.Contains(root, candidate) {
			continue
		}
		if len(root) > len(best) {
			best = root
		}
	}
	return best
}

// ContainsPath implements Adapter.
func (a *adapter) ContainsPath(hostPath string) bool {
	return a.OwningRoot(hostPath) != ""
}

// CollectionPathFor implements Adapter.
//
// A project at "<root>/Collection/Project" belongs to "Collection". A project
// at "<root>/Project" belongs to no collection, which is a supported layout:
// AgentMux must not require a collection folder.
func (a *adapter) CollectionPathFor(hostPath string) (string, error) {
	if strings.TrimSpace(hostPath) == "" {
		return "", ErrEmptyPath
	}
	abs, err := filepath.Abs(strings.TrimSpace(hostPath))
	if err != nil {
		return "", fmt.Errorf("host: resolve %q: %w", hostPath, err)
	}
	abs = filepath.Clean(abs)

	root := a.OwningRoot(abs)
	if root == "" {
		return "", fmt.Errorf("%w: %s", ErrOutsideProjectsRoot, abs)
	}
	rel, ok := pathutil.Relative(root, abs)
	if !ok || rel == "" {
		// The path is the root itself, or unrelatable.
		return "", nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 {
		// Direct child of the root: no collection layer.
		return "", nil
	}
	return filepath.Join(root, parts[0]), nil
}

// serverProbes are the programs the AgentMux server itself runs. They are
// looked for wherever the server runs, because that is where they execute.
var serverProbes = []Dependency{
	{Name: "git", Note: "Used when a new project is created with Initialize Git. Runs where the server runs."},
}

// runtimeProbes are the programs that must exist inside the terminal runtime.
// The configured shell and the configured tmux binary are appended to this list
// by runtimeDependencies, because both are configurable.
var runtimeProbes = []Dependency{
	{
		Name: "claude",
		Note: "Claude Code CLI. Required from Phase 3; this build never starts it.",
	},
}

// runtimeDefaultTmux is the binary the runtime runs when none is configured.
//
// It is written down here rather than imported from the session package,
// because host is the platform layer and session is not: the two agreeing on
// the string "tmux" is a fact about tmux, not a dependency worth creating.
const runtimeDefaultTmux = "tmux"

// tmuxBinary is the tmux executable the runtime will run.
func (a *adapter) tmuxBinary() string {
	if v := strings.TrimSpace(a.tmux); v != "" {
		return v
	}
	return runtimeDefaultTmux
}

// runtimeProbeTimeout bounds the cross-boundary dependency probe.
const runtimeProbeTimeout = 8 * time.Second

// runtimeProbeCacheTTL is how long a cross-boundary probe result is reused.
// It is short because a user who installs a missing program should see the
// change without restarting the server.
const runtimeProbeCacheTTL = 30 * time.Second

// CheckDependencies implements Adapter.
//
// The server's own tools and the runtime's tools are probed in the environment
// where each actually executes. On a Windows host those are different
// machines: reporting tmux from the Windows PATH would answer a question
// nobody asked, because tmux is never going to run there.
func (a *adapter) CheckDependencies(ctx context.Context) []Dependency {
	out := make([]Dependency, 0, len(serverProbes)+len(runtimeProbes)+2)

	for _, probe := range serverProbes {
		out = append(out, probeLocal(probe, string(a.env)))
	}

	runtime := a.runtimeDependencies()
	if a.env.Posix() {
		// The runtime is this process's own environment, so the probe is exact
		// and needs no subprocess.
		for _, probe := range runtime {
			out = append(out, probeLocal(probe, string(a.env)))
		}
		return out
	}
	return append(out, a.probeRuntimeAcrossBoundary(ctx, runtime)...)
}

// runtimeDependencies is the runtime probe list, including the configured
// shell and tmux binary, which are worth reporting because a session cannot
// start without either.
//
// The tmux entry is built here rather than listed in runtimeProbes because the
// binary is configurable. Its Name stays "tmux" whatever the path is: the
// report answers "is the terminal runtime available", and a consumer looking
// the entry up by name - the server info endpoint does - must keep finding it
// when a path is configured.
func (a *adapter) runtimeDependencies() []Dependency {
	out := make([]Dependency, 0, len(runtimeProbes)+2)
	out = append(out, Dependency{
		Name:     "tmux",
		Probe:    a.tmuxBinary(),
		Required: true,
		Note:     "The persistent terminal runtime. Each project runs on its own tmux server.",
	})
	out = append(out, runtimeProbes...)
	if a.shell != "" {
		out = append(out, Dependency{
			Name: a.shell,
			Note: "The shell a terminal session runs.",
		})
	}
	return out
}

// probeLocal looks a program up on this process's own PATH.
func probeLocal(probe Dependency, where string) Dependency {
	dep := probe
	dep.ProbedIn = where
	if found, err := exec.LookPath(probe.lookUp()); err == nil {
		dep.Available = true
		dep.Path = found
	}
	return dep
}

// probeRuntimeAcrossBoundary probes the runtime from a host that is not the
// runtime, which on AgentMux means a Windows server asking a WSL
// distribution.
//
// It is a diagnostic, not a data path: one command is spawned for the whole
// list, on demand, and its result is cached. Nothing about a terminal session
// travels this way.
func (a *adapter) probeRuntimeAcrossBoundary(ctx context.Context, runtime []Dependency) []Dependency {
	a.runtimeProbeMu.Lock()
	defer a.runtimeProbeMu.Unlock()

	if !a.runtimeProbeAt.IsZero() && time.Since(a.runtimeProbeAt) < runtimeProbeCacheTTL {
		return cloneDependencies(a.runtimeProbe)
	}

	ctx, cancel := context.WithTimeout(ctx, runtimeProbeTimeout)
	defer cancel()

	names := make([]string, 0, len(runtime))
	for _, dep := range runtime {
		names = append(names, dep.lookUp())
	}
	found, err := probeWSLRuntime(ctx, a.detectDistro(ctx), names)

	where := "wsl"
	if distro := a.detectDistro(ctx); distro != "" {
		where = "wsl:" + distro
	}

	out := make([]Dependency, 0, len(runtime))
	for _, dep := range runtime {
		dep.ProbedIn = where
		if err == nil {
			if path, ok := found[dep.lookUp()]; ok && path != "" {
				dep.Available = true
				dep.Path = path
			}
		} else {
			// The probe could not run at all. Saying "not installed" would be a
			// guess, so the failure is reported as the note instead.
			dep.Note = fmt.Sprintf("%s (could not be probed inside WSL: %v)", dep.Note, err)
		}
		out = append(out, dep)
	}

	a.runtimeProbe = cloneDependencies(out)
	a.runtimeProbeAt = time.Now()
	return out
}

// probeWSLRuntime runs one command inside the WSL distribution and reports
// which of names resolved there.
//
// The lookup uses the POSIX "command -v" builtin, so it asks the runtime's own
// shell where a program is rather than reimplementing PATH resolution from the
// outside.
func probeWSLRuntime(ctx context.Context, distro string, names []string) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "wsl.exe", runtimeLookupArgs(distro, names)...).Output()
	if err != nil {
		return nil, err
	}
	return parseRuntimeLookup(string(out)), nil
}

// runtimeLookupArgs builds the wsl.exe command line for the runtime probe.
//
// It is separate from running it because the one argument that matters is easy
// to get wrong and impossible to notice without a WSL distribution to hand.
// That argument is --exec, and it is not decoration. Given `wsl.exe -d D -- sh
// -lc SCRIPT`, wsl.exe does not run sh itself: it hands the rest of the line to
// the distribution's login shell, which expands every $name in SCRIPT before sh
// ever sees it. `$n` and `$p` become empty strings, the loop still runs, and
// every program is reported as missing - a server on Windows said tmux was not
// installed in a distribution where it plainly was. --exec passes argv through
// without a shell in front of it, which is what a lookup and its quoting need.
func runtimeLookupArgs(distro string, names []string) []string {
	args := []string{}
	if distro != "" {
		args = append(args, "-d", distro)
	}
	return append(args, "--exec", "sh", "-lc", runtimeLookupScript(names))
}

// runtimeLookupScript builds the shell program that resolves each name.
func runtimeLookupScript(names []string) string {
	var b strings.Builder
	b.WriteString("for n in")
	for _, name := range names {
		b.WriteString(" '")
		b.WriteString(strings.ReplaceAll(name, "'", `'\''`))
		b.WriteString("'")
	}
	// A name that does not resolve prints an empty value rather than nothing,
	// so the reply has one line per name and a missing program stays
	// distinguishable from a truncated reply.
	b.WriteString("; do p=$(command -v \"$n\" 2>/dev/null || true); printf '%s=%s\\n' \"$n\" \"$p\"; done")
	return b.String()
}

// parseRuntimeLookup reads the "name=path" lines the lookup script prints.
func parseRuntimeLookup(out string) map[string]string {
	found := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		name, path, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok || name == "" {
			continue
		}
		found[name] = strings.TrimSpace(path)
	}
	return found
}

// cloneDependencies copies a cached probe result so a caller cannot mutate it.
func cloneDependencies(in []Dependency) []Dependency {
	return append([]Dependency(nil), in...)
}

// OpenInVSCode implements Adapter.
//
// Deliberately unimplemented: launching an editor is a Phase 9 capability.
// Declaring it now fixes the interface without pretending the feature exists.
func (a *adapter) OpenInVSCode(_ context.Context, _ string) error {
	return fmt.Errorf("host: OpenInVSCode %w (planned for Phase 9)", ErrNotImplemented)
}
