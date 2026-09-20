# AgentMux Task and Agent Session Model

This document describes the task layer: what a task is, what an agent session
is, how the two relate to a runtime, and what this phase deliberately does not
do.

It is written for someone about to build on top of it - an agent integration, a
notification, a dashboard - because every one of those wants the task model to
mean something slightly different, and this is where the meaning is fixed.

Phase 7.2 builds this layer and almost nothing else. Like Phase 7.1, **nothing
in it is visible in the product**: there is no task screen, no task list, and no
way from the browser to create one. What exists is the data model, the API that
the next phase will drive, and the rules that keep the two honest.

## 1. The five levels

```text
Project
   │
   └── Task              what somebody wants done
          │
          └── AgentSession    one attempt at doing it
                 │
                 └── Runtime        the process that attempt runs in
                        │
                        └── AgentEvent    what happened
```

Each level answers a question the level above it cannot:

| Level | Question it answers | Lifetime |
| --- | --- | --- |
| Project | where the work lives | as long as it is registered |
| Task | what is wanted | until it is completed, failed or cancelled |
| AgentSession | which attempt this was | one attempt; several per task |
| Runtime | what process is running | as long as the session exists |
| AgentEvent | what happened, and when | forever |

Read the table downward and it is a decomposition. Read it upward and it is a
history: the event is the smallest fact, and each level above it is a way of
grouping the facts below.

## 2. Task

**A task is a piece of work somebody wants an agent to do.**

```go
type Task struct {
    ID          string
    ProjectID   string
    Title       string
    Status      TaskStatus
    CreatedAt   time.Time
    UpdatedAt   time.Time
    CompletedAt *time.Time
}
```

The example the brief gives:

```text
Task: Implement Controller Viewer
```

### What a task is not

A task is not a terminal, not a runtime, not a WebSocket, and not a Claude
process. It is the thing those exist to serve, and conflating the two is the
mistake this layer exists to prevent. Three questions make the difference
concrete:

- *"Is the terminal up?"* is a runtime question. It is answered by
  `Runtime.State`, it is true or false right now, and it stops meaning anything
  once the process exits.
- *"Was this work finished?"* is a task question. It is answered by
  `Task.Status`, and it stays true after every process involved has exited,
  been destroyed, and been forgotten.
- *"Which attempt was that?"* is a session question, and it is the one that
  makes the other two compatible.

A task with no runtime is therefore not an incomplete task. It is a task nobody
has started yet, which is the state every task begins in.

### Title

Free text, written by whoever asked for the work. It is bounded at 200
characters and must be valid UTF-8 with no control characters.

It is **not** HTML, it is **not** a command, and it is never executed,
interpolated into a shell, or interpreted as a path. A task title is stored and
returned as an opaque string; escaping it for display is the display layer's
job, and doing it here would mean storing the escaped form and corrupting it on
the second read.

The title is **not** copied into the event log. See §7.

### Status

```text
CREATED  ──▶ RUNNING ──▶ WAITING ──▶ RUNNING ──▶ COMPLETED
                │           │
                ├───────────┴──▶ FAILED
                │
CREATED ────────┴───────────────▶ CANCELLED
```

| Status | Means |
| --- | --- |
| `CREATED` | the task exists and nobody has started it |
| `RUNNING` | work is under way |
| `WAITING` | work is under way and is blocked on something outside it |
| `COMPLETED` | the work was done |
| `FAILED` | the work was attempted and did not succeed |
| `CANCELLED` | nobody is going to do this |

`WAITING` belongs here and not to the session. It is a statement about the
*work* - this task is blocked on a review, on a decision, on another task - and
that is a fact about the goal rather than about any process pursuing it. A
session that is waiting is a session that is idle, which is a different thing,
and Phase 7.2 does not need a word for it.

### Lifecycle

The allowed transitions are exactly the arrows above:

| From | To |
| --- | --- |
| `CREATED` | `RUNNING`, `CANCELLED` |
| `RUNNING` | `WAITING`, `COMPLETED`, `FAILED`, `CANCELLED` |
| `WAITING` | `RUNNING`, `COMPLETED`, `FAILED`, `CANCELLED` |
| `COMPLETED` | *nothing* |
| `FAILED` | *nothing* |
| `CANCELLED` | *nothing* |

The last three rows are the point of the table. `COMPLETED`, `FAILED` and
`CANCELLED` are terminal, and `COMPLETED → RUNNING` is refused. This is the
transition the brief names as forbidden, and it is forbidden for a reason
rather than for tidiness: a task that has been reported as done and is then
reported as running again has made the first report false, and anything that
read it - a person, a report, a later phase - was told something that turned out
not to be true. Reopening finished work is a real workflow, and it is a
different operation from a status change: it is a new task, or a later phase
adds an explicit transition with its own name.

Two consequences of the table are worth stating outright:

- **A task cannot be completed without having run.** `CREATED → COMPLETED` is
  refused. "Completed" is a claim about work that was done, and work that never
  started was not done. A caller that wants to record work done elsewhere
  creates the task, starts it, and completes it - three calls that say what
  happened, rather than one that asserts an outcome it skipped.
- **Nothing is automatic.** No transition happens because time passed, because a
  runtime stopped, or because an event was written. Every one is a request. See
  §7 for why that is deliberate rather than unfinished.

### CompletedAt

Set when a task enters `COMPLETED`, and cleared otherwise - so a task that is
`FAILED` has no completion time, which is the honest answer. It is set by the
server, never by a client (§9).

## 3. AgentSession

**An agent session is one attempt at a task.**

```go
type AgentSession struct {
    ID         string
    TaskID     string
    RuntimeID  string
    Status     AgentSessionStatus
    StartedAt  *time.Time
    EndedAt    *time.Time
    CreatedAt  time.Time
}
```

The brief's example:

```text
Task: Fix websocket bug

  AgentSession #1 ──▶ Runtime #1      the first attempt, in its terminal
  AgentSession #2 ──▶ Runtime #2      the second attempt, in its own
```

One task, two sessions: the first attempt went sideways, and the second is a
different attempt at the same goal. Neither session replaces the other, and the
task is the thing that says what both were for.

### Why a session exists at all

Because a task and a process have different lifetimes, and a model with only two
levels has to pretend otherwise.

If the task held the runtime id directly, then restarting the terminal would
either overwrite the history of the first attempt or force a new task to be
created for what is plainly the same work. Both are wrong, and the second is
worse: it makes "how many times did we try this" a question about task titles.

The session is where "how many attempts" lives, and it is the level at which
each attempt's own facts - when it started, when it ended, whether it succeeded
- are recorded.

### Status

```text
CREATED ──▶ RUNNING ──▶ COMPLETED
   │           │
   └───────────┴──────▶ FAILED
   │
   └──────────────────▶ CANCELLED
```

| From | To |
| --- | --- |
| `CREATED` | `RUNNING`, `FAILED`, `CANCELLED` |
| `RUNNING` | `COMPLETED`, `FAILED`, `CANCELLED` |
| `COMPLETED` | *nothing* |
| `FAILED` | *nothing* |
| `CANCELLED` | *nothing* |

**There is no `WAITING`.** The brief says not to add one unless the architecture
requires it, and this architecture does not: it is not clear what a session
would be waiting *for*. A session whose runtime has stopped is not waiting, it
is over. A session whose runtime is up but which the agent is not using is
idle, and idle is not a lifecycle state - it is what `RUNNING` looks like
between two things happening.

The task has a `WAITING` because a task can be blocked on something outside
itself, which is a real and long-lived condition. If Phase 7.3B finds that a
session can be blocked in a way that matters, adding a status is a migration
and a row in this table, and that is the right time to make it.

