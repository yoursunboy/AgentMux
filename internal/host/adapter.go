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
}

// New builds the Adapter for the platform AgentMux is running on.
func New(o Options) (Adapter, error) {
	kind := currentKind()
	mode, err := resolveMode(o.Mode, kind)
	if err != nil {
		return nil, err
	}
	a := &adapter{
		kind:   kind,
		mode:   mode,
		roots:  normalizeRoots(o.Roots),
		distro: strings.TrimSpace(o.Distro),
	}
	if kind == KindWindows && mode == RuntimeWSL {
		a.mapper = NewWSLPathMapper(o.WSLMountRoot)
	} else {
		a.mapper = NewNativePathMapper()
	}
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
func resolveMode(mode string, kind Kind) (RuntimeMode, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "auto":
		if kind == KindWindows && wslAvailable() {
			return RuntimeWSL, nil
		}
		return RuntimeNative, nil
	case "wsl":
		if kind != KindWindows {
			// Running inside WSL means the host filesystem is already the
			// runtime filesystem, so no translation is wanted.
			return "", fmt.Errorf(
				"host: runtime mode %q is only valid when the server runs on Windows; "+
					"inside WSL (or on Linux) use %q because host and runtime paths are already identical",
				mode, "native")
		}
		return RuntimeWSL, nil
	case "native":
		return RuntimeNative, nil
	default:
		return "", fmt.Errorf("host: unknown runtime mode %q", mode)
	}
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
// platform-shaped decision - which PathMapper to use - is made once in New.
type adapter struct {
	kind   Kind
	mode   RuntimeMode
	roots  []string
	mapper PathMapper

	distroOnce sync.Once
	distro     string
}

// Kind implements Adapter.
func (a *adapter) Kind() Kind { return a.kind }

// Mode implements Adapter.
func (a *adapter) Mode() RuntimeMode { return a.mode }

// PathMapper implements Adapter.
func (a *adapter) PathMapper() PathMapper { return a.mapper }

// ProjectsRoots implements Adapter.
func (a *adapter) ProjectsRoots() []string {
	return append([]string(nil), a.roots...)
}

// Info implements Adapter.
func (a *adapter) Info(ctx context.Context) SystemInfo {
	info := SystemInfo{
		HostOS:      a.kind,
		HostArch:    runtime.GOARCH,
		RuntimeMode: a.mode,
		RuntimeOS:   a.kind,
		PathMapper:  a.mapper.Describe(),
	}
	if a.mode == RuntimeWSL {
		info.RuntimeOS = KindLinux
		info.Distro = a.detectDistro(ctx)
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

// hostProbes are the external programs AgentMux looks for on the host.
var hostProbes = []Dependency{
	{Name: "git", Note: "Used when a new project is created with Initialize Git."},
	{Name: "tmux", Note: "Persistent terminal runtime. Required from Phase 2."},
	{Name: "claude", Note: "Claude Code CLI. Required from Phase 3."},
}

// CheckDependencies implements Adapter.
func (a *adapter) CheckDependencies(_ context.Context) []Dependency {
	out := make([]Dependency, 0, len(hostProbes)+1)
	for _, probe := range hostProbes {
		dep := probe
		if found, err := exec.LookPath(probe.Name); err == nil {
			dep.Available = true
			dep.Path = found
		}
		if a.mode == RuntimeWSL {
			// Be explicit: this probe inspected the Windows host PATH, not
			// the WSL environment where tmux and Claude Code actually run.
			dep.Note += " Probed on the Windows host, not inside WSL."
		}
		out = append(out, dep)
	}
	return out
}

// OpenInVSCode implements Adapter.
//
// Deliberately unimplemented: launching an editor is a Phase 9 capability.
// Declaring it now fixes the interface without pretending the feature exists.
func (a *adapter) OpenInVSCode(_ context.Context, _ string) error {
	return fmt.Errorf("host: OpenInVSCode %w (planned for Phase 9)", ErrNotImplemented)
}
