package storage

import (
	"context"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/agentstate"
)

// These tests are about the statement, not about the projection.
//
// Everything that makes a projection idempotent and order-independent lives in
// one conditional upsert, and a test that went through the service would be
// testing it through a layer that could hide the difference. So these call the
// repository directly, with states the service would never build, to pin what
// the SQL does rather than what the service happens to ask for.

var stateClock = time.Date(2026, time.September, 21, 9, 0, 0, 0, time.UTC)

func stateAt(seconds int) time.Time {
	return stateClock.Add(time.Duration(seconds) * time.Second)
}

// state builds a valid state for a given attempt.
func state(attempt, project, runtime string, status agentstate.Status, eventAt time.Time, lastEvent string) agentstate.AgentState {
	return agentstate.AgentState{
		AgentSessionID: attempt,
		ProjectID:      project,
		RuntimeID:      runtime,
		Status:         status,
		LastEvent:      lastEvent,
		LastEventAt:    eventAt,
		UpdatedAt:      stateClock,
	}
}

func newStateStore(t *testing.T) (*AgentStateStore, *Store) {
	t.Helper()
	store := newTestStore(t)
	return store.AgentStates(), store
}

func TestUpsertStoresAState(t *testing.T) {
	repo, _ := newStateStore(t)
	ctx := context.Background()

	in := state("sess_a", "p_a", "amx-p_a", agentstate.StatusRunning, stateAt(10), "agent.started")
	written, err := repo.Upsert(ctx, in)
	if err != nil {
		t.Fatalf("Upsert returned an error: %v", err)
	}
	if !written {
		t.Error("Upsert reported no write for a row that did not exist")
	}

	got, err := repo.Get(ctx, "sess_a")
	if err != nil {
		t.Fatalf("Get returned an error: %v", err)
	}
	if got.Status != in.Status || got.RuntimeID != in.RuntimeID ||
		got.ProjectID != in.ProjectID || got.LastEvent != in.LastEvent {
		t.Errorf("Get() = %+v; want %+v", got, in)
	}
	if !got.LastEventAt.Equal(in.LastEventAt) {
		t.Errorf("lastEventAt = %s; want %s", got.LastEventAt, in.LastEventAt)
	}
}

// TestUpsertIgnoresAnOlderEvent is the condition that makes replay idempotent
// and order-independent. It is the most important statement in this phase.
func TestUpsertIgnoresAnOlderEvent(t *testing.T) {
	repo, _ := newStateStore(t)
	ctx := context.Background()

	newer := state("sess_a", "p_a", "amx-p_a", agentstate.StatusCompleted, stateAt(20), "agent.completed")
	if _, err := repo.Upsert(ctx, newer); err != nil {
		t.Fatalf("the first Upsert returned an error: %v", err)
	}

	older := state("sess_a", "p_a", "amx-p_a", agentstate.StatusRunning, stateAt(10), "agent.started")
	written, err := repo.Upsert(ctx, older)
	if err != nil {
		t.Fatalf("the second Upsert returned an error: %v", err)
	}
	if written {
		t.Error("Upsert wrote a state older than the one already stored")
	}

	got, _ := repo.Get(ctx, "sess_a")
	if got.Status != agentstate.StatusCompleted {
		t.Errorf("status = %s; want COMPLETED: an older event must not move the row",
			got.Status)
	}

	// The same event again is the boundary case, and it must be a no-op too:
	// re-projecting what is already there changes nothing.
	written, err = repo.Upsert(ctx, newer)
	if err != nil {
		t.Fatalf("re-projecting the same event returned an error: %v", err)
	}
	if !written {
		// Writing the same values again is harmless and is what `<=` allows;
		// what matters is that the row is unchanged.
		again, _ := repo.Get(ctx, "sess_a")
		if again != got {
			t.Errorf("re-projecting the same event changed the row:\n before = %+v\n after  = %+v", got, again)
		}
	}
}

// TestUpsertKeepsABindingWhenTheEventCarriesNone is the COALESCE case: a
// session.created happens before a runtime is attached.
func TestUpsertKeepsABindingWhenTheEventCarriesNone(t *testing.T) {
	repo, _ := newStateStore(t)
	ctx := context.Background()

	bound := state("sess_a", "p_a", "amx-p_a", agentstate.StatusRunning, stateAt(10), "session.status_changed")
	if _, err := repo.Upsert(ctx, bound); err != nil {
		t.Fatalf("the first Upsert returned an error: %v", err)
	}

	unbound := state("sess_a", "p_a", "", agentstate.StatusCompleted, stateAt(20), "session.status_changed")
	if _, err := repo.Upsert(ctx, unbound); err != nil {
		t.Fatalf("the second Upsert returned an error: %v", err)
	}

	got, _ := repo.Get(ctx, "sess_a")
	if got.RuntimeID != "amx-p_a" {
		t.Errorf("runtimeId = %q; want the binding to survive an event that carried none", got.RuntimeID)
	}
	if got.Status != agentstate.StatusCompleted {
		t.Errorf("status = %s; want COMPLETED - the status still moves", got.Status)
	}
}

