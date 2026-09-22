# The Terminal Viewer

The console's card, with the terminal on it.

```text
Dashboard ──▶ Project Panel ──▶ Terminal Viewer ──▶ WebSocket ──▶ PTY ──▶ tmux ──▶ Claude Code
```

The console was a page of numbers: it said a runtime was up without anything on
it being up. This is the terminal itself, on the card, live. A person can watch a
project's Claude work from the console without walking to the workspace — and,
from Phase 7.4B-2B, can type at it from there too, once the card has been given
that project's control lease.

## 1. What a viewer is

A **viewer** is the protocol's default position rather than a restriction added
to a component. A client that never sends `control.request` is a viewer for the
life of its connection (`docs/MULTI_DEVICE.md` §5): it receives output, and the
server refuses anything it sends that would change the terminal.

**A console starts as a viewer, and can stop being one.** From Phase 7.4B-2B a
card can ask for the project's lease, and the client holding it is the one client
the server will accept input from. The claim is therefore no longer "the console
cannot type" — it is "the console does not type until it has been given the
keyboard". `docs/TERMINAL_CONTROLLER.md` is the whole of that design: the lease,
the four gates a keystroke has to pass, and what happens when two consoles want
one terminal.

What has not changed is the structure. A card does not claim anything by being
opened: nothing in `TerminalViewer` runs on mount that could send
`control.request`, and `web/e2e/suites/dashboard-terminal.mjs` asserts the
absence of that frame over six cards at once. Opening a terminal is not claiming
it.

```text
web/src/dashboard/
  TerminalViewer.tsx        one project's terminal: watched, typed into only when given the lease
  TerminalControl.tsx       the card's control bar — the badge, Request/Release, the queue
  ProjectPanel.tsx          the card: runtime, agent, screen, attention
```

`TerminalViewer` renders the workspace's own `components/TerminalView`. Two of
its props are now derived rather than constant, and one is not:

| Prop | Value | Why |
|---|---|---|
| `interactive` | `connected && held` | a card types only as the controller |
| `showKeys` | `held` | the touch keys call `session.input` directly, so they follow the same flag — and a card cannot afford a row of buttons that can never do anything |
| `mayResize` | `false`, always | §5: the console types and never reshapes |

It is the same component the workspace draws, the same xterm instance type, and
the same session hook — which is the point. A console with its own terminal
implementation would be two systems that drift, and `docs/CONTROLLER_UI.md` §8
said so before this phase existed. The control bar is new, and it is drawn from
the vocabulary in `components/ProjectTerminal.tsx` rather than from a second copy
of it: the badge's text, its tone, the button's label and the refusal sentence
are the same functions both pages call.

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

## 3. What a viewer cannot do

The four independent things that would have to fail before a keystroke could
reach the pty are the four gates of `docs/TERMINAL_CONTROLLER.md` §4. They are
the same four for a console and for a workspace panel, because the question is
about the *client* rather than about the page: what that document calls "a client
that is not the lease holder" is what this one calls a viewer.

What is particular to the console is the first two, and both are derived from
the lease rather than fixed:

| # | Where | What stops a console that is not the controller |
|---|---|---|
| 1 | `TerminalViewer` | passes `interactive={held}`, so `TerminalView` registers no `term.onData` while the card is a viewer. A keystroke has nothing to reach rather than a handler that declines it. |
| 2 | `TerminalViewer` | passes `showKeys={held}`, so the touch keys — each of which calls `session.input` directly rather than through `onData` — are not rendered for a viewer. |
| 3 | `useTerminalSession` | `input()` returns early unless the client holds the lease. The resize path is closed separately and independently, by `mayResize: false` (§5). |
| 4 | `internal/terminal` | a client without the lease is refused with `CodeNotController`, judged against the connection's own identity rather than against anything the frame says (`docs/TERMINAL_CONTROLLER.md` §5). |

Gate 2 is why `TerminalView` grew a `showKeys` prop in Phase 7.4B-2A. The
alternative — rendering the keys and disabling them — is decoration made of
controls: a row of buttons that can never do anything, on a card where the space
belongs to the terminal.

Gate 4 is the one that holds when the first three are wrong, and the one a client
cannot influence at all. `web/e2e/suites/controller-input.mjs` proves it by
sending an input frame past the console entirely, from the socket the console is
already holding.

**A refusal is a refusal, not a fault.** When the server does refuse something
this card sent, it answers with an error and leaves the connection open — one
refused keystroke is not an outage for everybody else watching. The card says the
server's sentence in the corner of a terminal it keeps drawing, which is what the
workspace has always done; it does not replace itself with "Unable to connect
terminal", because the connection is up and the terminal is live.

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
- **client identity** is per tab, so the server can tell two consoles apart. This
  is what makes the lease possible at all: `docs/TERMINAL_CONTROLLER.md` §6 has
  the two-client walkthrough, where one client asks, is given the terminal, and
  the other is told whose it is — by name, from the device label the server
  derived rather than one the client claimed.

Two consoles on one project therefore see the same bytes and can be read
independently, and exactly one of them at a time may type. The jsdom suite covers
the mechanism (`web/src/dashboard/TerminalViewer.test.tsx` — two viewers, two
subscriptions, two terminals, one client) and the browser suite covers the fact:
a marker typed into the pty from outside the browser appears on both consoles.

## 5. Resize authority

**A console never reshapes the pty.** The terminal on a card draws at the size
the *server* sent, which is the size the last client permitted to resize asked
for, and the card is sized around it. A console at 980px wide and a console at
1440px wide draw the identical grid, scaled as the glyphs allow.