`CREATED → FAILED` is allowed, and it is not an oversight. A session is created
before its runtime is, so a runtime that never came up produces a session that
failed without ever having started. That is the fact, and `StartedAt` being nil
is how it reads.

### RuntimeID

Empty until a runtime is attached.

It is **not** a foreign key. The runtime record in `project_runtime` is deleted
when a runtime is destroyed, and the session - "this attempt happened, and this
is the runtime it ran in" - has to outlive it. This is the same decision
migration `0003_agent_events.sql` makes about the same column for the same
reason, and Phase 7.2 keeps it rather than reversing it.

Empty is a first-class value, not a placeholder. See §4.

## 4. Why a session may have no runtime

Because creating a session and starting a process are two different acts, and
they happen at different times.

```text
t0   POST /api/tasks/{id}/sessions        AgentSession: CREATED, no runtime
t1   POST .../runtime/start               a runtime comes up
t2   PATCH /api/sessions/{id}             runtimeId attached, status RUNNING
```

The brief is explicit that these must not be forced into one database
transaction, and the reason is not convenience. A transaction spanning them
would have to hold a write lock open across a process launch, which takes
seconds and can fail in ways that leave the database describing a process that
does not exist. Worse, it would make the task layer a dependency of the runtime
layer: a tmux that refused to start would roll back the record that somebody
wanted this work done at all.

The same reasoning already governs the event log - a history write that could
fail a runtime start would make the event layer a dependency of the thing it is
recording - and a session is, in this respect, the same kind of thing.

So the two are separate calls, and the window between them is a real state that
the model names rather than hides: a session with no runtime is `CREATED`.

## 5. Relationships

| Relation | Cardinality | Enforced by |
| --- | --- | --- |
| Project → Task | 1 → N | `tasks.project_id`, foreign key, cascade |
| Task → AgentSession | 1 → N | `agent_sessions.task_id`, foreign key, cascade |
| AgentSession → Runtime | 0/1 → 1 | `agent_sessions.runtime_id`, a column and **not** a foreign key |

### Creation

- A task must name a project that exists. The service asks the project service,
  so a task cannot be created under a project id that was never registered.
- A session must name a task that exists, and it starts with no runtime.
- A runtime is attached to a session by a request, after it exists.

There is **no** endpoint that creates a task and starts an agent - see §8.

### Deletion

Nothing in this build deletes a task or a session, and there is no endpoint that
could. The cardinalities above are still declared, so that the day something
does delete one, the database decides the question instead of leaving orphan
rows behind:

- Deleting a project deletes its tasks, and deleting a task deletes its
  sessions. A task without its project is unreachable, and a session without its
  task is not an attempt at anything.
- **Neither cascade touches `agent_events`.** The event log is a separate,
  append-only history, it is keyed by project and runtime rather than by task,
  and Phase 7.1's guarantee - that a history cannot be edited or deleted - is
  not weakened by this phase. See §6.

## 6. How events relate

Task and session events are written through the same `event.Service` the runtime
bridge uses, with the same validation, the same identifier scheme and the same
append-only table. Nothing in this phase writes a row to `agent_events` by any
other route.

**`agent_events` does not gain a `task_id` or a `session_id` column.** This is a
decision, not an omission, and the reasons are:

- **The table is append-only, so existing rows could never be backfilled.** A
  column added now would be permanently null for every event already written,
  and a nullable column that is authoritative for new rows and meaningless for
  old ones is worse than no column: a reader cannot tell "this event has no
  task" from "this event predates the column".
- **The association is already derivable, in the direction that matters.** A
  session names its runtime; a runtime names its project; an event names its
  runtime. "Which events happened during this attempt" is therefore answerable
  today, by reading the session's runtime and then that runtime's timeline -
  which is one of the two endpoints Phase 7.1 built.
