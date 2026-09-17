package host

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// fixture returns the broad root, a root nested inside it, a project inside the
// nested root, and a project sitting directly under the broad root, spelled the
// way this host spells paths.
//
// The shape mirrors the real layout AgentMux was built for
// (Projects / Collection / Project), so the collection rules are exercised
// against the case they exist for.
func fixture() (broad, nested, inNested, directChild, outside string) {
	if runtime.GOOS == "windows" {
		return `D:\AI\Projects`,
			`D:\AI\Projects\2026 AgentMux`,
			`D:\AI\Projects\2026 AgentMux\AgentMux`,
			`D:\AI\Projects\Loose`,
			`C:\somewhere\else`
	}
	return "/mnt/d/AI/Projects",
		"/mnt/d/AI/Projects/2026 AgentMux",
		"/mnt/d/AI/Projects/2026 AgentMux/AgentMux",
		"/mnt/d/AI/Projects/Loose",
		"/somewhere/else"
}

// newTestAdapter builds an adapter directly, skipping New's platform
// detection so these tests describe the rules rather than the machine.
func newTestAdapter(roots []string, mapper PathMapper) *adapter {
	if mapper == nil {
		mapper = NewNativePathMapper()
	}
	env, distro := DetectEnvironment()
	supported, reason := runtimeSupport(env, RuntimeNative)
	return &adapter{
		kind:              currentKind(),
		env:               env,
		mode:              RuntimeNative,
		roots:             normalizeRoots(roots),
		mapper:            mapper,
		shell:             DefaultShell(),
		distro:            distro,
		supported:         supported,
		unsupportedReason: reason,
	}
}

func TestNormalizeRoots(t *testing.T) {
	broad, _, _, _, _ := fixture()

	got := normalizeRoots([]string{"", "  ", broad, broad + "/", "  " + broad + "  "})
	if len(got) != 1 {
		t.Fatalf("normalizeRoots returned %d roots (%q), want 1", len(got), got)
	}
	if got[0] != broad {
		t.Errorf("normalizeRoots kept %q, want %q", got[0], broad)
	}
}

func TestOwningRootPrefersTheMostSpecificRoot(t *testing.T) {
	broad, nested, inNested, directChild, outside := fixture()
	a := newTestAdapter([]string{broad, nested}, nil)

	if got := a.OwningRoot(inNested); got != nested {
		t.Errorf("OwningRoot(%q) = %q, want the nested root %q", inNested, got, nested)
	}
	if got := a.OwningRoot(directChild); got != broad {
		t.Errorf("OwningRoot(%q) = %q, want the broad root %q", directChild, got, broad)
	}
	if got := a.OwningRoot(outside); got != "" {
		t.Errorf("OwningRoot(%q) = %q, want an empty result for a path outside every root", outside, got)
	}
	if got := a.OwningRoot(""); got != "" {
		t.Errorf("OwningRoot(\"\") = %q, want an empty result", got)
	}
	if got := a.OwningRoot(nested); got != nested {
		t.Errorf("OwningRoot(%q) = %q, want the root itself", nested, got)
	}
}

// TestOwningRootIsNotFooledByASharedPrefix guards the containment check that
// every registration depends on.
func TestOwningRootIsNotFooledByASharedPrefix(t *testing.T) {
	broad, _, _, _, _ := fixture()
	a := newTestAdapter([]string{broad}, nil)

	if got := a.OwningRoot(broad + "-archive"); got != "" {
		t.Errorf("OwningRoot(%q) = %q, want an empty result: a sibling sharing a prefix is not inside the root",
			broad+"-archive", got)
	}
	if !a.ContainsPath(broad + string(sep()) + "archive") {
		t.Errorf("a real child of %q must be contained", broad)
	}
}

