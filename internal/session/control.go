package session

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// This file implements the output half of the tmux backend: a long-lived
// control-mode client (`tmux -C attach-session`).
//
// Why control mode rather than polling
//
// The obvious way to get a terminal's output out of tmux is to run
// `capture-pane` on a timer and diff the result. It is also wrong. Polling
// cannot see more than one screenful, so scrollback is lost; it cannot see
// anything between two ticks, so a spinner redrawing at 30Hz is sampled into
// nonsense; it re-encodes the screen as text, which loses the distinction
// between a carriage return and a newline; and it burns CPU in proportion to
// how many sessions exist multiplied by how often the timer fires, whether or
// not anything happened.
//
// Control mode replaces the timer with the event. tmux pushes `%output` lines
// the moment a pane produces bytes, before any screen state exists to lose.
// The cost is a parser, and the parser is this file.
//
// # The wire format
//
// A control-mode client emits line-oriented records. Lines beginning with '%'
// are asynchronous notifications; every other line belongs to a command
// response. AgentMux sends this client no commands - input travels a different
// path, see tmux.go - so it only ever has to understand notifications.
//
// The one that matters is:
//
//	%output %<pane-id> <payload>
//
// The payload is the pane's bytes with a small, exact escaping applied:
//
//	\            -> \\        (a literal backslash)
//	byte < 0x20  -> \ooo      (three octal digits)
//	byte == 0x7f -> \ooo
//	everything else passes through raw, including every byte >= 0x80
//
// That last clause is the important one and it was verified rather than
// assumed: UTF-8 and arbitrary high bytes arrive unescaped, so the payload
// must be decoded as bytes and never as ASCII. Treating it as text is how the
// first version of this code turned every Chinese character into U+FFFD.
//
// Other notifications are handled or deliberately ignored:
//
//	%begin/%end/%error        command responses; no commands are sent
//	%exit                     the client is going away
//	%pause/%continue          flow control; logged, never required
//	%session-changed and the  lifecycle events another client could act on;
//	rest of the family        this client acts on none of them

// controlChannelDepth is how much output may be queued for one subscription
// before the reader stops draining tmux.
//
// The number is a buffer, not a policy: a consumer that keeps up never fills
// it, and a consumer that does not will apply backpressure to tmux rather than
// have its terminal output silently dropped.
const controlChannelDepth = 1024

// maxCoalescedOutput bounds how much output is merged into a single delivery.
//
// Merging matters because tmux emits one `%output` line per write the pane
// performed, and an interactive program performs a great many small writes. A
// TUI redrawing itself produces hundreds of tiny records that are only
// meaningful together; handing them over as one chunk keeps the sequence
// numbers meaningful and the per-chunk overhead negligible.
const maxCoalescedOutput = 64 * 1024

// controlReadBuffer is the reader's buffer size. It is large because the
// reader's job is to swallow whatever tmux has already produced in as few
// syscalls as possible.
const controlReadBuffer = 64 * 1024

// Re-attach policy for a dropped control client.
const (
	controlInitialBackoff = 100 * time.Millisecond
	controlMaxBackoff     = 5 * time.Second
	controlBackoffFactor  = 2
)

// controlFallbackKills counts how many times a control client had to be killed
// because it did not leave within controlDetachGrace.
//
// The number matters more than it looks. "tmux lost a server after AgentMux
// detached" and "AgentMux shot its own client and tmux lost a server" are
// different findings with different owners, and the difference cannot be seen
// from outside this function: a detach that takes 600ms and a detach that was
// killed at 500ms look the same to a caller. Timing is a proxy; this is the
// fact, so the stability harness reads this rather than inferring.
var controlFallbackKills atomic.Int64

// controlReconnects counts how many times a control stream ended while its
// session was still there and had to be re-established.
//
// It is counted here rather than in the manager because this is where the
// reconnection happens. A subscription outlives its streams on purpose - that
// is what makes a dropped client invisible to everything above - and the price
// of that is that the manager never sees the event. A monitor whose client is
// being killed by something every few seconds looks perfectly healthy from
// above, with a working subscription and a live session, and this is the only
// number that says otherwise.
var controlReconnects atomic.Int64

