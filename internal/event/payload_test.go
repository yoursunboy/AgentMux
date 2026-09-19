package event

import (
	"strings"
	"testing"
)

// The payload rules are a security boundary rather than a formatting
// preference: a stored event is never updated and never deleted, so a
// credential that reaches one cannot be removed afterwards. These tests pin the
// boundary from both sides - what is refused, and what a legitimate payload
// looks like.

func TestCheckPayloadAcceptsWhatTheRuntimeWrites(t *testing.T) {
	valid := []string{
		``,
		`null`,
		`{}`,
		`{"state":"running"}`,
		`{"state":"running","cols":120,"rows":30}`,
		`{"operation":"start","state":"error"}`,
		`{"nested":{"deeply":{"value":1}}}`,
		`{"list":[{"state":"running"},{"cols":80}]}`,
		`{"message":"the session could not be created"}`,
		// A field whose *name* merely contains a suspicious word is fine; the
		// check is on the field name, not on the text of the payload.
		`{"notes":"the tokenizer failed"}`,
		`{"count":42,"flag":true,"nothing":null}`,
	}
	for _, payload := range valid {
		if err := CheckPayload([]byte(payload)); err != nil {
			t.Errorf("CheckPayload(%s) = %v, want it accepted", payload, err)
		}
	}
}

func TestCheckPayloadRefusesCredentialFields(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"password", `{"password":"hunter2"}`},
		{"passwd", `{"passwd":"hunter2"}`},
		{"pass", `{"pass":"hunter2"}`},
		{"pwd", `{"pwd":"hunter2"}`},
		{"token", `{"token":"abc"}`},
		{"access token", `{"access_token":"abc"}`},
		{"access token camel", `{"accessToken":"abc"}`},
		{"refresh token", `{"refresh_token":"abc"}`},
		{"id token", `{"idToken":"abc"}`},
		{"auth token", `{"auth-token":"abc"}`},
		{"secret", `{"secret":"abc"}`},
		{"client secret", `{"clientSecret":"abc"}`},
		{"credential", `{"credential":"abc"}`},
		{"credentials", `{"credentials":{"user":"x"}}`},
		{"api key", `{"api_key":"abc"}`},
		{"api key camel", `{"apiKey":"abc"}`},
		{"api key upper", `{"API-KEY":"abc"}`},
		{"api key spaced", `{"api key":"abc"}`},
		{"authorization", `{"authorization":"Bearer abc"}`},
		{"private key", `{"private_key":"..."}`},
		{"ssh key", `{"sshKey":"..."}`},
		{"cookie", `{"cookie":"session=1"}`},
		{"session id", `{"session_id":"abc"}`},

		// Prefixed, which is how these fields are actually named. A check that
		// matched only the bare word would let every one of these through.
		{"db password", `{"db_password":"hunter2"}`},
		{"github token", `{"github_token":"ghp_x"}`},
		{"aws access key", `{"aws_secret_access_key":"x"}`},
		{"secret key", `{"secret_key":"x"}`},
		{"user pass", `{"user_pass":"hunter2"}`},
		{"service credential", `{"service_credential":"x"}`},

		// Nested, in both containers.
		{"nested object", `{"outer":{"inner":{"token":"abc"}}}`},
		{"inside an array", `{"items":[{"state":"running"},{"password":"x"}]}`},
		{"inside a nested array", `{"items":[[{"client_secret":"x"}]]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPayload([]byte(tc.payload))
			if err == nil {
				t.Fatalf("CheckPayload(%s) was accepted, want a rejection", tc.payload)
			}
			if code := CodeOf(err); code != CodeInvalidEvent {
				t.Errorf("code = %q, want %q", code, CodeInvalidEvent)
			}
			if !strings.Contains(err.Error(), "names a") {
				t.Errorf("message = %q, want it to name what the field looks like", err.Error())
			}
		})
	}
}

// TestCheckPayloadNamesTheSameFieldEveryTime pins determinism. An error message
// that changed between two identical requests would be a report nobody could
// reproduce, so the walk visits keys in a fixed order rather than a map's.
func TestCheckPayloadNamesTheSameFieldEveryTime(t *testing.T) {
	payload := `{"zulu_token":"a","alpha_password":"b","mike_secret":"c"}`
	first := ""
	for i := 0; i < 20; i++ {
		err := CheckPayload([]byte(payload))
		if err == nil {
			t.Fatal("CheckPayload accepted a payload full of credentials")
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("run %d said %q, the first run said %q", i, err.Error(), first)
		}
	}
	// Sorted key order, so the alphabetically first offending field is named.
	if !strings.Contains(first, "alpha_password") {
		t.Errorf("message = %q, want the first key in sorted order", first)
	}
}

func TestCheckPayloadRefusesShape(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"array", `[1,2,3]`},
		{"string", `"just a string"`},
		{"number", `42`},
		{"boolean", `true`},
		{"truncated", `{"state":`},
		{"not json", `state=running`},
		{"two objects", `{"a":1}{"b":2}`},
		{"keys are not strings", `{1:2}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPayload([]byte(tc.payload))
			if err == nil {
				t.Fatalf("CheckPayload(%s) was accepted, want a rejection", tc.payload)
			}
			if code := CodeOf(err); code != CodeInvalidEvent {
				t.Errorf("code = %q, want %q", code, CodeInvalidEvent)
			}
		})
	}
}