func TestCollectionPathFor(t *testing.T) {
	broad, nested, inNested, directChild, outside := fixture()

	t.Run("with one root the collection is the folder under it", func(t *testing.T) {
		a := newTestAdapter([]string{broad}, nil)
		got, err := a.CollectionPathFor(inNested)
		if err != nil {
			t.Fatalf("CollectionPathFor(%q): %v", inNested, err)
		}
		if got != nested {
			t.Errorf("CollectionPathFor(%q) = %q, want %q", inNested, got, nested)
		}
	})

	t.Run("a project directly under the root has no collection", func(t *testing.T) {
		a := newTestAdapter([]string{broad}, nil)
		got, err := a.CollectionPathFor(directChild)
		if err != nil {
			t.Fatalf("CollectionPathFor(%q): %v", directChild, err)
		}
		if got != "" {
			t.Errorf("CollectionPathFor(%q) = %q, want an empty collection: a collection is optional", directChild, got)
		}
	})

	t.Run("the root itself has no collection", func(t *testing.T) {
		a := newTestAdapter([]string{broad}, nil)
		got, err := a.CollectionPathFor(broad)
		if err != nil {
			t.Fatalf("CollectionPathFor(%q): %v", broad, err)
		}
		if got != "" {
			t.Errorf("CollectionPathFor(%q) = %q, want an empty collection", broad, got)
		}
	})

	t.Run("declaring the collection as its own root removes the collection layer", func(t *testing.T) {
		a := newTestAdapter([]string{broad, nested}, nil)
		got, err := a.CollectionPathFor(inNested)
		if err != nil {
			t.Fatalf("CollectionPathFor(%q): %v", inNested, err)
		}
		if got != "" {
			t.Errorf("CollectionPathFor(%q) = %q, want an empty collection: the project is a direct child of the nested root %q",
				inNested, got, nested)
		}
	})

	t.Run("a path outside every root is refused", func(t *testing.T) {
		a := newTestAdapter([]string{broad}, nil)
		if got, err := a.CollectionPathFor(outside); !errors.Is(err, ErrOutsideProjectsRoot) {
			t.Errorf("CollectionPathFor(%q) = (%q, %v), want ErrOutsideProjectsRoot", outside, got, err)
		}
	})

	t.Run("an empty path is refused", func(t *testing.T) {
		a := newTestAdapter([]string{broad}, nil)
		if got, err := a.CollectionPathFor("  "); !errors.Is(err, ErrEmptyPath) {
			t.Errorf("CollectionPathFor(\"  \") = (%q, %v), want ErrEmptyPath", got, err)
		}
	})
}

func TestResolveMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		kind    Kind
		env     Environment
		want    RuntimeMode
		wantErr bool
	}{
		{name: "native on Windows", mode: "native", kind: KindWindows, env: EnvWindows, want: RuntimeNative},
		{name: "native on Linux", mode: "native", kind: KindLinux, env: EnvLinux, want: RuntimeNative},
		{name: "native on macOS", mode: "native", kind: KindDarwin, env: EnvDarwin, want: RuntimeNative},
		{name: "wsl on Windows", mode: "wsl", kind: KindWindows, env: EnvWindows, want: RuntimeWSL},
		{name: "mode is case-insensitive", mode: "WSL", kind: KindWindows, env: EnvWindows, want: RuntimeWSL},
		{name: "mode is trimmed", mode: "  native  ", kind: KindWindows, env: EnvWindows, want: RuntimeNative},
		{name: "auto on Linux is native", mode: "auto", kind: KindLinux, env: EnvLinux, want: RuntimeNative},
		{name: "auto on macOS is native", mode: "auto", kind: KindDarwin, env: EnvDarwin, want: RuntimeNative},
		{
			// The server is already inside the distribution, so its own paths
			// are the runtime's paths. Refusing this would mean one config file
			// could not be shared between the Windows and the WSL run.
			name: "wsl inside WSL resolves to native", mode: "wsl", kind: KindLinux, env: EnvWSL, want: RuntimeNative,
		},
		{
			name: "wsl on plain Linux is an error", mode: "wsl", kind: KindLinux, env: EnvLinux, wantErr: true,
		},
		{name: "wsl on macOS is an error", mode: "wsl", kind: KindDarwin, env: EnvDarwin, wantErr: true},
		{name: "an unknown mode is an error", mode: "container", kind: KindWindows, env: EnvWindows, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveMode(tt.mode, tt.kind, tt.env)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveMode(%q, %s, %s) = %q, want an error", tt.mode, tt.kind, tt.env, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveMode(%q, %s, %s) returned an error: %v", tt.mode, tt.kind, tt.env, err)
			}
			if got != tt.want {
				t.Errorf("resolveMode(%q, %s, %s) = %q, want %q", tt.mode, tt.kind, tt.env, got, tt.want)
			}
		})
	}
}