- **The brief says not to add fields because they might be useful later.** This
  is that field. When a phase needs to query events *by task* - and no phase
  does yet, because nothing reads events to decide anything - the association
  will need a column, and that phase will have a consumer to design it against.

What a task event carries instead is the task's identifier, in the payload:

```json
{"taskId":"task_4f3a…"}
```

That is a fact of the same size as the ones Phase 7.1 already stores, and it is
the identifier alone. **A task title is never written to an event**, for the
reason `docs/AGENT_EVENTS.md` §7 gives about error strings: a row that can never
be edited can never be redacted, and the title is the one field in this model
that a person wrote.

### What this phase emits

| Type | When | Payload |
| --- | --- | --- |
| `task.created` | a task is created | `{"taskId":"…"}` |
| `task.status_changed` | a task changes status | `{"taskId":"…","from":"…","to":"…"}` |
| `session.created` | a session is created | `{"taskId":"…","sessionId":"…"}` |
| `session.status_changed` | a session changes status | `{"taskId":"…","sessionId":"…","from":"…","to":"…"}` |

Four types, not one per status.

**This departs from the letter of the brief**, which lists `task.completed`,
`task.failed`, `session.started` and so on as examples. It keeps what those
examples are for - every one of them is recoverable from the two rows above -
while refusing the part the brief itself warns about, that event count should
not be inflated. A type per status would mean six task types and five session
types for two facts, and adding a status in a later phase would then require
adding an event type as well, in a package that is not supposed to know the
status vocabulary. `from` and `to` say the same thing in one field, and a
consumer that wants "every task that failed" reads one payload key.

### Source

Every task and session event in this phase is written with source `user`,
because every one of them is produced by a request. The vocabulary is not
widened.

`task` and `session` were considered as new sources, and rejected: `type`
already names the subsystem in its first segment (`task.created`), so a source
of `task` would say the same thing twice while making the field mean "which
subsystem" for these events and "who" for every event Phase 7.1 writes. The
existing definition - `user` is *a person asking for something through the API*
- is exactly right for a request that creates a task.

A later phase that completes a task on its own, without anybody asking, writes
`system`. That is what the source field is for.

## 7. Events are history; status is current state

This is the rule the whole phase is arranged around, and it is the same one
Phase 7.1 established:

```text
task.created          ┐
task.status_changed   ├── history    what happened, in order, forever
task.status_changed   ┘

Task.Status = COMPLETED              current state    what is true now
```

**Nothing derives a task's status from its events.** The service writes the
status column and the event in the same operation, and the column is the
authority. There is no code anywhere in this phase that reads `agent_events` to
work out what a task's status is, and a test asserts that the task endpoints
work with no event log attached at all.

The temptation to derive is real and it should be refused for a specific reason:
an event log is a record of what was *reported*, and a state derived from it is
only as correct as the reports. If a status change were ever lost - a crash
between the column write and the event write, an event pruned by a retention
policy that does not exist yet but will - a derived state would silently be
wrong, while an explicit column is simply stale in a way a reader can see.

Phase 7.3B may revisit this, when there is an agent deciding things and a reason
to reconcile. Until then the rule is: events record, the service decides.

## 8. What this phase does not do

Every item here is deferred deliberately. None is stubbed, and none is
half-present.

- **No Claude Hooks, no Claude Agent SDK, no agent intelligence.** Nothing in
  this layer knows what an agent is doing. A session has a status because
  somebody set it, and for no other reason.
- **No terminal output parsing, prompt parsing, or AI state inference.** The
  runtime bridge from Phase 7.1 is untouched by this phase.
- **No automatic anything.** Creating a task does not start a runtime. Starting
  a runtime does not attach it to a session. Completing a session does not
  complete its task. Every one of those links is a request a caller makes.
- **No retry.** `FAILED` is terminal on both aggregates. Retrying is a new
  session against the same task, which a caller can do by hand today.
- **No notifications, no token or model statistics, no summarization, no
  multi-agent orchestration, no user accounts, no billing.** All out of scope.
