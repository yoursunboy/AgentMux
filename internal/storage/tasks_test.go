package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/event"
	"github.com/kutonlagos/agentmux/internal/session"
	"github.com/kutonlagos/agentmux/internal/task"
	"github.com/kutonlagos/agentmux/migrations"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// registerProject creates a project row, because every task needs one.
//
// The foreign key is enforced on this connection - the DSN turns it on - so a
// test that stored a task against a project that does not exist would fail on
// the constraint rather than on the property it meant to check.
func registerProject(t *testing.T, store *Store, id string) {
	t.Helper()
	p := testProject(id, "App "+id, `D:\AI\Projects\`+id)
	if err := store.Projects().Create(context.Background(), p); err != nil {
		t.Fatalf("creating project %s: %v", id, err)
	}
}

// testTask builds a task in the shape the service would store.
func testTask(id, projectID, title, status string, createdAt time.Time) *task.Task {
	return &task.Task{
		ID:        id,
		ProjectID: projectID,
		Title:     title,
		Status:    status,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
}

// testSession builds an agent session in the shape the service would store.
func testSession(id, taskID string, createdAt time.Time) *task.AgentSession {
	return &task.AgentSession{
		ID:        id,
		TaskID:    taskID,
		Status:    task.StatusSessionCreated,
		CreatedAt: createdAt,
	}
}

// taskIDs is the id column of a task list, for comparing against an expected
// order.
func taskIDs(tasks []*task.Task) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.ID)
	}
	return out
}

func sessionIDs(sessions []*task.AgentSession) []string {
	out := make([]string, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.ID)
	}
	return out
}

// ---------------------------------------------------------------------------
// The contract and the round trip
// ---------------------------------------------------------------------------

func TestTaskStoreSatisfiesTheRepositoryContract(t *testing.T) {
	// The compile-time assertion in tasks.go is the real check; this makes the
	// intent visible in the suite as well.
	var store *TaskStore = newTestStore(t).Tasks()
	var _ task.Repository = store
}

func TestCreateTaskThenGetIt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")

	created := at(2026, time.September, 20, 9)
	original := testTask("task_0123456789abcdef01234567", "p_0123456789abcdef0123",
		"Fix the websocket reconnect bug", task.StatusRunning, created)

	if err := store.Tasks().CreateTask(ctx, original); err != nil {
		t.Fatalf("CreateTask returned an error: %v", err)
	}
	stored, err := store.Tasks().GetTask(ctx, original.ID)
	if err != nil {
		t.Fatalf("GetTask returned an error: %v", err)
	}

	if stored.ID != original.ID {
		t.Errorf("ID = %q, want %q", stored.ID, original.ID)
	}
	if stored.ProjectID != original.ProjectID {
		t.Errorf("ProjectID = %q, want %q", stored.ProjectID, original.ProjectID)
	}
	if stored.Title != original.Title {
		t.Errorf("Title = %q, want %q", stored.Title, original.Title)
	}
	if stored.Status != original.Status {
		t.Errorf("Status = %q, want %q", stored.Status, original.Status)
	}
	if !stored.CreatedAt.Equal(created) || !stored.UpdatedAt.Equal(created) {
		t.Errorf("timestamps = %v / %v, want %v", stored.CreatedAt, stored.UpdatedAt, created)
	}
	if stored.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil for a task that has not completed", stored.CompletedAt)
	}
}

func TestGetTaskReportsAMissingRow(t *testing.T) {
	store := newTestStore(t)

	_, err := store.Tasks().GetTask(context.Background(), "task_ffffffffffffffffffffffff")
	if !errors.Is(err, task.ErrNotFound) {
		t.Errorf("GetTask on an unknown id returned %v, want task.ErrNotFound", err)
	}
}

func TestCreateTaskRejectsANilTask(t *testing.T) {
	store := newTestStore(t)

	if err := store.Tasks().CreateTask(context.Background(), nil); err == nil {
		t.Error("CreateTask(nil) succeeded, want a rejection")
	}
}

// TestTaskTimestampsAreStoredInUTC keeps a task's times comparable with every
// other time in the database regardless of the zone the server happens to run
// in.
func TestTaskTimestampsAreStoredInUTC(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")

	zone := time.FixedZone("UTC+8", 8*3600)
	local := time.Date(2026, time.September, 20, 20, 0, 0, 0, zone)
	if err := store.Tasks().CreateTask(ctx,
		testTask("task_0123456789abcdef01234567", "p_0123456789abcdef0123", "Ship it", task.StatusCreated, local),
	); err != nil {
		t.Fatalf("CreateTask returned an error: %v", err)
	}

	stored, err := store.Tasks().GetTask(ctx, "task_0123456789abcdef01234567")
	if err != nil {
		t.Fatalf("GetTask returned an error: %v", err)
	}
	if !stored.CreatedAt.Equal(local) {
		t.Errorf("CreatedAt = %v, want the same instant as %v", stored.CreatedAt, local)
	}
	if got := stored.CreatedAt.UTC().Format(time.RFC3339); got != "2026-09-20T12:00:00Z" {
		t.Errorf("CreatedAt reads as %s, want 2026-09-20T12:00:00Z", got)
	}
}

// TestATaskTitleIsStoredAsOpaqueText is the storage half of the rule the model
// test states: a title is text a person wrote, and the only thing that must be
// true of it is that it comes back unchanged.
//
// Every one of these would break a query built by concatenation. They go
// through a bound parameter, so they do not, and this is what says so.
func TestATaskTitleIsStoredAsOpaqueText(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")

	titles := []string{
		`Robert"); DROP TABLE tasks; --`,
		"a title with 'single quotes' in it",
		`a title with "double quotes" in it`,
		"a title with a \u0000-free backslash \\ in it",
		"implement 控制器查看器",
		"a title with an emoji 🚀",
		`SELECT * FROM tasks WHERE 1=1`,
		"line one\nline two", // refused by the service, read correctly by storage
		strings.Repeat("x", 200),
	}
	for i, title := range titles {
		id := fmt.Sprintf("task_%024x", i)
		if err := store.Tasks().CreateTask(ctx,
			testTask(id, "p_0123456789abcdef0123", title, task.StatusCreated, at(2026, time.September, 20, 9)),
		); err != nil {
			t.Fatalf("CreateTask(%q) returned an error: %v", title, err)
		}
		stored, err := store.Tasks().GetTask(ctx, id)
		if err != nil {
			t.Fatalf("GetTask(%q) returned an error: %v", id, err)
		}
		if stored.Title != title {
			t.Errorf("title round-tripped as %q, want %q", stored.Title, title)
		}
	}

	// The table is still there, which is the point of the first title above.
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&count); err != nil {
		t.Fatalf("counting tasks failed: %v", err)
	}
	if count != len(titles) {
		t.Errorf("the table holds %d tasks, want %d", count, len(titles))
	}
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

