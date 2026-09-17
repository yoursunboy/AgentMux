package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	// Aliased because this package already declares a type named runtime - one
	// project's live runtime state. They are unrelated, and the alias keeps the
	// platform package from colliding with it.
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kutonlagos/agentmux/internal/project"
)

// This file is the Phase 2.5 stability harness.
//
// It exists because Phase 2 measured something it could not explain: on tmux 3.4
// under WSL2, terminating a control-mode client ends the whole tmux server
// roughly once in two hundred attempts. The measurement was real and repeatable;
// what was missing was any way to say whether the variable is the tmux version,
// the WSL2 kernel, or this package's own detach sequence.
//
// So this harness is deliberately one program. The same binary, with the same
// arguments, is run against every environment in the comparison matrix - WSL2
// and plain Linux, old tmux and new - and only the environment changes. A
// harness per environment would answer a different question ("does my harness
// reproduce it?") because each would have its own timing, its own idea of what
// "connected" means, and its own bugs.
//
// # Why it is a test and not a command
//
// It drives the real TmuxBackend, so it exercises the same code the server runs:
// the same argument construction, the same detach sequence, the same re-attach
// policy. A standalone program would have to either import this package - in
// which case it is this test with extra steps - or reimplement the lifecycle, in
// which case it measures the reimplementation.
//
// # Modes
//
// The first run of this harness, on the exact configuration Phase 2 measured,
// lost no servers in 5000 rounds. That is a discrepancy, not a refutation, and
// the difference has to be in *how* the client is torn down. So the round has
// modes, and the matrix is run once per mode until the trigger is found:
//
//	control    a raw control-mode client, detached by closing its stdin
//	kill       a raw control-mode client, killed outright with no grace
//	subscribe  the subscription a manager holds, closed through its own API
//
// # It measures, it does not assert
//
// The harness reports counts and fails only on a structural problem in the
// harness itself, such as a round that neither completed nor was recorded. It
// does not fail because the number is zero and it does not fail because the
// number is not zero: 0 failures in 10,000 rounds is a measurement, not a proof,
// and a test that turned it into a pass would invite exactly the overclaiming
// the numbers are meant to prevent.

// stressEnvPrefix is the prefix of every environment variable the harness reads.
const stressEnvPrefix = "AGENTMUX_STRESS_"

// Stress modes.
const (
	// stressModeControl detaches by closing the client's stdin, which is what
	// the backend does in normal operation.
	stressModeControl = "control"

	// stressModeKill kills the client process without giving it a chance to
	// leave. It is the control for "is it the signal that does it".
	stressModeKill = "kill"

	// stressModeSubscribe goes through the Subscription the manager holds, so
	// the whole re-attach pump is in the loop rather than one stream.
	stressModeSubscribe = "subscribe"

	// stressModeChurn starts a fresh tmux server for every round. It is a
	// different hypothesis from the others: a server that has just started is
	// still initialising, and a client arriving during that window is a
	// different kind of event from a client arriving at a server that has been
	// up for an hour.
	stressModeChurn = "churn"
)

// StressConfig is one run of the harness.
type StressConfig struct {
	// Rounds is how many attach/detach cycles to perform. Zero disables the
	// harness, which is how it stays out of the ordinary suite.
	Rounds int

	// Binary is the tmux executable to exercise. Empty means whatever the
	// backend defaults to, which is "tmux" from PATH.
	Binary string

	// SocketPath is the tmux socket path. Empty means a path unique to this run.
	SocketPath string

	// Mode is one of the stressMode constants.
	Mode string

	// Noisy keeps a command running in the session so that output is in flight
	// while clients come and go. A quiet pane and a busy one are different
	// systems to detach from.
	Noisy bool

	// Settle is how long a freshly attached client is given before it is
	// detached, when the attach is not confirmed.
	Settle time.Duration

	// AttachConfirmTimeout bounds the wait for tmux to report the client as
	// attached. Zero disables confirmation and uses Settle instead.
	AttachConfirmTimeout time.Duration

	// Keep leaves the socket and session behind for inspection.
	Keep bool
}

// Valid reports whether the mode is one this harness implements.
func (c StressConfig) Valid() bool {
	switch c.Mode {
	case stressModeControl, stressModeKill, stressModeSubscribe, stressModeChurn:
		return true
	}
	return false
}

