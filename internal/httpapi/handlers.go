package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/version"
)

// Bounds on the discovery depth a client may request for a single scan.
const (
	minDiscoveryDepth = 1
	maxDiscoveryDepth = 8
)

// providerStatus reports the state of the AI provider integration.
//
// It exists so the UI can disable the provider switch and say why, rather than
// showing a control that pretends an integration exists.
type providerStatus struct {
	Tool       string `json:"tool"`
	Integrated bool   `json:"integrated"`
	Status     string `json:"status"`
}

// serverInfoResponse is the body of GET /api/server. It contains no secrets:
// no credentials, tokens, or API keys, and no provider configuration.
//
// The installation identifier is deliberately not here. It is stored, and it
// stays in the settings table, but it is not something a client needs and it is
// not something an unauthenticated endpoint should hand out: an identifier that
// is stable across every request from a machine is a tracking token whether or
// not it is called a credential.
type serverInfoResponse struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
	Phase   string `json:"phase"`
	Status  string `json:"status"`

	StartedAt     string `json:"startedAt"`
	UptimeSeconds int64  `json:"uptimeSeconds"`

	Host        string `json:"host"`
	HostArch    string `json:"hostArch"`
	RuntimeMode string `json:"runtimeMode"`
	RuntimeOS   string `json:"runtimeOs"`
	Distro      string `json:"distro,omitempty"`
	PathMapper  string `json:"pathMapper"`

	// Environment is where the server process itself is running, which on
	// Windows is not where the runtime runs.
	Environment string `json:"environment"`

	// RuntimeAvailable reports whether a persistent terminal runtime can
	// execute here at all, and RuntimeUnavailableReason says what to do about
	// it when it cannot. A client must not render a terminal, or offer to start
	// one, while this is false.
	RuntimeAvailable         bool   `json:"runtimeAvailable"`
	RuntimeUnavailableReason string `json:"runtimeUnavailableReason,omitempty"`

	// ProjectsRoot is the first configured root: the default target for a new
	// project. ProjectsRoots is the full ordered list.
	ProjectsRoot   string   `json:"projectsRoot"`
	ProjectsRoots  []string `json:"projectsRoots"`
	DiscoveryDepth int      `json:"discoveryDepth"`

	DataDirectory string `json:"dataDirectory"`
	DatabasePath  string `json:"databasePath"`
	ConfigFile    string `json:"configFile,omitempty"`
	WebDirectory  string `json:"webDirectory,omitempty"`

	// TerminalRuntimeImplemented reports whether this build contains the
	// runtime at all. It is a statement about the software, while
	// RuntimeAvailable is a statement about the machine: a build with the
	// runtime installed on a Windows host that is not WSL has this true and
	// that false, and both facts matter.
	TerminalRuntimeImplemented bool `json:"terminalRuntimeImplemented"`

	// TerminalBlocker is why a terminal cannot be offered here, in one field,
	// and is empty when it can. It is the warning below, promoted: a client
	// that had to find the right sentence in a list of warnings would be a
	// second place the diagnosis is reconstructed, and the first place it
	// would get wrong is the machine that has two problems at once.
	TerminalBlocker string `json:"terminalBlocker,omitempty"`

	Dependencies []host.Dependency `json:"dependencies"`
	Provider     providerStatus    `json:"provider"`

	// Tmux describes the terminal runtime installation, and it is the answer
	// from the code that will actually run it: the version, the resolved path
	// of the binary, and where the per-project sockets live.
	//
	// It is reported beside the dependency probe rather than instead of it,
	// because the two answer different questions. The probe says whether a
	// program called tmux is on some PATH; this says which binary AgentMux will
	// execute and what version that binary is, which on a machine with two
	// tmux installations is the only one of the two that predicts behaviour.
	Tmux *session.TmuxStatus `json:"tmux,omitempty"`

	// Features lets the UI disable what this server cannot do yet, instead of
	// inferring capability from a version string.
	Features map[string]bool `json:"features"`

	Warnings []string `json:"warnings"`
}