// controlDetachGrace bounds how long a control client is given to hang up after
// its stdin is closed, before it is killed instead.
//
// It is a bound, not a policy: the client normally leaves in about two
// milliseconds (measured: 300 detaches, slowest 2.2ms), so the kill is there
// only so that a wedged client cannot hang Close, KillServer, or manager
// shutdown forever. It is not what makes detaching safe - see close().
const controlDetachGrace = 500 * time.Millisecond

// startControlStream launches a control-mode client attached to one session
// and begins decoding its output.
//
// The client is deliberately not given a TTY: tmux's control mode is designed
// to be driven over pipes, and the absence of a terminal is what keeps this
// client from imposing its own window size on the session it watches.
func startControlStream(ctx context.Context, bin string, args []string, session string, log *slog.Logger) (*controlStream, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("control: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("control: stdout pipe: %w", err)
	}
	// stderr is captured rather than discarded: "can't find session" and
	// friends arrive here, and they are the difference between a session that
	// ended and a client that failed.
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, remaining: 8 * 1024}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("control: start %s: %w", bin, err)
	}

	stream := &controlStream{
		session: session,
		output:  make(chan []byte, controlChannelDepth),
		done:    make(chan struct{}),
		abort:   make(chan struct{}),
		proc:    cmd,
		stdin:   stdin,
		stderr:  &stderr,
	}
	go stream.read(stdout, log)
	return stream, nil
}

// controlStream is one running control-mode client.
type controlStream struct {
	session string
	output  chan []byte
	done    chan struct{}

	// abort is closed by close(), and it exists for exactly one reason: the
	// reader can block handing a chunk to a consumer that has stopped
	// draining, and a reader that is blocked in a channel send never reaches
	// the process wait that close() is waiting on.
	//
	// Without it this is a deadlock, not a slow path. The reader blocks on
	// `s.output <- chunk`; close() closes stdin, the client exits, but the
	// reader is not reading any more, so it never calls Wait and never closes
	// done; close() waits out its grace, kills a process that is already dead,
	// and then waits on done forever. Measured before the fix: a subscription
	// closed while its consumer had stopped reading hung indefinitely.
	//
	// The window is narrow - the manager's forward loop drains continuously,
	// so it needs a cancellation racing a full channel to open at all - but a
	// hang is the worst failure mode available here, and the Control Monitor
	// work makes closing a stream a routine event rather than an exceptional
	// one.
	abort chan struct{}

	proc   *exec.Cmd
	stdin  io.WriteCloser
	stderr *bytes.Buffer

	mu      sync.Mutex
	err     error
	closing bool
}

