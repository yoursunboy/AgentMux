# The Claude Adapter

Phase 7.3B-1. This document describes the adapter that turns what Claude Code
reports about a session into AgentMux events: what it listens to, what it
records, what it refuses to hold, and what it deliberately does not do.

It is written for the person who has to change it next, and for the person
deciding what Phase 7.3B-2 may build on it. The short version: the adapter
observes. It receives, translates, and writes one row per event through the
event service. It answers nothing, decides nothing, and starts nothing.

Where this document states a measurement it was taken on this host on
2026-09-20; where it states a rule, the rule is enforced in code and has a test
behind it.

## 1. What the adapter is

An adaptation layer, in the sense the brief uses the word. Claude's formats are
Claude's: a hook payload is one JSON object with a `hook_event_name`, a stream is
newline-delimited JSON with a `type` and a `subtype`. None of that shape may
reach AgentMux's database, because the day Claude changes a field name is not
the day AgentMux should stop working.

```text
Claude JSON ──▶ ClaudeAdapter ──▶ AgentMux event model ──▶ EventService ──▶ agent_events
```

The rule that falls out of this, and the one worth holding on to: **nothing
downstream of the adapter knows that Claude exists.** An event's type is
`agent.started`, not `claude.session_start`; its payload names an event and a
tool, not a `hook_event_name` and a `tool_name`. A reader of the event log
cannot tell whether the row came from Claude, from a hook, or from a stream.

## 2. Architecture

```text
                    Claude Code (CLI, any host)
                          │
        ┌─────────────────┴──────────────────┐
        │ hook delivery                      │ stdout
        ▼                                    ▼
  POST /hooks/<nonce>                  newline-delimited JSON
        │                                    │
        ▼                                    ▼
   hookReceiver.ServeHTTP              Adapter.ConsumeStream
   (hooks.go)                          (stream.go)
        │                                    │
        └──────────────┬─────────────────────┘
                       ▼
                 Event  (model.go)
                       │
                  Adapter.observe  (adapter.go)
                       │
        ┌──────────────┼──────────────────────┐
        ▼              ▼                      ▼
    bind()        publish()              noteEvent()
  session map   subscribers          event.Service.CreateEvent
   (memory)      (channels)                    │
                                               ▼
                                        agent_events (SQLite)
```

### Package layout

Everything is in `internal/claude`, which already existed as the Claude Code CLI
launcher. The brief suggested this directory and the repository is flat - one
package per directory, `model.go` / `service.go` / `errors.go` per package - so
the adapter is six files beside the launcher rather than a package beside a
package:

| File | What it holds |
| --- | --- |
| `model.go` | `Kind`, `Event`, `Config`, `Binding` - the internal model |
| `errors.go` | this package's error codes and the shared error shape |
| `adapter.go` | the lifecycle: `Start`, `Stop`, `Subscribe`, `observe`, the session map |
| `hooks.go` | the hook receiver, the hook payload decoder, and the settings document |
| `stream.go` | the stream-json reader |
| `mapper.go` | the translation to AgentMux event types, and the payload rules |
| `events.go` | the one-method recorder interface and the bridge to the event service |

The launcher and the adapter share a package rather than a dependency. The
launcher starts nothing and the adapter starts nothing; neither imports the
other's state.

### The lifecycle

```go
Start(ctx, Config{ProjectID, RuntimeID, AgentSessionID, HookAddr}) error
Subscribe(ctx) (<-chan Event, error)
Stop(ctx) error
```

`Start` binds a loopback listener and returns. It does not start Claude and does
not require one to be running: the receiver is an endpoint, and an endpoint that
exists before the first delivery is what makes a session observable from its
first hook. `Stop` is idempotent, closes every subscriber's channel, and does not
touch Claude - a session being observed keeps running, and what ends is
AgentMux's attention to it.

### One adapter per thing observed

Everything an adapter owns is per-instance: its listener, its hook path, its
session map, its subscribers. Two adapters do not share an address, a path or a
channel, so a hook delivered to one is never attributed to the other.

## 3. How Claude reaches the receiver

Claude Code delivers a hook over whichever handler type its settings declare. Two
are usable here, and **they are not interchangeable**:

- `SessionStart` **accepts a command handler and refuses an HTTP handler.** The
  refusal is silent: the hook is declared, the configuration is accepted, and the
  delivery never arrives. This was reproduced in Phase 7.3B-0 as a negative
  control, in a configuration where three other events were delivered over HTTP
  normally.
- The other four events this phase reads are declared as HTTP handlers.

