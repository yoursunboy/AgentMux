# The Controller API

One response for a console, instead of the seven it would otherwise have to call
and join itself.

```text
GET /api/controller          the whole console
GET /api/controller/projects the cards, without the server block
```

Read `docs/API.md` for the request and response shapes. This document is why the
endpoint exists, what it reads, and what it must never become.

## 1. Purpose

Without this, a console has to do this:

```text
GET /api/server
GET /api/projects
GET /api/projects/{id}/runtime          ┐
GET /api/projects/{id}/tasks            │
GET /api/projects/{id}/agent-states     │ once per project
GET /api/projects/{id}/attention        │
GET /api/projects/{id}/actions          ┘
```

That is a client reimplementing a join — once per client, in whatever language it
happens to be written in. A web console, an iPad app and a future mobile client
would each write it again, and each would get the sorting subtly different.

It is also **four requests per project**. A hundred projects is four hundred
requests to draw one screen.

The join belongs on the server, where it is five queries for any number of
projects.

### What it is not

**It is not a new source.** There is no controller table, no controller cache and
no state that survives a request. Every field in the response comes from a
service that already existed, read at request time:

```text
existing services ──▶ aggregation ──▶ one JSON response
```

The moment this package stored something, it would be a second place the truth
lives — and the two would disagree, because the services underneath it change
without asking it.

**It is not a new way to change anything.** Every route is a GET. There is no
POST, no PATCH and no DELETE, and their absence is not an omission: the services
it reads each have their own API for being changed, with their own rules, and a
second way in would be a second place every rule is enforced.

**It is not the diagnostics surface.** `GET /api/server` reports tmux paths,
dependency checks and the filesystem layout. This reports four fields from it. A
console shows what a person looking at a screen needs; the endpoint that shows
an operator everything is still there for an operator.

## 2. Response model

```json
{
  "server": {
    "status": "online",
    "runtimeAvailable": true,
    "version": "0.6.5",
    "uptimeSeconds": 4211
  },
  "projects": [
    {
      "id": "p_6f1a…",
      "name": "checkout-service",
      "runtime": { "status": "running" },
      "agent": {
        "available": true,
        "sessionId": "sess_9a27…",
        "status": "WAITING_PERMISSION",
        "lastEvent": "agent.permission_requested",
        "updatedAt": "2026-09-21T09:00:12Z"
      },
      "attention": {
        "available": true,
        "level": "ACTION_REQUIRED",
        "reason": "permission requested"
      },
      "actions": { "available": true, "pending": 1 },
      "updatedAt": "2026-09-21T09:00:12Z"
    }
  ],
  "queue": { "needsYou": 1, "notices": 4 },
  "count": 1
}
```

### `queue`, and why it is on this response

`queue` is how much is waiting **across every project**, split the way a console
reads it: what stops work, and what can be read later.

```text
needsYou   pending, and ACTION_REQUIRED
notices    pending, and anything else
```

It rides here rather than being a second request because a console's bar draws
its own header, and a header that cost an endpoint of its own would be the
console calling two things to render one screen — which is the whole of what §1
above exists to prevent. It is deliberately **not** a sum of the cards' own
`actions.pending` fields: the two are different questions (see §3), and a client
adding up seven cards to draw a headline would be a client doing a join.

**The split is a product decision taken here and nowhere else.** "Which of these
stops work and which can be read later" is about a screen, and
`internal/attention` is deliberately not told it: that package reads a *type*,
`LevelOfAction` reads a type as a *level*, and folding levels into two lists is
the controller's job. A type this build does not know counts as a notice — it is
something to read, and it is not a claim that work has stopped.

**A failure is not fatal and not visible.** A server without an action reader, or
one whose count could not be read, sends `{"needsYou": 0, "notices": 0}` and logs
a warning. A header that could not count what is waiting says nothing rather than
taking the page down.

