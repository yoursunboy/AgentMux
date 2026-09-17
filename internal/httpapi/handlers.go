package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/host"
	"github.com/kutonlagos/agentmux/internal/project"
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
type serverInfoResponse struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
	Phase   string `json:"phase"`
	Status  string `json:"status"`

	StartedAt     string `json:"startedAt"`
	UptimeSeconds int64  `json:"uptimeSeconds"`

	// InstallID is an opaque identifier for this installation, not a
	// credential.
	InstallID string `json:"installId,omitempty"`

	Host        string `json:"host"`
	HostArch    string `json:"hostArch"`
	RuntimeMode string `json:"runtimeMode"`
	RuntimeOS   string `json:"runtimeOs"`
	Distro      string `json:"distro,omitempty"`
	PathMapper  string `json:"pathMapper"`

	// ProjectsRoot is the first configured root: the default target for a new
	// project. ProjectsRoots is the full ordered list.
	ProjectsRoot   string   `json:"projectsRoot"`
	ProjectsRoots  []string `json:"projectsRoots"`
	DiscoveryDepth int      `json:"discoveryDepth"`

	DataDirectory string `json:"dataDirectory"`
	DatabasePath  string `json:"databasePath"`
	ConfigFile    string `json:"configFile,omitempty"`
	WebDirectory  string `json:"webDirectory,omitempty"`

	// TerminalRuntimeImplemented is false until Phase 2. A client must not
	// render a terminal it cannot fill.
	TerminalRuntimeImplemented bool `json:"terminalRuntimeImplemented"`

	Dependencies []host.Dependency `json:"dependencies"`
	Provider     providerStatus    `json:"provider"`

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

	now := s.now()
	response := serverInfoResponse{
		AppName: version.AppName,
		Version: version.Version,
		Phase:   version.Phase,
		Status:  "online",

		StartedAt:     s.startedAt.UTC().Format(time.RFC3339),
		UptimeSeconds: int64(now.Sub(s.startedAt).Seconds()),
		InstallID:     s.installID,

		Host:        string(info.HostOS),
		HostArch:    info.HostArch,
		RuntimeMode: string(info.RuntimeMode),
		RuntimeOS:   string(info.RuntimeOS),
		Distro:      info.Distro,
		PathMapper:  info.PathMapper,

		ProjectsRoot:   firstRoot,
		ProjectsRoots:  roots,
		DiscoveryDepth: s.discoverer.MaxDepth(),

		DataDirectory: s.cfg.DataDir,
		DatabasePath:  s.cfg.SQLitePath(),
		ConfigFile:    s.cfg.SourceFile,
		WebDirectory:  s.webDir,

		TerminalRuntimeImplemented: version.TerminalRuntimeImplemented,
		Dependencies:               s.host.CheckDependencies(ctx),
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
			"terminal":            false,
			"providerSwitch":      false,
			"claudeHooks":         false,
			"controllerTransfer":  false,
		},
		Warnings: append([]string(nil), s.cfg.Warnings...),
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
