package storage

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/event"
)

// The event store is exercised against a real SQLite file rather than a fake,
// because what is being tested here is the SQL: the ordering pair, the keyset
// comparison, the scope clause, and the foreign key. The service's rules are
// tested in internal/event, and the two suites deliberately do not overlap.

// eventTestStore is a migrated database holding one project, which is what the
// event table's foreign key requires before anything can be written.
func eventTestStore(t *testing.T) *Store {
	t.Helper()
	store := newTestStore(t)
	if err := store.Projects().Create(context.Background(),
		testProject("p_1", "App", "/data/app")); err != nil {
		t.Fatalf("creating the fixture project failed: %v", err)
	}
	return store
}

// newEvent builds an event with a distinct identifier, so a test can name one
// without spelling out 36 characters.
func newEvent(t *testing.T, projectID, runtimeID, eventType string, createdAt time.Time) *event.AgentEvent {
	t.Helper()
	id, err := event.NewID()
	if err != nil {
		t.Fatalf("event.NewID failed: %v", err)
	}
	return &event.AgentEvent{
		ID:        id,
		ProjectID: projectID,
		RuntimeID: runtimeID,
		Type:      eventType,
		Source:    event.SourceRuntime,
		CreatedAt: createdAt,
	}
}

func mustCreateEvent(t *testing.T, store *Store, ev *event.AgentEvent) *event.AgentEvent {
	t.Helper()
	if err := store.Events().Create(context.Background(), ev); err != nil {
		t.Fatalf("Create(%s) returned an error: %v", ev.Type, err)
	}
	return ev
}

// TestEventStoreSatisfiesTheRepositoryContract makes the compile-time
// assertion in events.go visible in the suite as well.
func TestEventStoreSatisfiesTheRepositoryContract(t *testing.T) {
	var store *EventStore = newTestStore(t).Events()
	var _ event.Repository = store
}

// TestCreateEventWritesARow is §十三's "验证数据库存在": the row is read back
// with a query of its own rather than through the repository, so a Create that
// silently did nothing cannot pass.
func TestCreateEventWritesARow(t *testing.T) {
	store := eventTestStore(t)
	when := at(2026, time.September, 20, 9)

	ev := newEvent(t, "p_1", "amx-p_1-1234567890", event.TypeRuntimeStarted, when)
	ev.Payload = json.RawMessage(`{"state":"running","cols":120,"rows":30}`)
	mustCreateEvent(t, store, ev)

	var (
		id, projectID, runtimeID, eventType, source, payload, createdAt string
	)
	err := store.db.QueryRow(`
		SELECT id, project_id, runtime_id, type, source, payload, created_at
		FROM agent_events WHERE id = ?`, ev.ID).
		Scan(&id, &projectID, &runtimeID, &eventType, &source, &payload, &createdAt)
	if err != nil {
		t.Fatalf("reading the stored row back failed: %v", err)
	}

	if id != ev.ID {
		t.Errorf("id = %q, want %q", id, ev.ID)
	}
	if projectID != "p_1" {
		t.Errorf("project_id = %q, want %q", projectID, "p_1")
	}
	if runtimeID != "amx-p_1-1234567890" {
		t.Errorf("runtime_id = %q, want the runtime it was given", runtimeID)
	}
	if eventType != event.TypeRuntimeStarted {
		t.Errorf("type = %q, want %q", eventType, event.TypeRuntimeStarted)
	}
	if source != event.SourceRuntime {
		t.Errorf("source = %q, want %q", source, event.SourceRuntime)
	}
	if payload != `{"state":"running","cols":120,"rows":30}` {
		t.Errorf("payload = %q, want what was stored", payload)
	}
	// Stored as UTC text, so the database file stays readable with any SQLite
	// tool instead of holding an opaque integer.
	if createdAt != "2026-09-20T09:00:00Z" {
		t.Errorf("created_at = %q, want %q", createdAt, "2026-09-20T09:00:00Z")
	}
}