This is the intended answer and not a shortfall: a terminal has one pty and
therefore one shape. If every viewer's window could set it, a phone rotating
would reshape the output for everybody, and the last window to resize would win.

Three things make it true rather than hoped for:

1. `TerminalViewer` passes `mayResize={false}` to `TerminalView`, so
   `fitAddon.fit()` is never called — the size is never even measured;
2. `useTerminalSession.resize()` returns early under `mayResize: false`, and
   `setSize` — the path a subscribe's geometry takes — drops the value rather
   than sending it;
3. the server refuses a client without the lease with `CodeNotController` rather
   than ignoring it (`conn.go`) — a client that asked for a shape and was not
   given it is told, rather than left believing it has it.

Gate 2 is deliberately at the session rather than the view. A size reaches the
pty down two roads, not one: a `resize` frame, and the geometry a `subscribe`
carries, which the server applies for a client that may resize. Suppressing only
the first would leave the console able to reshape the terminal by subscribing to
it, so the rule is stated once, where both roads start.

Being the controller does not change this. **The console types and never
reshapes** — the two authorities are separate on the server (`MayInput` and
`MayResize` are separate methods), separate in the session (`interactive` follows
the lease; `mayResize` is fixed), and separate in the browser suite, which takes
control on a console and then asserts the pty is still `120x30` and that no
`resize` message left the page.

**The limitation worth recording** is what a viewer sees when the pty is shaped
for somebody else's window. A console card on a 1440px desktop showing a grid
that a 900px-wide controller produced will letterbox or clip depending on which
is larger; the terminal is drawn at its own grid and the card does not stretch it
to fit. The workspace's controller can ask for a geometry, because that is where
the person actually working sits; a console cannot, and giving it the ability
would mean a tablet in a pocket could reflow a terminal nobody at the tablet is
looking at.

## 6. Lifecycle and failure states

The viewer's lifecycle is the card's: mounting subscribes, unmounting releases.
Nothing else opens or closes anything.

| What happened | What the card says |
|---|---|
| the runtime is down | **Runtime stopped** — and no terminal is created at all |
| the socket is coming up | **Connecting…**, for the first 400ms |
| the socket dropped, and is retrying | **Terminal disconnected** |
| the connection gave up | **Unable to connect terminal**, with the reason and a **Retry** |
| the server ended the subscription | **No active terminal**, and a **Release control** if this client holds the lease |
| the server refused one message | the server's sentence, in the corner of the terminal it keeps drawing |

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

**"No active terminal" is the one ending that offers a way out of a lease.** The
connection is still up, the terminal went away underneath it, and the server
therefore has no reason to take the lease back: it is held by a client that is
present, is not suspended and will not expire. Without a control on screen, a
person who was typing would be holding a lease on a terminal that does not exist
and unable to shed it. The button appears only for the client that holds it —
`session.control.held` — so a viewer is not offered a control it has no standing
to use.

**A refusal is not a failure** (the last row, and the reason it is a row rather
than folded into the fourth). An error frame refuses *one message*; the socket
behind it stays open, and the terminal is live. The card says the server's
sentence in its corner and keeps drawing — which is what the workspace has always
done, and what `TerminalViewer.test.tsx` now pins in both directions: a refusal
with a connection keeps the terminal, and an error arriving with no connection
still gives way to "Unable to connect terminal".

**Nothing here creates a runtime.** §9 of the 7.4B-2A brief forbids it and there
is no call that could: the viewer subscribes to what exists.

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
| `web/src/dashboard/TerminalViewer.test.tsx` | mount and subscribe; output drawn; **no input handler bound while a viewer**; **nothing sent when a key is pressed anyway**; no `control.request` on mount; never resizes; each failure state; **a refusal keeping a live terminal** and an error with no connection not keeping one; two viewers at once; a stopped project holding no terminal while its neighbour does |
| `web/src/components/TerminalView.test.tsx` | the workspace's own behaviour, including that `showKeys` defaults to drawing them — the workspace is unchanged by this phase |
| `web/src/test/xterm.ts` | the double counts its input handlers (`dataListenerCount`), which is how the read-only claim is asserted rather than assumed |
| `web/e2e/suites/dashboard-terminal.mjs` | the terminal on a card being the real one: a marker typed into the pty from outside the browser appears on the card; a stopped runtime removes it and a start brings it back; one socket for seven cards; nothing typed at it reaches the pty and no `input` frame is sent; a reshaped window leaves the pty where it was; a reload reconnects and shows what happened meanwhile; two consoles watch one terminal |
| `web/e2e/suites/controller-input.mjs` | the console as a controller: two browsers on one card, one taking the lease and typing a marker the pty receives, the other's keystrokes and forged input frames both refused, the pty unreshaped, the lease released and taken by the second console |

**The console's own typing is proven against a real pty.** The jsdom suite can
show that `interactive` is false while the card is a viewer; only a browser with
a real tmux session behind it can show that a keystroke which *is* permitted
arrives as bytes. `controller-input.mjs` types at the console and then reads the
marker back out of the pty, which is the end of the chain the unit tests can only
approximate from the near end.

## 9. What is not here

Each is a later phase's subject rather than an omission:

- **controller-driven resize from a console.** A console types and does not
  reshape, whether or not it holds the lease (§5). The workspace's controller can
  ask for a geometry, and that is unchanged;
- **starting and stopping runtimes from the console.** The console shows and
  types; the workspace and the API operate. The browser suite starts and stops a
  runtime through the API for exactly this reason;
- **`No active terminal` on the workspace's panel.** That state belongs to the
  workspace's own lifecycle and is unchanged;
- **a mobile app.** The console is the same page at a narrower width. The iPad
  viewport in `controller-input.mjs` is a stated device profile on a second
  browser context, not an application.
