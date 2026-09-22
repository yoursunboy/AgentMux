package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/claude"
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

	// RuntimeBackend and TmuxAvailable are the two halves of "what would run a
	// terminal here, and is it installed".
	//
	// RuntimeBackend comes from the runtime itself rather than from a constant
	// in this package, because it is the runtime's own name for itself: the
	// value recorded with every runtime row in the database is the same string,
	// so a report that disagreed with the database would be a report about a
	// backend that never ran. It is "tmux" today, and it is empty when the
	// manager has no backend to name.
	//
	// TmuxAvailable is the dependency probe's verdict, promoted from the
	// Dependencies list to a field a client can read without searching that
	// list for the right entry. It answers "is the program on the PATH"; it is
	// not the same question as RuntimeAvailable, which asks whether this process
	// could execute a terminal at all.
	RuntimeBackend string `json:"runtimeBackend,omitempty"`
	TmuxAvailable  bool   `json:"tmuxAvailable"`

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

	// The four fields below describe where this server keeps its own files.
	// They are empty unless debug mode is on, and empty is the only other thing
	// they can be - each is resolved during configuration and cannot legitimately
	// be blank - so a client can read their absence as "withheld" rather than
	// "misconfigured".
	//
	// Why they are withheld: see ServerConfig.Debug, and docs/SECURITY.md §7.
	// The short version is that this endpoint has no authentication, and a list
	// of directories on the host is a map of the disk which is useful to
	// somebody who has not been given one. Nothing about them is a credential;
	// they are simply not needed to use the API and are needed to probe it.
	DataDirectory string `json:"dataDirectory,omitempty"`
	DatabasePath  string `json:"databasePath,omitempty"`
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

	// Claude describes the coding agent this server would start inside a
	// project's runtime, and it is the answer from the code that will actually
	// launch it: the resolved binary, its version, and the command line.
	//
	// It carries no credential and no authentication state. Whether the CLI is
	// signed in is Claude's own business, is decided when it starts, and is
	// said on Claude's own terminal; this server does not look, so it does not
	// claim. Absent when no agent is configured, which is a legitimate way to
	// run AgentMux.
	Claude *claude.Installation `json:"claude,omitempty"`

	// Features lets the UI disable what this server cannot do yet, instead of
	// inferring capability from a version string.
	Features map[string]bool `json:"features"`

	Warnings []string `json:"warnings"`
}

// handleServerInfo implements GET /api/server.
// serverInfo assembles the server's own report.
//
// It is a method rather than the body of the handler so that the controller
// dashboard can reuse it. §5 of the phase brief asks that the existing report be
// called rather than re-derived, and the only way to make that true is for there
// to be one place it is built: a second assembly would be a second answer to
// "can this machine run a terminal", and the two would drift.
func (s *Server) serverInfo(ctx context.Context) serverInfoResponse {
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
	terminalReady := s.terminalCanRun(info, dependencies)

	// The agent is resolved here rather than reported from a cached capability,
	// because the answer is a property of the machine and not of the build: the
	// same binary on a laptop with Claude Code and on a server without one has
	// to say different things.
	var agentInstallation *claude.Installation
	agentReady := false
	if s.agent != nil {
		resolved := s.agent.Resolve(ctx)
		agentInstallation = &resolved
		agentReady = resolved.Available && terminalReady
	}

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

		RuntimeBackend: s.runtime.BackendName(),
		TmuxAvailable:  tmuxAvailable,

		ProjectsRoot:   firstRoot,
		ProjectsRoots:  roots,
		DiscoveryDepth: s.discoverer.MaxDepth(),

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

			// A coding agent needs the terminal it runs in. Reporting this true
			// on a machine where the runtime cannot execute would offer a user
			// a Start button for a process that has nowhere to run.
			"claudeRuntime": agentReady,

			"providerSwitch":     false,
			"claudeHooks":        false,
			"controllerTransfer": false,
		},
		Claude:   agentInstallation,
		Warnings: append([]string(nil), s.cfg.Warnings...),
	}

	// Where this server keeps its files. Off by default, and on only when an
	// operator has asked for it with -debug, AGENTMUX_DEBUG or server.debug.
	if s.cfg.Server.Debug {
		response.DataDirectory = s.cfg.DataDir
		response.DatabasePath = s.cfg.SQLitePath()
		response.ConfigFile = s.cfg.SourceFile
		response.WebDirectory = s.webDir
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
	return response
}

// handleServerInfo implements GET /api/server.
func (s *Server) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, 5*time.Second)
	defer cancel()

	writeJSON(w, s.log, http.StatusOK, s.serverInfo(ctx))
}

