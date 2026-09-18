package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// testProject builds a stored-shape project. Only the fields the store reads
// and writes appear; status is derived and never persisted.
func testProject(id, name, hostPath string) *project.Project {
	return &project.Project{
		ID:          id,
		Name:        name,
		HostPath:    hostPath,
		RuntimePath: "/mnt/d" + hostPath,
		CreatedAt:   at(2026, time.September, 17, 12),
		UpdatedAt:   at(2026, time.September, 17, 12),
	}
}

// testProjectAt is testProject with a distinct registration time.
//
// Registration order is a project list's default order, and the list is sorted
// by creation time, so a test about ordering needs projects that were created
// at different moments. `order` is a day offset, which is enough to be strictly
// increasing without spelling out a timestamp per project.
func testProjectAt(id, name, hostPath string, order int) *project.Project {
	p := testProject(id, name, hostPath)
	p.CreatedAt = at(2026, time.September, order, 9)
	p.UpdatedAt = p.CreatedAt
	return p
}

func TestProjectStoreSatisfiesTheRepositoryContract(t *testing.T) {
	// The compile-time assertion in projects.go is the real check; this makes
	// the intent visible in the test suite as well.
	var store *ProjectStore = newTestStore(t).Projects()
	var _ project.Repository = store
}

func TestCreateThenGetByID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	slot := 3
	opened := at(2026, time.September, 17, 13)
	original := testProject("p_0123456789abcdef0123", "AgentMux", `D:\AI\Projects\AgentMux`)
	original.CollectionPath = `D:\AI\Projects`
	original.PinnedSlot = &slot
	original.LastOpenedAt = &opened

	if err := store.Projects().Create(ctx, original); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	got, err := store.Projects().GetByID(ctx, original.ID)
	if err != nil {
		t.Fatalf("GetByID returned an error: %v", err)
	}

	if got.ID != original.ID || got.Name != original.Name ||
		got.HostPath != original.HostPath || got.RuntimePath != original.RuntimePath ||
		got.CollectionPath != original.CollectionPath {
		t.Errorf("round trip changed the project: got %+v, want %+v", got, original)
	}
	if got.PinnedSlot == nil || *got.PinnedSlot != slot {
		t.Errorf("PinnedSlot = %v, want %d", got.PinnedSlot, slot)
	}
	if got.LastOpenedAt == nil || !got.LastOpenedAt.Equal(opened) {
		t.Errorf("LastOpenedAt = %v, want %v", got.LastOpenedAt, opened)
	}
	if got.Archived {
		t.Error("Archived = true after storing an unarchived project")
	}
	// Timestamps go through a text format, so compare instants rather than
	// representation.
	if !got.CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, original.CreatedAt)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt came back as the zero time")
	}
}

// TestRoundTripKeepsNullColumnsNull covers the two optional columns. A nil
// pinned slot stored as 0 would pin every project to the same panel.
func TestRoundTripKeepsNullColumnsNull(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	p := testProject("p_1", "App", "/data/App")
	if err := store.Projects().Create(ctx, p); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	got, err := store.Projects().GetByID(ctx, "p_1")
	if err != nil {
		t.Fatalf("GetByID returned an error: %v", err)
	}
	if got.PinnedSlot != nil {
		t.Errorf("PinnedSlot = %d, want nil", *got.PinnedSlot)
	}
	if got.LastOpenedAt != nil {
		t.Errorf("LastOpenedAt = %v, want nil", got.LastOpenedAt)
	}
	if got.CollectionPath != "" {
		t.Errorf("CollectionPath = %q, want an empty string for a project with no collection", got.CollectionPath)
	}
}

func TestGetByIDForAnUnknownProject(t *testing.T) {
	store := newTestStore(t)

	got, err := store.Projects().GetByID(context.Background(), "p_missing")
	if err == nil {
		t.Fatalf("GetByID returned %+v, want an error", got)
	}
	if !errors.Is(err, project.ErrNotFound) {
		t.Errorf("GetByID failed with %v, want project.ErrNotFound", err)
	}
}

