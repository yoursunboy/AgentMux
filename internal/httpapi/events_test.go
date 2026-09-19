package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/project"
)

// These tests exercise the two timeline endpoints through the real router, the
// real event service and the real SQLite statements. What they are about is the
// API: what a response contains, what it deliberately does not, and what a
// client is told when it asks for something that is not there.

// readTimeline is the response body of either endpoint, decoded.
type readTimeline struct {
	Events     []eventView `json:"events"`
	NextBefore string      `json:"nextBefore"`
}

// eventsOn asks the API for a project's timeline.
func (h *harness) eventsOn(t *testing.T, projectID, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/projects/" + projectID + "/events"
	if query != "" {
		target += "?" + query
	}
	return h.call(http.MethodGet, target, "")
}

// runtimeEventsOn asks the API for a runtime's timeline.
func (h *harness) runtimeEventsOn(t *testing.T, runtimeID, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/runtime/" + runtimeID + "/events"
	if query != "" {
		target += "?" + query
	}
	return h.call(http.MethodGet, target, "")
}

// record writes one event straight through the service, without going near a
// runtime.
//
// Recording directly rather than starting a runtime is what makes these tests
// about the API: the harness's backend is a double, so a failure here is a
// failure of the endpoint and never of a session.
func (h *harness) record(t *testing.T, projectID, runtimeID, eventType string, payload string) *event.AgentEvent {
	t.Helper()
	var raw json.RawMessage
	if payload != "" {
		raw = json.RawMessage(payload)
	}
	ev, err := h.events.CreateEvent(context.Background(), projectID, runtimeID,
		eventType, event.SourceRuntime, raw)
	if err != nil {
		t.Fatalf("recording a %s event: %v", eventType, err)
	}
	return ev
}

// assertNewestFirst checks a page against the order the API documents: newest
// first, and among events recorded in the same instant the larger identifier
// first.
//
// Tests assert the rule rather than an expected sequence because two events
// written one after the other can share a timestamp - the platform clock is
// coarser than the work between them - and a test that pinned which of the two
// came first would be asserting the clock's resolution. The storage layer's own
// tests pin the ordering exactly, with timestamps they choose.
func assertNewestFirst(t *testing.T, events []eventView) {
	t.Helper()
	for i := 1; i < len(events); i++ {
		newer, older := events[i-1], events[i]
		switch {
		case newer.CreatedAt.After(older.CreatedAt):
		case newer.CreatedAt.Equal(older.CreatedAt) && newer.ID > older.ID:
		default:
			t.Errorf("event %d (%s at %s) does not come before event %d (%s at %s)",
				i-1, newer.ID, newer.CreatedAt, i, older.ID, older.CreatedAt)
		}
	}
}

// indexByID turns a page into a lookup, so a test can assert what an event
// contains without depending on where in the page it landed.
func indexByID(events []eventView) map[string]eventView {
	out := make(map[string]eventView, len(events))
	for _, ev := range events {
		out[ev.ID] = ev
	}
	return out
}

// ---------------------------------------------------------------------------
// GET /api/projects/{id}/events
// ---------------------------------------------------------------------------

func TestProjectEventsReturnsATimeline(t *testing.T) {
	h := newHarness(t)
	projectID, hostPath := h.registerProject(t, "Events")
	runtimeID := project.SessionNameFor(projectID)

	first := h.record(t, projectID, runtimeID, event.TypeRuntimeStarted, `{"state":"running"}`)
	second := h.record(t, projectID, runtimeID, event.TypeRuntimeStopped, `{"state":"stopped"}`)

	recorder := h.eventsOn(t, projectID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}

	body := decode[readTimeline](t, recorder)
	if len(body.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(body.Events))
	}
	assertNewestFirst(t, body.Events)
	if body.NextBefore != "" {
		t.Errorf("nextBefore = %q on a complete timeline, want it absent", body.NextBefore)
	}

	byID := indexByID(body.Events)
	if _, ok := byID[second.ID]; !ok {
		t.Fatalf("the timeline is missing the second event; got %v", byID)
	}
	got, ok := byID[first.ID]
	if !ok {
		t.Fatalf("the timeline is missing the first event; got %v", byID)
	}
	if got.ProjectID != projectID {
		t.Errorf("projectId = %q, want %q", got.ProjectID, projectID)
	}
	if got.RuntimeID != runtimeID {
		t.Errorf("runtimeId = %q, want %q", got.RuntimeID, runtimeID)
	}
	if got.Type != event.TypeRuntimeStarted {
		t.Errorf("type = %q, want %q", got.Type, event.TypeRuntimeStarted)
	}
	if got.Source != event.SourceRuntime {
		t.Errorf("source = %q, want %q", got.Source, event.SourceRuntime)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("createdAt = %s, want %s", got.CreatedAt, first.CreatedAt)
	}
	if string(got.Payload) != `{"state":"running"}` {
		t.Errorf("payload = %s, want what was recorded", got.Payload)
	}

	// §十一: no internal path reaches the client. The project's host path is the
	// one this server certainly knows and has no reason to say.
	if strings.Contains(recorder.Body.String(), hostPath) {
		t.Errorf("the response contains the project's host path: %s", recorder.Body.String())
	}
}