// handleServerInfo implements GET /api/server.
func (s *Server) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, 5*time.Second)
	defer cancel()

	info := s.host.Info(ctx)
	roots := s.host.ProjectsRoots()
	firstRoot := ""
	if len(roots) > 0 {
		firstRoot = roots[0]
	}

	dependencies := s.host.CheckDependencies(ctx)
	tmuxAvailable := false
	if dep, ok := findDependency(dependencies, "tmux"); ok {
		tmuxAvailable = dep.Available
	}
	terminalReady := info.RuntimeAvailable && tmuxAvailable && version.TerminalRuntimeImplemented

	now := s.now()
	response := serverInfoResponse{
		AppName: version.AppName,
		Version: version.Version,
		Phase:   version.Phase,
		Status:  "online",

		StartedAt:     s.startedAt.UTC().Format(time.RFC3339),
		UptimeSeconds: int64(now.Sub(s.startedAt).Seconds()),

		Host:        string(info.HostOS),
		HostArch:    info.HostArch,
		RuntimeMode: string(info.RuntimeMode),
		RuntimeOS:   string(info.RuntimeOS),
		Distro:      info.Distro,
		PathMapper:  info.PathMapper,

		Environment:              string(info.Environment),
		RuntimeAvailable:         info.RuntimeAvailable,
		RuntimeUnavailableReason: info.RuntimeUnavailableReason,

		ProjectsRoot:   firstRoot,
		ProjectsRoots:  roots,
		DiscoveryDepth: s.discoverer.MaxDepth(),

		DataDirectory: s.cfg.DataDir,
		DatabasePath:  s.cfg.SQLitePath(),
		ConfigFile:    s.cfg.SourceFile,
		WebDirectory:  s.webDir,

		TerminalRuntimeImplemented: version.TerminalRuntimeImplemented,
		Dependencies:               dependencies,
		Provider: providerStatus{
			Tool:       "claude",
			Integrated: false,
			Status:     "not_integrated",
		},
		Features: map[string]bool{
			"projectRegistration": true,
			"projectCreation":     true,
			"projectDiscovery":    true,
			"gitInit":             true,

			// The runtime is a feature of the build, a property of the machine,
			// and a property of what is installed on it. All three have to hold
			// before a client may offer to start one, and the reason when they
			// do not is reported as a warning rather than left to be guessed.
			"terminal": terminalReady,

			"providerSwitch":     false,
			"claudeHooks":        false,
			"controllerTransfer": false,
		},
		Warnings: append([]string(nil), s.cfg.Warnings...),
	}

	// A runtime that cannot execute is worth saying in the warnings list,
	// because that is what the UI already renders as a banner. The reason is
	// the one a user can act on: which side of the boundary to start the server
	// on, or which environment is missing tmux.
	blocker := runtimeBlocker(info, dependencies, terminalReady)
	response.TerminalBlocker = blocker
	if blocker != "" {
		response.Warnings = append(response.Warnings, blocker)
	}

	// Which tmux this server will run, asked of the runtime itself. A backend
	// that cannot describe its installation leaves the field out rather than
	// filling it with a guess.
	if status, ok := s.runtime.RuntimeStatus(ctx); ok {
		response.Tmux = &status
	}
	if response.ProjectsRoots == nil {
		response.ProjectsRoots = []string{}
	}
	if response.Dependencies == nil {
		response.Dependencies = []host.Dependency{}
	}
	if response.Warnings == nil {
		response.Warnings = []string{}
	}
	writeJSON(w, s.log, http.StatusOK, response)
}

// runtimeBlocker explains why the terminal runtime is not ready, or "" when it
// is.
//
// The three cases are kept apart because they have three different fixes, and a
// user who is told the wrong one will spend their time on the wrong thing: a
// build without the runtime needs a different AgentMux, a server on the wrong
// side of the WSL boundary needs to be started inside the distribution, and a
// missing tmux needs installing - but only in the environment the probe
// actually looked in, which is not always the one the user is typing in.
func runtimeBlocker(info host.SystemInfo, deps []host.Dependency, ready bool) string {
	if ready {
		return ""
	}
	if !version.TerminalRuntimeImplemented {
		return "This build of AgentMux has no terminal runtime."
	}
	if !info.RuntimeAvailable {
		if reason := strings.TrimSpace(info.RuntimeUnavailableReason); reason != "" {
			return reason
		}
		return "The terminal runtime is not available in this environment."
	}

	dep, ok := findDependency(deps, "tmux")
	if !ok {
		// The host says a runtime can run here but the probe list does not
		// mention tmux at all, which means the two disagree. Say so rather than
		// inventing a diagnosis.
		return "The terminal runtime could not be confirmed: tmux was not probed for."
	}
	where := strings.TrimSpace(dep.ProbedIn)
	if where == "" {
		where = "the runtime environment"
	}
	return fmt.Sprintf(
		"Runtime unavailable: tmux is not installed in %s, where terminal sessions run. "+
			"Install it there (for example: sudo apt install tmux) and reload this page. "+
			"AgentMux will not install it for you.", where)
}

