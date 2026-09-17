package project

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/pathutil"
)

// Confidence levels for a discovered candidate.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

// Discovery scoring constants.
const (
	// discountPenalty is subtracted from a directory whose name marks it as
	// material *about* projects rather than a project.
	discountPenalty = 3

	// minCandidateScore is the score a directory must reach to be suggested at
	// all.
	//
	// A lone README.md scores 1 and deliberately falls below the bar: nearly
	// every collection, documentation, or notes folder has one, and the layout
	// AgentMux manages is exactly a collection folder holding a README next to
	// the real project. Two markers, or one strong one, clear it.
	minCandidateScore = 2

	// DefaultDiscoveryDepth reaches "Root/Collection/Project" with one level
	// of margin.
	DefaultDiscoveryDepth = 3

	// DefaultDiscoveryTimeout bounds a scan so a network drive or an enormous
	// tree cannot hang a request.
	DefaultDiscoveryTimeout = 20 * time.Second

	// DefaultMaxCandidates and DefaultMaxScannedDirs bound the work a single
	// scan can do.
	DefaultMaxCandidates  = 500
	DefaultMaxScannedDirs = 20000
)

// markerWeights scores the evidence that a directory is a project. A higher
// weight means more specific evidence. The weights are what let a directory
// with a go.mod outrank one with only a README.
var markerWeights = map[string]int{
	".git":           3,
	"CLAUDE.md":      3,
	"go.mod":         2,
	"package.json":   2,
	"pyproject.toml": 2,
	"Cargo.toml":     2,
	"pom.xml":        2,
	"build.gradle":   2,
	"composer.json":  2,
	"Gemfile":        2,
	"CMakeLists.txt": 2,
	"*.sln":          2,
	"*.csproj":       2,
	"README.md":      1,
}

// markerFiles are the file markers checked by exact name.
var markerFiles = []string{
	"CLAUDE.md",
	"go.mod",
	"package.json",
	"pyproject.toml",
	"Cargo.toml",
	"pom.xml",
	"build.gradle",
	"build.gradle.kts",
	"composer.json",
	"Gemfile",
	"CMakeLists.txt",
	"README.md",
}

// markerSuffixes are markers matched by file extension, because the file name
// itself is project-specific. They are reported and scored in the "*<suffix>"
// form, for example "*.sln".
var markerSuffixes = []string{".sln", ".csproj"}

// excludedDirNames are directories a scan never descends into. They are either
// generated, enormous, or both; walking them costs time and produces noise.
var excludedDirNames = map[string]bool{
	"node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, "out": true, "bin": true, "obj": true,
	"venv": true, "site-packages": true, "__pycache__": true,
	"coverage": true, "tmp": true, "temp": true, "logs": true,
}

// discountedNames are folder names that usually hold material about projects
// rather than a project.
//
// They are NOT banned. A user may register one deliberately, and registration
// honours that choice. Automatic discovery only ranks them lower, so that the
// real project next to them is suggested first.
var discountedNames = map[string]bool{
	"reference": true, "references": true,
	"design": true, "designs": true,
	"notes": true, "archive": true, "archives": true,
	"documents": true, "documentation": true, "docs": true,
	"screenshots": true, "assets": true, "media": true, "images": true,
	"backup": true, "backups": true, "old": true,
}

// Candidate is a directory that looks like it could be a project.
//
// A candidate is a suggestion. Nothing in this package registers one; that
// requires an explicit call to Service.Register.
type Candidate struct {
	Name           string   `json:"name"`
	HostPath       string   `json:"hostPath"`
	RuntimePath    string   `json:"runtimePath"`
	CollectionPath string   `json:"collectionPath"`
	ProjectsRoot   string   `json:"projectsRoot"`
	Depth          int      `json:"depth"`
	Markers        []string `json:"markers"`
	Score          int      `json:"score"`
	Confidence     string   `json:"confidence"`

	// NameDiscounted records that the folder name lowered this candidate's
	// rank, so the UI can explain why it appears further down.
	NameDiscounted bool `json:"nameDiscounted"`

	Registered bool   `json:"registered"`
	ProjectID  string `json:"projectId,omitempty"`
}

// RootScan reports what happened at one Projects Root.
type RootScan struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Error  string `json:"error,omitempty"`
}

