package httpapi

import (
	"net/http"
	"time"

	"github.com/kutonlagos/agentmux/internal/agentstate"
)

// This file is the agent state API: what the event log adds up to right now, as
// opposed to what happened, which is the timeline endpoints' subject.
//
// # Why the two are separate resources
//
// They answer different questions and have different costs. A timeline is read
// from `agent_events`, which is append-only and grows forever; a state is one
// row, derived from that log and kept current as it is written. A client that
// wants to know what an agent is doing should not have to page through
// everything that has ever happened to it, and a client that wants the history
// should not be handed a single overwritten row.
//
// # What is not here
//
// There is no write. Not a PATCH, not a PUT, not a POST. A state is what the
// events add up to, and an endpoint that let a client set one would make the
// state a second place the truth lives - and the two would disagree the moment
// the next event arrived. §15 of the phase brief is the rule and this is the
// whole of what it costs.

// agentStateTimeout bounds a state request. These are single-row reads and
// small listings over a local file, so they get the metadata budget.
const agentStateTimeout = 10 * time.Second

// agentStateResponse is the body of GET /api/sessions/{id}/state.
//
// The state is returned bare rather than wrapped, unlike the runtime and the
// agent. It is the only thing this endpoint has to say, and it has no siblings
// a later phase might add: a state is a closed description, and anything
// appended to it would be a different resource.
type agentStateResponse = agentstate.AgentState

// agentStateListResponse is the body of GET /api/projects/{id}/agent-states.
type agentStateListResponse struct {
	States []agentstate.AgentState `json:"states"`
	Count  int                     `json:"count"`
}

// handleGetAgentState implements GET /api/sessions/{id}/state.
//
// The id is an attempt's, which is what a state is about. A session is how the
// caller already refers to an attempt everywhere else in this API, so the state
// is reached the same way rather than through an id of its own.
//
// An attempt nothing has observed has no state, and that is a 404 rather than
// an empty object. It is the ordinary answer for an attempt that was created
// and never started, and for an agent that was started without a task - both of
// which are states of the world rather than failures. docs/AGENT_STATE.md §6.
func (s *Server) handleGetAgentState(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentStates(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, agentStateTimeout)
	defer cancel()

	state, err := s.agentStates.State(ctx, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, agentStateResponse(state))
}

// handleListAgentStates implements GET /api/projects/{id}/agent-states.
//
// The project is resolved first, so an unknown id is answered as a missing
// project rather than as a project with no agents. Those two answers mean
// different things, and an empty list is the one a client cannot tell apart
// from a project that exists and has nothing running.
func (s *Server) handleListAgentStates(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentStates(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, agentStateTimeout)
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
	states, err := s.agentStates.ListByProject(ctx, projectID, limit)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK,
		agentStateListResponse{States: states, Count: len(states)})
}

// requireAgentStates refuses a state request on a server built without the
// projection.
//
// The service is optional in Options the same way the event log and the task
// model are: a test of the REST surface that is not about states should not
// have to build one. A server without it explains itself rather than answering
// with a state it does not have.
func (s *Server) requireAgentStates(w http.ResponseWriter) bool {
	if s.agentStates == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal,
			"this server was started without an agent state projection", nil)
		return false
	}
	return true
}
