package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// GET /api/health is the beta's own liveness probe, and these tests are about
// the two things it must be: the same answer as /health, and nothing more than
// the four fields it documents.
//
// The harness's clock is fixed - it started at 12:00:00Z and now is 12:00:30Z -
// so uptime is not merely present here, it is an exact string. That is worth
// asserting rather than asserting that the field is non-empty: a field that
// said "0s" on a server that had been up for half a minute would pass a
// non-empty check and would be exactly the bug §六 is about, since the
// deployment a person checks uptime on is the one that just restarted.

func TestAPIHealthReportsTheFourFields(t *testing.T) {
	for _, tc := range []struct {
		name             string
		runtimeAvailable bool
		wantAvailable    bool
	}{
		{name: "a host that can run a terminal", runtimeAvailable: true, wantAvailable: true},
		{name: "a host that cannot", runtimeAvailable: false, wantAvailable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessOpts(t, harnessOptions{runtimeAvailable: tc.runtimeAvailable})
			recorder := h.call(http.MethodGet, "/api/health", "")

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body was %s", recorder.Code, recorder.Body.String())
			}
			health := decode[apiHealthResponse](t, recorder)

			if health.Status != apiHealthStatusOK {
				t.Errorf("status = %q, want %q", health.Status, apiHealthStatusOK)
			}
			if health.Version == "" {
				t.Error("version is empty; a health check that cannot say which build answered cannot confirm an upgrade")
			}
			if health.Uptime != "30s" {
				t.Errorf("uptime = %q, want %q", health.Uptime, "30s")
			}
			if health.RuntimeAvailable != tc.wantAvailable {
				t.Errorf("runtimeAvailable = %v, want %v", health.RuntimeAvailable, tc.wantAvailable)
			}
		})
	}
}

// TestAPIHealthSaysNothingElse is the disclosure check, made the same way the
// one for /health is: the exact key set rather than a list of forbidden names,
// because the failure this guards against is a field added later by somebody
// who did not read docs/SECURITY.md.
//
// It is run with debug on as well as off, because debug is the setting that
// widens GET /api/server and it must not reach this endpoint at all.
func TestAPIHealthSaysNothingElse(t *testing.T) {
	for _, debug := range []bool{false, true} {
		h := newHarnessOpts(t, harnessOptions{debug: debug})
		recorder := h.call(http.MethodGet, "/api/health", "")

		var raw map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
			t.Fatalf("the response is not a JSON object: %v", err)
		}

		allowed := map[string]bool{
			"status": true, "version": true, "uptime": true, "runtimeAvailable": true,
		}
		if len(raw) != len(allowed) {
			t.Errorf("debug=%v: the response has %d keys %v, want exactly %d",
				debug, len(raw), raw, len(allowed))
		}
		for key := range raw {
			if !allowed[key] {
				t.Errorf("debug=%v: the health response contains %q, which is not one of the four",
					debug, key)
			}
		}
	}
}

// TestTheTwoHealthEndpointsAgree is the reason there is a shared helper.
//
// /health and /api/health answer different audiences - a supervisor and an API
// client - but they answer the same question about the same machine, and a
// second derivation of "can a terminal run here" is how the two would come to
// disagree. The check is made at both settings of runtimeAvailable, because a
// test that only ran the available case would pass against two expressions that
// both hardcoded true.
func TestTheTwoHealthEndpointsAgree(t *testing.T) {
	for _, available := range []bool{false, true} {
		h := newHarnessOpts(t, harnessOptions{runtimeAvailable: available})

		plain := decode[healthResponse](t, h.call(http.MethodGet, "/health", ""))
		api := decode[apiHealthResponse](t, h.call(http.MethodGet, "/api/health", ""))

		if plain.Version != api.Version {
			t.Errorf("available=%v: /health reports version %q and /api/health reports %q",
				available, plain.Version, api.Version)
		}
		want := "unavailable"
		if api.RuntimeAvailable {
			want = "available"
		}
		if plain.Runtime != want {
			t.Errorf("available=%v: /health reports runtime %q beside /api/health's runtimeAvailable %v",
				available, plain.Runtime, api.RuntimeAvailable)
		}
	}
}

// TestAPIHealthIsAnAPIAnswer pins that the path is answered by the API rather
// than by the frontend fallback.
//
// It is the counterpart of TestHealthIsNotPartOfTheAPISurface and it is what
// would break if the route were ever dropped: without it, /api/health falls
// through to the /api/ catch-all and answers 404 - or, with a built frontend
// present, would be caught by a fallback that returns a page and a 200.
func TestAPIHealthIsAnAPIAnswer(t *testing.T) {
	h := newHarness(t)
	recorder := h.call(http.MethodGet, "/api/health", "")

	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/health = %d, want 200", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Errorf("GET /api/health answered with Content-Type %q, want JSON", contentType)
	}
}
