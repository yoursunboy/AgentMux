package httpapi

import (
	"net/http"
	"time"

	"github.com/kutonlagos/agentmux/internal/attention"
)

// This file is the attention and action API: whether anybody needs to look at
// an agent, and what they might do about it.
//
// # Three reads and no writes
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
