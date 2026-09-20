package task

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAgentSessionStatusesAreExactlyTheDeclaredSet(t *testing.T) {
	// There is no WAITING, and its absence is asserted here rather than left to
	// the reader: it is the one status the task has that the session
	// deliberately does not, and it is the kind of difference a later edit
	// "harmonising" the two lists would remove without noticing.
	want := []string{"CREATED", "RUNNING", "COMPLETED", "FAILED", "CANCELLED"}
	got := AgentSessionStatuses()
	if len(got) != len(want) {
		t.Fatalf("AgentSessionStatuses() = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AgentSessionStatuses()[%d] = %q; want %q", i, got[i], want[i])
		}
	}
	for _, s := range got {
		if !ValidSessionStatus(s) {
			t.Errorf("ValidSessionStatus(%q) is false for a listed status", s)
		}
	}
	if ValidSessionStatus(StatusWaiting) {
		t.Error("ValidSessionStatus(WAITING) is true; a session has no WAITING state")
	}
	for _, s := range []string{"", "created", "Running", "DONE", "WAITING", "IDLE"} {
		if ValidSessionStatus(s) {
			t.Errorf("ValidSessionStatus(%q) is true; want false", s)
		}
	}
}

func TestSessionTerminal(t *testing.T) {
	cases := map[string]bool{
		StatusSessionCreated:   false,
		StatusSessionRunning:   false,
		StatusSessionCompleted: true,
		StatusSessionFailed:    true,
		StatusSessionCancelled: true,
	}
	for status, want := range cases {
		if got := SessionTerminal(status); got != want {
			t.Errorf("SessionTerminal(%q) = %v; want %v", status, got, want)
		}
	}
}

// TestSessionTransitionsAreExactlyTheLifecycle pins every edge, including the
// ones that are absent.
//
// The absence worth naming is COMPLETED -> RUNNING: a session that finished and
// then reported itself running again would make the first report false, and
// everything that read it would have been told something untrue. A second
// attempt is a second session, which is why this level exists.
func TestSessionTransitionsAreExactlyTheLifecycle(t *testing.T) {
	statuses := AgentSessionStatuses()
	allowed := map[string]map[string]bool{
		StatusSessionCreated: {
			StatusSessionRunning: true, StatusSessionFailed: true, StatusSessionCancelled: true,
		},
		StatusSessionRunning: {
			StatusSessionCompleted: true, StatusSessionFailed: true, StatusSessionCancelled: true,
		},
		StatusSessionCompleted: {},
		StatusSessionFailed:    {},
		StatusSessionCancelled: {},
	}

	for _, from := range statuses {
		for _, to := range statuses {
			want := allowed[from][to]
			if got := CanTransitionSession(from, to); got != want {
				t.Errorf("CanTransitionSession(%s, %s) = %v; want %v", from, to, got, want)
			}
		}
	}
	for _, unknown := range []string{"", "running", "WAITING", "IDLE"} {
		if CanTransitionSession(unknown, StatusSessionRunning) {
			t.Errorf("CanTransitionSession(%q, RUNNING) is true for an unknown status", unknown)
		}
	}
}

// TestSessionMayFailWithoutRunning is the one edge a naive lifecycle omits.
//
// A runtime that never came up is a real attempt that did not happen, and a
// session that could only fail after running could not record it.
func TestSessionMayFailWithoutRunning(t *testing.T) {
	if !CanTransitionSession(StatusSessionCreated, StatusSessionFailed) {
		t.Error("a session cannot fail before it runs; a runtime that never came up has nowhere to be recorded")
	}
	if !CanTransitionSession(StatusSessionCreated, StatusSessionCancelled) {
		t.Error("a session cannot be cancelled before it runs")
	}
}

func TestAllowedSessionTransitionsIsACopy(t *testing.T) {
	first := AllowedSessionTransitions(StatusSessionCreated)
	if len(first) == 0 {
		t.Fatal("AllowedSessionTransitions(CREATED) is empty; the fixture is wrong")
	}
	first[0] = "TAMPERED"
	if AllowedSessionTransitions(StatusSessionCreated)[0] == "TAMPERED" {
		t.Fatal("AllowedSessionTransitions returned the map's own slice; a caller can edit the lifecycle")
	}
}

