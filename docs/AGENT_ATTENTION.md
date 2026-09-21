# Agent Attention and Actions

An agent state says what an agent is doing. It does not say whether anybody
needs to care. This document is about the second question, and about the queue of
things a person might do about the first.

Phase 7.3B-2 connected a running Claude session to the event log. Phase 7.3C-1
turned that log into a current state. Phase 7.3C-2 asks the question the other
two do not: **of everything that is happening, what needs me?**

## 1. Four readings of one log

```text
agent_events      what happened, append-only, forever
agent_states      what the agent is doing
agent_attention   whether anybody needs to care
agent_actions     what they might do about it
```

They are deliberately four layers, and the separation is the design. Consider
two agents:

| | agent A | agent B |
| --- | --- | --- |
| state | `RUNNING` | `WAITING_PERMISSION` |
| attention | `NONE` | `ACTION_REQUIRED` |

Both are working. Only one is waiting on a person. A schema that folded
attention into the state would make "what is it doing" and "does anybody need to
act" the same question, and the whole point of this layer is that they are not.
The same argument separates the runtime — which says whether a terminal exists —
from all three.

### The three status vocabularies, again

Phase 7.3C-1 drew the line between a runtime's state, a task's status and an
agent's state. This adds a fourth reading rather than a fourth vocabulary of
statuses: an attention level is about a *person*, and an action type is about
something they might *look at*.

| Layer | Answers |
| --- | --- |
| `internal/session` | is there a terminal, and what is running in it |
| `internal/task` | what does somebody want done |
| `internal/agentstate` | what did the agent report about itself |
| `internal/attention` | does anybody need to look, and at what |

## 2. Attention

```go
type Attention struct {
    AgentSessionID string   // the attempt
    ProjectID      string
    Level          Level    // NONE | ACTION_REQUIRED | WARNING | INFO
    Reason         string   // a short fixed phrase
    UpdatedAt      time.Time
}
```

One row per attempt, overwritten by each event that has something to say.

| Level | Means | Produced by |
| --- | --- | --- |
| `NONE` | nothing to see | an attempt created, a session bound, `agent.started`, `agent.prompt_submitted`, an attempt cancelled |
| `ACTION_REQUIRED` | nothing progresses without a person | `agent.permission_requested` |
| `WARNING` | something went wrong | `agent.failed`, an attempt that failed |
| `INFO` | worth knowing | `agent.completed`, `agent.session_ended`, an attempt that completed |

**`ACTION_REQUIRED` and `WARNING` are different for a reason.** A warning can be
read later. An action-required cannot: the agent is blocked, and it stays blocked
until somebody answers the question.

**Every attempt has a row.** An attempt that was created and never started is at
`NONE`, from its own creation event, rather than absent. A client renders one
shape, and `GET /api/sessions/{id}/attention` answers for an attempt the moment
it exists.

## 3. Actions

```go
type Action struct {
    ID             string        // derived from the event that raised it
    AgentSessionID string
    ProjectID      string
    Type           ActionType    // PERMISSION_REQUEST | VIEW_FAILURE | VIEW_COMPLETION
    Status         ActionStatus  // PENDING | RESOLVED | EXPIRED
    Reason         string
    CreatedAt      time.Time
    ResolvedAt     *time.Time
}
```

An attention is a level that the next event overwrites. An action is a row that
stays until something takes it off the queue. That is why they are two tables
rather than one row with a list in it.

| Type | Raised by | What it means |
| --- | --- | --- |
| `PERMISSION_REQUEST` | `agent.permission_requested` | Claude asked to use a tool and is blocked |
| `VIEW_FAILURE` | `agent.failed`, an attempt that failed | a turn, or a start, went wrong |
| `VIEW_COMPLETION` | `agent.completed`, an attempt that completed | a turn finished |

### The lifecycle

```text
   raised on an event
        │
        ▼
     PENDING ──────▶ RESOLVED      the attempt moved on
        │
        └──────────▶ EXPIRED       nothing produces this yet
```

