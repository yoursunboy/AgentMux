# The Action Centre

The console says *how much* is waiting. This page says **what** is waiting, and
it is the first screen in AgentMux that names an agent rather than a project.

```text
   Event ──▶ Projection ──▶ Action ──▶ Action Centre ──▶ a person
                                                     (who answers at the terminal)
```

The loop above is the whole design. Everything between the event and the person
was built in Phase 7.1 to 7.3C-2; this phase builds the last arrow and stops
there, deliberately, one step short of the person's answer.

It lives at `/actions`, and one action lives at `/actions/{id}`.

## 1. What an action is, and what it is not

An **action** is a row that says something happened which a person might want to
look at. It is not a task, not a decision, and not a request to AgentMux.

| | |
| --- | --- |
| an **event** | what happened. Append-only, forever, never read by a person directly |
| an **attention level** | whether anybody needs to care. Overwritten by the next event |
| an **action** | something to look at. Stays until something takes it off the queue |
| a **task** | what somebody asked for. A different thing entirely, and it outlives all of them |

`docs/AGENT_ATTENTION.md` §1 is the four readings of one log. What belongs here
is what the *screen* does with the fourth of them.

## 2. Two lists, and why

The brief for this phase names the states the queue may show: `PENDING`,
`RESOLVED`, `EXPIRED`. It also asks the page to lead with what needs handling.
Those two requirements meet in one fact about the projection:

> **`settlePending` resolves permission requests and nothing else.**
> `internal/attention/service.go` settles `[]ActionType{ActionPermissionRequest}`
> on an event that asserts a level other than `ACTION_REQUIRED`, so a
> `PERMISSION_REQUEST` clears itself the moment the session moves on — and a
> `VIEW_FAILURE` or a `VIEW_COMPLETION` never clears at all.

So the queue accumulates one entry per failed or finished attempt, for the life
of the project, and none of them will ever resolve. A page that led with the
length of that list would lead with a number that only ever grows, and would be
telling a person to go and look at a failure from last Tuesday.

The answer is **two lists rather than one**, and it is a product decision taken
on the client, not a change to the projection:

| List | What is in it | Counted in the bar as |
| --- | --- | --- |
| **Needs you** | pending, and `ACTION_REQUIRED` | `needsYou` |
| **Notices** | pending, and anything else | `notices` |
| **Settled** | not pending | — |

**The projection is unchanged by this phase.** No new resolution rule, no
reachable `EXPIRED`, no changed `settles()`. The split is a reading of the rows
that already exist, and `web/src/actions/actionStyle.ts` is the only place that
reads them that way.

**The list is split on the level, not the type.** The three types correspond
exactly to three levels, so a client that decided `PERMISSION_REQUEST` means
`ACTION_REQUIRED` would be a second place the two vocabularies could drift — and
the reason the server derives one from the other (`attention.LevelOfAction`) is
that drifting is the failure mode.

**A level this build does not know is a notice.** A row from a newer server is
something to read; it is certainly not a claim that work has stopped. That is
the direction to fail in.

**A settled action is in neither live list**, whatever it was. A permission
request that was answered does not need anybody, and leaving it under "Needs
you" would be the page telling a person to go and look at something already
done.

`Settled` carries no count in its heading. A count of history is not something
anybody acts on.

## 3. The queue

```text
┌───────────────────────────────────────────────────────────────────┐
│ Actions   Needs you: 1   Notices: 4          Console   Workspace  │
├───────────────────────────────────────────────────────────────────┤
│ Needs you 1                                                       │
│   ⚠  Claude is waiting for permission                             │
│      permission requested                          checkout  ●    │
│                                                           needs you│
│ Notices 4                                                         │
│      Something failed                                             │
│      attempt failed                                studio    ●    │
│                                                           warning │
└───────────────────────────────────────────────────────────────────┘
```

Each row is an `<article>` wrapping one `<a>`, and its text is three things: a
sentence this build wrote from the type, the server's reason phrase, and the
project's name. Nothing else is on it — §6 is why, and it is the load-bearing
constraint of the whole page.

**Rows are in the server's order.** `GET /api/actions` answers pending first and
then newest first, and the two sections are stable *filters* of that array, never
sorts of it. A second ordering here would be a second opinion about what is at
the top of the queue.

**The headings carry the server's counts, not the list's.** A row may be settled
inside "Needs you" — a permission request that was answered between the read and
the render — so a heading of `1` over two rows reads correctly. The number in the
heading is `queue.needsYou`; the number of rows is a different number and is not
shown.

**All three lists empty is one empty state**: *Nothing is waiting.* Not three
headings over nothing.

## 4. The action's own page

Seven labelled rows and no eighth:

| Row | Value |
| --- | --- |
| Project | the name, falling back to the id, which is in the `title` |
| Session | the attempt id, `sess_…` |
| Type | `PERMISSION_REQUEST`, `VIEW_FAILURE`, `VIEW_COMPLETION` |
| Status | a badge reading `needs you`, `warning`, `info`, `resolved`, … |
| Reason | the server's fixed phrase |
| Created | a phrase: *10 minutes ago* |
| Resolved | a phrase, or `-` while pending |

`Created` is relative rather than a clock time because the question a reader has
is "is this from now or from last week", and `formatRelativeTime` is the same
function the rest of the console uses.

The page states its own boundary rather than leaving a person to work it out:

> AgentMux shows you what is waiting; it does not answer it for you. The answer
> is given at the terminal.

There is **no button** on this page — not Allow, not Deny, not Acknowledge, not
Dismiss. A browser suite asserts `button` matches nothing at all here, which is
the strongest form that claim can take.

