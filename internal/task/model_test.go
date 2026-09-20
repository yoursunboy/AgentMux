package task

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/project"
)

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

func TestNewIDHasTheDeclaredShape(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	if !strings.HasPrefix(id, IDPrefix) {
		t.Errorf("NewID returned %q, which does not begin with %q", id, IDPrefix)
	}
	if want := len(IDPrefix) + taskID.BodyLen(); len(id) != want {
		t.Errorf("NewID returned %q, which is %d characters; want %d", id, len(id), want)
	}
	if !ValidID(id) {
		t.Errorf("ValidID(%q) is false for an id NewID produced", id)
	}
}

// TestIDsAreNotInterchangeable is the test that makes the prefixes worth having.
//
// Every one of these is a valid identifier somewhere in AgentMux, and none of
// them is a task. A ValidID that accepted a project id would let a task lookup
// be answered by a project row the day a repository keyed on a string, and the
// prefix is the only thing that makes that impossible rather than unlikely.
func TestIDsAreNotInterchangeable(t *testing.T) {
	projectID, err := project.NewID()
	if err != nil {
		t.Fatalf("project.NewID: %v", err)
	}
	eventID, err := event.NewID()
	if err != nil {
		t.Fatalf("event.NewID: %v", err)
	}
	sessionID, err := NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	taskIDValue, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}

	cases := []struct {
		name string
		id   string
		want bool
	}{
		{"a task id", taskIDValue, true},
		{"a project id", projectID, false},
		{"an event id", eventID, false},
		{"a session id", sessionID, false},
		{"the prefix alone", IDPrefix, false},
		{"an empty string", "", false},
		{"uppercase hex", IDPrefix + strings.ToUpper(strings.Repeat("a", taskID.BodyLen())), false},
		{"a non-hex body", IDPrefix + strings.Repeat("z", taskID.BodyLen()), false},
		{"a body one byte too short", IDPrefix + strings.Repeat("a", taskID.BodyLen()-1), false},
		{"a body one byte too long", IDPrefix + strings.Repeat("a", taskID.BodyLen()+1), false},
		{"a prefix with a trailing space", IDPrefix + strings.Repeat("a", taskID.BodyLen()) + " ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidID(tc.id); got != tc.want {
				t.Errorf("ValidID(%q) = %v; want %v", tc.id, got, tc.want)
			}
		})
	}
}

func TestNewSessionIDHasTheDeclaredShape(t *testing.T) {
	id, err := NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	if !strings.HasPrefix(id, SessionIDPrefix) {
		t.Errorf("NewSessionID returned %q, which does not begin with %q", id, SessionIDPrefix)
	}
	if !ValidSessionID(id) {
		t.Errorf("ValidSessionID(%q) is false for an id NewSessionID produced", id)
	}

	// The two prefixes differ, so neither validator may accept the other's ids.
	taskIDValue, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	if ValidSessionID(taskIDValue) {
		t.Errorf("ValidSessionID(%q) is true for a task id", taskIDValue)
	}
	if ValidID(id) {
		t.Errorf("ValidID(%q) is true for a session id", id)
	}
}

// ---------------------------------------------------------------------------
// Titles
// ---------------------------------------------------------------------------

func TestValidateTitleAccepts(t *testing.T) {
	cases := []struct {
		name  string
		title string
	}{
		{"a short sentence", "Fix the websocket reconnect bug"},
		{"one character", "x"},
		{"punctuation that would be markup elsewhere", `<script>alert("x")</script>`},
		{"a shell metacharacter", `$(rm -rf /) && echo "hi" | tee`},
		{"a path that must not be followed", `../../etc/passwd`},
		{"an emoji", "Ship it 🚀"},
		{"a CJK title", "实现控制器查看器"},
		{"exactly the maximum", strings.Repeat("a", MaxTitleLength)},
		{"exactly the maximum in CJK", strings.Repeat("字", MaxTitleLength)},
		{"inner whitespace", "Fix the   bug in the   viewer"},
		{"a colon and a slash", "viewer: add /sessions/:id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateTitle(tc.title); err != nil {
				t.Errorf("ValidateTitle(%q) = %v; want nil", tc.title, err)
			}
		})
	}
}