func TestCheckPayloadRefusesAnOversizedPayload(t *testing.T) {
	filler := strings.Repeat("x", MaxPayloadBytes)
	payload := `{"message":"` + filler + `"}`

	err := CheckPayload([]byte(payload))
	if err == nil {
		t.Fatal("CheckPayload accepted a payload over the limit")
	}
	if code := CodeOf(err); code != CodeInvalidEvent {
		t.Errorf("code = %q, want %q", code, CodeInvalidEvent)
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("message = %q, want it to name the limit", err.Error())
	}
}

func TestCheckPayloadRefusesDeepNesting(t *testing.T) {
	// A few bytes of payload that nest far past the bound, exactly the shape a
	// walk over untrusted input should refuse rather than follow.
	deep := strings.Repeat(`{"a":`, maxPayloadDepth+10) + "1" + strings.Repeat("}", maxPayloadDepth+10)

	err := CheckPayload([]byte(deep))
	if err == nil {
		t.Fatal("CheckPayload accepted a deeply nested payload")
	}
	if !strings.Contains(err.Error(), "nests") {
		t.Errorf("message = %q, want it to be about nesting", err.Error())
	}
}

func TestCheckPayloadAllowsNestingAtTheBound(t *testing.T) {
	// The bound is a limit and not an off-by-one: maxPayloadDepth levels is
	// accepted, and the test above shows one more is not.
	ok := strings.Repeat(`{"a":`, maxPayloadDepth) + "1" + strings.Repeat("}", maxPayloadDepth)

	if err := CheckPayload([]byte(ok)); err != nil {
		t.Errorf("CheckPayload refused %d levels: %v", maxPayloadDepth, err)
	}
}

func TestNormalizeKey(t *testing.T) {
	cases := map[string]string{
		"apiKey":       "apikey",
		"api_key":      "apikey",
		"API-KEY":      "apikey",
		"api key":      "apikey",
		"api/key":      "apikey",
		"api.key":      "apikey",
		"PASSWORD":     "password",
		"state":        "state",
		"cols":         "cols",
		"sessionId":    "sessionid",
		"clientSecret": "clientsecret",
	}
	for input, want := range cases {
		if got := normalizeKey(input); got != want {
			t.Errorf("normalizeKey(%q) = %q, want %q", input, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The log
// ---------------------------------------------------------------------------

// TestStringOmitsThePayload pins §十八. The payload is the one field a caller
// shapes, and a log line has no key check in front of it, so a credential that
// somehow got into a payload must not reach the log by way of a String call.
func TestStringOmitsThePayload(t *testing.T) {
	ev := &AgentEvent{
		ID:        "evt_0123456789abcdef0123456789abcdef",
		ProjectID: "p_abc",
		RuntimeID: "amx-p_abc-1",
		Type:      TypeRuntimeStarted,
		Source:    SourceRuntime,
		Payload:   []byte(`{"secret":"do-not-log-me"}`),
	}

	rendered := ev.String()
	if strings.Contains(rendered, "do-not-log-me") {
		t.Fatalf("String() leaked the payload: %s", rendered)
	}
	for _, want := range []string{ev.ID, ev.Type, ev.ProjectID, ev.RuntimeID} {
		if !strings.Contains(rendered, want) {
			t.Errorf("String() = %q, want it to contain %q", rendered, want)
		}
	}

	projectLevel := &AgentEvent{ID: ev.ID, ProjectID: ev.ProjectID, Type: ev.Type}
	if strings.Contains(projectLevel.String(), "runtime=") {
		t.Errorf("String() = %q, want no runtime part for a project-level event", projectLevel.String())
	}
}

func TestNilEventString(t *testing.T) {
	var ev *AgentEvent
	if got := ev.String(); got != "event(nil)" {
		t.Errorf("String() on a nil event = %q", got)
	}
}
