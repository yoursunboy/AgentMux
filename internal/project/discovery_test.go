package project

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/pathutil"
)

// discoveryHarness is a Discoverer wired to a temp Projects Root.
type discoveryHarness struct {
	discoverer *Discoverer
	root       string
}

func newDiscoveryHarness(t *testing.T, depth int, roots ...string) *discoveryHarness {
	t.Helper()
	if len(roots) == 0 {
		roots = []string{t.TempDir()}
	}
	adapter, err := host.New(host.Options{Mode: "native", Roots: roots})
	if err != nil {
		t.Fatalf("could not build the host adapter: %v", err)
	}
	discoverer, err := NewDiscoverer(DiscovererOptions{
		Host:    adapter,
		Depth:   depth,
		Timeout: 30 * time.Second,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("could not build the discoverer: %v", err)
	}
	return &discoveryHarness{discoverer: discoverer, root: roots[0]}
}

// write creates a file, and any directories above it, below the temp root.
func (h *discoveryHarness) write(t *testing.T, relPath string, content string) string {
	t.Helper()
	full := filepath.Join(h.root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("could not create %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("could not create %s: %v", full, err)
	}
	return full
}

func (h *discoveryHarness) mkdir(t *testing.T, relPath string) string {
	t.Helper()
	full := filepath.Join(h.root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatalf("could not create %s: %v", full, err)
	}
	return full
}

// find returns the candidate for a root-relative path.
func find(t *testing.T, result *DiscoveryResult, relPath string) *Candidate {
	t.Helper()
	want := filepath.Join(result.Roots[0].Path, filepath.FromSlash(relPath))
	for i := range result.Candidates {
		if pathutil.Same(result.Candidates[i].HostPath, want) {
			return &result.Candidates[i]
		}
	}
	names := make([]string, 0, len(result.Candidates))
	for _, c := range result.Candidates {
		names = append(names, c.HostPath)
	}
	sort.Strings(names)
	t.Fatalf("no candidate for %q; candidates were %q", want, names)
	return nil
}

// TestDiscoverFindsTheProjectAndNotItsCollection is the central discovery test.
// The layout is the one AgentMux was built for, and the mistake it guards
// against is treating the collection folder as a project.
func TestDiscoverFindsTheProjectAndNotItsCollection(t *testing.T) {
	h := newDiscoveryHarness(t, 3)

	// The collection holds material about the project, not a project.
	h.write(t, "2026 AgentMux/README.md", "# about this collection")
	h.write(t, "2026 AgentMux/Notes/notes.md", "scratch notes")
	// The project itself.
	h.write(t, "2026 AgentMux/AgentMux/CLAUDE.md", "# AgentMux")
	h.write(t, "2026 AgentMux/AgentMux/go.mod", "module example")

	result, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}

	project := find(t, result, "2026 AgentMux/AgentMux")
	if project.Registered {
		t.Error("nothing was registered, so the candidate must come back marked as not registered")
	}
	if project.ProjectID != "" {
		t.Errorf("ProjectID = %q, want it empty for an unregistered candidate", project.ProjectID)
	}
	if project.CollectionPath == "" {
		t.Error("the candidate must report the collection folder that contains it")
	}
	if project.Depth != 2 {
		t.Errorf("Depth = %d, want 2 for Root/Collection/Project", project.Depth)
	}
	if project.Score < 5 {
		t.Errorf("Score = %d with a CLAUDE.md and a go.mod, want a high score", project.Score)
	}
	if project.Confidence != ConfidenceHigh {
		t.Errorf("Confidence = %q, want %q", project.Confidence, ConfidenceHigh)
	}

	// The collection itself must not be offered as a project: it holds only a
	// README, which is one weak marker, and its name is not discounted.
	collection := filepath.Join(h.root, "2026 AgentMux")
	for _, c := range result.Candidates {
		if pathutil.Same(c.HostPath, collection) {
			t.Errorf("the collection folder %q was offered as a project", collection)
		}
	}
	if len(result.Candidates) != 1 {
		t.Errorf("Discover returned %d candidates, want only the project: %v",
			len(result.Candidates), candidatePaths(result))
	}
}

// TestDiscoverDoesNotTreatAFirstLevelDirectoryAsAProject repeats the rule for
// the flat layout, which is the case a naive "first level is a project"
// implementation gets wrong.
func TestDiscoverDoesNotTreatAFirstLevelDirectoryAsAProject(t *testing.T) {
	h := newDiscoveryHarness(t, 3)
	h.mkdir(t, "SomeFolder")
	h.mkdir(t, "AnotherFolder")
	h.write(t, "SomeFolder/note.txt", "nothing to see")

	result, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}
	if len(result.Candidates) != 0 {
		t.Errorf("Discover offered %v, want nothing: a plain directory is not a project", candidatePaths(result))
	}
}

