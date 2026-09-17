package session

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// These tests are the point of Phase 2.5.
//
// Phase 2 put every project's session on one tmux server, which made the fault
// domain the whole installation: one server dying took every terminal with it,
// and AgentMux could not tell "this project's work ended" from "the runtime
// died". One server per project is the change, and the claim that has to be
// measured - not asserted in a comment - is that a fault in one project stops
// there.
//
// The measurement is deliberately destructive: a real server is killed, and the
// other projects are required to keep running, keep their sessions, and keep
// producing output. A test that only started three projects and checked they
// had three sockets would pass on an architecture that shared nothing until the
// first fault.

// isolatedProjects is several projects on one manager, each with its own socket.
type isolatedProjects struct {
	manager  *Manager
	layout   *SocketDir
	projects []*project.Project
}

// newIsolatedProjects builds a manager over one project per label.
func newIsolatedProjects(t *testing.T, labels ...string) *isolatedProjects {
	t.Helper()
	return newIsolatedProjectsOn(t, uniqueSocketDir(t), newFakeStore(), labels...)
}

// newIsolatedProjectsOn is the same over a socket directory the caller chose, so
// that a test can restart AgentMux over the servers the previous manager left.
func newIsolatedProjectsOn(t *testing.T, socketDir string, store RuntimeStore, labels ...string) *isolatedProjects {
	t.Helper()

	runtimes := testRuntimes(t, socketDir)
	projects := make([]*project.Project, 0, len(labels))
	for _, label := range labels {
		p := testProject(label)
		p.RuntimePath = t.TempDir()
		projects = append(projects, p)
	}

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

	return &isolatedProjects{manager: m, layout: runtimes.Sockets(), projects: projects}
}

// socketPath is where a project's server listens.
func (f *isolatedProjects) socketPath(p *project.Project) string {
	return f.layout.Path(p.ID)
}

// backend returns a handle on a project's own socket, for reaching past the
// manager - killing a server, asking tmux directly - without going through the
// thing under test.
func (f *isolatedProjects) backend(t *testing.T, p *project.Project) *TmuxBackend {
	t.Helper()
	return newTestBackendOn(t, f.socketPath(p))
}

// startAll starts every project and requires each to reach Running.
func (f *isolatedProjects) startAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, p := range f.projects {
		if _, err := f.manager.Start(ctx, p.ID); err != nil {
			t.Fatalf("starting %s failed: %v", p.ID, err)
		}
		waitForStatus(t, f.manager, p.ID, project.StatusRunning)
	}
}

// mark types a token into a project's session and waits until its own output
// history carries it, so that "this project is still working" is a measurement
// rather than a status field.
func (f *isolatedProjects) mark(t *testing.T, p *project.Project, token string) {
	t.Helper()
	ctx := context.Background()
	if err := f.manager.Launch(ctx, p.ID, "echo "+token); err != nil {
		t.Fatalf("could not type into %s: %v", p.ID, err)
	}
	if got := waitForHistory(t, f.manager, p.ID, []byte(token)); !bytes.Contains(got, []byte(token)) {
		t.Fatalf("%s never echoed %q after the operation under test; its runtime is no longer observed", p.ID, token)
	}
}