// TestEventRoundTripsEveryColumn covers the two nullable columns, which are the
// ones a scan can get wrong without anything failing loudly.
func TestEventRoundTripsEveryColumn(t *testing.T) {
	store := eventTestStore(t)
	ctx := context.Background()
	when := at(2026, time.September, 20, 9)

	withRuntime := newEvent(t, "p_1", "amx-p_1-1", event.TypeRuntimeStarted, when)
	withRuntime.Payload = json.RawMessage(`{"state":"running"}`)
	mustCreateEvent(t, store, withRuntime)

	// A project-level event: no runtime, no payload.
	projectLevel := newEvent(t, "p_1", "", event.TypeRuntimeStarted, when.Add(time.Minute))
	mustCreateEvent(t, store, projectLevel)

	got, err := store.Events().List(ctx, event.Query{ProjectID: "p_1"})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List returned %d events, want 2", len(got))
	}
	// Newest first, so the project-level event is first.
	if got[0].ID != projectLevel.ID {
		t.Fatalf("List returned %s first, want the newer %s", got[0].ID, projectLevel.ID)
	}
	if got[0].RuntimeID != "" {
		t.Errorf("a stored NULL runtime_id read back as %q, want empty", got[0].RuntimeID)
	}
	if got[0].Payload != nil {
		t.Errorf("a stored NULL payload read back as %q, want nil", got[0].Payload)
	}

	if got[1].RuntimeID != "amx-p_1-1" {
		t.Errorf("RuntimeID = %q, want %q", got[1].RuntimeID, "amx-p_1-1")
	}
	if string(got[1].Payload) != `{"state":"running"}` {
		t.Errorf("Payload = %q, want what was stored", got[1].Payload)
	}
	if !got[1].CreatedAt.Equal(withRuntime.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got[1].CreatedAt, withRuntime.CreatedAt)
	}
	if !got[1].CreatedAt.Equal(got[1].CreatedAt.UTC()) {
		t.Error("a stored timestamp read back in a non-UTC zone")
	}
}

// TestEventRequiresAnExistingProject is the foreign key, which is the one thing
// the table asserts about its own contents: an event about a project that does
// not exist cannot be written.
func TestEventRequiresAnExistingProject(t *testing.T) {
	store := newTestStore(t) // migrated, with no projects in it

	err := store.Events().Create(context.Background(),
		newEvent(t, "p_missing", "", event.TypeRuntimeStarted, at(2026, time.September, 20, 9)))
	if err == nil {
		t.Fatal("an event was written for a project that does not exist")
	}
	if !event.IsCode(err, event.CodeStorageFailure) {
		t.Errorf("the failure carried %q, want %q: %v",
			event.CodeOf(err), event.CodeStorageFailure, err)
	}
}

// TestListOrdersByTheCreatedAtIDPair is the ordering key itself. Windows clock
// resolution is a millisecond or coarser and a runtime start writes two events
// in one instant, so a timestamp alone is not a total order - and a page walk
// over a non-total order either skips rows or repeats them.
func TestListOrdersByTheCreatedAtIDPair(t *testing.T) {
	store := eventTestStore(t)
	ctx := context.Background()
	when := at(2026, time.September, 20, 9)

	// Three events in the same instant, with ids chosen so that the expected
	// order is unambiguous rather than accidental.
	for _, id := range []string{
		"evt_00000000000000000000000000000003",
		"evt_00000000000000000000000000000001",
		"evt_00000000000000000000000000000002",
	} {
		ev := newEvent(t, "p_1", "", event.TypeRuntimeStarted, when)
		ev.ID = id
		mustCreateEvent(t, store, ev)
	}

	got, err := store.Events().List(ctx, event.Query{ProjectID: "p_1"})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	want := []string{
		"evt_00000000000000000000000000000003",
		"evt_00000000000000000000000000000002",
		"evt_00000000000000000000000000000001",
	}
	if len(got) != len(want) {
		t.Fatalf("List returned %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("order = %s, want %s (created_at DESC, id DESC)",
				idsOf(got), strings.Join(want, " "))
		}
	}
}

