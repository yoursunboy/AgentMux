# The Console

The first screen in AgentMux that is read rather than typed into. It answers one
question — *what needs me?* — and it answers it for every project at once.

```text
┌──────────────────────────────────────────────────────────────────────┐
│ ● Online | Runtime: available | CC Switch: Unknown [Switch]   0.7.5  │
├──────────────────────────────────────────────────────────────────────┤
│ ┌────────────────┐ ┌────────────────┐ ┌────────────────┐             │
│ │ checkout       │ │ studio         │ │ alpha          │             │
│ │ Runtime running│ │ Runtime running│ │ Runtime running│             │
│ │ Agent   running│ │ Agent   waiting│ │ Agent   running│             │
│ │ ┌────────────┐ │ │ ┌────────────┐ │ │ ┌────────────┐ │             │
│ │ │ ready> _   │ │ │ │ ● working  │ │ │ │ ready> _   │ │             │
│ │ │            │ │ │ │            │ │ │ │            │ │             │
│ │ └────────────┘ │ │ └────────────┘ │ │ └────────────┘ │             │
│ │ Attention needs│ │ Attention none │ │ Attention none │             │
│ │ Actions 1      │ │ Actions 0      │ │ Actions 0      │             │
│ └────────────────┘ └────────────────┘ └────────────────┘             │
└──────────────────────────────────────────────────────────────────────┘
```

The bar's right-hand end also carries `Needs you: 1 · Notices: 4`, linking to
`/actions`. It is left out of the sketch above because the bar is one fixed-height
line and the sketch is 70 columns wide.

It lives at `/dashboard`. The queue behind that link is `/actions`, and
`docs/ACTION_CENTER.md` is that page. Everything else is the workspace, as it has
always been.

## 1. Layout

Three pages, one document. `App` reads `window.location.pathname` and renders the
console at `/dashboard`, the action centre at `/actions` and `/actions/{id}`, and
the workspace everywhere else.

**Why a path and not a router.** The application has no router and does not need
one for three pages. The server already serves the single-page entry point for any
unknown path — which is what makes a deep link work at all — so the mechanism is
a string comparison. A router library would be a dependency, a history
abstraction and a route-matching language to answer a question `route.ts` answers
in a few lines.

**Why a full navigation and not a client-side swap.** A link is what a page load
is, and it means the pages share no React state that could get out of step. What
they do share is the terminal client, which lives above all of them — so walking
from the console to the workspace and back costs no socket at all: the first
subscribe on the new page opens one if the old page's has closed, and it is the
same client identity in either case (`docs/TERMINAL_VIEWER.md` §2).

**The pages have their own bars, and that is the design.** The workspace's bar
reports the terminal — which host answered, which tmux is installed, whether the
socket is up. The console's bar reports the machine, the build, and how much is
waiting. One bar answering all of it would be a component that had to be told
which page it was on.

## 2. Components

```text
web/src/dashboard/
  DashboardPage.tsx        the page: the three states, and the fourth
  ServerBar.tsx            server state, runtime availability, provider, queue
  ProjectGrid.tsx          the grid, and the empty case
  ProjectPanel.tsx         one project: its statuses, its screen, its queue
  TerminalViewer.tsx       one project's terminal, watched and never typed into
  StatusBadge.tsx          one status: a word and a dot
  status.ts                status → tone, and nothing about colour
  useDashboard.ts          the read, and the polling
  useDashboardColumns.ts   how many columns fit
  route.ts                 which page this is

web/src/actions/
  ActionsPage.tsx          the queue, or one action, by id
  ActionCenter.tsx         the queue: two live lists and a settled one
  ActionCard.tsx           one action, as a link
  ActionDetail.tsx         one action: seven rows and no button
  ActionsBar.tsx           the queue page's own bar
  actionStyle.ts           action → group, tone and sentence
  useActions.ts            the read, and the polling
```

Each one does one thing and the dependencies run one way:
`DashboardPage → ServerBar | ProjectGrid → ProjectPanel → StatusBadge →
status.ts`, and `ActionsPage → ActionCenter | ActionDetail → ActionCard →
actionStyle.ts`. Nothing below a page component fetches anything.

`route.ts` stays in `dashboard/` although it answers for all three pages, because
that is where routing has always lived and the workspace it already answered for
had its files somewhere else too.

### `status.ts` — the mapping, not the colour

§8 of the phase brief asks for a status mapping rather than
`if (status === 'RUNNING') green`. The two questions are different: *is this
good, urgent or broken* is a fact about the status and belongs in one file every
panel asks; *what colour is urgent* is a fact about the theme, and the stylesheet
already owns it as a custom property.

```text
a status        agentStyle("WAITING_PERMISSION")
   ↓            { tone: 'attention', label: 'waiting for permission', … }
a tone          .status-badge--attention
   ↓
a colour        color: var(--attention)
```

