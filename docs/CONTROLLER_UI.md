# The Console

The first screen in AgentMux that is read rather than typed into. It answers one
question — *what needs me?* — and it answers it for every project at once.

```text
┌──────────────────────────────────────────────────────────────────────┐
│ ● Online | Runtime: available | CC Switch: Unknown [Switch]   0.6.5  │
├──────────────────────────────────────────────────────────────────────┤
│ ┌────────────────┐ ┌────────────────┐ ┌────────────────┐             │
│ │ checkout       │ │ studio         │ │ alpha          │             │
│ │ Runtime running│ │ Runtime running│ │ Runtime running│             │
│ │ Agent   running│ │ Agent   waiting│ │ Agent   running│             │
│ │ Attention needs│ │ Attention none │ │ Attention none │             │
│ │ Actions 1      │ │ Actions 0      │ │ Actions 0      │             │
│ └────────────────┘ └────────────────┘ └────────────────┘             │
└──────────────────────────────────────────────────────────────────────┘
```

It lives at `/dashboard`. Everything else is the workspace, as it has always
been.

## 1. Layout

Two pages, one document. `App` reads `window.location.pathname` and renders the
console at `/dashboard` and the workspace everywhere else.

**Why a path and not a router.** The application has no router and does not need
one for two pages. The server already serves the single-page entry point for any
unknown path — which is what makes a deep link work at all — so the mechanism is
a string comparison. A router library would be a dependency, a history
abstraction and a route-matching language to answer a question `route.ts` answers
in four lines.

**Why a full navigation and not a client-side swap.** The console has no terminal
in it, so leaving the workspace costs a socket that is reopened on the way back.
That is a page load, which is what a link does, and it means the two pages share
no state that could get out of step.

**The two pages have two bars, and that is the design.** The workspace's bar
reports the terminal — which host answered, which tmux is installed, whether the
socket is up. The console's bar reports the machine and the build. One bar
answering both would be a component that had to be told which page it was on.

## 2. Components

```text
web/src/dashboard/
  DashboardPage.tsx        the page: the three states, and the fourth
  ServerBar.tsx            server state, runtime availability, provider
  ProjectGrid.tsx          the grid, and the empty case
  ProjectPanel.tsx         one project
  StatusBadge.tsx          one status: a word and a dot
  status.ts                status → tone, and nothing about colour
  useDashboard.ts          the read, and the polling
  useDashboardColumns.ts   how many columns fit
  route.ts                 which page this is
```

Each one does one thing and the dependencies run one way:
`DashboardPage → ServerBar | ProjectGrid → ProjectPanel → StatusBadge →
status.ts`. Nothing below `DashboardPage` fetches anything.

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

**One request.** The console reads `GET /api/controller` and nothing else. Every
runtime, agent, attention and action value arrives inside that one response,
already joined and already sorted.

§3 of the phase brief is explicit that the frontend must not call the runtime,
agent, attention and action endpoints and combine them itself, and the reason is
the one `docs/CONTROLLER_API.md` §1 gives: that would be this page reimplementing
a read model the server already exposes — once per client, and four requests per
project.

`client.ts` exposes `fetchDashboard` and `fetchControllerProjects`. Only the
first is used today.

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

**No fixed heights anywhere.** A card grows with its content and the grid grows
downwards; the page scrolls. There is no pagination either: the workspace
paginates because a page holds a fixed number of terminal panels and a grid
without pages would show a terminal nobody can reach. A card is not a terminal.

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

The only server-supplied text that reaches the DOM is a status word and a short
fixed reason phrase from the attention projection — both of which are
enumerations on the server side, and both of which React escapes.

## 7. What is not here

Everything §1 of the phase brief forbids, and each is a later phase's subject
rather than an omission:

- **no terminal**, no xterm.js and no WebSocket. The console has no terminal in
  it, which is what makes it a second page rather than a mode of the first;
- **no input**. Nothing on the console is typed into;
- **no Claude control and no permission action.** The permission badge says a
  question was asked and offers nothing that answers it — the answer is given in
  the workspace's terminal, and `docs/AGENT_ATTENTION.md` §6 is why;
- **no real CC Switch.** The provider section shows `Unknown` and a disabled
  button. It does not guess a model: a name that came from nowhere is a name
  somebody would act on;
- **no mobile app.** The phone layout is the same page at a narrower width.

## 8. Future terminal integration

The plausible next step is opening a project's terminal from its card — selecting
a card and getting the workspace's terminal for that project. What that would
need is already here or already decided:

- **the address.** A card carries the project id, and the workspace already opens
  a specific project by id.
- **the layout.** A card is a grid cell with no fixed height, so a terminal
  panel could replace one without the grid knowing.
- **the data.** The console would need the terminal's own state — the socket, the
  sequence numbers, the control lease — which is exactly what makes it a
  *different* page rather than an addition to this one. The workspace already
  holds all of it, and `docs/PROTOCOL.md` is the protocol.

What it must not become is a second terminal implementation. The workspace's
panel, its xterm instance, its resize rules and its controller lease are one
system, and a console that grew its own would be two — which is why this phase
stopped where it did.