// TestListKeysetPaginationOverTies is the case the pair exists for: pages of a
// timeline whose events share timestamps. Every event must appear exactly once
// across the walk.
func TestListKeysetPaginationOverTies(t *testing.T) {
	store := eventTestStore(t)
	ctx := context.Background()
	when := at(2026, time.September, 20, 9)

	const perInstant = 5
	const instants = 6
	for i := 0; i < instants; i++ {
		for j := 0; j < perInstant; j++ {
			mustCreateEvent(t, store,
				newEvent(t, "p_1", "", event.TypeRuntimeStarted, when.Add(time.Duration(i)*time.Millisecond)))
		}
	}

	var (
		seen   []string
		before *event.Cursor
		pages  int
	)
	for {
		query := event.Query{ProjectID: "p_1", Limit: 3, Before: before}
		page, err := store.Events().List(ctx, query)
		if err != nil {
			t.Fatalf("page %d returned an error: %v", pages, err)
		}
		pages++
		if pages > 100 {
			t.Fatal("the walk did not terminate")
		}
		if len(page) == 0 {
			break
		}
		for _, ev := range page {
			seen = append(seen, ev.ID)
		}
		last := page[len(page)-1]
		before = &event.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
		if len(page) < 3 {
			break
		}
	}

	if len(seen) != perInstant*instants {
		t.Fatalf("the walk returned %d events, want %d", len(seen), perInstant*instants)
	}
	unique := make(map[string]bool, len(seen))
	for _, id := range seen {
		if unique[id] {
			t.Fatalf("event %s appeared twice in the walk", id)
		}
		unique[id] = true
	}
}

func TestListScopeIsExclusive(t *testing.T) {
	store := eventTestStore(t)
	ctx := context.Background()
	when := at(2026, time.September, 20, 9)

	mustCreateEvent(t, store, newEvent(t, "p_1", "amx-p_1-1", event.TypeRuntimeStarted, when))
	mustCreateEvent(t, store, newEvent(t, "p_1", "amx-p_1-2", event.TypeRuntimeStarted, when.Add(time.Second)))

	byProject, err := store.Events().List(ctx, event.Query{ProjectID: "p_1"})
	if err != nil {
		t.Fatalf("List by project returned an error: %v", err)
	}
	if len(byProject) != 2 {
		t.Errorf("the project timeline has %d events, want both", len(byProject))
	}

	byRuntime, err := store.Events().List(ctx, event.Query{RuntimeID: "amx-p_1-1"})
	if err != nil {
		t.Fatalf("List by runtime returned an error: %v", err)
	}
	if len(byRuntime) != 1 {
		t.Fatalf("the runtime timeline has %d events, want 1", len(byRuntime))
	}
	if byRuntime[0].RuntimeID != "amx-p_1-1" {
		t.Errorf("the runtime timeline contains an event for %q", byRuntime[0].RuntimeID)
	}
}

// TestListRefusesAnUnscopedQuery records the deliberate refusal. The one answer
// that would hide the caller's bug is every event AgentMux has ever recorded,
// and an installation only accumulates more of them.
func TestListRefusesAnUnscopedQuery(t *testing.T) {
	store := eventTestStore(t)

	got, err := store.Events().List(context.Background(), event.Query{})
	if err == nil {
		t.Fatalf("an unscoped List returned %d events, want a refusal", len(got))
	}
	if !event.IsCode(err, event.CodeInvalidEvent) {
		t.Errorf("the refusal carried %q, want %q: %v",
			event.CodeOf(err), event.CodeInvalidEvent, err)
	}
}

func TestListAppliesTheLimit(t *testing.T) {
	store := eventTestStore(t)
	ctx := context.Background()
	when := at(2026, time.September, 20, 9)

	for i := 0; i < 10; i++ {
		mustCreateEvent(t, store,
			newEvent(t, "p_1", "", event.TypeRuntimeStarted, when.Add(time.Duration(i)*time.Second)))
	}

	for _, limit := range []int{1, 4, 10, 50} {
		got, err := store.Events().List(ctx, event.Query{ProjectID: "p_1", Limit: limit})
		if err != nil {
			t.Fatalf("List(limit=%d) returned an error: %v", limit, err)
		}
		want := limit
		if want > 10 {
			want = 10
		}
		if len(got) != want {
			t.Errorf("List(limit=%d) returned %d events, want %d", limit, len(got), want)
		}
	}
}