**Why the counts and the list can disagree.** `GET /api/actions` answers the same
two counts beside a limited page of rows. The counts are pending-only and exact;
the page includes settled rows. A caller that asked for ten rows still gets the
true totals, which is what lets a console show a headline that does not change
when somebody scrolls.

### Three vocabularies, passed through

Each section speaks the vocabulary of the layer it came from, and none is
translated:

| Field | Vocabulary | Values |
| --- | --- | --- |
| `runtime.status` | the project model's | `running`, `stopped`, `error`, … |
| `agent.status` | the agent state projection's | `RUNNING`, `WAITING_PERMISSION`, `COMPLETED`, … |
| `attention.level` | the attention projection's | `NONE`, `ACTION_REQUIRED`, `WARNING`, `INFO` |

Translating any of them would mean a second spelling of the same value, kept in
step by hand. A client that already knows one endpoint's vocabulary should not
have to learn this one's — and a client that sees `runtime.status` disagreeing
with `GET /api/projects/{id}` would not know which to believe.

### Null, and unavailable

These are two different things and the response distinguishes them:

| | Means |
| --- | --- |
| `"agent": null` | nothing has ever run in this project |
| `"agent": {"available": false}` | the server cannot answer — it has no state projection |

A client renders the first as "nothing running" and the second as "unknown". It
is the difference between a project nobody has started and a server that cannot
say.

### What a card never carries

Identifiers, statuses, counts and times. There is no field for a prompt, a
transcript, terminal output, a command, a tool input, a token count or a
credential — and their absence is the design rather than an omission. A
dashboard says what is happening; the things that would make it say more are the
things that must not leave the machine.

## 3. Data sources

| Section | Read from | Which is |
| --- | --- | --- |
| `server` | `host.Info`, the version constants, the process start time | the same assembly `GET /api/server` uses |
| `id`, `name`, `runtime.status` | the project service's listing | one query |
| `agent` | the agent state projection | one query for every project |
| `attention` | the attention projection | one query for every project |
| `actions.pending` | the attention projection's action counts | one grouped query |
| `queue` | the attention projection's pending counts by type | one grouped query |

The runtime status is the project service's own derived field, which it computes
from the runtime manager's in-memory map. That is what makes reading a hundred
projects one query rather than a hundred: nothing in this path asks tmux
anything.

### `agent` is about the most recent attempt

A project can have many attempts. The card describes the newest one, because
that is what "what is the agent doing" means — an agent is one process at a time.

`actions.pending` counts the **whole project**, not just that attempt. An action
is a backlog, and a backlog is something a project has rather than something one
attempt has. The two are different kinds of number and the asymmetry is
deliberate.

The last two rows of the table above are the same rows grouped two ways — by
project for the cards, by type for the bar — and they are two queries rather than
one because they answer two different questions. A later phase could read the
type-grouped counts once and fold them per project, and has not: the card's
number is a count of a project's pending actions *whatever they are*, and the
bar's two numbers are the whole installation's split by what they demand.

## 4. Sorting

Cards come back in the order §6 of the phase brief asks for:

| Rank | Card |
| --- | --- |
| 0 | attention `ACTION_REQUIRED` |
| 1 | attention `WARNING` |
| 2 | agent `RUNNING`, `WAITING_PERMISSION` or `WAITING_INPUT` |
| 3 | agent `COMPLETED` |
| 4 | everything else |

A console leads with what needs somebody. That is the whole reason the sort is
not by name, by id or by when the project was registered.

Within a rank the order is most recently changed first, then name, then id. The
id is the last resort and is never why two cards are in the order they are — it
is there so that two projects sharing a name and a change time still come back
in the same order twice, which is what stops a console that redraws every few
seconds from shuffling.

A section that is unavailable ranks as though it were absent, so a server
without the attention projection still sorts its working projects above its idle
ones rather than flattening every card together.

## 5. Performance

Five queries for any number of projects:

```text
projects.List                        one
agents.StatesForProjects             one
attention.AttentionForProjects       one
attention.PendingCountsForProjects   one
attention.PendingCountsByType        one     the bar's two counts
```

The implementation this exists to avoid is a loop that asks each project four
questions. That is the N+1 §13 of the phase brief forbids, and it is what a
client would have written for itself.

A console with no projects asks almost nothing: the three projection reads are
skipped rather than issued with an empty list, because a query for no rows is a
query that cannot match anything. The queue count is not skipped, because "how
much is waiting anywhere" is a question about the installation rather than about
its projects, and an empty project list is not the same as an empty queue.

**There is no cache.** §14 allowed a short in-memory one and it is not here,
because there is nothing yet to cache: the reads are local and bounded, and
a cache would be a copy of the truth that could disagree with it. A cache is
worth adding when a measurement says so, and the measurement to make is the
dashboard's p99 on an installation with a hundred projects — which this phase
did not have.

## 6. Security

**Statuses only.** The response carries identifiers, enumerations, counts and
times. Nothing it returns describes what an agent was told, what it said, or
what it ran.

A test asserts this over the bytes a client receives rather than over the DTO: it
searches the encoded response for the words that would appear if anything else
had got in — `password`, `token`, `secret`, `prompt`, `tool_input`, `transcript`
and their neighbours — and separately asserts that a card carries no field the
test does not know about. That second half is what would notice a later phase
adding one.

**It is one more unauthenticated read.** This endpoint joins what seven others
already exposed; it does not widen what is reachable, and it does not
authenticate. `docs/SECURITY.md` is the standing position, and it is unchanged
by this phase.

## 7. Limitations

1. **No pagination.** A hundred projects is the design point from §13; a
   thousand would be a large response and there is no cursor. The project
   listing has a limit option and this does not use it.
2. **No filtering.** A client that wants only the projects needing attention
   reads all of them and ignores the rest.
3. **Archived projects are excluded**, because the project listing excludes them
   by default. A console showing a project's history would need `GET
   /api/projects` directly.
4. **The `agent` section is one attempt, not the history.** A project whose
   current attempt is fine hides that its previous one failed — except through
   `actions.pending`, which counts the project.
5. **A project that failed stays at `WARNING` forever** until something runs in
   it again. Nothing clears an attention level except an event, and an attempt
   that ended has no further events. This is inherited from Phase 7.3C-2's
   design rather than introduced here, and it is the reason the pending count
   matters: the queue is where a person actually clears things.
6. **The server block is not cached**, so a dashboard request runs the same
   assembly `GET /api/server` does — which includes a dependency check and a
   tmux probe. Both are cached by the layers that do them, but a console polling
   every second is doing more work than the five queries above suggest.
7. **`queue` counts a backlog that only partly drains.** `needsYou` rises and
   falls, because a permission request settles itself when the session moves on.
   `notices` rises and does not fall, because a failure or a completion action is
   never resolved — `docs/AGENT_ATTENTION.md` §3 is the lifecycle that makes that
   true. A console showing both numbers is showing one that
   a person can clear and one that records what has happened. Anyone tempted to
   make the second one go down should read `docs/ACTION_CENTER.md` §2 first: the
   fix is a resolution rule, which is a product decision rather than a bug.

## 8. Where this sits

- `internal/controller/` — the aggregation, and the only package that reads
  three services to build one answer.
- `internal/httpapi/controller.go` — the two routes.
- `internal/controller/actions.go` — the queue the two `GET /api/actions` routes
  read through, and the one place a type is folded into `needsYou` or `notices`.
- `docs/AGENT_STATE.md` and `docs/AGENT_ATTENTION.md` — the two projections the
  cards are mostly made of.
- `docs/ACTION_CENTER.md` — the screen that reads the queue, and why the two
  counts above are shaped the way they are.
- `docs/ARCHITECTURE.md` §3 — where the aggregation sits among the components.
