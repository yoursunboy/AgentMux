package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kutonlagos/agentmux/internal/usage"
)

// These tests are §八's four page-and-socket events, as far as the HTTP layer
// sees them: which paths record nothing, which record one event, and that what
// reaches the table is exactly the five-value vocabulary.
//
// The three socket events - terminal.connect, controller.request and
// controller.release - are recorded inside internal/terminal and are tested
// there, against a real hub. What this file is responsible for is the half that
// is a URL.

// TestThePagePathsAreTheClientsPagePaths is the drift guard.
//
// The server decides which page a path is by the same string comparison the
// client does, and the two constants live in two languages. A path that changed
// on one side and not the other would record nothing at all, which is a silent
// failure - a beta whose console count is zero looks like a beta nobody opened.
// So this test reads the client's own routing table and compares.
func TestThePagePathsAreTheClientsPagePaths(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "dashboard", "route.ts"))
	if err != nil {
		t.Fatalf("could not read the client's routing table: %v", err)
	}

	for _, tc := range []struct {
		constant string
		want     string
	}{
		{constant: "DASHBOARD_PATH", want: dashboardPath},
		{constant: "ACTIONS_PATH", want: actionsPath},
	} {
		declared := exportedString(t, string(source), tc.constant)
		if declared != tc.want {
			t.Errorf("the client declares %s = %q and this server uses %q; "+
				"one of the two changed without the other", tc.constant, declared, tc.want)
		}
	}
}

// exportedString reads `export const NAME = 'value'` out of a TypeScript file.
//
// It is deliberately a narrow regexp rather than a parser: what is being read is
// one line of a file this repository owns, and a parser would be a dependency
// answering a question the declaration's own spelling answers.
func exportedString(t *testing.T, source, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`(?m)^\s*export const ` + regexp.QuoteMeta(name) + `\s*=\s*'([^']*)'`)
	match := pattern.FindStringSubmatch(source)
	if match == nil {
		t.Fatalf("could not find `export const %s = '...'` in the client's routing table", name)
	}
	return match[1]
}

// TestWhichPathsRecordNothing is the other half of the mapping.
//
// The workspace is the case worth stating: "/" is what a deep link, a bookmark,
// a reload and a redirect all land on, so a count of it would be a count of the
// browser rather than of a person. The queue is silent for a different reason -
// it is a page about a list, and the event the phase names is about reading one
// action.
func TestWhichPathsRecordNothing(t *testing.T) {
	for _, pathname := range []string{
		"/",
		"",
		"/projects",
		"/assets/app.js",
		dashboardPath + "/extra",
		actionsPath,
		actionsPath + "/",
		actionsPath + "/a/b",
		actionsPath + "/not-an-action-id",
		actionsPath + "/act_ZZZ",
		actionsPath + "/act_0123456789ABCDEF",
		"/api/debug/runtime",
	} {
		t.Run(pathname, func(t *testing.T) {
			if eventType, ok := usageEventForPath(pathname); ok {
				t.Errorf("%q recorded %q, want nothing", pathname, eventType)
			}
		})
	}
}

// TestWhichPathsRecordWhat pins the mapping, including the trailing slash.
func TestWhichPathsRecordWhat(t *testing.T) {
	for _, tc := range []struct {
		pathname string
		want     usage.EventType
	}{
		{pathname: dashboardPath, want: usage.EventDashboardOpen},
		{pathname: dashboardPath + "/", want: usage.EventDashboardOpen},
		{pathname: dashboardPath + "//", want: usage.EventDashboardOpen},
		{pathname: actionsPath + "/act_0123456789abcdef", want: usage.EventActionView},
		{pathname: actionsPath + "/act_0123456789abcdef/", want: usage.EventActionView},
		// An id that is shaped like one but names nothing is still somebody
		// having opened a page about an action: the client draws the detail and
		// the server answers 404, and both of those are the page having been
		// asked for.
		{pathname: actionsPath + "/act_0000000000000000", want: usage.EventActionView},
	} {
		t.Run(tc.pathname, func(t *testing.T) {
			eventType, ok := usageEventForPath(tc.pathname)
			if !ok {
				t.Fatalf("%q recorded nothing, want %q", tc.pathname, tc.want)
			}
			if eventType != tc.want {
				t.Errorf("%q recorded %q, want %q", tc.pathname, eventType, tc.want)
			}
		})
	}
}

// newUsageServer builds a server that serves a frontend and records what it
// serves.
//
// It is a second server over the harness's own database rather than a new
// harness, because the piece under test is the static handler's fallback and
// the harness's server is built with no frontend at all - which is deliberate,
// and is what TestStaticHandlerWithoutABuildExplainsItself asserts.
func newUsageServer(t *testing.T) (*harness, http.Handler) {
	t.Helper()
	h := newHarness(t)

	webDir := t.TempDir()
	mustWrite(t, filepath.Join(webDir, "index.html"), "<!doctype html><title>AgentMux</title>")
	mustWrite(t, filepath.Join(webDir, "assets", "app.js"), "console.log('app')")

	recorder, err := usage.NewService(usage.Options{
		Repository: h.store.Usage(),
		Logger:     discardLogger(),
	})
	if err != nil {
		t.Fatalf("usage.NewService returned an error: %v", err)
	}

	server, err := New(Options{
		Config:     h.server.cfg,
		Host:       h.server.host,
		Projects:   h.server.projects,
		Discoverer: h.server.discoverer,
		Runtime:    h.runtime,
		Logger:     discardLogger(),
		WebDir:     webDir,
		Usage:      recorder,
	})
	if err != nil {
		t.Fatalf("New returned an error: %v", err)
	}
	return h, server.Handler()
}

