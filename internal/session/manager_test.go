package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// These tests drive the RuntimeManager over a real tmux backend. The point of
// the manager is that every decision about sessions lives in one place, so the
// tests assert the decisions: which session a project gets, where it runs, what
// is written down, and what a restart makes of what it finds.

// fakeProjects is a ProjectLookup over a fixed set of projects.
type fakeProjects struct {
	byID map[string]*project.Project
}

func newFakeProjects(projects ...*project.Project) *fakeProjects {
	f := &fakeProjects{byID: make(map[string]*project.Project, len(projects))}
	for _, p := range projects {
		f.byID[p.ID] = p
	}
	return f
}

func (f *fakeProjects) Get(_ context.Context, id string) (*project.Project, error) {
	p, ok := f.byID[id]
	if !ok {
		return nil, &project.Error{Code: project.CodeNotFound, Message: "no project with id " + id}
	}
	return p, nil
}

func (f *fakeProjects) List(_ context.Context, _ project.ListFilter) ([]*project.Project, error) {
	out := make([]*project.Project, 0, len(f.byID))
	for _, p := range f.byID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// fakeStore is an in-memory RuntimeStore.
type fakeStore struct {
	records map[string]Record
}

func newFakeStore() *fakeStore { return &fakeStore{records: make(map[string]Record)} }

func (f *fakeStore) Save(_ context.Context, rec Record) error {
	f.records[rec.ProjectID] = rec
	return nil
}

func (f *fakeStore) Get(_ context.Context, projectID string) (Record, error) {
	rec, ok := f.records[projectID]
	if !ok {
		return Record{}, ErrRecordNotFound
	}
	return rec, nil
}

func (f *fakeStore) List(_ context.Context) ([]Record, error) {
	out := make([]Record, 0, len(f.records))
	for _, rec := range f.records {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectID < out[j].ProjectID })
	return out, nil
}

func (f *fakeStore) Delete(_ context.Context, projectID string) error {
	if _, ok := f.records[projectID]; !ok {
		return ErrRecordNotFound
	}
	delete(f.records, projectID)
	return nil
}

