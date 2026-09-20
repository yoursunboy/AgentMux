package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// This file reads the CLI's machine-readable output.
//
// # What the stream is
//
// Run with `--output-format stream-json --verbose --include-hook-events`, the
// Claude Code CLI writes newline-delimited JSON to stdout: one object per line,
// each describing something that happened. Phase 7.3A and 7.3B-0 both measured
// the shape, and the four message kinds this phase reads are `system/init`,
// `system/hook_started`, `system/hook_response` and `result`.
//
// # Why it is read at all, when the hooks already arrive
//
// Because the stream is the only thing that says how a turn *ended*. Hooks
// report that a session started, that a prompt was submitted and that the model
// stopped; none of them reports whether the work succeeded. The `result`
// envelope does, in `is_error`, and that single field is the difference between
// `agent.completed` and `agent.failed`.
//
// The stream is also the second, independent path to the session id. A
// `system/init` message carries it before any hook has necessarily been
// processed, which is what lets a session be correlated even when the
// `SessionStart` command hook failed to run.
//
// # What is not read
//
// The stream carries the whole conversation: assistant messages, tool results,
// and - because `--include-hook-events` was asked for - the *output of every
// hook*, echoed back verbatim. None of it has a field in streamMessage. A line
// that is not one of the four kinds is decoded and discarded, and its content
// is never held as a value, never logged, and never stored.

// streamLineMax bounds one line.
//
// A stream line is an assistant message as often as it is a control message, so
// the bound has to clear a large tool result; four mebibytes does. A line past
// it is skipped rather than fatal. The default a bufio.Scanner would apply - 64
// KiB - is small enough that a single long message would end the read, which is
// the failure mode this bound exists to avoid.
const streamLineMax = 4 << 20

// streamMessage is the whole of what the adapter reads out of a stream line.
//
// `result` is absent on purpose. So are `message`, `output` and `stdout`: those
// are the fields that carry conversation text and hook output, and a struct
// without them cannot pass any of it on.
type streamMessage struct {
	Type string `json:"type"`

	// Subtype discriminates a `system` message: "init", "hook_started",
	// "hook_response". It also appears on `result`, where it is *not* read -
	// see resultOutcome.
	Subtype string `json:"subtype"`

	// SessionID is Claude's session id, present on `system/init` and on the
	// final `result`.
	SessionID string `json:"session_id"`

	// IsError is the result envelope's outcome, and the only field that decides
	// whether a turn is recorded as completed or failed.
	IsError bool `json:"is_error"`

	// TerminalReason says why a turn ended the way it did, for example
	// "api_error". It is recorded beside IsError because the pair is the whole
	// of what the envelope actually claims.
	TerminalReason string `json:"terminal_reason"`

	// HookName identifies a hook in a `hook_started` or `hook_response`
	// message, for example "SessionStart:startup".
	HookName string `json:"hook_name"`
}

// streamEvent reduces one stream message to an observation.
//
// It reports false for every message this phase does not read, which is most of
// them: assistant messages, user messages, tool results, and any system subtype
// added by a later CLI version.
func streamEvent(m streamMessage, projectID, runtimeID string, now time.Time) (Event, bool) {
	event := Event{ProjectID: projectID, RuntimeID: runtimeID, SessionID: clip(m.SessionID), CreatedAt: now}

	switch m.Type {
	case "system":
		switch m.Subtype {
		case "init":
			event.Kind = KindInitialized
			event.Payload = payloadOf(map[string]any{"event": string(KindInitialized)})
		case "hook_started":
			event.Kind = KindHookStarted
			event.Payload = payloadOf(map[string]any{
				"event": string(KindHookStarted),
				"hook":  clip(m.HookName),
			})
		case "hook_response":
			event.Kind = KindHookResponse
			event.Payload = payloadOf(map[string]any{
				"event": string(KindHookResponse),
				"hook":  clip(m.HookName),
			})
		default:
			return Event{}, false
		}
	case "result":
		event.Kind = KindResult
		// is_error is written even when it is false. Its presence is the
		// claim: a reader of the row sees the envelope said "no error",
		// rather than seeing a field that was dropped because it was zero.
		event.Payload = payloadOf(map[string]any{
			"event":          string(KindResult),
			"isError":        m.IsError,
			"terminalReason": clip(m.TerminalReason),
		})
	default:
		return Event{}, false
	}
	return event, true
}