func TestTaskListIsScopedToItsProject(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")
	registerProject(t, store, "p_0123456789abcdef0124")

	mine := testTask("task_00000000000000000000000a", "p_0123456789abcdef0123",
		"mine", task.StatusCreated, at(2026, time.September, 20, 9))
	theirs := testTask("task_00000000000000000000000b", "p_0123456789abcdef0124",
		"theirs", task.StatusCreated, at(2026, time.September, 20, 10))
	for _, tk := range []*task.Task{mine, theirs} {
		if err := store.Tasks().CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask returned an error: %v", err)
		}
	}

	got, err := store.Tasks().ListTasks(ctx, task.TaskQuery{ProjectID: "p_0123456789abcdef0123"})
	if err != nil {
		t.Fatalf("ListTasks returned an error: %v", err)
	}
	if ids := taskIDs(got); len(ids) != 1 || ids[0] != mine.ID {
		t.Errorf("ListTasks returned %v, want only %s", ids, mine.ID)
	}
}

// TestTaskListIsNewestFirstWithTheIDAsATieBreak pins the ordering rule rather
// than a sequence, because two tasks created in the same millisecond are
// ordinary and the ordering has to be total anyway.
func TestTaskListIsNewestFirstWithTheIDAsATieBreak(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")

	same := at(2026, time.September, 20, 9)
	fixtures := []*task.Task{
		testTask("task_000000000000000000000001", "p_0123456789abcdef0123", "oldest", task.StatusCreated, at(2026, time.September, 20, 8)),
		// Three rows sharing a timestamp. Their order is decided by the id, in
		// descending order, which is what makes the listing stable across runs
		// rather than whenever the query planner feels like returning them.
		testTask("task_000000000000000000000003", "p_0123456789abcdef0123", "newest b", task.StatusCreated, same),
		testTask("task_000000000000000000000002", "p_0123456789abcdef0123", "newest a", task.StatusCreated, same),
		testTask("task_000000000000000000000004", "p_0123456789abcdef0123", "newest c", task.StatusCreated, same),
	}
	for _, tk := range fixtures {
		if err := store.Tasks().CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask returned an error: %v", err)
		}
	}

	got, err := store.Tasks().ListTasks(ctx, task.TaskQuery{ProjectID: "p_0123456789abcdef0123"})
	if err != nil {
		t.Fatalf("ListTasks returned an error: %v", err)
	}
	want := []string{
		"task_000000000000000000000004",
		"task_000000000000000000000003",
		"task_000000000000000000000002",
		"task_000000000000000000000001",
	}
	if ids := taskIDs(got); strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ListTasks returned %v, want %v", ids, want)
	}
}

func TestTaskListFiltersByStatus(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")

	for i, status := range []string{task.StatusCreated, task.StatusRunning, task.StatusCompleted} {
		tk := testTask(fmt.Sprintf("task_%024x", i), "p_0123456789abcdef0123",
			"work", status, at(2026, time.September, 20, 9+i))
		if err := store.Tasks().CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask returned an error: %v", err)
		}
	}

	got, err := store.Tasks().ListTasks(ctx, task.TaskQuery{
		ProjectID: "p_0123456789abcdef0123",
		Status:    task.StatusRunning,
	})
	if err != nil {
		t.Fatalf("ListTasks returned an error: %v", err)
	}
	if len(got) != 1 || got[0].Status != task.StatusRunning {
		t.Errorf("ListTasks(RUNNING) returned %d rows, want the one RUNNING task", len(got))
	}
}

// TestTaskListRefusesToGuessAScope is the guard against the read that would
// return every task AgentMux has ever recorded.
func TestTaskListRefusesToGuessAScope(t *testing.T) {
	store := newTestStore(t)

	for _, id := range []string{"", "   ", "\t"} {
		got, err := store.Tasks().ListTasks(context.Background(), task.TaskQuery{ProjectID: id})
		if err == nil {
			t.Fatalf("ListTasks(ProjectID: %q) returned %d rows, want a refusal", id, len(got))
		}
		if !task.IsCode(err, task.CodeInvalidInput) {
			t.Errorf("ListTasks(ProjectID: %q) failed with %v, want %q", id, err, task.CodeInvalidInput)
		}
	}
}

func TestTaskListReturnsAnEmptySliceNotNull(t *testing.T) {
	store := newTestStore(t)
	registerProject(t, store, "p_0123456789abcdef0123")

	got, err := store.Tasks().ListTasks(context.Background(),
		task.TaskQuery{ProjectID: "p_0123456789abcdef0123"})
	if err != nil {
		t.Fatalf("ListTasks returned an error: %v", err)
	}
	if got == nil {
		t.Error("ListTasks returned nil, want an empty slice")
	}
	if len(got) != 0 {
		t.Errorf("ListTasks returned %d rows, want none", len(got))
	}
}