func TestValidateTitleRejects(t *testing.T) {
	cases := []struct {
		name  string
		title string
	}{
		{"empty", ""},
		{"a single space", " "},
		{"leading whitespace", " Fix the bug"},
		{"trailing whitespace", "Fix the bug "},
		{"leading newline", "\nFix the bug"},
		{"an embedded newline", "Fix the\nbug"},
		{"an embedded tab", "Fix the\tbug"},
		{"an embedded NUL", "Fix the\x00bug"},
		{"an escape character", "Fix the \x1bbug"},
		{"one character too many", strings.Repeat("a", MaxTitleLength+1)},
		// The bound is in runes, so a title that is under the byte bound but
		// over the character bound is still refused, and so is one that is
		// under the character bound and far over the byte bound being accepted.
		{"one CJK character too many", strings.Repeat("字", MaxTitleLength+1)},
		{"invalid UTF-8", "Fix the \xff\xfe bug"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTitle(tc.title)
			if err == nil {
				t.Fatalf("ValidateTitle(%q) = nil; want a refusal", tc.title)
			}
			if !IsCode(err, CodeInvalidTitle) {
				t.Errorf("ValidateTitle(%q) carried code %q; want %q", tc.title, CodeOf(err), CodeInvalidTitle)
			}
			// The refusal must point at the field, because a client that sent a
			// task and a title needs to know which one was wrong when there is
			// more than one field in a request.
			var taskErr *Error
			if !errors.As(err, &taskErr) {
				t.Fatalf("ValidateTitle(%q) returned %T, which is not a task error", tc.title, err)
			}
			if taskErr.Details["field"] != "title" {
				t.Errorf("ValidateTitle(%q) details = %v; want field=title", tc.title, taskErr.Details)
			}
		})
	}
}

// TestTitleBoundIsInRunesNotBytes pins the reason the bound is what it is.
//
// A 200-byte bound would refuse a 67-character Chinese title while accepting a
// 200-character English one, which is the same limit meaning two different
// things depending on the script.
func TestTitleBoundIsInRunesNotBytes(t *testing.T) {
	title := strings.Repeat("字", MaxTitleLength)
	if len(title) <= MaxTitleLength {
		t.Fatalf("the fixture is %d bytes, which does not exercise the distinction", len(title))
	}
	if err := ValidateTitle(title); err != nil {
		t.Errorf("ValidateTitle(%d CJK characters, %d bytes) = %v; want nil",
			MaxTitleLength, len(title), err)
	}
}

// ---------------------------------------------------------------------------
// Task statuses
// ---------------------------------------------------------------------------

func TestTaskStatusesAreExactlyTheDeclaredSet(t *testing.T) {
	want := []string{"CREATED", "RUNNING", "WAITING", "COMPLETED", "FAILED", "CANCELLED"}
	got := TaskStatuses()
	if len(got) != len(want) {
		t.Fatalf("TaskStatuses() = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("TaskStatuses()[%d] = %q; want %q", i, got[i], want[i])
		}
	}
	// Every listed status must validate, and validation must agree with the
	// list rather than being a second copy of it that can drift.
	for _, s := range got {
		if !ValidTaskStatus(s) {
			t.Errorf("ValidTaskStatus(%q) is false for a listed status", s)
		}
	}
	for _, s := range []string{"", "created", "Running", "DONE", "PENDING", "WAITING ", " RUNNING"} {
		if ValidTaskStatus(s) {
			t.Errorf("ValidTaskStatus(%q) is true; want false", s)
		}
	}
}

func TestTaskTerminal(t *testing.T) {
	cases := map[string]bool{
		StatusCreated:   false,
		StatusRunning:   false,
		StatusWaiting:   false,
		StatusCompleted: true,
		StatusFailed:    true,
		StatusCancelled: true,
	}
	for status, want := range cases {
		if got := TaskTerminal(status); got != want {
			t.Errorf("TaskTerminal(%q) = %v; want %v", status, got, want)
		}
	}
}

// TestTaskTransitionsAreExactlyTheLifecycle pins the edges.
//
// It is written as a complete table rather than as a handful of spot checks,
// because the interesting bug in a lifecycle is not a legal edge being refused -
// it is an edge nobody meant to allow.
func TestTaskTransitionsAreExactlyTheLifecycle(t *testing.T) {
	statuses := TaskStatuses()
	allowed := map[string]map[string]bool{
		StatusCreated: {
			StatusRunning: true, StatusCancelled: true,
		},
		StatusRunning: {
			StatusWaiting: true, StatusCompleted: true,
			StatusFailed: true, StatusCancelled: true,
		},
		StatusWaiting: {
			StatusRunning: true, StatusCompleted: true,
			StatusFailed: true, StatusCancelled: true,
		},
		// The three terminal statuses have no outgoing edges at all, which is
		// the row that refuses COMPLETED -> RUNNING.
		StatusCompleted: {},
		StatusFailed:    {},
		StatusCancelled: {},
	}

	for _, from := range statuses {
		for _, to := range statuses {
			want := allowed[from][to]
			if got := CanTransitionTask(from, to); got != want {
				t.Errorf("CanTransitionTask(%s, %s) = %v; want %v", from, to, got, want)
			}
		}
	}

	// An unknown status has no edges, in either direction. A map read of a
	// missing key returns the zero value, so this is the case where a lifecycle
	// written as a table of forbidden edges would quietly allow everything.
	for _, unknown := range []string{"", "created", "DONE", "REOPENED"} {
		if CanTransitionTask(unknown, StatusRunning) {
			t.Errorf("CanTransitionTask(%q, RUNNING) is true for an unknown status", unknown)
		}
		if CanTransitionTask(StatusRunning, unknown) {
			t.Errorf("CanTransitionTask(RUNNING, %q) is true for an unknown status", unknown)
		}
	}
}