Only the last step is styling, and a palette change is six lines of CSS rather
than a search through components.

**A value this build does not know is shown, not hidden.** `agentStyle('SUMMONING')`
returns a neutral badge whose label is `SUMMONING`. An older frontend talking to a
newer server shows the word; a dashboard that blanked an unexpected status would
be reporting "fine" about something it could not read.

### Three ways a section can be empty

They look similar and mean different things, so the console renders them
differently — and a test asserts the difference.

| On the wire | On the card | Means |
| --- | --- | --- |
| `"agent": null` | `none` | nothing has ever run in this project |
| `"agent": {"available": false}` | `unavailable` | the server has no projection wired |
| `"agent": {"status": "SUMMONING"}` | `SUMMONING` | the server said something newer than this build |

Collapsing them into one blank would make a project nobody has started
indistinguishable from a server that cannot say.

## 3. API usage

**One request per page.** The console reads `GET /api/controller` and nothing
else. Every runtime, agent, attention and action value arrives inside that one
response, already joined and already sorted.

§3 of the phase brief is explicit that the frontend must not call the runtime,
agent, attention and action endpoints and combine them itself, and the reason is
the one `docs/CONTROLLER_API.md` §1 gives: that would be this page reimplementing
a read model the server already exposes — once per client, and four requests per
project.

**The action centre reads one route too, and for the same reason.** Its page
calls `GET /api/actions` and its detail calls `GET /api/actions/{id}`; neither
touches `GET /api/projects/{id}/actions`, which exists and is what a *per-project*
view would read. The queue joins the project names on the server
(`internal/controller/actions.go`) precisely so this page does not have to — a
name resolved in the browser would be one request per row, over a listing that
grows without bound. `docs/ACTION_CENTER.md` §7 is the long form.

`client.ts` exposes `fetchDashboard`, `fetchControllerProjects`, `fetchActions`
and `fetchAction`. Only the first is used by the console, and the last two only
by the action centre.

### Polling

`useDashboard` re-reads every five seconds. §13 asks for a simple poll and forbids
a WebSocket, and that is the right answer rather than the lazy one: the terminal
has a socket because a frame a second late is a frame somebody watched arrive
late, and a dashboard shows things that change a handful of times a minute. A
socket here would cost a connection, a reconnect policy, a server-side
subscription registry and a lifecycle to get wrong, in exchange for latency
nobody can perceive at this cadence.

The polling is expressed as `useAsyncResource`'s restart key rather than as a
second fetch path, so there is one place a dashboard request is made and one
place its result is applied — and a slow response cannot overwrite a newer one,
which matters more here than anywhere since the reads overlap by design.

## 4. Responsive rules

The grid takes its column count from `useDashboardColumns` and hands it to the
stylesheet as a custom property, so `grid-template-columns` is written once:

```css
.project-grid {
  display: grid;
  grid-template-columns: repeat(var(--columns, 3), minmax(0, 1fr));
}
```

| Width | Columns | Devices |
| --- | --- | --- |
| ≥ 768px | 3 | desktop, tablet sideways, tablet upright |
| ≥ 600px | 2 | a phone on its side, a small tablet upright |
| < 600px | 1 | a phone |

**The breakpoints are not the workspace's.** `workspace/useLayout` puts three
terminal panels across a grid from 1340px, because a terminal panel has to hold
about sixty-six *monospace columns*. A project card holds a name, a status word
and a count — a fraction of the width for the same usefulness — so three of them
fit far earlier. §10's device list is what the numbers above are chosen to
produce.

**No fixed heights anywhere.** A card is a flex column and the screen inside it is
`flex: 1` over a `min-height` floor, so a card grows with its row and the
terminal grows with the card; the grid grows downwards and the page scrolls.
There is no pagination either: the workspace paginates because a page holds a
fixed number of terminal panels and a grid without pages would show a terminal
nobody can reach. A card is a report with a screen on it, and a hundred of them
are a hundred things to scroll past.

### Why the column count is JavaScript at all

For one-versus-three it could be pure CSS, and it would be — except that a rule
living only in a stylesheet cannot be asserted by a test. jsdom does not evaluate
media queries, so "three columns on a desktop, one on a phone" would be a claim
nothing checked.

So the count is a value the hook produces and the grid publishes as
`data-columns`, the unit tests assert that value at several widths, and the
browser suite asserts what was actually painted by reading the computed
`grid-template-columns`. Those are two different claims and only one of them can
be made without a rendering engine.

## 5. Loading, error, and the fourth state

| | On screen |
| --- | --- |
| no data, loading | `Loading controller…` |
| no data, failed | `Unable to load dashboard`, the reason, Retry, and a link back |
| data | the console |
| data, and a poll failed | the console, and a strip saying it is not current |

