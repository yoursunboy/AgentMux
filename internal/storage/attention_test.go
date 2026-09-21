package storage

import (
	"context"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/attention"
)

// These tests are about the statements, not about the projection.
//
// Everything that makes the attention projection order-independent lives in one
// conditional upsert, and everything that makes the queue idempotent lives in
// one `DO NOTHING`. A test that went through the service would be testing both
// through a layer that could hide the difference.

var attentionClock = time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)

func attentionAt(seconds int) time.Time {
	return attentionClock.Add(time.Duration(seconds) * time.Second)
}

func newAttentionStore(t *testing.T) *AttentionStore {
	t.Helper()
	return newTestStore(t).Attention()
}

// attentionRow builds a valid attention row.
func attentionRow(attempt, project string, level attention.Level, reason string, at time.Time) attention.Attention {
	return attention.Attention{
		AgentSessionID: attempt,
		ProjectID:      project,
		Level:          level,
		Reason:         reason,
		UpdatedAt:      at,
	}
}

// actionRow builds a valid pending action.
func actionRow(id, attempt, project string, actionType attention.ActionType, at time.Time) attention.Action {
	return attention.Action{
		ID:             id,
		AgentSessionID: attempt,
		ProjectID:      project,
		Type:           actionType,
		Status:         attention.ActionPending,
		Reason:         "permission requested",
		CreatedAt:      at,
	}
}

// anActionID builds an action id of the shape the projection derives.
func anActionID(seed string) string {
	for len(seed) < 32 {
		seed += "0"
	}
	return "act_" + seed[:32]
}

// ---------------------------------------------------------------------------
// Attention
// ---------------------------------------------------------------------------

func TestUpsertAttentionStoresALevel(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	in := attentionRow("sess_a", "p_a", attention.LevelActionRequired, "permission requested", attentionAt(10))
	written, err := repo.UpsertAttention(ctx, in)
	if err != nil {
		t.Fatalf("UpsertAttention returned an error: %v", err)
	}
	if !written {
		t.Error("UpsertAttention reported no write for a row that did not exist")
	}

	got, err := repo.Attention(ctx, "sess_a")
	if err != nil {
		t.Fatalf("Attention returned an error: %v", err)
	}
	if got.Level != in.Level || got.Reason != in.Reason || got.ProjectID != in.ProjectID {
		t.Errorf("Attention() = %+v; want %+v", got, in)
	}
	if !got.UpdatedAt.Equal(in.UpdatedAt) {
		t.Errorf("updatedAt = %s; want %s", got.UpdatedAt, in.UpdatedAt)
	}
}

// TestUpsertAttentionIgnoresAnOlderEvent is the guard that makes the projection
// order-independent and replayable.
func TestUpsertAttentionIgnoresAnOlderEvent(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	newer := attentionRow("sess_a", "p_a", attention.LevelWarning, "agent failed", attentionAt(20))
	if _, err := repo.UpsertAttention(ctx, newer); err != nil {
		t.Fatalf("the first UpsertAttention returned an error: %v", err)
	}

	older := attentionRow("sess_a", "p_a", attention.LevelNone, "agent started", attentionAt(10))
	written, err := repo.UpsertAttention(ctx, older)
	if err != nil {
		t.Fatalf("the second UpsertAttention returned an error: %v", err)
	}
	if written {
		t.Error("UpsertAttention wrote a level older than the one already stored")
	}

	got, _ := repo.Attention(ctx, "sess_a")
	if got.Level != attention.LevelWarning {
		t.Errorf("level = %s; want WARNING: an older event must not move the row", got.Level)
	}
}

func TestAttentionReportsAnAttemptWithNoRow(t *testing.T) {
	repo := newAttentionStore(t)

	_, err := repo.Attention(context.Background(), "sess_missing")
	if !attention.IsCode(err, attention.CodeNotFound) {
		t.Errorf("Attention() = %v; want %q", err, attention.CodeNotFound)
	}
}