// DiscoveryResult is the outcome of a scan.
type DiscoveryResult struct {
	Roots      []RootScan  `json:"roots"`
	Candidates []Candidate `json:"candidates"`
	Warnings   []string    `json:"warnings"`

	ScannedDirectories int   `json:"scannedDirectories"`
	Truncated          bool  `json:"truncated"`
	DurationMS         int64 `json:"durationMs"`
}

// DiscovererOptions configures a Discoverer.
type DiscovererOptions struct {
	Host host.Adapter

	// Depth bounds how far below a root the scan walks. Zero means
	// DefaultDiscoveryDepth.
	Depth int

	MaxCandidates  int
	MaxScannedDirs int
	Timeout        time.Duration

	Logger *slog.Logger
}

// Discoverer finds project candidates under the configured Projects Roots.
type Discoverer struct {
	host           host.Adapter
	depth          int
	maxCandidates  int
	maxScannedDirs int
	timeout        time.Duration
	log            *slog.Logger
}

// NewDiscoverer builds a Discoverer.
func NewDiscoverer(o DiscovererOptions) (*Discoverer, error) {
	if o.Host == nil {
		return nil, errors.New("project: Host adapter is required")
	}
	d := &Discoverer{
		host:           o.Host,
		depth:          o.Depth,
		maxCandidates:  o.MaxCandidates,
		maxScannedDirs: o.MaxScannedDirs,
		timeout:        o.Timeout,
		log:            o.Logger,
	}
	if d.depth <= 0 {
		d.depth = DefaultDiscoveryDepth
	}
	if d.maxCandidates <= 0 {
		d.maxCandidates = DefaultMaxCandidates
	}
	if d.maxScannedDirs <= 0 {
		d.maxScannedDirs = DefaultMaxScannedDirs
	}
	if d.timeout <= 0 {
		d.timeout = DefaultDiscoveryTimeout
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	return d, nil
}

// MaxDepth is the deepest level a scan will descend to.
func (d *Discoverer) MaxDepth() int { return d.depth }

// Discover scans the configured Projects Roots for project candidates.
//
// registered is used only to mark candidates the user already has; it never
// removes them from the result, because seeing "already registered" next to a
// directory is useful information.
func (d *Discoverer) Discover(ctx context.Context, registered []*Project) (*DiscoveryResult, error) {
	started := time.Now()

	result := &DiscoveryResult{
		Roots:      []RootScan{},
		Candidates: []Candidate{},
		Warnings:   []string{},
	}

	registeredIndex := make(map[string]*Project, len(registered))
	for _, p := range registered {
		registeredIndex[pathutil.Key(p.HostPath)] = p
	}

	roots := d.host.ProjectsRoots()
	if len(roots) == 0 {
		result.Warnings = append(result.Warnings, "no Projects Root is configured")
	}

	scanCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	for _, root := range roots {
		scan := RootScan{Path: root}
		info, err := os.Stat(root)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			scan.Error = "does not exist"
			result.Warnings = append(result.Warnings, fmt.Sprintf("Projects Root %s does not exist", root))
		case err != nil:
			scan.Error = err.Error()
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("Projects Root %s is not accessible: %v", root, err))
		case !info.IsDir():
			scan.Error = "not a directory"
			result.Warnings = append(result.Warnings, fmt.Sprintf("Projects Root %s is not a directory", root))
		default:
			scan.Exists = true
		}
		result.Roots = append(result.Roots, scan)

		if scan.Exists {
			d.walkRoot(scanCtx, root, registeredIndex, result)
		}
	}

	sortCandidates(result.Candidates)
	result.DurationMS = time.Since(started).Milliseconds()

	d.log.Info("project discovery finished",
		"roots", len(roots),
		"candidates", len(result.Candidates),
		"scannedDirectories", result.ScannedDirectories,
		"truncated", result.Truncated,
		"durationMs", result.DurationMS,
	)
	return result, nil
}