// TestResolveModeAutoFollowsWSLAvailability keeps "auto" honest: it must prefer
// WSL exactly when the launcher is present, and never claim WSL on a host that
// cannot run it.
func TestResolveModeAutoFollowsWSLAvailability(t *testing.T) {
	for _, mode := range []string{"", "auto"} {
		got, err := resolveMode(mode, KindWindows, EnvWindows)
		if err != nil {
			t.Fatalf("resolveMode(%q, windows) returned an error: %v", mode, err)
		}
		want := RuntimeNative
		if wslAvailable() {
			want = RuntimeWSL
		}
		if got != want {
			t.Errorf("resolveMode(%q, windows) = %q, want %q (wsl.exe available: %v)",
				mode, got, want, wslAvailable())
		}
	}
}

// TestRuntimeSupportRefusesAWindowsHost records the Phase 2 architectural
// decision as a property of the code: a Windows process cannot host the tmux
// runtime, and the adapter says so with a reason rather than leaving the UI to
// discover it from a missing binary.
func TestRuntimeSupportRefusesAWindowsHost(t *testing.T) {
	ok, reason := runtimeSupport(EnvWindows, RuntimeWSL)
	if ok {
		t.Fatal("a Windows host must not report a usable terminal runtime")
	}
	if !strings.Contains(reason, "inside WSL") {
		t.Errorf("the refusal must say what to do about it, got %q", reason)
	}

	ok, reason = runtimeSupport(EnvWindows, RuntimeNative)
	if ok {
		t.Fatal("a Windows host must not report a usable terminal runtime in native mode either")
	}
	if reason == "" {
		t.Error("a refusal must carry a reason")
	}

	for _, env := range []Environment{EnvLinux, EnvWSL} {
		if ok, reason := runtimeSupport(env, RuntimeNative); !ok || reason != "" {
			t.Errorf("runtimeSupport(%s) = (%v, %q), want (true, \"\")", env, ok, reason)
		}
	}
}

func TestNewSelectsThePathMapperForTheMode(t *testing.T) {
	a, err := New(Options{Mode: "native"})
	if err != nil {
		t.Fatalf("New(native) returned an error: %v", err)
	}
	if a.Kind() != currentKind() {
		t.Errorf("Kind() = %q, want %q", a.Kind(), currentKind())
	}
	if a.Mode() != RuntimeNative {
		t.Errorf("Mode() = %q, want %q", a.Mode(), RuntimeNative)
	}
	if _, ok := a.PathMapper().(NativePathMapper); !ok {
		t.Errorf("PathMapper() is %T, want a NativePathMapper in native mode", a.PathMapper())
	}

	if runtime.GOOS != "windows" {
		if _, err := New(Options{Mode: "wsl"}); err == nil {
			t.Error("New(wsl) must fail on a host that is not Windows: inside WSL the paths are already identical")
		}
		return
	}

	wslAdapter, err := New(Options{Mode: "wsl", WSLMountRoot: "/mnt"})
	if err != nil {
		t.Fatalf("New(wsl) returned an error: %v", err)
	}
	if wslAdapter.Mode() != RuntimeWSL {
		t.Errorf("Mode() = %q, want %q", wslAdapter.Mode(), RuntimeWSL)
	}
	if _, ok := wslAdapter.PathMapper().(*WSLPathMapper); !ok {
		t.Errorf("PathMapper() is %T, want a WSLPathMapper in WSL mode", wslAdapter.PathMapper())
	}
}

func TestNewRejectsAnUnknownMode(t *testing.T) {
	if _, err := New(Options{Mode: "container"}); err == nil {
		t.Error("New must reject an unknown runtime mode rather than falling back to a default")
	}
}

// TestInfoReportsTheRuntimeNotTheHost checks that WSL mode reports a Linux
// runtime on a Windows host, which is what the UI shows in the global bar.
func TestInfoReportsTheRuntimeNotTheHost(t *testing.T) {
	a, err := New(Options{Mode: "native"})
	if err != nil {
		t.Fatalf("New(native) returned an error: %v", err)
	}
	info := a.Info(context.Background())
	if info.HostOS != currentKind() {
		t.Errorf("HostOS = %q, want %q", info.HostOS, currentKind())
	}
	if info.RuntimeOS != currentKind() {
		t.Errorf("RuntimeOS = %q, want %q in native mode", info.RuntimeOS, currentKind())
	}
	if info.HostArch == "" {
		t.Error("HostArch must be reported")
	}
	if info.PathMapper == "" {
		t.Error("PathMapper must describe itself")
	}
}