The fourth is the one worth having. A dashboard that blanked itself because one
refresh timed out would be unreadable in exactly the conditions somebody needs
it — and the data it was already showing was true thirty seconds ago and is very
likely true now. The strip is a strip rather than a panel because it must not be
the thing the eye lands on when the data underneath it is the answer.

## 6. Security

**The console renders named fields, never a spread.** There is no `{...card}`
anywhere in `web/src/dashboard/`, so a field the server adds in a later phase has
no path into the markup — and the fields that must never be shown, a prompt, a
transcript, a command, a credential, have no path either. §16 of the phase brief
asks for that to hold "even if the backend adds them later", and this is how it
holds: by construction rather than by a filter somebody could forget.

A test asserts the shape rather than trusting it: a card renders exactly four
labelled rows, and the browser suite reads the same four off the painted page.

**The action centre makes the same claim, and it is the harder one.** A card
carries counts; an action row is the first thing in this client that names what
an agent is doing, so it is the first place a prompt or a tool input could have
arrived. It did not: `web/src/actions/` renders named fields only, its detail
page has a fixed set of seven labels, and the test builder deliberately carries
`prompt`, `tool_input`, `command` and `token` keys that must never reach the DOM.
`docs/ACTION_CENTER.md` §6 is the argument.

The only server-supplied text that reaches the DOM is a status word and a short
fixed reason phrase from the attention projection — both of which are
enumerations on the server side, and both of which React escapes.

## 7. What is not here

Everything §1 of the phase brief forbids, and each is a later phase's subject
rather than an omission:

- **no permission action.** The permission badge says a question was asked and
  offers nothing that answers it — the answer is given in the workspace's
  terminal, and `docs/AGENT_ATTENTION.md` §6 is why. A console that can type at
  the terminal is not a console that may answer for the person sitting at it.
  **The page that shows the question in full is `/actions`**, added in Phase
  7.4C, and it is the same position one step further in: it names the action, the
  project and the type, and its detail page has no button at all —
  `docs/ACTION_CENTER.md` §4 and §6. The bar's `Needs you` link and a card's
  `⚠ n actions pending` row are the two ways there from here;
- **no real CC Switch.** The provider section shows `Unknown` and a disabled
  button. It does not guess a model: a name that came from nowhere is a name
  somebody would act on;
- **no runtime controls.** A card shows whether a runtime is up and cannot start
  or stop one, which is what `docs/TERMINAL_VIEWER.md` §9 records;
- **no notification.** A card does not tell anybody anything; it is read when it
  is looked at;
- **no mobile app.** The phone layout is the same page at a narrower width.

## 8. Terminal integration

Phase 7.4B-2A built it, and it is `docs/TERMINAL_VIEWER.md` in full. What that
document had said was the plausible next step — opening a project's terminal from
its card — turned out to be the smaller half of the answer: the console does not
open a terminal, it *draws* one, on every card whose runtime is up.

The three things this section predicted were needed were the three things that
decided the shape:

- **the address.** A card carries the project id, and it is what
  `TerminalViewer` subscribes with.
- **the layout.** A card is a grid cell with no fixed height, so the screen
  became a flex child of a flex card — `flex: 1` with a `min-height` floor rather
  than a height, so a card is as tall as its row and the terminal as tall as the
  card.
- **the data.** The socket, the sequence numbers and the control lease are the
  workspace's, and the console reads them through the *same* client — one
  WebSocket for the document, whether it is showing one page or the other. That
  is what keeps this one terminal implementation rather than two, which is what
  this section asked for.

Phase 7.4B-2B added the input half, and the prediction above is why it cost so
little: because the console drives the workspace's client, the lease it asks for
is the same lease the workspace asks for, on the same socket, with the same
`control.request` and `control.release` frames. **No endpoint was added** — not
`/api/runtime/{id}/controller` nor its request and release siblings — because the
console is never a different *kind* of client, only a different page. The three
pieces that are the console's own are:

- `dashboard/TerminalControl.tsx`, the card's control bar: the badge, the
  Request/Release button, and the pending requests a holder can accept or refuse.
  It is drawn from the vocabulary in `components/ProjectTerminal.tsx` —
  `describeControl`, `controlTone`, `controlButton`, `describeRefusal` — so the
  two pages cannot say different things about the same lease;
- the card's own two derived props (`interactive`, `showKeys`, both following the
  lease) and the one that is not (`mayResize={false}`, always);
- the third notice, so a refused message is a sentence in the corner rather than
  the whole card replaced by an error.

`docs/TERMINAL_CONTROLLER.md` is the design in full. What belongs here is the
seam this document has been describing since §1: a card that can take the
keyboard is still a card, and taking it changes nothing about the card's four
rows, its polling, or what it renders. The lease is a property of the connection
the page already had.
