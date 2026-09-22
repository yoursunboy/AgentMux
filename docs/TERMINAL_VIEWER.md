# The Terminal Viewer

The console's card, with the terminal on it.

```text
Dashboard ──▶ Project Panel ──▶ Terminal Viewer ──▶ WebSocket ──▶ PTY ──▶ tmux ──▶ Claude Code
```

The console was a page of numbers: it said a runtime was up without anything on
it being up. This is the terminal itself, on the card, live — and read-only. A
person can watch a project's Claude work from the console without walking to the
workspace, and cannot type at it from there.

## 1. What a viewer is

A **viewer** is the protocol's default position rather than a restriction added
to a component. A client that never sends `control.request` is a viewer for the
life of its connection (`docs/MULTI_DEVICE.md` §5): it receives output, and the
server refuses anything it sends that would change the terminal.

The console never sends `control.request`. There is no call in
`TerminalViewer.tsx` that could, which is why §7 of the phase brief — "the
terminal must be read-only, no `terminal.onData()`" — is satisfied structurally
rather than by a flag.

```text
web/src/dashboard/
  TerminalViewer.tsx        one project's terminal, watched and never typed into
  ProjectPanel.tsx          the card: runtime, agent, screen, attention
```

`TerminalViewer` renders the workspace's own `components/TerminalView` with three
props stated and never varied: `interactive={false}`, `mayResize={false}`,
`showKeys={false}`. It is the same component the workspace draws, the same xterm
instance type, and the same session hook — which is the point. A console with its
own terminal implementation would be two systems that drift, and
`docs/CONTROLLER_UI.md` §8 said so before this phase existed.

## 2. Websocket flow

There is no console WebSocket, and no new endpoint. `internal/terminal` already
serves one socket at `/api/ws` speaking `agentmux.terminal.v2`, and a console
subscription is an ordinary subscription on it.

```text
App
 └─ createTerminalClient()          once per document
     └─ TerminalProvider           one client, every terminal in the page
         ├─ DashboardPage ──▶ ProjectPanel ──▶ TerminalViewer ──▶ useTerminalSession(projectId)
         └─ WorkspaceApp  ──▶ ProjectPanel ──▶ TerminalView    ──▶ useTerminalSession(projectId)
```

The client is created in `App` rather than in either page, and this is the
load-bearing decision of the phase. Both pages have terminals in them now; a
client created per page would be a second connection to the same server from the
same tab. The socket is opened by the first subscribe and closed shortly after
the last release, so a page with no terminal in it costs nothing by holding one —
which is what makes a single client the cheaper answer as well as the correct
one.

Subscriptions are multiplexed on that socket, up to `MaxSubscriptions = 16`
(`internal/terminal/protocol.go`), and every frame carries the project it
belongs to. **Seven cards are one WebSocket.** The browser suite asserts the
count rather than the intention (`web/e2e/suites/dashboard-terminal.mjs`).

Client identity lives in `sessionStorage` under `agentmux.clientId`, so a reload
is the same client reconnecting and a second tab is a different one. That is what
makes §17's "refresh the page, reconnect successfully" a reconnection rather than
a new viewer arriving.

## 3. Readonly design

Four independent things would have to fail before a keystroke could reach the
pty. None of them is a check on the others; they are four different mechanisms
at four different layers.

| # | Where | What stops it |
|---|---|---|
| 1 | `TerminalViewer` | passes `interactive={false}`, so `TerminalView` never registers `term.onData`. No handler is bound, so a keystroke has nothing to reach. |
| 2 | `TerminalViewer` | passes `showKeys={false}`, so the touch keys — each of which calls `session.input` directly rather than through `onData` — are not rendered at all. |
| 3 | `useTerminalSession` | `input()` returns early unless the client holds the lease (`controlRef.current.held`), and a viewer never asks for one. |
| 4 | `internal/terminal` | a client without the lease is refused with `CodeNotController` (`conn.go`). The bytes are refused, and the refusal is logged without them. |

Layers 1 and 2 are why `TerminalView` grew a `showKeys` prop in this phase. The
alternative — rendering the keys and disabling them — is decoration made of
controls: a row of buttons that can never do anything, on a card where the space
belongs to the terminal.

Layer 4 is the one that holds when the first three are wrong, and it is the one
that was already there. This phase added no server-side rule.

**No credential is on the page.** The console renders the fields
`docs/CONTROLLER_API.md` names and nothing else — no prompt, no transcript, no
tool input, no session id. A terminal draws what tmux draws, which is what a
person sitting at the machine would see; it is not a channel the server writes
secrets into.

## 4. Multi viewer

Several browsers may watch one terminal, and they share the output because they
share the pty. There is one tmux session, one Claude, one stream of bytes — and
each viewer is a reader of it.

What is **not** shared is anything about the viewer:

- **scroll position** never crosses the wire. Scrolling a terminal is an xterm
  local operation against a local scrollback buffer, so one person scrolling up
  to read cannot move anybody else's screen.
- **viewport size** is each viewer's own, and is not sent (see §5).
- **client identity** is per tab, so the server can tell two consoles apart for
  the control lease even though a console never takes one.

