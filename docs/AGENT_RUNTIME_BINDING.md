# Binding a Claude Agent to a Runtime

Phase 7.3B-1 built the Claude adapter: a thing that receives Claude Code's hook
events and records them as `agent.*` events. Phase 7.3B-2 is what connects it to
the product. This document is the record of that connection — what it decides,
what it deliberately does not, and where it stops.

The short version: **one request starts a terminal if there is not one, launches
Claude in it under a session id AgentMux chose, attaches a receiver to the
hooks it will fire, and — when the caller names a task — records the attempt and
binds it.** Nothing else in the build decides which Claude session is which
attempt. This is where that decision is made, and it is made in memory.

## 1. Lifecycle

```text
POST /api/projects/{id}/runtime/agent/start   {"taskId": "task_…"}   (optional)

  1. runtime          ensure it is RUNNING; start it if it is not
  2. agent            if one is already running, adopt it and stop here
  3. session id       mint a version-4 UUID — AgentMux chooses, Claude obeys
  4. attempt          tasks.CreateSession          → CREATED
  5. receiver         adapters.Attach              → a bound port and a nonce path
  6. settings         adapters.HookSettings        → the document naming that port
                      settings.Write               → a file under the data directory
  7. launch           claude --session-id <uuid> --settings <path>
  8. bind             tasks.AttachSessionRuntime, then → RUNNING
```

Two of those steps are ordered the way they are for reasons worth stating.

**The receiver comes before the document.** The settings document names the
adapter's port. A document written first would name a port nothing is listening
on, and Claude would deliver every hook into a closed socket — *silently*,
because an undelivered hook produces no error at the hook site. A broken
configuration would be indistinguishable from an agent that had nothing to say,
which is the exact failure this ordering removes.

**The session id comes before the process.** An id chosen after the launch would
be a fact read back from the CLI rather than a decision, and there would be a
window in which a running session's hooks named a session AgentMux had never
heard of. Choosing first is what makes every payload self-identifying.

### Adoption

If an agent is already running in the runtime, nothing is launched and nothing
is bound. That is not an optimisation; it is the honest answer.

A Claude process that AgentMux did not start has a session id AgentMux never
chose and no hook configuration pointing at a receiver. There is no observation
to bind an attempt to. Recording one anyway would put a row into the task
history with no evidence behind it, and attaching a receiver would leave a
listener nothing will ever post to — which reads exactly like a healthy agent
with nothing to report.

So an adoption with a `taskId` is **refused**, with an error that says what to
do about it: stop the agent and start it again.

## 2. Runtime relationship

The runtime is unchanged. This phase adds launch arguments and an observer; it
does not touch the tmux backend, the PTY, the control client, the terminal
transport, or the runtime state machine.

```text
Project ──▶ Runtime ──▶ AgentSession ──▶ Claude session_id ──▶ agent.* events
```

`docs/TASK_MODEL.md` §1 orders these levels Project → Task → AgentSession →
Runtime, and that ordering holds. An attempt is what names a runtime, and a
runtime is where the attempt ran.

The phase brief asked for the chain to enter at `POST runtime/start`. It enters
at the agent endpoint instead, for two reasons that are properties of the model
rather than preferences:

- `agent_sessions.task_id` is `NOT NULL` with a foreign key to `tasks`
  (`migrations/0005_agent_sessions.sql`). An AgentSession *is* an attempt at a
  task, and `runtime/start` carries no task.
- A runtime outlives the agents in it. `runtime/start` starts a terminal that may
  never host Claude, and stopping the agent leaves the terminal running — which
  is the whole point of hosting a program in tmux.

So `runtime/start` still means "start a terminal", `agent/start` means "make
Claude run here", and the second one starts the first when it has to. That also
makes true a claim `internal/httpapi/runtime.go` had been making in a comment
for two phases — that the agent endpoint's budget covers a runtime start too.
It does now.

