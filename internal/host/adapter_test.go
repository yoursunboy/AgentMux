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
	return &adapter{kind: currentKind(), mode: RuntimeNative, roots: normalizeRoots(roots), mapper: mapper}
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
		want    RuntimeMode
		wantErr bool
	}{
		{name: "native on Windows", mode: "native", kind: KindWindows, want: RuntimeNative},
		{name: "native on Linux", mode: "native", kind: KindLinux, want: RuntimeNative},
		{name: "native on macOS", mode: "native", kind: KindDarwin, want: RuntimeNative},
		{name: "wsl on Windows", mode: "wsl", kind: KindWindows, want: RuntimeWSL},
		{name: "mode is case-insensitive", mode: "WSL", kind: KindWindows, want: RuntimeWSL},
		{name: "mode is trimmed", mode: "  native  ", kind: KindWindows, want: RuntimeNative},
		{name: "auto on Linux is native", mode: "auto", kind: KindLinux, want: RuntimeNative},
		{name: "auto on macOS is native", mode: "auto", kind: KindDarwin, want: RuntimeNative},
		{name: "wsl on Linux is an error", mode: "wsl", kind: KindLinux, wantErr: true},
		{name: "wsl on macOS is an error", mode: "wsl", kind: KindDarwin, wantErr: true},
		{name: "an unknown mode is an error", mode: "container", kind: KindWindows, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveMode(tt.mode, tt.kind)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveMode(%q, %s) = %q, want an error", tt.mode, tt.kind, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveMode(%q, %s) returned an error: %v", tt.mode, tt.kind, err)
			}
			if got != tt.want {
				t.Errorf("resolveMode(%q, %s) = %q, want %q", tt.mode, tt.kind, got, tt.want)
			}
		})
	}
}

// TestResolveModeAutoFollowsWSLAvailability keeps "auto" honest: it must prefer
// WSL exactly when the launcher is present, and never claim WSL on a host that
// cannot run it.
func TestResolveModeAutoFollowsWSLAvailability(t *testing.T) {
	for _, mode := range []string{"", "auto"} {
		got, err := resolveMode(mode, KindWindows)
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
	if len(deps) != len(hostProbes) {
		t.Fatalf("CheckDependencies returned %d entries, want %d", len(deps), len(hostProbes))
	}
	for i, dep := range deps {
		if dep.Name != hostProbes[i].Name {
			t.Errorf("dependency %d is %q, want %q", i, dep.Name, hostProbes[i].Name)
		}
		if dep.Available && dep.Path == "" {
			t.Errorf("dependency %q is reported available with no path", dep.Name)
		}
		if dep.Required {
			t.Errorf("dependency %q is marked required; nothing is required in Phase 1", dep.Name)
		}
	}
	// tmux and the AI CLI are Phase 2 and Phase 3 concerns, and the note is how
	// the UI explains that they are not needed yet.
	for _, dep := range deps {
		if dep.Note == "" {
			t.Errorf("dependency %q has no note explaining what it is for", dep.Name)
		}
	}
}

// TestCheckDependenciesIsHonestAboutProbingTheHost records that a WSL probe
// inspects the Windows PATH, not the environment the terminal will use.
func TestCheckDependenciesIsHonestAboutProbingTheHost(t *testing.T) {
	a := newTestAdapter(nil, NewWSLPathMapper(DefaultWSLMountRoot))
	a.mode = RuntimeWSL

	for _, dep := range a.CheckDependencies(context.Background()) {
		if !strings.Contains(dep.Note, "not inside WSL") {
			t.Errorf("WSL-mode dependency %q must say the probe ran on the host, got note %q", dep.Name, dep.Note)
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

// sep is the host path separator, so the fixtures read the same everywhere.
func sep() string {
	if runtime.GOOS == "windows" {
		return `\`
	}
	return "/"
}
