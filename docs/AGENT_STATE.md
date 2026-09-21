# Agent State

An event is a fact about the past. A state is a reading of those facts that
changes constantly. This document is about the second, and about how the first
is folded into it.

Phase 7.3B-2 connected a running Claude session to the event log: start an agent
and `agent.started`, `agent.prompt_submitted`, `agent.permission_requested`,
`agent.completed_candidate` and `agent.session_ended` land in `agent_events`.
Phase 7.3C-1 is what turns that history into an answer to "what is it doing now".

## 1. Event versus state

```text
agent.permission_requested     something asked for a permission, at 10:04:11
WAITING_PERMISSION             it is waiting for one, right now
```

The first is a row that will never change. The second is a value that will be
overwritten by the next thing that happens. Neither is derived from the other at
read time, and neither replaces the other:

| | Event | State |
| --- | --- | --- |
| Question | what happened | what is true now |
| Cost of a read | the history, paged | one row |
| Lifetime | forever | until the next event |
| Written by | `event.Service.CreateEvent` | the projection, from that event |
| Rewritable | no | constantly |
| Recoverable | no — it is the record | yes — recomputed from the log |

**The state is derived and the events are not.** That asymmetry is the whole
design. It is why the state table can be added to an installation that already
has a history, why it can be deleted and recomputed at any moment, and why
nothing in the build is allowed to write one directly.

### What a state is not

There are three status vocabularies in AgentMux and they are deliberately
separate. A phase that merged any two of them would lose the ability to tell
them apart.

| Layer | Says | Example |
| --- | --- | --- |
| `internal/session` | whether a terminal exists and what is running in it | `Runtime.State = RUNNING` |
| `internal/task` | what somebody wants done, and whether they have said it is finished | `Task.Status = COMPLETED` |
| `internal/agentstate` | what the agent reported about itself | `WAITING_PERMISSION` |

An agent waiting for a permission is not a stopped runtime: the terminal is
still there and still running. An agent that completed a turn is not a completed
task: the work is not finished because one turn went well. And nothing in this
phase moves a task at all — `PATCH /api/tasks/{id}` remains the only route that
does, and it is a person who calls it.

## 2. The projection

```text
ClaudeAdapter
     │  (an observation)
     ▼
event.Service.CreateEvent
     │  (the fact is stored)
     ▼
agentstate.Service.Project      ← the only path
     │
     ▼
agent_states                    ← one row per attempt, overwritten
     │
     ▼
GET /api/sessions/{id}/state
```

The projection is called by the event service, after the event is stored, and by
nothing else. There is no path from an observer of Claude to a state row: the
adapter does not know this package exists, and the coordinator that binds an
attempt does not write states either. §10 of the phase brief is the rule, and the
reason for it is not tidiness — a projection that could be written to directly
would be a projection whose contents the log could not reproduce, and §4 below
would stop being true.

**A projection that fails does not fail the event.** The fact is already
recorded, and a reading that could not be computed from it does not un-happen
it. The error is logged, the event stands, and the next rebuild picks it up.

### Which events are read

| Event | Effect on the state |
| --- | --- |
| `session.created` | creates the row, status `CREATED`, no runtime |
| `session.status_changed` | binds the runtime to the attempt, and maps `to` onto a status |
| `agent.started` | `RUNNING` |
| `agent.prompt_submitted` | `RUNNING` |
| `agent.permission_requested` | `WAITING_PERMISSION` |
| `agent.completed` | `COMPLETED` |
| `agent.failed` | `FAILED` |
| `agent.session_ended` | `STOPPED` |
| `agent.completed_candidate` | **no status change** — records the event only |
| anything else | ignored |

### Where the attempt comes from

An `agent.*` event carries a project and a runtime and **no attempt id**, and §一
of the brief forbids changing the adapter to add one. The attempt is resolved
from the log itself: `session.status_changed` is the only event in the build that
carries both an attempt and a runtime, so it is what binds a runtime to the
attempt running in it. An `agent.*` event on that runtime then projects onto that
attempt.

The binding is set once and never cleared, and the store enforces it: an event
that carries no runtime — `session.created` does not — leaves whatever a
previous event established. A second attempt on the same runtime rebinds it,
because its own binding event arrives after the first attempt's last one.