**`Open project` links to `/`**, the workspace root, and not to the project. The
workspace has no URL-addressed project: it restores its layout from
`localStorage` and nothing in a path or a query selects one. A link carrying an
id nothing read would be a link that appeared to work. §8 lists it as a limit
rather than hiding it.

## 5. Getting there from the console

Two ways in, both from `/dashboard`:

- **the server bar**, which reads `Needs you: 1 · Notices: 4` and links to
  `/actions`. Both numbers come from the dashboard response, so the bar and the
  cards below it are one answer that can be wrong in one way;
- **a project's card**, whose `Actions` row becomes `⚠ 1 action pending` and
  links to `/actions` when the count is not zero. Zero stays a plain number,
  because there is nothing to go and look at.

The card links to the queue and not to one action, and that is forced: a card
carries a count, not an id. A per-project filter is deliberately not in this
phase.

**The bar's queue element uses its own class.** `web/e2e/suites/dashboard.mjs`
clicks `.server-bar__link` and resolves `.server-bar__item` with `.first()`, and
Playwright's strict mode throws when a selector matches two elements. A second
element sharing either class would break a suite about something else, so
`.server-bar__queue` is its own thing — and a unit test pins
`server-bar__link` at exactly one match, so the constraint is checked rather
than remembered.

## 6. What it never shows

§16 of the phase brief is one claim, and it is the same claim
`docs/AGENT_ATTENTION.md` §7 makes about the rows: **an action carries
identifiers, an enumeration and a fixed phrase, and there is no field for
anything else.**

The page is the last place that could break it, because it is the only place a
person reads one. So:

- every value rendered is a **named field** — there is no `{...action}` anywhere
  in `web/src/actions/`, and a builder in `web/src/test/actions.ts` deliberately
  carries `prompt`, `tool_input`, `command`, `token` and `stdout` keys that must
  never reach `container.textContent`. That is what makes the rule a test rather
  than a convention;
- the type's sentence is written **by this build** — `actionTypeLabel` says
  "Claude is waiting for permission" and not what the permission was for;
- the reason is the server's constant. The longest phrase it can be is twenty
  characters (`permission requested`), and the column is bounded at 64, so a
  reason cannot become a payload by accident.

The bad version of this page — the one the brief names — reads
`Claude wants: sudo apt install xxx because …`. That is a path, a command line
and possibly a token, on a page that will be read over somebody's shoulder. It is
also *worse than useless*: the person who can answer the question is the person
at the terminal, and they can already see the whole thing there.

A browser suite reads `document.body.textContent` on both pages and fails on
`tool_input`, `toolInput`, `transcript`, `sk-` or `passphrase`.

## 7. Reading it

```text
GET /api/actions        every project's queue, pending first then newest
GET /api/actions/{id}   one action, with its project named
```

Both routes are additively new in this phase; `GET /api/projects/{id}/actions`
already existed and is unchanged.

**The queue is read through `internal/controller`,** not through
`internal/attention` directly, because naming a project is a join and the
controller is where the joins live (`docs/CONTROLLER_API.md` §1). Naming each
action's project one at a time would be the N+1 that document's §13 forbids, and
it would be the worst kind of it: the queue is the one listing in this API that
grows without bound, because nothing resolves a `VIEW_FAILURE`.

**Polling, not a WebSocket.** §12 asks for a poll and forbids a socket, and the
reason is the same one `docs/CONTROLLER_UI.md` §3 gives for the console: a
terminal needs real-time because a frame a second late is a frame somebody
watched arrive late, and an action does not. Five seconds, expressed as
`useAsyncResource`'s restart key, so there is one place a queue request is made
and one place its result is applied.

**There is still no write.** No `POST /api/actions/{id}/resolve`, no
acknowledge, no dismiss. `TestAnActionCannotBeAnswered` is unchanged by this
phase and is run as written, which is the point: the two new routes did not open
a way to answer anything.

## 8. What is not here

1. **No answering.** `docs/AGENT_ATTENTION.md` §6 is why: answering a permission
   makes AgentMux a participant in a Claude session rather than an observer of
   one, and that is a phase with its own threat model.
2. **No notification.** Nothing pushes anything anywhere. The page is read when
   it is looked at.
3. **A failure or a completion never leaves the queue.** Inherited from the
   projection and deliberately not fixed here — fixing it would mean changing
   when an action resolves, which is a product decision about what "dealt with"
   means. So "Notices" only grows, and the settled list only ever holds
   permission requests that resolved on their own.
4. **`EXPIRED` is still unreachable.** The badge renders it if a server ever
   sends one; nothing produces one.
5. **`Open project` lands on the workspace root**, for the reason §4 gives.
6. **A project that is archived has no name**, because the project listing
   excludes archived projects. The page falls back to the project id, which is
   deliberate rather than a bug.
7. **No per-project filter and no pagination.** The queue has a `limit` and the
   page does not use it; a hundred actions are a hundred things to scroll past.

## 9. Where this sits

- `internal/attention/` — the projection, the queue, and `LevelOfAction`. Read
  by this phase and **not changed** by it.
- `internal/controller/actions.go` — the join, the two counts, and the one
  place a type is folded into "needs you" or "notices".
- `internal/httpapi/attention.go` — the two routes, beside the three reads that
  were already there.
- `web/src/actions/` — the page, its rows, the detail, and the vocabulary
  module.
- `web/src/dashboard/route.ts` — the three-page switch, which is still one
  string comparison rather than a router.
- `docs/AGENT_ATTENTION.md` — the layer underneath, and §6 of it is the reason
  this page has no buttons.
- `docs/CONTROLLER_API.md` — the aggregation the two routes read through.