func TestAgentSessionHasRuntime(t *testing.T) {
	if (&AgentSession{Status: StatusSessionCreated}).HasRuntime() {
		t.Error("a session with no runtime reports one")
	}
	if !(&AgentSession{RuntimeID: "amx-p_0123"}).HasRuntime() {
		t.Error("a session with a runtime reports none")
	}
	var nilSession *AgentSession
	if nilSession.HasRuntime() {
		t.Error("a nil session reports a runtime")
	}
}

func TestAgentSessionIsTerminal(t *testing.T) {
	if (&AgentSession{Status: StatusSessionRunning}).IsTerminal() {
		t.Error("a RUNNING session is terminal")
	}
	if !(&AgentSession{Status: StatusSessionCancelled}).IsTerminal() {
		t.Error("a CANCELLED session is not terminal")
	}
}

func TestAgentSessionCloneIsDeep(t *testing.T) {
	started := time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC)
	original := &AgentSession{
		ID:        "sess_0123456789abcdef01234567",
		TaskID:    "task_0123456789abcdef01234567",
		Status:    StatusSessionRunning,
		StartedAt: &started,
		CreatedAt: started,
	}
	clone := original.Clone()
	*clone.StartedAt = clone.StartedAt.Add(time.Hour)
	if *original.StartedAt == *clone.StartedAt {
		t.Fatal("Clone shares the StartedAt pointer")
	}
	clone.Status = StatusSessionCompleted
	if original.Status != StatusSessionRunning {
		t.Fatal("Clone shares the struct")
	}
	var nilSession *AgentSession
	if nilSession.Clone() != nil {
		t.Error("cloning a nil session produced a value")
	}
}

// TestAgentSessionStringOmitsNothingItShouldNot is the mirror of the task's
// test: a session carries no user text at all, so the only thing to check is
// that it renders both with and without a runtime.
func TestAgentSessionStringOmitsNothingItShouldNot(t *testing.T) {
	with := (&AgentSession{
		ID: "sess_0123456789abcdef01234567", TaskID: "task_0123",
		Status: StatusSessionRunning, RuntimeID: "amx-p_0123",
	}).String()
	for _, want := range []string{"sess_", "task_0123", "amx-p_0123", StatusSessionRunning} {
		if !strings.Contains(with, want) {
			t.Errorf("String() = %q, which does not name %q", with, want)
		}
	}
	without := (&AgentSession{
		ID: "sess_0123456789abcdef01234567", TaskID: "task_0123",
		Status: StatusSessionCreated,
	}).String()
	if strings.Contains(without, "runtime=") {
		t.Errorf("String() = %q, which claims a runtime it does not have", without)
	}
	if got := (*AgentSession)(nil).String(); got != "session(nil)" {
		t.Errorf("a nil session renders as %q", got)
	}
}

// TestAgentSessionJSONShape pins the wire names and the one omitempty.
//
// runtimeId is omitted rather than null when a session has no runtime, because
// the field is the one place the two-step creation is visible: a client that
// sees no runtimeId knows the attempt has not been given one yet, and a null
// would make that indistinguishable from a server that does not report it.
func TestAgentSessionJSONShape(t *testing.T) {
	encoded, err := json.Marshal(&AgentSession{
		ID:        "sess_0123456789abcdef01234567",
		TaskID:    "task_0123456789abcdef01234567",
		RuntimeID: "amx-p_0123456789abcdef0123",
		Status:    StatusSessionRunning,
		CreatedAt: time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var withRuntime map[string]any
	if err := json.Unmarshal(encoded, &withRuntime); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"id", "taskId", "runtimeId", "status", "startedAt", "endedAt", "createdAt"} {
		if _, ok := withRuntime[field]; !ok {
			t.Errorf("%s is missing from %s", field, encoded)
		}
	}

	encoded, err = json.Marshal(&AgentSession{
		ID:        "sess_0123456789abcdef01234567",
		TaskID:    "task_0123456789abcdef01234567",
		Status:    StatusSessionCreated,
		CreatedAt: time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var noRuntime map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &noRuntime); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := noRuntime["runtimeId"]; ok {
		t.Errorf("runtimeId is present in %s for a session with no runtime", encoded)
	}
	// startedAt and endedAt are present and null: a session that never ran has
	// no start time, which is different from a response that does not say.
	for _, field := range []string{"startedAt", "endedAt"} {
		raw, ok := noRuntime[field]
		if !ok {
			t.Fatalf("%s is absent from %s; it must be present and null", field, encoded)
		}
		if string(raw) != "null" {
			t.Errorf("%s = %s; want null", field, raw)
		}
	}
}
