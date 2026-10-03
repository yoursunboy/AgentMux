package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/kutonlagos/agentmux/internal/claude"
	"github.com/kutonlagos/agentmux/internal/project"
	"github.com/kutonlagos/agentmux/internal/session"
)

// The two settings endpoints, end to end through the router.
//
// # Why this file exists at all
//
// Phase 7.5.1 built both routes and tested the rule underneath them -
// project.Service.SetPermissionMode refuses a mode that is not one of the three,
// and internal/project/settings_test.go walks the enum and the hostile values
// that check is made of. What was never exercised is the layer above it: the
// JSON body, the optionalString distinction between "absent" and "null", the
// status codes, and the envelope a client actually reads.
//
// That is the layer §15 of this phase's brief is about. "`manual; rm -rf /` must
// return 400" is a claim about a response, and a service test that asserts an
// error code is evidence for it rather than the thing itself. A handler that
// dropped the body on the floor, or answered 500, would pass every test the
// previous phase had.
//
// # What is deliberately not here
//
// The dashboard's card. `settings` is a section of the controller response and
// internal/controller is where it is folded in; this file is about the resource
// a card writes to.

// absentProjectID is a well-formed project identifier that names nothing.
//
// The shape matters: an id that is merely malformed is refused before storage
// is asked, and a test written against one would be testing the shape check
// while believing it was testing an absence.
const absentProjectID = "p_00000000000000000000"