// TestGetByHostPathIgnoresCase is what makes re-registering the same folder
// under different letter casing resolve to the existing project instead of
// creating a second one.
func TestGetByHostPathIgnoresCase(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.Projects().Create(ctx, testProject("p_1", "App", `D:\AI\Projects\App`)); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	for _, spelling := range []string{
		`D:\AI\Projects\App`,
		`d:\ai\projects\app`,
		`D:\AI\PROJECTS\APP`,
	} {
		got, err := store.Projects().GetByHostPath(ctx, spelling)
		if err != nil {
			t.Errorf("GetByHostPath(%q) returned an error: %v", spelling, err)
			continue
		}
		if got.ID != "p_1" {
			t.Errorf("GetByHostPath(%q) = %q, want p_1", spelling, got.ID)
		}
	}
}

func TestGetByHostPathForAnUnknownPath(t *testing.T) {
	store := newTestStore(t)

	if _, err := store.Projects().GetByHostPath(context.Background(), "/nowhere"); !errors.Is(err, project.ErrNotFound) {
		t.Errorf("GetByHostPath failed with %v, want project.ErrNotFound", err)
	}
}

// TestCreateRejectsADuplicateHostPath checks the store reports the duplicate
// as the project model's own code, so the HTTP layer has one vocabulary.
func TestCreateRejectsADuplicateHostPath(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.Projects().Create(ctx, testProject("p_1", "App", "/data/App")); err != nil {
		t.Fatalf("the first Create returned an error: %v", err)
	}
	err := store.Projects().Create(ctx, testProject("p_2", "App again", "/data/App"))
	if err == nil {
		t.Fatal("the second Create succeeded, want a duplicate rejection")
	}
	if !project.IsCode(err, project.CodeAlreadyRegistered) {
		t.Errorf("error code = %q, want %q", project.CodeOf(err), project.CodeAlreadyRegistered)
	}

	var target *project.Error
	if !errors.As(err, &target) {
		t.Fatalf("error %v is not a *project.Error", err)
	}
	if target.Details["hostPath"] != "/data/App" {
		t.Errorf("Details[hostPath] = %v, want the clashing path", target.Details["hostPath"])
	}
}

func TestCreateRejectsADuplicateID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.Projects().Create(ctx, testProject("p_1", "App", "/data/App")); err != nil {
		t.Fatalf("the first Create returned an error: %v", err)
	}
	if err := store.Projects().Create(ctx, testProject("p_1", "Other", "/data/Other")); !project.IsCode(err, project.CodeAlreadyRegistered) {
		t.Errorf("storing a duplicate identifier failed with %v, want %q", err, project.CodeAlreadyRegistered)
	}
}

func TestCreateRejectsANilProject(t *testing.T) {
	store := newTestStore(t)

	if err := store.Projects().Create(context.Background(), nil); err == nil {
		t.Error("Create(nil) succeeded, want a rejection")
	}
}

func TestUpdateStoresEveryMutableField(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	p := testProject("p_1", "Before", "/data/App")
	if err := store.Projects().Create(ctx, p); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	slot := 2
	opened := at(2026, time.October, 1, 9)
	p.Name = "After"
	p.CollectionPath = "/data"
	p.Archived = true
	p.PinnedSlot = &slot
	p.LastOpenedAt = &opened
	p.UpdatedAt = at(2026, time.October, 1, 9)

	if err := store.Projects().Update(ctx, p); err != nil {
		t.Fatalf("Update returned an error: %v", err)
	}

	got, err := store.Projects().GetByID(ctx, "p_1")
	if err != nil {
		t.Fatalf("GetByID returned an error: %v", err)
	}
	if got.Name != "After" {
		t.Errorf("Name = %q, want %q", got.Name, "After")
	}
	if got.CollectionPath != "/data" {
		t.Errorf("CollectionPath = %q, want %q", got.CollectionPath, "/data")
	}
	if !got.Archived {
		t.Error("Archived = false after archiving the project")
	}
	if got.PinnedSlot == nil || *got.PinnedSlot != slot {
		t.Errorf("PinnedSlot = %v, want %d", got.PinnedSlot, slot)
	}
	if got.LastOpenedAt == nil || !got.LastOpenedAt.Equal(opened) {
		t.Errorf("LastOpenedAt = %v, want %v", got.LastOpenedAt, opened)
	}
	// CreatedAt is not in the UPDATE statement, so identity is preserved.
	if !got.CreatedAt.Equal(p.CreatedAt) {
		t.Errorf("CreatedAt = %v, want it unchanged at %v", got.CreatedAt, p.CreatedAt)
	}
}

