package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/project"
)

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

// memoryRepo is a task repository that keeps rows in maps.
//
// It implements the conditional updates the real store implements with
// `WHERE id = ? AND status = ?`, because those are the contract the service is
// written against and a double that applied every write unconditionally would
// make the conflict paths unreachable in a test.
type memoryRepo struct {
	mu       sync.Mutex
	tasks    map[string]*Task
	sessions map[string]*AgentSession

	// fail, when set, is returned by every method. It is a plain error and not a
	// coded one, which is what a driver failure looks like.
	fail error

	// onUpdateTaskStatus runs at the top of UpdateTaskStatus, with the lock
	// already held, before the conditional check. It is how a test drives the
	// race the conditional update exists to survive: the hook writes to the row
	// in the window between the service's read and its write.
	//
	// It must not call back into the repository - the lock is held.
	onUpdateTaskStatus func(r *memoryRepo)

	// writes counts the status updates that actually reached a row, as opposed
	// to the ones refused because the row had moved. A concurrency test asserts
	// on it, because the service counts a no-op as a success and the difference
	// between that and a write is the whole question.
	writes int
}

// taskStatusWrites reports how many status changes have been applied.
func (r *memoryRepo) taskStatusWrites() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writes
}

func newMemoryRepo() *memoryRepo {
	return &memoryRepo{
		tasks:    map[string]*Task{},
		sessions: map[string]*AgentSession{},
	}
}

func (r *memoryRepo) CreateTask(_ context.Context, t *Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.tasks[t.ID] = t.Clone()
	return nil
}

func (r *memoryRepo) GetTask(_ context.Context, id string) (*Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return nil, r.fail
	}
	stored, ok := r.tasks[id]
	if !ok {
		return nil, ErrNotFound
	}
	return stored.Clone(), nil
}

func (r *memoryRepo) ListTasks(_ context.Context, query TaskQuery) ([]*Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return nil, r.fail
	}
	out := make([]*Task, 0, len(r.tasks))
	for _, stored := range r.tasks {
		if stored.ProjectID != query.ProjectID {
			continue
		}
		if query.Status != "" && stored.Status != query.Status {
			continue
		}
		out = append(out, stored.Clone())
	}
	// The same ordering the store's SQL produces: newest first, with the id as
	// the tie-break, because two rows written in the same instant are ordinary
	// and an ordering that is not total is not an ordering.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > query.LimitOr() {
		out = out[:query.LimitOr()]
	}
	return out, nil
}

func (r *memoryRepo) UpdateTaskStatus(_ context.Context, id string, change TaskStatusChange) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	if r.onUpdateTaskStatus != nil {
		r.onUpdateTaskStatus(r)
	}
	stored, ok := r.tasks[id]
	if !ok {
		return ErrNotFound
	}
	if stored.Status != change.From {
		return ErrStatusConflict
	}
	r.writes++
	stored.Status = change.To
	stored.UpdatedAt = change.At
	if change.CompletedAt != nil {
		at := *change.CompletedAt
		stored.CompletedAt = &at
	} else {
		stored.CompletedAt = nil
	}
	return nil
}

func (r *memoryRepo) UpdateTaskTitle(_ context.Context, id, title string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	stored, ok := r.tasks[id]
	if !ok {
		return ErrNotFound
	}
	stored.Title = title
	stored.UpdatedAt = at
	return nil
}

func (r *memoryRepo) CreateSession(_ context.Context, s *AgentSession) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.sessions[s.ID] = s.Clone()
	return nil
}

func (r *memoryRepo) GetSession(_ context.Context, id string) (*AgentSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return nil, r.fail
	}
	stored, ok := r.sessions[id]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return stored.Clone(), nil
}

func (r *memoryRepo) ListSessions(_ context.Context, query SessionQuery) ([]*AgentSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return nil, r.fail
	}
	out := make([]*AgentSession, 0, len(r.sessions))
	for _, stored := range r.sessions {
		if stored.TaskID != query.TaskID {
			continue
		}
		out = append(out, stored.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > query.LimitOr() {
		out = out[:query.LimitOr()]
	}
	return out, nil
}

func (r *memoryRepo) UpdateSessionStatus(_ context.Context, id string, change SessionStatusChange) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	stored, ok := r.sessions[id]
	if !ok {
		return ErrSessionNotFound
	}
	if stored.Status != change.From {
		return ErrStatusConflict
	}
	stored.Status = change.To
	if change.StartedAt != nil {
		at := *change.StartedAt
		stored.StartedAt = &at
	}
	// Assigned rather than coalesced, matching the store: entering RUNNING
	// clears the end time.
	if change.EndedAt != nil {
		at := *change.EndedAt
		stored.EndedAt = &at
	} else {
		stored.EndedAt = nil
	}
	return nil
}

func (r *memoryRepo) AttachSessionRuntime(_ context.Context, id, runtimeID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	stored, ok := r.sessions[id]
	if !ok {
		return ErrSessionNotFound
	}
	if stored.RuntimeID != "" {
		return ErrStatusConflict
	}
	stored.RuntimeID = runtimeID
	return nil
}

// existingProjects answers whether a project exists, from a fixed set.
//
// A task must belong to a project that exists, and the set is a whole map
// rather than a lookup that always says yes, because "the project does not
// exist" is one of the refusals this phase has to get right.
type existingProjects map[string]bool

func (p existingProjects) Get(_ context.Context, id string) (*project.Project, error) {
	if !p[id] {
		return nil, &project.Error{Code: project.CodeNotFound, Message: "no project"}
	}
	return &project.Project{ID: id}, nil
}

// recordedEvent is one CreateEvent call, kept whole so a test can assert the
// source and the payload and not only the type.
type recordedEvent struct {
	ProjectID string
	RuntimeID string
	Type      string
	Source    string
	Payload   map[string]any
}

// memoryRecorder is the event log, as the task model sees it.
type memoryRecorder struct {
	mu     sync.Mutex
	events []recordedEvent

	// err, when set, is returned by every write. The service swallows it, which
	// is the behaviour one of these tests pins.
	err error
}

func (r *memoryRecorder) CreateEvent(
	_ context.Context,
	projectID, runtimeID, eventType, source string,
	payload json.RawMessage,
) (*event.AgentEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	decoded := map[string]any{}
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &decoded)
	}
	r.events = append(r.events, recordedEvent{
		ProjectID: projectID,
		RuntimeID: runtimeID,
		Type:      eventType,
		Source:    source,
		Payload:   decoded,
	})
	return nil, nil
}

func (r *memoryRecorder) all() []recordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedEvent, len(r.events))
	copy(out, r.events)
	return out
}