// TestKillingOneProjectsServerLeavesTheOthersRunning is the fault-isolation
// measurement.
//
// If this fails - if killing one project's server takes another project's
// terminal with it - then Phase 2.5 has not achieved what it was for, whatever
// else passes.
func TestKillingOneProjectsServerLeavesTheOthersRunning(t *testing.T) {
	ctx := context.Background()
	f := newIsolatedProjects(t, "alpha", "bravo", "charlie")
	f.startAll(t)

	// Every project has work in it that would be lost, and a token in its
	// history that proves it is being observed.
	for _, p := range f.projects {
		f.mark(t, p, "amx-before-"+p.ID)
	}

	victim := f.projects[1]
	survivors := []*project.Project{f.projects[0], f.projects[2]}

	// The fault: one server, killed the way a crash kills it. Nothing tells the
	// manager; it finds out because its stream ends.
	if _, err := f.backend(t, victim).run(ctx, "kill-server"); err != nil {
		t.Fatalf("could not kill %s's server: %v", victim.ID, err)
	}

	// The victim must stop being reported as running. A status line is what a
	// user reads to decide whether their terminal is still there.
	deadline := time.Now().Add(outputWait)
	for f.manager.ProjectStatus(victim.ID) == project.StatusRunning {
		if time.Now().After(deadline) {
			t.Fatalf("%s still reports %q after its server died", victim.ID, project.StatusRunning)
		}
		time.Sleep(pollInterval)
	}

	// The survivors must be untouched: same state, live sessions, and still
	// delivering output through their own control monitors.
	for _, p := range survivors {
		if got := f.manager.ProjectStatus(p.ID); got != project.StatusRunning {
			t.Errorf("%s is %q after another project's server was killed, want %q", p.ID, got, project.StatusRunning)
		}

		alive, err := f.backend(t, p).Exists(ctx, p.SessionName())
		if err != nil {
			t.Fatalf("could not ask whether %s's session is alive: %v", p.ID, err)
		}
		if !alive {
			t.Fatalf("%s's session ended when another project's server was killed", p.ID)
		}

		if probe := f.layout.Probe(ctx, f.socketPath(p)); probe.State != SocketLive {
			t.Errorf("%s's server is %q, want %q", p.ID, probe.State, SocketLive)
		}

		f.mark(t, p, "amx-after-"+p.ID)
	}
}

// TestDestroyingOneProjectLeavesTheOthersAlone is the same isolation for the
// deliberate act rather than the accidental one.
//
// Destroy is the most destructive thing AgentMux does to a project. It must
// reach that project's server and nothing else - not another project's, and
// certainly not every tmux on the machine.
func TestDestroyingOneProjectLeavesTheOthersAlone(t *testing.T) {
	ctx := context.Background()
	f := newIsolatedProjects(t, "alpha", "bravo", "charlie")
	f.startAll(t)

	victim := f.projects[1]
	survivors := []*project.Project{f.projects[0], f.projects[2]}

	if err := f.manager.Destroy(ctx, victim.ID); err != nil {
		t.Fatalf("Destroy returned an error: %v", err)
	}

	// The victim's session is gone and its socket file with it. A destroyed
	// project should not leave a name in the socket directory for the next
	// reconciliation to puzzle over.
	alive, err := f.backend(t, victim).Exists(ctx, victim.SessionName())
	if err != nil {
		t.Fatalf("Exists returned an error: %v", err)
	}
	if alive {
		t.Error("Destroy left the project's session running")
	}
	if probe := f.layout.Probe(ctx, f.socketPath(victim)); probe.State == SocketLive {
		t.Error("Destroy left the project's tmux server running")
	}

	// Its record is gone too, so nothing downstream keeps a phantom runtime for
	// a project that is not there. A reconciliation reporting it as stopped is
	// the shape the bug would take: a client would offer to restart a project it
	// had just been told was destroyed.
	report, err := f.manager.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if contains(report.Running, victim.ID) || contains(report.Stopped, victim.ID) {
		t.Errorf("Reconcile still accounts for the destroyed project %s", victim.ID)
	}

	for _, p := range survivors {
		if got := f.manager.ProjectStatus(p.ID); got != project.StatusRunning {
			t.Errorf("%s is %q after another project was destroyed, want %q", p.ID, got, project.StatusRunning)
		}
		survivorAlive, err := f.backend(t, p).Exists(ctx, p.SessionName())
		if err != nil {
			t.Fatalf("could not ask whether %s's session is alive: %v", p.ID, err)
		}
		if !survivorAlive {
			t.Fatalf("destroying %s ended %s's session", victim.ID, p.ID)
		}
		if probe := f.layout.Probe(ctx, f.socketPath(p)); probe.State != SocketLive {
			t.Errorf("%s's server is %q after another project was destroyed, want %q", p.ID, probe.State, SocketLive)
		}
		f.mark(t, p, "amx-survives-"+p.ID)
	}
}