func TestTaskListHonoursTheLimit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")

	for i := 0; i < 5; i++ {
		tk := testTask(fmt.Sprintf("task_%024x", i), "p_0123456789abcdef0123",
			"work", task.StatusCreated, at(2026, time.September, 20, 9+i))
		if err := store.Tasks().CreateTask(ctx, tk); err != nil {
			t.Fatalf("CreateTask returned an error: %v", err)
		}
	}

	got, err := store.Tasks().ListTasks(ctx,
		task.TaskQuery{ProjectID: "p_0123456789abcdef0123", Limit: 2})
	if err != nil {
		t.Fatalf("ListTasks returned an error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("ListTasks(Limit: 2) returned %d rows, want 2", len(got))
	}
}

// ---------------------------------------------------------------------------
// Conditional updates
// ---------------------------------------------------------------------------

func TestUpdateTaskStatusMovesTheRow(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")

	tk := testTask("task_0123456789abcdef01234567", "p_0123456789abcdef0123",
		"Fix the viewer", task.StatusCreated, at(2026, time.September, 20, 9))
	if err := store.Tasks().CreateTask(ctx, tk); err != nil {
		t.Fatalf("CreateTask returned an error: %v", err)
	}

	moved := at(2026, time.September, 20, 10)
	err := store.Tasks().UpdateTaskStatus(ctx, tk.ID, task.TaskStatusChange{
		From: task.StatusCreated, To: task.StatusRunning, At: moved,
	})
	if err != nil {
		t.Fatalf("UpdateTaskStatus returned an error: %v", err)
	}

	stored, err := store.Tasks().GetTask(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetTask returned an error: %v", err)
	}
	if stored.Status != task.StatusRunning {
		t.Errorf("Status = %q, want %q", stored.Status, task.StatusRunning)
	}
	if !stored.UpdatedAt.Equal(moved) {
		t.Errorf("UpdatedAt = %v, want %v", stored.UpdatedAt, moved)
	}
	if !stored.CreatedAt.Equal(tk.CreatedAt) {
		t.Errorf("CreatedAt = %v, want it unchanged at %v", stored.CreatedAt, tk.CreatedAt)
	}
}

// TestUpdateTaskStatusRefusesAStaleFrom is the concurrency answer, asserted at
// the layer that implements it.
func TestUpdateTaskStatusRefusesAStaleFrom(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")

	tk := testTask("task_0123456789abcdef01234567", "p_0123456789abcdef0123",
		"Fix the viewer", task.StatusCreated, at(2026, time.September, 20, 9))
	if err := store.Tasks().CreateTask(ctx, tk); err != nil {
		t.Fatalf("CreateTask returned an error: %v", err)
	}
	if err := store.Tasks().UpdateTaskStatus(ctx, tk.ID, task.TaskStatusChange{
		From: task.StatusCreated, To: task.StatusRunning, At: at(2026, time.September, 20, 10),
	}); err != nil {
		t.Fatalf("the first UpdateTaskStatus returned an error: %v", err)
	}

	// A second caller that read CREATED before the first landed still believes
	// the row holds CREATED. It must not apply.
	err := store.Tasks().UpdateTaskStatus(ctx, tk.ID, task.TaskStatusChange{
		From: task.StatusCreated, To: task.StatusCancelled, At: at(2026, time.September, 20, 11),
	})
	if !errors.Is(err, task.ErrStatusConflict) {
		t.Fatalf("the stale update returned %v, want task.ErrStatusConflict", err)
	}

	stored, err := store.Tasks().GetTask(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetTask returned an error: %v", err)
	}
	if stored.Status != task.StatusRunning {
		t.Errorf("Status = %q after a refused update, want %q unchanged", stored.Status, task.StatusRunning)
	}
	if !stored.UpdatedAt.Equal(at(2026, time.September, 20, 10)) {
		t.Errorf("UpdatedAt = %v after a refused update, want it unchanged", stored.UpdatedAt)
	}
}

// TestTheConditionalUpdateTellsAMissingRowFromARace records the distinction the
// service is written against: "gone" and "moved" are different answers.
func TestTheConditionalUpdateTellsAMissingRowFromARace(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")

	err := store.Tasks().UpdateTaskStatus(ctx, "task_ffffffffffffffffffffffff", task.TaskStatusChange{
		From: task.StatusCreated, To: task.StatusRunning, At: at(2026, time.September, 20, 10),
	})
	if !errors.Is(err, task.ErrNotFound) {
		t.Errorf("updating a task that does not exist returned %v, want task.ErrNotFound", err)
	}
	if errors.Is(err, task.ErrStatusConflict) {
		t.Error("a missing row was reported as a lost race")
	}
}

// TestCompletedAtFollowsTheStatus pins the column's rule at the storage layer:
// it is set by completion and cleared by everything else, so a failed task
// carries no claim that something was completed.
func TestCompletedAtFollowsTheStatus(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")

	tk := testTask("task_0123456789abcdef01234567", "p_0123456789abcdef0123",
		"Fix the viewer", task.StatusCreated, at(2026, time.September, 20, 9))
	if err := store.Tasks().CreateTask(ctx, tk); err != nil {
		t.Fatalf("CreateTask returned an error: %v", err)
	}

	completed := at(2026, time.September, 20, 11)
	if err := store.Tasks().UpdateTaskStatus(ctx, tk.ID, task.TaskStatusChange{
		From: task.StatusCreated, To: task.StatusCompleted, At: completed, CompletedAt: &completed,
	}); err != nil {
		t.Fatalf("completing the task returned an error: %v", err)
	}
	stored, err := store.Tasks().GetTask(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetTask returned an error: %v", err)
	}
	if stored.CompletedAt == nil {
		t.Fatal("CompletedAt is nil after the task completed")
	}
	if !stored.CompletedAt.Equal(completed) {
		t.Errorf("CompletedAt = %v, want %v", stored.CompletedAt, completed)
	}

	// The store does not check lifecycles - that is the service's job - so it
	// will apply an edge the service would refuse. What is asserted here is
	// only that the completion time goes away with the status.
	if err := store.Tasks().UpdateTaskStatus(ctx, tk.ID, task.TaskStatusChange{
		From: task.StatusCompleted, To: task.StatusFailed, At: at(2026, time.September, 20, 12),
	}); err != nil {
		t.Fatalf("the second update returned an error: %v", err)
	}
	stored, err = store.Tasks().GetTask(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetTask returned an error: %v", err)
	}
	if stored.CompletedAt != nil {
		t.Errorf("CompletedAt = %v after leaving COMPLETED, want nil", stored.CompletedAt)
	}
}