// TestUpdateCanClearAnOptionalColumn checks that a project can be unpinned,
// which requires writing a real NULL rather than leaving the old value.
func TestUpdateCanClearAnOptionalColumn(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	slot := 1
	p := testProject("p_1", "App", "/data/App")
	p.PinnedSlot = &slot
	if err := store.Projects().Create(ctx, p); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	p.PinnedSlot = nil
	p.LastOpenedAt = nil
	if err := store.Projects().Update(ctx, p); err != nil {
		t.Fatalf("Update returned an error: %v", err)
	}

	got, err := store.Projects().GetByID(ctx, "p_1")
	if err != nil {
		t.Fatalf("GetByID returned an error: %v", err)
	}
	if got.PinnedSlot != nil {
		t.Errorf("PinnedSlot = %d after unpinning, want nil", *got.PinnedSlot)
	}
}

func TestUpdateReportsAMissingProject(t *testing.T) {
	store := newTestStore(t)

	err := store.Projects().Update(context.Background(), testProject("p_missing", "App", "/data/App"))
	if !errors.Is(err, project.ErrNotFound) {
		t.Errorf("Update failed with %v, want project.ErrNotFound", err)
	}
}

func TestUpdateRejectsANilProject(t *testing.T) {
	store := newTestStore(t)

	if err := store.Projects().Update(context.Background(), nil); err == nil {
		t.Error("Update(nil) succeeded, want a rejection")
	}
}

// TestListOrdersByWorkspaceSlot pins the ordering the workspace grid is built
// on.
//
// A pinned project comes before an unpinned one, pinned projects come in slot
// order, and everything else follows in registration order. Name is
// deliberately not part of it: two projects whose names sort next to each other
// have no reason to sit next to each other on screen.
func TestListOrdersByWorkspaceSlot(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Registration order is the creation order: Alpha, beta, zeta.
	for _, p := range []*project.Project{
		testProjectAt("p_1", "Alpha", "/data/alpha", 1),
		testProjectAt("p_2", "zeta", "/data/zeta", 2),
		testProjectAt("p_3", "beta", "/data/beta", 3),
	} {
		if err := store.Projects().Create(ctx, p); err != nil {
			t.Fatalf("Create(%s) returned an error: %v", p.ID, err)
		}
	}

	// beta is pinned to the front, zeta to the end.
	pin(t, store, ctx, "p_3", 0)
	pin(t, store, ctx, "p_2", 9)

	got, err := store.Projects().List(ctx, project.ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}

	// beta (slot 0), then zeta (slot 9), then Alpha, which has no slot and is
	// the oldest of the unpinned.
	want := []string{"p_3", "p_2", "p_1"}
	assertOrder(t, got, want)
}