// read decodes the client's output until it ends.
func (s *controlStream) read(stdout io.Reader, log *slog.Logger) {
	defer close(s.done)
	defer close(s.output)

	reader := bufio.NewReaderSize(stdout, controlReadBuffer)
	var pending []byte

	// flush hands the accumulated output over, reporting false when the stream
	// is being torn down and the reader should stop. It must not block
	// indefinitely: see abort.
	flush := func() bool {
		if len(pending) == 0 {
			return true
		}
		// The slice is handed over, so the next round must start from a new
		// one; reusing the backing array would let tmux's next write race a
		// consumer that is still reading this chunk.
		chunk := make([]byte, len(pending))
		copy(chunk, pending)
		pending = pending[:0]
		select {
		case s.output <- chunk:
			return true
		case <-s.abort:
			return false
		}
	}

	aborted := false
	for !aborted {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			s.handleLine(bytes.TrimRight(line, "\n"), &pending, log)
		}
		// Everything already in the buffer arrived together, so it is handed
		// over together. Checking the buffer rather than waiting for a timer
		// means a burst of output becomes one chunk and a single keystroke
		// echo is not held back.
		if reader.Buffered() == 0 || len(pending) >= maxCoalescedOutput {
			if !flush() {
				aborted = true
			}
		}
		if err != nil {
			if err != io.EOF && !s.isClosing() {
				s.setErr(fmt.Errorf("control: read: %w", err))
			}
			break
		}
	}

	if !aborted {
		flush()
	}

	// The process is gone; collect why. A clean detach is not an error.
	//
	// This runs even when the reader was aborted, because reaping the child is
	// not optional: a client left unreaped is a zombie for the life of the
	// server.
	_ = s.stdin.Close()
	waitErr := s.proc.Wait()
	if s.isClosing() {
		return
	}
	if waitErr != nil {
		detail := bytes.TrimSpace(s.stderr.Bytes())
		if len(detail) > 0 {
			s.setErr(fmt.Errorf("control: client exited: %w: %s", waitErr, detail))
		} else {
			s.setErr(fmt.Errorf("control: client exited: %w", waitErr))
		}
		return
	}
	s.setErr(fmt.Errorf("control: client exited: %w", ErrSessionExited))
}

// handleLine processes one control-mode record, appending any pane output to
// pending.
func (s *controlStream) handleLine(line []byte, pending *[]byte, log *slog.Logger) {
	if len(line) == 0 {
		return
	}
	if line[0] != '%' {
		// A command response line. AgentMux sends no commands through this
		// client, so this is either stale output or something new in tmux; it
		// is logged rather than guessed at.
		log.Debug("tmux control: unexpected command output", "session", s.session, "line", string(line))
		return
	}

	name, rest := splitControlRecord(line)
	switch name {
	case "output":
		if payload, ok := controlPayload(rest); ok {
			*pending = decodeControlEscapes(*pending, payload)
		} else {
			log.Debug("tmux control: unparseable %output record", "session", s.session, "line", string(line))
		}

	case "extended-output":
		// Same payload, with an age prefix: "%extended-output %0 0 : <data>".
		if payload, ok := extendedOutputPayload(rest); ok {
			*pending = decodeControlEscapes(*pending, payload)
		}

	case "pause":
		// tmux is telling us it has stopped sending because we are behind.
		log.Debug("tmux control: paused", "session", s.session)

	case "continue":
		log.Debug("tmux control: resumed", "session", s.session)

	case "exit":
		log.Debug("tmux control: exit", "session", s.session)

	default:
		// Session, window, and layout lifecycle events. Nothing in Phase 2
		// acts on them, and logging each one at debug level keeps them
		// visible while debugging without making them noise.
		log.Debug("tmux control: notification", "session", s.session, "kind", name)
	}
}

// splitControlRecord splits "%name rest" into its parts.
func splitControlRecord(line []byte) (name string, rest []byte) {
	body := line[1:]
	if i := bytes.IndexByte(body, ' '); i >= 0 {
		return string(body[:i]), body[i+1:]
	}
	return string(body), nil
}

// controlPayload extracts the payload of "%output %<pane> <payload>".
//
// The payload is the remainder of the line, verbatim: it may contain spaces
// and any byte >= 0x20, so it is located by position rather than by scanning
// for a delimiter that could also occur inside it.
func controlPayload(rest []byte) ([]byte, bool) {
	// A pane id is "%" followed by digits.
	if len(rest) == 0 || rest[0] != '%' {
		return nil, false
	}
	space := bytes.IndexByte(rest, ' ')
	if space < 0 {
		return nil, false
	}
	return rest[space+1:], true
}

// extendedOutputPayload extracts the payload of
// "%extended-output %<pane> <age> : <payload>".
func extendedOutputPayload(rest []byte) ([]byte, bool) {
	if payload, ok := controlPayload(rest); ok {
		// The remainder is "<age> : <payload>".
		if i := bytes.Index(payload, []byte(" : ")); i >= 0 {
			return payload[i+3:], true
		}
		return payload, true
	}
	return nil, false
}