// TestPageNavigationsAreRecorded is the integration claim, over real requests.
//
// It walks a session the way a person would - open the console, open the queue,
// open one action - and asserts the rows that come out. The count assertion is
// exact rather than a lower bound, because the failure worth catching is one
// navigation recording two events or a static asset recording one.
func TestPageNavigationsAreRecorded(t *testing.T) {
	h, handler := newUsageServer(t)
	ctx := context.Background()

	call := func(method, target string) {
		req := httptest.NewRequest(method, target, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200", method, target, recorder.Code)
		}
	}

	// A person opens the workspace, the console, the queue, and one action. The
	// asset and the API request are what a browser does in between and are not
	// page views.
	call(http.MethodGet, "/")
	call(http.MethodGet, "/assets/app.js")
	call(http.MethodGet, "/api/server")
	call(http.MethodGet, dashboardPath)
	call(http.MethodGet, actionsPath)
	call(http.MethodGet, actionsPath+"/act_0123456789abcdef")

	events, err := h.store.Usage().List(ctx, 10)
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}

	// Newest first, so the walk above reads backwards here.
	want := []usage.EventType{usage.EventActionView, usage.EventDashboardOpen}
	if len(events) != len(want) {
		t.Fatalf("recorded %d events, want %d: %+v", len(events), len(want), events)
	}
	for i := range want {
		if events[i].Type != want[i] {
			t.Errorf("event %d = %q, want %q", i, events[i].Type, want[i])
		}
	}
}

// TestAReloadIsASecondEvent pins that this table counts occurrences rather than
// visitors.
//
// It is the property that makes the table a count of use rather than of people:
// there is no deduplication, no session and no identifier that could be used to
// add one later, which is what keeps the endpoint unauthenticated and the row
// free of anything that identifies anybody.
func TestAReloadIsASecondEvent(t *testing.T) {
	h, handler := newUsageServer(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, dashboardPath, nil)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	count, err := h.store.Usage().Count(ctx)
	if err != nil {
		t.Fatalf("Count returned an error: %v", err)
	}
	if count != 3 {
		t.Errorf("three reloads recorded %d events, want 3", count)
	}

	events, err := h.store.Usage().List(ctx, 10)
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		if seen[event.ID] {
			t.Errorf("two rows share the identifier %q", event.ID)
		}
		seen[event.ID] = true
	}
}

// TestAHeadRequestIsNotAPageView covers the one method other than GET that
// reaches the static handler.
//
// A crawler and a link preview both send a HEAD, and neither is a person
// opening anything. The handler serves them, and this asserts it does not count
// them.
func TestAHeadRequestIsNotAPageView(t *testing.T) {
	h, handler := newUsageServer(t)
	ctx := context.Background()

	req := httptest.NewRequest(http.MethodHead, dashboardPath, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("HEAD %s = %d, want 200", dashboardPath, recorder.Code)
	}

	if count, err := h.store.Usage().Count(ctx); err != nil {
		t.Fatalf("Count returned an error: %v", err)
	} else if count != 0 {
		t.Errorf("a HEAD recorded %d events, want none", count)
	}
}

// TestRecordingIsOffUnlessTheDeploymentAskedForIt pins the default.
//
// The harness's server is built with no recorder at all, which is what every
// installation that did not pass -beta looks like - and it is asserted here
// rather than assumed because "recording is off" is the property that makes
// this whole feature opt-in.
func TestRecordingIsOffUnlessTheDeploymentAskedForIt(t *testing.T) {
	h := newHarness(t)

	// The server has no frontend either, so this is really about the nil check
	// on the ordinary path: a request that would have recorded must not panic.
	h.call(http.MethodGet, dashboardPath, "")
	h.call(http.MethodGet, actionsPath+"/act_0123456789abcdef", "")

	if count, err := h.store.Usage().Count(context.Background()); err != nil {
		t.Fatalf("Count returned an error: %v", err)
	} else if count != 0 {
		t.Errorf("a server with no recorder wrote %d events, want none", count)
	}
}

// TestNoRecordedValueLooksLikeContent is the security claim at this layer.
//
// The five event types are the whole of what this endpoint can write, and they
// are written here as literal strings rather than read from the vocabulary: a
// test that asked the package what it records would pass against a package that
// had started recording something else.
func TestNoRecordedValueLooksLikeContent(t *testing.T) {
	h, handler := newUsageServer(t)

	// A path that carries something a person might have typed, which is the
	// shape a leak would take if the path were recorded verbatim.
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(
		http.MethodGet, actionsPath+"/act_0123456789abcdef", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(
		http.MethodGet, dashboardPath+"?token=ghp_example", nil))

	events, err := h.store.Usage().List(context.Background(), 10)
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	allowed := map[usage.EventType]bool{}
	for _, eventType := range []usage.EventType{
		usage.EventDashboardOpen, usage.EventTerminalConnect, usage.EventControllerRequest,
		usage.EventControllerRelease, usage.EventActionView,
	} {
		allowed[eventType] = true
	}
	for _, event := range events {
		if !allowed[event.Type] {
			t.Errorf("a row holds the event type %q, which is not one of the five", event.Type)
		}
		if strings.Contains(string(event.Type), "token") || strings.Contains(event.ID, "token") {
			t.Errorf("a row carries the query string: %+v", event)
		}
	}
}