func TestUpdateTaskTitleRewritesOnlyTheTitle(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")

	created := at(2026, time.September, 20, 9)
	tk := testTask("task_0123456789abcdef01234567", "p_0123456789abcdef0123",
		"Fix the viewer", task.StatusRunning, created)
	if err := store.Tasks().CreateTask(ctx, tk); err != nil {
		t.Fatalf("CreateTask returned an error: %v", err)
	}

	renamed := at(2026, time.September, 20, 10)
	if err := store.Tasks().UpdateTaskTitle(ctx, tk.ID, "Fix the terminal viewer", renamed); err != nil {
		t.Fatalf("UpdateTaskTitle returned an error: %v", err)
	}

	stored, err := store.Tasks().GetTask(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetTask returned an error: %v", err)
	}
	if stored.Title != "Fix the terminal viewer" {
		t.Errorf("Title = %q, want the new title", stored.Title)
	}
	if stored.Status != task.StatusRunning {
		t.Errorf("Status = %q, want it untouched at %q", stored.Status, task.StatusRunning)
	}
	if !stored.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", stored.CreatedAt, created)
	}
	if !stored.UpdatedAt.Equal(renamed) {
		t.Errorf("UpdatedAt = %v, want %v", stored.UpdatedAt, renamed)
	}
}

func TestUpdateTaskTitleOnAMissingRow(t *testing.T) {
	store := newTestStore(t)

	err := store.Tasks().UpdateTaskTitle(context.Background(),
		"task_ffffffffffffffffffffffff", "anything", at(2026, time.September, 20, 9))
	if !errors.Is(err, task.ErrNotFound) {
		t.Errorf("UpdateTaskTitle on an unknown id returned %v, want task.ErrNotFound", err)
	}
}

// TestConcurrentStatusChangesApplyExactlyOnce drives the conditional update
// under real concurrency against real SQLite.
//
// The memory double asserts the same property, but it asserts it about a Go
// map. This runs the statement that actually decides it, on a real connection
// pool, where two writers really are two writers.
func TestConcurrentStatusChangesApplyExactlyOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")

	tk := testTask("task_0123456789abcdef01234567", "p_0123456789abcdef0123",
		"Fix the viewer", task.StatusRunning, at(2026, time.September, 20, 9))
	if err := store.Tasks().CreateTask(ctx, tk); err != nil {
		t.Fatalf("CreateTask returned an error: %v", err)
	}

	const workers = 16
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		applied  int
		conflict int
		other    []error
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			completed := at(2026, time.September, 20, 10)
			err := store.Tasks().UpdateTaskStatus(ctx, tk.ID, task.TaskStatusChange{
				From: task.StatusRunning, To: task.StatusCompleted,
				At: completed, CompletedAt: &completed,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				applied++
			case errors.Is(err, task.ErrStatusConflict):
				conflict++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(other) != 0 {
		t.Fatalf("updates failed for reasons that are not conflicts: %v", other)
	}
	if applied != 1 {
		t.Errorf("%d of %d concurrent updates applied; exactly one may", applied, workers)
	}
	if conflict != workers-1 {
		t.Errorf("%d updates were refused; want %d", conflict, workers-1)
	}

	stored, err := store.Tasks().GetTask(ctx, tk.ID)
	if err != nil {
		t.Fatalf("GetTask returned an error: %v", err)
	}
	if stored.Status != task.StatusCompleted || stored.CompletedAt == nil {
		t.Errorf("the task ended as %q with CompletedAt %v, want COMPLETED with a time",
			stored.Status, stored.CompletedAt)
	}
}

// ---------------------------------------------------------------------------
// Agent sessions
// ---------------------------------------------------------------------------

func createTaskForSession(t *testing.T, store *Store, taskID, projectID string) {
	t.Helper()
	tk := testTask(taskID, projectID, "Fix the viewer", task.StatusRunning, at(2026, time.September, 20, 9))
	if err := store.Tasks().CreateTask(context.Background(), tk); err != nil {
		t.Fatalf("CreateTask returned an error: %v", err)
	}
}

func TestCreateSessionStartsWithNoRuntime(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")
	createTaskForSession(t, store, "task_0123456789abcdef01234567", "p_0123456789abcdef0123")

	s := testSession("sess_0123456789abcdef01234567", "task_0123456789abcdef01234567",
		at(2026, time.September, 20, 10))
	if err := store.Tasks().CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession returned an error: %v", err)
	}

	stored, err := store.Tasks().GetSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetSession returned an error: %v", err)
	}
	if stored.RuntimeID != "" {
		t.Errorf("RuntimeID = %q, want empty for a session created before its runtime", stored.RuntimeID)
	}
	// An absent start is nil and not the zero time: a session that never ran has
	// no start time, and year 1 is a corrupted row.
	if stored.StartedAt != nil {
		t.Errorf("StartedAt = %v, want nil", stored.StartedAt)
	}
	if stored.EndedAt != nil {
		t.Errorf("EndedAt = %v, want nil", stored.EndedAt)
	}
	if stored.Status != task.StatusSessionCreated {
		t.Errorf("Status = %q, want %q", stored.Status, task.StatusSessionCreated)
	}
}

func TestGetSessionReportsAMissingRow(t *testing.T) {
	store := newTestStore(t)

	_, err := store.Tasks().GetSession(context.Background(), "sess_ffffffffffffffffffffffff")
	if !errors.Is(err, task.ErrSessionNotFound) {
		t.Errorf("GetSession on an unknown id returned %v, want task.ErrSessionNotFound", err)
	}
	// The two miss sentinels are different values, so a caller that confused a
	// task with a session is told which one was missing.
	if errors.Is(err, task.ErrNotFound) {
		t.Error("a missing session was reported as a missing task")
	}
}