**One runtime id, one spelling.** The runtime identifier is
`project.SessionNameFor(projectID)`, which is `"amx-" + projectID` and is the
same string tmux is given, the same string `runtime.*` events carry in their
`runtime_id` column, and the same string `agent_sessions.runtime_id` stores.
It is computed from the project id and is not derived from a directory, a
process name, or a clock.

## 3. AgentSession mapping

| What happened | The attempt becomes |
| --- | --- |
| `agent/start` with a task, launch succeeded | `CREATED` → `RUNNING`, `runtime_id` attached, `started_at` set |
| `agent/stop` and the interrupt took | `CANCELLED`, `ended_at` set |
| the `SessionEnd` hook arrived | `COMPLETED`, `ended_at` set |
| the launch failed | `FAILED` |
| `DELETE /api/projects/{id}/runtime` | `FAILED` |
| `POST /api/projects/{id}/runtime/stop` | `CANCELLED` |
| the agent died and the next start replaced it | `FAILED` |

The statuses are the task model's own and no transition rule was changed. The
translation from "how it ended" to a status happens in exactly one function
(`agent.taskStatusFor`), so this package never restates the task vocabulary and
the task model never learns what an outcome is.

**Task status is never touched.** §六 of the phase brief is explicit and the code
follows it: an attempt ending is not the work ending. Completing a task is a
statement about the work, and nothing here is in a position to make it — a
session that ran to its own end is evidence that it ran, not that it succeeded.

**A declined interrupt does not close anything.** `StopAgent` can come back with
the agent still running, because a program that handles Ctrl-C itself is still
there. Closing the attempt then would record that a session ended while it was
still going, and detaching would stop observing it. The attempt stays `RUNNING`
and the runtime's own explanation is passed back to the caller.

**An adopted agent records no attempt.** See §1.

## 4. Claude session binding

```text
AgentMux                       Claude Code                    AgentMux
   │                               │                             │
   ├── uuid := UUIDv4() ───────────┤                             │
   ├── --session-id <uuid> ───────▶│                             │
   │                               ├── SessionStart hook ───────▶│  session_id = <uuid>
   │                               ├── system/init ─────────────▶│  session_id = <uuid>
   │                               ├── result ──────────────────▶│  session_id = <uuid>
```

The id is chosen before the launch and dictated on the command line. Phase
7.3B-0 verified that Claude Code reports that exact value back in every hook
payload, in the stream's init message, and in the final result — five
independent places, one distinct id per run.

That is the whole correlation. Nothing is matched on a working directory, a
process name, a start time, or a tmux session name. Each of those is a way a
correlation can be silently wrong, and a wrong correlation in an append-only log
is permanent.

**Claude refuses an id already in use**, so a fresh one is minted per launch and
a collision is a launch failure rather than a retry with the same value. Since
the ids are random, the collision case is a resumable session — and resuming
wants `--resume`, which this build does not do.

**The binding is in memory.** It is the adapter's `Binding`, reachable through
`claude.Manager.SessionFor`, and it holds the Claude session id, the project, the
runtime and the attempt. It is not a column and no schema changed (§十三 of the
brief). §6 says what that costs.

## 5. Which signals are reachable

This is the part most likely to be misread, so it is stated plainly.

**The five hooks are the whole reachable vocabulary on this path.** Claude Code
runs here as a TUI in a tmux pane, with no `--output-format stream-json`, so the
stream half of the adapter — `claude.Adapter.ConsumeStream` — has **no
production caller at all**. It exists, it is tested, and nothing feeds it.

The consequence is that two of the seven `agent.*` types do not occur in the
running product:

| Type | Source | Reachable here |
| --- | --- | --- |
| `agent.started` | `SessionStart` hook | yes |
| `agent.prompt_submitted` | `UserPromptSubmit` hook | yes |
| `agent.permission_requested` | `PermissionRequest` hook | yes |
| `agent.completed_candidate` | `Stop` hook | yes |
| `agent.session_ended` | `SessionEnd` hook | yes |
| `agent.completed` | the `result` envelope | **no — needs the stream** |
| `agent.failed` | the `result` envelope | **no — needs the stream** |