A `session.status_changed` whose `to` this build does not recognise still binds
the runtime. That is unreachable from the task service, which validates a
transition before it emits one, and it is handled rather than skipped because
skipping would drop the binding along with the status: the runtime would stop
finding its attempt and every agent event after it would go unprojected. Such an
event keeps the status the row already has, which is what an event that asserts
no status does.

### `completed_candidate` moves nothing

| | |
| --- | --- |
| the model stopped producing | that is what the `Stop` hook reports |
| the turn succeeded | that is what the `result` envelope reports |

An aborted turn, a refused tool call and a crash after the last token all look
the same from `Stop`. So the event updates `last_event` and `last_event_at` and
leaves `status` where it was: the row then says *the last thing seen was Stop,
and the state is still RUNNING*, which is two true facts rather than one
invented one. Phase 7.3B-1 made the same distinction in the event vocabulary by
naming the type `agent.completed_candidate` rather than `agent.completed`.

## 3. The state machine

```text
                   ┌──────────────────────────────────────────┐
                   │                                          │
  CREATED ──▶ RUNNING ──▶ WAITING_PERMISSION ──▶ RUNNING ─────┤
                 │                                             │
                 ├──▶ COMPLETED                                │
                 ├──▶ FAILED                                   │
                 └──▶ STOPPED ◀────────────────────────────────┘

  WAITING_INPUT ── nothing produces this yet
```

**It is not enforced.** The projection applies whatever an event asserts, in the
order the events happened, and it refuses nothing. An event that arrived is an
event that happened; a projection that started rejecting transitions would be
inventing rules the log does not have, and a state that disagreed with the log
would be worse than a state that looked odd. The diagram is a description of what
the current event vocabulary can produce, not a gate.

`TERMINAL` — `COMPLETED`, `FAILED`, `STOPPED` — is a description too. Nothing
consults it to decide anything.

### The statuses

| Status | Means |
| --- | --- |
| `CREATED` | the attempt exists and nothing has started it |
| `RUNNING` | the agent is working |
| `WAITING_INPUT` | waiting for a person to say something |
| `WAITING_PERMISSION` | asked to use a tool and is blocked on the answer |
| `COMPLETED` | the turn finished and reported success |
| `FAILED` | the turn finished and reported failure |
| `STOPPED` | the session ended and nothing is running |

**`WAITING_INPUT` is unreachable.** No event this build records produces it:
Claude Code's idle notification is not among the hooks the adapter reads. It is
in the vocabulary because it is a real thing for an agent to be doing and the API
should not have to grow a new word for it later, and it is listed here as
unreachable rather than left for someone to discover.

**`COMPLETED` and `FAILED` are unreachable on the live path too**, for the reason
`docs/AGENT_RUNTIME_BINDING.md` §5 gives: they come from the `result` envelope,
which is on the stream-json half of the adapter, and the runtime hosts Claude as
a TUI in a tmux pane with nothing feeding that stream. `STOPPED` and the three
waiting/running states are what a real session produces.

## 4. Rebuild

`agentstate.Service.Rebuild` throws the table away and recomputes it from
`agent_events`.

It is the property the design is built around and the test of it: a projection
whose rebuild produced something other than its live writes would be a projection
that had started depending on something other than the log.

**It reads before it clears.** The whole log is read first and only then is the
table emptied, so a log that cannot be read leaves the projection exactly as it
was. Clearing first would mean a failed rebuild left a server reporting no agent
states at all, when the table had been correct a moment earlier. The fold itself
cannot fail it: an event that will not project is logged and skipped.

**The server calls it when the projection is empty.** That is the state a first
run after this phase leaves behind — the table is new and nothing has projected
into it — and it is the only condition under which a rebuild happens by itself.
A projection that already holds something is left alone: it is kept current as
events are written, so a non-empty table is a current one, and rebuilding on
every start would pay the whole history every start for nothing.

There is no endpoint that triggers a rebuild. An endpoint would be a write, and
§15 forbids one; a forced rebuild is an operator action for a later phase.

## 5. Storage

One table, `agent_states`, added by `migrations/0006_agent_states.sql`.

```sql
agent_session_id  TEXT NOT NULL PRIMARY KEY   -- the attempt
project_id        TEXT NOT NULL
runtime_id        TEXT                        -- NULL until bound
status            TEXT NOT NULL
last_event        TEXT NOT NULL
last_event_at     TEXT NOT NULL               -- the event's own time
updated_at        TEXT NOT NULL               -- this server's clock
```