So `SessionStart` is declared as a command that runs `curl` against the same
endpoint the HTTP handlers post to. Claude's payload is identical either way, and
nothing downstream can tell which transport carried it.

`HookSettings()` renders that document for the caller to write. The adapter does
not write it and does not choose where it goes: a settings file is Claude's
configuration, and a component that decided where to put one would be editing a
user's environment as a side effect of starting. The caller passes the file to
the CLI with `--settings`, which is how every experiment in Phase 7.3A and 7.3B-0
ran without touching any project or user configuration.

The settings document is:

```json
{
  "allowedHttpHookUrls": ["http://127.0.0.1:PORT/hooks/<48 hex characters>"],
  "hooks": {
    "SessionStart":      [{"hooks": [{"type": "command", "command": "curl ... '<url>'"}]}],
    "UserPromptSubmit":  [{"hooks": [{"type": "http", "url": "<url>"}]}],
    "PermissionRequest": [{"hooks": [{"type": "http", "url": "<url>"}]}],
    "Stop":              [{"hooks": [{"type": "http", "url": "<url>"}]}],
    "SessionEnd":        [{"hooks": [{"type": "http", "url": "<url>"}]}]
  }
}
```

`allowedHttpHookUrls` is required. Without it Claude refuses an HTTP hook, and
refuses it quietly. The entry is the receiver's own URL and nothing wider.

### The response is always empty

A Claude hook's stdout **is** its answer: what a hook prints is read as a
decision to allow, a decision to deny, or context to add. This phase makes no
decision, so the body is empty on every status, including the error ones. That
matters because a `SessionStart` hook is delivered by running `curl`, and `curl`
prints a response body to stdout by default - a "400 bad request" body would be
handed back to Claude as session context. Empty bodies everywhere is what makes
an adapter's own error message becoming part of a session's prompt impossible
rather than unlikely.

The write happens before the response. A hook's delivery is not acknowledged
until the event has been offered to the event service, so a caller that sees a
200 is not racing a background write.

### The statuses the receiver answers with

| Status | When | Recorded |
| --- | --- | --- |
| `200` | an event this phase reads, and one it does not | only the first |
| `400` | the body is not a JSON object | no |
| `404` | the path is not the nonce this adapter minted | no |
| `405` | the method is not `POST` | no |
| `413` | the body is larger than 256 KiB | no |
| `503` | the adapter is not observing - it has not started, or has stopped | no |

`503` is the receiver outliving the adapter it serves. The server takes a
moment to stop after the adapter has stopped, and a delivery arriving in that
window has no project or runtime left to be attributed to; it is refused
rather than written against a session that has ended. A delivery that arrives
while the adapter *is* recording is recorded even if the response races a Stop,
which is right, because it did arrive.

## 4. Supported Claude events

Five hooks and four stream message kinds. Nothing else is read.

### Hooks

| Claude hook | What it means |
| --- | --- |
| `SessionStart` | a session began; carries `session_id` and a `source` of `startup`, `resume`, `clear` or `compact` |
| `UserPromptSubmit` | a prompt was submitted |
| `PermissionRequest` | Claude asked to use a tool; carries the tool's **name** |
| `Stop` | the model stopped producing |
| `SessionEnd` | the session ended; carries an enumeration `reason` |

Claude Code declares thirty-three hook events. The other twenty-eight are
answered with 200 and dropped, and are not recorded as anything. A settings file
that declares more than these five therefore loses nothing that was ever going to
be kept, and a later phase extends the map rather than changing it.

### Stream messages

The stream is `--output-format stream-json --verbose --include-hook-events`:

| Message | Why it is read |
| --- | --- |
| `system/init` | carries `session_id` before any hook has necessarily been processed - the second, independent path to correlating a session |
| `system/hook_started` | published to subscribers; not recorded |
| `system/hook_response` | published to subscribers; not recorded |
| `result` | **the only thing that reports how a turn ended**, in `is_error` |

### Why the stream is read at all, when the hooks already arrive

Because hooks report that a session started, that a prompt was submitted and that
the model stopped. None of them reports whether the work succeeded. The `result`
envelope does, and that one field is the difference between `agent.completed` and
`agent.failed`.

### Why `hook_started` and `hook_response` are not recorded

They are how the adapter follows a session, not facts about the work in it, and
the hook events they describe already arrived in full through the receiver.
Recording them again from the stream would put a duplicate of every hook event
into an append-only log. They are published to subscribers and not recorded,
which is the whole of the difference.