func TestCheckDependenciesReportsEveryProbe(t *testing.T) {
	a := newTestAdapter(nil, nil)
	deps := a.CheckDependencies(context.Background())

	want := append([]Dependency(nil), serverProbes...)
	want = append(want, a.runtimeDependencies()...)
	if len(deps) != len(want) {
		t.Fatalf("CheckDependencies returned %d entries, want %d", len(deps), len(want))
	}
	for i, dep := range deps {
		if dep.Name != want[i].Name {
			t.Errorf("dependency %d is %q, want %q", i, dep.Name, want[i].Name)
		}
		if dep.Available && dep.Path == "" {
			t.Errorf("dependency %q is reported available with no path", dep.Name)
		}
		if dep.ProbedIn == "" {
			t.Errorf("dependency %q does not say which environment answered", dep.Name)
		}
		if dep.Note == "" {
			t.Errorf("dependency %q has no note explaining what it is for", dep.Name)
		}
	}

	// tmux stops being optional in Phase 2: without it there is no runtime, and
	// a UI that treats its absence as a footnote would be lying.
	var tmux *Dependency
	for i := range deps {
		if deps[i].Name == "tmux" {
			tmux = &deps[i]
		}
	}
	if tmux == nil {
		t.Fatal("tmux is not probed at all")
	}
	if !tmux.Required {
		t.Error("tmux must be marked required from Phase 2")
	}
}

// TestTheTmuxProbeAsksAboutTheConfiguredBinary is the dependency report's half
// of the tmuxBinary setting.
//
// The report has to say "tmux", because that is what the user is being told
// about, while the lookup has to ask about the binary the runtime will actually
// run. On a machine with two tmux installations those are different questions,
// and answering the first with the second's answer is how a runtime ends up
// reported ready and then failing.
func TestTheTmuxProbeAsksAboutTheConfiguredBinary(t *testing.T) {
	a := newTestAdapter(nil, nil)
	a.tmux = "/opt/custom/bin/tmux-3.7c"

	var tmux *Dependency
	for _, dep := range a.runtimeDependencies() {
		if dep.Name == "tmux" {
			d := dep
			tmux = &d
		}
	}
	if tmux == nil {
		t.Fatal("tmux is not in the runtime probe list")
	}
	if tmux.lookUp() != a.tmux {
		t.Errorf("the tmux probe looks up %q, want the configured binary %q", tmux.lookUp(), a.tmux)
	}
	if tmux.Name != "tmux" {
		t.Errorf("the probe is reported as %q; the report names the program, not the path", tmux.Name)
	}

	// And the default is still a bare name, resolved on the runtime's PATH.
	b := newTestAdapter(nil, nil)
	for _, dep := range b.runtimeDependencies() {
		if dep.Name == "tmux" && dep.lookUp() != "tmux" {
			t.Errorf("the default tmux probe looks up %q, want %q", dep.lookUp(), "tmux")
		}
	}
}

// TestRuntimeDependenciesIncludeTheConfiguredShell keeps the shell in the
// report: a session cannot start without one, so its absence is a runtime
// blocker rather than a detail.
func TestRuntimeDependenciesIncludeTheConfiguredShell(t *testing.T) {
	a := newTestAdapter(nil, nil)
	a.shell = "/bin/definitely-not-a-real-shell"

	var found bool
	for _, dep := range a.runtimeDependencies() {
		if dep.Name == a.shell {
			found = true
		}
	}
	if !found {
		t.Errorf("the configured shell %q is missing from the runtime probe list", a.shell)
	}
}

// TestServerToolsAreProbedWhereTheServerRuns separates the two questions the
// dependency report has to answer: what the server needs here, and what the
// runtime needs there.
func TestServerToolsAreProbedWhereTheServerRuns(t *testing.T) {
	a := newTestAdapter(nil, nil)
	for _, dep := range a.CheckDependencies(context.Background()) {
		if dep.Name != "git" {
			continue
		}
		if dep.ProbedIn != string(a.env) {
			t.Errorf("git is probed in %q, want the server's own environment %q", dep.ProbedIn, a.env)
		}
	}
}