// TestProjectEventsEmptyTimelineIsAnEmptyList pins the shape a client can
// render without a special case.
func TestProjectEventsEmptyTimelineIsAnEmptyList(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "Empty")

	recorder := h.eventsOn(t, projectID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	// Read as raw text, because the difference between `[]` and `null` is the
	// thing being tested and a decoded nil slice cannot tell them apart.
	if body := recorder.Body.String(); !strings.Contains(body, `"events":[]`) {
		t.Errorf("body = %s, want an empty array rather than null", body)
	}
	if strings.Contains(recorder.Body.String(), "nextBefore") {
		t.Errorf("body = %s, want no nextBefore on an empty timeline", recorder.Body.String())
	}
}

// TestProjectEventsHidesTheOptionalFieldsWhenTheyAreEmpty keeps a project-level
// event from carrying an empty runtime id or a null payload, which a client
// would otherwise have to test for before using.
func TestProjectEventsHidesTheOptionalFieldsWhenTheyAreEmpty(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "Sparse")
	h.record(t, projectID, "", event.TypeRuntimeStarted, "")

	body := h.eventsOn(t, projectID, "").Body.String()
	if strings.Contains(body, "runtimeId") {
		t.Errorf("body = %s, want no runtimeId on a project-level event", body)
	}
	if strings.Contains(body, "payload") {
		t.Errorf("body = %s, want no payload when there is none", body)
	}
}

// TestProjectEventsRejectsAnUnknownProject pins the deliberate asymmetry with
// the runtime endpoint below: a project is a stored resource, so it can be
// looked up, and a 404 about the project is a different answer from an empty
// timeline - the second is the one a client cannot tell apart from a project
// with nothing in its history.
func TestProjectEventsRejectsAnUnknownProject(t *testing.T) {
	h := newHarness(t)

	h.wantError(t, h.eventsOn(t, "p_0123456789abcdef0123", ""),
		http.StatusNotFound, project.CodeNotFound)
}

// TestProjectEventsUnsatisfiablePathIsAnAPIError records that a path the router
// cannot match is answered as JSON rather than as a page of the SPA.
func TestProjectEventsUnsatisfiablePathIsAnAPIError(t *testing.T) {
	h := newHarness(t)

	for _, target := range []string{"/api/projects/p_1/extra/events", "/api/projects/events"} {
		recorder := h.call(http.MethodGet, target, "")
		if recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want 404", target, recorder.Code)
			continue
		}
		if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
			t.Errorf("GET %s has Content-Type %q, want JSON rather than a page", target, contentType)
		}
	}
}

// ---------------------------------------------------------------------------
// GET /api/runtime/{id}/events
// ---------------------------------------------------------------------------

func TestRuntimeEventsReturnsOnlyThatRuntime(t *testing.T) {
	h := newHarness(t)
	first, _ := h.registerProject(t, "First")
	second, _ := h.registerProject(t, "Second")

	mine := project.SessionNameFor(first)
	theirs := project.SessionNameFor(second)

	mineEvent := h.record(t, first, mine, event.TypeRuntimeStarted, `{"state":"running"}`)
	h.record(t, second, theirs, event.TypeRuntimeStarted, `{"state":"running"}`)
	// A project-level event belongs to the project's timeline and to no
	// runtime's.
	h.record(t, first, "", event.TypeRuntimeStarted, "")

	recorder := h.runtimeEventsOn(t, mine, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	body := decode[readTimeline](t, recorder)
	if len(body.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(body.Events))
	}
	if body.Events[0].ID != mineEvent.ID {
		t.Errorf("got event %s, want %s", body.Events[0].ID, mineEvent.ID)
	}
	if body.Events[0].RuntimeID != mine {
		t.Errorf("runtimeId = %q, want %q", body.Events[0].RuntimeID, mine)
	}
}