### One bad line does not end the stream

A line that is not JSON, or that is longer than the reader will hold, is skipped
and counted, and the read continues. That direction is chosen deliberately: the
stream is the only source of the turn's outcome, and ending it over one stray
line would lose the `result` envelope that follows - turning a cosmetic problem
on stdout into a session whose completion is never recorded. The counts are
reported in one log line when the stream ends, so a stream that was mostly noise
is visible rather than merely survived.

## 5. The mapping table

| Claude fact | internal `Kind` | AgentMux event type | Recorded | Payload |
| --- | --- | --- | --- | --- |
| `SessionStart` hook | `SessionStart` | `agent.started` | yes | `{"event":"SessionStart","source":"startup"}` |
| `UserPromptSubmit` hook | `UserPromptSubmit` | `agent.prompt_submitted` | yes | `{"event":"UserPromptSubmit"}` |
| `PermissionRequest` hook | `PermissionRequest` | `agent.permission_requested` | yes | `{"event":"PermissionRequest","tool":"Bash"}` |
| `Stop` hook | `Stop` | `agent.completed_candidate` | yes | `{"event":"Stop"}` |
| `SessionEnd` hook | `SessionEnd` | `agent.session_ended` | yes | `{"event":"SessionEnd","reason":"other"}` |
| `system/init` | `system/init` | - | no | `{"event":"system/init"}` |
| `system/hook_started` | `system/hook_started` | - | no | `{"event":"system/hook_started","hook":"Stop:1"}` |
| `system/hook_response` | `system/hook_response` | - | no | `{"event":"system/hook_response","hook":"Stop:1"}` |
| `result`, `is_error` false | `result` | `agent.completed` | yes | `{"event":"result","isError":false,"terminalReason":"completed"}` |
| `result`, `is_error` true | `result` | `agent.failed` | yes | `{"event":"result","isError":true,"terminalReason":"api_error"}` |

Every row is written with `source = "agent"`.

### `Stop` is a completion *candidate*

`Stop` means the model stopped producing. A turn that was aborted, a tool call
that was refused and a crash after the last token all look exactly like this from
there, so recording it as `agent.completed` would put a claim into an immutable
row that the row's own evidence does not support. It gets a type of its own
instead.

A reader who wants **"did this work"** reads `agent.completed` or `agent.failed`.
A reader who wants **"did it stop"** reads `agent.completed_candidate`. The two
are different questions and the log answers them separately.

### `is_error` decides the outcome, and `subtype` does not

The result envelope's `subtype` is not read anywhere in this package. It has been
measured reading `"success"` on a turn that did not succeed -
`docs/CLAUDE_RUNTIME_VALIDATION.md` §11.2 is the measurement - and a field that
has been caught lying is not a field to decide an immutable row with. `is_error`
and `terminal_reason` are, and they are the only two.

A `result` payload that cannot be decoded is treated as a failure. That branch is
unreachable in production, because the payload is built by this package; the
direction is chosen for what it would mean if it were reached, and recording an
unreadable result as a success is the one mistake the row could not be corrected
for.

### The turn's outcome is not a task status

Nothing in this phase reads an event to decide anything. A Task's state machine
is a later phase, and §10 records what that phase will have to settle.

## 6. Session correlation

A Claude `session_id` is what ties a hook delivery, the stream's init message and
the final result to one session. The adapter preserves it in three places and
nowhere else:

```text
Claude session_id ──▶ Event.SessionID ──▶ Adapter.bindings[SessionID]
                                                 │
                                                 ├─ ProjectID      (given at Start)
                                                 ├─ RuntimeID      (given at Start)
                                                 ├─ AgentSessionID (given at Start)
                                                 ├─ FirstSeen / LastSeen
                                                 └─ Events (a count)
```

The mapping is **in memory only**. `docs/CLAUDE_ADAPTER.md` §9 records what that
costs and what a later phase would add to keep it.

### Why it is not in the database

Two reasons that point the same way.

The first is the brief's: the first version of this adapter is not allowed to
change the database, and the mapping is restored by the next adapter that
observes the same session.

The second is a rule this repository already had.
`internal/event/payload.go` refuses any payload field whose normalised name ends
in `sessionid` - `sessionId` and `claudeSessionId` are both refused - and that
rule is not weakened here. Weakening a security rule to make a feature fit is how
a security rule stops being one.

So the Claude session id appears in the binding and in `Event.SessionID`, which
is what `Subscribe` hands to a caller, and it is **never written to a row**. The
correlation key stored per event is `runtime_id`. A test asserts both halves:
the id is in the binding, and it is in no recorded payload.

