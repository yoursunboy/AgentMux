package project

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/pathutil"
)

// Service implements the project model: registration, creation, and lookup.
//
// It owns the rules, not the storage. Every filesystem effect a caller can
// trigger is checked against the configured Projects Roots first, so that a
// malformed request cannot create or register anything outside the roots the
// user configured.
type Service struct {
	repo    Repository
	host    host.Adapter
	gitInit GitInitFunc
	now     func() time.Time
	newID   func() (string, error)
	log     *slog.Logger
}

// Options configures a Service. Only Repository and Host are required.
type Options struct {
	Repository Repository
	Host       host.Adapter

	// GitInit initialises Git for New Project. Nil means ExecGitInit.
	GitInit GitInitFunc

	// Now supplies the current time. Nil means time.Now.
	Now func() time.Time

	// NewID generates project identifiers. Nil means NewID.
	NewID func() (string, error)

	// Logger receives project lifecycle events. Nil means slog.Default.
	Logger *slog.Logger
}

// NewService builds a Service.
func NewService(o Options) (*Service, error) {
	if o.Repository == nil {
		return nil, errors.New("project: Repository is required")
	}
	if o.Host == nil {
		return nil, errors.New("project: Host adapter is required")
	}
	s := &Service{
		repo:    o.Repository,
		host:    o.Host,
		gitInit: o.GitInit,
		now:     o.Now,
		newID:   o.NewID,
		log:     o.Logger,
	}
	if s.gitInit == nil {
		s.gitInit = ExecGitInit
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = NewID
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// List returns registered projects.
func (s *Service) List(ctx context.Context, filter ListFilter) ([]*Project, error) {
	projects, err := s.repo.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		withDerivedStatus(p)
	}
	return projects, nil
}

// Get returns one project by identifier.
func (s *Service) Get(ctx context.Context, id string) (*Project, error) {
	if !ValidID(id) {
		return nil, newError(CodeNotFound, "no project with id %q", id)
	}
	p, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, mapNotFound(err, id)
	}
	return withDerivedStatus(p), nil
}

// RegisterInput describes an explicit registration of an existing directory.
type RegisterInput struct {
	// HostPath is the directory to register. Required.
	HostPath string

	// Name overrides the display name. Empty means the directory's own name.
	Name string
}

// Register registers an existing directory as a project.
//
// Re-registering the same directory does not create a second project. It
// returns CodeAlreadyRegistered carrying the existing project, so the caller
// can show the user what they already have instead of an opaque failure.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*Project, error) {
	hostPath, err := s.resolveRegistrable(in.HostPath)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = filepath.Base(hostPath)
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	if existing, err := s.repo.GetByHostPath(ctx, hostPath); err == nil {
		withDerivedStatus(existing)
		return nil, newError(CodeAlreadyRegistered, "%s is already registered as %q", hostPath, existing.Name).
			withDetail("hostPath", existing.HostPath).
			withDetail("projectId", existing.ID).
			// The whole project is carried as well, so a client can open what
			// already exists instead of only reporting the clash.
			withDetail("project", existing)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	p, err := s.buildProject(hostPath, name)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Create(ctx, p); err != nil {
		return nil, err
	}

	s.log.Info("project registered",
		"projectId", p.ID,
		"name", p.Name,
		"hostPath", p.HostPath,
		"runtimePath", p.RuntimePath,
		"collectionPath", p.CollectionPath,
	)
	return p, nil
}

// CreateInput describes a New Project request.
type CreateInput struct {
	// Name is the project name. It becomes the directory name.
	Name string

	// CollectionPath is the collection/group folder to create the project
	// inside. Empty means the Projects Root itself, which is a supported
	// layout: a collection is not required.
	CollectionPath string

	// ProjectsRoot picks which configured root to use when CollectionPath is
	// empty. Empty means the first configured root.
	ProjectsRoot string

	// InitGit runs "git init" inside the new project directory.
	InitGit bool
}