func TestCreateSessionRejectsANilSession(t *testing.T) {
	store := newTestStore(t)

	if err := store.Tasks().CreateSession(context.Background(), nil); err == nil {
		t.Error("CreateSession(nil) succeeded, want a rejection")
	}
}

func TestSessionListIsScopedToItsTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")
	createTaskForSession(t, store, "task_00000000000000000000000a", "p_0123456789abcdef0123")
	createTaskForSession(t, store, "task_00000000000000000000000b", "p_0123456789abcdef0123")

	mine := testSession("sess_00000000000000000000000a", "task_00000000000000000000000a", at(2026, time.September, 20, 9))
	theirs := testSession("sess_00000000000000000000000b", "task_00000000000000000000000b", at(2026, time.September, 20, 10))
	for _, s := range []*task.AgentSession{mine, theirs} {
		if err := store.Tasks().CreateSession(ctx, s); err != nil {
			t.Fatalf("CreateSession returned an error: %v", err)
		}
	}

	got, err := store.Tasks().ListSessions(ctx, task.SessionQuery{TaskID: "task_00000000000000000000000a"})
	if err != nil {
		t.Fatalf("ListSessions returned an error: %v", err)
	}
	if ids := sessionIDs(got); len(ids) != 1 || ids[0] != mine.ID {
		t.Errorf("ListSessions returned %v, want only %s", ids, mine.ID)
	}
}

// TestTwoAttemptsAtOneTask is §六's diagram, at the layer that stores it: one
// task, two sessions, neither replacing the other.
func TestTwoAttemptsAtOneTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")
	createTaskForSession(t, store, "task_0123456789abcdef01234567", "p_0123456789abcdef0123")

	first := testSession("sess_000000000000000000000001", "task_0123456789abcdef01234567", at(2026, time.September, 20, 9))
	second := testSession("sess_000000000000000000000002", "task_0123456789abcdef01234567", at(2026, time.September, 20, 10))
	for _, s := range []*task.AgentSession{first, second} {
		if err := store.Tasks().CreateSession(ctx, s); err != nil {
			t.Fatalf("CreateSession returned an error: %v", err)
		}
	}

	got, err := store.Tasks().ListSessions(ctx, task.SessionQuery{TaskID: "task_0123456789abcdef01234567"})
	if err != nil {
		t.Fatalf("ListSessions returned an error: %v", err)
	}
	want := []string{second.ID, first.ID} // newest first
	if ids := sessionIDs(got); strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ListSessions returned %v, want %v", ids, want)
	}
}

func TestSessionListRefusesToGuessAScope(t *testing.T) {
	store := newTestStore(t)

	for _, id := range []string{"", "   "} {
		got, err := store.Tasks().ListSessions(context.Background(), task.SessionQuery{TaskID: id})
		if err == nil {
			t.Fatalf("ListSessions(TaskID: %q) returned %d rows, want a refusal", id, len(got))
		}
		if !task.IsCode(err, task.CodeInvalidInput) {
			t.Errorf("ListSessions(TaskID: %q) failed with %v, want %q", id, err, task.CodeInvalidInput)
		}
	}
}

// TestSessionStatusKeepsTheStartTimeItAlreadyHad is the COALESCE in
// UpdateSessionStatus, asserted: a session's start is a fact about the attempt
// and a later transition does not rewrite it.
func TestSessionStatusKeepsTheStartTimeItAlreadyHad(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")
	createTaskForSession(t, store, "task_0123456789abcdef01234567", "p_0123456789abcdef0123")

	s := testSession("sess_0123456789abcdef01234567", "task_0123456789abcdef01234567", at(2026, time.September, 20, 10))
	if err := store.Tasks().CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession returned an error: %v", err)
	}

	started := at(2026, time.September, 20, 11)
	if err := store.Tasks().UpdateSessionStatus(ctx, s.ID, task.SessionStatusChange{
		From: task.StatusSessionCreated, To: task.StatusSessionRunning, StartedAt: &started,
	}); err != nil {
		t.Fatalf("starting the session returned an error: %v", err)
	}

	ended := at(2026, time.September, 20, 12)
	if err := store.Tasks().UpdateSessionStatus(ctx, s.ID, task.SessionStatusChange{
		From: task.StatusSessionRunning, To: task.StatusSessionCompleted, EndedAt: &ended,
	}); err != nil {
		t.Fatalf("completing the session returned an error: %v", err)
	}

	stored, err := store.Tasks().GetSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetSession returned an error: %v", err)
	}
	if stored.StartedAt == nil || !stored.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want the original %v", stored.StartedAt, started)
	}
	if stored.EndedAt == nil || !stored.EndedAt.Equal(ended) {
		t.Errorf("EndedAt = %v, want %v", stored.EndedAt, ended)
	}
}

// TestSessionEndTimeIsClearedWhenItRunsAgain records the asymmetry the store
// documents: started_at coalesces, ended_at is assigned.
func TestSessionEndTimeIsClearedWhenItRunsAgain(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")
	createTaskForSession(t, store, "task_0123456789abcdef01234567", "p_0123456789abcdef0123")

	s := testSession("sess_0123456789abcdef01234567", "task_0123456789abcdef01234567", at(2026, time.September, 20, 10))
	if err := store.Tasks().CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession returned an error: %v", err)
	}

	ended := at(2026, time.September, 20, 11)
	if err := store.Tasks().UpdateSessionStatus(ctx, s.ID, task.SessionStatusChange{
		From: task.StatusSessionCreated, To: task.StatusSessionCancelled, EndedAt: &ended,
	}); err != nil {
		t.Fatalf("cancelling the session returned an error: %v", err)
	}
	// The store applies what it is given and does not check the lifecycle, so
	// this writes a transition the service would refuse. The property under
	// test is the column's behaviour, not the lifecycle's.
	if err := store.Tasks().UpdateSessionStatus(ctx, s.ID, task.SessionStatusChange{
		From: task.StatusSessionCancelled, To: task.StatusSessionRunning,
	}); err != nil {
		t.Fatalf("the second update returned an error: %v", err)
	}

	stored, err := store.Tasks().GetSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetSession returned an error: %v", err)
	}
	if stored.EndedAt != nil {
		t.Errorf("EndedAt = %v after the session started running again, want nil", stored.EndedAt)
	}
}