func TestOpenInVSCodeIsDeclaredButNotImplemented(t *testing.T) {
	a := newTestAdapter(nil, nil)
	err := a.OpenInVSCode(context.Background(), `D:\AI\Projects\App`)
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("OpenInVSCode error = %v, want it to wrap ErrNotImplemented", err)
	}
}

func TestProjectsRootsReturnsACopy(t *testing.T) {
	broad, _, _, _, _ := fixture()
	a := newTestAdapter([]string{broad}, nil)

	got := a.ProjectsRoots()
	got[0] = "mutated"
	if a.ProjectsRoots()[0] != broad {
		t.Error("ProjectsRoots handed out its internal slice; a caller could change the configured roots")
	}
}

func TestDefaultProjectsRootsMatchesTheHost(t *testing.T) {
	roots := DefaultProjectsRoots()
	if len(roots) != 1 {
		t.Fatalf("DefaultProjectsRoots returned %d roots (%q), want 1", len(roots), roots)
	}
	if runtime.GOOS == "windows" && roots[0] != DefaultWindowsProjectsRoot {
		t.Errorf("DefaultProjectsRoots() = %q on Windows, want %q", roots[0], DefaultWindowsProjectsRoot)
	}
	if strings.TrimSpace(roots[0]) == "" {
		t.Error("DefaultProjectsRoots must not be empty")
	}
}

func TestDefaultDataDirIsNotEmpty(t *testing.T) {
	if strings.TrimSpace(DefaultDataDir()) == "" {
		t.Error("DefaultDataDir must not be empty")
	}
}

// TestRuntimeLookupArgsPassArgvWithoutAShell is a regression test for a bug that
// only shows up against a real distribution. Given `wsl.exe -d D -- sh -lc
// SCRIPT`, wsl.exe does not run sh itself: it hands the rest of the line to the
// distribution's login shell, which expands every $name in SCRIPT before sh
// ever sees it. The loop still runs, every name resolves to the empty string,
// and a server running on Windows reported tmux as missing inside a
// distribution where it was installed.
//
// The whole fix is one argument, so it is pinned here: --exec passes argv
// through untouched and a bare -- does not.
func TestRuntimeLookupArgsPassArgvWithoutAShell(t *testing.T) {
	args := runtimeLookupArgs("Ubuntu-24.04", []string{"tmux", "claude"})

	want := []string{"-d", "Ubuntu-24.04", "--exec", "sh", "-lc"}
	if len(args) != len(want)+1 {
		t.Fatalf("runtimeLookupArgs returned %d arguments (%q), want %d", len(args), args, len(want)+1)
	}
	for i, w := range want {
		if args[i] != w {
			t.Errorf("argument %d is %q, want %q", i, args[i], w)
		}
	}
	for _, a := range args {
		if a == "--" {
			t.Error("the probe passes a bare --, which makes wsl.exe run the script through a shell that expands it")
		}
	}

	// The script is still the last argument, and still the script: its $names
	// are the thing the fix protects.
	script := args[len(args)-1]
	for _, name := range []string{"tmux", "claude"} {
		if !strings.Contains(script, "'"+name+"'") {
			t.Errorf("the lookup script does not name %q: %q", name, script)
		}
	}
	if !strings.Contains(script, "$n") {
		t.Errorf("the lookup script no longer uses $n, so this test proves nothing: %q", script)
	}
}

// TestRuntimeLookupArgsWithoutADistro covers the one-distribution case, where
// the name is left to wsl.exe's own default.
func TestRuntimeLookupArgsWithoutADistro(t *testing.T) {
	args := runtimeLookupArgs("", []string{"tmux"})
	if len(args) == 0 || args[0] != "--exec" {
		t.Errorf("runtimeLookupArgs with no distro is %q, want it to start with --exec", args)
	}
	for _, a := range args {
		if a == "-d" {
			t.Error("the probe selects a distribution when none was named")
		}
	}
}

// sep is the host path separator, so the fixtures read the same everywhere.
func sep() string {
	if runtime.GOOS == "windows" {
		return `\`
	}
	return "/"
}