**`RESOLVED` is reached by evidence, not by a person.** There is no event that
says "the permission was answered" — the answer is given at the terminal, and the
log never sees it. So an action is resolved by the next thing that happens to the
attempt: the event that follows shows the agent has moved on, and that is the
only evidence there is.

Concretely, an event resolves pending permission requests when it asserts a level
other than `ACTION_REQUIRED`. A submitted prompt is the ordinary case; a failure
is another, because an agent that broke is not still waiting for anything.

**`agent.completed_candidate` resolves nothing.** The model stopping is not the
question being settled: a turn can end with a tool call still refused and
unanswered.

**`EXPIRED` is unreachable.** Nothing produces it. It is in the vocabulary
because an action queue needs a way to say "too late to matter", and the policy
that would decide when — an age, a count, a project being archived — is a product
decision no phase has made. It is listed here rather than left for somebody to
discover, the way `WAITING_INPUT` is in `docs/AGENT_STATE.md` §3.

### Why an action's id is derived

`ActionIDForEvent` turns `evt_<hex>` into `act_<hex>`. The identity is a function
of the event that raised it, and that is the whole of what makes the queue
idempotent:

- a **rebuild** replays the entire log, and would otherwise raise a second copy
  of every action;
- an event **projected twice** would do the same;
- and the insert is a plain `ON CONFLICT DO NOTHING`, which needs nothing else —
  no uniqueness rule on a timestamp, no comparison, no clock.

It also means a re-raise can never reset a settled action, which a conditional
upsert would have done.

## 4. The projection

```text
ClaudeAdapter ──▶ event.Service.CreateEvent
                        │
                        ├──▶ agentstate.Project     the state
                        │
                        └──▶ attention.Project      the level, and the queue
```

Both run inside `CreateEvent`, in the order the server installs them, and that
order is a real dependency: an `agent.*` event names a runtime and no attempt,
and the state is what says which attempt a runtime belongs to.

Neither projection knows the other exists. `internal/attention` declares the one
method it needs from the state — `StateByRuntime` — and the composition in
`cmd/server` is the single line that relates them.

### Which events are read

| Event | Level | Action |
| --- | --- | --- |
| `session.created` | `NONE` — "attempt created" | — |
| `session.status_changed` → `RUNNING` | `NONE` — "attempt running" | — |
| `session.status_changed` → `COMPLETED` | `INFO` — "attempt completed" | `VIEW_COMPLETION` |
| `session.status_changed` → `FAILED` | `WARNING` — "attempt failed" | `VIEW_FAILURE` |
| `session.status_changed` → `CANCELLED` | `NONE` — "attempt cancelled" | — |
| `agent.started` | `NONE` | — |
| `agent.prompt_submitted` | `NONE` | — |
| `agent.permission_requested` | `ACTION_REQUIRED` | `PERMISSION_REQUEST` |
| `agent.completed` | `INFO` | `VIEW_COMPLETION` |
| `agent.session_ended` | `INFO` | — |
| `agent.failed` | `WARNING` | `VIEW_FAILURE` |
| `agent.completed_candidate` | *no change* | — |
| anything else | ignored | — |

### Why the attempt's own events are in that table

§7 of the phase brief gives six rows, all of them `agent.*`. The rows for
`session.status_changed` are an addition, and the reason is that without them the
projection would miss the commonest failure there is.

**An agent that never launched produces no `agent.*` event at all.** The
coordinator marks the attempt `FAILED`, and the only record is a
`session.status_changed`. A home screen built on this projection would say
"nothing needs you" about a start that failed — which is the one answer it must
never give.

The session status is read from the event's own `to` field. Nothing is inferred.

### `completed_candidate` moves nothing

Same rule as the state's, for the same reason: it is not evidence of anything. An
aborted turn, a refused tool call and a crash after the last token all look the
same from the `Stop` hook, so it records no level and resolves no action.