Two consoles on one project therefore see the same bytes and can be read
independently. The jsdom suite covers the mechanism
(`web/src/dashboard/TerminalViewer.test.tsx` — two viewers, two subscriptions,
two terminals, one client) and the browser suite covers the fact: a marker typed
into the pty from outside the browser appears on both consoles.

## 5. Resize limitation — recorded, not worked around

**A viewer's size never reaches the pty.** The terminal on a card draws at the
size the *server* chose, which is the size the controller's window produced, and
the card is sized around it. A console at 980px wide and a console at 1440px wide
draw the identical 80×24 grid, scaled as the glyphs allow.

This is the intended answer and not a shortfall: a terminal has one pty and
therefore one shape. If every viewer's window could set it, a phone rotating
would reshape Claude's output for everybody, and the last window to resize would
win.

Three things make it true rather than hoped for:

1. `TerminalViewer` passes `mayResize={false}` to `TerminalView`, so
   `fitAddon.fit()` is never called — the size is never even measured;
2. `useTerminalSession.resize()` returns early without the lease, so nothing is
   sent;
3. the server refuses a viewer's `resize` with `CodeNotController` rather than
   ignoring it (`conn.go`) — a client that asked for a shape and was not given it
   is told, rather than left believing it has it.

**The limitation worth recording** is what a viewer sees when the pty is shaped
for somebody else's window. A console card on a 1440px desktop showing an 80×24
terminal that a 900px-wide controller produced will letterbox or clip depending
on which is larger; the terminal is drawn at its own grid and the card does not
stretch it to fit. Controller-driven resize — a console able to *ask* for a
geometry — is not implemented, and belongs to a phase where a viewer has an
identity worth giving authority to.

## 6. Lifecycle and failure states

The viewer's lifecycle is the card's: mounting subscribes, unmounting releases.
Nothing else opens or closes anything.

| What happened | What the card says |
|---|---|
| the runtime is down | **Runtime stopped** — and no terminal is created at all |
| the socket is coming up | **Connecting…**, for the first 400ms |
| the socket dropped, and is retrying | **Terminal disconnected** |
| the connection gave up | **Unable to connect terminal**, with the reason and a **Retry** |
| the server ended the subscription | **No active terminal** |

**A stopped runtime has no terminal in it**, and this is deliberate on two
counts. The server refuses a subscribe to a stopped project, so a client that
tried would spend a reconnect cycle being told; and a console showing a hundred
stopped projects should not have a hundred xterm instances in the document. The
`running` value comes from the same field the Runtime row above it renders, so a
card cannot say the runtime is stopped and show a live terminal at the same time.

**Retry is offered only where it can help.** It calls `resync`, which
re-establishes a subscription from a fresh snapshot — a connection problem. It is
not offered for "No active terminal", because there is nothing to retry until a
runtime starts, and a button that cannot help is worse than none.

**Nothing here creates a runtime.** §9 of the phase brief forbids it and there is
no call that could: the viewer subscribes to what exists.

## 7. Cost, and why it does not compound

Six projects on the console are six subscriptions on one socket and six xterm
instances. Three things keep that bounded:

- one WebSocket for the page, regardless of card count;
- no xterm instance for a project whose runtime is stopped;
- the reconnect policy is the one the workspace already had, unchanged: delays
  growing from 250ms to a 10s cap, jittered so tabs opened together do not come
  back in step, and a client that has *never* connected giving up after five
  attempts rather than retrying forever at a page nobody can read
  (`web/src/terminal/backoff.ts`). Because this is the same client the workspace
  uses, a console adds no second policy — and §13 of the phase brief's "no
  infinite reconnecting" is that cap plus that give-up rule, not a new one.

The dashboard's own read is a five-second poll of `GET /api/controller`, not a
second socket — `docs/CONTROLLER_UI.md` §3 has the reasoning.

## 8. Tests

| Where | What it holds |
|---|---|
| `web/src/dashboard/TerminalViewer.test.tsx` | mount and subscribe; output drawn; **no input handler bound**; **nothing sent when a key is pressed anyway**; never requests control; never resizes; each failure state; two viewers at once; a stopped project holding no terminal while its neighbour does |
| `web/src/components/TerminalView.test.tsx` | the workspace's own behaviour, including that `showKeys` defaults to drawing them — the workspace is unchanged by this phase |
| `web/src/test/xterm.ts` | the double counts its input handlers (`dataListenerCount`), which is how the read-only claim is asserted rather than assumed |
| `web/e2e/suites/dashboard-terminal.mjs` | the terminal on a card being the real one: a marker typed into the pty from outside the browser appears on the card; a stopped runtime removes it and a start brings it back; one socket for seven cards; nothing typed at it reaches the pty and no `input` frame is sent; a reshaped window leaves the pty where it was; a reload reconnects and shows what happened meanwhile; two consoles watch one terminal |

## 9. What is not here

Each is a later phase's subject rather than an omission:

- **input.** No keyboard, no prompt bar, no permission action on the console;
- **controller authority.** A console never asks for the lease;
- **controller-driven resize.** A viewer cannot ask for a geometry (§5);
- **starting and stopping runtimes from the console.** The console shows; the
  workspace and the API operate. The browser suite starts and stops a runtime
  through the API for exactly this reason;
- **`No active terminal` on the workspace's panel.** That state belongs to the
  workspace's own lifecycle and is unchanged;
- **a mobile app.** The console is the same page at a narrower width.