// TestProjectsDoNotShareInputOrOutput checks the everyday case the fault tests
// depend on: three sessions on three servers, none of them seeing the others'
// bytes.
//
// It is asserted separately from the fault tests because it is a different
// failure with the same shape - a runtime that typed into the wrong server would
// show up here long before it showed up as a crash, and the fault tests would
// then be measuring a system that was already wrong.
func TestProjectsDoNotShareInputOrOutput(t *testing.T) {
	f := newIsolatedProjects(t, "alpha", "bravo", "charlie")
	f.startAll(t)

	for i, p := range f.projects {
		token := fmt.Sprintf("amx-only-%d", i)
		if err := f.manager.Launch(context.Background(), p.ID, "echo "+token); err != nil {
			t.Fatalf("could not type into %s: %v", p.ID, err)
		}
		if got := waitForHistory(t, f.manager, p.ID, []byte(token)); !bytes.Contains(got, []byte(token)) {
			t.Errorf("%s never received its own input", p.ID)
		}
	}

	for i, p := range f.projects {
		for j := range f.projects {
			if i == j {
				continue
			}
			other := fmt.Sprintf("amx-only-%d", j)
			history, err := f.manager.History(context.Background(), p.ID, 0)
			if err != nil {
				t.Fatalf("History returned an error: %v", err)
			}
			for _, chunk := range history {
				if bytes.Contains(chunk.Data, []byte(other)) {
					t.Errorf("output typed into project %d appeared in project %d", j, i)
				}
			}
		}
	}
}

// TestResizingOneProjectDoesNotTouchTheOthers checks the third thing a shared
// server used to share.
//
// A pty size is a property of a session, but a control-mode client that imposed
// its own window size, or a server-wide default that a resize command reached,
// would make one project's resize another project's problem.
func TestResizingOneProjectDoesNotTouchTheOthers(t *testing.T) {
	ctx := context.Background()
	f := newIsolatedProjects(t, "alpha", "bravo", "charlie")
	f.startAll(t)

	const (
		wideCols, wideRows = 132, 43
		baseCols, baseRows = DefaultCols, DefaultRows
	)

	if _, err := f.manager.Resize(ctx, f.projects[0].ID, wideCols, wideRows); err != nil {
		t.Fatalf("Resize returned an error: %v", err)
	}

	// Read the sizes back from tmux itself rather than from the runtime's
	// fields: a field that was updated while the pty was not is exactly the bug
	// this is looking for.
	if got := paneSize(t, f.backend(t, f.projects[0]), f.projects[0].SessionName()); got != fmt.Sprintf("%dx%d", wideCols, wideRows) {
		t.Errorf("the resized project is %s, want %dx%d", got, wideCols, wideRows)
	}
	for _, p := range f.projects[1:] {
		want := fmt.Sprintf("%dx%d", baseCols, baseRows)
		if got := paneSize(t, f.backend(t, p), p.SessionName()); got != want {
			t.Errorf("%s is %s after another project was resized, want %s", p.ID, got, want)
		}
	}
}

// paneSize reads a session's real terminal size from tmux.
func paneSize(t *testing.T, backend *TmuxBackend, name string) string {
	t.Helper()
	out, err := backend.run(context.Background(), "list-panes", "-t", name, "-F", "#{pane_width}x#{pane_height}")
	if err != nil {
		t.Fatalf("could not read the size of %q: %v", name, err)
	}
	return strings.TrimSpace(firstLine(out))
}

// TestRestartingAgentMuxLeavesEveryProjectRunning is the restart requirement.
//
// Shutting AgentMux down is not a request to end anyone's work: the servers own
// the sessions, and AgentMux is a client. The second half is the harder one -
// when it comes back it has to find all three, adopt them, and start observing
// them again.
func TestRestartingAgentMuxLeavesEveryProjectRunning(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	socketDir := uniqueSocketDir(t)

	first := newIsolatedProjectsOn(t, socketDir, store, "alpha", "bravo", "charlie")
	first.startAll(t)
	for _, p := range first.projects {
		first.mark(t, p, "amx-before-restart-"+p.ID)
	}

	// AgentMux stops. Nothing is asked of the sessions.
	if err := first.manager.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}

	for _, p := range first.projects {
		alive, err := first.backend(t, p).Exists(ctx, p.SessionName())
		if err != nil {
			t.Fatalf("could not ask whether %s's session is alive: %v", p.ID, err)
		}
		if !alive {
			t.Fatalf("%s's session ended when AgentMux shut down", p.ID)
		}
		if probe := first.layout.Probe(ctx, first.socketPath(p)); probe.State != SocketLive {
			t.Errorf("%s's server is %q after AgentMux shut down, want %q", p.ID, probe.State, SocketLive)
		}
	}

	// AgentMux comes back, with the same socket directory and the same database.
	second := newIsolatedProjectsOn(t, socketDir, store, "alpha", "bravo", "charlie")

	report, err := second.manager.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if len(report.Running) != len(second.projects) {
		t.Errorf("Reconcile reported running %v, want all %d projects", report.Running, len(second.projects))
	}
	if len(report.Stopped) != 0 {
		t.Errorf("Reconcile reported %v stopped, want nothing", report.Stopped)
	}
	if len(report.Orphans) != 0 {
		t.Errorf("Reconcile reported %v orphaned, want nothing", report.Orphans)
	}

	// And the adopted runtimes are really being observed, not merely adopted.
	for _, p := range second.projects {
		waitForStatus(t, second.manager, p.ID, project.StatusRunning)
		second.mark(t, p, "amx-after-restart-"+p.ID)
	}
}