// TestRuntimeEventsAllowsAnEmptyTimeline records the asymmetry with the project
// endpoint: a runtime is not a stored resource, so there is nothing to look up
// and an id with no events is honestly an empty timeline.
func TestRuntimeEventsAllowsAnEmptyTimeline(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "NoRuntime")

	recorder := h.runtimeEventsOn(t, project.SessionNameFor(projectID), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	if body := decode[readTimeline](t, recorder); len(body.Events) != 0 {
		t.Errorf("got %d events, want none", len(body.Events))
	}
}

// TestRuntimeEventsRejectsAnIDThatIsNotARuntime keeps the endpoint from being a
// general event search by another name.
func TestRuntimeEventsRejectsAnIDThatIsNotARuntime(t *testing.T) {
	h := newHarness(t)

	for _, id := range []string{"p_0123456789abcdef0123", "not-a-runtime", "1", "amx"} {
		h.wantError(t, h.runtimeEventsOn(t, id, ""), http.StatusBadRequest, CodeInvalidRequest)
	}
}

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

func TestEventsDefaultLimitIsFifty(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "Paging")

	for i := 0; i < event.DefaultLimit+10; i++ {
		h.record(t, projectID, "", event.TypeRuntimeStarted, "")
	}

	recorder := h.eventsOn(t, projectID, "")
	body := decode[readTimeline](t, recorder)
	if len(body.Events) != event.DefaultLimit {
		t.Fatalf("got %d events, want the default %d", len(body.Events), event.DefaultLimit)
	}
	if body.NextBefore == "" {
		t.Fatal("nextBefore is absent with more events to read")
	}
	// The cursor names the last event in this page, which is where the next page
	// starts - not the first event of the next one, which this page has not read.
	if body.NextBefore != body.Events[len(body.Events)-1].ID {
		t.Errorf("nextBefore = %q, want the last event in the page (%q)",
			body.NextBefore, body.Events[len(body.Events)-1].ID)
	}
}

func TestEventsHonoursLimit(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "Limit")
	for i := 0; i < 5; i++ {
		h.record(t, projectID, "", event.TypeRuntimeStarted, "")
	}

	recorder := h.eventsOn(t, projectID, "limit=2")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	body := decode[readTimeline](t, recorder)
	if len(body.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(body.Events))
	}
	if body.NextBefore == "" {
		t.Error("nextBefore is absent with three events still to read")
	}
}

// TestEventsClampsLimitAboveTheMaximum is §十二's ceiling: a client asking for a
// thousand gets MaxLimit and not an error, because it asked for as much as it
// could have.
func TestEventsClampsLimitAboveTheMaximum(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "Clamp")
	for i := 0; i < event.MaxLimit+20; i++ {
		h.record(t, projectID, "", event.TypeRuntimeStarted, "")
	}

	recorder := h.eventsOn(t, projectID, "limit=1000")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	body := decode[readTimeline](t, recorder)
	if len(body.Events) != event.MaxLimit {
		t.Fatalf("got %d events, want the maximum %d", len(body.Events), event.MaxLimit)
	}
	if body.NextBefore == "" {
		t.Error("nextBefore is absent with more events to read")
	}
}

// TestEventsPagingWalksTheWholeTimeline is the property a cursor exists for: a
// client that follows nextBefore to the end sees every event exactly once.
func TestEventsPagingWalksTheWholeTimeline(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "Walk")

	const total = 12
	for i := 0; i < total; i++ {
		h.record(t, projectID, "", event.TypeRuntimeStarted, "")
	}

	var (
		seen   []string
		before string
		pages  int
	)
	for {
		query := "limit=5"
		if before != "" {
			query += "&before=" + before
		}
		recorder := h.eventsOn(t, projectID, query)
		if recorder.Code != http.StatusOK {
			t.Fatalf("page %d: status = %d, body was %s", pages, recorder.Code, recorder.Body.String())
		}
		body := decode[readTimeline](t, recorder)
		pages++
		if pages > 20 {
			t.Fatal("paging did not terminate")
		}
		for _, ev := range body.Events {
			seen = append(seen, ev.ID)
		}
		if body.NextBefore == "" {
			break
		}
		before = body.NextBefore
	}

	if len(seen) != total {
		t.Fatalf("walked %d events over %d pages, want %d", len(seen), pages, total)
	}
	unique := map[string]bool{}
	for _, id := range seen {
		if unique[id] {
			t.Fatalf("event %s appeared on two pages", id)
		}
		unique[id] = true
	}
}