// TestListKeepsUnpinnedProjectsInRegistrationOrder is the rule that makes a
// grid of terminals usable: a project's position does not depend on its name,
// on when it was last opened, or on anything that changes while the page is
// open.
func TestListKeepsUnpinnedProjectsInRegistrationOrder(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Registered in an order that is neither alphabetical nor reverse
	// alphabetical, so a name sort would show up as a failure.
	for _, p := range []*project.Project{
		testProjectAt("p_c", "Charlie", "/data/c", 1),
		testProjectAt("p_a", "alpha", "/data/a", 2),
		testProjectAt("p_b", "Bravo", "/data/b", 3),
	} {
		if err := store.Projects().Create(ctx, p); err != nil {
			t.Fatalf("Create(%s) returned an error: %v", p.ID, err)
		}
	}

	got, err := store.Projects().List(ctx, project.ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	assertOrder(t, got, []string{"p_c", "p_a", "p_b"})

	// Opening a project is what last_opened_at records, and it must not move
	// anything: the panel a person is looking at is where they left it.
	opened, err := store.Projects().GetByID(ctx, "p_b")
	if err != nil {
		t.Fatalf("GetByID returned an error: %v", err)
	}
	when := at(2026, time.September, 18, 9)
	opened.LastOpenedAt = &when
	if err := store.Projects().Update(ctx, opened); err != nil {
		t.Fatalf("Update returned an error: %v", err)
	}

	after, err := store.Projects().List(ctx, project.ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	assertOrder(t, after, []string{"p_c", "p_a", "p_b"})
}

// TestPinningMovesOneProjectAndLeavesTheRest keeps the two halves of the slot
// model apart: a pinned slot is an absolute position, and the projects without
// one close up behind it.
func TestPinningMovesOneProjectAndLeavesTheRest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for _, p := range []*project.Project{
		testProjectAt("p_1", "One", "/data/1", 1),
		testProjectAt("p_2", "Two", "/data/2", 2),
		testProjectAt("p_3", "Three", "/data/3", 3),
	} {
		if err := store.Projects().Create(ctx, p); err != nil {
			t.Fatalf("Create(%s) returned an error: %v", p.ID, err)
		}
	}

	// Pinning the last project to slot 0 puts it first without renumbering
	// anybody: the others keep the order they already had.
	pin(t, store, ctx, "p_3", 0)

	got, err := store.Projects().List(ctx, project.ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	assertOrder(t, got, []string{"p_3", "p_1", "p_2"})

	// Unpinning it puts it back where registration order says it belongs.
	pin(t, store, ctx, "p_3", nil)
	back, err := store.Projects().List(ctx, project.ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	assertOrder(t, back, []string{"p_1", "p_2", "p_3"})
}

// pin sets or clears a project's workspace slot.
func pin(t *testing.T, store *Store, ctx context.Context, id string, slot any) {
	t.Helper()
	p, err := store.Projects().GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID(%s) returned an error: %v", id, err)
	}
	switch value := slot.(type) {
	case nil:
		p.PinnedSlot = nil
	case int:
		p.PinnedSlot = &value
	default:
		t.Fatalf("pin(%s): unsupported slot %v", id, slot)
	}
	if err := store.Projects().Update(ctx, p); err != nil {
		t.Fatalf("Update(%s) returned an error: %v", id, err)
	}
}

// assertOrder checks a listing against the identifiers it should hold, in
// order, and names the position that differs when it does not.
func assertOrder(t *testing.T, got []*project.Project, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("List returned %d projects, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("position %d is %q, want %q (list was %v)",
				i, got[i].ID, id, projectIDs(got))
		}
	}
}

func projectIDs(projects []*project.Project) []string {
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	return ids
}

func TestListHidesArchivedProjectsByDefault(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	visible := testProject("p_1", "Visible", "/data/visible")
	hidden := testProject("p_2", "Hidden", "/data/hidden")
	hidden.Archived = true
	for _, p := range []*project.Project{visible, hidden} {
		if err := store.Projects().Create(ctx, p); err != nil {
			t.Fatalf("Create(%s) returned an error: %v", p.ID, err)
		}
	}

	got, err := store.Projects().List(ctx, project.ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "p_1" {
		t.Errorf("List returned %d projects, want only the unarchived one", len(got))
	}

	all, err := store.Projects().List(ctx, project.ListFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("List with IncludeArchived returned %d projects, want 2", len(all))
	}
}

func TestListOnAnEmptyDatabase(t *testing.T) {
	store := newTestStore(t)

	got, err := store.Projects().List(context.Background(), project.ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	// An empty slice rather than nil, so the HTTP layer encodes [] and not
	// null.
	if got == nil {
		t.Fatal("List returned nil, want an empty slice")
	}
	if len(got) != 0 {
		t.Errorf("List returned %d projects, want none", len(got))
	}
}

// TestListDoesNotShareStateWithTheCaller checks that mutating a returned
// project cannot change what is stored.
func TestListDoesNotShareStateWithTheCaller(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	slot := 1
	p := testProject("p_1", "App", "/data/App")
	p.PinnedSlot = &slot
	if err := store.Projects().Create(ctx, p); err != nil {
		t.Fatalf("Create returned an error: %v", err)
	}

	first, err := store.Projects().List(ctx, project.ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	first[0].Name = "Tampered"
	*first[0].PinnedSlot = 9

	second, err := store.Projects().List(ctx, project.ListFilter{})
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if second[0].Name != "App" {
		t.Errorf("Name = %q after the caller mutated a previous result, want %q", second[0].Name, "App")
	}
	if *second[0].PinnedSlot != 1 {
		t.Errorf("PinnedSlot = %d after the caller mutated a previous result, want 1", *second[0].PinnedSlot)
	}
}