// TestDiscoverLowersTheRankOfMaterialFolders covers the Reference/Notes rule:
// they are ranked lower, not banned.
func TestDiscoverLowersTheRankOfMaterialFolders(t *testing.T) {
	h := newDiscoveryHarness(t, 3)

	// Both hold a real project; only the folder names differ.
	markers := []string{"CLAUDE.md", "go.mod", "README.md"}
	for _, dir := range []string{"Reference", "Actual"} {
		for _, marker := range markers {
			h.write(t, dir+"/"+marker, "content")
		}
	}

	result, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}

	discounted := find(t, result, "Reference")
	normal := find(t, result, "Actual")

	if !discounted.NameDiscounted {
		t.Error("a Reference folder must be reported as name-discounted")
	}
	if normal.NameDiscounted {
		t.Error("an ordinary folder must not be name-discounted")
	}
	if discounted.Score >= normal.Score {
		t.Errorf("Reference scored %d and Actual scored %d; the material folder must rank lower",
			discounted.Score, normal.Score)
	}
	// Ranking lower is not the same as hiding: the user may register it.
	if len(result.Candidates) != 2 {
		t.Errorf("Discover returned %d candidates, want both: a discount must not remove a candidate",
			len(result.Candidates))
	}
	if result.Candidates[0].Name != "Actual" {
		t.Errorf("the first candidate is %q, want %q", result.Candidates[0].Name, "Actual")
	}
}

// TestDiscoverNeedsMoreThanAReadme pins the threshold. A README alone is the
// signature of a documentation or collection folder, not of a project, and the
// layout AgentMux manages is exactly a collection folder holding one.
func TestDiscoverNeedsMoreThanAReadme(t *testing.T) {
	h := newDiscoveryHarness(t, 3)
	h.write(t, "Docs/README.md", "# docs")
	h.write(t, "Collection/README.md", "# about this collection")
	h.write(t, "Collection/AgentMux/go.mod", "module app")

	result, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}

	// The project inside the collection must still be found: a folder that is
	// not a project must not stop the scan.
	if len(result.Candidates) != 1 {
		t.Fatalf("Discover offered %v, want only the nested project", candidatePaths(result))
	}
	if result.Candidates[0].Name != "AgentMux" {
		t.Errorf("the candidate is %q, want %q", result.Candidates[0].Name, "AgentMux")
	}
	if result.Candidates[0].CollectionPath == "" {
		t.Error("the nested project must report its collection folder")
	}
}

// TestDiscoverOffersALowConfidenceCandidate keeps low confidence meaningful: a
// single real marker still produces a suggestion, just a cautious one.
func TestDiscoverOffersALowConfidenceCandidate(t *testing.T) {
	h := newDiscoveryHarness(t, 3)
	h.write(t, "SomeApp/package.json", "{}")

	result, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}
	candidate := find(t, result, "SomeApp")
	if candidate.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %q, want %q for a single ordinary marker", candidate.Confidence, ConfidenceLow)
	}
}

func TestDiscoverHidesAWeakDiscountedFolder(t *testing.T) {
	h := newDiscoveryHarness(t, 3)
	h.write(t, "Reference/README.md", "just a readme")
	h.write(t, "Notes/README.md", "just a readme")

	result, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}
	if len(result.Candidates) != 0 {
		t.Errorf("Discover offered %v, want nothing below the threshold", candidatePaths(result))
	}
}

