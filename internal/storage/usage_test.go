package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/usage"
)

// These tests are about the table and the statements, not about the recorder.
//
// The recorder's job is to decide what may be written; the store's is to write
// it and to keep the shape the migration promised. The two claims worth pinning
// here are the schema's - three columns and no fourth - and that a stored event
// comes back as what it was.

var usageClock = time.Date(2026, time.September, 22, 12, 0, 0, 0, time.UTC)

func usageAt(seconds int) time.Time {
	return usageClock.Add(time.Duration(seconds) * time.Second)
}

func newUsageStore(t *testing.T) *UsageStore {
	t.Helper()
	return newTestStore(t).Usage()
}

// TestTheUsageTableHasExactlyThreeColumns is the schema half of the promise
// that no content is recorded.
//
// It is asserted over PRAGMA rather than over the Go struct, because the struct
// is what a later change would edit and the table is what a later change would
// also have to edit. A Detail column added to both would fail this test, which
// is the point: the promise is kept by there being nowhere to put a payload,
// and that is a claim about the database.
func TestTheUsageTableHasExactlyThreeColumns(t *testing.T) {
	store := newTestStore(t)

	rows, err := store.db.Query(`PRAGMA table_info(usage_events)`)
	if err != nil {
		t.Fatalf("could not read the table's columns: %v", err)
	}
	defer rows.Close()

	var columns []string
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal any
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
			t.Fatalf("could not scan a column: %v", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the columns failed: %v", err)
	}

	want := "id,event_type,created_at"
	if got := strings.Join(columns, ","); got != want {
		t.Errorf("usage_events columns = %s, want %s", got, want)
	}
}

// TestStoringAndReadingBackAUsageEvent round-trips one row.
func TestStoringAndReadingBackAUsageEvent(t *testing.T) {
	store := newUsageStore(t)
	ctx := context.Background()

	if err := store.Insert(ctx, usage.Event{
		ID:        "use_0123456789abcdef",
		Type:      usage.EventDashboardOpen,
		CreatedAt: usageAt(0),
	}); err != nil {
		t.Fatalf("Insert returned an error: %v", err)
	}

	events, err := store.List(ctx, 10)
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("List returned %d events, want 1", len(events))
	}
	got := events[0]
	if got.ID != "use_0123456789abcdef" {
		t.Errorf("ID = %q", got.ID)
	}
	if got.Type != usage.EventDashboardOpen {
		t.Errorf("Type = %q, want %q", got.Type, usage.EventDashboardOpen)
	}
	if !got.CreatedAt.Equal(usageAt(0)) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, usageAt(0))
	}
}

// TestUsageEventsComeBackNewestFirst pins the one ordering, which is the one
// the table's only index exists for.
func TestUsageEventsComeBackNewestFirst(t *testing.T) {
	store := newUsageStore(t)
	ctx := context.Background()

	// Inserted oldest first, so a store that returned insertion order would
	// pass a test that only checked the set.
	for i, eventType := range []usage.EventType{
		usage.EventDashboardOpen,
		usage.EventTerminalConnect,
		usage.EventControllerRequest,
	} {
		if err := store.Insert(ctx, usage.Event{
			ID:        "use_" + string(rune('a'+i)) + "000000000000000",
			Type:      eventType,
			CreatedAt: usageAt(i),
		}); err != nil {
			t.Fatalf("Insert returned an error: %v", err)
		}
	}

	events, err := store.List(ctx, 10)
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	want := []usage.EventType{
		usage.EventControllerRequest,
		usage.EventTerminalConnect,
		usage.EventDashboardOpen,
	}
	if len(events) != len(want) {
		t.Fatalf("List returned %d events, want %d", len(events), len(want))
	}
	for i := range want {
		if events[i].Type != want[i] {
			t.Errorf("events[%d].Type = %q, want %q", i, events[i].Type, want[i])
		}
	}
}

// TestCountingUsageEvents covers the read the recorder's own tests use.
func TestCountingUsageEvents(t *testing.T) {
	store := newUsageStore(t)
	ctx := context.Background()

	count, err := store.Count(ctx)
	if err != nil {
		t.Fatalf("Count returned an error: %v", err)
	}
	if count != 0 {
		t.Errorf("Count = %d on an empty table, want 0", count)
	}

	for i := 0; i < 3; i++ {
		if err := store.Insert(ctx, usage.Event{
			ID:        "use_" + string(rune('a'+i)) + "000000000000000",
			Type:      usage.EventActionView,
			CreatedAt: usageAt(i),
		}); err != nil {
			t.Fatalf("Insert returned an error: %v", err)
		}
	}

	if count, err = store.Count(ctx); err != nil {
		t.Fatalf("Count returned an error: %v", err)
	} else if count != 3 {
		t.Errorf("Count = %d, want 3", count)
	}
}

// TestListingWithNoLimitReturnsNothing pins the guard on a table that has no
// bound of its own.
func TestListingWithNoLimitReturnsNothing(t *testing.T) {
	store := newUsageStore(t)
	ctx := context.Background()

	if err := store.Insert(ctx, usage.Event{
		ID: "use_0123456789abcdef", Type: usage.EventActionView, CreatedAt: usageAt(0),
	}); err != nil {
		t.Fatalf("Insert returned an error: %v", err)
	}

	for _, limit := range []int{0, -1} {
		events, err := store.List(ctx, limit)
		if err != nil {
			t.Fatalf("List(%d) returned an error: %v", limit, err)
		}
		if len(events) != 0 {
			t.Errorf("List(%d) returned %d events, want none", limit, len(events))
		}
	}
}