// TestListClampsAboveTheHardLimit covers the ceiling a repository applies, which
// is one above the ceiling a request may name.
func TestListClampsAboveTheHardLimit(t *testing.T) {
	store := eventTestStore(t)
	ctx := context.Background()
	when := at(2026, time.September, 20, 9)

	for i := 0; i < 3; i++ {
		mustCreateEvent(t, store,
			newEvent(t, "p_1", "", event.TypeRuntimeStarted, when.Add(time.Duration(i)*time.Second)))
	}

	// A query one past the caller's maximum must still be answered; it is what
	// the service asks for when it looks for a further page.
	got, err := store.Events().List(ctx, event.Query{ProjectID: "p_1", Limit: 10_000})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("List returned %d events, want all 3", len(got))
	}
}

func TestListWithoutALimitUsesTheDefault(t *testing.T) {
	store := eventTestStore(t)
	ctx := context.Background()
	when := at(2026, time.September, 20, 9)

	for i := 0; i < event.DefaultLimit+5; i++ {
		mustCreateEvent(t, store,
			newEvent(t, "p_1", "", event.TypeRuntimeStarted, when.Add(time.Duration(i)*time.Second)))
	}

	got, err := store.Events().List(ctx, event.Query{ProjectID: "p_1"})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(got) != event.DefaultLimit {
		t.Errorf("List returned %d events, want the default %d", len(got), event.DefaultLimit)
	}
}

func TestResolveCursor(t *testing.T) {
	store := eventTestStore(t)
	ctx := context.Background()
	when := at(2026, time.September, 20, 9)
	ev := mustCreateEvent(t, store, newEvent(t, "p_1", "", event.TypeRuntimeStarted, when))

	cursor, err := store.Events().ResolveCursor(ctx, ev.ID)
	if err != nil {
		t.Fatalf("ResolveCursor returned an error: %v", err)
	}
	if cursor.ID != ev.ID {
		t.Errorf("cursor.ID = %q, want %q", cursor.ID, ev.ID)
	}
	if !cursor.CreatedAt.Equal(ev.CreatedAt) {
		t.Errorf("cursor.CreatedAt = %v, want %v", cursor.CreatedAt, ev.CreatedAt)
	}

	// The sentinel, not a storage-specific error, so the service can decide what
	// a missing row means without knowing which database is behind it.
	missing, err := event.NewID()
	if err != nil {
		t.Fatalf("event.NewID failed: %v", err)
	}
	if _, err := store.Events().ResolveCursor(ctx, missing); !errors.Is(err, event.ErrNotFound) {
		t.Errorf("resolving an unknown id returned %v, want event.ErrNotFound", err)
	}
}

func TestCreateRejectsANilEvent(t *testing.T) {
	store := eventTestStore(t)

	if err := store.Events().Create(context.Background(), nil); err == nil {
		t.Fatal("Create(nil) succeeded, want a rejection")
	}
}