// decodeControlEscapes appends src to dst, undoing tmux's control-mode
// escaping.
//
// The rules are the ones tmux applies, and nothing more:
//
//	\\         a literal backslash
//	\ooo       three octal digits, one byte
//
// Every other byte, including every byte >= 0x80, is copied unchanged. This
// function is byte-oriented on purpose: the payload is not text and must not
// be treated as such.
func decodeControlEscapes(dst, src []byte) []byte {
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c != '\\' {
			dst = append(dst, c)
			continue
		}
		if i+1 >= len(src) {
			// A trailing backslash cannot be an escape; tmux always writes
			// escapes in full, so this is data.
			dst = append(dst, c)
			continue
		}
		if src[i+1] == '\\' {
			dst = append(dst, '\\')
			i++
			continue
		}
		if i+3 < len(src) {
			if value, ok := octalByte(src[i+1 : i+4]); ok {
				dst = append(dst, value)
				i += 3
				continue
			}
		}
		// Not a form tmux produces. Copying the backslash through preserves
		// the bytes rather than inventing an interpretation.
		dst = append(dst, c)
	}
	return dst
}

// octalByte decodes exactly three octal digits.
func octalByte(digits []byte) (byte, bool) {
	if len(digits) != 3 {
		return 0, false
	}
	var value byte
	for _, d := range digits {
		if d < '0' || d > '7' {
			return 0, false
		}
		value = value<<3 | (d - '0')
	}
	return value, true
}

// Err returns why the stream ended, or nil when it ended cleanly.
func (s *controlStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *controlStream) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

func (s *controlStream) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

// close terminates the client without touching the session.
//
// Killing the client is the whole detach: the session, its processes, and its
// scrollback live in the tmux server, which is a different process. This is
// what makes the runtime persistent, and it is load-bearing rather than
// incidental.
//
// Closing stdin is therefore tried first, and the kill is the fallback for a
// client that will not go. Asking first is worth the two milliseconds it costs:
// it makes the ordinary detach a request the client acts on, and it keeps
// AgentMux from sending a signal in the case that dominates every other.
//
// What this ordering does not do is make detaching harmless. It was once
// written here that the fallback kill raced the reader's Wait and could signal
// a recycled pid; that was tested and is false. The fallback does not run at
// all in practice - 300 detaches, slowest 2.2ms against a 500ms grace - and the
// rare loss of a tmux server that follows a detach happens with or without it.
// See the "known limitation" section of docs/RUNTIME.md for what was measured
// and what remains unexplained.
func (s *controlStream) close() {
	s.mu.Lock()
	already := s.closing
	s.closing = true
	s.mu.Unlock()
	if already {
		return
	}
	// Released before the grace period rather than after it. The reader may be
	// blocked handing over a chunk nobody is collecting, and closing stdin
	// does not reach it; see abort. Doing this first is what makes the wait
	// below a wait on a client that is actually leaving.
	close(s.abort)
	_ = s.stdin.Close()
	select {
	case <-s.done:
	case <-time.After(controlDetachGrace):
		if s.proc.Process != nil {
			controlFallbackKills.Add(1)
			_ = s.proc.Process.Kill()
		}
		<-s.done
	}
}

// tmuxSubscription keeps one control stream alive for a session, re-attaching
// when it drops.
//
// Re-attachment is not an error path bolted on afterwards; it is the normal
// consequence of the client being a separate process from the session. A
// client can be killed, a laptop can sleep, a tmux server can be restarted out
// from under us. In every case the session is still there and the right answer
// is to attach again.
type tmuxSubscription struct {
	backend *TmuxBackend
	name    string
	log     *slog.Logger

	output chan []byte
	done   chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	// connected tracks whether a control client is live right now. It is
	// written by the pump and read by anyone asking for the runtime's status,
	// so it is atomic rather than guarded by the mutex that protects err.
	connected atomic.Bool

	mu  sync.Mutex
	err error
}