### Resolution is decided by the event, not by the state

The obvious way to decide whether a permission was answered is to read the agent
state and ask whether it is still `WAITING_PERMISSION`. That is wrong, and the
reason is worth writing down: **a rebuild reads the state as it is now**, long
after the event it is replaying, so every replayed event would see the attempt's
*final* status and resolve on the first one. The rebuild would then disagree with
the live projection about when an action was resolved. Reading the decision off
the event makes it the same input — and the same answer — in both.

## 5. Rebuild

`attention.Service.Rebuild` recomputes both tables from `agent_events`. Like the
state projection's, it **reads before it clears**, so a log that cannot be read
leaves the projection as it was rather than emptying it.

**It is one method rather than two.** The brief sketched `RebuildAttention` and
`RebuildActions`; they are one pass here because they are folded from the same
events in the same order, and two methods would either replay the log twice or
let one table be recomputed without the other. The report says what each holds
afterwards, so nothing the split would have given is lost.

**It reads the state while folding**, so the states must be current before it
runs. At start-up they are: `cmd/server` rebuilds the state projection first and
the attention projection second, and that order is the same dependency the
projector chain expresses.

A rebuild reproduces the live projection exactly, including `resolved_at` — which
is what §4's note about deciding by event rather than by state buys.

## 6. What there is not

**There is no way to answer an action.** Not an endpoint, not a field, not a
decision. A `PERMISSION_REQUEST` action records that Claude asked for something;
it does not allow it, deny it, or influence it in any way. There is no `ALLOW`,
no `DENY` and no `EXECUTE` in the vocabulary, and their absence is the boundary
this phase stops at rather than an unfinished edge.

The reason is not caution for its own sake. Answering a permission makes AgentMux
a **participant** in a Claude session rather than an observer of one: it would
have to hold an opinion about what a tool call does, it would be the thing that
decided, and the trust boundary around the hook receiver — a loopback endpoint
guarded by an unguessable path — would stop being a boundary and become an
authorization surface. That is a phase with its own threat model, not a route
added to this one.

So the loop is deliberately open, and it closes at the terminal:

```text
Claude asks ──▶ event ──▶ ACTION_REQUIRED + a pending action
                                        │
                          a client shows it to somebody
                                        │
                          they answer it in the terminal
                                        │
                          the next event resolves the action
```

**There is no notification.** Nothing pushes anything anywhere. The three read
endpoints are how a client finds out.

## 7. Security

An attention row and an action row each hold identifiers, an enumeration and a
short fixed phrase. Neither has a field for a prompt, a tool input, a transcript
path, a credential or a token count, and their absence is the design.

**`Reason` is a constant from this package's vocabulary**, never a quotation.
The longest phrase is nineteen characters and the column is bounded at 64, which
is what makes it impossible for a reason to become an event payload by accident.
Nothing here names a tool, quotes a payload, or includes anything a person wrote.

**`EXPIRED` and `RESOLVED` are not decisions.** `RESOLVED` is what the log shows,
and nothing consults it to decide anything.

## 8. Reading it

```text
GET /api/sessions/{id}/attention    one attempt
GET /api/projects/{id}/attention    a project's, most recently updated first
GET /api/projects/{id}/actions      the queue: pending first, then newest
```

A client that wants "what needs me" reads the project's actions and takes the
pending ones from the top. A client that wants "what is everything doing" reads
the project's attention. Neither exists to be polled on a timer: the answers
change when an event arrives, and a client already watching a runtime over the
WebSocket knows when something happened.

**What a UI would do with it, when one exists.** The actions list is a queue
panel: the pending items, newest first, each one naming the attempt and what
kind of thing it is. Selecting one opens that attempt's terminal — which is where
the answer actually gets given, and which is why the action carries no button
that answers anything. The attention list is a per-project badge: the worst level
in the project, and a count of what is waiting. Neither needs anything the
endpoints do not return today.