// TestEventIndexesExist pins the four indexes migration 0003 creates. They are
// in the schema for the queries the API actually runs, and an index that was
// dropped or renamed would otherwise only show up as a slow timeline on a large
// installation.
func TestEventIndexesExist(t *testing.T) {
	store := eventTestStore(t)

	names := map[string]bool{}
	rows, err := store.db.Query(`SELECT name FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'agent_events'`)
	if err != nil {
		t.Fatalf("could not list the indexes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("could not scan an index name: %v", err)
		}
		names[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the index list failed: %v", err)
	}

	for _, want := range []string{
		"idx_agent_events_project",
		"idx_agent_events_runtime",
		"idx_agent_events_created",
		"idx_agent_events_type",
	} {
		if !names[want] {
			t.Errorf("index %s is missing; found %v", want, sortedKeys(names))
		}
	}
}

// TestEventsSurviveReopening is the property the whole phase rests on: the
// history is on disk, not in a process's memory, so it outlives the runtime it
// describes and the server that recorded it.
func TestEventsSurviveReopening(t *testing.T) {
	path := t.TempDir() + "/agentmux.db"
	ctx := context.Background()

	first := newTestStoreAt(t, path)
	if err := first.Projects().Create(ctx, testProject("p_1", "App", "/data/app")); err != nil {
		t.Fatalf("creating the fixture project failed: %v", err)
	}
	ev := mustCreateEvent(t, first, newEvent(t, "p_1", "amx-p_1-1",
		event.TypeRuntimeStarted, at(2026, time.September, 20, 9)))
	if err := first.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}

	second, err := Open(ctx, OpenOptions{Path: path})
	if err != nil {
		t.Fatalf("reopening returned an error: %v", err)
	}
	defer second.Close()

	got, err := second.Events().List(ctx, event.Query{ProjectID: "p_1"})
	if err != nil {
		t.Fatalf("List after reopening returned an error: %v", err)
	}
	if len(got) != 1 || got[0].ID != ev.ID {
		t.Fatalf("after reopening, the timeline is %s, want the event that was written", idsOf(got))
	}
}

// TestDeletingAProjectRemovesItsEvents records the cascade. A project's history
// is about the project; keeping rows for one that no longer exists would leave
// a timeline nothing can be shown for.
func TestDeletingAProjectRemovesItsEvents(t *testing.T) {
	store := eventTestStore(t)
	ctx := context.Background()

	mustCreateEvent(t, store, newEvent(t, "p_1", "amx-p_1-1",
		event.TypeRuntimeStarted, at(2026, time.September, 20, 9)))

	if _, err := store.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, "p_1"); err != nil {
		t.Fatalf("deleting the project failed: %v", err)
	}

	var remaining int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_events WHERE project_id = ?`, "p_1").Scan(&remaining); err != nil {
		t.Fatalf("counting the remaining events failed: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d events survived the deletion of their project", remaining)
	}
}

// TestEventsAreAppendOnly is the storage half of the model's promise: a stored
// event is never changed, and the store offers no way to change one.
//
// The interface assertion is the compile-time half. The reflection below is
// what catches the mistake the interface would not: an extra method added to
// the concrete type and called directly from somewhere that holds a *EventStore.
func TestEventsAreAppendOnly(t *testing.T) {
	var store *EventStore = newTestStore(t).Events()
	var _ event.Repository = store

	typ := reflect.TypeOf(store)
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		for _, forbidden := range []string{"Update", "Delete", "Remove", "Set", "Insert", "Replace"} {
			if strings.HasPrefix(name, forbidden) {
				t.Errorf("*EventStore has a %s method; the event log is append-only", name)
			}
		}
	}
	// ResolveCursor is named explicitly so that a store which lost its reader
	// methods would not pass this test by having nothing left to inspect.
	if _, ok := typ.MethodByName("ResolveCursor"); !ok {
		t.Error("*EventStore has no ResolveCursor method")
	}
}

// TestNullableStringAndPayload covers the two conversions a scan round trip
// depends on.
func TestNullableStringAndPayload(t *testing.T) {
	if got := nullableString(""); got != nil {
		t.Errorf("nullableString(\"\") = %v, want nil", got)
	}
	if got := nullableString("x"); got != "x" {
		t.Errorf("nullableString(\"x\") = %v, want %q", got, "x")
	}
	if got := nullablePayload(nil); got != nil {
		t.Errorf("nullablePayload(nil) = %v, want nil", got)
	}
	if got := nullablePayload([]byte{}); got != nil {
		t.Errorf("nullablePayload(empty) = %v, want nil", got)
	}
	if got := nullablePayload([]byte(`{"a":1}`)); got != `{"a":1}` {
		t.Errorf("nullablePayload = %v, want the string form", got)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func idsOf(events []*event.AgentEvent) string {
	ids := make([]string, 0, len(events))
	for _, ev := range events {
		ids = append(ids, ev.ID)
	}
	return strings.Join(ids, " ")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