func TestEventsRejectsAMalformedQuery(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "BadQuery")

	cases := []struct {
		name  string
		query string
	}{
		{"limit is not a number", "limit=twenty"},
		{"limit is zero", "limit=0"},
		{"limit is negative", "limit=-5"},
		{"limit is fractional", "limit=2.5"},
		{"before is not an event id", "before=1"},
		{"before is a project id", "before=p_0123456789abcdef0123"},
		{"before is a truncated event id", "before=evt_0123"},
		{"before is an event id in the wrong case", "before=evt_0123456789ABCDEF0123456789ABCDEF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.wantError(t, h.eventsOn(t, projectID, tc.query), http.StatusBadRequest, CodeInvalidRequest)
		})
	}
}

// TestEventsCursorThatNamesNothingIsNotFound pins the mapping in respond.go: an
// event id that no stored event carries is a request about a resource that is
// not there, and the code the event model produces has to reach the client
// unchanged rather than being flattened into a 500.
func TestEventsCursorThatNamesNothingIsNotFound(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "BadCursor")
	h.record(t, projectID, "", event.TypeRuntimeStarted, "")

	missing, err := event.NewID()
	if err != nil {
		t.Fatalf("event.NewID: %v", err)
	}

	h.wantError(t, h.eventsOn(t, projectID, "before="+missing),
		http.StatusNotFound, event.CodeNotFound)
}

// TestEventsCursorIsScopedToTheTimeline is the isolation rule at the cursor: an
// event id held for one project does not widen another project's timeline.
func TestEventsCursorIsScopedToTheTimeline(t *testing.T) {
	h := newHarness(t)
	first, _ := h.registerProject(t, "CursorFirst")
	second, _ := h.registerProject(t, "CursorSecond")

	for i := 0; i < 4; i++ {
		h.record(t, first, "", event.TypeRuntimeStarted, "")
		h.record(t, second, "", event.TypeRuntimeStarted, "")
	}

	other := decode[readTimeline](t, h.eventsOn(t, second, "limit=1"))
	if len(other.Events) != 1 {
		t.Fatalf("got %d events for the second project, want 1", len(other.Events))
	}

	recorder := h.eventsOn(t, first, "before="+other.Events[0].ID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	body := decode[readTimeline](t, recorder)
	if len(body.Events) == 0 {
		t.Fatal("paging from another project's cursor returned nothing")
	}
	for _, ev := range body.Events {
		if ev.ProjectID != first {
			t.Fatalf("the first project's timeline contains an event for %q", ev.ProjectID)
		}
	}
}

func TestEventsAreIsolatedBetweenProjects(t *testing.T) {
	h := newHarness(t)
	first, _ := h.registerProject(t, "Alpha")
	second, _ := h.registerProject(t, "Beta")

	for i := 0; i < 3; i++ {
		h.record(t, first, "", event.TypeRuntimeStarted, "")
		h.record(t, second, "", event.TypeRuntimeStopped, "")
	}

	for _, projectID := range []string{first, second} {
		body := decode[readTimeline](t, h.eventsOn(t, projectID, ""))
		if len(body.Events) != 3 {
			t.Fatalf("project %s has %d events, want its own 3", projectID, len(body.Events))
		}
		for _, ev := range body.Events {
			if ev.ProjectID != projectID {
				t.Errorf("project %s's timeline contains an event for %s", projectID, ev.ProjectID)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The surface
// ---------------------------------------------------------------------------

// TestEventsAreReadOnly pins the absence of a write endpoint. Events are
// produced by the layers that know what happened; an endpoint that accepted one
// from a client would let anything that can reach this server put a row into an
// append-only history that nothing can correct afterwards.
//
// The status is asserted loosely because two answers are both correct: the
// router may refuse the method, or the /api catch-all may report that no
// endpoint matches. What must not happen is a success.
func TestEventsAreReadOnly(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "ReadOnly")

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		recorder := h.call(method, "/api/projects/"+projectID+"/events", `{"type":"runtime.started"}`)
		switch recorder.Code {
		case http.StatusOK, http.StatusCreated, http.StatusNoContent:
			t.Errorf("%s /api/projects/{id}/events returned %d; the event log is read-only",
				method, recorder.Code)
		}
		if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
			t.Errorf("%s /api/projects/{id}/events has Content-Type %q, want a JSON error",
				method, contentType)
		}
	}
}

// TestTimelineExplainsItselfWithoutAnEventLog covers the optional-collaborator
// case: a server built without an event service says so rather than panicking,
// exactly as the terminal endpoints do. The service is a pointer field, so
// nothing but a check in the handler keeps a nil one from being dereferenced.
func TestTimelineExplainsItselfWithoutAnEventLog(t *testing.T) {
	server := &Server{}

	cases := []struct {
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"/api/projects/p_1/events", server.handleListProjectEvents},
		{"/api/runtime/amx-p_1/events", server.handleListRuntimeEvents},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		tc.handler(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))

		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("%s returned %d, want 503", tc.path, recorder.Code)
		}
		if body := recorder.Body.String(); !strings.Contains(body, "event log") {
			t.Errorf("%s said %s, want it to explain what is missing", tc.path, body)
		}
	}
}