// Create creates a new project directory and registers it.
func (s *Service) Create(ctx context.Context, in CreateInput) (*Project, error) {
	name := strings.TrimSpace(in.Name)
	if err := ValidateName(name); err != nil {
		return nil, err
	}

	parent, err := s.resolveParent(in.CollectionPath, in.ProjectsRoot)
	if err != nil {
		return nil, err
	}

	target, err := SafeJoin(parent, name)
	if err != nil {
		return nil, err
	}

	// Defence in depth. SafeJoin already proved target is a direct child of
	// parent, and parent was proved to be inside a root; re-checking here
	// means a future change to either cannot quietly escape the roots.
	if !s.host.ContainsPath(target) {
		return nil, newError(CodeOutsideProjectsRoot,
			"%s is outside the configured Projects Roots (%s)",
			target, strings.Join(s.host.ProjectsRoots(), ", "))
	}
	if root := s.host.OwningRoot(target); pathutil.Same(root, target) {
		return nil, newError(CodePathIsProjectsRoot,
			"%s is a Projects Root itself and cannot be created as a project", target)
	}

	created, err := s.prepareTarget(target)
	if err != nil {
		return nil, err
	}

	if in.InitGit {
		if err := s.gitInit(ctx, target); err != nil {
			if created {
				// Remove only the directory this call created. Leaving an
				// empty directory behind after a failed create would be a
				// silent half-success.
				if rmErr := os.RemoveAll(target); rmErr != nil {
					s.log.Warn("could not clean up project directory after failed git init",
						"path", target, "error", rmErr)
				}
			}
			s.log.Error("project creation failed during git init", "path", target, "error", err)
			// Whether the directory survived depends on whether this call made
			// it, and only the server knows. The UI cannot say something true
			// about the user's disk without being told.
			var projectErr *Error
			if errors.As(err, &projectErr) {
				err = projectErr.withDetail("rolledBack", created)
			}
			return nil, err
		}
	}

	p, err := s.buildProject(target, name)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Create(ctx, p); err != nil {
		return nil, err
	}

	s.log.Info("project created",
		"projectId", p.ID,
		"name", p.Name,
		"hostPath", p.HostPath,
		"runtimePath", p.RuntimePath,
		"collectionPath", p.CollectionPath,
		"gitInitialized", in.InitGit,
	)
	return p, nil
}

// prepareTarget makes sure target is a usable, empty project directory. It
// reports whether this call created the directory.
func (s *Service) prepareTarget(target string) (created bool, err error) {
	info, statErr := os.Stat(target)
	switch {
	case statErr == nil && !info.IsDir():
		return false, newError(CodeTargetExists, "%s already exists and is not a directory", target)
	case statErr == nil:
		entries, readErr := os.ReadDir(target)
		if readErr != nil {
			return false, wrapError(readErr, CodePathNotAccessible, "cannot read %s", target)
		}
		if len(entries) > 0 {
			// Never write into a directory that already holds someone's work.
			return false, newError(CodeTargetExists,
				"%s already exists and is not empty; choose another name or register the existing directory", target)
		}
		return false, nil
	case errors.Is(statErr, fs.ErrNotExist):
		if mkErr := os.MkdirAll(target, 0o755); mkErr != nil {
			return false, wrapError(mkErr, CodePathNotAccessible, "cannot create %s", target)
		}
		return true, nil
	default:
		return false, wrapError(statErr, CodePathNotAccessible, "cannot access %s", target)
	}
}

// resolveRegistrable validates a path supplied for registration.
func (s *Service) resolveRegistrable(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", newError(CodeInvalidInput, "hostPath is required")
	}
	abs, err := filepath.Abs(strings.TrimSpace(raw))
	if err != nil {
		return "", wrapError(err, CodeInvalidInput, "cannot resolve %q", raw)
	}
	abs = filepath.Clean(abs)

	info, statErr := os.Stat(abs)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return "", wrapError(statErr, CodePathNotFound, "%s does not exist", abs)
	case statErr != nil:
		return "", wrapError(statErr, CodePathNotAccessible, "cannot access %s", abs)
	case !info.IsDir():
		return "", newError(CodeNotADirectory, "%s is not a directory", abs)
	}

	if pathutil.IsRoot(abs) {
		return "", newError(CodePathIsProjectsRoot,
			"%s is a filesystem root and cannot be registered as a project", abs)
	}

	roots := s.host.ProjectsRoots()
	root := s.host.OwningRoot(abs)
	if root == "" {
		return "", newError(CodeOutsideProjectsRoot,
			"%s is outside the configured Projects Roots (%s); add it as a Projects Root first",
			abs, strings.Join(roots, ", "))
	}
	if pathutil.Same(root, abs) {
		// Registering a root would make every project below it a child of a
		// registered project, and would run a session at the top of the tree.
		return "", newError(CodePathIsProjectsRoot,
			"%s is the Projects Root itself; register a project inside it instead", abs)
	}
	return abs, nil
}