// TestCompletedIsTerminal is the one edge the brief names by hand, asserted on
// its own so that a future edit to the transition map fails a test whose name
// says what it broke.
func TestCompletedIsTerminal(t *testing.T) {
	for _, to := range TaskStatuses() {
		if CanTransitionTask(StatusCompleted, to) {
			t.Errorf("CanTransitionTask(COMPLETED, %s) is true; completed work must not move", to)
		}
	}
	if got := AllowedTaskTransitions(StatusCompleted); len(got) != 0 {
		t.Errorf("AllowedTaskTransitions(COMPLETED) = %v; want empty", got)
	}
}

func TestAllowedTaskTransitionsIsACopy(t *testing.T) {
	first := AllowedTaskTransitions(StatusRunning)
	if len(first) == 0 {
		t.Fatal("AllowedTaskTransitions(RUNNING) is empty; the fixture is wrong")
	}
	first[0] = "TAMPERED"

	second := AllowedTaskTransitions(StatusRunning)
	if second[0] == "TAMPERED" {
		t.Fatal("AllowedTaskTransitions returned the map's own slice; a caller can edit the lifecycle")
	}
}

// ---------------------------------------------------------------------------
// The Task value
// ---------------------------------------------------------------------------

func TestTaskIsTerminal(t *testing.T) {
	if (&Task{Status: StatusRunning}).IsTerminal() {
		t.Error("a RUNNING task is terminal")
	}
	if !(&Task{Status: StatusCompleted}).IsTerminal() {
		t.Error("a COMPLETED task is not terminal")
	}
	// A nil task is not terminal rather than a panic, because this is called on
	// values that came out of a lookup which can return nil.
	var nilTask *Task
	if nilTask.IsTerminal() {
		t.Error("a nil task is terminal")
	}
}

func TestTaskCloneIsDeep(t *testing.T) {
	completed := time.Date(2026, time.September, 20, 9, 30, 0, 0, time.UTC)
	original := &Task{
		ID:          "task_0123456789abcdef01234567",
		ProjectID:   "p_0123456789abcdef0123",
		Title:       "Fix the viewer",
		Status:      StatusCompleted,
		CreatedAt:   time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC),
		UpdatedAt:   completed,
		CompletedAt: &completed,
	}
	clone := original.Clone()

	if *clone.CompletedAt = clone.CompletedAt.Add(time.Hour); *original.CompletedAt == *clone.CompletedAt {
		t.Fatal("Clone shares the CompletedAt pointer; a caller can edit stored state through it")
	}
	clone.Status = StatusFailed
	if original.Status != StatusCompleted {
		t.Fatal("Clone shares the struct; editing the copy changed the original")
	}
}

// TestTaskStringOmitsTheTitle is a small test of a rule with a large blast
// radius: a title is the one field a person wrote, and a log is the easiest
// place for it to end up somewhere it should not be.
func TestTaskStringOmitsTheTitle(t *testing.T) {
	tk := &Task{
		ID:        "task_0123456789abcdef01234567",
		ProjectID: "p_0123456789abcdef0123",
		Title:     "the secret plan",
		Status:    StatusRunning,
	}
	got := tk.String()
	if strings.Contains(got, "secret") {
		t.Errorf("String() = %q, which contains the title", got)
	}
	for _, want := range []string{tk.ID, tk.ProjectID, tk.Status} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, which does not name %q", got, want)
		}
	}
	if got := (*Task)(nil).String(); got != "task(nil)" {
		t.Errorf("a nil task renders as %q", got)
	}
}

// TestTaskJSONShape pins the wire names, because a client that decodes
// `project_id` and finds nothing has no way to tell that from a task with no
// project.
func TestTaskJSONShape(t *testing.T) {
	completed := time.Date(2026, time.September, 20, 9, 30, 0, 0, time.UTC)
	encoded, err := json.Marshal(&Task{
		ID:          "task_0123456789abcdef01234567",
		ProjectID:   "p_0123456789abcdef0123",
		Title:       "Fix the viewer",
		Status:      StatusCompleted,
		CreatedAt:   time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC),
		UpdatedAt:   completed,
		CompletedAt: &completed,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"id", "projectId", "title", "status", "createdAt", "updatedAt", "completedAt"} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("%s is missing from %s", field, encoded)
		}
	}
	if len(decoded) != 7 {
		t.Errorf("a task encodes %d fields (%s); the model has 7 and a new one is a decision", len(decoded), encoded)
	}
}

// TestTaskJSONKeepsCompletedAtPresentWhenNil pins the difference between a task
// that has no completion time and a response that does not mention one.
//
// It is not omitempty on purpose: a client rendering "completed at" needs to be
// able to tell "not completed" from "this server does not report that".
func TestTaskJSONKeepsCompletedAtPresentWhenNil(t *testing.T) {
	encoded, err := json.Marshal(&Task{ID: "task_0123456789abcdef01234567", Status: StatusRunning})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw, ok := decoded["completedAt"]
	if !ok {
		t.Fatalf("completedAt is absent from %s; it must be present and null", encoded)
	}
	if string(raw) != "null" {
		t.Errorf("completedAt = %s; want null", raw)
	}
}