func TestListAttentionByProjectIsScopedAndOrdered(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	entries := []attention.Attention{
		attentionRow("sess_1", "p_a", attention.LevelNone, "agent started", attentionAt(10)),
		attentionRow("sess_2", "p_a", attention.LevelWarning, "agent failed", attentionAt(20)),
		attentionRow("sess_3", "p_b", attention.LevelNone, "agent started", attentionAt(30)),
	}
	for _, a := range entries {
		if _, err := repo.UpsertAttention(ctx, a); err != nil {
			t.Fatalf("UpsertAttention returned an error: %v", err)
		}
	}

	list, err := repo.ListAttentionByProject(ctx, "p_a", 10)
	if err != nil {
		t.Fatalf("ListAttentionByProject returned an error: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListAttentionByProject(p_a) returned %d row(s); want 2", len(list))
	}
	if list[0].AgentSessionID != "sess_2" {
		t.Errorf("the listing is not most-recently-updated first: got %q", list[0].AgentSessionID)
	}
	for _, a := range list {
		if a.ProjectID != "p_a" {
			t.Errorf("the listing returned a row from %q", a.ProjectID)
		}
	}

	limited, err := repo.ListAttentionByProject(ctx, "p_a", 1)
	if err != nil {
		t.Fatalf("ListAttentionByProject with a limit returned an error: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("ListAttentionByProject(p_a, 1) returned %d row(s); want 1", len(limited))
	}
}

func TestAttentionCountAndDeleteAll(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	if n, err := repo.CountAttention(ctx); err != nil || n != 0 {
		t.Errorf("CountAttention() = %d, %v on an empty table; want 0, nil", n, err)
	}
	if _, err := repo.UpsertAttention(ctx, attentionRow("sess_a", "p_a", attention.LevelNone, "agent started", attentionAt(10))); err != nil {
		t.Fatalf("UpsertAttention returned an error: %v", err)
	}
	if n, _ := repo.CountAttention(ctx); n != 1 {
		t.Errorf("CountAttention() = %d; want 1", n)
	}
	if err := repo.DeleteAllAttention(ctx); err != nil {
		t.Fatalf("DeleteAllAttention returned an error: %v", err)
	}
	if n, _ := repo.CountAttention(ctx); n != 0 {
		t.Errorf("CountAttention() = %d after DeleteAllAttention; want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

// TestRaiseActionIsIdempotent is the whole of why the action id is derived from
// its event.
func TestRaiseActionIsIdempotent(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	id := anActionID("aaaa")
	in := actionRow(id, "sess_a", "p_a", attention.ActionPermissionRequest, attentionAt(10))

	raised, err := repo.RaiseAction(ctx, in)
	if err != nil {
		t.Fatalf("RaiseAction returned an error: %v", err)
	}
	if !raised {
		t.Error("RaiseAction reported no write for an action that did not exist")
	}

	for i := 0; i < 3; i++ {
		again, err := repo.RaiseAction(ctx, in)
		if err != nil {
			t.Fatalf("raises #%d returned an error: %v", i+2, err)
		}
		if again {
			t.Errorf("raise #%d wrote a second row for the same event", i+2)
		}
	}

	list, err := repo.Actions(ctx, "sess_a")
	if err != nil {
		t.Fatalf("Actions returned an error: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("the attempt has %d action(s); want 1", len(list))
	}
}

// TestReRaisingDoesNotResetAResolvedAction is what `DO NOTHING` buys over an
// upsert: a replayed event must not put a settled action back into the queue.
func TestReRaisingDoesNotResetAResolvedAction(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	id := anActionID("bbbb")
	in := actionRow(id, "sess_a", "p_a", attention.ActionPermissionRequest, attentionAt(10))
	if _, err := repo.RaiseAction(ctx, in); err != nil {
		t.Fatalf("RaiseAction returned an error: %v", err)
	}
	if _, err := repo.ResolveActions(ctx, "sess_a", []attention.ActionType{attention.ActionPermissionRequest}, attentionAt(20)); err != nil {
		t.Fatalf("ResolveActions returned an error: %v", err)
	}

	if _, err := repo.RaiseAction(ctx, in); err != nil {
		t.Fatalf("the second RaiseAction returned an error: %v", err)
	}

	list, _ := repo.Actions(ctx, "sess_a")
	if list[0].Status != attention.ActionResolved {
		t.Errorf("status = %s after a re-raise; want RESOLVED - a replayed event must not "+
			"put a settled action back into the queue", list[0].Status)
	}
}

// TestListActionsByProjectPutsPendingFirst is the ordering that makes the list
// a queue rather than a log.
func TestListActionsByProjectPutsPendingFirst(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	// The oldest action is the one still waiting.
	old := actionRow(anActionID("cccc"), "sess_a", "p_a", attention.ActionPermissionRequest, attentionAt(10))
	newer := actionRow(anActionID("dddd"), "sess_a", "p_a", attention.ActionViewCompletion, attentionAt(20))
	other := actionRow(anActionID("eeee"), "sess_b", "p_b", attention.ActionViewFailure, attentionAt(30))

	for _, a := range []attention.Action{old, newer, other} {
		if _, err := repo.RaiseAction(ctx, a); err != nil {
			t.Fatalf("RaiseAction returned an error: %v", err)
		}
	}
	// Settle the newer one, so the listing has to reorder.
	if _, err := repo.ResolveActions(ctx, "sess_a", []attention.ActionType{attention.ActionViewCompletion}, attentionAt(40)); err != nil {
		t.Fatalf("ResolveActions returned an error: %v", err)
	}

	list, err := repo.ListActionsByProject(ctx, "p_a", 10)
	if err != nil {
		t.Fatalf("ListActionsByProject returned an error: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListActionsByProject(p_a) returned %d action(s); want 2", len(list))
	}
	if list[0].ID != old.ID || list[0].Status != attention.ActionPending {
		t.Errorf("the queue leads with %+v; want the pending action, which is the older one", list[0])
	}
	if list[1].Status != attention.ActionResolved {
		t.Errorf("the settled action is %s; want RESOLVED", list[1].Status)
	}
	for _, a := range list {
		if a.ProjectID != "p_a" {
			t.Errorf("the listing returned an action from %q", a.ProjectID)
		}
	}
}

// TestResolveActionsOnlyTouchesPendingOnesOfTheGivenType is the statement the
// settle rule rests on.
func TestResolveActionsOnlyTouchesPendingOnesOfTheGivenType(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	permission := actionRow(anActionID("f001"), "sess_a", "p_a", attention.ActionPermissionRequest, attentionAt(10))
	failure := actionRow(anActionID("f002"), "sess_a", "p_a", attention.ActionViewFailure, attentionAt(11))
	elsewhere := actionRow(anActionID("f003"), "sess_b", "p_a", attention.ActionPermissionRequest, attentionAt(12))
	for _, a := range []attention.Action{permission, failure, elsewhere} {
		if _, err := repo.RaiseAction(ctx, a); err != nil {
			t.Fatalf("RaiseAction returned an error: %v", err)
		}
	}

	n, err := repo.ResolveActions(ctx, "sess_a", []attention.ActionType{attention.ActionPermissionRequest}, attentionAt(20))
	if err != nil {
		t.Fatalf("ResolveActions returned an error: %v", err)
	}
	if n != 1 {
		t.Errorf("ResolveActions changed %d row(s); want 1", n)
	}

	// The failure on the same attempt is untouched.
	mine, _ := repo.Actions(ctx, "sess_a")
	for _, a := range mine {
		want := attention.ActionResolved
		if a.Type == attention.ActionViewFailure {
			want = attention.ActionPending
		}
		if a.Status != want {
			t.Errorf("action %s is %s; want %s", a.ID, a.Status, want)
		}
	}
	// And the other attempt's is untouched.
	theirs, _ := repo.Actions(ctx, "sess_b")
	if theirs[0].Status != attention.ActionPending {
		t.Errorf("another attempt's action was resolved by this one")
	}

	// Resolving again changes nothing.
	if n, err := repo.ResolveActions(ctx, "sess_a", []attention.ActionType{attention.ActionPermissionRequest}, attentionAt(30)); err != nil || n != 0 {
		t.Errorf("a second ResolveActions changed %d row(s), %v; want 0, nil", n, err)
	}
}

func TestResolveActionsWithNoTypesIsNothing(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	if _, err := repo.RaiseAction(ctx, actionRow(anActionID("f004"), "sess_a", "p_a", attention.ActionPermissionRequest, attentionAt(10))); err != nil {
		t.Fatalf("RaiseAction returned an error: %v", err)
	}
	n, err := repo.ResolveActions(ctx, "sess_a", nil, attentionAt(20))
	if err != nil || n != 0 {
		t.Errorf("ResolveActions with no types = %d, %v; want 0, nil", n, err)
	}
	// An empty type list must not mean "every type", which an `IN ()` would.
	list, _ := repo.Actions(ctx, "sess_a")
	if list[0].Status != attention.ActionPending {
		t.Errorf("an empty type list resolved %s", list[0].Type)
	}
}

func TestActionCountAndDeleteAll(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	if n, err := repo.CountActions(ctx); err != nil || n != 0 {
		t.Errorf("CountActions() = %d, %v on an empty table; want 0, nil", n, err)
	}
	if _, err := repo.RaiseAction(ctx, actionRow(anActionID("f005"), "sess_a", "p_a", attention.ActionViewFailure, attentionAt(10))); err != nil {
		t.Fatalf("RaiseAction returned an error: %v", err)
	}
	if n, _ := repo.CountActions(ctx); n != 1 {
		t.Errorf("CountActions() = %d; want 1", n)
	}
	if err := repo.DeleteAllActions(ctx); err != nil {
		t.Fatalf("DeleteAllActions returned an error: %v", err)
	}
	if n, _ := repo.CountActions(ctx); n != 0 {
		t.Errorf("CountActions() = %d after DeleteAllActions; want 0", n)
	}
}

func TestAttentionStoreReportsAStorageFailure(t *testing.T) {
	store := newTestStore(t)
	repo := store.Attention()
	if err := store.Close(); err != nil {
		t.Fatalf("closing the store failed: %v", err)
	}
	ctx := context.Background()

	if _, err := repo.UpsertAttention(ctx, attentionRow("sess_a", "p_a", attention.LevelNone, "agent started", attentionAt(10))); !attention.IsCode(err, attention.CodeStorageFailure) {
		t.Errorf("UpsertAttention error = %v; want %q", err, attention.CodeStorageFailure)
	}
	if _, err := repo.RaiseAction(ctx, actionRow(anActionID("f006"), "sess_a", "p_a", attention.ActionViewFailure, attentionAt(10))); !attention.IsCode(err, attention.CodeStorageFailure) {
		t.Errorf("RaiseAction error = %v; want %q", err, attention.CodeStorageFailure)
	}
	if _, err := repo.ResolveActions(ctx, "sess_a", []attention.ActionType{attention.ActionViewFailure}, attentionAt(20)); !attention.IsCode(err, attention.CodeStorageFailure) {
		t.Errorf("ResolveActions error = %v; want %q", err, attention.CodeStorageFailure)
	}
	if _, err := repo.CountAttention(ctx); !attention.IsCode(err, attention.CodeStorageFailure) {
		t.Errorf("CountAttention error = %v; want %q", err, attention.CodeStorageFailure)
	}
	if _, err := repo.CountActions(ctx); !attention.IsCode(err, attention.CodeStorageFailure) {
		t.Errorf("CountActions error = %v; want %q", err, attention.CodeStorageFailure)
	}
	if err := repo.DeleteAllAttention(ctx); !attention.IsCode(err, attention.CodeStorageFailure) {
		t.Errorf("DeleteAllAttention error = %v; want %q", err, attention.CodeStorageFailure)
	}
	if err := repo.DeleteAllActions(ctx); !attention.IsCode(err, attention.CodeStorageFailure) {
		t.Errorf("DeleteAllActions error = %v; want %q", err, attention.CodeStorageFailure)
	}
}

// ---------------------------------------------------------------------------
// Batch reads
// ---------------------------------------------------------------------------

func TestNewestAttentionByProjectsPicksEachProjectsLatest(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	entries := []attention.Attention{
		attentionRow("sess_a1", "p_a", attention.LevelWarning, "agent failed", attentionAt(10)),
		attentionRow("sess_a2", "p_a", attention.LevelNone, "agent started", attentionAt(20)),
		attentionRow("sess_b1", "p_b", attention.LevelActionRequired, "permission requested", attentionAt(15)),
	}
	for _, a := range entries {
		if _, err := repo.UpsertAttention(ctx, a); err != nil {
			t.Fatalf("UpsertAttention returned an error: %v", err)
		}
	}

	got, err := repo.NewestAttentionByProjects(ctx, []string{"p_a", "p_b", "p_absent"})
	if err != nil {
		t.Fatalf("NewestAttentionByProjects returned an error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("returned %d row(s); want 2", len(got))
	}
	byProject := map[string]string{}
	for _, a := range got {
		byProject[a.ProjectID] = a.AgentSessionID
	}
	if byProject["p_a"] != "sess_a2" {
		t.Errorf("p_a's newest attention is %q; want the later of its two attempts", byProject["p_a"])
	}
	if byProject["p_b"] != "sess_b1" {
		t.Errorf("p_b's attention is %q", byProject["p_b"])
	}
}

// TestPendingActionCountsGroupsByProject is the other half of the dashboard's
// queue badge, and it counts only what is waiting.
func TestPendingActionCountsGroupsByProject(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	rows := []attention.Action{
		actionRow(anActionID("a001"), "sess_a1", "p_a", attention.ActionPermissionRequest, attentionAt(10)),
		actionRow(anActionID("a002"), "sess_a2", "p_a", attention.ActionViewFailure, attentionAt(11)),
		actionRow(anActionID("a003"), "sess_a3", "p_a", attention.ActionViewCompletion, attentionAt(12)),
		actionRow(anActionID("a004"), "sess_b1", "p_b", attention.ActionPermissionRequest, attentionAt(13)),
	}
	for _, a := range rows {
		if _, err := repo.RaiseAction(ctx, a); err != nil {
			t.Fatalf("RaiseAction returned an error: %v", err)
		}
	}
	// Settle one of p_a's, so the count has to notice.
	if _, err := repo.ResolveActions(ctx, "sess_a3", []attention.ActionType{attention.ActionViewCompletion}, attentionAt(20)); err != nil {
		t.Fatalf("ResolveActions returned an error: %v", err)
	}

	counts, err := repo.PendingActionCounts(ctx, []string{"p_a", "p_b", "p_absent"})
	if err != nil {
		t.Fatalf("PendingActionCounts returned an error: %v", err)
	}
	if counts["p_a"] != 2 {
		t.Errorf("p_a has %d pending; want 2 - the settled one is not waiting", counts["p_a"])
	}
	if counts["p_b"] != 1 {
		t.Errorf("p_b has %d pending; want 1", counts["p_b"])
	}
	if _, ok := counts["p_absent"]; ok {
		t.Error("a project with nothing pending was reported")
	}
}

func TestBatchReadsWithNoProjects(t *testing.T) {
	repo := newAttentionStore(t)
	ctx := context.Background()

	attentionRows, err := repo.NewestAttentionByProjects(ctx, nil)
	if err != nil || attentionRows != nil {
		t.Errorf("NewestAttentionByProjects(nil) = %v, %v; want nothing", attentionRows, err)
	}
	counts, err := repo.PendingActionCounts(ctx, nil)
	if err != nil {
		t.Fatalf("PendingActionCounts(nil) returned an error: %v", err)
	}
	if len(counts) != 0 {
		t.Errorf("PendingActionCounts(nil) = %v; want an empty map", counts)
	}
}