// resolveParent determines the directory a new project is created inside.
func (s *Service) resolveParent(collectionPath, projectsRoot string) (string, error) {
	roots := s.host.ProjectsRoots()
	if len(roots) == 0 {
		return "", newError(CodeInvalidInput, "no Projects Root is configured")
	}

	if raw := strings.TrimSpace(collectionPath); raw != "" {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return "", wrapError(err, CodeInvalidInput, "cannot resolve %q", raw)
		}
		clean := filepath.Clean(abs)
		dir, err := s.requireDirectory(clean)
		if IsCode(err, CodePathNotFound) {
			// A collection is chosen by path, so a mistyped or not-yet-made
			// one is an ordinary mistake. Saying only "does not exist" leaves
			// the user stuck, since AgentMux never invents parent folders.
			return "", newError(CodePathNotFound,
				"%s does not exist; create that folder first, or leave the collection empty to create the project directly under a Projects Root",
				clean)
		}
		return dir, err
	}

	if raw := strings.TrimSpace(projectsRoot); raw != "" {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return "", wrapError(err, CodeInvalidInput, "cannot resolve %q", raw)
		}
		return s.requireDirectory(filepath.Clean(abs))
	}

	return s.requireDirectory(roots[0])
}

// requireDirectory verifies that path is an existing directory inside a
// configured Projects Root.
func (s *Service) requireDirectory(path string) (string, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", wrapError(err, CodePathNotFound, "%s does not exist", path)
	case err != nil:
		return "", wrapError(err, CodePathNotAccessible, "cannot access %s", path)
	case !info.IsDir():
		return "", newError(CodeNotADirectory, "%s is not a directory", path)
	}
	if !s.host.ContainsPath(path) {
		return "", newError(CodeOutsideProjectsRoot,
			"%s is outside the configured Projects Roots (%s)",
			path, strings.Join(s.host.ProjectsRoots(), ", "))
	}
	return path, nil
}

// buildProject assembles a Project record, translating the host path into a
// runtime path through the HostAdapter.
func (s *Service) buildProject(hostPath, name string) (*Project, error) {
	runtimePath, err := s.host.ToRuntimePath(hostPath)
	if err != nil {
		return nil, wrapError(err, CodeRuntimePathMappingFailed,
			"cannot express %s as a runtime path", hostPath)
	}
	collection, err := s.host.CollectionPathFor(hostPath)
	if err != nil {
		return nil, wrapError(err, CodeRuntimePathMappingFailed,
			"cannot determine the collection folder for %s", hostPath)
	}
	id, err := s.newID()
	if err != nil {
		return nil, wrapError(err, CodeStorageFailure, "cannot generate a project id")
	}
	now := s.now().UTC()
	return &Project{
		ID:             id,
		Name:           name,
		HostPath:       hostPath,
		RuntimePath:    runtimePath,
		CollectionPath: collection,
		Status:         StatusStopped,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// withDerivedStatus fills in fields that are computed rather than stored.
//
// Phase 1 has no terminal runtime, so every project is stopped. When the
// SessionBackend lands, this is the single place that changes.
func withDerivedStatus(p *Project) *Project {
	if p == nil {
		return nil
	}
	p.Status = StatusStopped
	return p
}

// mapNotFound converts a repository miss into a project-model error.
func mapNotFound(err error, id string) error {
	if errors.Is(err, ErrNotFound) {
		return newError(CodeNotFound, "no project with id %q", id)
	}
	return err
}