// TestTimelineRoutesAreRegistered records that both endpoints exist under the
// names the documentation uses. A route that was dropped would otherwise be
// answered by the SPA fallback with a 200 and a page of HTML.
func TestTimelineRoutesAreRegistered(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "Routes")

	for _, target := range []string{
		"/api/projects/" + projectID + "/events",
		"/api/runtime/" + project.SessionNameFor(projectID) + "/events",
	} {
		recorder := h.call(http.MethodGet, target, "")
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s returned %d, want 200; body was %s",
				target, recorder.Code, recorder.Body.String())
			continue
		}
		if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
			t.Errorf("GET %s has Content-Type %q, want JSON", target, contentType)
		}
	}
}

// TestTimelineResponsesAreNotCached records the caching posture rather than
// testing it: nothing in this build sets a cache header on an API response. The
// assertion is here so that adding one later is a decision about a growing,
// append-only resource rather than an accident.
func TestTimelineResponsesAreNotCached(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "Cache")

	recorder := h.eventsOn(t, projectID, "")
	for _, header := range []string{"Cache-Control", "ETag", "Last-Modified", "Expires"} {
		if value := recorder.Header().Get(header); value != "" {
			t.Errorf("%s = %q on a timeline response, want none", header, value)
		}
	}
}

// TestEventsLimitBoundaryValues walks the edges of the accepted range, because
// an off-by-one in a clamp is invisible everywhere except at the boundary.
func TestEventsLimitBoundaryValues(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "Boundary")

	// Five events, so every limit at or above five is bounded by the data rather
	// than by the ceiling, and the test says what it means.
	for i := 0; i < 5; i++ {
		h.record(t, projectID, "", event.TypeRuntimeStarted, "")
	}

	cases := []struct {
		limit string
		want  int
	}{
		{"1", 1},
		{"5", 5},
		{fmt.Sprint(event.DefaultLimit), 5},
		{fmt.Sprint(event.MaxLimit), 5},
		{fmt.Sprint(event.MaxLimit + 1), 5},
	}
	for _, tc := range cases {
		t.Run("limit="+tc.limit, func(t *testing.T) {
			recorder := h.eventsOn(t, projectID, "limit="+tc.limit)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body was %s", recorder.Code, recorder.Body.String())
			}
			if got := len(decode[readTimeline](t, recorder).Events); got != tc.want {
				t.Errorf("got %d events, want %d", got, tc.want)
			}
		})
	}
}

// TestEventIDInAResponseIsUsableAsACursor is the property the identifier format
// exists for at this layer: what a response hands out is what a request accepts
// back.
func TestEventIDInAResponseIsUsableAsACursor(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "CursorShape")
	for i := 0; i < 3; i++ {
		h.record(t, projectID, "", event.TypeRuntimeStarted, "")
	}

	first := decode[readTimeline](t, h.eventsOn(t, projectID, "limit=1"))
	if len(first.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(first.Events))
	}
	cursor := first.Events[0].ID
	if !event.ValidID(cursor) {
		t.Fatalf("the response's id %q is not one the endpoint would accept back", cursor)
	}

	second := h.eventsOn(t, projectID, "before="+cursor+"&limit=1")
	if second.Code != http.StatusOK {
		t.Fatalf("paging from a response's own id returned %d: %s", second.Code, second.Body.String())
	}
	page := decode[readTimeline](t, second)
	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(page.Events))
	}
	if page.Events[0].ID == cursor {
		t.Error("paging from an id returned that id again")
	}
}

