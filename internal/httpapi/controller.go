package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/kutonlagos/agentmux/internal/controller"
)

// This file is the controller API: one response for a console, instead of the
// seven endpoints it would otherwise have to call and join itself.
//
// # Two reads and no writes
//
// Every route here is a GET. There is no POST, no PATCH and no DELETE, and
// their absence is not an omission: this is a read model over services that
// already have their own ways to be changed, and a second way to change them
// would be a second place every rule about them is enforced.
//
// docs/CONTROLLER_API.md is the long form.

// controllerTimeout bounds a dashboard request.
//
// It is wider than the metadata budget because the request reads four things
// rather than one, and the widest of them is a listing over every project. It is
// still a read of a local file and a few in-memory maps, so it is not the
// runtime budget either.
const controllerTimeout = 15 * time.Second

// controllerProjectsResponse is the body of GET /api/controller/projects.
//
// The dashboard response is returned bare - it is a closed description with
// nothing a later phase would append - but this one is wrapped, so that a
// pagination cursor or a filter summary could be added beside the list without
// changing the shape of what a client already parses.
type controllerProjectsResponse struct {
	Projects []controller.ProjectCard `json:"projects"`
	Count    int                      `json:"count"`
}

// handleController implements GET /api/controller.
//
// It is the whole console: what this server is, and one card per project with
// the project's runtime, agent, attention and pending work on it.
func (s *Server) handleController(w http.ResponseWriter, r *http.Request) {
	if !s.requireController(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, controllerTimeout)
	defer cancel()

	dashboard, err := s.controller.Dashboard(ctx, s.controllerServer(ctx))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, dashboard)
}

// handleControllerProjects implements GET /api/controller/projects.
//
// The cards without the server block, for a client that already knows what
// server it is talking to - one that polled the dashboard once and now only
// wants what changed.
func (s *Server) handleControllerProjects(w http.ResponseWriter, r *http.Request) {
	if !s.requireController(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, controllerTimeout)
	defer cancel()

	projects, err := s.controller.Projects(ctx)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK,
		controllerProjectsResponse{Projects: projects, Count: len(projects)})
}

// controllerServer reduces this server's own report to what a console needs.
//
// It calls serverInfo rather than assembling anything itself, which is §5 of the
// phase brief: the report is built in one place and a dashboard takes the fields
// it wants from it. A second assembly would be a second answer to "can this
// machine run a terminal", and the two would drift on exactly the machine where
// the answer is interesting.
func (s *Server) controllerServer(ctx context.Context) controller.ServerSummary {
	info := s.serverInfo(ctx)
	return controller.ServerSummary{
		Status:                   info.Status,
		RuntimeAvailable:         info.RuntimeAvailable,
		RuntimeUnavailableReason: info.RuntimeUnavailableReason,
		Version:                  info.Version,
		UptimeSeconds:            info.UptimeSeconds,
	}
}

// requireController refuses a dashboard request on a server built without the
// aggregation.
//
// The service is optional in Options like the others, and a server without it
// explains itself rather than answering with a console it never built.
func (s *Server) requireController(w http.ResponseWriter) bool {
	if s.controller == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal,
			"this server was started without the controller aggregation", nil)
		return false
	}
	return true
}