// TestDiscoverKeepsAQualifyingDiscountedFolder pins the other half of the
// Reference/Notes rule. A folder named Archive that holds a real project
// qualifies on its own evidence, so the penalty may only push it down the list.
// Subtracting the penalty before the threshold test would drop it silently,
// which is the permanent ban the rule exists to avoid.
func TestDiscoverKeepsAQualifyingDiscountedFolder(t *testing.T) {
	h := newDiscoveryHarness(t, 3)

	// Identical evidence; only the folder names differ.
	for _, dir := range []string{"Archive", "Ordinary"} {
		h.write(t, dir+"/go.mod", "module app")
		h.write(t, dir+"/package.json", "{}")
	}

	result, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}

	discounted := find(t, result, "Archive")
	normal := find(t, result, "Ordinary")

	if !discounted.NameDiscounted {
		t.Error("an Archive folder must be reported as name-discounted")
	}
	if discounted.Score >= normal.Score {
		t.Errorf("Archive scored %d and Ordinary scored %d; the discounted folder must rank lower",
			discounted.Score, normal.Score)
	}
	if discounted.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %q, want %q: the discount costs it the benefit of the doubt",
			discounted.Confidence, ConfidenceLow)
	}
	if result.Candidates[len(result.Candidates)-1].Name != "Archive" {
		t.Errorf("the last candidate is %q, want %q", result.Candidates[len(result.Candidates)-1].Name, "Archive")
	}
}

func TestDiscoverSkipsGeneratedDirectories(t *testing.T) {
	h := newDiscoveryHarness(t, 4)
	h.write(t, "App/go.mod", "module app")
	// These hold dependencies and build output, not projects.
	h.write(t, "node_modules/pkg/package.json", "{}")
	h.write(t, "App/node_modules/dep/package.json", "{}")
	h.write(t, "App/vendor/dep/go.mod", "module dep")
	h.write(t, "App/dist/package.json", "{}")
	h.write(t, "App/build/go.mod", "module build")
	h.write(t, "App/target/Cargo.toml", "[package]")
	h.write(t, "App/venv/pyproject.toml", "[project]")
	h.write(t, "App/.venv/pyproject.toml", "[project]")
	h.write(t, "App/.git/config", "[core]")

	result, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}
	if len(result.Candidates) != 1 {
		t.Errorf("Discover offered %v, want only App", candidatePaths(result))
	}
}

// TestDiscoverStopsAtAProjectBoundary records that a project is a leaf: the
// directories inside it belong to it.
func TestDiscoverStopsAtAProjectBoundary(t *testing.T) {
	h := newDiscoveryHarness(t, 5)
	h.write(t, "App/go.mod", "module app")
	h.write(t, "App/services/api/go.mod", "module api")

	result, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}
	if len(result.Candidates) != 1 {
		t.Errorf("Discover offered %v, want only the outer project", candidatePaths(result))
	}
}

func TestDiscoverHonoursTheDepthLimit(t *testing.T) {
	h := newDiscoveryHarness(t, 1)
	h.write(t, "App/go.mod", "module app")
	h.write(t, "Collection/App/go.mod", "module app")
	h.write(t, "Collection/Deeper/App/go.mod", "module app")

	result, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("Discover offered %v, want only the level-1 project", candidatePaths(result))
	}
	if result.Candidates[0].Depth != 1 {
		t.Errorf("Depth = %d, want 1", result.Candidates[0].Depth)
	}
}

// TestDiscoverMarksAlreadyRegisteredProjects checks that a known project is
// reported rather than hidden, which is what lets the UI say "already added".
func TestDiscoverMarksAlreadyRegisteredProjects(t *testing.T) {
	h := newDiscoveryHarness(t, 3)
	target := h.write(t, "2026 AgentMux/AgentMux/CLAUDE.md", "# AgentMux")

	registered := []*Project{{
		ID:       "p_0123456789abcdef0123",
		Name:     "AgentMux",
		HostPath: filepath.Dir(target),
	}}

	result, err := h.discoverer.Discover(context.Background(), registered)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}
	candidate := find(t, result, "2026 AgentMux/AgentMux")
	if !candidate.Registered {
		t.Error("a project the user already has must be reported as registered")
	}
	if candidate.ProjectID != registered[0].ID {
		t.Errorf("ProjectID = %q, want %q", candidate.ProjectID, registered[0].ID)
	}
}