Wiring the stream would mean reading the pane, which is the runtime layer's
output and which this phase is forbidden to parse. It is a later phase's
decision, and it is the one that would make "did this turn succeed" answerable
rather than merely "did it stop".

`agent.completed_candidate` is why that distinction is already in the vocabulary.
`Stop` means the model stopped producing, which is also what an aborted turn, a
refused tool call and a crash after the last token look like from there.

## 6. Where the binding lives, and what that costs

The mapping from a Claude session id to an attempt is in the coordinator's
memory. It is rebuilt from scratch by the next launch, and it is gone when the
process is.

**What this buys.** No schema change, no migration, and no table that has to be
kept consistent with a process that may have died. The alternative — a
`claude_session_id` column — was considered and declined in the brief, and the
reason it is affordable is that an event is already *findable*: a stored event
names its project and its runtime, and `idx_agent_sessions_runtime` finds every
attempt that ran in that runtime. What the column would add is precision about
*which* attempt, when a runtime has hosted several.

**What it costs, stated honestly:**

1. **A server restart loses the binding, not the session.** Claude is a child of
   the shell inside tmux and outlives the server. After a restart the runtime is
   reconciled and the agent is adopted — and an adopted agent is not observed,
   because the new process cannot know the id the old one dictated. Events resume
   when the agent is next restarted.
2. **An attempt can be left `RUNNING`.** `Service.Close` deliberately does not
   change any attempt's status, because the server stopping is not the work
   stopping. An attempt whose server went away is still running in its terminal,
   and nothing on the next boot reconciles it. Within a running process the next
   `agent/start` closes it as `FAILED` — a binding that survives the "no agent is
   running" check can only be an attempt whose process went away without a
   `SessionEnd` hook.
3. **The Claude session id is nowhere in the database.** By decision, and it is
   also what keeps it out of every event payload.

A later phase that wants the binding to survive a restart needs the column, and
with it a migration and a reconciliation step at boot. Neither is here.

## 7. Event routing

Every `agent.*` event is written by the adapter through
`event.Service.CreateEvent`, which remains the only path into `agent_events`.
The coordinator holds no database handle and writes no row directly — the
attempt goes through the task service and the events through the event service.

The two values an event is placed by come from the adapter's configuration,
which the coordinator supplies at attach time:

| Column | Value |
| --- | --- |
| `project_id` | the project the agent was started in |
| `runtime_id` | `"amx-" + projectID` |
| `source` | `"agent"` |

An event does **not** carry the attempt id. The relationship is reached the
other way round: a reader who has a runtime id finds its attempts through
`GET /api/tasks/{id}/sessions`, and a reader who has an attempt finds its
runtime in the attempt's own `runtimeId` field. Nothing was added to the event
payload shape, which is what §十一 asked for.

### The payload field that had to be renamed

`session.created` and `session.status_changed` named their session `sessionId`
until this phase. `event.CheckPayload` refuses any field whose normalised name
ends in `sessionid` — the entry exists to catch a session *token*, which is a
credential — and the refusal was swallowed by `noteEvent`. **Both events were
never written, and nothing said so.**

The field is now `agentSession`. The obvious repairs do not work:
`agentSessionId` and `agent_session_id` both normalise to `agentsessionid`, which
the same suffix rule refuses for the same reason. The security rule was not
weakened to accommodate this; the field name changed instead.

There is now a test that runs the real `event.CheckPayload` over the real
payload builders, because the failure mode is silent by construction and a
recorder double never sees it.

## 8. Failure handling

The coordinator takes back **what it did and nothing else.**

| Failed at | Taken back |
| --- | --- |
| the runtime would not start | nothing — there is nothing |
| the task does not exist, or belongs to another project | nothing — nothing was created |
| the receiver would not bind | the runtime, if this call started it |
| the settings document would not write | the receiver, and the runtime if this call started it |
| the agent never appeared | the receiver, the document, the attempt → `FAILED`, and the runtime if this call started it |