func (r *memoryRecorder) types() []string {
	out := []string{}
	for _, ev := range r.all() {
		out = append(out, ev.Type)
	}
	return out
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// Runtime identifiers of the shape this server produces: the session prefix
// followed by a project id.
//
// They are spelled out rather than assembled from a project the harness
// registers, because a task's project and a runtime's project are allowed to
// disagree - the column is not a foreign key - and the tests below depend on
// being able to name a runtime whose project is not in the project set.
const (
	runtimeIDForTest      = "amx-p_0123456789abcdef0123"
	otherRuntimeIDForTest = "amx-p_0123456789abcdef0124"
	ghostRuntimeIDForTest = "amx-p_ffffffffffffffffffff"
)

type harness struct {
	service  *Service
	repo     *memoryRepo
	recorder *memoryRecorder
	projects existingProjects
	now      time.Time
	counter  int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		repo:     newMemoryRepo(),
		recorder: &memoryRecorder{},
		projects: existingProjects{"p_alpha": true, "p_beta": true},
		now:      time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC),
	}
	service, err := NewService(Options{
		Repository: h.repo,
		Projects:   h.projects,
		Events:     h.recorder,
		Now:        func() time.Time { return h.now },
		NewTaskID:  func() (string, error) { return h.nextID(NewID) },
		NewSessionID: func() (string, error) {
			return h.nextID(NewSessionID)
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h.service = service
	return h
}

// nextID produces a deterministic identifier using a real generator.
//
// It calls the production generator rather than assembling an identifier by
// hand, so that a test can never pass with an id the model would reject; the
// counter is then written over the random body, which keeps the ids valid and
// makes them sort in the order they were handed out. A listing test can
// therefore say which row comes first instead of computing it.
func (h *harness) nextID(generate func() (string, error)) (string, error) {
	h.counter++
	id, err := generate()
	if err != nil {
		return "", err
	}
	prefix, bodyLen := IDPrefix, taskID.BodyLen()
	if strings.HasPrefix(id, SessionIDPrefix) {
		prefix, bodyLen = SessionIDPrefix, sessionID.BodyLen()
	}
	return fmt.Sprintf("%s%0*x", prefix, bodyLen, h.counter), nil
}

// taskIDValue returns a deterministic task identifier that names nothing.
func (h *harness) taskIDValue() string {
	id, err := h.nextID(NewID)
	if err != nil {
		panic(err)
	}
	return id
}

// sessionIDValue returns a deterministic session identifier that names nothing.
func (h *harness) sessionIDValue() string {
	id, err := h.nextID(NewSessionID)
	if err != nil {
		panic(err)
	}
	return id
}

// tick advances the clock, so a write after it is strictly newer.
func (h *harness) tick(d time.Duration) { h.now = h.now.Add(d) }

// createTask records a task and fails the test if it cannot.
func (h *harness) createTask(t *testing.T, projectID, title string) *Task {
	t.Helper()
	tk, err := h.service.CreateTask(context.Background(), CreateTaskInput{
		ProjectID: projectID,
		Title:     title,
	})
	if err != nil {
		t.Fatalf("CreateTask(%s, %q): %v", projectID, title, err)
	}
	return tk
}

// createSession records an attempt and fails the test if it cannot.
func (h *harness) createSession(t *testing.T, taskID string) *AgentSession {
	t.Helper()
	sess, err := h.service.CreateSession(context.Background(), CreateSessionInput{TaskID: taskID})
	if err != nil {
		t.Fatalf("CreateSession(%s): %v", taskID, err)
	}
	return sess
}

// advance moves a task to a status and fails the test if it is refused.
func (h *harness) advance(t *testing.T, id, to string) *Task {
	t.Helper()
	tk, err := h.service.UpdateTaskStatus(context.Background(), id, to)
	if err != nil {
		t.Fatalf("UpdateTaskStatus(%s, %s): %v", id, to, err)
	}
	return tk
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestNewServiceRequiresItsCollaborators(t *testing.T) {
	if _, err := NewService(Options{Projects: existingProjects{}}); err == nil {
		t.Error("NewService accepted a service with no repository")
	}
	if _, err := NewService(Options{Repository: newMemoryRepo()}); err == nil {
		t.Error("NewService accepted a service with no project lookup")
	}
}

// TestANilRecorderIsALegitimateService pins the §三十七 rule at the level of
// construction: a service with no event log manages tasks exactly as well.
func TestANilRecorderIsALegitimateService(t *testing.T) {
	repo := newMemoryRepo()
	service, err := NewService(Options{
		Repository: repo,
		Projects:   existingProjects{"p_alpha": true},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	ctx := context.Background()
	tk, err := service.CreateTask(ctx, CreateTaskInput{ProjectID: "p_alpha", Title: "Fix the viewer"})
	if err != nil {
		t.Fatalf("CreateTask with no recorder: %v", err)
	}
	if _, err := service.UpdateTaskStatus(ctx, tk.ID, StatusRunning); err != nil {
		t.Fatalf("UpdateTaskStatus with no recorder: %v", err)
	}
	got, err := service.GetTask(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %q; want %q", got.Status, StatusRunning)
	}
}

// TestTheRecorderCannotReadEvents is the §三十七 rule enforced structurally.
//
// The rule is that a task's status is never derived from its history. The
// strongest form of that is not a convention but an interface with no read on
// it: a service that could list events is a service that could decide a status
// from them, and the next person to want a "recompute" would find the method
// already there.
func TestTheRecorderCannotReadEvents(t *testing.T) {
	typ := reflect.TypeOf((*EventRecorder)(nil)).Elem()
	if typ.NumMethod() != 1 {
		t.Fatalf("EventRecorder has %d methods; the task model must only ever write to the log", typ.NumMethod())
	}
	if name := typ.Method(0).Name; name != "CreateEvent" {
		t.Errorf("EventRecorder's only method is %q; want CreateEvent", name)
	}
}

// ---------------------------------------------------------------------------
// Creating tasks
// ---------------------------------------------------------------------------

func TestCreateTaskStoresTheTask(t *testing.T) {
	h := newHarness(t)
	const title = "Implement the controller viewer"
	tk := h.createTask(t, "p_alpha", title)

	if !ValidID(tk.ID) {
		t.Errorf("id %q is not a task id", tk.ID)
	}
	if tk.ProjectID != "p_alpha" {
		t.Errorf("projectId = %q; want p_alpha", tk.ProjectID)
	}
	// Stored and returned exactly as it was given: a title is what the user
	// asked for, and quietly storing a rewritten one means showing them
	// something they did not write.
	if tk.Title != title {
		t.Errorf("title = %q; want %q", tk.Title, title)
	}
	if tk.Status != StatusCreated {
		t.Errorf("status = %q; want %q: a task begins before anything starts it", tk.Status, StatusCreated)
	}
	if tk.CompletedAt != nil {
		t.Errorf("completedAt = %v; want nil", tk.CompletedAt)
	}
	if !tk.CreatedAt.Equal(h.now) || !tk.UpdatedAt.Equal(h.now) {
		t.Errorf("createdAt = %v, updatedAt = %v; want both %v", tk.CreatedAt, tk.UpdatedAt, h.now)
	}
	if loc := tk.CreatedAt.Location(); loc != time.UTC {
		t.Errorf("createdAt is in %v; times are stored in UTC", loc)
	}

	// It is stored, not merely returned: reading it back through a second call
	// is what proves the row exists rather than the value.
	stored, err := h.service.GetTask(context.Background(), tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.ID != tk.ID || stored.Status != StatusCreated {
		t.Errorf("the stored task is %s; want %s", stored, tk)
	}
}

// TestCreateTaskStartsNothing is the phase's central restraint, asserted.
//
// Creating a task records that somebody wants work done. It launches no
// process, and the event it writes names no runtime, because there is none.
func TestCreateTaskStartsNothing(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Implement the controller viewer")

	sessions, err := h.service.ListSessions(context.Background(), ListSessionsInput{TaskID: tk.ID})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Errorf("a new task has %d attempts; creating one starts nothing", len(sessions))
	}

	events := h.recorder.all()
	if len(events) != 1 {
		t.Fatalf("creating a task recorded %v; want exactly one event", h.recorder.types())
	}
	if events[0].RuntimeID != "" {
		t.Errorf("the task event names runtime %q; nothing has been started", events[0].RuntimeID)
	}
}

// TestCreateTaskNeverRecordsTheTitle is the rule about user text.
//
// A title is the one field in this model a person wrote. An event row can never
// be edited, so a row that can never be edited can never be redacted either.
func TestCreateTaskNeverRecordsTheTitle(t *testing.T) {
	h := newHarness(t)
	const title = "the launch codes are hunter2"
	tk := h.createTask(t, "p_alpha", title)

	for _, ev := range h.recorder.all() {
		encoded, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(encoded), "hunter2") {
			t.Errorf("event %s carries the task title", encoded)
		}
		if ev.Payload["taskId"] != tk.ID {
			t.Errorf("the event payload names %v; want taskId %s", ev.Payload, tk.ID)
		}
	}
}

func TestCreateTaskRecordsOneEvent(t *testing.T) {
	h := newHarness(t)
	h.createTask(t, "p_alpha", "Fix the viewer")

	events := h.recorder.all()
	if len(events) != 1 {
		t.Fatalf("events = %v; want one", h.recorder.types())
	}
	got := events[0]
	if got.Type != TypeTaskCreated {
		t.Errorf("type = %q; want %q", got.Type, TypeTaskCreated)
	}
	if got.ProjectID != "p_alpha" {
		t.Errorf("projectId = %q; want p_alpha", got.ProjectID)
	}
	if got.Source != event.SourceUser {
		t.Errorf("source = %q; want %q, which is a source the event model already allows",
			got.Source, event.SourceUser)
	}
}

func TestCreateTaskRequiresAnExistingProject(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.CreateTask(context.Background(), CreateTaskInput{
		ProjectID: "p_nowhere",
		Title:     "Fix the viewer",
	})
	if !IsCode(err, CodeProjectNotFound) {
		t.Fatalf("CreateTask under an unknown project returned %v; want %q", err, CodeProjectNotFound)
	}
	if len(h.repo.tasks) != 0 {
		t.Error("a task was stored under a project that does not exist")
	}
	if len(h.recorder.all()) != 0 {
		t.Error("a refused creation recorded an event")
	}
}

func TestCreateTaskRejectsABadTitle(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.CreateTask(context.Background(), CreateTaskInput{
		ProjectID: "p_alpha",
		Title:     "",
	})
	if !IsCode(err, CodeInvalidTitle) {
		t.Fatalf("CreateTask with an empty title returned %v; want %q", err, CodeInvalidTitle)
	}
	if len(h.repo.tasks) != 0 {
		t.Error("a task with an empty title was stored")
	}
}

func TestCreateTaskReportsAGeneratorFailure(t *testing.T) {
	repo := newMemoryRepo()
	service, err := NewService(Options{
		Repository: repo,
		Projects:   existingProjects{"p_alpha": true},
		NewTaskID:  func() (string, error) { return "", errors.New("no entropy") },
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = service.CreateTask(context.Background(), CreateTaskInput{ProjectID: "p_alpha", Title: "x"})
	if !IsCode(err, CodeStorageFailure) {
		t.Fatalf("a failing generator returned %v; want %q", err, CodeStorageFailure)
	}
}

// TestACodedRepositoryErrorIsPassedThrough checks the contract the HTTP layer
// depends on: every error leaving this package carries a code.
func TestACodedRepositoryErrorIsPassedThrough(t *testing.T) {
	h := newHarness(t)
	h.repo.fail = errors.New("the disk is on fire")
	_, err := h.service.GetTask(context.Background(), h.taskIDValue())
	if !IsCode(err, CodeStorageFailure) {
		t.Fatalf("a driver failure surfaced as %v; want %q", err, CodeStorageFailure)
	}
}

// ---------------------------------------------------------------------------
// Reading tasks
// ---------------------------------------------------------------------------

// TestAnIdentifierOutsideTheNamespaceIsNotFound is the case a caller meets when
// it confuses a project id with a task id.
//
// It is a 404 and not a 400 on purpose: the caller looked something up by an id
// it was given, and the honest answer is that no such task is here.
func TestAnIdentifierOutsideTheNamespaceIsNotFound(t *testing.T) {
	h := newHarness(t)
	for _, id := range []string{"p_0123456789abcdef0123", "sess_0123456789abcdef0123456", "", "task_short"} {
		_, err := h.service.GetTask(context.Background(), id)
		if !IsCode(err, CodeTaskNotFound) {
			t.Errorf("GetTask(%q) returned %v; want %q", id, err, CodeTaskNotFound)
		}
		_, err = h.service.GetSession(context.Background(), id)
		if !IsCode(err, CodeSessionNotFound) {
			t.Errorf("GetSession(%q) returned %v; want %q", id, err, CodeSessionNotFound)
		}
	}
}

func TestListTasksIsScopedToTheProject(t *testing.T) {
	h := newHarness(t)
	alpha := h.createTask(t, "p_alpha", "Alpha work")
	h.createTask(t, "p_beta", "Beta work")

	tasks, err := h.service.ListTasks(context.Background(), ListTasksInput{ProjectID: "p_alpha"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != alpha.ID {
		t.Fatalf("ListTasks(p_alpha) = %d tasks; want only %s", len(tasks), alpha.ID)
	}
}

func TestListTasksRequiresAnExistingProject(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.ListTasks(context.Background(), ListTasksInput{ProjectID: "p_nowhere"})
	if !IsCode(err, CodeProjectNotFound) {
		t.Fatalf("ListTasks under an unknown project returned %v; want %q", err, CodeProjectNotFound)
	}

	// An empty project identifier is a caller's mistake rather than a missing
	// project, and it is refused before the lookup rather than after it.
	_, err = h.service.ListTasks(context.Background(), ListTasksInput{ProjectID: "  "})
	if !IsCode(err, CodeInvalidInput) {
		t.Fatalf("ListTasks with no project returned %v; want %q", err, CodeInvalidInput)
	}
}

func TestListTasksFiltersByStatus(t *testing.T) {
	h := newHarness(t)
	first := h.createTask(t, "p_alpha", "One")
	h.tick(time.Second)
	second := h.createTask(t, "p_alpha", "Two")
	h.tick(time.Second)
	third := h.createTask(t, "p_alpha", "Three")

	h.advance(t, first.ID, StatusRunning)
	h.advance(t, second.ID, StatusRunning)
	h.advance(t, second.ID, StatusCompleted)

	// The listing is newest first, which is the contract the store's SQL
	// provides; this asserts the service passes it through unshuffled.
	all, err := h.service.ListTasks(context.Background(), ListTasksInput{ProjectID: "p_alpha"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListTasks returned %d tasks; want 3", len(all))
	}
	if all[0].ID != third.ID || all[2].ID != first.ID {
		t.Errorf("ListTasks is not newest first: got %s, %s, %s", all[0].ID, all[1].ID, all[2].ID)
	}

	running, err := h.service.ListTasks(context.Background(),
		ListTasksInput{ProjectID: "p_alpha", Status: StatusRunning})
	if err != nil {
		t.Fatalf("ListTasks(status=RUNNING): %v", err)
	}
	if len(running) != 1 || running[0].ID != first.ID {
		t.Errorf("ListTasks(status=RUNNING) = %d tasks; want only %s", len(running), first.ID)
	}
}

func TestListTasksRejectsAnUnknownStatus(t *testing.T) {
	h := newHarness(t)
	for _, status := range []string{"running", "DONE", "PENDING", "WAITING "} {
		_, err := h.service.ListTasks(context.Background(),
			ListTasksInput{ProjectID: "p_alpha", Status: status})
		if !IsCode(err, CodeInvalidInput) {
			t.Errorf("ListTasks(status=%q) returned %v; want %q", status, err, CodeInvalidInput)
		}
	}
}

func TestListTasksReturnsAnEmptyListNotNull(t *testing.T) {
	h := newHarness(t)
	tasks, err := h.service.ListTasks(context.Background(), ListTasksInput{ProjectID: "p_alpha"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if tasks == nil {
		t.Fatal("ListTasks returned nil; an empty project is an empty list")
	}
	if len(tasks) != 0 {
		t.Errorf("ListTasks returned %d tasks; want none", len(tasks))
	}
}

func TestListTasksHonoursAndClampsTheLimit(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		h.createTask(t, "p_alpha", fmt.Sprintf("Work %d", i))
		h.tick(time.Second)
	}

	limited, err := h.service.ListTasks(context.Background(),
		ListTasksInput{ProjectID: "p_alpha", Limit: 2})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("ListTasks(limit=2) returned %d tasks", len(limited))
	}

	// A limit above the ceiling is clamped rather than refused: a caller asking
	// for a thousand wants as much as it can have.
	huge, err := h.service.ListTasks(context.Background(),
		ListTasksInput{ProjectID: "p_alpha", Limit: MaxLimit * 10})
	if err != nil {
		t.Fatalf("ListTasks with an enormous limit: %v", err)
	}
	if len(huge) != 5 {
		t.Errorf("ListTasks with an enormous limit returned %d tasks; want all 5", len(huge))
	}
}

// ---------------------------------------------------------------------------
// Changing a task
// ---------------------------------------------------------------------------

func TestUpdateTaskStatusMovesTheTask(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")

	h.tick(time.Minute)
	running := h.advance(t, tk.ID, StatusRunning)
	if running.Status != StatusRunning {
		t.Fatalf("status = %q; want %q", running.Status, StatusRunning)
	}
	if !running.UpdatedAt.Equal(h.now) {
		t.Errorf("updatedAt = %v; want %v: a status change moves it", running.UpdatedAt, h.now)
	}
	if !running.CreatedAt.Equal(tk.CreatedAt) {
		t.Errorf("createdAt moved from %v to %v", tk.CreatedAt, running.CreatedAt)
	}
	if running.CompletedAt != nil {
		t.Errorf("completedAt = %v; want nil for a running task", running.CompletedAt)
	}

	// And the change is stored, not only returned.
	stored, err := h.service.GetTask(context.Background(), tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != StatusRunning {
		t.Errorf("the stored task is %s; the status was not written", stored)
	}
}

// TestCompletedAtIsSetOnlyByCompletion walks the two paths that differ.
func TestCompletedAtIsSetOnlyByCompletion(t *testing.T) {
	h := newHarness(t)

	done := h.createTask(t, "p_alpha", "Finish this")
	h.advance(t, done.ID, StatusRunning)
	h.tick(time.Minute)
	completed := h.advance(t, done.ID, StatusCompleted)
	if completed.CompletedAt == nil {
		t.Fatal("a completed task has no completion time")
	}
	if !completed.CompletedAt.Equal(h.now) {
		t.Errorf("completedAt = %v; want %v", completed.CompletedAt, h.now)
	}

	failed := h.createTask(t, "p_alpha", "Fail this")
	h.advance(t, failed.ID, StatusRunning)
	h.tick(time.Minute)
	got := h.advance(t, failed.ID, StatusFailed)
	if got.CompletedAt != nil {
		t.Errorf("completedAt = %v for a FAILED task; nothing was completed", got.CompletedAt)
	}

	// The column is cleared rather than left behind. This is the case that
	// matters: a task cannot reach FAILED from COMPLETED, so the only way to
	// observe a stale value is through the write itself, which is what the
	// repository is handed.
	change := TaskStatusChange{From: StatusRunning, To: StatusFailed, At: h.now}
	if change.CompletedAt != nil {
		t.Error("the service built a FAILED change carrying a completion time")
	}
}

func TestUpdateTaskStatusRefusesAnIllegalTransition(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	h.advance(t, tk.ID, StatusRunning)
	h.advance(t, tk.ID, StatusCompleted)

	_, err := h.service.UpdateTaskStatus(context.Background(), tk.ID, StatusRunning)
	if !IsCode(err, CodeInvalidTransition) {
		t.Fatalf("COMPLETED -> RUNNING returned %v; want %q", err, CodeInvalidTransition)
	}

	// The refusal has to say what would have worked, because "invalid
	// transition" alone sends the caller to the documentation for an answer
	// that is a property of the current status.
	var taskErr *Error
	if !errors.As(err, &taskErr) {
		t.Fatalf("the refusal is %T, not a task error", err)
	}
	if taskErr.Details["from"] != StatusCompleted || taskErr.Details["to"] != StatusRunning {
		t.Errorf("details = %v; want the from and to statuses", taskErr.Details)
	}
	if allowed, ok := taskErr.Details["allowed"].([]string); !ok || len(allowed) != 0 {
		t.Errorf("details.allowed = %v; a completed task may move nowhere", taskErr.Details["allowed"])
	}
	if !strings.Contains(taskErr.Message, "COMPLETED") {
		t.Errorf("message = %q; it should name the status the task is actually in", taskErr.Message)
	}

	// Nothing changed.
	stored, err := h.service.GetTask(context.Background(), tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != StatusCompleted {
		t.Errorf("a refused transition wrote %q", stored.Status)
	}
}

func TestUpdateTaskRefusesAnUnknownStatus(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	_, err := h.service.UpdateTaskStatus(context.Background(), tk.ID, "DONE")
	if !IsCode(err, CodeInvalidInput) {
		t.Fatalf("an unknown status returned %v; want %q", err, CodeInvalidInput)
	}
}

// TestAskingForTheCurrentStatusIsANoOp is the retry case.
//
// A client that is not sure a request landed should get the state it asked for,
// not a refusal, and a status change that changes nothing must not write an
// event claiming something happened.
func TestAskingForTheCurrentStatusIsANoOp(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	before := len(h.recorder.all())

	h.tick(time.Minute)
	same, err := h.service.UpdateTaskStatus(context.Background(), tk.ID, StatusCreated)
	if err != nil {
		t.Fatalf("UpdateTaskStatus to the current status: %v", err)
	}
	if same.Status != StatusCreated {
		t.Errorf("status = %q; want %q", same.Status, StatusCreated)
	}
	if !same.UpdatedAt.Equal(tk.UpdatedAt) {
		t.Errorf("updatedAt moved to %v on a no-op", same.UpdatedAt)
	}
	if got := len(h.recorder.all()); got != before {
		t.Errorf("a no-op recorded %d events; want none", got-before)
	}
}

func TestUpdateTaskRetitles(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Old title")

	h.tick(time.Minute)
	title := "New title"
	updated, err := h.service.UpdateTask(context.Background(), tk.ID, UpdateTaskInput{Title: &title})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if updated.Title != title {
		t.Errorf("title = %q; want %q", updated.Title, title)
	}
	if !updated.UpdatedAt.Equal(h.now) {
		t.Errorf("updatedAt = %v; want %v", updated.UpdatedAt, h.now)
	}
	if updated.Status != StatusCreated {
		t.Errorf("status = %q; a retitle is not a transition", updated.Status)
	}
	// A retitle is a change to the task, not to what happened: the events are
	// about the lifecycle, and a title never enters the log at all.
	if got := len(h.recorder.all()); got != 1 {
		t.Errorf("a retitle recorded %v; want only the creation event", h.recorder.types())
	}
}

// TestUpdateTaskValidatesEverythingBeforeWriting is the all-or-nothing rule.
//
// A request carrying a legal status and an illegal title must change nothing,
// rather than changing the half that happened to be checked first.
func TestUpdateTaskValidatesEverythingBeforeWriting(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")

	bad := "  " // whitespace only: refused by ValidateTitle
	running := StatusRunning
	_, err := h.service.UpdateTask(context.Background(), tk.ID, UpdateTaskInput{
		Title:  &bad,
		Status: &running,
	})
	if !IsCode(err, CodeInvalidTitle) {
		t.Fatalf("UpdateTask with a bad title returned %v; want %q", err, CodeInvalidTitle)
	}

	stored, err := h.service.GetTask(context.Background(), tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != StatusCreated {
		t.Errorf("status = %q; a request with an invalid title changed the status anyway", stored.Status)
	}
	if stored.Title != "Fix the viewer" {
		t.Errorf("title = %q; want the original", stored.Title)
	}
}

func TestUpdateTaskWithNothingToChangeIsRefused(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	_, err := h.service.UpdateTask(context.Background(), tk.ID, UpdateTaskInput{})
	if !IsCode(err, CodeInvalidInput) {
		t.Fatalf("an empty update returned %v; want %q", err, CodeInvalidInput)
	}
}

func TestUpdateTaskRecordsAStatusEvent(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	h.advance(t, tk.ID, StatusRunning)

	events := h.recorder.all()
	if len(events) != 2 {
		t.Fatalf("events = %v; want a creation and a status change", h.recorder.types())
	}
	last := events[1]
	if last.Type != TypeTaskStatusChanged {
		t.Errorf("type = %q; want %q", last.Type, TypeTaskStatusChanged)
	}
	if last.Payload["from"] != StatusCreated || last.Payload["to"] != StatusRunning {
		t.Errorf("payload = %v; want the from and to statuses", last.Payload)
	}
	if last.Payload["taskId"] != tk.ID {
		t.Errorf("payload names %v; want taskId %s", last.Payload, tk.ID)
	}
}

// TestAFailingRecorderDoesNotFailARequest is the other half of §三十七.
//
// The status has already been written when the event is attempted. A request
// that failed because the history could not be written would make the log a
// precondition of the thing it records.
func TestAFailingRecorderDoesNotFailARequest(t *testing.T) {
	h := newHarness(t)
	h.recorder.err = errors.New("the log is unavailable")

	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	running, err := h.service.UpdateTaskStatus(context.Background(), tk.ID, StatusRunning)
	if err != nil {
		t.Fatalf("a failing recorder failed the request: %v", err)
	}
	if running.Status != StatusRunning {
		t.Errorf("status = %q; want %q", running.Status, StatusRunning)
	}
	stored, err := h.service.GetTask(context.Background(), tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != StatusRunning {
		t.Errorf("the stored status is %q; the change was rolled back by a log failure", stored.Status)
	}
}

// TestUpdateTaskStatusRejectsAnUnknownTask keeps the 404 path honest: an id
// that has the right shape but names nothing is a missing task and not a
// storage failure.
func TestUpdateTaskStatusRejectsAnUnknownTask(t *testing.T) {
	h := newHarness(t)
	missing := h.taskIDValue()
	_, err := h.service.UpdateTaskStatus(context.Background(), missing, StatusRunning)
	if !IsCode(err, CodeTaskNotFound) {
		t.Fatalf("UpdateTaskStatus on a missing task returned %v; want %q", err, CodeTaskNotFound)
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// TestAStatusChangeThatLosesARaceIsRefused drives the window between the
// service's read and its write.
//
// This is the service half of the concurrency answer: the repository refuses
// the write because the row no longer holds the status the transition was
// validated against, and the service has to recognise that as a lost race
// rather than as a storage failure. The database half - that two real requests
// cannot both apply - is asserted against SQLite in internal/storage.
func TestAStatusChangeThatLosesARaceIsRefused(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	h.advance(t, tk.ID, StatusRunning)

	// Another writer moves the task to COMPLETED after the service has read it
	// as RUNNING and validated RUNNING -> FAILED against that.
	h.repo.onUpdateTaskStatus = func(r *memoryRepo) {
		r.tasks[tk.ID].Status = StatusCompleted
		r.onUpdateTaskStatus = nil
	}

	_, err := h.service.UpdateTaskStatus(context.Background(), tk.ID, StatusFailed)
	if !IsCode(err, CodeConflict) {
		t.Fatalf("a lost race returned %v; want %q", err, CodeConflict)
	}

	var taskErr *Error
	if !errors.As(err, &taskErr) {
		t.Fatalf("the refusal is %T, not a task error", err)
	}
	if taskErr.Details["expectedStatus"] != StatusRunning {
		t.Errorf("details = %v; want the status the caller validated against", taskErr.Details)
	}

	// The winner's value stands. The loser's transition was never applied, and
	// the task is not left in a state neither of them asked for.
	stored, err := h.service.GetTask(context.Background(), tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != StatusCompleted {
		t.Errorf("status = %q; want the winner's %q", stored.Status, StatusCompleted)
	}
}

// TestConcurrentTransitionsLeaveTheTaskInALegalState hammers one task from many
// goroutines at once.
//
// Every goroutine asks for the same legal transition from the same status, and
// the assertions are about what the *store* ends up holding rather than about
// who won. A goroutine that reads the task after the winner has written finds
// it already COMPLETED, and UpdateTaskStatus answers that with the task as it
// stands instead of a refusal - a client retrying a request it is not sure
// landed should get the state it asked for. So the successes counted below are
// one transition and a number of no-ops, and the number of no-ops depends on
// scheduling.
//
// The invariant that does not depend on scheduling is the one asserted: exactly
// one write reached the row, no request failed for a reason other than a lost
// race, and the task ends in the status every one of them asked for.
func TestConcurrentTransitionsLeaveTheTaskInALegalState(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	h.advance(t, tk.ID, StatusRunning)
	// The baseline is taken after the setup transition, so that what is counted
	// below is the concurrent block alone.
	before := h.repo.taskStatusWrites()

	const workers = 32
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		refused int
		other   []error
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := h.service.UpdateTaskStatus(context.Background(), tk.ID, StatusCompleted)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && !IsCode(err, CodeConflict) {
				other = append(other, err)
			}
			if IsCode(err, CodeConflict) {
				refused++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) != 0 {
		t.Fatalf("transitions failed for reasons that are not conflicts: %v", other)
	}
	if refused > workers-1 {
		t.Errorf("%d of %d requests were refused; at most %d may be", refused, workers, workers-1)
	}

	// The count that matters: the row was written once. Every other request
	// either lost the race or found the work already done.
	if applied := h.repo.taskStatusWrites() - before; applied != 1 {
		t.Errorf("%d status writes reached the row; exactly one may", applied)
	}

	stored, err := h.service.GetTask(context.Background(), tk.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != StatusCompleted {
		t.Errorf("status = %q; want %q", stored.Status, StatusCompleted)
	}
	if stored.CompletedAt == nil {
		t.Error("the winning transition did not record a completion time")
	}
}

// ---------------------------------------------------------------------------
// Agent sessions
// ---------------------------------------------------------------------------

func TestCreateSessionStartsWithNoRuntime(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")

	h.tick(time.Minute)
	sess := h.createSession(t, tk.ID)

	if !ValidSessionID(sess.ID) {
		t.Errorf("id %q is not a session id", sess.ID)
	}
	if sess.TaskID != tk.ID {
		t.Errorf("taskId = %q; want %q", sess.TaskID, tk.ID)
	}
	if sess.RuntimeID != "" {
		t.Errorf("runtimeId = %q; an attempt begins before a runtime is chosen", sess.RuntimeID)
	}
	if sess.Status != StatusSessionCreated {
		t.Errorf("status = %q; want %q", sess.Status, StatusSessionCreated)
	}
	if sess.StartedAt != nil || sess.EndedAt != nil {
		t.Errorf("startedAt = %v, endedAt = %v; an attempt that has not run has neither",
			sess.StartedAt, sess.EndedAt)
	}
	if !sess.CreatedAt.Equal(h.now) {
		t.Errorf("createdAt = %v; want %v", sess.CreatedAt, h.now)
	}
}

func TestCreateSessionRequiresAnExistingTask(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.CreateSession(context.Background(), CreateSessionInput{
		TaskID: h.taskIDValue(),
	})
	if !IsCode(err, CodeTaskNotFound) {
		t.Fatalf("CreateSession on a missing task returned %v; want %q", err, CodeTaskNotFound)
	}
	_, err = h.service.CreateSession(context.Background(), CreateSessionInput{})
	if !IsCode(err, CodeInvalidInput) {
		t.Fatalf("CreateSession with no task returned %v; want %q", err, CodeInvalidInput)
	}
}

func TestCreateSessionRecordsOneEvent(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	sess := h.createSession(t, tk.ID)

	events := h.recorder.all()
	if len(events) != 2 {
		t.Fatalf("events = %v; want a task and a session creation", h.recorder.types())
	}
	last := events[1]
	if last.Type != TypeSessionCreated {
		t.Errorf("type = %q; want %q", last.Type, TypeSessionCreated)
	}
	if last.ProjectID != "p_alpha" {
		t.Errorf("projectId = %q; want p_alpha: a session is placed by its task", last.ProjectID)
	}
	if last.RuntimeID != "" {
		t.Errorf("runtimeId = %q; no runtime has been attached", last.RuntimeID)
	}
	if last.Payload["sessionId"] != sess.ID || last.Payload["taskId"] != tk.ID {
		t.Errorf("payload = %v; want both identifiers", last.Payload)
	}
}

func TestListSessionsIsScopedToTheTask(t *testing.T) {
	h := newHarness(t)
	first := h.createTask(t, "p_alpha", "One")
	second := h.createTask(t, "p_alpha", "Two")

	one := h.createSession(t, first.ID)
	h.tick(time.Second)
	h.createSession(t, second.ID)
	h.tick(time.Second)
	two := h.createSession(t, first.ID)

	sessions, err := h.service.ListSessions(context.Background(), ListSessionsInput{TaskID: first.ID})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ListSessions returned %d attempts; want 2", len(sessions))
	}
	if sessions[0].ID != two.ID || sessions[1].ID != one.ID {
		t.Errorf("ListSessions is not newest first: got %s, %s", sessions[0].ID, sessions[1].ID)
	}
	for _, sess := range sessions {
		if sess.TaskID != first.ID {
			t.Errorf("attempt %s belongs to %s", sess.ID, sess.TaskID)
		}
	}
}

func TestListSessionsRequiresAnExistingTask(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.ListSessions(context.Background(), ListSessionsInput{
		TaskID: h.taskIDValue(),
	})
	if !IsCode(err, CodeTaskNotFound) {
		t.Fatalf("ListSessions on a missing task returned %v; want %q", err, CodeTaskNotFound)
	}
	_, err = h.service.ListSessions(context.Background(), ListSessionsInput{})
	if !IsCode(err, CodeInvalidInput) {
		t.Fatalf("ListSessions with no task returned %v; want %q", err, CodeInvalidInput)
	}
}

func TestSessionStatusSetsTheTimestampsItShould(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	sess := h.createSession(t, tk.ID)

	h.tick(time.Minute)
	started, err := h.service.UpdateSessionStatus(context.Background(), sess.ID, StatusSessionRunning)
	if err != nil {
		t.Fatalf("UpdateSessionStatus: %v", err)
	}
	if started.StartedAt == nil || !started.StartedAt.Equal(h.now) {
		t.Errorf("startedAt = %v; want %v", started.StartedAt, h.now)
	}
	if started.EndedAt != nil {
		t.Errorf("endedAt = %v; a running attempt has not ended", started.EndedAt)
	}

	h.tick(time.Minute)
	ended, err := h.service.UpdateSessionStatus(context.Background(), sess.ID, StatusSessionCompleted)
	if err != nil {
		t.Fatalf("UpdateSessionStatus: %v", err)
	}
	if ended.EndedAt == nil || !ended.EndedAt.Equal(h.now) {
		t.Errorf("endedAt = %v; want %v", ended.EndedAt, h.now)
	}
	// The start is a fact about the attempt and a later transition does not
	// rewrite it.
	if ended.StartedAt == nil || !ended.StartedAt.Equal(*started.StartedAt) {
		t.Errorf("startedAt moved from %v to %v", started.StartedAt, ended.StartedAt)
	}
	if ended.Status != StatusSessionCompleted {
		t.Errorf("status = %q; want %q", ended.Status, StatusSessionCompleted)
	}
}

// TestAnAttemptThatNeverRanHasAnEndAndNoStart is the case the timestamps are
// shaped around.
//
// A runtime that never came up is a real attempt that did not happen, and it is
// recorded by failing the session without ever running it.
func TestAnAttemptThatNeverRanHasAnEndAndNoStart(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	sess := h.createSession(t, tk.ID)

	h.tick(time.Minute)
	failed, err := h.service.UpdateSessionStatus(context.Background(), sess.ID, StatusSessionFailed)
	if err != nil {
		t.Fatalf("UpdateSessionStatus CREATED -> FAILED: %v", err)
	}
	if failed.StartedAt != nil {
		t.Errorf("startedAt = %v; the attempt never ran", failed.StartedAt)
	}
	if failed.EndedAt == nil {
		t.Error("endedAt is nil; the attempt is over")
	}
}

func TestSessionStatusRefusesAnIllegalTransition(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	sess := h.createSession(t, tk.ID)
	h.service.UpdateSessionStatus(context.Background(), sess.ID, StatusSessionRunning)
	h.service.UpdateSessionStatus(context.Background(), sess.ID, StatusSessionCompleted)

	_, err := h.service.UpdateSessionStatus(context.Background(), sess.ID, StatusSessionRunning)
	if !IsCode(err, CodeInvalidTransition) {
		t.Fatalf("COMPLETED -> RUNNING for a session returned %v; want %q", err, CodeInvalidTransition)
	}
}

func TestSessionStatusRejectsAnUnknownStatus(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	sess := h.createSession(t, tk.ID)
	// WAITING is a task status and deliberately not a session status.
	_, err := h.service.UpdateSessionStatus(context.Background(), sess.ID, StatusWaiting)
	if !IsCode(err, CodeInvalidInput) {
		t.Fatalf("a session moved to WAITING returned %v; want %q", err, CodeInvalidInput)
	}
}

// ---------------------------------------------------------------------------
// Attaching a runtime
// ---------------------------------------------------------------------------

func TestAttachSessionRuntimeBindsOnce(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	sess := h.createSession(t, tk.ID)

	const runtimeID = runtimeIDForTest
	attached, err := h.service.AttachSessionRuntime(context.Background(), sess.ID, runtimeID)
	if err != nil {
		t.Fatalf("AttachSessionRuntime: %v", err)
	}
	if attached.RuntimeID != runtimeID {
		t.Errorf("runtimeId = %q; want %q", attached.RuntimeID, runtimeID)
	}
	if !attached.HasRuntime() {
		t.Error("a session with a runtime does not report one")
	}
	// The status is untouched: the link is bookkeeping, not a transition.
	if attached.Status != StatusSessionCreated {
		t.Errorf("status = %q; attaching a runtime is not a status change", attached.Status)
	}

	// A second attach is refused rather than overwriting. The column reads "the
	// runtime this attempt ran in", and a field that can be rewritten stops
	// answering that.
	_, err = h.service.AttachSessionRuntime(context.Background(), sess.ID, otherRuntimeIDForTest)
	if !IsCode(err, CodeConflict) {
		t.Fatalf("a second attach returned %v; want %q", err, CodeConflict)
	}
	stored, err := h.service.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if stored.RuntimeID != runtimeID {
		t.Errorf("runtimeId = %q; the second attach overwrote the first", stored.RuntimeID)
	}
}

func TestAttachSessionRuntimeRequiresARuntimeIdentifier(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	sess := h.createSession(t, tk.ID)

	for _, bad := range []string{
		"",
		"p_alpha",                     // a project id, not a runtime id
		"runtime-1",                   // a name from nowhere
		"AMX-p_0123456789abcdef0123",  // the right shape in the wrong case
		"amx-",                        // the prefix and nothing else
		"amx-1",                       // a body that is not a project id
		"amx-p_alpha",                 // a body that looks like one and is not
		"amx-p_0123456789abcdef012",   // one character short
		"amx-p_0123456789abcdef01234", // one character long
		"amx-p_0123456789ABCDEF0123",  // uppercase hex
	} {
		_, err := h.service.AttachSessionRuntime(context.Background(), sess.ID, bad)
		if !IsCode(err, CodeInvalidInput) {
			t.Errorf("AttachSessionRuntime(%q) returned %v; want %q", bad, err, CodeInvalidInput)
		}
	}
	if _, err := h.service.AttachSessionRuntime(context.Background(), sess.ID, runtimeIDForTest); err != nil {
		t.Errorf("AttachSessionRuntime on a well-formed runtime id returned %v", err)
	}
}

// TestAttachingDoesNotRequireTheRuntimeToExist is the "history outlives the
// process" rule, seen from the service side.
//
// The runtime id must be well-formed and it must not be looked up. A runtime is
// destroyed and its row is deleted; an attempt that ran in it remains, and the
// question "does this runtime exist right now" is the wrong one to ask about
// the runtime an attempt ran in.
func TestAttachingDoesNotRequireTheRuntimeToExist(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	sess := h.createSession(t, tk.ID)

	// A runtime id for a project that does not exist, naming a runtime that was
	// never started.
	const ghost = ghostRuntimeIDForTest
	attached, err := h.service.AttachSessionRuntime(context.Background(), sess.ID, ghost)
	if err != nil {
		t.Fatalf("attaching a runtime that does not exist: %v", err)
	}
	if attached.RuntimeID != ghost {
		t.Fatalf("runtimeId = %q; want %q", attached.RuntimeID, ghost)
	}

	// And it survives: reading the attempt back still names it, which is what a
	// foreign key into a table whose rows are deleted would have prevented.
	stored, err := h.service.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if stored.RuntimeID != ghost {
		t.Errorf("runtimeId = %q; the link did not survive", stored.RuntimeID)
	}
}

// TestAttachingRecordsNothing pins the decision recorded in docs/TASK_MODEL.md
// §6: attaching is bookkeeping rather than a change in what is happening.
func TestAttachingRecordsNothing(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	sess := h.createSession(t, tk.ID)
	before := len(h.recorder.all())

	if _, err := h.service.AttachSessionRuntime(context.Background(), sess.ID, runtimeIDForTest); err != nil {
		t.Fatalf("AttachSessionRuntime: %v", err)
	}
	if got := len(h.recorder.all()); got != before {
		t.Errorf("attaching recorded %v; an attach is not a fact about what happened",
			h.recorder.types()[before:])
	}
}

// TestASessionEventNamesTheRuntimeOnceAttached checks where the timeline places
// an attempt's status change.
//
// A session names its runtime, and a runtime's events are one of the two
// endpoints Phase 7.1 built. That is the whole of the link between the task
// layer and the event log: no column was added to agent_events for it.
func TestASessionEventNamesTheRuntimeOnceAttached(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	sess := h.createSession(t, tk.ID)
	if _, err := h.service.AttachSessionRuntime(context.Background(), sess.ID, runtimeIDForTest); err != nil {
		t.Fatalf("AttachSessionRuntime: %v", err)
	}
	if _, err := h.service.UpdateSessionStatus(context.Background(), sess.ID, StatusSessionRunning); err != nil {
		t.Fatalf("UpdateSessionStatus: %v", err)
	}

	events := h.recorder.all()
	last := events[len(events)-1]
	if last.Type != TypeSessionStatusChanged {
		t.Fatalf("type = %q; want %q", last.Type, TypeSessionStatusChanged)
	}
	if last.RuntimeID != runtimeIDForTest {
		t.Errorf("runtimeId = %q; the timeline has no other way to place this", last.RuntimeID)
	}
	if last.ProjectID != "p_alpha" {
		t.Errorf("projectId = %q; want p_alpha", last.ProjectID)
	}
	if last.Payload["to"] != StatusSessionRunning {
		t.Errorf("payload = %v; want the status it moved to", last.Payload)
	}
}

// TestASessionStatusChangeSurvivesAMissingTaskRow covers the one lookup that is
// allowed to fail.
//
// The project is resolved so the event can be placed, and by then the status
// change has already been written. A lookup that failed the request would be a
// lie about the state.
func TestASessionStatusChangeSurvivesAMissingTaskRow(t *testing.T) {
	h := newHarness(t)
	tk := h.createTask(t, "p_alpha", "Fix the viewer")
	sess := h.createSession(t, tk.ID)

	// The task row disappears between the session write and the event lookup.
	h.repo.mu.Lock()
	delete(h.repo.tasks, tk.ID)
	h.repo.mu.Unlock()

	running, err := h.service.UpdateSessionStatus(context.Background(), sess.ID, StatusSessionRunning)
	if err != nil {
		t.Fatalf("a missing task row failed the status change: %v", err)
	}
	if running.Status != StatusSessionRunning {
		t.Errorf("status = %q; want %q", running.Status, StatusSessionRunning)
	}
}

// TestSessionUpdatesRejectAMissingSession keeps the session 404 honest.
func TestSessionUpdatesRejectAMissingSession(t *testing.T) {
	h := newHarness(t)
	missing := h.sessionIDValue()
	_, err := h.service.UpdateSessionStatus(context.Background(), missing, StatusSessionRunning)
	if !IsCode(err, CodeSessionNotFound) {
		t.Fatalf("UpdateSessionStatus on a missing session returned %v; want %q", err, CodeSessionNotFound)
	}
	_, err = h.service.AttachSessionRuntime(context.Background(), missing, runtimeIDForTest)
	if !IsCode(err, CodeSessionNotFound) {
		t.Fatalf("AttachSessionRuntime on a missing session returned %v; want %q", err, CodeSessionNotFound)
	}
}