func TestSessionStatusRefusesAStaleFrom(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")
	createTaskForSession(t, store, "task_0123456789abcdef01234567", "p_0123456789abcdef0123")

	s := testSession("sess_0123456789abcdef01234567", "task_0123456789abcdef01234567", at(2026, time.September, 20, 10))
	if err := store.Tasks().CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession returned an error: %v", err)
	}
	if err := store.Tasks().UpdateSessionStatus(ctx, s.ID, task.SessionStatusChange{
		From: task.StatusSessionCreated, To: task.StatusSessionRunning,
	}); err != nil {
		t.Fatalf("starting the session returned an error: %v", err)
	}

	err := store.Tasks().UpdateSessionStatus(ctx, s.ID, task.SessionStatusChange{
		From: task.StatusSessionCreated, To: task.StatusSessionCancelled,
	})
	if !errors.Is(err, task.ErrStatusConflict) {
		t.Errorf("the stale update returned %v, want task.ErrStatusConflict", err)
	}
}

func TestSessionStatusOnAMissingRow(t *testing.T) {
	store := newTestStore(t)

	err := store.Tasks().UpdateSessionStatus(context.Background(), "sess_ffffffffffffffffffffffff",
		task.SessionStatusChange{From: task.StatusSessionCreated, To: task.StatusSessionRunning})
	if !errors.Is(err, task.ErrSessionNotFound) {
		t.Errorf("UpdateSessionStatus on an unknown id returned %v, want task.ErrSessionNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// The runtime relationship
// ---------------------------------------------------------------------------

func TestAttachRuntimeBindsOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")
	createTaskForSession(t, store, "task_0123456789abcdef01234567", "p_0123456789abcdef0123")

	s := testSession("sess_0123456789abcdef01234567", "task_0123456789abcdef01234567", at(2026, time.September, 20, 10))
	if err := store.Tasks().CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession returned an error: %v", err)
	}

	const runtimeID = "amx-p_0123456789abcdef0123"
	if err := store.Tasks().AttachSessionRuntime(ctx, s.ID, runtimeID); err != nil {
		t.Fatalf("AttachSessionRuntime returned an error: %v", err)
	}
	stored, err := store.Tasks().GetSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetSession returned an error: %v", err)
	}
	if stored.RuntimeID != runtimeID {
		t.Errorf("RuntimeID = %q, want %q", stored.RuntimeID, runtimeID)
	}

	// A second attach loses, because the column answers "which runtime did this
	// attempt run in" and a field that can be rewritten stops answering it.
	err = store.Tasks().AttachSessionRuntime(ctx, s.ID, "amx-p_0123456789abcdef0124")
	if !errors.Is(err, task.ErrStatusConflict) {
		t.Fatalf("the second attach returned %v, want task.ErrStatusConflict", err)
	}
	stored, err = store.Tasks().GetSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetSession returned an error: %v", err)
	}
	if stored.RuntimeID != runtimeID {
		t.Errorf("RuntimeID = %q after a refused attach, want %q unchanged", stored.RuntimeID, runtimeID)
	}
}

func TestAttachRuntimeOnAMissingSession(t *testing.T) {
	store := newTestStore(t)

	err := store.Tasks().AttachSessionRuntime(context.Background(),
		"sess_ffffffffffffffffffffffff", "amx-p_0123456789abcdef0123")
	if !errors.Is(err, task.ErrSessionNotFound) {
		t.Errorf("attaching to an unknown session returned %v, want task.ErrSessionNotFound", err)
	}
}

// TestARuntimeThatNoLongerExistsIsStillRecorded is §十八, asserted against the
// schema.
//
// The session's runtime is deliberately not a foreign key. A runtime is
// destroyed and its row deleted; "this attempt happened, and this is the
// runtime it ran in" has to outlive the process it used. If runtime_id were a
// cascade, destroying the terminal would delete the record of the work.
func TestARuntimeThatNoLongerExistsIsStillRecorded(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")
	createTaskForSession(t, store, "task_0123456789abcdef01234567", "p_0123456789abcdef0123")

	const runtimeID = "amx-p_0123456789abcdef0123"
	if err := store.Runtimes().Save(ctx, session.Record{
		ProjectID: "p_0123456789abcdef0123",
		Backend:   "tmux",
		Session:   runtimeID,
		State:     session.StateRunning,
		Cols:      120,
		Rows:      30,
		CreatedAt: at(2026, time.September, 20, 9),
		UpdatedAt: at(2026, time.September, 20, 9),
	}); err != nil {
		t.Fatalf("saving the runtime returned an error: %v", err)
	}

	s := testSession("sess_0123456789abcdef01234567", "task_0123456789abcdef01234567", at(2026, time.September, 20, 10))
	if err := store.Tasks().CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession returned an error: %v", err)
	}
	if err := store.Tasks().AttachSessionRuntime(ctx, s.ID, runtimeID); err != nil {
		t.Fatalf("AttachSessionRuntime returned an error: %v", err)
	}

	// The runtime is destroyed.
	if err := store.Runtimes().Delete(ctx, "p_0123456789abcdef0123"); err != nil {
		t.Fatalf("deleting the runtime returned an error: %v", err)
	}
	if _, err := store.Runtimes().Get(ctx, "p_0123456789abcdef0123"); !errors.Is(err, session.ErrRecordNotFound) {
		t.Fatalf("the runtime row is still there after Delete: Get returned %v", err)
	}

	stored, err := store.Tasks().GetSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetSession after destroying the runtime returned an error: %v", err)
	}
	if stored.RuntimeID != runtimeID {
		t.Errorf("RuntimeID = %q after the runtime was destroyed, want %q",
			stored.RuntimeID, runtimeID)
	}
}