A runtime that was **already running** when the call arrived is never stopped. The
caller asked for an agent; ending a terminal somebody was using because a launch
failed would be a much larger failure than the one being reported.

The cleanup runs on a context that outlives the request, because the caller may
have hung up and the tidying still has to happen. It is bounded, and no failure
in it can replace the error the caller is being given.

**Cross-project misattribution is refused before anything is created.** A task
exists under one project and the task service enforces that when the task is
made — but nothing enforced it for a *later* request naming both, because until
this phase no request named both. `POST /api/projects/{A}/runtime/agent/start`
with a task belonging to B would bind A's runtime to B's attempt and write
`session.created` under B while `session.status_changed` carried A's runtime id
— into a log that cannot be corrected. `agent.Service.taskBelongsTo` checks it
first, and the error says which project the task belongs to.

## 9. Security

The adapter's own rules are unchanged and are stated in
`docs/CLAUDE_ADAPTER.md` §7. What this phase adds:

**The settings document is a file on disk under the data directory.** Not in the
project — a project is somebody's repository, and `CLAUDE.md` rule 11 forbids
storing runtime metadata there. The path is `<DataDir>/claude/<runtimeId>/settings.json`,
the directory is `0700` and the file `0600`. The document is not a credential,
but it tells a program where to send what it observes, and a writable one would
let anything on the machine redirect that. It is written atomically — a reader
that caught it half-written would see something that is not JSON — and it is
removed when the adapter is detached.

**The runtime id is checked before it becomes a path component.** It is
`"amx-" + projectId` and is not a value a caller can point anywhere, but the
check is what keeps that true if the naming rule ever changes.

**Nothing about the session id reaches the log or the API.** It is on the command
line — which is visible to anything that can read the host's process list, the
same as every other argument — and in the adapter's memory. It is not in a
payload, not in a response, and not in a column. `Run.SessionID` is tagged
`json:"-"`.

**No new credentials, and none read.** The launch adds two arguments, neither of
which is a secret: a random identifier and a path. Authentication is Claude
Code's own, exactly as it was before this phase — AgentMux does not hold an API
key, does not sign in on a user's behalf, and does not see a token.

## 10. What this does not do

Gathered from the sections above so it can be read in one place:

- no permission control. `agent.permission_requested` is recorded and read by
  nobody; every response the receiver sends has an empty body;
- no task status change, ever;
- no notification, no UI, no dashboard;
- no `agent.completed` or `agent.failed` on this path — they need the stream, and
  the stream is not wired (§5);
- no binding that survives a restart, and no reconciliation of attempts left
  `RUNNING` by one (§6);
- no terminal parsing, and none anywhere downstream of this;
- no Claude Agent SDK. The CLI is launched by the runtime, in a tmux pane, by
  typing a command into a shell — and the terminal is still the only thing that
  can see what it prints.

## 11. Where this sits

```text
HTTP  POST /api/projects/{id}/runtime/agent/start
        │
        ▼
internal/agent          the coordinator: the chain, the binding, the settings
        │                 it declares RuntimeOperator, AdapterOperator,
        │                 SessionOperator and SettingsWriter and imports
        │                 session, claude and task — and nothing imports it
        │                 except cmd/server and httpapi
        ├──▶ internal/session   the terminal, and the process in it
        ├──▶ internal/claude    the adapter manager, then the adapter
        └──▶ internal/task      the attempt
```

`internal/session` never sees a `*claude.Adapter`; `internal/claude` never sees
a task or a runtime. The coordinator is the only package that knows all three
vocabularies, and it is the only one that has to.

`docs/CLAUDE_ADAPTER.md` is the adapter's own document. `docs/AGENT_EVENTS.md`
is the event vocabulary. This one is the join between them.
