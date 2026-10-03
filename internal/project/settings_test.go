package project

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/claude"
)

// fakeSettings is an in-memory SettingsRepository.
//
// It reproduces the two storage behaviours the service depends on - a missing
// row is ErrSettingsNotFound rather than a zero value, and a save replaces what
// was there - without a database, so that a service failure cannot be mistaken
// for a storage failure.
type fakeSettings struct {
	mu      sync.Mutex
	byID    map[string]Settings
	writes  int
	failOn  string
	failErr error
}

func newFakeSettings() *fakeSettings {
	return &fakeSettings{byID: make(map[string]Settings)}
}

func (r *fakeSettings) fail(operation string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failOn = operation
	r.failErr = err
}

func (r *fakeSettings) check(operation string) error {
	if r.failOn == operation {
		return r.failErr
	}
	return nil
}

func (r *fakeSettings) GetSettings(_ context.Context, projectID string) (Settings, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check("get"); err != nil {
		return Settings{}, err
	}
	s, ok := r.byID[projectID]
	if !ok {
		return Settings{}, ErrSettingsNotFound
	}
	return s, nil
}

func (r *fakeSettings) SettingsByProjects(_ context.Context, projectIDs []string) ([]Settings, error) {
	// The early return is before the failure injection on purpose: the real
	// store answers an empty listing without touching the database, and a fake
	// that queried anyway would let a test pass that pinned a behaviour storage
	// does not have.
	if len(projectIDs) == 0 {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check("list"); err != nil {
		return nil, err
	}
	out := make([]Settings, 0, len(projectIDs))
	for _, id := range projectIDs {
		if s, ok := r.byID[id]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func (r *fakeSettings) SaveSettings(_ context.Context, s Settings) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check("save"); err != nil {
		return err
	}
	// created_at is not moved by a replacement, which is the property the
	// SQLite upsert is written to have.
	if existing, ok := r.byID[s.ProjectID]; ok {
		s.CreatedAt = existing.CreatedAt
	}
	r.byID[s.ProjectID] = s
	r.writes++
	return nil
}

// newSettingsHarness is the service harness, whose settings store it already
// holds - nothing is rebuilt here, so a test that inspects h.settings is
// inspecting the store the service actually writes to.
func newSettingsHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t)
}

// register registers a project through the service, so the fixture has a real
// id in the shape the endpoints validate.
func (h *harness) register(t *testing.T, name string) string {
	t.Helper()
	p, err := h.service.Register(context.Background(), RegisterInput{HostPath: h.mkdir(t, name)})
	if err != nil {
		t.Fatalf("Register returned an error: %v", err)
	}
	return p.ID
}

// ---------------------------------------------------------------------------
// 1. The default
// ---------------------------------------------------------------------------

// TestAProjectNobodyConfiguredLaunchesBypass pins §二's default.
//
// It is checked by reading rather than by reading the constant, because the
// constant being right and the read path returning it are two different facts -
// and it is the second one a launch depends on.
func TestAProjectNobodyConfiguredLaunchesBypass(t *testing.T) {
	h := newSettingsHarness(t)
	id := h.register(t, "checkout-service")

	settings, err := h.service.Settings(context.Background(), id)
	if err != nil {
		t.Fatalf("Settings returned an error: %v", err)
	}
	if settings.PermissionMode != claude.PermissionBypass {
		t.Errorf("permission mode = %q, want %q", settings.PermissionMode, claude.PermissionBypass)
	}
	if settings.ProjectID != id {
		t.Errorf("project id = %q, want %q", settings.ProjectID, id)
	}

	// Reading is not configuring. A project nobody has chosen a mode for must
	// not acquire a row just by being looked at, or the table would fill with
	// rows that record no decision - and it is the absence of a row that makes
	// the default changeable at all.
	if h.settings.writes != 0 {
		t.Errorf("%d writes after a read; a read must not configure anything", h.settings.writes)
	}
}

