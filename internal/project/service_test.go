package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/pathutil"
)

// fakeRepo is an in-memory Repository.
//
// It reproduces the two storage behaviours the service depends on - the
// ErrNotFound contract and a case-aware host path lookup - without a database,
// so a service failure cannot be confused with a storage failure.
type fakeRepo struct {
	mu      sync.Mutex
	byID    map[string]*Project
	order   []string
	failOn  string
	failErr error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{byID: make(map[string]*Project)}
}

func (r *fakeRepo) fail(operation string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failOn = operation
	r.failErr = err
}

func (r *fakeRepo) check(operation string) error {
	if r.failOn == operation {
		return r.failErr
	}
	return nil
}

func (r *fakeRepo) Create(_ context.Context, p *Project) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check("create"); err != nil {
		return err
	}
	for _, existing := range r.byID {
		if pathutil.Same(existing.HostPath, p.HostPath) {
			return newError(CodeAlreadyRegistered, "%s is already registered", p.HostPath)
		}
	}
	r.byID[p.ID] = p.Clone()
	r.order = append(r.order, p.ID)
	return nil
}

func (r *fakeRepo) Update(_ context.Context, p *Project) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check("update"); err != nil {
		return err
	}
	if _, ok := r.byID[p.ID]; !ok {
		return ErrNotFound
	}
	r.byID[p.ID] = p.Clone()
	return nil
}

func (r *fakeRepo) GetByID(_ context.Context, id string) (*Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check("get"); err != nil {
		return nil, err
	}
	p, ok := r.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return p.Clone(), nil
}

func (r *fakeRepo) GetByHostPath(_ context.Context, hostPath string) (*Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check("getByHostPath"); err != nil {
		return nil, err
	}
	for _, existing := range r.byID {
		if pathutil.Same(existing.HostPath, hostPath) {
			return existing.Clone(), nil
		}
	}
	return nil, ErrNotFound
}

func (r *fakeRepo) List(_ context.Context, filter ListFilter) ([]*Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check("list"); err != nil {
		return nil, err
	}
	out := make([]*Project, 0, len(r.order))
	for _, id := range r.order {
		p := r.byID[id]
		if p == nil || (p.Archived && !filter.IncludeArchived) {
			continue
		}
		out = append(out, p.Clone())
	}
	// Name, then identifier, matching the SQLite ordering.
	sort.SliceStable(out, func(i, j int) bool {
		if !strings.EqualFold(out[i].Name, out[j].Name) {
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (r *fakeRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

// gitRecorder stands in for the real git binary, so no test runs git and no
// repository is ever created inside a temp directory.
//
// It reproduces ExecGitInit's error contract by attaching code, which is what
// the service and the HTTP layer switch on.
type gitRecorder struct {
	calls []string
	code  string
	err   error
}

func (g *gitRecorder) init(_ context.Context, dir string) error {
	g.calls = append(g.calls, dir)
	if g.err == nil {
		return nil
	}
	return wrapError(g.err, g.code, "git init failed in %s", dir)
}

// harness is a Service wired to a temp Projects Root, a fake repository, and a
// stubbed git.
type harness struct {
	service *Service
	repo    *fakeRepo
	git     *gitRecorder
	root    string
	now     time.Time
}

func newHarness(t *testing.T, roots ...string) *harness {
	t.Helper()
	if len(roots) == 0 {
		roots = []string{t.TempDir()}
	}
	adapter, err := host.New(host.Options{Mode: "native", Roots: roots})
	if err != nil {
		t.Fatalf("could not build the host adapter: %v", err)
	}
	repo := newFakeRepo()
	git := &gitRecorder{}
	fixed := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	service, err := NewService(Options{
		Repository: repo,
		Host:       adapter,
		GitInit:    git.init,
		Now:        func() time.Time { return fixed },
		Logger:     discardLogger(),
	})
	if err != nil {
		t.Fatalf("could not build the service: %v", err)
	}
	return &harness{service: service, repo: repo, git: git, root: roots[0], now: fixed}
}

// mkdir creates a directory below the temp root and returns its path.
func (h *harness) mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{h.root}, parts...)...)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("could not create %s: %v", path, err)
	}
	return path
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %q failure, got none", code)
	}
	if got := CodeOf(err); got != code {
		t.Fatalf("error code = %q (%v), want %q", got, err, code)
	}
}