- **No task UI.** The frontend gains types and API functions and no screens.
- **No delete.** A session records that an attempt happened; an attempt that has
  been forgotten is not history. Deleting a task would have to decide what
  happens to its sessions and its events, and no product decision has been made
  about that.

## 9. Time

Every timestamp is produced by the server, in UTC, and stored as RFC3339 with
nanoseconds - the format every other timestamp in this database uses.

**A client never supplies a time.** `createdAt`, `updatedAt`, `startedAt`,
`endedAt` and `completedAt` are all set by the service from its own clock. The
API has no field for one, and a request that carries a timestamp field is
refused rather than ignored, because unknown fields are rejected throughout this
API.

The practical consequence: a device with a wrong clock cannot write a task that
was completed before it was created. Converting to a local time zone is the
display layer's job.

## 10. Concurrency

Two requests can change the same task's status at the same moment. The model
answers that with a **conditional update** rather than a lock:

```sql
UPDATE tasks SET status = ?, updated_at = ?, completed_at = ?
WHERE id = ? AND status = ?        -- the status the service read
```

If the row no longer carries the status the service validated the transition
against, zero rows change, and the service reports a conflict rather than
retrying. The losing caller is told the task moved; it is not told its request
succeeded, and it is not silently given a state it did not ask for.

The alternative - reading, deciding, and writing unconditionally - has a window
between the read and the write in which the other request lands, and the result
is a transition that was never legal, written by a service that had checked. The
window is small and it is not zero, and a test drives it on purpose.

There is no lock table, no version column on the API, and no optimistic-
concurrency token for a client to carry. One conditional statement is enough for
a single-process server, and it costs nothing to keep.

## 11. Identifiers

```text
task_<24 hex chars>      12 random bytes
sess_<24 hex chars>      12 random bytes
```

Generated by the server from `crypto/rand`, never by the database, for the
reason every identifier in this codebase is generated: identity the storage
engine hands out is identity only the storage engine knows.

The prefix and the entropy differ from a project id (`p_`, 10 bytes) and an
event id (`evt_`, 16 bytes) because they name different things and are read in
different places - a task id is in a URL, an event id is a cursor. The
*generation*, though, is one implementation: `internal/idgen`, which the project
model and the event model now also use. Before this phase there were two copies
of the same twenty lines; there are not three.

## 12. Listing

A project's tasks and a task's sessions are listed, ordered by `(created_at, id)`
descending, and **capped**:

```text
limit absent    →  100
limit > 500     →  500
```

There is no cursor on either listing, and `count` is the number of rows in the
page rather than the number that exist. This is a real limitation and it is
recorded as one rather than dressed up as a design.

It is deliberate for the reason the phase brief gives: a cursor is a commitment
to an ordering that survives concurrent writes, and nothing in this phase reads
these lists at a scale where the commitment buys anything. The event timeline
needed one because an event log grows without bound and a reader pages through
it as a history. A task list is read as a worklist - "what is outstanding for
this project" - and a project with more than 500 tasks in it is not a case this
phase has a consumer for.

The ordering key is the pair and not the timestamp alone. Two tasks created in
the same instant - which a loop of requests does routinely - would otherwise have
no defined order, and a page boundary landing between them would skip one or
repeat one. The event log takes the same pair as its ordering key, for the same
reason, and `docs/AGENT_EVENTS.md` §6 is where that is worked out.

The limits are declared once in `internal/task/repository.go` as `DefaultLimit`
and `MaxLimit`, and the service clamps to them. A `limit` above the maximum is
clamped rather than refused, because a caller asking for a thousand wants as much
as it can have; a `limit` below 1 is refused, because zero is not "no limit" and
a caller that asked for nothing has made a mistake.

`docs/AGENT_EVENTS.md` §6 has the keyset pagination this deliberately does not
have, for the reader who wants to know what the difference costs.