### The adapter is told, and never guesses

`Start` requires `ProjectID` and `RuntimeID` and refuses a configuration that
cannot give them. `AgentSessionID` is optional. The adapter does not derive any
of the three from the working directory, from a tmux session name, or from the
time - a guessed correlation is written into a history that cannot be corrected,
and the guess would be wrong in exactly the case that matters, which is two
runtimes for one project.

`Config.validate` checks that the identifiers are present and nothing more. It
does not check that they name anything: resolving a project id to a project would
make this layer depend on the project store, and what to do about an event for a
project that no longer exists is a decision that belongs to the caller.

## 7. Security rules

### What a payload may contain

An identifier or an enumeration. Never content.

**Forbidden, and not decoded in the first place:**

- the prompt, in whole or in part;
- the assistant's response, in whole or in part;
- `tool_input` - the arguments a tool was called with;
- the transcript path and the working directory;
- any credential, token, key, cookie or authorization header.

The strictest guarantee here is not a filter. The hook decoder
(`hookPayload`) and the stream decoder (`streamMessage`) have **no field** for
any of those, so an assistant message, a tool result and the verbatim output of
every hook are decoded into a struct that cannot hold them. A filter is a
decision made once per field and revisitable by mistake; an absent field is not.

Every string that *is* kept passes through `clip`, which bounds it at 128 bytes
and marks a truncation with `...` rather than silently shortening it. The values
are Claude's enumerations - `startup`, `clear`, `Bash` - and the bound exists
because a value that grew without limit would turn a recorded event into a
refused one.

A field whose value is empty is dropped rather than stored as an empty string.
"This event did not say" and "this event said nothing" are not the same fact, and
only the first is true.

### The settings document

It names nothing but the receiver. Every URL in it is the endpoint this adapter
opened, and no credential appears in it. A test asserts both.

### The endpoint

The path is a 128-bit nonce minted at `Start`, and it is the whole of the
receiver's authentication. What that is worth is stated plainly: it stops a
process that never saw the settings file from posting events, and it stops
nothing else. The receiver binds loopback only, refuses any method but `POST`,
refuses any path but its own, and refuses a body over 256 KiB. It does not
authenticate the caller, because Claude Code's hook delivery has no credential to
present - and a token this adapter minted would have to be written into the
settings file, which is a file the adapter deliberately does not own.

**Be precise about the trust boundary.** Anything that can read the settings file
or the process's loopback traffic can post an event. Such a process is already
running as the user, on the user's machine, inside the environment that holds the
user's Claude configuration - so the receiver is not the weakest link, and
hardening it without first isolating that environment would be hardening the
wrong thing. A hook is a notification, and this adapter treats it as one: it
records what it is told and decides nothing from it.

### Writing events

Every event goes `ClaudeAdapter → EventService → agent_events`. There is no path
from the adapter to SQLite: `events.go` declares a one-method interface that
`*event.Service` satisfies, and the adapter holds nothing else. A test in this
package uses a recorder that applies the same three checks the real service
applies, so a payload that names a credential or that grew past the bound fails
in the suite rather than at runtime in a warning nobody reads.

`Recorder` may be nil, which means nothing is recorded. That is a legitimate
configuration - an adapter used only for its subscriber channel - and not a
silently broken one.

## 8. Manual validation

The unit suite never runs Claude and needs no account. The end-to-end path was
validated by hand on 2026-09-20 on this host, against the native Windows Claude
Code 2.1.278, with a real `event.Service` over a real migrated SQLite database.

Two runs, and everything they produced was deleted afterwards:

| Run | Prompt | Observed |
| --- | --- | --- |
| 1 | a one-word answer | `agent.started`, `agent.prompt_submitted`, `agent.completed_candidate`, `agent.completed`, `agent.session_ended` |
| 2 | a prompt that uses the Bash tool | the same five; 14 stream messages decoded, 0 malformed, 0 oversized |

Both runs correlated one Claude session to the configured project, runtime and
agent session, and both recorded five rows in `agent_events`, all with
`source = "agent"`. The stream's `result` envelope reported `isError: false`,
`terminalReason: "completed"`.

Two things the validation settled that were open before it:

- **The exact-URL form of `allowedHttpHookUrls` is accepted.** Three HTTP hooks
  fired with a single exact entry, so no wildcard is needed.