// stressConfigFromEnv reads the harness configuration.
func stressConfigFromEnv() StressConfig {
	return StressConfig{
		Rounds:               envInt(stressEnvPrefix + "ROUNDS"),
		Binary:               strings.TrimSpace(os.Getenv(stressEnvPrefix + "TMUX")),
		SocketPath:           strings.TrimSpace(os.Getenv(stressEnvPrefix + "SOCKET")),
		Mode:                 orDefault(strings.TrimSpace(os.Getenv(stressEnvPrefix+"MODE")), stressModeControl),
		Noisy:                os.Getenv(stressEnvPrefix+"NOISY") != "",
		Settle:               envDuration(stressEnvPrefix+"SETTLE", 50*time.Millisecond),
		AttachConfirmTimeout: envDuration(stressEnvPrefix+"CONFIRM", 2*time.Second),
		Keep:                 os.Getenv(stressEnvPrefix+"KEEP") != "",
	}
}

func envInt(name string) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return 0
	}
	return n
}

func envDuration(name string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(name)))
	if err != nil {
		return fallback
	}
	return d
}

// StressResult is what one run measured. The field names are the column names
// of the comparison matrix in docs/RUNTIME.md.
type StressResult struct {
	TMuxVersion string
	Binary      string
	SocketPath  string
	Platform    string
	Mode        string
	Noisy       bool

	Rounds int

	// Attaches counts control clients that were observed attached before being
	// detached, and Unconfirmed counts the ones detached without tmux ever
	// confirming them. A run with many unconfirmed attaches is measuring
	// something looser than a run with none, so the two are reported apart
	// rather than summed.
	Attaches    int
	Unconfirmed int

	// ServerDeaths counts rounds after which the tmux server was gone. It is
	// the number Phase 2 was chasing.
	ServerDeaths int

	// SessionDeaths counts rounds after which the server was alive but the
	// session was not. A session death is a different event: it is what
	// remain-on-exit and a pane command govern, and Phase 2 already showed it
	// is not what happens here.
	SessionDeaths int

	// FailureRounds lists the rounds in which either death was observed, so
	// that a pattern in the round numbers (every hundredth, say) is visible
	// rather than averaged away.
	FailureRounds []int

	// SlowestDetach is the longest a detach took. Compared against
	// controlDetachGrace it says whether the fallback kill can be the cause.
	SlowestDetach time.Duration

	// DetachesOverGrace counts detaches that outlived controlDetachGrace.
	DetachesOverGrace int

	// FallbackKills is how many times AgentMux actually killed its own control
	// client. It is read from the counter inside close(), so it is the fact
	// itself rather than an inference from timing.
	FallbackKills int64

	// ServerPIDsSeen counts distinct tmux server process ids. More than one
	// means the server was restarted during the run, which is the death signal
	// seen a different way.
	ServerPIDsSeen int

	// ConfirmEnabled records whether the run waited for tmux to report each
	// client attached. It is part of the result because it changes what the
	// result means: an unconfirmed run detaches clients that may never have
	// finished attaching, which is a different experiment.
	ConfirmEnabled bool

	// Stuck counts rounds whose detach had to be abandoned by the harness
	// because it did not return at all. A stuck detach is a hang in AgentMux
	// rather than a tmux problem, and it is counted rather than waited on.
	Stuck int
}

// detachAbandon bounds how long the harness waits for a detach to return before
// declaring it stuck and moving on.
//
// The bound exists because the thing being tested includes a graceful detach
// that is allowed to take controlDetachGrace and then a kill. Anything far
// beyond that is not slow, it is wedged, and a harness that waited forever would
// stop measuring at the first hang - which is exactly the moment worth reporting.
const detachAbandon = 10 * time.Second