// detailOf reads one structured detail off a failure, for the cases where the
// caller is expected to act on it rather than only show the message.
func detailOf(err error, key string) any {
	var projectErr *Error
	if !errors.As(err, &projectErr) {
		return nil
	}
	return projectErr.Details[key]
}

// ---------------------------------------------------------------------------
// Register
// ---------------------------------------------------------------------------

func TestRegisterRegistersAnExistingDirectory(t *testing.T) {
	h := newHarness(t)
	collection := h.mkdir(t, "2026 AgentMux")
	target := h.mkdir(t, "2026 AgentMux", "AgentMux")

	p, err := h.service.Register(context.Background(), RegisterInput{HostPath: target})
	if err != nil {
		t.Fatalf("Register returned an error: %v", err)
	}

	if p.Name != "AgentMux" {
		t.Errorf("Name = %q, want the directory name %q", p.Name, "AgentMux")
	}
	if p.HostPath != target {
		t.Errorf("HostPath = %q, want %q", p.HostPath, target)
	}
	if p.CollectionPath != collection {
		t.Errorf("CollectionPath = %q, want %q", p.CollectionPath, collection)
	}
	if p.RuntimePath == "" {
		t.Error("RuntimePath must be resolved at registration time")
	}
	if !ValidID(p.ID) {
		t.Errorf("ID = %q, which is not a valid project identifier", p.ID)
	}
	if p.Status != StatusStopped {
		t.Errorf("Status = %q, want %q in Phase 1", p.Status, StatusStopped)
	}
	if !p.CreatedAt.Equal(h.now) || !p.UpdatedAt.Equal(h.now) {
		t.Errorf("timestamps = %v / %v, want %v", p.CreatedAt, p.UpdatedAt, h.now)
	}
	if p.LastOpenedAt != nil {
		t.Errorf("LastOpenedAt = %v, want nil for a project that has never been opened", p.LastOpenedAt)
	}
	if p.Archived {
		t.Error("a newly registered project must not be archived")
	}
	if p.PinnedSlot != nil {
		t.Error("a newly registered project must not occupy a pinned slot")
	}
	if p.SessionName() != "amx-"+p.ID {
		t.Errorf("SessionName() = %q, want it derived from the stable ID", p.SessionName())
	}
	if h.repo.count() != 1 {
		t.Errorf("the repository holds %d projects, want 1", h.repo.count())
	}
}

func TestRegisterAcceptsAnExplicitName(t *testing.T) {
	h := newHarness(t)
	target := h.mkdir(t, "checkout")

	p, err := h.service.Register(context.Background(), RegisterInput{
		HostPath: target,
		Name:     "Readable Name",
	})
	if err != nil {
		t.Fatalf("Register returned an error: %v", err)
	}
	if p.Name != "Readable Name" {
		t.Errorf("Name = %q, want the supplied name", p.Name)
	}
	if p.HostPath != target {
		t.Errorf("HostPath = %q, want %q: the display name must not affect the location", p.HostPath, target)
	}
}

// TestRegisterDoesNotCreateASecondProject is the duplicate rule: the same
// directory registered twice must stay one project, and the caller must be told
// which one it already has.
func TestRegisterDoesNotCreateASecondProject(t *testing.T) {
	h := newHarness(t)
	target := h.mkdir(t, "App")

	first, err := h.service.Register(context.Background(), RegisterInput{HostPath: target})
	if err != nil {
		t.Fatalf("the first Register returned an error: %v", err)
	}

	_, err = h.service.Register(context.Background(), RegisterInput{HostPath: target})
	wantCode(t, err, CodeAlreadyRegistered)

	var projectErr *Error
	if !errors.As(err, &projectErr) {
		t.Fatalf("the error is %T, want a *project.Error carrying details", err)
	}
	existing, ok := projectErr.Details["project"].(*Project)
	if !ok {
		t.Fatalf("Details[project] is %T, want the existing *Project", projectErr.Details["project"])
	}
	if existing.ID != first.ID {
		t.Errorf("the reported existing project is %q, want %q", existing.ID, first.ID)
	}
	if h.repo.count() != 1 {
		t.Errorf("the repository holds %d projects, want 1", h.repo.count())
	}
}

