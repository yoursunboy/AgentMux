package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Memory repository
//
// The service tests run against an in-memory Repository rather than SQLite, so
// that a failure here is a failure of the service's rules and never of a
// statement. The SQL is exercised against a real database in
// internal/storage/events_test.go, and the two suites deliberately do not
// overlap.
//
// The fake reproduces the parts of the contract the service depends on: newest
// first, the (created_at, id) ordering pair, the page limit, and ErrNotFound
// from ResolveCursor.
type memoryRepo struct {
	mu     sync.Mutex
	events []*AgentEvent

	// createErr, when set, is returned instead of storing.
	createErr error

	// listErr, when set, is returned by List.
	listErr error

	// listCalls records the queries List was asked, so a test can assert the
	// limit the service asked storage for.
	listCalls []Query
}

var _ Repository = (*memoryRepo)(nil)

func (r *memoryRepo) Create(_ context.Context, ev *AgentEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return r.createErr
	}
	// Store a copy: the service hands the caller the same pointer, and a test
	// that mutated one would otherwise mutate what was "stored" too.
	stored := *ev
	r.events = append(r.events, &stored)
	return nil
}

func (r *memoryRepo) List(_ context.Context, query Query) ([]*AgentEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listCalls = append(r.listCalls, query)
	if r.listErr != nil {
		return nil, r.listErr
	}

	matched := make([]*AgentEvent, 0, len(r.events))
	for _, ev := range r.events {
		switch {
		case query.ProjectID != "" && ev.ProjectID != query.ProjectID:
			continue
		case query.RuntimeID != "" && ev.RuntimeID != query.RuntimeID:
			continue
		case query.ProjectID == "" && query.RuntimeID == "":
			return nil, errors.New("memoryRepo: query names neither a project nor a runtime")
		}
		matched = append(matched, ev)
	}

	// Newest first, on the same pair the SQL orders by. The second comparison is
	// what makes the order total, and a page walk depends on its being total.
	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})

	if query.Before != nil {
		after := make([]*AgentEvent, 0, len(matched))
		for _, ev := range matched {
			if ev.CreatedAt.Before(query.Before.CreatedAt) ||
				(ev.CreatedAt.Equal(query.Before.CreatedAt) && ev.ID < query.Before.ID) {
				after = append(after, ev)
			}
		}
		matched = after
	}

	if limit := query.LimitOr(); len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}

func (r *memoryRepo) ResolveCursor(_ context.Context, id string) (Cursor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.ID == id {
			return Cursor{CreatedAt: ev.CreatedAt, ID: ev.ID}, nil
		}
	}
	return Cursor{}, ErrNotFound
}

// countingIDs hands out identifiers in a fixed sequence rather than at random,
// so that a test which deliberately produces events in the same instant knows
// what order they will sort into.
type countingIDs struct {
	mu   sync.Mutex
	next int
	id   string
	err  error
}

func (c *countingIDs) generate() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return "", c.err
	}
	if c.id != "" {
		return c.id, nil
	}
	c.next++
	// Counting down, so each identifier is smaller than the one before it. The
	// ordering is `id DESC`, so this makes the order the events were recorded in
	// the reverse of the order they sort into - which is exactly the case a
	// timestamp-only ordering gets wrong, and the one worth pinning.
	return fmt.Sprintf("%s%032x", IDPrefix, 1<<20-c.next), nil
}

