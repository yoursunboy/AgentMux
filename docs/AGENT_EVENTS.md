# AgentMux Agent Events

This document describes the event foundation: what an event is, what is stored,
who may write one, how it is read back, and what this layer deliberately does
not do.

It is written for someone about to build on top of it - a task model, a
notification, a hook into the coding agent - because every one of those is a
consumer of this layer and every one of them will be tempted to widen it.

Phase 7.1 builds this layer and nothing else. Nothing in it is visible in the
product yet, and that is the point: it is the database and the message bus that
the phases after it stand on.

## 1. What an event is

**An event is a record that something happened.** It is not a status, not a
current value, and not a summary of what things are like now.

The distinction is the whole design, and it is easiest to see as a pair of
wrong and right answers to the same question:

```text
wrong:  runtime_status = waiting          a column, overwritten by the next value

right:  runtime.waiting                   a row, written once, never changed
```

The wrong version is what a state table already is. AgentMux already knows a
project's status; `Runtime.State` answers it, and `docs/RUNTIME.md` describes it.
An event does not duplicate that answer - it records that the answer *changed*,
and when.

Three consequences follow, and they are the reasons the rest of this document
is shaped the way it is:

- **An event is immutable.** It is written once. Nothing in this phase updates or
  deletes one, and there is no API that could. A history that can be edited is
  not a history.
- **An event is about the past, and stays true.** "The runtime started at
  14:02" is true forever, including after the runtime has stopped, been
  destroyed, and been started again. A status column would have been overwritten
  twice by then.
- **An event is not a state machine.** It does not decide anything and nothing
  reads it to decide anything. If a later phase wants to know whether a runtime
  is running, it asks the runtime. The events are what happened; the runtime is
  what is.

## 2. The model

```go
type AgentEvent struct {
    ID        string          `json:"id"`
    ProjectID string          `json:"projectId"`
    RuntimeID string          `json:"runtimeId,omitempty"`
    Type      string          `json:"type"`
    Source    string          `json:"source"`
    Payload   json.RawMessage `json:"payload,omitempty"`
    CreatedAt time.Time       `json:"createdAt"`
}
```

### ID

The event's identity, `evt_` followed by 128 random bits in hex.

It is generated rather than assigned by the database, and the reason is the one
this codebase gives everywhere else: identity that the storage engine hands out
is identity that only the storage engine knows. Two AgentMux servers writing to
two databases, or one server writing a row that will later be merged or
forwarded, cannot agree on an integer. A random identifier is unique before it
is ever stored, which means an event can be logged, queued, or handed to a
client before it has been written.

It is not a UUID, and that is a deliberate departure from the letter of the
Phase 7.1 brief: every other identifier in AgentMux is `prefix_hex` (`p_` for a
project, `inst_` for an installation), and a second identifier format in the
same product is a second thing for a reader to recognise. 128 bits of
`crypto/rand` is the same entropy a version-4 UUID carries, and it is the
entropy that matters. If a future phase needs RFC 4122 on the wire, the
identifier is opaque and can be re-encoded without changing the schema.

### ProjectID

The project this happened to. Required, always.

### RuntimeID

The runtime this happened to, or empty when the event is about the project
rather than about a runtime.