// TestRegisterIgnoresTheLetterCaseOfThePath repeats the duplicate check the way
// a case-insensitive filesystem sees it.
func TestRegisterIgnoresTheLetterCaseOfThePath(t *testing.T) {
	if !pathutil.IsCaseInsensitive() {
		t.Skip("this host compares paths case-sensitively, so two spellings are two directories")
	}
	h := newHarness(t)
	target := h.mkdir(t, "App")

	if _, err := h.service.Register(context.Background(), RegisterInput{HostPath: target}); err != nil {
		t.Fatalf("the first Register returned an error: %v", err)
	}
	_, err := h.service.Register(context.Background(), RegisterInput{HostPath: strings.ToUpper(target)})
	wantCode(t, err, CodeAlreadyRegistered)
	if h.repo.count() != 1 {
		t.Errorf("the repository holds %d projects, want 1", h.repo.count())
	}
}

func TestRegisterRejectsAnUnusablePath(t *testing.T) {
	h := newHarness(t)
	outside := t.TempDir()
	// The directory exists, so the reason for the refusal really is the root
	// check rather than a missing path.
	outsideTarget := filepath.Join(outside, "elsewhere")
	if err := os.Mkdir(outsideTarget, 0o755); err != nil {
		t.Fatalf("could not create the outside fixture: %v", err)
	}
	file := filepath.Join(h.root, "a-file.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatalf("could not create the fixture file: %v", err)
	}

	tests := []struct {
		name     string
		hostPath string
		want     string
	}{
		{"empty", "", CodeInvalidInput},
		{"only whitespace", "   ", CodeInvalidInput},
		{"does not exist", filepath.Join(h.root, "absent"), CodePathNotFound},
		{"is a file", file, CodeNotADirectory},
		{"an existing directory outside every root", outsideTarget, CodeOutsideProjectsRoot},
		{"the root itself", h.root, CodePathIsProjectsRoot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.service.Register(context.Background(), RegisterInput{HostPath: tt.hostPath})
			wantCode(t, err, tt.want)
		})
	}
	if h.repo.count() != 0 {
		t.Errorf("a rejected registration stored %d projects", h.repo.count())
	}
}

func TestRegisterRejectsAnUnusableName(t *testing.T) {
	h := newHarness(t)
	target := h.mkdir(t, "App")

	for _, name := range []string{"../escape", "with/slash", "CON", "App."} {
		t.Run(name, func(t *testing.T) {
			_, err := h.service.Register(context.Background(), RegisterInput{
				HostPath: target,
				Name:     name,
			})
			wantCode(t, err, CodeInvalidName)
		})
	}
}

// TestRegisterRejectsADirectoryWhoseOwnNameIsUnusable records a consequence of
// sharing one name rule between creating and registering: a directory named
// like a reserved device name cannot be registered by its own name. An explicit
// Name works around it, and nothing about the project model depends on the
// name.
func TestRegisterRejectsADirectoryWhoseOwnNameIsUnusable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a directory named \"con\" cannot exist on Windows, so there is nothing to register")
	}
	h := newHarness(t)
	target := h.mkdir(t, "con")

	_, err := h.service.Register(context.Background(), RegisterInput{HostPath: target})
	wantCode(t, err, CodeInvalidName)

	p, err := h.service.Register(context.Background(), RegisterInput{HostPath: target, Name: "Console App"})
	if err != nil {
		t.Fatalf("Register with an explicit name returned an error: %v", err)
	}
	if p.HostPath != target {
		t.Errorf("HostPath = %q, want %q", p.HostPath, target)
	}
}