Indexed on `project_id`, `runtime_id` and `status`.

**Why a table rather than a computation.** Reading a state by replaying a
project's whole event log would make every read cost the whole history, and the
cost would grow with the age of the installation rather than with what is being
asked. The projection pays it once, at write time, where it is one row per event.

**Why not a column on `agent_sessions`.** The attempt is the task model's record
of an attempt; this is what the agent reported about it. Different sources,
different lifetimes, different owners — and a schema that merged them would make
the task model depend on the event vocabulary.

**`agent_session_id` is not a foreign key**, deliberately. An event names a
runtime and a project and never an attempt, and the attempt is resolved from the
log. A foreign key would make a rebuild fail on an event whose attempt the task
model has since forgotten, and would make the task tables a precondition of
rebuilding — which is the one thing a rebuild must not require.

### Idempotency and concurrency

Everything that makes the projection correct under replay and under concurrent
events lives in one conditional upsert:

```sql
ON CONFLICT(agent_session_id) DO UPDATE SET ...
WHERE agent_states.last_event_at <= excluded.last_event_at
```

**The condition compares the event's own time, not the clock of whoever is
writing.** That is what makes replay idempotent — re-projecting an event cannot
move the state back to where the log had already moved it on — and what makes two
events arriving at once land on the same answer whichever order their writes
reach the database in. Without it, a rebuild running alongside live traffic would
produce a state that depended on timing.

Nothing in the service locks around a projection. Two events are two conditional
writes, and the database is the only coordination this needs. §13 of the phase
brief asked for exactly that, in preference to a lock that would achieve less.

## 6. What it does not cover

**An agent started without a task is not projected.** The attempt is what a state
is about, and an agent started through `POST …/runtime/agent/start` with no
`taskId` has none — there is no attempt for its events to be the state of, and no
`session.*` event to bind a runtime to one. Its events are recorded, its
`agent_events` rows are complete, and no state row appears. This is the UI path:
the workspace's Start button sends no body.

The fix is on the other side of a boundary this phase is forbidden to cross. The
adapter knows the attempt — it is in its `Config` — and does not put it in the
payload; adding it there would make an agent event self-identifying and would let
a stateless runtime be projected too. That is an adapter change, and §一 rules it
out. Recorded here, and in the phase report, rather than worked around with a
guess.

**A state does not survive a change in what the log means.** If a later phase
maps an event to a different status, the stored rows are what the old mapping
produced until something rebuilds them. Nothing rebuilds automatically once the
table is non-empty.

**Two attempts in one runtime are separated by their binding events, not by their
lifecycles.** The runtime is rebound by the second attempt's
`session.status_changed`, which arrives after the first attempt's last event, so
the newest row wins. An `agent.*` event that arrived for the first attempt after
that would be projected onto the second. The window is bounded by how long a
process takes to die and is not reachable through the API.

**`status` and `TERMINAL` are descriptions.** Nothing in this build reads a state
to decide anything — not the runtime, not the task model, not the coordinator.

## 7. Security

A state is an identifier and an enumeration. It has no field for a prompt, a
transcript, tool input, a token count or a model name, and its absence is the
design: none of those is a state, and a row that held one would be a row that
could never be redacted.

`last_event` is an event **type** and never a payload. The payload is where a
prompt would be.

The Claude session id does not appear in a state row at all — it is not in
`agent_events` either, and `docs/AGENT_RUNTIME_BINDING.md` §7 is why. What a
state holds is AgentMux's own identifiers: the attempt, the project, the runtime.

## 8. Reading it

```text
GET /api/sessions/{id}/state          one attempt
GET /api/projects/{id}/agent-states   a project's, most recently updated first
```

A state for an attempt nothing has observed is a `404` with code
`agent_state_not_found`. That is the ordinary answer for an attempt that was
created and never started, and for an agent started without a task.

There is no write. §15 forbids one, and the reason is not symmetry: a client that
could set a status would make the state a second place the truth lives, and the
two would disagree the moment the next event arrived.

A state says what an agent is doing. It does not say whether anybody needs to
care about it, and the endpoints that answer that are a layer above this one:
`GET /api/sessions/{id}/attention`, `GET /api/projects/{id}/attention` and
`GET /api/projects/{id}/actions`. `docs/AGENT_ATTENTION.md` is that layer, and
§2 of it is the difference between the two questions.