// settingsOf reads one project's settings through the API.
func settingsOf(t *testing.T, h *harness, id string) settingsView {
	t.Helper()
	recorder := h.call(http.MethodGet, "/api/projects/"+id+"/settings", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET settings = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}
	return decode[settingsResponse](t, recorder).Settings
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// TestAnUnconfiguredProjectIsAnsweredWithTheDefaultMode is §十六's first case at
// the layer a client meets it.
//
// It is the whole of the phase's central claim in one assertion: a project
// nobody has chosen a mode for reads as `bypassPermissions`, and it reads that
// way without anything having been written.
func TestAnUnconfiguredProjectIsAnsweredWithTheDefaultMode(t *testing.T) {
	h := newHarness(t)
	p := h.register("checkout-service")

	got := settingsOf(t, h, p.ID)
	if got.PermissionMode != string(claude.PermissionBypass) {
		t.Errorf("permissionMode = %q, want %q", got.PermissionMode, claude.PermissionBypass)
	}

	// Reading is not configuring. The default is a value this server derives
	// from the absence of a row, so a read that created one would turn the
	// default into a decision somebody had made - and would make changing the
	// default in a later phase reach nobody.
	if mode := settingsOf(t, h, p.ID).PermissionMode; mode != string(claude.PermissionBypass) {
		t.Errorf("a second read answered %q", mode)
	}
}

// TestTheSettingsOfAnUnknownProjectAreNotFound pins the other absence.
//
// A project that does not exist and a project that does exist are different
// questions, and the first is a 404 rather than the default - answering with a
// mode would be this endpoint claiming a project is there.
func TestTheSettingsOfAnUnknownProjectAreNotFound(t *testing.T) {
	h := newHarness(t)

	h.wantError(t, h.call(http.MethodGet, "/api/projects/"+absentProjectID+"/settings", ""),
		http.StatusNotFound, project.CodeNotFound)
	h.wantError(t, h.patch("/api/projects/"+absentProjectID+"/settings",
		`{"permissionMode":"manual"}`), http.StatusNotFound, project.CodeNotFound)
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

// TestEveryOfferedModeCanBeStored covers §十六's cases 2 to 4 through the API.
//
// Each mode is written and then read back with a second request, because the
// question is what a client gets on its next read rather than what the write
// said - a handler that echoed the request would pass a test that only looked
// at the response to the PATCH.
func TestEveryOfferedModeCanBeStored(t *testing.T) {
	for _, mode := range claude.PermissionModes() {
		t.Run(string(mode), func(t *testing.T) {
			h := newHarness(t)
			p := h.register("checkout-service")

			recorder := h.patch("/api/projects/"+p.ID+"/settings",
				`{"permissionMode":`+jsonString(string(mode))+`}`)
			if recorder.Code != http.StatusOK {
				t.Fatalf("PATCH settings = %d, want 200; body was %s",
					recorder.Code, recorder.Body.String())
			}
			if body := decode[settingsResponse](t, recorder).Settings.PermissionMode; body != string(mode) {
				t.Errorf("the PATCH answered %q, want %q", body, mode)
			}
			if stored := settingsOf(t, h, p.ID).PermissionMode; stored != string(mode) {
				t.Errorf("a later read answered %q, want %q", stored, mode)
			}
		})
	}
}

// TestAProjectConfiguredBeforeThisPhaseKeepsItsMode is §三's promise - an
// existing project is not silently moved onto the new default - stated where a
// client can see it.
//
// `manual` is the value that matters: it was this build's default until Phase
// 7.5.2, so it is both what most stored rows hold and the value the default
// stopped handing out. The row is written through the API rather than planted
// in the database, because what is being checked is that a stored value
// outranks the default on the read path - and both halves of that are public
// behaviour.
func TestAProjectConfiguredBeforeThisPhaseKeepsItsMode(t *testing.T) {
	for _, mode := range []claude.PermissionMode{
		claude.PermissionManual,
		claude.PermissionAcceptEdits,
		claude.PermissionBypass,
	} {
		t.Run(string(mode), func(t *testing.T) {
			h := newHarness(t)
			p := h.register("checkout-service")

			recorder := h.patch("/api/projects/"+p.ID+"/settings",
				`{"permissionMode":`+jsonString(string(mode))+`}`)
			if recorder.Code != http.StatusOK {
				t.Fatalf("PATCH settings = %d, want 200; body was %s",
					recorder.Code, recorder.Body.String())
			}
			if got := settingsOf(t, h, p.ID).PermissionMode; got != string(mode) {
				t.Errorf("permissionMode = %q, want the stored %q", got, mode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// What is refused
// ---------------------------------------------------------------------------

// TestAnIllegalPermissionModeIsRefusedWithA400 is §十五 and §十六's eleventh case.
//
// Every value here is something a client can actually put in a JSON body - that
// is what makes this a test of the endpoint rather than of the vocabulary - and
// each is refused, not escaped. The assertion that nothing was stored is the
// half worth having: a 400 that wrote the value anyway would leave a row that a
// later read hands back as a mode.
func TestAnIllegalPermissionModeIsRefusedWithA400(t *testing.T) {
	for _, value := range []string{
		"xxx",
		"auto",    // a real CLI mode this build does not offer
		"dontAsk", // and another
		"plan",    // and the third
		"MANUAL",
		"manual; rm -rf /",
		"$(whoami)",
		"`id`",
		"manual'--dangerously-skip-permissions",
		"manual\n--dangerously-skip-permissions",
	} {
		t.Run(value, func(t *testing.T) {
			h := newHarness(t)
			p := h.register("checkout-service")

			body := h.wantError(t,
				h.patch("/api/projects/"+p.ID+"/settings",
					`{"permissionMode":`+jsonString(value)+`}`),
				http.StatusBadRequest, project.CodeInvalidInput)

			// The field is named in `details` so a client can point at it
			// without reading prose, and the message lists the vocabulary so a
			// caller that guessed is told what the choices are.
			if got := body.Error.Details["field"]; got != "permissionMode" {
				t.Errorf("details.field = %v, want %q", got, "permissionMode")
			}
			for _, mode := range claude.PermissionModes() {
				if !strings.Contains(body.Error.Message, string(mode)) {
					t.Errorf("the message does not name %q: %s", mode, body.Error.Message)
				}
			}

			// And nothing was stored: the project is still on the default, not
			// on the value that was refused.
			if got := settingsOf(t, h, p.ID).PermissionMode; got != string(claude.PermissionBypass) {
				t.Errorf("after a refused write the project reads %q, want the default", got)
			}
		})
	}
}

// TestASettingsRequestMustNameTheMode is the other 400.
//
// "Not sent" and "sent as null" are told apart from each other and from a mode
// that does not exist, and all three are refused - there is no mode that means
// "unset". They are a different code from an unknown value because they are a
// different mistake: one is a malformed request and the other is a well-formed
// request for something this build does not have.
func TestASettingsRequestMustNameTheMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"no field at all", `{}`},
		{"the field sent as null", `{"permissionMode":null}`},
		{"an empty body", ``},
		{"a field this resource does not have", `{"mode":"manual"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			p := h.register("checkout-service")

			h.wantError(t, h.patch("/api/projects/"+p.ID+"/settings", tc.body),
				http.StatusBadRequest, CodeInvalidRequest)
			if got := settingsOf(t, h, p.ID).PermissionMode; got != string(claude.PermissionBypass) {
				t.Errorf("after a refused request the project reads %q, want the default", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// And nothing else
// ---------------------------------------------------------------------------

// TestSavingASettingsDoesNotTouchTheRuntime is §八 and §十六's cases 8 to 10 at
// the layer where a restart would actually happen.
//
// A restart is a call to the runtime manager, so the runtime itself is what
// answers this - not the words on the card, and not a mock that a handler could
// have called instead of the real thing. The project is left *running* with
// output in its scrollback, which is what makes the three refusals observable:
// a stop would end the session, a start would refuse it as already running, and
// a restart would create a second session, moving `StartedAt` forward and
// throwing the scrollback away.
//
// That is why the assertions are about the session and the sequence rather than
// about a boolean somewhere. "Nothing was restarted" is a claim about a
// terminal that is still the same terminal, and only the terminal can make it.
func TestSavingASettingsDoesNotTouchTheRuntime(t *testing.T) {
	h := newHarnessOpts(t, harnessOptions{runtimeAvailable: true})
	p := h.register("checkout-service")
	h.startRuntime(t, p.ID)

	// Output first, so that a restart has something to lose. Without this a
	// fresh session and an untouched one would look alike.
	h.backend.deliver(t, project.SessionNameFor(p.ID), []byte("hello\n"))
	h.waitForSequence(t, p.ID, 1)

	before := h.runtimeState(t, p.ID)

	recorder := h.patch("/api/projects/"+p.ID+"/settings", `{"permissionMode":"acceptEdits"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PATCH settings = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}

	after := h.runtimeState(t, p.ID)
	if after.State != session.StateRunning {
		t.Errorf("the runtime is %s after a settings write, want %s", after.State, session.StateRunning)
	}
	if !after.SessionAlive {
		t.Error("the session is gone after a settings write; choosing a mode stops nothing")
	}
	if after.Session != before.Session {
		t.Errorf("the session changed from %q to %q", before.Session, after.Session)
	}
	// The two that a restart cannot fake its way past: a new session is created
	// now, and its output starts over at zero.
	if !after.StartedAt.Equal(before.StartedAt) {
		t.Errorf("the session was created again at %s; it was created at %s",
			after.StartedAt, before.StartedAt)
	}
	if after.Sequence != before.Sequence {
		t.Errorf("the sequence went from %d to %d; a restart discards the scrollback",
			before.Sequence, after.Sequence)
	}
}

// TestTheSettingsResponseSaysWhatItIsAndNothingElse is the disclosure check,
// made over the bytes a client receives rather than over the struct.
//
// It is the same shape the controller's own check takes, and for the same
// reason: the failure it guards against is a field added later by somebody who
// did not read docs/SECURITY.md. A settings resource has one thing to say, and
// the exact key set is the smallest true statement about it.
//
// Both responses are checked, and the read is checked twice - once for a
// project on the default and once for one that has been configured. The two
// answer through different code paths inside the service, so a response that
// was the right shape for one of them would not be evidence for the other.
func TestTheSettingsResponseSaysWhatItIsAndNothingElse(t *testing.T) {
	h := newHarness(t)
	unconfigured := h.register("checkout-service")
	configured := h.register("billing-service")
	if recorder := h.patch("/api/projects/"+configured.ID+"/settings",
		`{"permissionMode":"manual"}`); recorder.Code != http.StatusOK {
		t.Fatalf("PATCH settings = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
	}

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"a project on the default", unconfigured.ID},
		{"a project that has been configured", configured.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := h.call(http.MethodGet, "/api/projects/"+tc.id+"/settings", "")
			if recorder.Code != http.StatusOK {
				t.Fatalf("GET settings = %d, want 200; body was %s",
					recorder.Code, recorder.Body.String())
			}

			var raw map[string]json.RawMessage
			if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
				t.Fatalf("the response is not a JSON object: %v", err)
			}
			if len(raw) != 1 {
				t.Errorf("the response has %d top-level key(s) %v, want the envelope alone",
					len(raw), keysOf(raw))
			}

			var settings map[string]json.RawMessage
			if err := json.Unmarshal(raw["settings"], &settings); err != nil {
				t.Fatalf("settings is not a JSON object: %v", err)
			}
			if len(settings) != 1 {
				t.Errorf("settings has %d key(s) %v, want exactly permissionMode",
					len(settings), keysOf(settings))
			}
			if _, ok := settings["permissionMode"]; !ok {
				t.Errorf("settings carries no permissionMode: %v", keysOf(settings))
			}
		})
	}
}

// keysOf names the keys of a decoded object, so that a failure lists them
// rather than dumping raw JSON at the reader.
func keysOf(raw map[string]json.RawMessage) []string {
	out := make([]string, 0, len(raw))
	for key := range raw {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
