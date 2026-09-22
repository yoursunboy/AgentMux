package httpapi

import (
	"net/http"
	"time"

	"github.com/kutonlagos/agentmux/internal/attention"
	"github.com/kutonlagos/agentmux/internal/controller"
)

// This file is the attention and action API: whether anybody needs to look at
// an agent, and what they might do about it.
//
// # Five reads and no writes
//
// There is no endpoint that answers an action. Not a POST, not a PATCH, not a
// "resolve" sub-resource. Every action here is something a person *looks at*,
// and §18 of the phase brief forbids the other kind: a PERMISSION_REQUEST action
// records that Claude asked for something, and an endpoint that answered it
// would make AgentMux a participant in a Claude session rather than an observer
// of one. That decision belongs to a phase with its own threat model.
//
// The consequence is worth stating where the routes are: a client reading these
// endpoints can find out that something is waiting, and the thing it does about
// it is at the terminal. docs/AGENT_ATTENTION.md §6.
//
// The last two are the console's rather than a project's: one queue across
// every project and one action by id. They are flat rather than nested under a
// project because the question they answer is not about one project - it is
// "which agent needs me", which is the one question a project-scoped route
// cannot be asked. They read through the controller aggregation, which is where
// the join onto project names lives; see docs/CONTROLLER_API.md §1.

// attentionTimeout bounds an attention request. These are single-row reads and
// small listings over a local file, so they get the metadata budget.
const attentionTimeout = 10 * time.Second

// attentionResponse is the body of GET /api/sessions/{id}/attention.
//
// The attention is returned bare, unlike the runtime and the agent, because it
// is the only thing the endpoint has to say and it has no siblings a later phase
// might add: a level and a reason are a closed description.
type attentionResponse = attention.Attention

// attentionListResponse is the body of GET /api/projects/{id}/attention.
type attentionListResponse struct {
	Attention []attention.Attention `json:"attention"`
	Count     int                   `json:"count"`
}

// actionListResponse is the body of GET /api/projects/{id}/actions.
type actionListResponse struct {
	Actions []attention.Action `json:"actions"`
	Count   int                `json:"count"`
}

// queueResponse is the body of GET /api/actions.
//
// The two counts describe what is *pending*, across every project, and they are
// not counts of the list: a caller that asked for ten rows still gets the true
// totals. A headline that was a page length would be a headline that changed
// when somebody scrolled, and the one thing a console's bar has to be is
// something a person can trust without scrolling.
type queueResponse struct {
	Actions  []controller.ActionItem `json:"actions"`
	Count    int                     `json:"count"`
	NeedsYou int                     `json:"needsYou"`
	Notices  int                     `json:"notices"`
}

// handleGetAttention implements GET /api/sessions/{id}/attention.
//
// The id is an attempt's, which is what attention is about. An attempt the log
// has never mentioned is a 404: every attempt that exists gets a level from its
// own creation event, so an absence means the attempt is not one this server
// knows.
func (s *Server) handleGetAttention(w http.ResponseWriter, r *http.Request) {
	if !s.requireAttention(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, attentionTimeout)
	defer cancel()

	a, err := s.attention.Attention(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, attentionResponse(a))
}

// handleListAttention implements GET /api/projects/{id}/attention.
//
// The project is resolved first, so an unknown id is answered as a missing
// project rather than as a project with nothing needing attention. Those two
// answers mean different things, and an empty list is the one a client cannot
// tell apart from a project that exists and is quiet.
func (s *Server) handleListAttention(w http.ResponseWriter, r *http.Request) {
	if !s.requireAttention(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, attentionTimeout)
	defer cancel()

	projectID := r.PathValue("id")
	if _, err := s.projects.Get(ctx, projectID); err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	limit, ok := limitFrom(w, r)
	if !ok {
		return
	}

	list, err := s.attention.ListAttentionByProject(ctx, projectID, limit)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK,
		attentionListResponse{Attention: list, Count: len(list)})
}

// handleListActions implements GET /api/projects/{id}/actions.
//
// It is the queue: pending first, then newest first. The project is resolved
// first, for the reason the attention listing gives.
func (s *Server) handleListActions(w http.ResponseWriter, r *http.Request) {
	if !s.requireAttention(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, attentionTimeout)
	defer cancel()

	projectID := r.PathValue("id")
	if _, err := s.projects.Get(ctx, projectID); err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	limit, ok := limitFrom(w, r)
	if !ok {
		return
	}

	actions, err := s.attention.ListActionsByProject(ctx, projectID, limit)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK,
		actionListResponse{Actions: actions, Count: len(actions)})
}

// handleListQueue implements GET /api/actions.
//
// It is the console's queue: every project's actions, pending first and then
// newest first, each with its project named, plus the two counts a bar shows.
//
// It reads through the controller aggregation rather than through the attention
// projection directly, because naming a project is a join and the controller is
// where the joins live - docs/CONTROLLER_API.md §1. The route is flat rather
// than nested under a project for the reason the file comment gives: the
// question is not about one project.
func (s *Server) handleListQueue(w http.ResponseWriter, r *http.Request) {
	if !s.requireController(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, controllerTimeout)
	defer cancel()

	limit, ok := limitFrom(w, r)
	if !ok {
		return
	}

	view, err := s.controller.Actions(ctx, limit)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, queueResponse{
		Actions:  view.Actions,
		Count:    view.Count,
		NeedsYou: view.NeedsYou,
		Notices:  view.Notices,
	})
}

// handleGetAction implements GET /api/actions/{id}.
//
// The action is returned bare, like an attempt's attention: it is the only
// thing the endpoint has to say. An id the queue has never held is a 404 - an
// absence means the action is not one this server knows, and answering with an
// empty object would be the one answer a client cannot tell from a real one.
func (s *Server) handleGetAction(w http.ResponseWriter, r *http.Request) {
	if !s.requireController(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, controllerTimeout)
	defer cancel()

	action, err := s.controller.Action(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, action)
}

// requireAttention refuses an attention request on a server built without the
// projection.
//
// The service is optional in Options the same way the others are: a test of the
// REST surface that is not about attention should not have to build one. A
// server without it explains itself rather than answering with a level it never
// derived.
func (s *Server) requireAttention(w http.ResponseWriter) bool {
	if s.attention == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal,
			"this server was started without an attention projection", nil)
		return false
	}
	return true
}