// testProject builds a project whose host path and collection deliberately
// differ from its runtime path, so that a session started in the wrong one
// cannot pass by accident. The runtime path is under /tmp so that the suite
// never creates anything in a real Projects Root.
func testProject(label string) *project.Project {
	id := testProjectID(label)
	return &project.Project{
		ID:             id,
		Name:           "Project " + label,
		HostPath:       `D:\AI\Projects\2026 AgentMux\` + label,
		RuntimePath:    "/tmp/agentmux-not-a-real-path/" + label,
		CollectionPath: `D:\AI\Projects\2026 AgentMux`,
	}
}

// testProjectID turns a readable label into an identifier of the shape AgentMux
// issues, so a failure message can still name the project it is about without
// the identifier being a label.
//
// It exists because the labels this suite used to pass as identifiers - "p_start",
// "p_adopt" - stopped being identifiers in Phase 2.5. A project's socket path is
// derived from its id and only an id AgentMux issued yields one, so a
// label-shaped id produced an empty socket path, and an empty socket path made
// newTestBackendOn skip. Every one of these tests would have gone green by not
// running, which is the failure mode a test suite is least able to report about
// itself.
func testProjectID(label string) string {
	digest := sha256.Sum256([]byte(label))
	id := project.IDPrefix + hex.EncodeToString(digest[:testIDDigestBytes])
	if !project.ValidID(id) {
		// A panic rather than a skip: the point of this function is that an
		// unusable id must be loud, and the id format changing is exactly the
		// event that would otherwise silence the suite again.
		panic("session tests: " + id + " is not an identifier AgentMux would issue")
	}
	return id
}

// testIDDigestBytes is how much of the digest of a label becomes an id. Ten
// bytes is what the production generator uses, and using the same number here
// keeps the ids in this suite the same shape as the ones in a database.
const testIDDigestBytes = 10

// testManager builds a manager over a real per-project runtime factory, with a
// socket directory of its own.
func testManager(t *testing.T, store RuntimeStore, projects ...*project.Project) (*Manager, *TmuxBackend) {
	t.Helper()
	return testManagerOn(t, uniqueSocketDir(t), store, projects...)
}

// testManagerOn builds a manager over a given socket directory, for the tests
// that need two managers to look at the same servers in turn.
//
// The returned backend is a second handle to the first project's server, on the
// socket the manager itself will use for that project. It exists so a test can
// reach past the manager - kill a server, list what is really there - without
// going through the thing it is testing.
func testManagerOn(t *testing.T, socketDir string, store RuntimeStore, projects ...*project.Project) (*Manager, *TmuxBackend) {
	t.Helper()

	runtimes := testRuntimes(t, socketDir)
	m, err := NewManager(ManagerOptions{
		Backends: runtimes,
		Sockets:  runtimes.Sockets(),
		Projects: newFakeProjects(projects...),
		Store:    store,
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	var backend *TmuxBackend
	if len(projects) > 0 {
		backend = newTestBackendOn(t, runtimes.Sockets().Path(projects[0].ID))
	}
	return m, backend
}

// discardLogger keeps runtime logging out of the test log while still exercising
// every call site that logs.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestManagerStartCreatesTheSessionItsProjectNeeds covers session identity, the
// working directory, and the canonical size: the three things a runtime has to
// get right before anything else about it matters.
func TestManagerStartCreatesTheSessionItsProjectNeeds(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	p := testProject("p_start")
	dir := t.TempDir()
	p.RuntimePath = dir

	m, backend := testManager(t, store, p)

	rt, err := m.Start(ctx, p.ID)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if rt.Session != p.SessionName() {
		t.Errorf("the runtime's session is %q, want %q", rt.Session, p.SessionName())
	}
	if rt.State != StateRunning {
		t.Errorf("the runtime is %q, want %q", rt.State, StateRunning)
	}
	if !rt.SessionAlive {
		t.Error("the runtime reports its session as not alive straight after starting it")
	}
	if rt.Cols != DefaultCols || rt.Rows != DefaultRows {
		t.Errorf("the runtime is %dx%d, want %dx%d", rt.Cols, rt.Rows, DefaultCols, DefaultRows)
	}

	inspected, err := backend.Inspect(ctx, rt.Session)
	if err != nil {
		t.Fatalf("Inspect returned an error: %v", err)
	}
	if inspected.Dir != dir {
		t.Errorf("the session runs in %q, want the project's runtime path %q", inspected.Dir, dir)
	}
}

// TestManagerStartUsesTheRuntimePathNotTheCollection is the working-directory
// requirement stated as a measurement inside the session.
//
// Starting at a Projects Root or at a collection puts an agent in the wrong
// repository while every status field still says running, so the directory is
// asked of the shell rather than read back from the model.
func TestManagerStartUsesTheRuntimePathNotTheCollection(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_cwd")
	runtimeDir := t.TempDir()
	// A collection directory that exists, so getting it wrong would be a
	// working session in the wrong place rather than a failure that announces
	// itself.
	collectionDir := t.TempDir()
	p.RuntimePath = runtimeDir
	p.CollectionPath = collectionDir

	m, _ := testManager(t, newFakeStore(), p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	want, err := filepath.EvalSymlinks(runtimeDir)
	if err != nil {
		t.Fatalf("could not resolve the runtime path: %v", err)
	}
	report := filepath.Join(runtimeDir, "pwd.txt")
	if err := m.Launch(ctx, p.ID, "pwd > "+report); err != nil {
		t.Fatalf("could not ask the session for its directory: %v", err)
	}

	got := strings.TrimSpace(string(waitForFile(t, report, readinessWait, func(d []byte) bool {
		return len(bytes.TrimSpace(d)) > 0
	})))
	if got == collectionDir {
		t.Error("the session was started in the collection rather than in the project")
	}
	if got != want {
		t.Errorf("the session runs in %q, want the project's runtime path %q", got, want)
	}
}

// TestManagerStartRecordsTheRuntime checks that what has to survive a restart is
// actually written down.
func TestManagerStartRecordsTheRuntime(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	p := testProject("p_record")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, store, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	rec, err := store.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("no runtime record was written: %v", err)
	}
	if rec.Session != p.SessionName() {
		t.Errorf("the record names the session %q, want %q", rec.Session, p.SessionName())
	}
	if rec.Backend != "tmux" {
		t.Errorf("the record names the backend %q, want %q", rec.Backend, "tmux")
	}
	if rec.State != StateRunning {
		t.Errorf("the record says the runtime is %q, want %q", rec.State, StateRunning)
	}
	if rec.Cols != DefaultCols || rec.Rows != DefaultRows {
		t.Errorf("the record holds %dx%d, want %dx%d", rec.Cols, rec.Rows, DefaultCols, DefaultRows)
	}
	if rec.LastSeenAt == nil {
		t.Error("the record has no last-seen time, so nothing says when the session was last confirmed")
	}
}

// TestManagerStartIsIdempotent is the property that stops a double click from
// producing two terminals writing to one repository.
func TestManagerStartIsIdempotent(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_twice")
	p.RuntimePath = t.TempDir()
	m, backend := testManager(t, newFakeStore(), p)

	first, err := m.Start(ctx, p.ID)
	if err != nil {
		t.Fatalf("the first Start returned an error: %v", err)
	}
	second, err := m.Start(ctx, p.ID)
	if err != nil {
		t.Fatalf("the second Start returned an error: %v", err)
	}

	if first.Session != second.Session {
		t.Errorf("the two starts produced different sessions: %q and %q", first.Session, second.Session)
	}
	if !first.StartedAt.Equal(second.StartedAt) {
		t.Errorf("the second start changed the runtime's start time from %v to %v", first.StartedAt, second.StartedAt)
	}

	sessions, err := backend.List(ctx)
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("there are %d sessions, want exactly 1", len(sessions))
	}
}

// TestManagerGivesEachProjectItsOwnServer is the isolation requirement measured
// from the manager's side: three projects, three sockets, three servers, and
// each server holding exactly one project's session.
//
// This test used to be called TestManagerStartsSeveralSessionsOnOneServer and
// asserted the opposite - three sessions sharing one server - which is exactly
// the arrangement Phase 2.5 removed. It survives in this shape because the
// property worth keeping from it is not where the sessions are but that they do
// not touch each other.
func TestManagerGivesEachProjectItsOwnServer(t *testing.T) {
	ctx := context.Background()

	labels := []string{"first", "second", "third"}
	projects := make([]*project.Project, 0, len(labels))
	dirs := make(map[string]string, len(labels))
	for _, label := range labels {
		p := testProject(label)
		p.RuntimePath = t.TempDir()
		dirs[p.ID] = p.RuntimePath
		projects = append(projects, p)
	}

	runtimes := testRuntimes(t, uniqueSocketDir(t))
	m, err := NewManager(ManagerOptions{
		Backends: runtimes,
		Sockets:  runtimes.Sockets(),
		Projects: newFakeProjects(projects...),
		Store:    newFakeStore(),
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	sockets := make(map[string]string, len(projects))
	for _, p := range projects {
		rt, err := m.Start(ctx, p.ID)
		if err != nil {
			t.Fatalf("starting %s failed: %v", p.ID, err)
		}
		if rt.State != StateRunning {
			t.Errorf("%s is %q, want %q", p.ID, rt.State, StateRunning)
		}
		if !rt.SessionAlive {
			t.Errorf("%s reports its session as not alive straight after starting it", p.ID)
		}
		if rt.Session != project.SessionNameFor(p.ID) {
			t.Errorf("%s runs in session %q, want %q", p.ID, rt.Session, project.SessionNameFor(p.ID))
		}
		sockets[p.ID] = runtimes.Sockets().Path(p.ID)
	}

	// Three projects, three sockets. Two projects sharing a path would be one
	// server and one fault domain, which is the whole of what this phase is
	// about, so it is asserted rather than assumed.
	seen := make(map[string]string, len(sockets))
	for id, path := range sockets {
		if path == "" {
			t.Fatalf("%s has no socket path", id)
		}
		if other, ok := seen[path]; ok {
			t.Errorf("%s and %s share the socket %q", id, other, path)
		}
		seen[path] = id
	}

	// Each socket carries this project's session and no other.
	for _, p := range projects {
		backend := newTestBackendOn(t, sockets[p.ID])
		sessions, err := backend.List(ctx)
		if err != nil {
			t.Fatalf("List on %s's socket returned an error: %v", p.ID, err)
		}
		if len(sessions) != 1 {
			t.Errorf("%s's server holds %d sessions, want only its own", p.ID, len(sessions))
			continue
		}
		if sessions[0].Name != project.SessionNameFor(p.ID) {
			t.Errorf("%s's server holds %q, want %q", p.ID, sessions[0].Name, project.SessionNameFor(p.ID))
		}
		if sessions[0].Dir != dirs[p.ID] {
			t.Errorf("%s runs in %q, want its own directory %q", p.ID, sessions[0].Dir, dirs[p.ID])
		}
	}

	// An input reaches its own session and no other.
	first := projects[0].ID
	if err := m.Launch(ctx, first, "echo amx-manager-only"); err != nil {
		t.Fatalf("could not type into %s: %v", first, err)
	}
	deadline := time.Now().Add(outputWait)
	var got []byte
	for time.Now().Before(deadline) {
		chunks, err := m.History(ctx, first, 0)
		if err != nil {
			t.Fatalf("History returned an error: %v", err)
		}
		got = nil
		for _, c := range chunks {
			got = append(got, c.Data...)
		}
		if bytes.Contains(got, []byte("amx-manager-only")) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bytes.Contains(got, []byte("amx-manager-only")) {
		t.Errorf("the session that was typed into never received its own output")
	}
	for _, p := range projects[1:] {
		chunks, err := m.History(ctx, p.ID, 0)
		if err != nil {
			t.Fatalf("History returned an error: %v", err)
		}
		for _, c := range chunks {
			if bytes.Contains(c.Data, []byte("amx-manager-only")) {
				t.Errorf("output meant for %s arrived at %s", first, p.ID)
			}
		}
	}
}

// TestManagerStartsBesideAForeignSessionOnItsOwnSocket keeps the code path the
// old shared-server test was written for.
//
// Starting a session when the server is already up with somebody else's session
// in it is a different situation for tmux, which words its complaint by target
// kind ("can't find window" from list-panes rather than "can't find session").
// That path was wrong once, and a suite that only ever started one session per
// socket could not see it. Since Phase 2.5 a project's socket is its own, so the
// only session that can already be there is one nobody registered - and Start
// has to work anyway, without touching it.
func TestManagerStartsBesideAForeignSessionOnItsOwnSocket(t *testing.T) {
	ctx := context.Background()

	p := testProject("beside")
	p.RuntimePath = t.TempDir()

	runtimes := testRuntimes(t, uniqueSocketDir(t))
	backend := newTestBackendOn(t, runtimes.Sockets().Path(p.ID))

	foreignDir := t.TempDir()
	foreign := project.SessionNameFor(testProjectID("foreign"))
	if _, err := backend.Create(ctx, SessionSpec{Name: foreign, Dir: foreignDir, Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("could not create the foreign session: %v", err)
	}

	m, err := NewManager(ManagerOptions{
		Backends: runtimes,
		Sockets:  runtimes.Sockets(),
		Projects: newFakeProjects(p),
		Store:    newFakeStore(),
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	rt, err := m.Start(ctx, p.ID)
	if err != nil {
		t.Fatalf("Start failed on a socket that already had a session on it: %v", err)
	}
	if rt.State != StateRunning {
		t.Errorf("the runtime is %q, want %q", rt.State, StateRunning)
	}

	// Both sessions are on the server, and the foreign one is untouched.
	sessions, err := backend.List(ctx)
	if err != nil {
		t.Fatalf("List returned an error: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("the server holds %d sessions, want the foreign one and the project's", len(sessions))
	}
	alive, err := backend.Exists(ctx, foreign)
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if !alive {
		t.Error("starting the project's runtime ended the session that was already there")
	}

	report, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if len(report.Orphans) != 1 || report.Orphans[0].Session != foreign {
		t.Errorf("Reconcile reported the orphans %v, want just %q", report.Orphans, foreign)
	}
}

// TestManagerStartAdoptsASurvivingSession checks that starting a project whose
// session is already there adopts it rather than failing or making a second one,
// and that the work inside it is not disturbed.
func TestManagerStartAdoptsASurvivingSession(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_adopt")
	dir := t.TempDir()
	p.RuntimePath = dir

	// The session has to be on the socket the manager will look at, which is
	// this project's own. A surviving session on somebody else's socket is not
	// this project's runtime, and adopting it would be the shared-server
	// behaviour Phase 2.5 removed.
	runtimes := testRuntimes(t, uniqueSocketDir(t))
	backend := newTestBackendOn(t, runtimes.Sockets().Path(p.ID))
	if _, err := backend.Create(ctx, SessionSpec{Name: p.SessionName(), Dir: dir, Cols: 90, Rows: 25}); err != nil {
		t.Fatalf("could not create the surviving session: %v", err)
	}
	if err := backend.Launch(ctx, p.SessionName(), "echo amx-survivor"); err != nil {
		t.Fatalf("could not type into the surviving session: %v", err)
	}

	m, err := NewManager(ManagerOptions{
		Backends: runtimes,
		Sockets:  runtimes.Sockets(),
		Projects: newFakeProjects(p),
		Store:    newFakeStore(),
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	rt, err := m.Start(ctx, p.ID)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if rt.Session != p.SessionName() {
		t.Errorf("Start used the session %q, want the one that already existed", rt.Session)
	}
	if rt.Cols != 90 || rt.Rows != 25 {
		t.Errorf("the runtime is %dx%d, want the surviving session's 90x25", rt.Cols, rt.Rows)
	}

	snapshot := screenRows(t, backend, rt.Session)
	if !bytes.Contains(snapshot, []byte("amx-survivor")) {
		t.Errorf("the adopted session lost the work it was holding; the pane holds:\n%s", snapshot)
	}
}

// TestManagerStartHonoursTheRecordedSize checks that a size a user chose is the
// size a later start restores, rather than the default.
func TestManagerStartHonoursTheRecordedSize(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	socketDir := uniqueSocketDir(t)

	p := testProject("p_size")
	p.RuntimePath = t.TempDir()

	first, backend := testManagerOn(t, socketDir, store, p)
	if _, err := first.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if _, err := first.Resize(ctx, p.ID, 100, 30); err != nil {
		t.Fatalf("Resize returned an error: %v", err)
	}

	// The session ends; the record does not. That is what the record is for.
	if err := backend.Destroy(ctx, p.SessionName()); err != nil {
		t.Fatalf("could not destroy the session under the manager: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}

	second, backend2 := testManagerOn(t, socketDir, store, p)
	rt, err := second.Start(ctx, p.ID)
	if err != nil {
		t.Fatalf("the second Start returned an error: %v", err)
	}
	if rt.Cols != 100 || rt.Rows != 30 {
		t.Errorf("the restarted runtime is %dx%d, want the recorded 100x30", rt.Cols, rt.Rows)
	}
	inspected, err := backend2.Inspect(ctx, rt.Session)
	if err != nil {
		t.Fatalf("Inspect returned an error: %v", err)
	}
	if inspected.Cols != 100 || inspected.Rows != 30 {
		t.Errorf("the session is %dx%d, want 100x30", inspected.Cols, inspected.Rows)
	}
}

// TestManagerStopInterruptsAndKeepsTheSession covers the Stop half of the
// Stop/Destroy distinction, including what is written down.
func TestManagerStopInterruptsAndKeepsTheSession(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	p := testProject("p_stop")
	p.RuntimePath = t.TempDir()
	m, backend := testManager(t, store, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	shell := shellCommand(t, backend, p.SessionName())

	if err := m.Launch(ctx, p.ID, "sleep 300"); err != nil {
		t.Fatalf("could not start a long-running command: %v", err)
	}
	waitForPaneCommand(t, backend, p.SessionName(), "sleep")

	rt, err := m.Stop(ctx, p.ID)
	if err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}
	if rt.State != StateStopped {
		t.Errorf("the runtime is %q after Stop, want %q", rt.State, StateStopped)
	}
	if !rt.SessionAlive {
		t.Error("Stop reports the session as gone; it must only end the work")
	}
	waitForPaneCommand(t, backend, p.SessionName(), shell)

	rec, err := store.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("the runtime record disappeared on Stop: %v", err)
	}
	if rec.State != StateStopped {
		t.Errorf("the record says %q after Stop, want %q", rec.State, StateStopped)
	}

	// Stopping again is a success: the caller's intent is already true.
	if _, err := m.Stop(ctx, p.ID); err != nil {
		t.Errorf("a second Stop returned an error: %v", err)
	}
}

// TestManagerStopOnAProjectWithNoRuntime is the boundary: stopping nothing is
// not an error, because the intent is satisfied.
func TestManagerStopOnAProjectWithNoRuntime(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_nostop")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, newFakeStore(), p)

	rt, err := m.Stop(ctx, p.ID)
	if err != nil {
		t.Fatalf("Stop on a project with no runtime returned an error: %v", err)
	}
	if rt.State != StateStopped {
		t.Errorf("the runtime is %q, want %q", rt.State, StateStopped)
	}
}

// TestManagerDestroyEndsTheSessionAndForgetsIt covers the irreversibility that
// separates Destroy from Stop.
func TestManagerDestroyEndsTheSessionAndForgetsIt(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	p := testProject("p_destroy")
	p.RuntimePath = t.TempDir()
	m, backend := testManager(t, store, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	if err := m.Destroy(ctx, p.ID); err != nil {
		t.Fatalf("Destroy returned an error: %v", err)
	}

	alive, err := backend.Exists(ctx, p.SessionName())
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if alive {
		t.Error("the session still exists after Destroy")
	}
	if _, err := store.Get(ctx, p.ID); !errors.Is(err, ErrRecordNotFound) {
		t.Errorf("the runtime record survived Destroy: %v", err)
	}

	// The project goes back to having no runtime rather than a broken one.
	rt, err := m.Runtime(ctx, p.ID)
	if err != nil {
		t.Fatalf("Runtime returned an error after Destroy: %v", err)
	}
	if rt.State != StateStopped {
		t.Errorf("the runtime is %q after Destroy, want %q", rt.State, StateStopped)
	}

	// Destroying again is a success, for the same reason a second Stop is.
	if err := m.Destroy(ctx, p.ID); err != nil {
		t.Errorf("a second Destroy returned an error: %v", err)
	}
}

// TestManagerRuntimeForANeverStartedProject checks the shape a client renders:
// one answer, not a not-found that has to be handled separately.
func TestManagerRuntimeForANeverStartedProject(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_never")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, newFakeStore(), p)

	rt, err := m.Runtime(ctx, p.ID)
	if err != nil {
		t.Fatalf("Runtime for a project with no runtime returned an error: %v", err)
	}
	if rt.State != StateStopped {
		t.Errorf("the runtime is %q, want %q", rt.State, StateStopped)
	}
	if rt.SessionAlive {
		t.Error("the runtime claims a session that was never started is alive")
	}
	if rt.Session != p.SessionName() {
		t.Errorf("the runtime names the session %q, want the one it would use", rt.Session)
	}
	if rt.Cols <= 0 || rt.Rows <= 0 {
		t.Errorf("the runtime is %dx%d, want a usable default size", rt.Cols, rt.Rows)
	}
}

// TestManagerRuntimeForAnUnknownProject checks that a missing project is a
// not-found rather than a runtime that looks stopped.
func TestManagerRuntimeForAnUnknownProject(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_known")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, newFakeStore(), p)

	if _, err := m.Runtime(ctx, "p_unknown"); !IsCode(err, CodeProjectNotFound) {
		t.Errorf("Runtime for an unknown project failed with %v, want %s", err, CodeProjectNotFound)
	}
}

// TestManagerRuntimeDoesNotAssumeASessionExists checks that a record left by a
// previous run does not make the runtime claim a live session.
func TestManagerRuntimeDoesNotAssumeASessionExists(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	p := testProject("p_ghost")
	p.RuntimePath = t.TempDir()
	if err := store.Save(ctx, Record{
		ProjectID: p.ID, Backend: "tmux", Session: p.SessionName(),
		State: StateRunning, Cols: 100, Rows: 30, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("could not seed the store: %v", err)
	}

	m, _ := testManager(t, store, p)

	rt, err := m.Runtime(ctx, p.ID)
	if err != nil {
		t.Fatalf("Runtime returned an error: %v", err)
	}
	if rt.SessionAlive {
		t.Error("the runtime reports a session as alive when none exists")
	}
}

// TestManagerInputAndResizeRequireARunningRuntime checks that the calls which
// need a terminal say so rather than silently doing nothing.
func TestManagerInputAndResizeRequireARunningRuntime(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_guard")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, newFakeStore(), p)

	if err := m.Input(ctx, p.ID, []byte("x")); !IsCode(err, CodeNotRunning) {
		t.Errorf("Input before Start failed with %v, want %s", err, CodeNotRunning)
	}
	if err := m.Launch(ctx, p.ID, "echo x"); !IsCode(err, CodeNotRunning) {
		t.Errorf("Launch before Start failed with %v, want %s", err, CodeNotRunning)
	}
	if _, err := m.Resize(ctx, p.ID, 100, 30); !IsCode(err, CodeNotRunning) {
		t.Errorf("Resize before Start failed with %v, want %s", err, CodeNotRunning)
	}
	if err := m.Input(ctx, "p_unknown", []byte("x")); !IsCode(err, CodeProjectNotFound) {
		t.Errorf("Input for an unknown project failed with %v, want %s", err, CodeProjectNotFound)
	}
}

// TestManagerResizeRejectsAnUnusableSize checks the guard before the backend.
func TestManagerResizeRejectsAnUnusableSize(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_badsize")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, newFakeStore(), p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	for _, size := range [][2]int{{0, 30}, {100, 0}, {-1, -1}} {
		if _, err := m.Resize(ctx, p.ID, size[0], size[1]); !IsCode(err, CodeInvalidSize) {
			t.Errorf("Resize to %dx%d failed with %v, want %s", size[0], size[1], err, CodeInvalidSize)
		}
	}
}

// TestManagerStartRefusesAProjectWithNoRuntimePath covers the guard that keeps a
// session from starting in whatever directory the server happens to be in.
func TestManagerStartRefusesAProjectWithNoRuntimePath(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_nopath")
	p.RuntimePath = "   "
	m, _ := testManager(t, newFakeStore(), p)

	if _, err := m.Start(ctx, p.ID); !IsCode(err, CodeStartFailed) {
		t.Errorf("Start with no runtime path failed with %v, want %s", err, CodeStartFailed)
	}
}

// TestManagerStartReportsAnUnusableRuntimePath checks that a path which does not
// exist is a failure with a reason, and that the failure is recorded so a later
// restart explains itself.
func TestManagerStartReportsAnUnusableRuntimePath(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	p := testProject("p_gone")
	p.RuntimePath = filepath.Join(t.TempDir(), "not-created")
	m, _ := testManager(t, store, p)

	if _, err := m.Start(ctx, p.ID); !IsCode(err, CodeStartFailed) {
		t.Errorf("Start in a directory that does not exist failed with %v, want %s", err, CodeStartFailed)
	}
	rec, err := store.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("a failed start left no record, so a restart cannot explain it: %v", err)
	}
	if rec.State != StateError {
		t.Errorf("the record says %q, want %q", rec.State, StateError)
	}
	if got := m.ProjectStatus(p.ID); got != project.StatusError {
		t.Errorf("the project's status is %q after a failed start, want %q", got, project.StatusError)
	}
}

// TestManagerOutputIsSequenced is the sequence-number requirement: one counter
// per runtime, starting at one and gap-free, shared by every reader.
func TestManagerOutputIsSequenced(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_seq")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, newFakeStore(), p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	backlog, chunks, cancel, err := m.Watch(ctx, p.ID, 0)
	if err != nil {
		t.Fatalf("Watch returned an error: %v", err)
	}
	defer cancel()

	// Whatever the shell printed while the runtime was starting is the start of
	// the stream, so the backlog has to begin at one if it is there at all.
	next := uint64(0)
	if len(backlog) > 0 {
		if backlog[0].Sequence != 1 {
			t.Errorf("the history begins at sequence %d, want 1", backlog[0].Sequence)
		}
		next = backlog[len(backlog)-1].Sequence
	}

	// Something that produces several chunks rather than one.
	if err := m.Launch(ctx, p.ID, "for i in 1 2 3; do echo amx-seq-$i; done"); err != nil {
		t.Fatalf("could not type into the runtime: %v", err)
	}

	var collected []byte
	deadline := time.Now().Add(outputWait)
	for time.Now().Before(deadline) && !bytes.Contains(collected, []byte("amx-seq-3")) {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatal("the watch channel closed while the runtime was running")
			}
			if chunk.ProjectID != p.ID {
				t.Errorf("a chunk for %q arrived on %q's channel", chunk.ProjectID, p.ID)
			}
			if chunk.Timestamp.IsZero() {
				t.Error("a chunk carries no timestamp")
			}
			if chunk.Sequence != next+1 {
				t.Errorf("the sequence jumped from %d to %d", next, chunk.Sequence)
			}
			next = chunk.Sequence
			collected = append(collected, chunk.Data...)
		case <-time.After(pollInterval):
		}
	}
	if !bytes.Contains(collected, []byte("amx-seq-3")) {
		t.Fatalf("the runtime's output never arrived; it produced:\n%q", collected)
	}

	// The history is the same stream, addressable by sequence number.
	history, err := m.History(ctx, p.ID, 0)
	if err != nil {
		t.Fatalf("History returned an error: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("History returned nothing for a runtime that has produced output")
	}
	if history[0].Sequence != 1 {
		t.Errorf("the history begins at sequence %d, want 1", history[0].Sequence)
	}
	var replay []byte
	for _, c := range history {
		replay = append(replay, c.Data...)
	}
	if !bytes.Contains(replay, []byte("amx-seq-3")) {
		t.Errorf("the replayed history does not contain the output:\n%q", replay)
	}

	// Wait for the stream to go quiet before asking what is new. The bytes the
	// loop above waited for are not the end of the stream: the shell still has
	// a prompt to print, and a test that asked straight away would be asking a
	// stream that is still talking to have nothing more to say. The manager
	// appends to the history and hands the chunk to its watchers under one
	// lock, so a channel that has gone quiet means the history holds nothing
	// newer either, and the claim below is safe to make.
	quiet := time.Now().Add(outputQuiet)
	for time.Now().Before(quiet) {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatal("the watch channel closed while the runtime was running")
			}
			next = chunk.Sequence
			quiet = time.Now().Add(outputQuiet)
		case <-time.After(pollInterval):
		}
	}

	// Asking for what has already been seen returns only what is new - which is
	// what a client reconnecting needs.
	resumed, err := m.History(ctx, p.ID, next)
	if err != nil {
		t.Fatalf("History returned an error: %v", err)
	}
	if len(resumed) != 0 {
		t.Errorf("History after the latest sequence returned %d chunks, want none", len(resumed))
	}
}

// TestManagerHistoryForAnUnknownProject returns nothing rather than an error:
// history is a read of something that may simply never have existed.
func TestManagerHistoryForAnUnknownProject(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_h")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, newFakeStore(), p)

	history, err := m.History(ctx, "p_never_existed", 0)
	if err != nil {
		t.Fatalf("History returned an error for a project with no runtime: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("History returned %d chunks for a project with no runtime", len(history))
	}
}

// TestManagerWatchClosesCleanly checks that a watcher is released when it says
// it is, and that saying so twice is harmless.
func TestManagerWatchClosesCleanly(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_watch")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, newFakeStore(), p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	_, chunks, cancel, err := m.Watch(ctx, p.ID, 0)
	if err != nil {
		t.Fatalf("Watch returned an error: %v", err)
	}
	cancel()
	cancel()

	deadline := time.Now().Add(outputWait)
	for time.Now().Before(deadline) {
		if _, ok := <-chunks; !ok {
			return
		}
	}
	t.Error("the watch channel was not closed by cancel")
}

// TestManagerReconcileAdoptsASurvivingSession is Case A: the session exists and
// the record says it should be running, so it is adopted rather than restarted.
func TestManagerReconcileAdoptsASurvivingSession(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	socketDir := uniqueSocketDir(t)

	p := testProject("p_case_a")
	p.RuntimePath = t.TempDir()

	first, _ := testManagerOn(t, socketDir, store, p)
	if _, err := first.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	// The server process ends; the session does not.
	if err := first.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}

	second, _ := testManagerOn(t, socketDir, store, p)

	report, err := second.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if len(report.Running) != 1 || report.Running[0] != p.ID {
		t.Errorf("Reconcile reported running %v, want [%s]", report.Running, p.ID)
	}
	if len(report.Stopped) != 0 {
		t.Errorf("Reconcile reported %v stopped, want nothing", report.Stopped)
	}
	if len(report.Orphans) != 0 {
		t.Errorf("Reconcile reported %v as orphaned, want nothing", report.Orphans)
	}

	// Adopted means attached: output arrives without anybody pressing start.
	waitForStatus(t, second, p.ID, project.StatusRunning)
	if err := second.Launch(ctx, p.ID, "echo amx-reconciled"); err != nil {
		t.Fatalf("the adopted runtime did not accept input: %v", err)
	}
	if got := waitForHistory(t, second, p.ID, []byte("amx-reconciled")); !bytes.Contains(got, []byte("amx-reconciled")) {
		t.Errorf("no output arrived from the adopted runtime:\n%q", got)
	}
}

// waitForStatus polls until a project's reported status is want.
func waitForStatus(t *testing.T, m *Manager, projectID, want string) {
	t.Helper()
	deadline := time.Now().Add(outputWait)
	last := ""
	for time.Now().Before(deadline) {
		last = m.ProjectStatus(projectID)
		if last == want {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("the project's status is %q, want %q", last, want)
}

// waitForHistory polls a runtime's buffered output until it contains want.
func waitForHistory(t *testing.T, m *Manager, projectID string, want []byte) []byte {
	t.Helper()
	deadline := time.Now().Add(outputWait)
	var replay []byte
	for time.Now().Before(deadline) {
		history, err := m.History(context.Background(), projectID, 0)
		if err != nil {
			t.Fatalf("History returned an error: %v", err)
		}
		replay = replay[:0]
		for _, c := range history {
			replay = append(replay, c.Data...)
		}
		if bytes.Contains(replay, want) {
			return replay
		}
		time.Sleep(pollInterval)
	}
	return replay
}

// TestManagerReconcileReportsAStoppedProject is Case B: the record exists, the
// session does not, and nothing is started, because a server restart is not a
// request to resume work.
func TestManagerReconcileReportsAStoppedProject(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	p := testProject("p_case_b")
	p.RuntimePath = t.TempDir()

	if err := store.Save(ctx, Record{
		ProjectID: p.ID,
		Backend:   "tmux",
		Session:   p.SessionName(),
		State:     StateRunning,
		Cols:      111,
		Rows:      33,
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("could not seed the store: %v", err)
	}

	m, backend := testManager(t, store, p)

	report, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if len(report.Stopped) != 1 || report.Stopped[0] != p.ID {
		t.Errorf("Reconcile reported stopped %v, want [%s]", report.Stopped, p.ID)
	}
	if len(report.Running) != 0 {
		t.Errorf("Reconcile reported %v as running, want nothing", report.Running)
	}

	// Nothing was started on the user's behalf.
	alive, err := backend.Exists(ctx, p.SessionName())
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if alive {
		t.Error("Reconcile started a session; a restart must not resume work by itself")
	}

	// The recorded size is kept, so the next start is the size the user chose.
	rt, err := m.Runtime(ctx, p.ID)
	if err != nil {
		t.Fatalf("Runtime returned an error: %v", err)
	}
	if rt.Cols != 111 || rt.Rows != 33 {
		t.Errorf("the runtime is %dx%d, want the recorded 111x33", rt.Cols, rt.Rows)
	}
	if got := m.ProjectStatus(p.ID); got != project.StatusStopped {
		t.Errorf("the project's status is %q, want %q", got, project.StatusStopped)
	}
}

// TestManagerReconcileReportsAnOrphan is Case C: a session with no project
// behind it is reported and left alone. It may hold work somebody needs, and
// killing it would be a guess.
func TestManagerReconcileReportsAnOrphan(t *testing.T) {
	ctx := context.Background()

	known := testProject("known")
	known.RuntimePath = t.TempDir()
	m, backend := testManager(t, newFakeStore(), known)

	// A session for a project nobody has a record of, sitting on the known
	// project's own socket. Its name is a real identifier, so the orphan is
	// reported with one.
	deleted := testProjectID("deleted")
	orphanDir := t.TempDir()
	if _, err := backend.Create(ctx, SessionSpec{
		Name: project.SessionNameFor(deleted), Dir: orphanDir, Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatalf("could not create the orphan session: %v", err)
	}

	report, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if len(report.Orphans) != 1 {
		t.Fatalf("Reconcile reported %d orphans, want 1", len(report.Orphans))
	}
	orphan := report.Orphans[0]
	if want := project.SessionNameFor(deleted); orphan.Session != want {
		t.Errorf("the orphan is %q, want %q", orphan.Session, want)
	}
	if orphan.ProjectID != deleted {
		t.Errorf("the orphan names the project %q, want %q", orphan.ProjectID, deleted)
	}
	if orphan.Socket == "" {
		t.Error("the orphan does not name the socket it is on; a session on a per-project socket is only identifiable with it")
	}
	if orphan.Dir != orphanDir {
		t.Errorf("the orphan's directory is %q, want %q", orphan.Dir, orphanDir)
	}

	if got := m.Orphans(); len(got) != 1 || got[0].Session != project.SessionNameFor(deleted) {
		t.Errorf("Orphans() = %v, want the session Reconcile just found", got)
	}

	// It is still running: reporting an orphan is not a licence to kill it.
	alive, err := backend.Exists(ctx, project.SessionNameFor(deleted))
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if !alive {
		t.Error("Reconcile destroyed an orphaned session")
	}
}

// TestManagerReconcileKeepsADeliberatelyStoppedRuntimeStopped checks that a user
// who stopped a runtime is not overruled by a restart. The session outlives the
// server, and reporting it as running would undo their decision.
func TestManagerReconcileKeepsADeliberatelyStoppedRuntimeStopped(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	p := testProject("p_stopped")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, store, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if _, err := m.Stop(ctx, p.ID); err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}

	// The session is still there - Stop does not destroy it - and a restart
	// then looks at it again.
	report, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if len(report.Running) != 0 {
		t.Errorf("Reconcile reported %v as running, but the user stopped it", report.Running)
	}
	if len(report.Stopped) != 1 || report.Stopped[0] != p.ID {
		t.Errorf("Reconcile reported stopped %v, want [%s]", report.Stopped, p.ID)
	}
	if got := m.ProjectStatus(p.ID); got != project.StatusStopped {
		t.Errorf("the project's status is %q, want %q", got, project.StatusStopped)
	}
}

// TestManagerReconcileIsQuietOnAFreshInstall checks the ordinary case: nothing
// recorded, nothing running, nothing to report and no error.
func TestManagerReconcileIsQuietOnAFreshInstall(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_fresh")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, newFakeStore(), p)

	report, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile returned an error on a fresh install: %v", err)
	}
	if len(report.Running)+len(report.Stopped)+len(report.Orphans) != 0 {
		t.Errorf("Reconcile reported %+v on a fresh install, want nothing", report)
	}
}

// TestManagerProjectStatusIsEmptyWithoutARuntime checks the contract the project
// service relies on: "" means "the runtime has nothing to say", which is not the
// same as "stopped".
func TestManagerProjectStatusIsEmptyWithoutARuntime(t *testing.T) {
	p := testProject("p_quiet")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, newFakeStore(), p)

	if got := m.ProjectStatus(p.ID); got != "" {
		t.Errorf("ProjectStatus for a project with no runtime = %q, want the empty string", got)
	}
}

// TestManagerProjectStatusForARunningRuntime checks the observable status of a
// healthy runtime, which is what the frontend renders.
func TestManagerProjectStatusForARunningRuntime(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_status")
	p.RuntimePath = t.TempDir()
	m, _ := testManager(t, newFakeStore(), p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	waitForStatus(t, m, p.ID, project.StatusRunning)

	rt, err := m.Runtime(ctx, p.ID)
	if err != nil {
		t.Fatalf("Runtime returned an error: %v", err)
	}
	if rt.State != StateRunning || !rt.SessionAlive {
		t.Errorf("the runtime is %q with alive=%v, want %q and true", rt.State, rt.SessionAlive, StateRunning)
	}
	if rt.UpdatedAt.IsZero() {
		t.Error("the runtime has no update time")
	}
	if rt.Backend != "tmux" {
		t.Errorf("the runtime's backend is %q, want %q", rt.Backend, "tmux")
	}
}

// TestManagerCloseLeavesSessionsAlone is the persistence requirement at the
// manager's level: shutting the server down must not shut the work down.
func TestManagerCloseLeavesSessionsAlone(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	p := testProject("p_close")
	p.RuntimePath = t.TempDir()
	m, backend := testManager(t, store, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}

	alive, err := backend.Exists(ctx, p.SessionName())
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if !alive {
		// What tmux says separates the two ways this can be false - the session
		// went away, or the server did - and they are different bugs.
		raw, _ := backend.run(ctx, "list-sessions")
		t.Errorf("closing the manager ended the session; the work must outlive the server (tmux says: %q)",
			strings.TrimSpace(raw))
	}

	// And it refuses to start new work once it is shut down.
	if _, err := m.Start(ctx, p.ID); !IsCode(err, CodeUnavailable) {
		t.Errorf("Start after Close failed with %v, want %s", err, CodeUnavailable)
	}
}

// TestManagerStopsClaimingRunningWhenTheServerGoesAway covers the failure the
// runtime cannot prevent: the tmux server itself dies underneath it.
//
// Sessions live inside that server, so they end with it, and there is nothing
// AgentMux can do about that from outside. What it must not do is keep saying
// the work is running. A status line is what a user reads to decide whether
// their terminal is still there, and "Running" about a session that no longer
// exists is worse than no status at all.
func TestManagerStopsClaimingRunningWhenTheServerGoesAway(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	p := testProject("p_server_gone")
	p.RuntimePath = t.TempDir()
	m, backend := testManager(t, store, p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	waitForStatus(t, m, p.ID, project.StatusRunning)

	// A server crash is exactly this, seen from the manager's side. Nothing
	// tells the manager; it finds out because its stream ends, which is the
	// whole point of the test.
	if _, err := backend.run(ctx, "kill-server"); err != nil {
		t.Fatalf("kill-server returned an error: %v", err)
	}

	rt, err := m.Runtime(ctx, p.ID)
	if err != nil {
		t.Fatalf("Runtime returned an error: %v", err)
	}
	if rt.SessionAlive {
		t.Error("the runtime still reports a live session after the server died")
	}

	// And the project's advertised status stops saying the work is running. The
	// transition is the pump noticing its stream ended, so it is waited for
	// rather than demanded instantly.
	deadline := time.Now().Add(outputWait)
	for {
		got := m.ProjectStatus(p.ID)
		if got != project.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the project still reports %q after the server died", got)
		}
		time.Sleep(pollInterval)
	}
}

// TestManagerFileWritesAreVisibleToTheServer ties the two halves of the runtime
// together: a session's files land on the filesystem the server can see, which
// is what makes a registered project a project the agent can work in. It is
// trivially true on one machine, and it is exactly the assumption that Windows
// plus WSL depends on, so it is worth stating once.
func TestManagerFileWritesAreVisibleToTheServer(t *testing.T) {
	ctx := context.Background()

	p := testProject("p_files")
	dir := t.TempDir()
	p.RuntimePath = dir
	m, _ := testManager(t, newFakeStore(), p)

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	created := filepath.Join(dir, "written-by-the-session")
	if err := m.Launch(ctx, p.ID, "echo hello > "+created); err != nil {
		t.Fatalf("could not type into the runtime: %v", err)
	}

	got := waitForFile(t, created, readinessWait, func(d []byte) bool {
		return bytes.Contains(d, []byte("hello"))
	})
	if strings.TrimSpace(string(got)) != "hello" {
		t.Errorf("the file the session wrote holds %q, want %q", got, "hello\n")
	}
}

// TestManagerRejectsAnIncompleteConfiguration checks the constructor's guards,
// since a manager missing a store would fail much later and less clearly.
func TestManagerRejectsAnIncompleteConfiguration(t *testing.T) {
	projects := newFakeProjects(testProject("p_x"))
	store := newFakeStore()
	runtimes := testRuntimes(t, uniqueSocketDir(t))

	cases := []struct {
		name string
		opts ManagerOptions
	}{
		{"no backend", ManagerOptions{Sockets: runtimes.Sockets(), Projects: projects, Store: store}},
		{"no sockets", ManagerOptions{Backends: runtimes, Projects: projects, Store: store}},
		{"no projects", ManagerOptions{Backends: runtimes, Sockets: runtimes.Sockets(), Store: store}},
		{"no store", ManagerOptions{Backends: runtimes, Sockets: runtimes.Sockets(), Projects: projects}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewManager(tc.opts); err == nil {
				t.Error("NewManager accepted an incomplete configuration")
			}
		})
	}
}

// TestTestProjectStaysOutOfRealProjects is a check on the tests themselves.
//
// The runtime path must be a temporary directory so that running the suite never
// creates anything under a real Projects Root. This asserts the helper rather
// than the code, because the failure it guards against - a test that quietly
// registers a project in somebody's working tree - is one nobody would notice
// until it had happened many times.
func TestTestProjectStaysOutOfRealProjects(t *testing.T) {
	p := testProject("p_temp")
	if !strings.HasPrefix(p.RuntimePath, "/tmp/") {
		t.Errorf("testProject builds a runtime path outside /tmp: %q", p.RuntimePath)
	}
	if !strings.HasPrefix(p.HostPath, `D:\AI\Projects\2026 AgentMux\`) {
		t.Errorf("testProject's host path is %q, which is not the shape these tests mean to mimic", p.HostPath)
	}
	if p.CollectionPath == p.RuntimePath {
		t.Error("testProject's collection and runtime path are the same, so the working-directory test proves nothing")
	}
}