// TestTheDefaultIsTheOnlyThingThatChanged covers §四 and §十六's cases 2 to 4.
//
// The phase lowered the default from `manual` to `bypassPermissions`, and the
// claim that makes that safe is that it reaches *only* projects with no row. So
// each project here is given a row the way one written before this phase would
// have been - straight into the store, without going through SetPermissionMode,
// which is the writer the console uses - and is then read back through the
// service a launch uses.
//
// A test that saved through SetPermissionMode would be testing the writer. The
// question here is about the reader, and specifically that it prefers a stored
// row to the default.
func TestTheDefaultIsTheOnlyThingThatChanged(t *testing.T) {
	for _, mode := range claude.PermissionModes() {
		t.Run(string(mode), func(t *testing.T) {
			h := newSettingsHarness(t)
			id := h.register(t, "checkout-service")

			// The row an earlier build would have left behind. `manual` is the
			// one that matters: it was the old default, so it is both the value
			// most rows hold and the value this phase stopped handing out.
			h.settings.byID[id] = Settings{
				ProjectID:      id,
				PermissionMode: mode,
				CreatedAt:      time.Now().Add(-time.Hour).UTC(),
				UpdatedAt:      time.Now().Add(-time.Hour).UTC(),
			}

			got, err := h.service.Settings(context.Background(), id)
			if err != nil {
				t.Fatalf("Settings returned an error: %v", err)
			}
			if got.PermissionMode != mode {
				t.Errorf("permission mode = %q, want the stored %q", got.PermissionMode, mode)
			}

			// And the same answer on the read the console makes, which folds the
			// default in for every project at once. The two paths must not
			// disagree about which projects the default applies to.
			modes, err := h.service.PermissionModesForProjects(context.Background(), []string{id})
			if err != nil {
				t.Fatalf("PermissionModesForProjects returned an error: %v", err)
			}
			if modes[id] != mode {
				t.Errorf("the console reads %q, want the stored %q", modes[id], mode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 2-3. Choosing a mode
// ---------------------------------------------------------------------------

// TestSettingThePermissionMode covers §十's cases 2 and 3, and the third mode
// with them so that the enum is walked rather than sampled.
func TestSettingThePermissionMode(t *testing.T) {
	for _, mode := range claude.PermissionModes() {
		t.Run(string(mode), func(t *testing.T) {
			h := newSettingsHarness(t)
			id := h.register(t, "checkout-service")

			saved, err := h.service.SetPermissionMode(context.Background(), id, mode)
			if err != nil {
				t.Fatalf("SetPermissionMode returned an error: %v", err)
			}
			if saved.PermissionMode != mode {
				t.Errorf("the returned mode = %q, want %q", saved.PermissionMode, mode)
			}
			if saved.CreatedAt.IsZero() || saved.UpdatedAt.IsZero() {
				t.Errorf("timestamps were not stamped: %+v", saved)
			}

			// Read back through the service, not from the returned value: what
			// a launch does is read, and a write that did not persist would
			// otherwise pass.
			got, err := h.service.Settings(context.Background(), id)
			if err != nil {
				t.Fatalf("Settings returned an error: %v", err)
			}
			if got.PermissionMode != mode {
				t.Errorf("the stored mode = %q, want %q", got.PermissionMode, mode)
			}
		})
	}
}

// TestChangingTheModeKeepsTheFirstConfiguredTime pins the upsert's contract: a
// second choice replaces the mode without moving when the project was first
// configured.
func TestChangingTheModeKeepsTheFirstConfiguredTime(t *testing.T) {
	h := newSettingsHarness(t)
	id := h.register(t, "checkout-service")

	first, err := h.service.SetPermissionMode(context.Background(), id, claude.PermissionAcceptEdits)
	if err != nil {
		t.Fatalf("SetPermissionMode returned an error: %v", err)
	}
	second, err := h.service.SetPermissionMode(context.Background(), id, claude.PermissionBypass)
	if err != nil {
		t.Fatalf("SetPermissionMode returned an error: %v", err)
	}

	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("createdAt moved from %s to %s; configuring again is not first configuring",
			first.CreatedAt, second.CreatedAt)
	}
	if second.PermissionMode != claude.PermissionBypass {
		t.Errorf("the mode = %q, want %q", second.PermissionMode, claude.PermissionBypass)
	}
	if h.settings.writes != 2 {
		t.Errorf("%d writes; want 2 - one row per project, replaced", h.settings.writes)
	}
}

// ---------------------------------------------------------------------------
// 4 and §十一. What is refused
// ---------------------------------------------------------------------------

// TestAnIllegalPermissionModeIsRefused is §十's case 4 and §十一's check in one
// table.
//
// Every value here is something a client could put in the JSON body. The
// injection-shaped ones are not parsed, escaped or sanitised - they are simply
// not modes, which is the whole of the argument: the vocabulary is closed, so
// the question "what would this do in a shell" never arises. The test asserts
// the refusal AND that nothing reached the store, because a refusal that wrote
// the value anyway would be worse than no check at all.
func TestAnIllegalPermissionModeIsRefused(t *testing.T) {
	for _, value := range []string{
		"",
		"default",
		"MANUAL",
		"manual ",
		"accept-edits",
		"plan",    // a real CLI mode this build does not offer
		"dontAsk", // and another
		"xxx; rm -rf /",
		"manual; rm -rf /",
		"$(whoami)",
		"`id`",
		"--dangerously-skip-permissions",
		"manual\nbypassPermissions",
	} {
		t.Run(value, func(t *testing.T) {
			h := newSettingsHarness(t)
			id := h.register(t, "checkout-service")

			_, err := h.service.SetPermissionMode(context.Background(), id, claude.PermissionMode(value))
			if err == nil {
				t.Fatalf("SetPermissionMode accepted %q", value)
			}
			if code := CodeOf(err); code != CodeInvalidInput {
				t.Errorf("error code = %q, want %q", code, CodeInvalidInput)
			}
			// The message names the vocabulary, so a caller that guessed is
			// told what the choices are rather than only that it guessed wrong.
			for _, mode := range claude.PermissionModes() {
				if !strings.Contains(err.Error(), string(mode)) {
					t.Errorf("the error does not name %q: %v", mode, err)
				}
			}
			if h.settings.writes != 0 {
				t.Errorf("%d writes after a refused value; a refused value must not be stored",
					h.settings.writes)
			}
		})
	}
}

// TestAnUnknownProjectHasNoSettings pins the other absence. A mode that does
// not exist and a project that does not exist are different questions, and both
// are answered rather than one being answered as the other.
func TestAnUnknownProjectHasNoSettings(t *testing.T) {
	h := newSettingsHarness(t)

	if _, err := h.service.Settings(context.Background(), "prj_does_not_exist_at_all"); CodeOf(err) != CodeNotFound {
		t.Errorf("Settings error code = %q, want %q", CodeOf(err), CodeNotFound)
	}
	if _, err := h.service.SetPermissionMode(context.Background(), "prj_does_not_exist_at_all",
		claude.PermissionManual); CodeOf(err) != CodeNotFound {
		t.Errorf("SetPermissionMode error code = %q, want %q", CodeOf(err), CodeNotFound)
	}
	if _, err := h.service.Settings(context.Background(), ""); CodeOf(err) != CodeNotFound {
		t.Errorf("an empty id: error code = %q, want %q", CodeOf(err), CodeNotFound)
	}
}

// TestASettingsFailureIsReportedNotSwallowed keeps the storage error on the
// path out, the way TestServicePropagatesStorageFailuresUnchanged does for
// projects: storage attaches the code and the service does not reinterpret it.
func TestASettingsFailureIsReportedNotSwallowed(t *testing.T) {
	h := newSettingsHarness(t)
	id := h.register(t, "checkout-service")

	storageErr := &Error{Code: CodeStorageFailure, Message: "could not store the project's settings"}
	h.settings.fail("save", storageErr)

	_, err := h.service.SetPermissionMode(context.Background(), id, claude.PermissionAcceptEdits)
	if !errors.Is(err, storageErr) {
		t.Fatalf("SetPermissionMode returned %v, want the storage error unchanged", err)
	}
	if code := CodeOf(err); code != CodeStorageFailure {
		t.Errorf("error code = %q, want %q", code, CodeStorageFailure)
	}
}

// ---------------------------------------------------------------------------
// The console's read
// ---------------------------------------------------------------------------

// TestPermissionModesForProjectsAnswersForEveryProject is the read the
// dashboard's cards are built from.
//
// The property worth pinning is that the map has an entry for every id it was
// given - configured or not - because the caller substitutes no default of its
// own and a missing key would render as an empty mode.
func TestPermissionModesForProjectsAnswersForEveryProject(t *testing.T) {
	h := newSettingsHarness(t)
	configured := h.register(t, "checkout-service")
	untouched := h.register(t, "billing-service")
	alsoConfigured := h.register(t, "search-service")

	for id, mode := range map[string]claude.PermissionMode{
		configured:     claude.PermissionAcceptEdits,
		alsoConfigured: claude.PermissionBypass,
	} {
		if _, err := h.service.SetPermissionMode(context.Background(), id, mode); err != nil {
			t.Fatalf("SetPermissionMode returned an error: %v", err)
		}
	}

	modes, err := h.service.PermissionModesForProjects(context.Background(),
		[]string{configured, untouched, alsoConfigured, "prj_not_a_project"})
	if err != nil {
		t.Fatalf("PermissionModesForProjects returned an error: %v", err)
	}
	want := map[string]claude.PermissionMode{
		configured:          claude.PermissionAcceptEdits,
		untouched:           claude.PermissionBypass,
		alsoConfigured:      claude.PermissionBypass,
		"prj_not_a_project": claude.PermissionBypass,
	}
	if len(modes) != len(want) {
		t.Fatalf("%d entries; want %d - every id asked about must be answered", len(modes), len(want))
	}
	for id, mode := range want {
		if modes[id] != mode {
			t.Errorf("%s = %q, want %q", id, modes[id], mode)
		}
	}
}

// TestAskingAboutNoProjectsAsksNothing pins the empty case: a listing for no
// projects is answered without a query, which is what the store does and what
// the dashboard relies on when nothing is registered.
func TestAskingAboutNoProjectsAsksNothing(t *testing.T) {
	h := newSettingsHarness(t)
	h.settings.fail("list", errors.New("a listing for no projects must not reach storage"))

	modes, err := h.service.PermissionModesForProjects(context.Background(), nil)
	if err != nil {
		t.Fatalf("PermissionModesForProjects returned an error: %v", err)
	}
	if len(modes) != 0 {
		t.Errorf("%d entries; want none", len(modes))
	}
}