// findDependency looks a probe result up by program name.
func findDependency(deps []host.Dependency, name string) (host.Dependency, bool) {
	for _, dep := range deps {
		if dep.Name == name {
			return dep, true
		}
	}
	return host.Dependency{}, false
}

// projectListResponse is the body of GET /api/projects.
type projectListResponse struct {
	Projects []*project.Project `json:"projects"`
	Count    int                `json:"count"`
}

// handleListProjects implements GET /api/projects.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, 10*time.Second)
	defer cancel()

	filter := project.ListFilter{IncludeArchived: boolQuery(r, "includeArchived")}
	projects, err := s.projects.List(ctx, filter)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	if projects == nil {
		projects = []*project.Project{}
	}
	writeJSON(w, s.log, http.StatusOK, projectListResponse{Projects: projects, Count: len(projects)})
}

// handleGetProject implements GET /api/projects/{id}.
func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, 10*time.Second)
	defer cancel()

	p, err := s.projects.Get(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, projectResponse{Project: p})
}

// handleDiscoverProjects implements GET /api/projects/discover.
//
// Discovery only suggests. It never registers anything, and a candidate the
// user already has is returned marked as registered rather than filtered out.
func (s *Server) handleDiscoverProjects(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, s.discoveryTimeout()+5*time.Second)
	defer cancel()

	registered, err := s.projects.List(ctx, project.ListFilter{IncludeArchived: true})
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}

	discoverer := s.discoverer
	if requested := intQuery(r, "depth"); requested > 0 {
		depth := clamp(requested, minDiscoveryDepth, maxDiscoveryDepth)
		if depth != s.discoverer.MaxDepth() {
			custom, err := project.NewDiscoverer(project.DiscovererOptions{
				Host:           s.host,
				Depth:          depth,
				MaxCandidates:  s.cfg.Projects.MaxCandidates,
				MaxScannedDirs: s.cfg.Projects.MaxScanDirs,
				Timeout:        s.discoveryTimeout(),
				Logger:         s.log,
			})
			if err != nil {
				writeServiceError(w, s.log, err)
				return
			}
			discoverer = custom
		}
	}

	result, err := discoverer.Discover(ctx, registered)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, result)
}

// projectResponse is the body of the endpoints that return a single project.
type projectResponse struct {
	Project *project.Project `json:"project"`
}

// registerProjectRequest is the body of POST /api/projects/register.
type registerProjectRequest struct {
	HostPath string `json:"hostPath"`
	Name     string `json:"name"`
}

// handleRegisterProject implements POST /api/projects/register.
func (s *Server) handleRegisterProject(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, 15*time.Second)
	defer cancel()

	var req registerProjectRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}

	p, err := s.projects.Register(ctx, project.RegisterInput{
		HostPath: req.HostPath,
		Name:     req.Name,
	})
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusCreated, projectResponse{Project: p})
}

// createProjectRequest is the body of POST /api/projects.
type createProjectRequest struct {
	Name string `json:"name"`

	// CollectionPath is the collection folder to create the project inside.
	// Empty means the Projects Root itself.
	CollectionPath string `json:"collectionPath"`

	// ProjectsRoot selects a root when CollectionPath is empty.
	ProjectsRoot string `json:"projectsRoot"`

	InitGit bool `json:"initGit"`
}

// handleCreateProject implements POST /api/projects.
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, 30*time.Second)
	defer cancel()

	var req createProjectRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}

	p, err := s.projects.Create(ctx, project.CreateInput{
		Name:           req.Name,
		CollectionPath: req.CollectionPath,
		ProjectsRoot:   req.ProjectsRoot,
		InitGit:        req.InitGit,
	})
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusCreated, projectResponse{Project: p})
}

// discoveryTimeout is the configured scan budget.
func (s *Server) discoveryTimeout() time.Duration {
	return time.Duration(s.cfg.Projects.ScanTimeoutSec) * time.Second
}

// boolQuery reads a boolean query parameter. Anything other than a recognised
// true value is false.
func boolQuery(r *http.Request, name string) bool {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	return value == "1" || value == "true" || value == "yes"
}

// intQuery reads an integer query parameter, returning 0 when absent or
// unparsable.
func intQuery(r *http.Request, name string) int {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return 0
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return n
}

func clamp(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