**In this build the runtime id is the runtime's session name** - `amx-<project
id>`, the same string `Runtime.Session` carries and the same one the runtime
manager logs, reconciles and destroys by. It is stored per event rather than
derived from the project id at read time, and the reason is the one migration
`0002_project_runtime.sql` already gives for storing the session name: the name
is derivable today, and storing it means a change to the naming rule cannot make
an existing event unreachable or misleading.

It is **not** a foreign key. The runtime record in `project_runtime` is deleted
when a runtime is destroyed, and the events about that runtime have to outlive
it - "this runtime was destroyed" is not a fact that stops being true when the
record goes. A cascade there would delete the history of the thing it was
recording.

A future backend that hosts more than one runtime per project would put its own
instance name here. No schema change is needed for that, which is the other
reason the column exists now rather than being added later.

### Type

What happened, in a dotted namespace: `runtime.started`, `runtime.stopped`,
`runtime.error`, `runtime.destroyed`.

Types are validated by **shape** - lowercase, dotted, at least two segments,
bounded in length - and not by an allowlist. An allowlist here would mean that
adding a type in Phase 7.3 required editing this package, and the vocabulary of
"what can happen" belongs to the layers that find out. The shape check exists to
catch a caller that passed a sentence or a status value where a type belongs,
which is the mistake that would actually be made.

### Source

Who or what produced the event. One of a closed set, because the set is closed
by the domain and not by convenience:

| Source | What it means |
| --- | --- |
| `runtime` | the runtime manager observed something about a session |
| `user` | a person asked for something through the API |
| `system` | AgentMux itself, not acting on anyone's behalf - a reconciliation, a shutdown |
| `agent` | the coding agent inside a runtime |

This is validated against the list. A typo in a source would produce an event
that no filter ever matches, which is worse than a rejection at the write.

### Payload

Structured detail, as JSON. Optional, and bounded at 4 KiB.

Deliberately small and deliberately not a document. Everything a later phase
wants to know about an event that it cannot learn from the four columns above
goes here, and nothing else does - see §7 for what is refused.

### CreatedAt

When the event happened, in UTC. Stored as RFC3339 with nanoseconds, like every
other timestamp in the metadata database, so the file stays readable with any
SQLite tool. The conversion to a local time zone is the display layer's job and
is not done here.

## 3. Storage

One table, `agent_events`, from migration `0003_agent_events.sql`:

```sql
id, project_id, runtime_id, type, source, payload, created_at
```

with indexes on `project_id`, `runtime_id`, `created_at` and `type`.

The three that matter are for the queries that will exist the moment something
reads this table:

- **`project_id`** - the project timeline, `GET /api/projects/{id}/events`, which
  is the query every consumer starts from;
- **`runtime_id`** - the runtime timeline, one project's runtime across the
  restarts and destructions it has been through;
- **`created_at`** - ordering, which every read does and which no index can be
  left out of.

`type` is indexed because "every time a runtime failed" is a question somebody
will ask of a table they cannot easily scan by hand.

The table has a foreign key to `projects` with `ON DELETE CASCADE`: an event
about a project that no longer exists is unreachable through every endpoint this
API has, so keeping the rows would be unbounded garbage rather than history.
There is no delete endpoint in this build, so nothing exercises it today; the
constraint is there so that the day one is added, it decides the question
instead of leaving rows behind.

**No statement about events lives outside `internal/storage`.** The HTTP layer
never sees a `*sql.DB`, exactly as it does not for projects and runtimes.

## 4. Who writes an event

Everything goes through one method:

```go
CreateEvent(ctx, projectID, runtimeID, eventType, source, payload)
```

on `event.Service`. There is no second way in, and that is the whole design of
this layer: the runtime bridge, a future Claude hook, and a user action in a
later phase all call this, so the validation, the identifier, the timestamp and
the size bound are applied in one place and cannot be skipped by a caller that
found a shorter route.

**A failed write never fails the operation that produced the event.** The
runtime bridge calls this after the runtime has already changed state, and a
history write that could fail a start would make the event layer a dependency of
the thing it is recording. Failures are logged and returned to the caller - so a
handler that wants to report one can - and the runtime bridge drops them with a
warning.

## 5. The runtime bridge

`internal/session` emits four events. It is the only emitter in this phase.

| Event | When | Payload |
| --- | --- | --- |
| `runtime.started` | a project's runtime reached `RUNNING` | `{"state":"running","cols":N,"rows":N}` |
| `runtime.stopped` | a runtime was stopped, keeping its session | `{"state":"stopped"}` |
| `runtime.error` | a start, a stop or a destroy failed | `{"operation":"start\|stop\|destroy","state":"error"}` |
| `runtime.destroyed` | a runtime and its session were removed | `{"state":"stopped"}` |

The payloads are what the bridge produces; nothing else reads or writes them.
`runtime.error` is the one with a varying shape: the state is recorded only when
the runtime was actually moved into `ERROR`. A destroy that failed is the case
where it was not - a destroy that fails on the host leaves the runtime exactly as
it was, so the event records which operation failed and claims nothing about
state that the failure did not change.

**The state machine is not changed by any of this.** Each event is emitted after
the state has already been written, at the point the fact is established. The
bridge observes; it does not participate. If the event layer were deleted
tomorrow, every one of those methods would behave exactly as it does now.

Four cases deliberately produce nothing:

- **A start on a runtime that is already running.** `Start` is idempotent and
  returns what is there. Nothing happened, so nothing is recorded. Writing
  `runtime.started` for it would put a false entry in an immutable history, and
  that is the one kind of entry a history cannot be repaired of.
- **A stop on a runtime that is already stopped.** Same reason, same result.
- **A destroy of a project that has no runtime.** `Destroy` is idempotent too,
  and "gone" is the end state it was asked for. A `runtime.destroyed` for a
  runtime that was never started would be a row with no beginning, and every
  reader of that timeline would have to work out for themselves that it meant
  nothing. A destroy of a runtime that *does* exist records one, and the two
  calls leave the host in the same state - so the event is about what happened,
  not about what the state ended up being.
- **A request naming a project that does not exist.** The project is resolved
  before a runtime is looked up, so there is no runtime for the event to be
  about. This is an API error, and the API reports it as one.

A destroy *does* record its history when there was a runtime, and the events
outlive the runtime record: `project_runtime` is emptied by a destroy, and
`agent_events` still says the runtime existed and what happened to it. That is
the difference between the two tables in one call.

**Every call writes at most one event.** A start that succeeds writes
`runtime.started` and a start that fails writes `runtime.error`, never both, and
the failures that move a runtime into `ERROR` are recorded through one function
(`failRuntime`) so that the state and the event cannot drift apart.

There is one asymmetry worth naming, because it looks like a missing event and is
not. **A start whose terminal came up and whose record could not then be written
writes `runtime.started` and no `runtime.error`.** Both facts would be true at
once - the terminal is running, the record of it is not - and an error event
there would contradict the start written in the same instant and send a reader
looking for a failed runtime that is in fact running. That failure is logged, is
returned to the caller, and leaves the runtime `RUNNING`, which is what it is.
The timeline says the runtime started, and it did.

### Where the state stays

```text
RuntimeManager   owns  what is true now      Runtime.State, in memory and in
                                             project_runtime