// TestASessionNeedsATaskThatExists checks the one foreign key the session
// table does declare.
func TestASessionNeedsATaskThatExists(t *testing.T) {
	store := newTestStore(t)

	err := store.Tasks().CreateSession(context.Background(),
		testSession("sess_0123456789abcdef01234567", "task_ffffffffffffffffffffffff", at(2026, time.September, 20, 10)))
	if err == nil {
		t.Fatal("a session against a task that does not exist was stored, want a rejection")
	}
	if !task.IsCode(err, task.CodeStorageFailure) {
		t.Errorf("the rejection carried %v, want %q", err, task.CodeStorageFailure)
	}
}

// TestATaskNeedsAProjectThatExists is the same check one level up. It also
// proves foreign keys are really enforced on this connection, which the DSN
// asks for and a test rather than a comment should confirm.
func TestATaskNeedsAProjectThatExists(t *testing.T) {
	store := newTestStore(t)

	err := store.Tasks().CreateTask(context.Background(),
		testTask("task_0123456789abcdef01234567", "p_ffffffffffffffffffff", "work", task.StatusCreated,
			at(2026, time.September, 20, 9)))
	if err == nil {
		t.Fatal("a task against a project that does not exist was stored, want a rejection")
	}
	if !task.IsCode(err, task.CodeStorageFailure) {
		t.Errorf("the rejection carried %v, want %q", err, task.CodeStorageFailure)
	}
}

// TestDeletingATaskTakesItsAttemptsButNotItsHistory is §十九, asserted against
// the schema.
//
// A task's sessions cascade with it: a session without its task is an attempt
// at nothing. What must not cascade is agent_events. The event log is an
// independent append-only history, keyed by project and runtime rather than by
// task, and the schema declares no foreign key from an event to a task - so
// removing the task cannot reach it. Phase 7.1's guarantee that a history
// cannot be deleted is not weakened by this phase.
//
// (An event about a project whose row is deleted does cascade, because
// agent_events declares that foreign key. That is Phase 7.1's decision, taken
// before this phase existed, and nothing here changes it. Nothing in this build
// deletes a project and no endpoint could.)
func TestDeletingATaskTakesItsAttemptsButNotItsHistory(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")
	registerProject(t, store, "p_0123456789abcdef0124")

	tk := testTask("task_0123456789abcdef01234567", "p_0123456789abcdef0123",
		"work", task.StatusRunning, at(2026, time.September, 20, 9))
	if err := store.Tasks().CreateTask(ctx, tk); err != nil {
		t.Fatalf("CreateTask returned an error: %v", err)
	}
	s := testSession("sess_0123456789abcdef01234567", tk.ID, at(2026, time.September, 20, 10))
	if err := store.Tasks().CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession returned an error: %v", err)
	}

	// A task belonging to the other project, which must survive.
	survivor := testTask("task_000000000000000000000002", "p_0123456789abcdef0124",
		"other work", task.StatusCreated, at(2026, time.September, 20, 9))
	if err := store.Tasks().CreateTask(ctx, survivor); err != nil {
		t.Fatalf("CreateTask returned an error: %v", err)
	}

	// The project's history, which is not this task's to remove.
	if err := store.Events().Create(ctx, eventForTest("evt_0123456789abcdef01234567", "p_0123456789abcdef0123")); err != nil {
		t.Fatalf("creating the event returned an error: %v", err)
	}
	if err := store.Events().Create(ctx, eventForTest("evt_000000000000000000000002", "p_0123456789abcdef0124")); err != nil {
		t.Fatalf("creating the event returned an error: %v", err)
	}

	if _, err := store.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, tk.ID); err != nil {
		t.Fatalf("deleting the task failed: %v", err)
	}

	if _, err := store.Tasks().GetTask(ctx, tk.ID); !errors.Is(err, task.ErrNotFound) {
		t.Errorf("the task survived its own deletion: %v", err)
	}
	if _, err := store.Tasks().GetSession(ctx, s.ID); !errors.Is(err, task.ErrSessionNotFound) {
		t.Errorf("the session survived its task: %v", err)
	}
	if _, err := store.Tasks().GetTask(ctx, survivor.ID); err != nil {
		t.Errorf("a task in another project did not survive: %v", err)
	}

	// The history is untouched. This is the assertion the whole test exists for.
	if got := countEvents(t, store, "p_0123456789abcdef0123"); got != 1 {
		t.Errorf("the event log holds %d events after its task was deleted, want 1", got)
	}
	if got := countEvents(t, store, "p_0123456789abcdef0124"); got != 1 {
		t.Errorf("the event log holds %d events for the other project, want 1", got)
	}
}

// TestDeletingASessionLeavesTheHistory is the other half of §十九.
func TestDeletingASessionLeavesTheHistory(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	registerProject(t, store, "p_0123456789abcdef0123")
	createTaskForSession(t, store, "task_0123456789abcdef01234567", "p_0123456789abcdef0123")

	tk, err := store.Tasks().GetTask(ctx, "task_0123456789abcdef01234567")
	if err != nil {
		t.Fatalf("GetTask returned an error: %v", err)
	}
	s := testSession("sess_0123456789abcdef01234567", tk.ID, at(2026, time.September, 20, 10))
	if err := store.Tasks().CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession returned an error: %v", err)
	}
	if err := store.Events().Create(ctx, eventForTest("evt_0123456789abcdef01234567", "p_0123456789abcdef0123")); err != nil {
		t.Fatalf("creating the event returned an error: %v", err)
	}

	// An attempt that failed and is being cleaned up must not take the record
	// of what happened with it.
	if _, err := store.db.ExecContext(ctx, `DELETE FROM agent_sessions WHERE id = ?`, s.ID); err != nil {
		t.Fatalf("deleting the session failed: %v", err)
	}

	if _, err := store.Tasks().GetSession(ctx, s.ID); !errors.Is(err, task.ErrSessionNotFound) {
		t.Errorf("the session survived its own deletion: %v", err)
	}
	if _, err := store.Tasks().GetTask(ctx, tk.ID); err != nil {
		t.Errorf("deleting an attempt deleted the task it was an attempt at: %v", err)
	}
	if got := countEvents(t, store, "p_0123456789abcdef0123"); got != 1 {
		t.Errorf("the event log holds %d events after a session was deleted, want 1", got)
	}
}