// harness is a service wired to a fake repository and a clock a test controls.
type harness struct {
	service *Service
	repo    *memoryRepo
	ids     *countingIDs
	now     time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	repo := &memoryRepo{}
	ids := &countingIDs{}
	h := &harness{
		repo: repo,
		ids:  ids,
		now:  time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC),
	}
	service, err := NewService(Options{
		Repository: repo,
		NewID:      ids.generate,
		Now:        func() time.Time { return h.now },
		// A logger that discards, so a test run is not a wall of "event created".
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h.service = service
	return h
}

// record stores one event and fails the test if it cannot.
func (h *harness) record(t *testing.T, projectID, runtimeID, eventType, source string, payload string) *AgentEvent {
	t.Helper()
	ev, err := h.service.CreateEvent(context.Background(), projectID, runtimeID, eventType, source, rawPayload(payload))
	if err != nil {
		t.Fatalf("CreateEvent(%s, %s): %v", projectID, eventType, err)
	}
	return ev
}

// tick advances the clock, so an event recorded after it is strictly newer.
func (h *harness) tick(d time.Duration) { h.now = h.now.Add(d) }

func rawPayload(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// TestCreateEventStoresTheEvent is the "验证数据库存在" case: what CreateEvent
// returns is what the repository holds, field for field.
func TestCreateEventStoresTheEvent(t *testing.T) {
	h := newHarness(t)
	payload := `{"state":"running","cols":120,"rows":30}`

	created := h.record(t, "p_abc", "amx-p_abc-1234567890", TypeRuntimeStarted, SourceRuntime, payload)

	if !ValidID(created.ID) {
		t.Errorf("CreateEvent returned id %q, which is not an event id", created.ID)
	}
	if created.ProjectID != "p_abc" {
		t.Errorf("ProjectID = %q, want %q", created.ProjectID, "p_abc")
	}
	if created.RuntimeID != "amx-p_abc-1234567890" {
		t.Errorf("RuntimeID = %q, want the runtime it was given", created.RuntimeID)
	}
	if created.Type != TypeRuntimeStarted {
		t.Errorf("Type = %q, want %q", created.Type, TypeRuntimeStarted)
	}
	if created.Source != SourceRuntime {
		t.Errorf("Source = %q, want %q", created.Source, SourceRuntime)
	}
	if string(created.Payload) != payload {
		t.Errorf("Payload = %s, want %s", created.Payload, payload)
	}
	if !created.CreatedAt.Equal(h.now) {
		t.Errorf("CreatedAt = %s, want the injected clock %s", created.CreatedAt, h.now)
	}
	if created.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt location = %s, want UTC", created.CreatedAt.Location())
	}

	stored, err := h.repo.List(context.Background(), Query{ProjectID: "p_abc"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d events, want 1", len(stored))
	}
	if stored[0].ID != created.ID || stored[0].Type != created.Type {
		t.Errorf("stored event = %+v, want the one CreateEvent returned", stored[0])
	}
}

// TestCreateEventTimestampIsUTC pins the storage rule: whatever zone the clock
// is in, what is written is UTC.
func TestCreateEventTimestampIsUTC(t *testing.T) {
	h := newHarness(t)
	zone := time.FixedZone("UTC+8", 8*60*60)
	h.now = time.Date(2026, time.September, 20, 17, 0, 0, 0, zone)

	ev := h.record(t, "p_abc", "", TypeRuntimeStarted, SourceRuntime, "")

	if ev.CreatedAt.Location() != time.UTC {
		t.Fatalf("CreatedAt location = %s, want UTC", ev.CreatedAt.Location())
	}
	if hour := ev.CreatedAt.Hour(); hour != 9 {
		t.Errorf("CreatedAt hour = %d, want 9 - 17:00+08:00 is 09:00 UTC", hour)
	}
}

// TestCreateEventAllowsProjectLevelEvents covers the empty runtime id, which is
// what anything the API produces looks like.
func TestCreateEventAllowsProjectLevelEvents(t *testing.T) {
	h := newHarness(t)

	// The type is one of the runtime ones on purpose: the model does not couple
	// a type to a scope, and a project-level event with no runtime is the
	// ordinary shape rather than a special case.
	ev := h.record(t, "p_abc", "", TypeRuntimeStarted, SourceUser, "")

	if ev.RuntimeID != "" {
		t.Errorf("RuntimeID = %q, want empty", ev.RuntimeID)
	}
}

func TestCreateEventRejectsMalformedInput(t *testing.T) {
	validID := func() (string, error) {
		id, err := NewID()
		return id, err
	}

	cases := []struct {
		name      string
		projectID string
		runtimeID string
		eventType string
		source    string
		payload   string
	}{
		{name: "no project", projectID: "", eventType: "runtime.started", source: "runtime"},
		{name: "blank project", projectID: "   ", eventType: "runtime.started", source: "runtime"},
		{name: "project id too long", projectID: strings.Repeat("p", 129), eventType: "runtime.started", source: "runtime"},
		{name: "runtime id too long", projectID: "p_abc", runtimeID: strings.Repeat("r", 129), eventType: "runtime.started", source: "runtime"},
		{name: "type is not dotted", projectID: "p_abc", eventType: "started", source: "runtime"},
		{name: "type has no action", projectID: "p_abc", eventType: "runtime.", source: "runtime"},
		{name: "type is uppercase", projectID: "p_abc", eventType: "Runtime.Started", source: "runtime"},
		{name: "type has a space", projectID: "p_abc", eventType: "runtime started", source: "runtime"},
		{name: "type is a status", projectID: "p_abc", eventType: "waiting", source: "runtime"},
		{name: "unknown source", projectID: "p_abc", eventType: "runtime.started", source: "claude"},
		{name: "empty source", projectID: "p_abc", eventType: "runtime.started", source: ""},
		{name: "payload is an array", projectID: "p_abc", eventType: "runtime.started", source: "runtime", payload: `[1,2]`},
		{name: "payload is a string", projectID: "p_abc", eventType: "runtime.started", source: "runtime", payload: `"hello"`},
		{name: "payload is not JSON", projectID: "p_abc", eventType: "runtime.started", source: "runtime", payload: `{`},
		{name: "payload names a token", projectID: "p_abc", eventType: "runtime.started", source: "runtime", payload: `{"token":"abc"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, err := NewService(Options{Repository: &memoryRepo{}, NewID: validID})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			_, err = service.CreateEvent(context.Background(), tc.projectID, tc.runtimeID,
				tc.eventType, tc.source, rawPayload(tc.payload))
			if err == nil {
				t.Fatal("CreateEvent accepted the event, want a rejection")
			}
			if code := CodeOf(err); code != CodeInvalidEvent {
				t.Errorf("code = %q, want %q (%v)", code, CodeInvalidEvent, err)
			}
		})
	}
}

// TestCreateEventRejectsNothingWasStored is the other half of a rejection: a
// refused event must not reach the repository.
func TestCreateEventRejectsNothingWasStored(t *testing.T) {
	h := newHarness(t)

	if _, err := h.service.CreateEvent(context.Background(), "p_abc", "",
		"not a type", SourceRuntime, nil); err == nil {
		t.Fatal("CreateEvent accepted a malformed type")
	}
	stored, err := h.repo.List(context.Background(), Query{ProjectID: "p_abc"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("a rejected event was stored: %+v", stored)
	}
}

func TestCreateEventReportsStorageFailure(t *testing.T) {
	h := newHarness(t)
	h.repo.createErr = errors.New("disk on fire")

	_, err := h.service.CreateEvent(context.Background(), "p_abc", "",
		TypeRuntimeStarted, SourceRuntime, nil)
	if err == nil {
		t.Fatal("CreateEvent swallowed a storage failure")
	}
	if code := CodeOf(err); code != CodeStorageFailure {
		t.Errorf("code = %q, want %q (%v)", code, CodeStorageFailure, err)
	}
}

func TestNewServiceRequiresARepository(t *testing.T) {
	if _, err := NewService(Options{}); err == nil {
		t.Fatal("NewService accepted a nil repository")
	}
}

// ---------------------------------------------------------------------------
// Query
// ---------------------------------------------------------------------------

// TestListProjectReturnsOnlyThatProject covers both of §十三's query cases at
// once: a project timeline contains that project's events and no others.
func TestListProjectReturnsOnlyThatProject(t *testing.T) {
	h := newHarness(t)
	h.record(t, "p_a", "amx-p_a-1", TypeRuntimeStarted, SourceRuntime, "")
	h.tick(time.Second)
	h.record(t, "p_b", "amx-p_b-1", TypeRuntimeStarted, SourceRuntime, "")
	h.tick(time.Second)
	h.record(t, "p_a", "amx-p_a-1", TypeRuntimeStopped, SourceRuntime, "")

	page, err := h.service.ListProject(context.Background(), "p_a", ListOptions{})
	if err != nil {
		t.Fatalf("ListProject: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(page.Events))
	}
	for _, ev := range page.Events {
		if ev.ProjectID != "p_a" {
			t.Errorf("project A's timeline contains an event for %q", ev.ProjectID)
		}
	}
}

// TestListOrdersNewestFirst is §十三's "验证排序".
func TestListOrdersNewestFirst(t *testing.T) {
	h := newHarness(t)
	h.record(t, "p_a", "", TypeRuntimeStarted, SourceRuntime, "")
	h.tick(time.Second)
	h.record(t, "p_a", "", TypeRuntimeStopped, SourceRuntime, "")
	h.tick(time.Second)
	h.record(t, "p_a", "", TypeRuntimeDestroyed, SourceRuntime, "")

	page, err := h.service.ListProject(context.Background(), "p_a", ListOptions{})
	if err != nil {
		t.Fatalf("ListProject: %v", err)
	}
	got := typesOf(page.Events)
	want := []string{TypeRuntimeDestroyed, TypeRuntimeStopped, TypeRuntimeStarted}
	if !equalStrings(got, want) {
		t.Fatalf("order = %v, want %v (newest first)", got, want)
	}
	for i := 1; i < len(page.Events); i++ {
		if page.Events[i-1].CreatedAt.Before(page.Events[i].CreatedAt) {
			t.Fatalf("event %d is newer than event %d, want descending", i-1, i)
		}
	}
}

// TestListOrdersEventsInTheSameInstant pins the tiebreak. A runtime start
// writes two events in one instant as a matter of course, so the ordering key
// has to be the (created_at, id) pair rather than the timestamp alone - with
// ties, a page walk either skips rows or repeats them.
func TestListOrdersEventsInTheSameInstant(t *testing.T) {
	h := newHarness(t)
	// No tick between them: both events carry exactly the same timestamp.
	first := h.record(t, "p_a", "", TypeRuntimeStarted, SourceRuntime, "")
	second := h.record(t, "p_a", "", TypeRuntimeStarted, SourceRuntime, "")

	if !first.CreatedAt.Equal(second.CreatedAt) {
		t.Fatalf("the fixture did not produce a tie: %s vs %s", first.CreatedAt, second.CreatedAt)
	}

	page, err := h.service.ListProject(context.Background(), "p_a", ListOptions{})
	if err != nil {
		t.Fatalf("ListProject: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(page.Events))
	}
	// Both carry the same timestamp, so only the id can order them - and the
	// order is `id DESC`, the same direction as the timestamp. The point of the
	// assertion is not which of the two comes first, it is that reading twice
	// gives the same answer; a timestamp-only ordering would leave that to the
	// query planner.
	if page.Events[0].ID < page.Events[1].ID {
		t.Fatalf("order = [%s %s], want descending ids so the order is total",
			page.Events[0].ID, page.Events[1].ID)
	}

	again, err := h.service.ListProject(context.Background(), "p_a", ListOptions{})
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if again.Events[0].ID != page.Events[0].ID || again.Events[1].ID != page.Events[1].ID {
		t.Fatalf("two reads of the same timeline disagreed: [%s %s] then [%s %s]",
			page.Events[0].ID, page.Events[1].ID, again.Events[0].ID, again.Events[1].ID)
	}
}

func TestListRuntimeReturnsOnlyThatRuntime(t *testing.T) {
	h := newHarness(t)
	h.record(t, "p_a", "amx-p_a-1", TypeRuntimeStarted, SourceRuntime, "")
	h.tick(time.Second)
	h.record(t, "p_a", "amx-p_a-2", TypeRuntimeStarted, SourceRuntime, "")
	h.tick(time.Second)
	h.record(t, "p_b", "amx-p_a-1", TypeRuntimeStarted, SourceRuntime, "")

	page, err := h.service.ListRuntime(context.Background(), "amx-p_a-1", ListOptions{})
	if err != nil {
		t.Fatalf("ListRuntime: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(page.Events))
	}
	for _, ev := range page.Events {
		if ev.RuntimeID != "amx-p_a-1" {
			t.Errorf("runtime timeline contains an event for %q", ev.RuntimeID)
		}
	}
}

// TestListEmptyTimelineIsAnEmptyList covers the shape a client renders: not
// null, so it needs no special case.
func TestListEmptyTimelineIsAnEmptyList(t *testing.T) {
	h := newHarness(t)

	page, err := h.service.ListProject(context.Background(), "p_empty", ListOptions{})
	if err != nil {
		t.Fatalf("ListProject: %v", err)
	}
	if page.Events == nil {
		t.Fatal("Events is nil, want an empty slice")
	}
	if len(page.Events) != 0 {
		t.Fatalf("got %d events, want 0", len(page.Events))
	}
	if page.NextBefore != "" {
		t.Errorf("NextBefore = %q, want empty on the last page", page.NextBefore)
	}
}

func TestListRequiresAScope(t *testing.T) {
	h := newHarness(t)

	if _, err := h.service.ListProject(context.Background(), "  ", ListOptions{}); err == nil {
		t.Error("ListProject accepted a blank project id")
	}
	if _, err := h.service.ListRuntime(context.Background(), "", ListOptions{}); err == nil {
		t.Error("ListRuntime accepted a blank runtime id")
	}
}

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

// TestListDefaultsToFifty is §十二's default.
func TestListDefaultsToFifty(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 60; i++ {
		h.record(t, "p_a", "", TypeRuntimeStarted, SourceRuntime, "")
		h.tick(time.Millisecond)
	}

	page, err := h.service.ListProject(context.Background(), "p_a", ListOptions{})
	if err != nil {
		t.Fatalf("ListProject: %v", err)
	}
	if len(page.Events) != DefaultLimit {
		t.Fatalf("got %d events, want the default %d", len(page.Events), DefaultLimit)
	}
	if page.NextBefore == "" {
		t.Fatal("NextBefore is empty, want a cursor for the next page")
	}
	// The service asks storage for one past the page, and never returns it.
	if got := h.repo.listCalls[len(h.repo.listCalls)-1].Limit; got != DefaultLimit+1 {
		t.Errorf("storage was asked for %d rows, want %d", got, DefaultLimit+1)
	}
}

// TestListHonoursLimit is §十三's "验证 limit".
func TestListHonoursLimit(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 10; i++ {
		h.record(t, "p_a", "", TypeRuntimeStarted, SourceRuntime, "")
		h.tick(time.Millisecond)
	}

	page, err := h.service.ListProject(context.Background(), "p_a", ListOptions{Limit: 3})
	if err != nil {
		t.Fatalf("ListProject: %v", err)
	}
	if len(page.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(page.Events))
	}
	if page.NextBefore != page.Events[2].ID {
		t.Errorf("NextBefore = %q, want the last event handed out (%q)", page.NextBefore, page.Events[2].ID)
	}
}

// TestListClampsToTheMaximum is §十二's ceiling: a client asking for a thousand
// gets 200, not a thousand.
func TestListClampsToTheMaximum(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < MaxLimit+10; i++ {
		h.record(t, "p_a", "", TypeRuntimeStarted, SourceRuntime, "")
		h.tick(time.Microsecond)
	}

	page, err := h.service.ListProject(context.Background(), "p_a", ListOptions{Limit: 1000})
	if err != nil {
		t.Fatalf("ListProject: %v", err)
	}
	if len(page.Events) != MaxLimit {
		t.Fatalf("got %d events, want the maximum %d", len(page.Events), MaxLimit)
	}
}

func TestEffectiveLimit(t *testing.T) {
	cases := []struct{ requested, want int }{
		{0, DefaultLimit},
		{-1, DefaultLimit},
		{1, 1},
		{DefaultLimit, DefaultLimit},
		{MaxLimit, MaxLimit},
		{MaxLimit + 1, MaxLimit},
		{1 << 20, MaxLimit},
	}
	for _, tc := range cases {
		if got := EffectiveLimit(tc.requested); got != tc.want {
			t.Errorf("EffectiveLimit(%d) = %d, want %d", tc.requested, got, tc.want)
		}
	}
}

// TestPaginationWalksTheWholeTimelineWithoutGapsOrRepeats is the property the
// cursor exists for. It runs against a timeline whose events share timestamps,
// because that is the case a timestamp-only cursor gets wrong.
func TestPaginationWalksTheWholeTimelineWithoutGapsOrRepeats(t *testing.T) {
	h := newHarness(t)

	// Three events per iteration, and the tick lands between the first and the
	// rest - so every instant after the first carries three events that tie on
	// the ordering key's first half. That is the case a timestamp-only cursor
	// gets wrong, and it is why the walk below asks for seven at a time: pages
	// that do not line up with the ties.
	const iterations = 25
	for i := 0; i < iterations; i++ {
		h.record(t, "p_a", "", TypeRuntimeStarted, SourceRuntime, "")
		h.tick(time.Millisecond)
		h.record(t, "p_a", "", TypeRuntimeStopped, SourceRuntime, "")
		h.record(t, "p_a", "", TypeRuntimeStarted, SourceRuntime, "")
	}
	want := iterations * 3

	var (
		seen     []string
		before   string
		pages    int
		lastTime time.Time
	)
	for {
		page, err := h.service.ListProject(context.Background(), "p_a",
			ListOptions{Limit: 7, Before: before})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		if pages > 100 {
			t.Fatal("pagination did not terminate")
		}
		for _, ev := range page.Events {
			seen = append(seen, ev.ID)
			if !lastTime.IsZero() && ev.CreatedAt.After(lastTime) {
				t.Fatalf("event %s is newer than the one before it: the walk went backwards", ev.ID)
			}
			lastTime = ev.CreatedAt
		}
		if page.NextBefore == "" {
			break
		}
		before = page.NextBefore
	}

	if len(seen) != want {
		t.Fatalf("walked %d events over %d pages, want %d", len(seen), pages, want)
	}
	unique := make(map[string]bool, len(seen))
	for _, id := range seen {
		if unique[id] {
			t.Fatalf("event %s appeared on two pages", id)
		}
		unique[id] = true
	}
}

// TestPaginationReportsTheLastPage pins the cursor's absence: a page with
// nothing after it carries no nextBefore, so a client stops without an extra
// request that would come back empty.
func TestPaginationReportsTheLastPage(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		h.record(t, "p_a", "", TypeRuntimeStarted, SourceRuntime, "")
		h.tick(time.Millisecond)
	}

	first, err := h.service.ListProject(context.Background(), "p_a", ListOptions{Limit: 3})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if first.NextBefore == "" {
		t.Fatal("first page has no cursor, want one")
	}

	second, err := h.service.ListProject(context.Background(), "p_a",
		ListOptions{Limit: 3, Before: first.NextBefore})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Events) != 2 {
		t.Fatalf("second page has %d events, want the remaining 2", len(second.Events))
	}
	if second.NextBefore != "" {
		t.Errorf("NextBefore = %q on the last page, want empty", second.NextBefore)
	}
}

// TestUnlimitedPageHasNoCursor covers the exactly-full case: a page that
// happens to hold every remaining event must not offer a cursor, because
// following it would return an empty page.
func TestUnlimitedPageHasNoCursor(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 4; i++ {
		h.record(t, "p_a", "", TypeRuntimeStarted, SourceRuntime, "")
		h.tick(time.Millisecond)
	}

	page, err := h.service.ListProject(context.Background(), "p_a", ListOptions{Limit: 4})
	if err != nil {
		t.Fatalf("ListProject: %v", err)
	}
	if len(page.Events) != 4 {
		t.Fatalf("got %d events, want 4", len(page.Events))
	}
	if page.NextBefore != "" {
		t.Errorf("NextBefore = %q on an exactly-full page, want empty", page.NextBefore)
	}
}

func TestUnknownCursorIsNotFound(t *testing.T) {
	h := newHarness(t)
	h.record(t, "p_a", "", TypeRuntimeStarted, SourceRuntime, "")

	missing, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	_, err = h.service.ListProject(context.Background(), "p_a", ListOptions{Before: missing})
	if err == nil {
		t.Fatal("ListProject accepted a cursor that names no event")
	}
	if code := CodeOf(err); code != CodeNotFound {
		t.Errorf("code = %q, want %q (%v)", code, CodeNotFound, err)
	}
}

// ---------------------------------------------------------------------------
// Isolation
// ---------------------------------------------------------------------------

// TestProjectTimelinesAreIsolated is §十三's isolation case: project B's events
// never appear in project A's timeline, and a cursor into another project's
// history does not open a way around that.
func TestProjectTimelinesAreIsolated(t *testing.T) {
	h := newHarness(t)

	for i := 0; i < 3; i++ {
		h.record(t, "p_a", "amx-p_a-1", TypeRuntimeStarted, SourceRuntime, "")
		h.tick(time.Millisecond)
		h.record(t, "p_b", "amx-p_b-1", TypeRuntimeStarted, SourceRuntime, "")
		h.tick(time.Millisecond)
	}

	for _, projectID := range []string{"p_a", "p_b"} {
		page, err := h.service.ListProject(context.Background(), projectID, ListOptions{Limit: MaxLimit})
		if err != nil {
			t.Fatalf("ListProject(%s): %v", projectID, err)
		}
		if len(page.Events) != 3 {
			t.Fatalf("project %s has %d events, want its own 3", projectID, len(page.Events))
		}
		for _, ev := range page.Events {
			if ev.ProjectID != projectID {
				t.Errorf("project %s's timeline contains an event for %s", projectID, ev.ProjectID)
			}
		}
	}
}

// TestCursorFromAnotherProjectCannotWidenATimeline is the sharper half: even
// holding an id from project B, a read of project A returns only project A.
// The scope and the cursor are separate clauses of the same query, and neither
// replaces the other.
func TestCursorFromAnotherProjectCannotWidenATimeline(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 4; i++ {
		h.record(t, "p_a", "", TypeRuntimeStarted, SourceRuntime, "")
		h.tick(time.Millisecond)
		h.record(t, "p_b", "", TypeRuntimeStarted, SourceRuntime, "")
		h.tick(time.Millisecond)
	}

	bPage, err := h.service.ListProject(context.Background(), "p_b", ListOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ListProject(p_b): %v", err)
	}
	if len(bPage.Events) != 1 {
		t.Fatalf("got %d events for p_b, want 1", len(bPage.Events))
	}
	foreignCursor := bPage.Events[0].ID

	page, err := h.service.ListProject(context.Background(), "p_a",
		ListOptions{Limit: MaxLimit, Before: foreignCursor})
	if err != nil {
		t.Fatalf("ListProject(p_a, before=%s): %v", foreignCursor, err)
	}
	for _, ev := range page.Events {
		if ev.ProjectID != "p_a" {
			t.Fatalf("a cursor from project B let project %s's event into project A's timeline", ev.ProjectID)
		}
	}
}

// TestRuntimeTimelinesAreIsolated is the same rule on the other scope. It is
// worth its own test because the runtime scope is the one the bridge writes to,
// and a query that lost its WHERE clause on that path would look like a
// working timeline.
func TestRuntimeTimelinesAreIsolated(t *testing.T) {
	h := newHarness(t)
	h.record(t, "p_a", "amx-p_a-1", TypeRuntimeStarted, SourceRuntime, "")
	h.tick(time.Millisecond)
	h.record(t, "p_a", "amx-p_a-2", TypeRuntimeStarted, SourceRuntime, "")
	h.tick(time.Millisecond)
	h.record(t, "p_b", "amx-p_b-1", TypeRuntimeStarted, SourceRuntime, "")

	page, err := h.service.ListRuntime(context.Background(), "amx-p_a-1", ListOptions{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("ListRuntime: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(page.Events))
	}
	if page.Events[0].RuntimeID != "amx-p_a-1" {
		t.Fatalf("runtime timeline contains an event for %q", page.Events[0].RuntimeID)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func typesOf(events []*AgentEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Type)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