Event Service    owns  what happened         agent_events, append-only
```

Neither replaces the other. `Runtime` is asked whether a session is alive,
because only the runtime can answer that. Events are read to find out what has
happened to it, because only a history can answer that. A phase that starts
reading events to decide whether something is running has made the mistake this
section exists to prevent - the events would be minutes stale and the answer
would be wrong in exactly the case that matters, which is the runtime that died
without saying so.

## 6. Reading events

Two endpoints.

```text
GET /api/projects/{id}/events        one project's timeline
GET /api/runtime/{id}/events         one runtime's timeline
```

Both take the same two query parameters and return the same envelope:

```json
{
  "events": [
    {
      "id": "evt_1f0c…",
      "projectId": "p_9a3b…",
      "runtimeId": "amx-p_9a3b…",
      "type": "runtime.started",
      "source": "runtime",
      "payload": { "state": "running", "cols": 120, "rows": 30 },
      "createdAt": "2026-09-20T11:04:07.318204Z"
    }
  ],
  "nextBefore": "evt_1f0c…"
}
```

`nextBefore` is present only when more events remain, and it is passed back as
`before` to fetch the next page.

### Pagination

```text
limit    how many to return.  Default 50, maximum 200.
before   an event id.  Return the events older than that one.
```

`limit` is refused as a 400 when it is not a whole number or is below 1 - zero is
neither "the default" nor "no limit", and answering a client that asked for no
events with fifty would hide its mistake. Above the maximum it is **clamped**
rather than refused, because a client asking for a thousand is asking for as much
as it can have, and `nextBefore` tells it whether more remains.

**`before` is an event id and not a timestamp**, and this is the one piece of
this API that is worth stating twice. Timestamps on this host have
millisecond-or-coarser resolution, and a runtime that starts emits two events in
the same instant routinely. Paging by "everything before 11:04:07.318" would
either skip the second event or return it twice depending on which way the
comparison fell, and neither failure is visible in a test with one event per
second in it. The ordering key is therefore the pair `(created_at, id)`, and the
cursor names the whole pair by naming the id.

The two ways a `before` can be wrong are two different answers, because they are
two different mistakes:

| `before` | Status | Code |
| --- | --- | --- |
| not an event id at all | 400 | `invalid_request` |
| a well-formed id that no stored event carries | 404 | `event_not_found` |

The first is a client that has mangled the value - it never was a cursor. The
second is a client holding a cursor this server cannot resolve, which is a
request about a resource that is not there. Neither is an empty page: a client
that has been handed a cursor the server cannot resolve has a bug, and an empty
list is the one answer that would hide it.

**A cursor is scoped by the timeline it is used on, not by where it came from.**
An id belonging to another project's event resolves as a cursor and then filters
that project's timeline, which is the correct behaviour and not a leak: the
cursor says where in *time* to start, and the scope says whose events to return.
The response contains only events of the project or runtime that was asked for.

Events are returned newest first. A timeline that grows at the top is the one
that can be read while it is still being written.

### What is not returned

No internal path, no socket name, no host directory, no credential. The event
envelope is the six fields above and the payload is bounded to what §7 allows,
so there is nothing in a response that was not deliberately put there.

There is also no write endpoint, and no route that would accept one. Events are
produced by the layers that know what happened; an endpoint that took one from a
client would let anything that can reach this server put a row into a history
that nothing can correct afterwards. `POST`, `PUT`, `PATCH` and `DELETE` on a
timeline path are answered as an API error.

A timeline is a growing resource, so no response is cached: no `Cache-Control`,
no `ETag`, no `Last-Modified`. If a later phase wants conditional reads, that is
a decision about a table that only ever gets longer rather than something to add
by default.

## 7. What an event may not contain

An event row is immutable and is written to a file that gets backed up. That
combination is what makes the following a hard rule rather than a preference:

- **no password, token, credential or API key**;
- **no terminal output**, in any form, at any granularity;
- **no free text copied out of an error.**

The first is enforced at the write: a payload whose *keys* name a credential -
`password`, `token`, `secret`, `credential`, `apiKey`, `authorization`, and the
near spellings of each - is refused. That is a structural check on the shape of
the JSON and not a scan of its text, which matters because scanning values would
be unenforceable and would reject a legitimate message for containing a word.
It is a guard rail against the realistic mistake - a caller putting a value in a
field named `token` - and it is not a guarantee; the guarantee is that nothing
in this phase has a credential to put there, and that the one caller is the
runtime bridge, whose payloads are the two fixed shapes in §5.

The second and third are not enforced by a check, because neither can be
detected structurally. They are enforced by the payloads being what §5 says
they are: a state name and a terminal size, and nothing else. `runtime.error`
records *that* a start failed and what state it left, and **does not record the
error string**. The error is in the log, where it can be rotated, and in the
runtime's own message, where it is current rather than permanent. An error
string copied into an append-only row is a string that can never be redacted -
and the one place it would matter is the tmux failure that names a socket path
under the data directory, which §6 says must not be returned.

## 8. What this phase does not do

None of the following exists, and none of it is stubbed, scaffolded, or
partially wired:

- no Claude Code hooks;
- no terminal output parsing, and no pattern matching against a pane;
- no state inference from anything an event contains;
- no task model, no notification, no token or model accounting;
- no automatic decision of any kind;
- no user interface. The only consumer is the API, and the API exists so the
  data model can be exercised and tested before anything is built on it.

There is no event type in this build that no code emits. `runtime.*` are the
four the bridge produces, and the vocabulary stops there until a phase produces
a fifth.

## 9. Where this sits

```text
                 AgentMux

          Event Foundation          internal/event
                 |                  docs/AGENT_EVENTS.md (this file)
          agent_events
                 |
        +--------+--------+
        |                 |
 Runtime Events     User Events      runtime.* now; user.* in a later phase
        |
   Control Layer                    internal/terminal, internal/project
        |
   Runtime Layer                    internal/session
        |
      Claude
```

The event layer sits under everything and depends on nothing: `internal/event`
imports the standard library and nothing else from this repository. The runtime
imports it for the vocabulary of what a runtime can report, and for nothing
else - the recorder is an interface the runtime declares and this package
satisfies, so the runtime is not coupled to a service it does not use.

## 10. Adding a type in a later phase

1. Add the constant to `internal/event`, in the same dotted namespace.
2. Emit it through `CreateEvent`. Do not add a second way to write a row.
3. Add it to §5 or to the table that is right for its source, with the payload
   it carries and why.
4. If it carries anything a user will read, remember that §7 has no exception
   and that the row cannot be corrected afterwards.