// TestRegisterDoesNotRequireACollection covers the flat layout: a project may
// sit directly under the Projects Root.
func TestRegisterDoesNotRequireACollection(t *testing.T) {
	h := newHarness(t)
	target := h.mkdir(t, "Loose")

	p, err := h.service.Register(context.Background(), RegisterInput{HostPath: target})
	if err != nil {
		t.Fatalf("Register returned an error: %v", err)
	}
	if p.CollectionPath != "" {
		t.Errorf("CollectionPath = %q, want an empty string for a direct child of the root", p.CollectionPath)
	}
}

// TestRegisterMapsTheRuntimePath checks that a native host stores the runtime
// path it actually has. The WSL translation is covered separately.
func TestRegisterMapsTheRuntimePath(t *testing.T) {
	h := newHarness(t)
	target := h.mkdir(t, "Mapped")

	p, err := h.service.Register(context.Background(), RegisterInput{HostPath: target})
	if err != nil {
		t.Fatalf("Register returned an error: %v", err)
	}
	if p.RuntimePath != p.HostPath {
		t.Errorf("in native mode RuntimePath = %q, want it identical to HostPath %q", p.RuntimePath, p.HostPath)
	}
}

// TestRegisterUnderWSLMode exercises Windows -> WSL translation through the
// service, using a temp directory so no real project is touched.
func TestRegisterUnderWSLMode(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("WSL mode only exists when the server runs on Windows")
	}
	root := t.TempDir()
	adapter, err := host.New(host.Options{Mode: "wsl", WSLMountRoot: "/mnt", Roots: []string{root}})
	if err != nil {
		t.Fatalf("could not build a WSL adapter: %v", err)
	}
	repo := newFakeRepo()
	service, err := NewService(Options{
		Repository: repo,
		Host:       adapter,
		GitInit:    (&gitRecorder{}).init,
		Logger:     discardLogger(),
	})
	if err != nil {
		t.Fatalf("could not build the service: %v", err)
	}

	target := filepath.Join(root, "App")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("could not create the fixture directory: %v", err)
	}

	p, err := service.Register(context.Background(), RegisterInput{HostPath: target})
	if err != nil {
		t.Fatalf("Register returned an error: %v", err)
	}
	if !strings.HasPrefix(p.RuntimePath, "/mnt/") {
		t.Errorf("RuntimePath = %q, want a path under /mnt", p.RuntimePath)
	}
	if strings.Contains(p.RuntimePath, `\`) {
		t.Errorf("RuntimePath = %q, which still contains Windows separators", p.RuntimePath)
	}
	drive := strings.ToLower(string(p.HostPath[0]))
	if want := "/mnt/" + drive + "/"; !strings.HasPrefix(p.RuntimePath, want) {
		t.Errorf("RuntimePath = %q, want it to begin with %q", p.RuntimePath, want)
	}
	if p.HostPath != target {
		t.Errorf("HostPath = %q, want the Windows path %q to be stored unchanged", p.HostPath, target)
	}
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

func TestCreateMakesTheDirectoryAndRegistersIt(t *testing.T) {
	h := newHarness(t)
	collection := h.mkdir(t, "2026 AgentMux")

	p, err := h.service.Create(context.Background(), CreateInput{
		Name:           "AgentMux",
		CollectionPath: collection,
	})
	if err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	want := filepath.Join(collection, "AgentMux")
	if p.HostPath != want {
		t.Errorf("HostPath = %q, want %q", p.HostPath, want)
	}
	if p.CollectionPath != collection {
		t.Errorf("CollectionPath = %q, want %q", p.CollectionPath, collection)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Errorf("the directory %q was not created: %v", want, err)
	}
	if h.repo.count() != 1 {
		t.Errorf("the repository holds %d projects, want 1", h.repo.count())
	}
	if len(h.git.calls) != 0 {
		t.Errorf("git init ran %d times, want 0 when Initialize Git is not requested", len(h.git.calls))
	}
}

func TestCreateNeedsNoCollection(t *testing.T) {
	h := newHarness(t)

	p, err := h.service.Create(context.Background(), CreateInput{Name: "Loose"})
	if err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}
	if p.HostPath != filepath.Join(h.root, "Loose") {
		t.Errorf("HostPath = %q, want the project inside the root %q", p.HostPath, h.root)
	}
	if p.CollectionPath != "" {
		t.Errorf("CollectionPath = %q, want an empty string", p.CollectionPath)
	}
}

func TestCreateChoosesTheRequestedRoot(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	h := newHarness(t, first, second)

	p, err := h.service.Create(context.Background(), CreateInput{
		Name:         "App",
		ProjectsRoot: second,
	})
	if err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}
	if p.HostPath != filepath.Join(second, "App") {
		t.Errorf("HostPath = %q, want the project inside the requested root %q", p.HostPath, second)
	}
}

// TestCreateRefusesToEscapeTheProjectsRoot is the security test for new
// project creation.
func TestCreateRefusesToEscapeTheProjectsRoot(t *testing.T) {
	h := newHarness(t)

	names := []string{
		"..",
		"../escape",
		`..\escape`,
		"../../escape",
		"a/../../escape",
		`a\..\..\escape`,
		"/absolute",
		`C:\absolute`,
		"nested/child",
		"my..project",
		"",
		"   ",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			_, err := h.service.Create(context.Background(), CreateInput{Name: name})
			if err == nil {
				t.Fatalf("Create accepted the name %q", name)
			}
			if code := CodeOf(err); code != CodeInvalidName && code != CodeInvalidInput {
				t.Errorf("Create(%q) error code = %q, want %q or %q", name, code, CodeInvalidName, CodeInvalidInput)
			}
		})
	}

	// Nothing may have appeared next to the root.
	sibling := filepath.Join(filepath.Dir(h.root), "escape")
	if _, err := os.Stat(sibling); err == nil {
		t.Errorf("a rejected Create created %q", sibling)
	}
	if h.repo.count() != 0 {
		t.Errorf("a rejected Create stored %d projects", h.repo.count())
	}
}

func TestCreateRejectsACollectionOutsideTheRoot(t *testing.T) {
	h := newHarness(t)
	outside := t.TempDir()

	_, err := h.service.Create(context.Background(), CreateInput{
		Name:           "App",
		CollectionPath: outside,
	})
	wantCode(t, err, CodeOutsideProjectsRoot)
}

func TestCreateRejectsAnUnusableCollection(t *testing.T) {
	h := newHarness(t)
	file := filepath.Join(h.root, "a-file.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatalf("could not create the fixture file: %v", err)
	}

	tests := []struct {
		name       string
		collection string
		want       string
	}{
		{"does not exist", filepath.Join(h.root, "absent"), CodePathNotFound},
		{"is a file", file, CodeNotADirectory},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.service.Create(context.Background(), CreateInput{
				Name:           "App",
				CollectionPath: tt.collection,
			})
			wantCode(t, err, tt.want)
		})
	}
}

// TestCreateNeverOverwritesExistingWork is the rule that protects a directory
// the user already has.
func TestCreateNeverOverwritesExistingWork(t *testing.T) {
	h := newHarness(t)
	existing := h.mkdir(t, "Existing")
	precious := filepath.Join(existing, "important.txt")
	if err := os.WriteFile(precious, []byte("do not lose me"), 0o644); err != nil {
		t.Fatalf("could not create the fixture file: %v", err)
	}

	_, err := h.service.Create(context.Background(), CreateInput{Name: "Existing"})
	wantCode(t, err, CodeTargetExists)

	content, readErr := os.ReadFile(precious)
	if readErr != nil {
		t.Fatalf("the existing file was removed: %v", readErr)
	}
	if string(content) != "do not lose me" {
		t.Errorf("the existing file was modified: %q", content)
	}
	if h.repo.count() != 0 {
		t.Errorf("a rejected Create stored %d projects", h.repo.count())
	}
}

func TestCreateReusesAnEmptyDirectory(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "Empty")

	p, err := h.service.Create(context.Background(), CreateInput{Name: "Empty"})
	if err != nil {
		t.Fatalf("Create refused to reuse an empty directory: %v", err)
	}
	if p.HostPath != filepath.Join(h.root, "Empty") {
		t.Errorf("HostPath = %q, want %q", p.HostPath, filepath.Join(h.root, "Empty"))
	}
}

func TestCreateRejectsAFileAtTheTarget(t *testing.T) {
	h := newHarness(t)
	if err := os.WriteFile(filepath.Join(h.root, "App"), []byte("a file"), 0o644); err != nil {
		t.Fatalf("could not create the fixture file: %v", err)
	}

	_, err := h.service.Create(context.Background(), CreateInput{Name: "App"})
	wantCode(t, err, CodeTargetExists)
}

func TestCreateRunsGitInitInTheProjectDirectory(t *testing.T) {
	h := newHarness(t)
	collection := h.mkdir(t, "Collection")

	p, err := h.service.Create(context.Background(), CreateInput{
		Name:           "App",
		CollectionPath: collection,
		InitGit:        true,
	})
	if err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	if len(h.git.calls) != 1 {
		t.Fatalf("git init ran %d times, want exactly 1", len(h.git.calls))
	}
	if h.git.calls[0] != p.HostPath {
		t.Errorf("git init ran in %q, want the project directory %q", h.git.calls[0], p.HostPath)
	}
	// Initialising one level up would put a repository in the collection folder,
	// which is the mistake the project model exists to prevent.
	if pathutil.Same(h.git.calls[0], collection) {
		t.Error("git init ran in the collection folder, not the project directory")
	}
	if h.git.calls[0] == h.root {
		t.Error("git init ran in the Projects Root")
	}
}

func TestCreateRemovesTheDirectoryWhenGitInitFails(t *testing.T) {
	h := newHarness(t)
	h.git.code = CodeGitInitFailed
	h.git.err = errors.New("exit status 128")

	_, err := h.service.Create(context.Background(), CreateInput{Name: "App", InitGit: true})
	wantCode(t, err, CodeGitInitFailed)

	// This call created the directory, so leaving it behind would be a silent
	// half-success.
	if _, statErr := os.Stat(filepath.Join(h.root, "App")); !os.IsNotExist(statErr) {
		t.Errorf("the directory created by the failed call still exists (stat error: %v)", statErr)
	}
	if h.repo.count() != 0 {
		t.Errorf("a failed Create stored %d projects", h.repo.count())
	}
	// The UI can only tell the user what happened to their disk if it is told.
	if rolledBack, _ := detailOf(err, "rolledBack").(bool); !rolledBack {
		t.Errorf("rolledBack = %v, want true: this call created and then removed the directory",
			detailOf(err, "rolledBack"))
	}
}

// TestCreateKeepsAPreExistingDirectoryWhenGitInitFails draws the line: only the
// directory this call created is cleaned up.
func TestCreateKeepsAPreExistingDirectoryWhenGitInitFails(t *testing.T) {
	h := newHarness(t)
	h.mkdir(t, "App")
	h.git.code = CodeGitInitFailed
	h.git.err = errors.New("exit status 128")

	_, err := h.service.Create(context.Background(), CreateInput{Name: "App", InitGit: true})
	if err == nil {
		t.Fatal("Create must report a failed git init")
	}

	info, statErr := os.Stat(filepath.Join(h.root, "App"))
	if statErr != nil || !info.IsDir() {
		t.Errorf("a directory the user already had must not be removed (stat error: %v)", statErr)
	}
	if rolledBack, _ := detailOf(err, "rolledBack").(bool); rolledBack {
		t.Error("rolledBack = true, but the directory was left in place")
	}
}

// TestCreateReportsAMissingGit covers the "git is not installed" path without
// requiring git to be absent from the machine running the tests.
func TestCreateReportsAMissingGit(t *testing.T) {
	h := newHarness(t)
	h.git.code = CodeGitUnavailable
	h.git.err = errors.New("executable file not found in %PATH%")

	_, err := h.service.Create(context.Background(), CreateInput{Name: "App", InitGit: true})
	wantCode(t, err, CodeGitUnavailable)
	if h.repo.count() != 0 {
		t.Errorf("a failed Create stored %d projects", h.repo.count())
	}
}

// ---------------------------------------------------------------------------
// Read paths
// ---------------------------------------------------------------------------

func TestGetAndListReportEveryProjectAsStopped(t *testing.T) {
	h := newHarness(t)
	target := h.mkdir(t, "App")

	p, err := h.service.Register(context.Background(), RegisterInput{HostPath: target})
	if err != nil {
		t.Fatalf("Register returned an error: %v", err)
	}

	// Force a runtime-backed status into the store. Phase 1 must still report
	// stopped, because no runtime exists to make "running" true.
	stored := p.Clone()
	stored.Status = StatusRunning
	if err := h.repo.Update(context.Background(), stored); err != nil {
		t.Fatalf("could not prepare the fixture: %v", err)
	}

	got, err := h.service.Get(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Get returned an error: %v", err)
	}
	if got.Status != StatusStopped {
		t.Errorf("Get reported status %q, want %q", got.Status, StatusStopped)
	}

	list, err := h.service.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(list) != 1 || list[0].Status != StatusStopped {
		t.Errorf("List = %+v, want a single project reported as %q", list, StatusStopped)
	}
}

func TestGetRejectsAnUnknownIdentifier(t *testing.T) {
	h := newHarness(t)

	for _, id := range []string{"", "nonsense", "../../etc/passwd", "p_short"} {
		t.Run(id, func(t *testing.T) {
			_, err := h.service.Get(context.Background(), id)
			wantCode(t, err, CodeNotFound)
		})
	}

	// A well-formed identifier that is not stored is also not found.
	unknown, err := NewID()
	if err != nil {
		t.Fatalf("could not generate a fixture identifier: %v", err)
	}
	_, err = h.service.Get(context.Background(), unknown)
	wantCode(t, err, CodeNotFound)
}

func TestListExcludesArchivedProjectsByDefault(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"Active", "Hidden"} {
		if _, err := h.service.Register(context.Background(), RegisterInput{
			HostPath: h.mkdir(t, name),
		}); err != nil {
			t.Fatalf("Register(%s) returned an error: %v", name, err)
		}
	}

	list, err := h.service.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List returned %d projects, want 2", len(list))
	}

	archived := list[0].Clone()
	archived.Archived = true
	if err := h.repo.Update(context.Background(), archived); err != nil {
		t.Fatalf("could not prepare the fixture: %v", err)
	}

	visible, err := h.service.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(visible) != 1 {
		t.Errorf("List returned %d projects, want 1 after archiving one", len(visible))
	}

	all, err := h.service.List(context.Background(), ListFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("List(IncludeArchived) returned %d projects, want 2", len(all))
	}
}

// TestServicePropagatesStorageFailuresUnchanged keeps the responsibility where
// it belongs: storage attaches the error code, the service does not reinterpret
// it.
func TestServicePropagatesStorageFailuresUnchanged(t *testing.T) {
	h := newHarness(t)
	target := h.mkdir(t, "App")

	storageErr := &Error{Code: CodeStorageFailure, Message: "could not store the project"}
	h.repo.fail("create", storageErr)

	_, err := h.service.Register(context.Background(), RegisterInput{HostPath: target})
	if !errors.Is(err, storageErr) {
		t.Fatalf("Register returned %v, want the storage error unchanged", err)
	}
	if code := CodeOf(err); code != CodeStorageFailure {
		t.Errorf("error code = %q, want %q", code, CodeStorageFailure)
	}
}

func TestNewServiceRequiresItsCollaborators(t *testing.T) {
	adapter, err := host.New(host.Options{Mode: "native", Roots: []string{t.TempDir()}})
	if err != nil {
		t.Fatalf("could not build the host adapter: %v", err)
	}
	if _, err := NewService(Options{Host: adapter}); err == nil {
		t.Error("NewService must reject a missing Repository")
	}
	if _, err := NewService(Options{Repository: newFakeRepo()}); err == nil {
		t.Error("NewService must reject a missing Host adapter")
	}
}