// TestEventsTimestampsAreUTC pins §十七 at the boundary a client sees: what
// comes back is UTC, and turning it into a local time is the display layer's
// job.
func TestEventsTimestampsAreUTC(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.registerProject(t, "Zones")
	h.record(t, projectID, "", event.TypeRuntimeStarted, "")

	recorder := h.eventsOn(t, projectID, "")
	if body := recorder.Body.String(); !strings.Contains(body, `"createdAt":"`) {
		t.Fatalf("body = %s, want a createdAt", body)
	}

	body := decode[readTimeline](t, recorder)
	if len(body.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(body.Events))
	}
	at := body.Events[0].CreatedAt
	if at.Location() != time.UTC {
		t.Errorf("createdAt is in %s, want UTC", at.Location())
	}
	if at.After(time.Now().Add(time.Minute)) {
		t.Errorf("createdAt = %s, which is in the future", at)
	}
}

// ---------------------------------------------------------------------------
// Through the runtime
// ---------------------------------------------------------------------------

// TestRuntimeTimelineAfterARealDestroy is the end-to-end case a client actually
// hits, driven through the HTTP API: start a runtime, destroy it, and read the
// history that outlived it.
//
// It drives a real runtime through the harness's fake backend, so it is what
// says the bridge and the API agree on the same runtime id.
func TestRuntimeTimelineAfterARealDestroy(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "Lifecycle")
	runtimeID := project.SessionNameFor(projectID)

	if recorder := h.call(http.MethodPost, "/api/projects/"+projectID+"/runtime/start", ""); recorder.Code != http.StatusOK {
		t.Fatalf("starting the runtime: status = %d, body was %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.call(http.MethodDelete, "/api/projects/"+projectID+"/runtime", ""); recorder.Code != http.StatusOK {
		t.Fatalf("destroying the runtime: status = %d, body was %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.runtimeEventsOn(t, runtimeID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("reading the runtime's history: status = %d, body was %s", recorder.Code, recorder.Body.String())
	}
	body := decode[readTimeline](t, recorder)
	assertNewestFirst(t, body.Events)

	var types []string
	for _, ev := range body.Events {
		types = append(types, ev.Type)
		if ev.RuntimeID != runtimeID {
			t.Errorf("an event names runtime %q, want %q", ev.RuntimeID, runtimeID)
		}
		if ev.ProjectID != projectID {
			t.Errorf("an event names project %q, want %q", ev.ProjectID, projectID)
		}
	}
	if want := []string{event.TypeRuntimeStarted, event.TypeRuntimeDestroyed}; !sameSet(types, want) {
		t.Fatalf("timeline = %v, want %v", types, want)
	}

	// The runtime record is gone; what happened to it is not, and reading it
	// twice gives the same answer.
	after := decode[readTimeline](t, h.runtimeEventsOn(t, runtimeID, ""))
	if len(after.Events) != len(types) {
		t.Errorf("the history changed between two reads: %v then %d events", types, len(after.Events))
	}
}

// sameSet reports whether two slices hold the same strings, order aside.
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	counts := make(map[string]int, len(want))
	for _, s := range want {
		counts[s]++
	}
	for _, s := range got {
		counts[s]--
		if counts[s] < 0 {
			return false
		}
	}
	return true
}

// TestProjectTimelineShowsEveryRuntimeOfTheProject is the project-level view of
// the same history: a destroy is a fact about a runtime in the project's
// timeline, not only in the runtime's own.
func TestProjectTimelineShowsEveryRuntimeOfTheProject(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	projectID, _ := h.registerProject(t, "ProjectView")

	for _, step := range []struct{ method, path string }{
		{http.MethodPost, "/runtime/start"},
		{http.MethodPost, "/runtime/stop"},
		{http.MethodDelete, "/runtime"},
	} {
		recorder := h.call(step.method, "/api/projects/"+projectID+step.path, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s %s: status = %d, body was %s", step.method, step.path, recorder.Code, recorder.Body.String())
		}
	}

	body := decode[readTimeline](t, h.eventsOn(t, projectID, ""))
	assertNewestFirst(t, body.Events)

	var got []string
	for _, ev := range body.Events {
		got = append(got, ev.Type)
	}
	want := []string{
		event.TypeRuntimeStarted,
		event.TypeRuntimeStopped,
		event.TypeRuntimeDestroyed,
	}
	if !sameSet(got, want) {
		t.Fatalf("timeline = %v, want %v", got, want)
	}
}