func TestDiscoverReportsAMissingRootWithoutFailing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-mounted")
	h := newDiscoveryHarness(t, 3, missing)

	result, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover must tolerate a missing root, got: %v", err)
	}
	if len(result.Roots) != 1 {
		t.Fatalf("Roots = %+v, want one entry", result.Roots)
	}
	if result.Roots[0].Exists {
		t.Error("a missing root must be reported as not existing")
	}
	if result.Roots[0].Error == "" {
		t.Error("a missing root must carry an explanation")
	}
	if len(result.Warnings) == 0 {
		t.Error("a missing root must produce a warning")
	}
}

func TestDiscoverIsDeterministic(t *testing.T) {
	h := newDiscoveryHarness(t, 3)
	h.write(t, "Alpha/go.mod", "module alpha")
	h.write(t, "Beta/go.mod", "module beta")
	h.write(t, "Gamma/package.json", "{}")

	first, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}
	second, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}

	got := candidatePaths(first)
	want := candidatePaths(second)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("two scans of the same tree produced different orders: %v and %v", got, want)
	}
	// Zero scores aside, equal scores must fall back to the path, not to
	// filesystem iteration order.
	if len(got) != 3 {
		t.Fatalf("Discover offered %v, want three candidates", got)
	}
}

func TestDiscoverHonoursCancellation(t *testing.T) {
	h := newDiscoveryHarness(t, 5)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		h.write(t, name+"/go.mod", "module "+name)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := h.discoverer.Discover(ctx, nil)
	if err != nil {
		t.Fatalf("Discover must return a partial result rather than an error on cancellation: %v", err)
	}
	if result == nil {
		t.Fatal("Discover returned no result")
	}
	if !result.Truncated {
		t.Error("a cancelled scan must be reported as truncated, not as a complete scan of nothing")
	}
}

func TestNewDiscovererAppliesDefaults(t *testing.T) {
	adapter, err := host.New(host.Options{Mode: "native", Roots: []string{t.TempDir()}})
	if err != nil {
		t.Fatalf("could not build the host adapter: %v", err)
	}
	d, err := NewDiscoverer(DiscovererOptions{Host: adapter, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("NewDiscoverer returned an error: %v", err)
	}
	if d.MaxDepth() != DefaultDiscoveryDepth {
		t.Errorf("MaxDepth() = %d, want %d", d.MaxDepth(), DefaultDiscoveryDepth)
	}
	if d.maxCandidates != DefaultMaxCandidates {
		t.Errorf("maxCandidates = %d, want %d", d.maxCandidates, DefaultMaxCandidates)
	}
	if d.maxScannedDirs != DefaultMaxScannedDirs {
		t.Errorf("maxScannedDirs = %d, want %d", d.maxScannedDirs, DefaultMaxScannedDirs)
	}
	if d.timeout != DefaultDiscoveryTimeout {
		t.Errorf("timeout = %v, want %v", d.timeout, DefaultDiscoveryTimeout)
	}
	if _, err := NewDiscoverer(DiscovererOptions{}); err == nil {
		t.Error("NewDiscoverer must reject a missing Host adapter")
	}
}

// TestDiscoverRespectsTheCandidateLimit proves the scan stops instead of
// walking an enormous tree to completion.
func TestDiscoverRespectsTheCandidateLimit(t *testing.T) {
	root := t.TempDir()
	adapter, err := host.New(host.Options{Mode: "native", Roots: []string{root}})
	if err != nil {
		t.Fatalf("could not build the host adapter: %v", err)
	}
	d, err := NewDiscoverer(DiscovererOptions{
		Host:          adapter,
		Depth:         2,
		MaxCandidates: 3,
		Timeout:       30 * time.Second,
		Logger:        discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewDiscoverer returned an error: %v", err)
	}
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("could not create %s: %v", path, err)
		}
		if err := os.WriteFile(filepath.Join(path, "go.mod"), []byte("module "+name), 0o644); err != nil {
			t.Fatalf("could not create the marker: %v", err)
		}
	}

	result, err := d.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}
	if len(result.Candidates) != 3 {
		t.Errorf("Discover returned %d candidates, want the limit of 3", len(result.Candidates))
	}
	if !result.Truncated {
		t.Error("hitting the candidate limit must be reported as truncated")
	}
}