// newTmuxSubscription starts the pump goroutine for one session.
func newTmuxSubscription(parent context.Context, b *TmuxBackend, name string, log *slog.Logger) *tmuxSubscription {
	ctx, cancel := context.WithCancel(parent)
	sub := &tmuxSubscription{
		backend: b,
		name:    name,
		log:     log,
		output:  make(chan []byte, controlChannelDepth),
		done:    make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
	go sub.pump()
	return sub
}

// Name implements Subscription.
func (s *tmuxSubscription) Name() string { return s.name }

// Output implements Subscription.
func (s *tmuxSubscription) Output() <-chan []byte { return s.output }

// Connected implements Subscription.
func (s *tmuxSubscription) Connected() bool { return s.connected.Load() }

// Err implements Subscription.
func (s *tmuxSubscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close implements Subscription.
func (s *tmuxSubscription) Close() error {
	s.cancel()
	<-s.done
	return nil
}

func (s *tmuxSubscription) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// pump attaches, forwards, and re-attaches until the session is gone.
func (s *tmuxSubscription) pump() {
	defer close(s.done)
	defer close(s.output)

	backoff := controlInitialBackoff
	for {
		if s.ctx.Err() != nil {
			return
		}

		// Whether there is anything to stream is asked before a stream is
		// opened, and the order is the point of this loop.
		//
		// `tmux -C attach-session` starts a server when there is none:
		// measured on tmux 3.4 and 3.7c, it prints "no sessions", exits, and
		// leaves the socket file behind. Attaching first and asking afterwards -
		// which is what this used to do - therefore meant that a control
		// monitor whose session had ended created a tmux server on the
		// project's socket in order to discover there was nothing on it. Two
		// things follow, and both were observed rather than reasoned about: a
		// socket file appears on a project whose server had just been confirmed
		// absent, and a probe of that socket running at that moment reaches the
		// newborn server and is told "server exited unexpectedly" - a state
		// that is neither live nor provably stale, so nothing cleans it and a
		// reconciliation reports it as unreadable.
		//
		// A reader of a terminal must not be able to bring a server into
		// existence. The session, not the client, decides whether there is
		// still something to stream.
		if !s.backend.sessionExists(s.ctx, s.name) {
			s.setErr(fmt.Errorf("control: session %q ended: %w", s.name, ErrSessionExited))
			return
		}

		stream, err := s.backend.startControl(s.ctx, s.name)
		established := err == nil
		if established {
			backoff = controlInitialBackoff
			s.connected.Store(true)
			err = s.forward(stream)
			s.connected.Store(false)
			stream.close()
		} else {
			s.log.Warn("tmux control: attach failed", "session", s.name, "error", err)
		}

		if s.ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Warn("tmux control: stream dropped, re-attaching",
				"session", s.name, "retryIn", backoff, "error", err)
		}
		if !sleepContext(s.ctx, backoff) {
			return
		}
		backoff = min(backoff*controlBackoffFactor, controlMaxBackoff)
		if established {
			// A stream existed and ended while the session did not. Whatever
			// the loop does next is a reconnection, and this is the only place
			// that knows it happened: see controlReconnects.
			controlReconnects.Add(1)
		}
	}
}

// forward moves one stream's output to the subscription until it ends.
func (s *tmuxSubscription) forward(stream *controlStream) error {
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case chunk, ok := <-stream.output:
			if !ok {
				return stream.Err()
			}
			select {
			case s.output <- chunk:
			case <-s.ctx.Done():
				return s.ctx.Err()
			}
		}
	}
}

// sleepContext waits for d, reporting false when the context ended first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// limitedWriter keeps a subprocess's stderr from growing without bound. A
// client that fails in a loop could otherwise fill memory with its own
// complaints.
type limitedWriter struct {
	w         io.Writer
	remaining int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return len(p), nil
	}
	if len(p) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.w.Write(p)
	l.remaining -= n
	return n, err
}