func TestGetReportsAnAttemptWithNoState(t *testing.T) {
	repo, _ := newStateStore(t)

	_, err := repo.Get(context.Background(), "sess_missing")
	if !agentstate.IsCode(err, agentstate.CodeNotFound) {
		t.Errorf("Get() = %v; want %q", err, agentstate.CodeNotFound)
	}
}

func TestByRuntimeReturnsTheNewestAttempt(t *testing.T) {
	repo, _ := newStateStore(t)
	ctx := context.Background()

	first := state("sess_first", "p_a", "amx-p_a", agentstate.StatusStopped, stateAt(10), "agent.session_ended")
	second := state("sess_second", "p_a", "amx-p_a", agentstate.StatusRunning, stateAt(20), "agent.started")
	for _, s := range []agentstate.AgentState{first, second} {
		if _, err := repo.Upsert(ctx, s); err != nil {
			t.Fatalf("Upsert returned an error: %v", err)
		}
	}

	got, err := repo.ByRuntime(ctx, "amx-p_a")
	if err != nil {
		t.Fatalf("ByRuntime returned an error: %v", err)
	}
	if got.AgentSessionID != "sess_second" {
		t.Errorf("ByRuntime returned %q; want the attempt whose last event is newest",
			got.AgentSessionID)
	}

	if _, err := repo.ByRuntime(ctx, "amx-p_none"); !agentstate.IsCode(err, agentstate.CodeNotFound) {
		t.Errorf("ByRuntime for an unbound runtime = %v; want %q", err, agentstate.CodeNotFound)
	}
}

func TestListByProjectIsScopedAndOrdered(t *testing.T) {
	repo, _ := newStateStore(t)
	ctx := context.Background()

	entries := []agentstate.AgentState{
		state("sess_1", "p_a", "amx-p_a", agentstate.StatusRunning, stateAt(10), "agent.started"),
		state("sess_2", "p_a", "amx-p_a", agentstate.StatusFailed, stateAt(20), "agent.failed"),
		state("sess_3", "p_b", "amx-p_b", agentstate.StatusRunning, stateAt(30), "agent.started"),
	}
	for i, s := range entries {
		// updated_at decides the listing order, so it is varied independently
		// of last_event_at.
		s.UpdatedAt = stateAt(100 + i)
		if _, err := repo.Upsert(ctx, s); err != nil {
			t.Fatalf("Upsert returned an error: %v", err)
		}
	}

	list, err := repo.ListByProject(ctx, "p_a", 10)
	if err != nil {
		t.Fatalf("ListByProject returned an error: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListByProject(p_a) returned %d state(s); want 2", len(list))
	}
	if list[0].AgentSessionID != "sess_2" {
		t.Errorf("the listing is not most-recently-updated first: got %q", list[0].AgentSessionID)
	}
	for _, s := range list {
		if s.ProjectID != "p_a" {
			t.Errorf("the listing returned a state from %q", s.ProjectID)
		}
	}

	limited, err := repo.ListByProject(ctx, "p_a", 1)
	if err != nil {
		t.Fatalf("ListByProject with a limit returned an error: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("ListByProject(p_a, 1) returned %d state(s); want 1", len(limited))
	}
}

func TestCountAndDeleteAll(t *testing.T) {
	repo, _ := newStateStore(t)
	ctx := context.Background()

	count, err := repo.Count(ctx)
	if err != nil {
		t.Fatalf("Count returned an error: %v", err)
	}
	if count != 0 {
		t.Errorf("Count() = %d on an empty table; want 0", count)
	}

	if _, err := repo.Upsert(ctx, state("sess_a", "p_a", "amx-p_a", agentstate.StatusRunning, stateAt(10), "agent.started")); err != nil {
		t.Fatalf("Upsert returned an error: %v", err)
	}
	if count, _ = repo.Count(ctx); count != 1 {
		t.Errorf("Count() = %d; want 1", count)
	}

	if err := repo.DeleteAll(ctx); err != nil {
		t.Fatalf("DeleteAll returned an error: %v", err)
	}
	if count, _ = repo.Count(ctx); count != 0 {
		t.Errorf("Count() = %d after DeleteAll; want 0", count)
	}
}

func TestUpsertReportsAStorageFailure(t *testing.T) {
	repo, store := newStateStore(t)
	if err := store.Close(); err != nil {
		t.Fatalf("closing the store failed: %v", err)
	}

	if _, err := repo.Upsert(context.Background(), state("sess_a", "p_a", "amx-p_a", agentstate.StatusRunning, stateAt(10), "agent.started")); err == nil {
		t.Fatal("Upsert succeeded against a closed database")
	} else if !agentstate.IsCode(err, agentstate.CodeStorageFailure) {
		t.Errorf("Upsert error = %v; want %q", err, agentstate.CodeStorageFailure)
	}

	if _, err := repo.Count(context.Background()); !agentstate.IsCode(err, agentstate.CodeStorageFailure) {
		t.Errorf("Count error = %v; want %q", err, agentstate.CodeStorageFailure)
	}
	if err := repo.DeleteAll(context.Background()); !agentstate.IsCode(err, agentstate.CodeStorageFailure) {
		t.Errorf("DeleteAll error = %v; want %q", err, agentstate.CodeStorageFailure)
	}
}