- **The `SessionStart` command hook works on Windows.** The generated `curl`
  command, with POSIX single-quote quoting, was run by Claude Code on Windows and
  delivered. That is worth stating because it was the least certain part of the
  design.

What the validation did **not** cover, and what that means:

- `PermissionRequest` was not observed. In both runs Claude Code auto-approved
  the tool it wanted, so the hook never fired. The event is covered by unit tests
  - the mapping, the payload, and the receiver's handling of it - and it has not
  been seen end to end.
- `agent.failed` was not observed. Both turns succeeded. The failure mapping is
  covered by a unit test that replays the measured case from
  `docs/CLAUDE_RUNTIME_VALIDATION.md` §11.2, where `subtype` reads `"success"`
  and `is_error` is true.

## 9. Limitations

Stated rather than hidden. Each of these is a real cost, not a caveat.

1. **The session mapping is in memory.** It is lost when the process restarts,
   and it is not visible to any other component. A phase that needs the
   correlation to outlive the process, or to be joined against
   `agent_sessions`, has to store it - which means a migration, and a decision
   about what a Claude session id means to AgentMux that this phase deliberately
   did not make.

2. **`SessionStart` needs `curl`** on PATH inside the environment Claude runs in.
   That is a real dependency on a program this project does not ship, created by
   Claude Code's refusal of HTTP handlers for that one event. A future Claude
   Code that accepts an HTTP `SessionStart` would remove it.

3. **Nothing is recorded unless something delivers.** The adapter is passive: if
   the settings file is not passed to the CLI, or is passed to a CLI that was
   already running, no hook arrives and no event is written. There is no
   reconciliation and no way to notice a session that was never observed. A
   `system/init` message on a stream the adapter is reading is the only
   independent evidence a session exists.

4. **`Stop` and the result envelope can disagree, and both are recorded.** A
   `Stop` followed by a `result` with `is_error: true` produces
   `agent.completed_candidate` and then `agent.failed`. That is the intended
   reading - the model did stop, and the turn did fail - but a consumer that
   treats "a candidate appeared" as "the turn is over" will be wrong, and the
   two events have to be joined on the runtime rather than read one at a time.

5. **Ordering across the two paths is not guaranteed.** A hook arrives over HTTP
   and a stream message arrives on a pipe, and they are independent. In the
   measured runs the hook always arrived first, but nothing enforces it, and
   `created_at` is the write time rather than Claude's own ordering.

6. **The receiver's authentication is a path, not a credential.** §7 says what
   that is worth.

7. **No permission decision, and no way to make one.** `agent.permission_requested`
   is a record. Nothing reads it. This is the brief's boundary for this phase and
   also a design position: see §10.

## 10. Future work

Ordered by what has to be settled first, not by size.

**Phase 7.3B-2 and the permission question.** `PermissionRequest` is currently
record-only, and the phase that makes it actionable has to answer a question this
one avoided: whether AgentMux decides a permission and returns a decision to
Claude Code. The mechanism exists - Phase 7.3A proved a hook's stdout is read as
a decision - and turning it on makes AgentMux a participant in a Claude session
rather than an observer of one. Every property of this adapter that says "it
decides nothing" would have to be re-examined, and the trust boundary in §7 stops
being a boundary and becomes an authorization surface.

**Persisting the session mapping.** Requires a migration, and the decision in
§9.1.

**Task state derived from events.** A task's state machine would read
`agent.started` / `agent.completed` / `agent.failed`. The `Stop` / result split in
§5 is what makes that safe, and the phase that builds it should read
`agent.completed_candidate` as "a turn ended", never as "the work succeeded".

**More events.** The hook map is the extension point. `PreToolUse`,
`PostToolUse` and `Notification` are the obvious next three, and each needs the
same question answered: identifier or enumeration, never content.

**Wiring it into the server.** Nothing in this phase constructs an adapter. The
runtime manager is the component that knows a runtime's identity, and it is the
one that would own an adapter's lifetime - which is Phase 7.3B-2's decision, not
this phase's.

## 11. Where this sits

- `internal/claude/claude.go` - the CLI launcher, and the package's own account
  of the two halves.
- `docs/CLAUDE_INTEGRATION.md` - what Claude Code offers and how it was measured.
- `docs/CLAUDE_RUNTIME_VALIDATION.md` - the environment measurements, including
  the `SessionStart` refusal and the `subtype` / `is_error` divergence.
- `docs/AGENT_EVENTS.md` - the event foundation this adapter writes into.
- `docs/ARCHITECTURE.md` §3 - where the adapter sits among the other components.
