package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// These tests are §七 of the phase brief, and the boundary they guard is not
// "does the debug endpoint work" - it is "is it there at all". A diagnostic
// surface that is switched off is still a surface, which is why the route is
// registered conditionally rather than the handler refusing; see
// internal/httpapi/debug.go.

// TestTheDebugEndpointIsAbsentWithoutDebug is the claim that matters most.
//
// It runs against the harness every other test in this package uses, which is
// the ordinary installation: debug off. The path must be a 404 with the API's
// own not-found code, not a 403, not an empty 200, and not a handler that
// explains itself - anything other than 404 would be this server telling a
// stranger that there is something here.
func TestTheDebugEndpointIsAbsentWithoutDebug(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			h.wantError(t, h.call(method, "/api/debug/runtime", ""), http.StatusNotFound, CodeNotFound)
		})
	}
}

// TestTheDebugEndpointReportsCounts is the endpoint's whole purpose: four
// numbers and a boolean, all of them about this process.
func TestTheDebugEndpointReportsCounts(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, debug: true})
	id, _ := h.registerProject(t, "app")
	h.startRuntime(t, id)

	recorder := h.call(http.MethodGet, "/api/debug/runtime", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("the response is not JSON: %v", err)
	}

	// The key set is asserted exactly. This is the same claim as the one
	// internal/usage makes about its table and it is made the same way: the
	// promise is kept by there being nowhere to put the thing that must not be
	// there, and a field added later fails this build rather than the promise.
	want := []string{"runtimeCount", "tmuxAvailable", "activeSessions", "websocketConnections", "subscriptions"}
	if len(body) != len(want) {
		t.Fatalf("the response has %d keys, want exactly %d: %v", len(body), len(want), body)
	}
	for _, key := range want {
		if _, ok := body[key]; !ok {
			t.Errorf("the response has no %q; got %v", key, body)
		}
	}

	// One project has a runtime, and this server can host a terminal, so both
	// of those are non-zero - which is what makes the assertions above about
	// the shape and these about the values.
	if got := body["runtimeCount"]; got != float64(1) {
		t.Errorf("runtimeCount = %v, want 1", got)
	}
	if got := body["tmuxAvailable"]; got != true {
		t.Errorf("tmuxAvailable = %v, want true", got)
	}
	if got := body["activeSessions"]; got != float64(1) {
		t.Errorf("activeSessions = %v, want 1", got)
	}
	// Nobody has opened a socket in a unit test, and zero is the honest answer.
	if got := body["websocketConnections"]; got != float64(0) {
		t.Errorf("websocketConnections = %v, want 0", got)
	}
}

// TestTheDebugEndpointSaysWhichConditionFailed pins the difference between the
// probe and the verdict.
//
// A machine with no tmux is the case this field exists for: an operator looking
// at a runtime that will not start needs to know whether the missing piece is
// tmux, and a single boolean answering "can a terminal run" would not say.
func TestTheDebugEndpointSaysWhichConditionFailed(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, tmuxMissing: true, debug: true})

	recorder := h.call(http.MethodGet, "/api/debug/runtime", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("the response is not JSON: %v", err)
	}
	if got := body["tmuxAvailable"]; got != false {
		t.Errorf("tmuxAvailable = %v, want false on a machine with no tmux", got)
	}
}

// TestTheDebugEndpointNeverCarriesTerminalContent is the security claim.
//
// It is made over the encoded bytes rather than over the struct, because the
// struct is what a later change would edit and the bytes are what a caller
// receives. The words below are the ones that would appear if a session name, a
// project name, a path or a byte of output ever reached this response.
func TestTheDebugEndpointNeverCarriesTerminalContent(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, debug: true})
	id, name := h.registerProject(t, "secret-project")
	h.startRuntime(t, id)

	recorder := h.call(http.MethodGet, "/api/debug/runtime", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}

	body := strings.ToLower(recorder.Body.String())
	forbidden := []string{
		strings.ToLower(id),
		strings.ToLower(name),
		strings.ToLower(h.root),
		strings.ToLower(h.dataDir),
		"password", "token", "secret", "prompt", "transcript", "input", "capture", "output",
	}
	for _, word := range forbidden {
		if strings.Contains(body, word) {
			t.Errorf("the response contains %q: %s", word, recorder.Body.String())
		}
	}
}

// TestTheDebugEndpointRefusesAnythingButARead pins that the one diagnostic
// endpoint that came back is not a second way to touch the runtime.
//
// The route is registered for GET alone, so a POST is answered by the /api/
// catch-all rather than by a mux 405: the catch-all matches every method, so
// what a write to this path gets is the same "no API endpoint matches" that any
// path that does not exist gets. That is the stronger answer of the two - it
// does not distinguish a real endpoint from an invented one - and it is what
// the other unwritable surfaces in this API already return.
func TestTheDebugEndpointRefusesAnythingButARead(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true, debug: true})
	id, _ := h.registerProject(t, "app")
	h.startRuntime(t, id)
	before := h.runtime.MonitorStats().Active

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			h.wantError(t, h.call(method, "/api/debug/runtime", `{}`), http.StatusNotFound, CodeNotFound)
		})
	}

	if after := h.runtime.MonitorStats().Active; after != before {
		t.Errorf("the runtime count went from %d to %d across four refused writes", before, after)
	}
}