// TestDiscoverFixtureRealLayout builds the documented layout end to end and
// asserts the whole shape of the answer.
func TestDiscoverFixtureRealLayout(t *testing.T) {
	h := newDiscoveryHarness(t, 3)
	h.write(t, "2026 AgentMux/README.md", "# collection")
	h.write(t, "2026 AgentMux/AgentMux/CLAUDE.md", "# AgentMux")
	h.write(t, "2026 AgentMux/AgentMux/README.md", "# AgentMux")
	h.write(t, "2026 AgentMux/Reference/README.md", "reference material")
	h.write(t, "2026 AgentMux/Notes/README.md", "notes")
	h.write(t, "2026 AgentMux/Design/README.md", "design")
	h.write(t, "AnotherProject/go.mod", "module another")

	result, err := h.discoverer.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}

	got := candidatePaths(result)
	if len(got) != 2 {
		t.Fatalf("Discover offered %v, want the project and the loose project", got)
	}
	if result.Candidates[0].Name != "AgentMux" {
		t.Errorf("the strongest candidate is %q, want %q", result.Candidates[0].Name, "AgentMux")
	}
	if result.Truncated {
		t.Error("a scan this small must not be reported as truncated")
	}
	if result.DurationMS < 0 {
		t.Error("DurationMS must not be negative")
	}
	if result.ScannedDirectories == 0 {
		t.Error("ScannedDirectories must count the directories the scan looked at")
	}
}

// TestDiscoverUnderWSLMode checks that a candidate carries the runtime path and
// a Windows host path at the same time, which is what a session needs.
func TestDiscoverUnderWSLMode(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("WSL mode only exists when the server runs on Windows")
	}
	root := t.TempDir()
	app := filepath.Join(root, "Collection", "App")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatalf("could not create the fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(app, "go.mod"), []byte("module app"), 0o644); err != nil {
		t.Fatalf("could not create the marker: %v", err)
	}

	adapter, err := host.New(host.Options{Mode: "wsl", WSLMountRoot: "/mnt", Roots: []string{root}})
	if err != nil {
		t.Fatalf("could not build the WSL adapter: %v", err)
	}
	d, err := NewDiscoverer(DiscovererOptions{
		Host:    adapter,
		Depth:   3,
		Timeout: 30 * time.Second,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewDiscoverer returned an error: %v", err)
	}

	result, err := d.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned an error: %v", err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("Discover offered %v, want one candidate", candidatePaths(result))
	}

	candidate := result.Candidates[0]
	if !strings.HasPrefix(candidate.RuntimePath, "/mnt/") {
		t.Errorf("RuntimePath = %q, want a path under /mnt", candidate.RuntimePath)
	}
	if strings.Contains(candidate.RuntimePath, `\`) {
		t.Errorf("RuntimePath = %q still contains Windows separators", candidate.RuntimePath)
	}
	if !filepath.IsAbs(candidate.HostPath) || strings.HasPrefix(candidate.HostPath, "/mnt") {
		t.Errorf("HostPath = %q, want the Windows path", candidate.HostPath)
	}
	if candidate.CollectionPath != filepath.Join(root, "Collection") {
		t.Errorf("CollectionPath = %q, want %q", candidate.CollectionPath, filepath.Join(root, "Collection"))
	}
}

func candidatePaths(result *DiscoveryResult) []string {
	out := make([]string, 0, len(result.Candidates))
	for _, c := range result.Candidates {
		out = append(out, c.HostPath)
	}
	return out
}

// discardLogger keeps the lifecycle records out of the test output, so that a
// failure is the only thing on screen.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