// walkRoot scans one Projects Root.
func (d *Discoverer) walkRoot(ctx context.Context, root string, registered map[string]*Project, result *DiscoveryResult) {
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			result.Truncated = true
			return fs.SkipAll
		}
		if err != nil {
			// An unreadable directory is skipped rather than fatal: a root may
			// legitimately contain a folder the user cannot read.
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.IsDir() || path == root {
			return nil
		}

		name := entry.Name()
		// Any dot-directory is skipped: they hold tool state and caches
		// (.git, .venv, .next, .gradle, ...), never a project.
		if strings.HasPrefix(name, ".") || excludedDirNames[strings.ToLower(name)] {
			return fs.SkipDir
		}

		depth := relativeDepth(root, path)
		if depth > d.depth {
			return fs.SkipDir
		}

		result.ScannedDirectories++
		if result.ScannedDirectories > d.maxScannedDirs {
			result.Truncated = true
			return fs.SkipAll
		}

		markers := detectMarkers(path)
		if len(markers) == 0 {
			// No evidence: keep descending, the project may be nested deeper.
			return nil
		}

		candidate := d.buildCandidate(root, path, depth, markers, registered)
		if candidate == nil {
			// Not enough evidence to call this a project, so the scan keeps
			// descending. Stopping here would let a collection folder holding
			// nothing but a README hide the real project inside it, which is
			// the one mistake the three-level model exists to prevent.
			return nil
		}

		result.Candidates = append(result.Candidates, *candidate)
		if len(result.Candidates) >= d.maxCandidates {
			result.Truncated = true
			return fs.SkipAll
		}

		// A detected project is a leaf: its subdirectories belong to it, and
		// are not sibling projects.
		return fs.SkipDir
	})

	if walkErr != nil {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("scanning %s stopped early: %v", root, walkErr))
	}
}

// buildCandidate decides whether a directory with markers is worth suggesting.
func (d *Discoverer) buildCandidate(
	root, path string, depth int, markers []string, registered map[string]*Project,
) *Candidate {
	// Whether a directory qualifies is decided on its own evidence. The
	// discounted-name penalty then lowers its rank, but never removes it: a
	// folder named Archive or Reference that really is a project must still be
	// suggested, just below its peers. Deciding inclusion after the penalty
	// would turn "lower priority" into a silent ban.
	evidence := scoreMarkers(markers)
	if evidence < minCandidateScore {
		return nil
	}

	discounted := discountedNames[strings.ToLower(filepath.Base(path))]
	score := evidence
	if discounted {
		score -= discountPenalty
	}

	candidate := &Candidate{
		Name:           filepath.Base(path),
		HostPath:       filepath.Clean(path),
		ProjectsRoot:   root,
		Depth:          depth,
		Markers:        markers,
		Score:          score,
		Confidence:     confidenceFor(score),
		NameDiscounted: discounted,
	}
	if runtimePath, err := d.host.ToRuntimePath(candidate.HostPath); err == nil {
		candidate.RuntimePath = runtimePath
	}
	if collection, err := d.host.CollectionPathFor(candidate.HostPath); err == nil {
		candidate.CollectionPath = collection
	}
	if existing, ok := registered[pathutil.Key(candidate.HostPath)]; ok {
		candidate.Registered = true
		candidate.ProjectID = existing.ID
	}
	return candidate
}

// detectMarkers lists the project markers present directly in dir.
//
// The directory is read once rather than probed file by file, so a scan of a
// large tree costs one syscall per directory instead of a dozen.
func detectMarkers(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var markers []string
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case name == ".git":
			// A directory in a normal repository, a file in a linked
			// worktree. Both mean the directory is version controlled.
			markers = append(markers, ".git")
		case entry.IsDir():
			continue
		case containsString(markerFiles, name):
			markers = append(markers, name)
		default:
			for _, suffix := range markerSuffixes {
				if strings.HasSuffix(name, suffix) {
					markers = append(markers, "*"+suffix)
					break
				}
			}
		}
	}
	sort.Strings(markers)
	return dedupeStrings(markers)
}

func scoreMarkers(markers []string) int {
	total := 0
	for _, marker := range markers {
		total += markerWeights[marker]
	}
	return total
}

func confidenceFor(score int) string {
	switch {
	case score >= 5:
		return ConfidenceHigh
	case score >= 3:
		return ConfidenceMedium
	default:
		return ConfidenceLow
	}
}

// sortCandidates orders candidates most-likely-first, so the UI can present
// them without re-ranking. Deterministic: equal scores fall back to depth and
// then path, so the same tree always produces the same order.
func sortCandidates(candidates []Candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		return a.HostPath < b.HostPath
	})
}

func relativeDepth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return 1 << 30
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, item := range in {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