// TestControlMonitorReconnectsUnderRepeatedDrops is the Control Monitor stress,
// quantified.
//
// The fault injected here is the one the monitor exists to survive: its control
// client goes away while the server and the session stay up. That is what a
// network interruption, a suspended laptop, or a tmux client being killed looks
// like from inside AgentMux. The monitor must re-establish, and re-establishing
// must never be a reason to end the runtime - the tmux server owns the session,
// and the monitor is only a reader.
//
// The counts are reported rather than summarised, because "the monitor is fine"
// is not a measurement: reconnects happened or they did not, and servers died or
// they did not.
//
// What is asserted is what the architecture promises, and the promise has an
// edge that has to be stated rather than discovered. A control-mode client is a
// live stream and not a buffer: tmux sends a client nothing that happened before
// it attached, so output produced between the drop and the re-establish is not
// recoverable, and AgentMux takes no snapshot on reconnect. The test therefore
// waits for the replacement client to be attached before it types anything;
// typing earlier would measure that gap rather than the reconnect. See the
// Control Mode section of docs/RUNTIME.md for the gap and for the remedy the
// protocol does allow.
func TestControlMonitorReconnectsUnderRepeatedDrops(t *testing.T) {
	ctx := context.Background()
	f := newIsolatedProjects(t, "watched")
	p := f.projects[0]
	f.startAll(t)

	// Twelve rounds is a smoke test, not a stress measurement. The number is
	// raised through the same variable the stability harness uses so that a
	// documented run can report a count worth quoting, while the default suite
	// stays quick enough to run on every change.
	rounds := 12
	if n := envInt(stressEnvPrefix + "MONITOR_ROUNDS"); n > 0 {
		rounds = n
	}

	dropped := 0
	serverDeaths := 0
	runtimeDeaths := 0
	slowest := time.Duration(0)
	for round := 0; round < rounds; round++ {
		token := fmt.Sprintf("amx-round-%d", round)
		if err := f.manager.Launch(ctx, p.ID, "echo "+token); err != nil {
			t.Fatalf("round %d: could not type into the session: %v", round, err)
		}
		waitForHistory(t, f.manager, p.ID, []byte(token))

		killed, err := dropControlClients(ctx, f.socketPath(p))
		if err != nil {
			t.Fatalf("round %d: could not drop the control clients: %v", round, err)
		}
		if len(killed) == 0 {
			t.Fatalf("round %d: there was no control client to drop, so the monitor was not attached", round)
		}
		dropped += len(killed)

		// The server and the session must both survive the drop.
		if probe := f.layout.Probe(ctx, f.socketPath(p)); probe.State != SocketLive {
			serverDeaths++
			t.Fatalf("round %d: the server is %q after its control client was killed (tmux said: %s)",
				round, probe.State, probe.Detail)
		}

		// The monitor has to come back, and the proof is a client that was not
		// there before. Nothing outside the subscription asks it to reconnect,
		// so a replacement client can only be the monitor rebuilding its own
		// stream - and how long that took is how long the project went
		// unobserved, which is the number this stress exists to produce.
		took, err := waitForNewControlClient(ctx, f.socketPath(p), killed)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if took > slowest {
			slowest = took
		}

		// Only now is it meaningful to type: a stream exists again, so what
		// arrives is the reconnected monitor's output rather than the hole the
		// drop left behind.
		back := fmt.Sprintf("amx-reconnected-%d", round)
		if err := f.manager.Launch(ctx, p.ID, "echo "+back); err != nil {
			t.Fatalf("round %d: could not type after the reconnect: %v", round, err)
		}
		if got := waitForHistory(t, f.manager, p.ID, []byte(back)); !bytes.Contains(got, []byte(back)) {
			t.Fatalf("round %d: the monitor did not resume observing; nothing typed after it reattached arrived", round)
		}

		if got := f.manager.ProjectStatus(p.ID); got != project.StatusRunning {
			runtimeDeaths++
			t.Errorf("round %d: the runtime is %q after its control client was killed, want %q", round, got, project.StatusRunning)
		}
	}

	stats := f.manager.MonitorStats()
	t.Logf("control monitor stress: %d rounds, %d control clients dropped, %d stream reconnects, "+
		"%d subscription reconnects, %d monitor failures, %d server deaths, %d runtime deaths, "+
		"%d monitor(s) attached, slowest reconnect %s",
		rounds, dropped, stats.StreamReconnects, stats.Reconnects, stats.Failures,
		serverDeaths, runtimeDeaths, stats.Active, slowest.Round(time.Millisecond))

	// Every drop is one stream that ended and had to be replaced, so the count
	// of re-established streams is the count of drops at minimum. The counter
	// lives where the reconnection happens - inside the subscription, which is
	// the only place that knows - because a monitor whose client is being killed
	// by something every few seconds goes on reporting a healthy subscription
	// and a live session from above.
	if stats.StreamReconnects < int64(dropped) {
		t.Errorf("the monitor reports %d re-established streams for %d dropped clients; "+
			"every drop is a stream that ended and had to be replaced", stats.StreamReconnects, dropped)
	}
	if stats.Failures != 0 {
		t.Errorf("%d monitor failures during the reconnect stress; a control client going away "+
			"is the fault the monitor is built to absorb", stats.Failures)
	}
	if serverDeaths != 0 {
		t.Errorf("%d server deaths during the reconnect stress; losing a control client must not end a server", serverDeaths)
	}
	if runtimeDeaths != 0 {
		t.Errorf("%d runtime deaths during the reconnect stress; the monitor is not the owner of the runtime", runtimeDeaths)
	}
	if stats.Active != 1 {
		t.Errorf("there are %d control monitors attached, want exactly one per running runtime", stats.Active)
	}
}