// eventForTest builds an event row in the shape the event store expects.
func eventForTest(id, projectID string) *event.AgentEvent {
	return &event.AgentEvent{
		ID:        id,
		ProjectID: projectID,
		RuntimeID: "amx-" + projectID,
		Type:      event.TypeRuntimeStarted,
		Source:    event.SourceSystem,
		CreatedAt: at(2026, time.September, 20, 9),
	}
}

// countEvents reports how many events the log holds for a project, which is the
// number every cascade assertion in this file is really about.
func countEvents(t *testing.T, store *Store, projectID string) int {
	t.Helper()
	got, err := store.Events().List(context.Background(), event.Query{ProjectID: projectID})
	if err != nil {
		t.Fatalf("listing events for %s returned an error: %v", projectID, err)
	}
	return len(got)
}

// ---------------------------------------------------------------------------
// The upgrade path
// ---------------------------------------------------------------------------

// TestMigrateUpgradesAPhase71Database is §四十二: a database at 0003 has to
// reach 0005 without losing what it holds.
//
// The store has no "migrate to version N" entry point and is not getting one
// for a test. Instead the first three migrations are applied here through the
// same embed the runner reads, recorded in schema_migrations the same way the
// runner records them, and then the real Migrate is called. What it must do is
// apply exactly the two new ones and leave the old tables alone.
func TestMigrateUpgradesAPhase71Database(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, OpenOptions{Path: newDatabasePath(t)})
	if err != nil {
		t.Fatalf("Open returned an error: %v", err)
	}
	defer store.Close()

	all, err := migrations.All()
	if err != nil {
		t.Fatalf("migrations.All returned an error: %v", err)
	}

	// Bring the database to Phase 7.1 by hand.
	if _, err := store.db.ExecContext(ctx, schemaMigrationsTable); err != nil {
		t.Fatalf("creating schema_migrations failed: %v", err)
	}
	var before []string
	for _, m := range all {
		if m.Version > 3 {
			break
		}
		if _, err := store.db.ExecContext(ctx, m.SQL); err != nil {
			t.Fatalf("applying %s failed: %v", m, err)
		}
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.Version, m.Name, formatTime(at(2026, time.September, 1, 0)),
		); err != nil {
			t.Fatalf("recording %s failed: %v", m, err)
		}
		before = append(before, m.String())
	}
	if len(before) != 3 {
		t.Fatalf("the database was brought to version %d, want 3", len(before))
	}

	// A Phase 7.1 database holds projects, runtime records and events.
	if err := store.Projects().Create(ctx, testProject("p_0123456789abcdef0123", "App", `D:\AI\Projects\App`)); err != nil {
		t.Fatalf("creating the project failed: %v", err)
	}
	if err := store.Events().Create(ctx, eventForTest("evt_0123456789abcdef01234567", "p_0123456789abcdef0123")); err != nil {
		t.Fatalf("creating the event failed: %v", err)
	}

	// The upgrade.
	result, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate returned an error: %v", err)
	}
	want := []string{"0004_tasks", "0005_agent_sessions"}
	if len(result.Applied) != len(want) {
		t.Fatalf("Migrate applied %v, want %v", result.Applied, want)
	}
	for i := range want {
		if result.Applied[i] != want[i] {
			t.Errorf("Migrate applied %q, want %q", result.Applied[i], want[i])
		}
	}
	if want := latestVersion(t); result.Version != want {
		t.Errorf("Version = %d, want %d", result.Version, want)
	}

	// The Phase 7.1 tables are intact and still readable.
	if _, err := store.Projects().GetByID(ctx, "p_0123456789abcdef0123"); err != nil {
		t.Errorf("the project did not survive the upgrade: %v", err)
	}
	if got := countEvents(t, store, "p_0123456789abcdef0123"); got != 1 {
		t.Errorf("the event log holds %d events after the upgrade, want 1", got)
	}

	// The new tables are usable, which is the point of the upgrade.
	tk := testTask("task_0123456789abcdef01234567", "p_0123456789abcdef0123",
		"work", task.StatusCreated, at(2026, time.September, 20, 9))
	if err := store.Tasks().CreateTask(ctx, tk); err != nil {
		t.Fatalf("the upgraded database cannot store a task: %v", err)
	}
	if _, err := store.Tasks().GetTask(ctx, tk.ID); err != nil {
		t.Errorf("the upgraded database cannot read a task: %v", err)
	}
}

// TestMigrateUpgradeIsIdempotent makes sure the upgrade can be run twice, which
// is what happens on every restart.
func TestMigrateUpgradeIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	second, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate returned an error: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Errorf("a second Migrate applied %v, want nothing", second.Applied)
	}
	if second.Version != latestVersion(t) {
		t.Errorf("Version = %d, want %d", second.Version, latestVersion(t))
	}
}

// TestTheNewIndexesArePresent records the indexes the migrations declare, since
// an index removed by a later edit is invisible until a listing is slow.
func TestTheNewIndexesArePresent(t *testing.T) {
	store := newTestStore(t)

	rows, err := store.db.Query(`SELECT name FROM sqlite_master WHERE type = 'index' ORDER BY name`)
	if err != nil {
		t.Fatalf("listing indexes failed: %v", err)
	}
	defer rows.Close()

	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning an index name failed: %v", err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the index list failed: %v", err)
	}

	for _, want := range []string{
		"idx_tasks_project", "idx_tasks_status", "idx_tasks_created",
		"idx_agent_sessions_task", "idx_agent_sessions_runtime",
		"idx_agent_sessions_status", "idx_agent_sessions_created",
	} {
		if !found[want] {
			t.Errorf("index %s is missing", want)
		}
	}
}

// newDatabasePath is newTestStore's path, for the one test that opens a store
// without migrating it.
func newDatabasePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "agentmux.db")
}