// ConsumeStream reads the CLI's stream-json output until it ends.
//
// It is called with the read end of whatever the caller started the CLI with -
// a pipe, in ordinary use - and it returns when that reader ends, when it is
// closed, or when the context is cancelled between lines. A cancelled context
// is not a failure and is not reported as one: the caller asking the read to
// stop is not a fault in the stream.
//
// # One bad line does not end the stream
//
// A line that is not JSON, or that is longer than the reader will hold, is
// skipped and counted, and the read continues. That direction is chosen
// deliberately: the stream is the only source of the turn's outcome, and
// ending it over one stray line would lose the `result` envelope that follows -
// turning a cosmetic problem on stdout into a session whose completion is never
// recorded. The counts are reported in the log when the stream ends, so a
// stream that was mostly noise is visible rather than merely survived.
//
// A line is never logged. A stream line is as likely to be an assistant message
// as a control message, and the log is not a place conversation text may reach.
// What is logged is the byte offset and the decoder's reason.
func (a *Adapter) ConsumeStream(ctx context.Context, r io.Reader) error {
	a.mu.Lock()
	cfg, started := a.cfg, a.started
	a.mu.Unlock()
	if !started {
		return newError(CodeNotStarted, "the adapter has not started, so it cannot attribute a stream")
	}
	if r == nil {
		return newError(CodeStreamBroken, "there is no stream to read")
	}

	reader := bufio.NewReaderSize(r, 64<<10)
	var (
		// line counts every line read, including the ones that were skipped,
		// so that a log line can say where in the stream something happened.
		// It is a count and not a byte offset: a skipped line is discarded as
		// it is read, and its length is not something this loop ever learns.
		line      int
		decoded   int
		malformed int
		oversized int
		// session is the most recent session id seen on the stream. The
		// `result` and `hook_*` messages do not all carry one, and the stream
		// belongs to exactly one session for its whole length, so the last one
		// seen is the one they belong to.
		session string
	)

	for {
		if err := ctx.Err(); err != nil {
			a.logStreamSummary(decoded, malformed, oversized, true)
			return nil
		}

		lineText, err := readStreamLine(reader, streamLineMax)
		line++
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				a.logStreamSummary(decoded, malformed, oversized, false)
				return nil
			case errors.Is(err, errLineTooLong):
				oversized++
				// Expected: a large assistant message or tool result. It is
				// counted rather than warned about, because it is not by
				// itself a problem.
				a.log.Debug("skipped an oversized stream-json line", "line", line, "limit", streamLineMax)
				continue
			default:
				a.logStreamSummary(decoded, malformed, oversized, false)
				return wrapError(err, CodeStreamBroken, "could not read the claude stream after %d lines", line)
			}
		}

		if len(bytes.TrimSpace(lineText)) == 0 {
			continue
		}

		var message streamMessage
		if err := json.Unmarshal(lineText, &message); err != nil {
			// Anomalous: the CLI writes one JSON object per line on stdout.
			// Whatever it was, it is not repeated into the log.
			malformed++
			a.log.Warn("skipped a stream-json line that is not JSON",
				"line", line, "reason", decodeReason(err))
			continue
		}
		decoded++

		ev, ok := streamEvent(message, cfg.ProjectID, cfg.RuntimeID, a.now())
		if !ok {
			continue
		}
		if ev.SessionID != "" {
			session = ev.SessionID
		} else {
			ev.SessionID = session
		}
		a.observe(ctx, ev)
	}
}

// logStreamSummary reports what a completed read saw.
//
// It is one line per stream rather than one per message, so that a stream that
// ended early, or one that was mostly noise, leaves a record without every
// healthy stream writing a line per message.
func (a *Adapter) logStreamSummary(decoded, malformed, oversized int, cancelled bool) {
	level := a.log.Info
	if malformed > 0 {
		level = a.log.Warn
	}
	level("claude stream ended",
		"decoded", decoded, "malformed", malformed, "oversized", oversized, "cancelled", cancelled)
}

// decodeReason turns a JSON decode failure into the part of it a caller can act
// on, without quoting the document it failed on.
func decodeReason(err error) string {
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	switch {
	case errors.As(err, &typeErr):
		return "field " + typeErr.Field + " has the wrong type"
	case errors.As(err, &syntaxErr):
		return "not valid JSON"
	default:
		return err.Error()
	}
}

// errLineTooLong is returned by readStreamLine for a line past the bound. The
// line has already been consumed and discarded by the time it is returned.
var errLineTooLong = errors.New("claude: stream-json line is longer than the reader accepts")

// readStreamLine reads one newline-terminated line, up to max bytes.
//
// It is a hand-written loop rather than a bufio.Scanner because a Scanner's
// reaction to a line past its buffer is to stop scanning with an error, and the
// caller's reaction to that would be to end the stream. Here the excess is
// discarded and the line is reported as skipped, so the read continues.
//
// The returned slice is a copy and does not alias the reader's buffer. The
// trailing newline is not included; a carriage return is, and is trimmed, so
// that a stream captured on Windows reads the same as one captured on Linux.
func readStreamLine(r *bufio.Reader, max int) ([]byte, error) {
	var (
		buf      []byte
		overflow bool
	)
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > max {
			overflow = true
		}
		if !overflow {
			buf = append(buf, chunk...)
		}

		switch {
		case err == nil:
			if overflow {
				return nil, errLineTooLong
			}
			return bytes.TrimRight(buf, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			// A partial line; keep reading.
			continue
		case errors.Is(err, io.EOF):
			if overflow {
				return nil, errLineTooLong
			}
			if len(buf) == 0 {
				return nil, io.EOF
			}
			// A final line with no trailing newline is still a line.
			return bytes.TrimRight(buf, "\r\n"), nil
		default:
			return nil, err
		}
	}
}