// healthResponse is the body of GET /health.
//
// It is three fields, and the shortness is the design. A health check is asked
// by a supervisor every few seconds, and its answer is read by a program that
// has to decide one thing: is this process working. Everything else - which
// paths it uses, what it has configured, who it is - is either useless to that
// decision or reconnaissance. docs/SECURITY.md §7 has the full list of what
// this endpoint deliberately does not say.
//
// There is no field here that could carry a credential, and no field that
// describes the filesystem. The three that remain are the process's liveness,
// its identity, and the one machine capability a person checking by hand
// actually wants.
type healthResponse struct {
	Status string `json:"status"`

	// Version is the release, and Commit the git revision it was built from.
	// The pair is what makes "did the upgrade take effect" a question curl can
	// answer: a service that restarted into the old binary looks identical to
	// one that restarted into the new one until you compare this. Commit is
	// absent for a binary built without the linker flag rather than reported as
	// an empty string, so that its presence means something.
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`

	// Runtime is "available" or "unavailable": whether a persistent terminal
	// session can execute on this machine at all.
	//
	// It is not "healthy" or "unhealthy", because the two are different
	// questions and the difference matters to whoever reads this. A server on a
	// host with no tmux is working perfectly - it manages projects, serves the
	// UI and answers every endpoint - it simply cannot host a terminal. Calling
	// that unhealthy would have a supervisor restart a process that is doing
	// nothing wrong, in a loop, and the restart would not install tmux.
	Runtime string `json:"runtime"`
}

// runtimeAvailable reports whether a persistent terminal session can run on
// this machine, probing for itself.
//
// It exists because two handlers now ask the question and neither of them has a
// reason to gather the probe results: GET /health and GET /api/health are
// terse answers for a script, so they ask this and are told yes or no. GET
// /api/server is the opposite - it publishes the probe results themselves - and
// so it calls terminalCanRun directly with the values it already holds rather
// than probing a second time.
//
// Whichever way it is reached, the rule is the one in terminalCanRun.
func (s *Server) runtimeAvailable(ctx context.Context) bool {
	return s.terminalCanRun(s.host.Info(ctx), s.host.CheckDependencies(ctx))
}

// uptime is how long this process has been serving, rendered for a reader.
//
// The rendering is time.Duration's own - "30s", "2m15s", "1h3m" - rather than a
// number of seconds, because this string is what an operator sees when they run
// curl against a service they have just restarted, and "a minute and a half" is
// the answer they are looking for. GET /api/server keeps its seconds as a
// number: that one is read by the dashboard, which formats it itself.
func (s *Server) uptime() string {
	return s.now().Sub(s.startedAt).Round(time.Second).String()
}

// handleHealth implements GET /health.
//
// It answers 200 whenever the process can serve a request at all. There is no
// unhealthy status, and that is deliberate: the only failures this handler
// could report are failures of the machine it runs on, and a health endpoint
// that returned 503 for a missing tmux would be asking a supervisor to restart
// AgentMux until tmux appeared.
//
// Readiness, as opposed to liveness, is the `runtime` field, and it is also
// reported by GET /api/server with its inputs and an explanation. This is the
// one-line version for a script.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, 5*time.Second)
	defer cancel()

	response := healthResponse{
		Status:  "ok",
		Version: version.Version,
		Commit:  version.ShortCommit(),
		Runtime: "unavailable",
	}
	if s.runtimeAvailable(ctx) {
		response.Runtime = "available"
	}
	writeJSON(w, s.log, http.StatusOK, response)
}

// apiHealthResponse is the body of GET /api/health.
//
// It exists beside GET /health rather than replacing it, and the difference
// between them is who reads each one. /health is for a supervisor: it is
// deliberately outside /api, it is the shortest thing that answers "is this
// process alive", and install.sh and the systemd unit both curl it. /api/health
// is for the beta's own clients - a person checking a deployment from a
// browser, and the usage collector that wants to stamp an environment - so it
// follows the API's naming and lives under the API's prefix where the CORS
// middleware, the request log and the error envelope already apply.
//
// The two answer the same question and must not disagree, which is why both
// call runtimeAvailable rather than each deriving it.
//
// The fields are the four the phase brief names, and there is no fifth. No
// password, no path, no environment, no credential, no build metadata beyond
// the version: this endpoint is unauthenticated like the rest of the API, and
// docs/SECURITY.md §7 is the argument for what it therefore may not say.
type apiHealthResponse struct {
	// Status is "ok" while the process can serve this request. Like /health,
	// there is no other value: this endpoint reports liveness, and the machine
	// facts are in RuntimeAvailable.
	Status string `json:"status"`

	// Version is the release this binary was built from.
	Version string `json:"version"`

	// Uptime is how long the process has been up, as a duration string.
	Uptime string `json:"uptime"`

	// RuntimeAvailable is whether a persistent terminal session can run here,
	// which is readiness rather than liveness and is a different question from
	// Status. A server with no tmux is up and answers every request; it simply
	// cannot host a terminal.
	RuntimeAvailable bool `json:"runtimeAvailable"`
}

// apiHealthStatusOK is the one value Status takes.
//
// It is a constant because it is compared in tests and in the deployment
// contract: a beta health check that a script greps for has to be a string that
// is spelled identically everywhere.
const apiHealthStatusOK = "ok"

// handleAPIHealth implements GET /api/health.
//
// It is a single call with no arguments and no side effects, and it is
// deliberately not a read of anything that could fail: a health check that
// returns 500 when the database is locked would be reporting a different thing
// from what it is asked. Storage failures are reported by every endpoint that
// needs storage.
func (s *Server) handleAPIHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, 5*time.Second)
	defer cancel()

	writeJSON(w, s.log, http.StatusOK, apiHealthResponse{
		Status:           apiHealthStatusOK,
		Version:          version.Version,
		Uptime:           s.uptime(),
		RuntimeAvailable: s.runtimeAvailable(ctx),
	})
}

// terminalCanRun reports whether a persistent terminal session can run on this
// machine.
//
// It is the one place the rule is written down, and it is written down once
// because two endpoints report it: GET /api/server publishes the verdict beside
// the probe results it was derived from, and GET /health publishes the verdict
// alone. A second copy of the expression is how the two would come to disagree
// about the same machine - and the disagreement would be invisible, because
// each answer is internally consistent.
//
// All three conditions are required, and they are three different kinds of
// fact. TerminalRuntimeImplemented is about this build; RuntimeAvailable is
// about which side of the WSL boundary the process is on; the tmux probe is
// about what is installed in the environment the runtime will actually use.
func (s *Server) terminalCanRun(info host.SystemInfo, dependencies []host.Dependency) bool {
	dep, ok := findDependency(dependencies, "tmux")
	return info.RuntimeAvailable && ok && dep.Available && version.TerminalRuntimeImplemented
}

// tmuxAvailable reports whether the tmux probe found a usable tmux.
//
// It is the probe alone, without the two conditions around it that make up
// terminalCanRun, and the debug endpoint asks for it in that form deliberately:
// an operator looking at a runtime that will not start needs to know which of
// the three conditions failed, and "the build is fine and the runtime is
// available but tmux is missing" is a different afternoon's work from the other
// two.
func (s *Server) tmuxAvailable(ctx context.Context) bool {
	dep, ok := findDependency(s.host.CheckDependencies(ctx), "tmux")
	return ok && dep.Available
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

// slotValue is a pinned-slot field that can tell "not sent" from "sent as
// null".
//
// The two mean different things here - leave the pin where it is, versus take
// it off - and a plain *int collapses them into one, which would make it
// impossible to distinguish a request that forgot the field from one that
// deliberately cleared it.
type slotValue struct {
	// Present is true when the field appeared in the body at all.
	Present bool
	// Slot is the requested slot, or nil for "no pin".
	Slot *int
}

// UnmarshalJSON records that the field was sent, then decodes it.
func (v *slotValue) UnmarshalJSON(data []byte) error {
	v.Present = true
	if strings.TrimSpace(string(data)) == "null" {
		v.Slot = nil
		return nil
	}
	var slot int
	if err := json.Unmarshal(data, &slot); err != nil {
		return err
	}
	v.Slot = &slot
	return nil
}

// updateProjectRequest is the body of PATCH /api/projects/{id}.
type updateProjectRequest struct {
	PinnedSlot slotValue `json:"pinnedSlot"`
}

// handleUpdateProject implements PATCH /api/projects/{id}.
//
// The only field it accepts is the workspace slot, and that is deliberate. A
// general "edit a project" endpoint is where every future field arrives with no
// decision attached to it; here, a caller asking to rename a project is told the
// field is unknown, which is a clearer answer than a write with no rule behind
// it. Archiving, renaming and removing stay where they already are.
func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.contextWithTimeout(r, 10*time.Second)
	defer cancel()

	var req updateProjectRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), nil)
		return
	}
	if !req.PinnedSlot.Present {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"the request must set pinnedSlot, to a slot number or to null", nil)
		return
	}

	p, err := s.projects.SetSlot(ctx, r.PathValue("id"), req.PinnedSlot.Slot)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, projectResponse{Project: p})
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