// TestControlModeAttachDetachStress is the Phase 2.5 stability harness.
//
// It is skipped unless AGENTMUX_STRESS_ROUNDS is set. Run it in the environment
// under test, not through a wrapper, so that the numbers describe that
// environment:
//
//	AGENTMUX_STRESS_ROUNDS=2000 \
//	AGENTMUX_STRESS_TMUX=/usr/bin/tmux \
//	go test ./internal/session/ -run TestControlModeAttachDetachStress -v -timeout 0
func TestControlModeAttachDetachStress(t *testing.T) {
	cfg := stressConfigFromEnv()
	if cfg.Rounds <= 0 {
		t.Skipf("set %sROUNDS to enable the stability harness", stressEnvPrefix)
	}
	if !cfg.Valid() {
		t.Fatalf("unknown %sMODE %q", stressEnvPrefix, cfg.Mode)
	}

	socket := cfg.SocketPath
	if socket == "" {
		dir, err := os.MkdirTemp("", "amx-stress-")
		if err != nil {
			t.Fatalf("could not make a socket directory for the run: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		socket = filepath.Join(dir, "tmux.sock")
	}
	b := NewTmuxBackend(TmuxOptions{
		Binary:     cfg.Binary,
		SocketPath: socket,
		Logger:     slog.New(slog.DiscardHandler),
	})
	if !cfg.Keep {
		t.Cleanup(func() { _ = b.KillServer(context.Background()) })
	}
	if err := b.Available(context.Background()); err != nil {
		t.Fatalf("the tmux under test is not usable: %v", err)
	}

	ctx := context.Background()
	if err := b.KillServer(ctx); err != nil {
		t.Fatalf("could not start the run from an empty server: %v", err)
	}

	result := StressResult{
		SocketPath:     socket,
		Rounds:         cfg.Rounds,
		Mode:           cfg.Mode,
		Noisy:          cfg.Noisy,
		ConfirmEnabled: cfg.AttachConfirmTimeout > 0,
	}
	result.Binary = b.Binary()
	result.TMuxVersion, _ = b.install.version(ctx)
	result.Platform = platformDescription()

	pids := make(map[string]bool)
	for round := 1; round <= cfg.Rounds; round++ {
		if cfg.Mode == stressModeChurn {
			// A fresh server every round. The socket name is the only thing
			// carried across, which is what makes this a server-startup
			// experiment rather than a client-lifecycle one.
			_ = b.KillServer(ctx)
		}
		name, err := ensureStressSession(ctx, b, round, cfg)
		if err != nil {
			t.Fatalf("round %d: could not establish a session to attach to: %v", round, err)
		}

		outcome := stressRound(ctx, b, name, cfg)
		switch {
		case outcome.stuck:
			result.Stuck++
		case outcome.confirmed:
			result.Attaches++
		default:
			result.Unconfirmed++
		}
		if outcome.detach > result.SlowestDetach {
			result.SlowestDetach = outcome.detach
		}
		if outcome.detach >= controlDetachGrace {
			result.DetachesOverGrace++
		}

		// The state of the world is read after every round, because the event
		// being hunted is one where the next command cannot be sent at all.
		alive, sessionAlive, pid := serverState(ctx, b, name)
		if pid != "" {
			pids[pid] = true
		}
		switch {
		case !alive:
			result.ServerDeaths++
			result.FailureRounds = append(result.FailureRounds, round)
		case !sessionAlive:
			result.SessionDeaths++
			result.FailureRounds = append(result.FailureRounds, round)
		}

		if outcome.stuck {
			// A wedged detach leaves a goroutine and a process behind, and the
			// next round would inherit them. Stopping keeps every number above
			// attributable to the rounds actually measured.
			t.Logf("round %d: detach did not return within %s; stopping the run", round, detachAbandon)
			result.Rounds = round
			break
		}
	}
	result.ServerPIDsSeen = len(pids)
	result.FallbackKills = controlFallbackKills.Load()

	reportStressResult(t, result)
}

// roundOutcome is what one round observed.
type roundOutcome struct {
	confirmed bool
	detach    time.Duration
	stuck     bool
}

// ensureStressSession makes sure a session exists for the next round, creating
// it after a death so the run keeps going and keeps counting.
//
// Continuing after a death is the point: the measurement wanted is a rate, and
// a harness that stopped at the first loss would report "1" for every
// environment.
func ensureStressSession(ctx context.Context, b *TmuxBackend, round int, cfg StressConfig) (string, error) {
	name := project.SessionNameFor("stress")
	if b.sessionExists(ctx, name) {
		return name, nil
	}
	dir, err := os.MkdirTemp("", "amx-stress-")
	if err != nil {
		return "", err
	}
	if _, err := b.Create(ctx, SessionSpec{
		Name: name,
		Dir:  dir,
		Cols: 80,
		Rows: 24,
	}); err != nil {
		return "", fmt.Errorf("round %d: %w", round, err)
	}
	if cfg.Noisy {
		// Output the pane keeps producing. It gives the control client
		// something to be doing at the moment it is detached, which a shell
		// sitting at a prompt does not.
		if err := b.Launch(ctx, name,
			`while true; do printf 'tick %s\n' "$(date +%s%N)"; sleep 0.02; done`); err != nil {
			return "", fmt.Errorf("round %d: could not start the noisy command: %w", round, err)
		}
	}
	return name, nil
}

// stressRound performs one attach-settle-detach and reports what it saw.
func stressRound(ctx context.Context, b *TmuxBackend, name string, cfg StressConfig) roundOutcome {
	switch cfg.Mode {
	case stressModeSubscribe:
		return subscribeRound(ctx, b, name, cfg)
	case stressModeKill:
		return killRound(ctx, b, name, cfg)
	default:
		return controlRound(ctx, b, name, cfg)
	}
}

// controlRound attaches a raw control client and detaches it by closing stdin,
// which is what the backend does in normal operation.
func controlRound(ctx context.Context, b *TmuxBackend, name string, cfg StressConfig) roundOutcome {
	stream, err := b.startControl(ctx, name)
	if err != nil {
		return roundOutcome{}
	}
	out := roundOutcome{confirmed: confirmAttach(ctx, b, cfg)}
	return finishRound(out, func() { stream.close() })
}

// killRound attaches a raw control client and kills it outright.
//
// It is the control for the fallback path: if a run of these loses servers and a
// run of graceful detaches does not, the signal is the trigger rather than the
// detach.
func killRound(ctx context.Context, b *TmuxBackend, name string, cfg StressConfig) roundOutcome {
	stream, err := b.startControl(ctx, name)
	if err != nil {
		return roundOutcome{}
	}
	out := roundOutcome{confirmed: confirmAttach(ctx, b, cfg)}
	return finishRound(out, func() {
		if stream.proc.Process != nil {
			_ = stream.proc.Process.Kill()
		}
		// The reader and the process still have to be reaped, or the round
		// leaks a goroutine and the next round measures a machine with one more
		// of them on it.
		select {
		case <-stream.done:
		case <-time.After(controlDetachGrace):
		}
	})
}

// subscribeRound goes through the Subscription the manager holds.
//
// It is a different experiment from controlRound in a way that matters: the
// subscription owns a pump goroutine that re-attaches whenever its stream drops,
// so closing one is a request to a loop rather than a single teardown.
func subscribeRound(ctx context.Context, b *TmuxBackend, name string, cfg StressConfig) roundOutcome {
	sub, err := b.Attach(ctx, name)
	if err != nil {
		return roundOutcome{}
	}
	out := roundOutcome{confirmed: confirmAttach(ctx, b, cfg)}
	return finishRound(out, func() { _ = sub.Close() })
}

// confirmAttach waits for tmux to report the client, when confirmation is on.
func confirmAttach(ctx context.Context, b *TmuxBackend, cfg StressConfig) bool {
	if cfg.AttachConfirmTimeout <= 0 {
		if cfg.Settle > 0 {
			time.Sleep(cfg.Settle)
		}
		return false
	}
	return waitForClientAttached(ctx, b, cfg.AttachConfirmTimeout)
}

// finishRound times a detach, abandoning it if it never returns.
func finishRound(out roundOutcome, detach func()) roundOutcome {
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		detach()
		done <- time.Since(start)
	}()
	select {
	case d := <-done:
		out.detach = d
	case <-time.After(detachAbandon):
		out.stuck = true
	}
	return out
}

// waitForClientAttached reports whether tmux comes to list a control client.
//
// Confirming the attach matters more than it looks. A round that starts a client
// and immediately closes it is not necessarily testing a detach at all - the
// process may not have reached tmux yet - so a run of unconfirmed rounds could
// report a low failure rate simply because nothing ever attached. Confirmation
// is what makes the rate mean "of the clients that did attach, this many took
// the server with them".
//
// It is also deliberately a separate tmux invocation. Asking over the control
// client itself would only prove the client can talk, not that the server has
// finished admitting it.
func waitForClientAttached(ctx context.Context, b *TmuxBackend, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := b.run(ctx, "list-clients", "-F", "#{client_name}")
		if err == nil && len(strings.TrimSpace(out)) > 0 {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// serverState reports whether the tmux server is alive, whether the named
// session is alive, and the server's process id.
//
// The two questions are separated deliberately. "The server is gone" and "the
// session is gone" are different failures with different causes, and Phase 2
// spent time on the second before a two-session experiment showed it was the
// first. Reading both from one command would blur them together again.
func serverState(ctx context.Context, b *TmuxBackend, name string) (serverAlive, sessionAlive bool, pid string) {
	out, err := b.run(ctx, "list-sessions", "-F", "#{pid}")
	if err != nil || isMissingTarget(out) || strings.TrimSpace(out) == "" {
		return false, false, ""
	}
	serverAlive = true
	pid = strings.TrimSpace(firstLine(out))
	sessionAlive = b.sessionExists(ctx, name)
	return serverAlive, sessionAlive, pid
}

// platformDescription names the environment a run happened in.
//
// It exists because the whole point of the matrix is to compare environments,
// and a row that does not say which one it is cannot be compared with anything.
// The kernel string is what separates WSL2 from plain Linux: a WSL2 kernel
// announces itself, and nothing else about the userspace does. Reading
// /proc/version is the cheapest honest way to ask, and it is allowed to fail -
// on a machine without /proc the answer is simply the Go platform.
func platformDescription() string {
	base := goruntime.GOOS + "/" + goruntime.GOARCH
	raw, err := os.ReadFile("/proc/version")
	if err != nil {
		return base
	}
	version := strings.ToLower(string(raw))
	if strings.Contains(version, "microsoft") || strings.Contains(version, "wsl") {
		return base + " wsl2"
	}
	return base + " linux"
}

// reportStressResult writes the run as the row it will become in the matrix.
//
// It is logged rather than asserted, and it is logged in full even when every
// number is zero: a run of ten thousand rounds with nothing to report is itself
// a result, and one that printed only on failure would leave no evidence that it
// happened.
func reportStressResult(t *testing.T, r StressResult) {
	t.Helper()

	rate := 0.0
	if r.Rounds > 0 {
		rate = float64(r.ServerDeaths) / float64(r.Rounds) * 100
	}
	t.Logf("=== tmux stability harness ===")
	t.Logf("platform        : %s", r.Platform)
	t.Logf("tmux            : %s", r.TMuxVersion)
	t.Logf("binary          : %s", r.Binary)
	t.Logf("socket          : %s", r.SocketPath)
	t.Logf("mode            : %s (noisy=%v, confirm=%v)", r.Mode, r.Noisy, r.ConfirmEnabled)
	t.Logf("rounds          : %d", r.Rounds)
	t.Logf("attaches        : %d (unconfirmed %d)", r.Attaches, r.Unconfirmed)
	t.Logf("server deaths   : %d", r.ServerDeaths)
	t.Logf("session deaths  : %d", r.SessionDeaths)
	t.Logf("failure rounds  : %v", r.FailureRounds)
	t.Logf("failure rate    : %.3f%%", rate)
	t.Logf("slowest detach  : %s", r.SlowestDetach)
	t.Logf("over grace      : %d (grace %s)", r.DetachesOverGrace, controlDetachGrace)
	t.Logf("fallback kills  : %d", r.FallbackKills)
	t.Logf("stuck detaches  : %d", r.Stuck)
	t.Logf("distinct server pids: %d", r.ServerPIDsSeen)

	if r.Stuck > 0 {
		// Not a harness problem: a detach that never returns is a finding, and
		// it is reported rather than asserted away.
		t.Logf("NOTE: %d detach(es) never returned", r.Stuck)
	}
	if r.Attaches+r.Unconfirmed+r.Stuck != r.Rounds {
		t.Errorf("the harness lost count: %d confirmed + %d unconfirmed + %d stuck for %d rounds",
			r.Attaches, r.Unconfirmed, r.Stuck, r.Rounds)
	}
	if r.ConfirmEnabled && r.Attaches == 0 {
		t.Errorf("no control client was ever confirmed attached, so this run measured nothing")
	}
}