// controlClientPIDs lists the pids of the tmux clients attached to a socket.
func controlClientPIDs(ctx context.Context, socketPath string) ([]string, error) {
	out, err := runTmux(ctx, DefaultTmuxBinary, "-S", socketPath, "list-clients", "-F", "#{client_pid}")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// dropControlClients kills every tmux client attached to a socket and reports
// the pids it killed.
//
// Killing the client process is how the monitor's stream ends without the server
// or the session being involved - the fault the monitor has to survive, injected
// deliberately rather than waited for. The pids are returned rather than a count
// so that the caller can tell the replacement client from the one it killed,
// which is the only way to observe a reconnection the manager is designed not to
// announce.
func dropControlClients(ctx context.Context, socketPath string) ([]string, error) {
	pids, err := controlClientPIDs(ctx, socketPath)
	if err != nil {
		return nil, err
	}
	killed := make([]string, 0, len(pids))
	for _, pid := range pids {
		if err := exec.CommandContext(ctx, "kill", "-TERM", pid).Run(); err != nil {
			return killed, fmt.Errorf("could not kill the client %s: %w", pid, err)
		}
		killed = append(killed, pid)
	}
	return killed, nil
}

// waitForNewControlClient waits for a client that was not among the killed ones
// and reports how long that took.
//
// This is the measurement rather than a convenience. A client that appears
// without anything asking the monitor to reconnect can only have come from the
// subscription re-establishing its own stream, and the time it took is the
// window in which the project's output went unwatched.
func waitForNewControlClient(ctx context.Context, socketPath string, killed []string) (time.Duration, error) {
	started := time.Now()
	deadline := started.Add(outputWait)
	for {
		pids, err := controlClientPIDs(ctx, socketPath)
		if err != nil {
			return 0, fmt.Errorf("could not list the control clients: %w", err)
		}
		for _, pid := range pids {
			if !contains(killed, pid) {
				return time.Since(started), nil
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("no control client replaced the %d killed within %s; "+
				"the subscription did not re-establish its stream", len(killed), outputWait)
		}
		if !sleepContext(ctx, pollInterval) {
			return 0, ctx.Err()
		}
	}
}

// TestStaleSocketIsReclaimedButOnlyAfterAProbe is Case D of the reconciliation,
// with the safety property attached.
//
// A destroyed runtime leaves its socket file behind - kill-server does that on
// every version measured - so a stale socket is the ordinary state of a project
// nobody is running. It has to be cleaned up, because a directory that only ever
// grows makes the socket file indistinguishable from a live one at a glance. It
// has to be cleaned up carefully, because the one thing worse than a stale socket
// is deleting a live server's.
func TestStaleSocketIsReclaimedButOnlyAfterAProbe(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()

	p := testProject("stale")
	p.RuntimePath = t.TempDir()

	runtimes := testRuntimes(t, uniqueSocketDir(t))
	m, err := NewManager(ManagerOptions{
		Backends: runtimes,
		Sockets:  runtimes.Sockets(),
		Projects: newFakeProjects(p),
		Store:    store,
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })

	if _, err := m.Start(ctx, p.ID); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	waitForStatus(t, m, p.ID, project.StatusRunning)

	socketPath := runtimes.Sockets().Path(p.ID)
	backend := newTestBackendOn(t, socketPath)
	if err := backend.KillServer(ctx); err != nil {
		t.Fatalf("could not stop the server: %v", err)
	}
	if probe := runtimes.Sockets().Probe(ctx, socketPath); probe.State != SocketStale {
		t.Fatalf("the socket is %q after the server was stopped, want %q (tmux said: %s)",
			probe.State, SocketStale, probe.Detail)
	}

	report, err := m.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
	if len(report.StaleSockets) != 1 || report.StaleSockets[0] != socketPath {
		t.Errorf("Reconcile reported the stale sockets %v, want [%s]", report.StaleSockets, socketPath)
	}
	if len(report.Running) != 0 {
		t.Errorf("Reconcile reported %v as running, but the server is gone", report.Running)
	}
	if !contains(report.Stopped, p.ID) {
		t.Errorf("Reconcile reported %v stopped, want it to include %s", report.Stopped, p.ID)
	}

	if _, err := runTmux(ctx, DefaultTmuxBinary, "-S", socketPath, "list-sessions"); err == nil {
		t.Error("a server is answering on the reclaimed socket")
	}
	if probe := runtimes.Sockets().Probe(ctx, socketPath); probe.State != SocketAbsent {
		t.Errorf("the socket is %q after reconciliation, want %q", probe.State, SocketAbsent)
	}
}

// TestReconcileLeavesASocketItCannotClassify is the refusal that keeps the stale
// cleanup safe.
//
// An unreadable or unclassifiable socket might be a live server AgentMux cannot
// reach. Unlinking it would not stop that server - it would only make it
// unreachable forever, with somebody's work still running inside it.
func TestReconcileLeavesASocketItCannotClassify(t *testing.T) {
	ctx := context.Background()

	// A path that is a directory is the simplest thing tmux cannot answer about
	// in a way that means "there is definitely no server here".
	layout := &SocketDir{dir: t.TempDir(), binary: filepath.Join(t.TempDir(), "no-such-tmux")}
	id := testProjectID("unclassifiable")
	socketPath := layout.Path(id)
	if err := os.MkdirAll(socketPath, 0o700); err != nil {
		t.Fatalf("could not make the path: %v", err)
	}

	probe := layout.Probe(ctx, socketPath)
	if probe.State != SocketUnknown {
		t.Fatalf("the socket is %q, want %q when nothing can be concluded", probe.State, SocketUnknown)
	}
	removed, err := layout.Reclaim(ctx, socketPath)
	if err != nil {
		t.Fatalf("Reclaim returned an error: %v", err)
	}
	if removed {
		t.Error("Reclaim removed a path it could not classify")
	}
	if _, err := os.Lstat(socketPath); err != nil {
		t.Errorf("the path is gone: %v", err)
	}
}

// contains reports whether a list holds a value, for the report assertions.
func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}
