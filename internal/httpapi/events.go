package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/project"
)

// This file is the read half of the event API: one project's timeline and one
// runtime's timeline.
//
// There is no write endpoint, and its absence is the design. Events are
// produced by the layers that know what happened - the runtime bridge today -
// and never by a client. An endpoint that accepted an event from a browser
// would let anything that can reach this server put a row in an append-only
// history that nothing can correct afterwards, which is the one property this
// table has that a status column does not.
//
// See docs/AGENT_EVENTS.md.

// eventsReadTimeout bounds a timeline read. It is a metadata query and not a
// runtime one, so it gets the metadata budget.
const eventsReadTimeout = 10 * time.Second

// eventsResponse is the body of both timeline endpoints.
//
// The events are wrapped rather than returned bare, for the same reason the
// runtime is: a later phase can add siblings - a total, a cursor that pages
// forwards - without changing the shape of every existing response.
type eventsResponse struct {
	Events []eventView `json:"events"`

	// NextBefore is the cursor for the following page, absent when this page is
	// the last one. It is the same value the client passes back as `before`.
	NextBefore string `json:"nextBefore,omitempty"`
}

// eventView is one event as this API returns it.
//
// It is a view rather than the stored struct returned directly, and the
// difference is the point: what a response contains is a decision this layer
// makes, not a consequence of a field somebody added to the model. A field
// added to event.AgentEvent for internal reasons does not appear here until
// somebody decides it should.
//
// The fields are exactly what docs/AGENT_EVENTS.md §6 documents: identifier,
// scope, type, source, payload and time. No path, no socket name, no host
// directory, and nothing that was not deliberately put in a payload - which is
// itself the field the event model refuses credentials in.
type eventView struct {
	ID        string          `json:"id"`
	ProjectID string          `json:"projectId"`
	RuntimeID string          `json:"runtimeId,omitempty"`
	Type      string          `json:"type"`
	Source    string          `json:"source"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
}

// handleListProjectEvents implements GET /api/projects/{id}/events.
//
// The project is resolved first, so that a timeline is only ever returned for a
// project that exists. An unknown id is a 404 about the project rather than an
// empty list about events, because those two answers mean different things and
// an empty list is the one a client cannot tell apart from a project with
// nothing in its history.
func (s *Server) handleListProjectEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireEvents(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, eventsReadTimeout)
	defer cancel()

	projectID := r.PathValue("id")
	if _, err := s.projects.Get(ctx, projectID); err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	opts, ok := listOptionsFrom(w, r)
	if !ok {
		return
	}

	page, err := s.events.ListProject(ctx, projectID, opts)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, eventsPage(page))
}

// handleListRuntimeEvents implements GET /api/runtime/{id}/events.
//
// The runtime is named by its id, which in this build is its session name - the
// same string the runtime resource reports as `session`. A runtime that was
// destroyed and started again keeps that id, so this is the history of the
// runtime across everything it has been through rather than of one session.
//
// There is no existence check to make here, because a runtime is not a stored
// resource: it exists while its session does, and the events about it outlive
// both. A runtime id that has no events is an empty timeline, which is the
// honest answer.
func (s *Server) handleListRuntimeEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireEvents(w) {
		return
	}
	ctx, cancel := s.contextWithTimeout(r, eventsReadTimeout)
	defer cancel()

	runtimeID := strings.TrimSpace(r.PathValue("id"))
	if !strings.HasPrefix(runtimeID, project.SessionPrefix) {
		// An id outside the namespace AgentMux names its runtimes in cannot be
		// one of its runtimes, so this is a bad request rather than an empty
		// timeline. The prefix is asked of the package that spells it, so there
		// is one spelling of the namespace and not two.
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			fmt.Sprintf("%q is not a runtime identifier; a runtime id is its session name, "+
				"which begins with %q", runtimeID, project.SessionPrefix), nil)
		return
	}
	opts, ok := listOptionsFrom(w, r)
	if !ok {
		return
	}

	page, err := s.events.ListRuntime(ctx, runtimeID, opts)
	if err != nil {
		writeServiceError(w, s.log, err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, eventsPage(page))
}

// eventsPage turns a service page into a response body.
func eventsPage(page event.Page) eventsResponse {
	events := make([]eventView, 0, len(page.Events))
	for _, stored := range page.Events {
		events = append(events, eventView{
			ID:        stored.ID,
			ProjectID: stored.ProjectID,
			RuntimeID: stored.RuntimeID,
			Type:      stored.Type,
			Source:    stored.Source,
			Payload:   stored.Payload,
			CreatedAt: stored.CreatedAt,
		})
	}
	return eventsResponse{Events: events, NextBefore: page.NextBefore}
}

// listOptionsFrom reads the pagination parameters, answering the request itself
// when one of them is malformed.
//
// The second result reports whether the caller should carry on.
func listOptionsFrom(w http.ResponseWriter, r *http.Request) (event.ListOptions, bool) {
	var opts event.ListOptions
	query := r.URL.Query()

	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest,
				fmt.Sprintf("limit must be a whole number, not %q", raw), nil)
			return opts, false
		}
		if value < 1 {
			// Zero is not "no limit" and is not "the default". A caller that
			// asked for no events has made a mistake, and silently answering
			// with fifty would hide it.
			writeError(w, http.StatusBadRequest, CodeInvalidRequest,
				fmt.Sprintf("limit must be at least 1, not %d", value), nil)
			return opts, false
		}
		// Above the ceiling the value is clamped rather than refused: a client
		// asking for a thousand wants as much as it can have, and the response
		// tells it whether another page remains. The ceiling is
		// event.MaxLimit, and the service applies the same one.
		opts.Limit = value
	}

	if raw := strings.TrimSpace(query.Get("before")); raw != "" {
		if !event.ValidID(raw) {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest,
				fmt.Sprintf("before must be an event id, not %q", raw), nil)
			return opts, false
		}
		opts.Before = raw
	}
	return opts, true
}

// requireEvents refuses a timeline request on a server built without an event
// log.
//
// The service is optional so that the REST tests that are not about events do
// not have to build one, exactly as the terminal hub is optional. A server
// without it explains itself rather than panicking.
func (s *Server) requireEvents(w http.ResponseWriter) bool {
	if s.events == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal,
			"this server was started without an event log", nil)
		return false
	}
	return true
}
