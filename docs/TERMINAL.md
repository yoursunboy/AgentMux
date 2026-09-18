# AgentMux Terminal Transport

This document describes how a project's terminal reaches a browser: the `/api/ws` endpoint, the
messages that travel over it, what a snapshot does and does not carry, and the limits that keep an
untrusted client from costing the server more than it should.

It is the reference for the wire. `docs/RUNTIME.md` describes what is behind it — tmux, the one
control connection per runtime, and the runtime manager — and `docs/ARCHITECTURE.md` §9–§12 describes
where this layer sits and why.

## 1. What this layer is

```text
tmux (one server, one socket, one session per project)
  ↓  one control connection, owned by the runtime manager
RuntimeManager  (sequence numbers, bounded history, one watcher per subscriber)
  ↓  subscriptions
terminal.Hub  (this package: frames, limits, one socket per browser)
  ↓  WebSocket, binary frames for output, text frames for control
browser  (xterm.js)
```

The vertical direction is the point. **This layer may only subscribe to the runtime manager.** It
has no tmux socket path, no session name, and no way to acquire either: every question about a
project's terminal is answered by `internal/session`, and this package asks it rather than working it
out. A transport that opened its own `tmux -C` connection would be a second path to the same
terminal, and two paths to one terminal is how a server ends up with a control connection per browser
tab, a pane that resizes when somebody closes a window, and output that arrives twice or not at all
depending on which connection won.

The horizontal consequence is what makes the rest of this document make sense: a browser connecting
or disconnecting is a subscriber arriving or leaving, and it does not touch the control connection.
Closing every browser leaves the runtime running, the Claude process alive, and the terminal
unchanged. That is checked rather than asserted — see §12.

## 2. The endpoint

```text
GET /api/ws?v=2&client=c_7f3a91b2
Sec-WebSocket-Protocol: agentmux.terminal.v2
```

There is exactly one real-time endpoint, and it is one socket per browser rather than one per
terminal. A browser watching two projects subscribes to both on the same socket; the frames carry
the project they belong to. A second socket would double the connection count, the ping traffic and
the failure modes for no gain.

**Version.** `v` states the protocol version the client speaks. Omitted is allowed, because a client
that does not know to send it is a client this version can still serve. Present and wrong is not:
the request is refused with `400` rather than upgraded, because a client that states a version it
does not have is telling the server it will mis-draw what it is sent. The negotiated subprotocol
carries the same number so that a proxy's log says which protocol was agreed.

The current version is **2**, raised by Phase 6, and it is worth stating why a raise was needed for
changes that were additive. The control messages are additive and would not have needed one. What
did is that `input` and `resize` stopped being things any subscriber may send: a version 1 client
watching a terminal could type into it, and a version 2 client is a viewer until it asks. A version
1 client served by this server would find its Prompt Bar refused every time, with no Request Control
anywhere to explain why — a client drawing a state that is not true, which is exactly what this
parameter exists to catch. It is the one place where a stale cached page has to reload.

**Client.** `client` names the browser session: a tab, on a device, for as long as it is open. It is
sent here rather than in a message because it has to be known before the first one — a reconnecting
client's leases are restored the moment its socket opens — and it is stated by the client because
only the client knows which of its tabs this is. It is held in `sessionStorage`, so it survives a
reload and does not survive a new tab.

The two identifiers in the greeting below are the point of it. A *client* is a browser session; a
*connection* is one socket. One client has many connections over its life — a reload, a network
change, a server restart — and the difference between the two is what lets a reconnect be told apart
from a departure.

A client that states nothing, or states something that is not an identifier, is given one by the
server and told what it is in the greeting. Nothing is refused for this: a program on this machine
that opens the endpoint with no query string is a legitimate client, and it is a viewer like any
other until it asks. `docs/MULTI_DEVICE.md` §1 and §11 have the shape of an identifier and what it
is and is not.

**Origin.** A WebSocket handshake carries `Origin`, and a browser page on another site can attempt
one. The policy is:

| `Origin` | Result |
| --- | --- |
| absent | allowed — a program, not a page |
| same host as the request | allowed |
| listed in `server.allowedOrigins` in `config.json` | allowed |
| anything else | `403 forbidden`, and a line in the log |

A missing `Origin` is allowed deliberately: browsers always send one on a WebSocket handshake, so a
request without one comes from a program on this machine, and the cross-origin attack this check
exists to stop requires a browser. Refusing it would break every non-browser client — including this
project's own tests — without protecting anything. The default is *not* "accept everything": a page
served from a hostname that is neither this server nor on the list is refused.

**The server never opens the connection on a browser's behalf.** A client cannot name a host, a port,
a socket path, or a directory. It names a `projectId`, and the server resolves that through the
project repository to a runtime it is already running. A project the server does not know is
`bad_project`; a project it knows but has no runtime for is `project_not_found`. There is no message
in this protocol that takes a filesystem path.

## 3. The wire

Two kinds of frame, split by what they carry:

- **binary** — terminal output, and snapshots. Terminal bytes are not text; a JSON envelope around a
  screenful of escape sequences would mean base64, a third more bytes, and an encoding step on the
  hot path.
- **text** — every control message, as JSON. Control is rare, small, and worth being readable in a
  log or in a browser's frame list.

A **client never sends a binary frame.** Raw input is bytes, but it is bytes the client is sending
rather than bytes a terminal is producing, and routing it through the same typed, size-limited,
individually rejectable channel as every other client intent is worth the base64. A binary message
from a client is refused as `unsupported`.

### 3.1 Binary frame layout

```text
offset  size  field
0       1     version            (1)
1       1     type               (0x01 output, 0x02 snapshot)
2       8     firstSeq           (big-endian uint64)
10      8     lastSeq            (big-endian uint64)
18      2     idLength           (big-endian uint16)
20      n     projectId          (UTF-8, n = idLength)

— output frame: payload follows immediately (header is 20 bytes)
— snapshot frame: 2 cols, 2 rows, 1 flags, then payload (header is 25 bytes)
```

The version is stamped into every frame, in the first byte, where it costs one byte to check.
Binary frames are the part of this protocol a client cannot validate by reading: JSON that says
`"protocol": 1` is self-describing and a run of bytes is not. A client that finds a version it does
not know knows to stop rather than to draw.

The two types are different types rather than one type with a flag, because a client must not be able
to confuse them: applying a snapshot as an append duplicates it, and applying an append as a
snapshot erases everything before it.

**Flags** (snapshot only): bit 0 is the alternate screen, which is the buffer a full-screen program
draws in. A client that draws a snapshot must select the same buffer, or the program's next full
repaint lands on the wrong one.

### 3.2 The sequence pair

Every frame carries two sequence numbers, and the type says how to read them.

**Output** — `[firstSeq, lastSeq]` is the range of published chunks the payload contains. Frames do
not overlap and do not skip: the next frame's `firstSeq` is this frame's `lastSeq + 1`. The sequence
is per project and monotonic, assigned by the runtime manager.

**Snapshot** — `firstSeq == lastSeq == boundary`. The boundary is the highest sequence number the
runtime had published when the capture was requested, and its meaning is precise in one direction and
deliberately not claimed in the other:

- every chunk at or below the boundary is **already drawn** in the snapshot, so a client must not
  replay it;
- every chunk above the boundary was published **after** the capture was requested, so a client
  applies it.

It is not a claim that the snapshot is the result of applying chunks 1..n and nothing else. A chunk
published *during* the capture may already be drawn in it. See §5 for what makes that rare and §11
for why it is not eliminated.

## 4. Messages

### 4.1 Client → server

Every message is a JSON object with a `type` and, where it applies, a `projectId`. The envelope is
strict: a field that does not belong to the message's type is refused rather than ignored, so a
`cols` on a `subscribe` is an error and not a silent no-op.

| `type` | fields | meaning |
| --- | --- | --- |
| `subscribe` | `projectId`, `cols?`, `rows?` | start watching a terminal, optionally stating the size to render at |
| `unsubscribe` | `projectId` | stop watching it |
| `input` | `projectId`, `data` (base64) | raw terminal input |
| `resize` | `projectId`, `cols`, `rows` | set the canonical size |
| `resync` | `projectId` | re-establish from a fresh snapshot |
| `ping` | — | application-level liveness |
| `control.request` | `projectId` | ask for the lease |
| `control.release` | `projectId` | give it up |
| `control.accept` | `projectId`, `clientId` | answer a request by handing it over |
| `control.reject` | `projectId`, `clientId` | answer a request by refusing it |

The last four are Phase 6's, documented in `docs/MULTI_DEVICE.md` §12. Two things about them belong
in this table. They are the only client messages whose *acceptance* depends on something other than
the connection's own state — schema, subscription, size — and that something is who holds a lease,
which is a fact about other clients. And `input` and `resize`, which were unconditional in version 1,
are now in that same position: see §4.3.

There is no `control.withdraw`. A request that has not been answered is taken back by
`unsubscribe`, which is what a person closing the tab or moving to another project does; a second
message for it would be a second way to say the same thing, with a second chance for the two to
disagree.

`subscribe` carries a size because a browser knows how large its viewport is before it knows anything
else, and stating it up front means the runtime is resized once, before the snapshot is taken, rather
than the client drawing a screen at the old size and then reflowing.

`resync` carries no reason field, and that is a decision rather than an omission. A reason would be
free text from a browser, and free text from a browser that reaches a log is a place terminal output —
or a prompt — can end up somewhere nobody meant it to be. The server learns everything it needs from
the sequence number the subscription stopped at, and it knows that already.

`ping` is not the WebSocket ping. This one proves the client's own message loop is running and is
answered by the server's, which is the pair a stalled tab breaks.

### 4.2 Server → client

| `type` | fields | meaning |
| --- | --- | --- |
| `hello` | `protocol`, `clientId`, `connectionId`, `device`, `server`, `version` | first message on every connection |
| `unsubscribed` | `projectId` | the subscription has ended |
| `resized` | `projectId`, `cols`, `rows` | the canonical size changed |
| `error` | `code`, `message`, `projectId?`, `about?` | a client message failed |
| `pong` | — | answers `ping` |
| `control.granted` | `projectId`, `control` | this client now holds the lease |
| `control.denied` | `projectId`, `control`, `reason` | this client's request was refused |
| `control.revoked` | `projectId`, `control` | this client no longer holds it |
| `control.expired` | `projectId`, `control` | a suspended lease lapsed |
| `control.changed` | `projectId`, `control` | the roster changed |

`hello` is always first, and states the protocol version so a client can refuse to go further rather
than guess. `clientId` names the browser session and `connectionId` names this socket; §2 has why
they are two.

The `control.*` messages are Phase 6's and are documented in `docs/MULTI_DEVICE.md` §12, which is the
reference for them. What belongs here is the shape they share with the rest of this table: they are
text frames, they are projects-scoped, and the `control` they carry is the roster — who holds the
lease, who is waiting, and how many others are watching. It is carried by *every* message in that
family rather than sent once, so a client has exactly one thing to apply and cannot end up holding a
roster that disagrees with the message that brought it.

`resized` goes to **every** subscriber of that project, not only to the one that asked. There is one
pty, so there is one size; a second tab rendering at its own idea of the size would draw a terminal
that does not match the one the program inside is drawing for.

`error.about` names the client message type that failed, so a client with several subscriptions can
tell which one broke.

### 4.3 Error codes

These are the transport's own namespace. Runtime errors are passed through with their own codes, so a
client switching on an error can tell whether the protocol was spoken wrongly — a bug in the client —
or whether the runtime refused, which is a state of the world.

| code | meaning |
| --- | --- |
| `bad_message` | not valid protocol: malformed JSON, unknown type, missing or ill-typed field |
| `unsupported` | understood, but this server will not act on it: a binary message from a client, an unknown protocol version |
| `bad_project` | the named project is not one this server can serve |
| `project_not_found` | known project, no runtime |
| `too_many_subscriptions` | the connection is already watching as many projects as it may |
| `not_subscribed` | input or resize named a project this connection is not watching |
| `not_controller` | input or resize for a project whose lease this client does not hold — `about` says which |
| `not_pending` | a handover or a refusal named a client that has no request waiting |
| `input_too_large` | one input message carried more than `MaxInputBytes` after decoding |
| `stream_unstable` | the connection could not keep up and re-synchronising did not help (§10) |
| `internal` | the server failed in a way the client cannot act on |

`not_controller` is the one Phase 6 added to a path that already existed, and it is what makes the
keyboard a thing a client is given rather than a thing it has. It is sent to the device that earned
it and to nobody else: ten viewers typing into a terminal they do not hold produce ten refusals, not
a hundred messages. `docs/MULTI_DEVICE.md` §8 is the rule.

## 5. Snapshot and live output

A subscription is answered with a snapshot, and then with output. Neither is sufficient alone: a
snapshot without output is a screenshot, and output without a snapshot begins wherever the client
happened to arrive.

A snapshot is the visible pane, its geometry, its cursor, and which buffer it is on — rendered as
`enter-or-leave alternate screen`, `reset attributes`, `erase the visible screen`, `home the cursor`,
then the rows. That sequence is chosen so the result is independent of whatever the client was
showing before, which is what makes it usable as recovery rather than only as a first draw.

**Erasing is CSI 2J, not CSI 3J.** 2J erases the visible screen and leaves the client's scrollback
alone, which matters because scrollback belongs to each client (§7). Erasing it would let one
client's resync destroy another client's history — and its own.

**Before the capture, the runtime is given a moment to be quiet.** The boundary is exact only if
nothing is published while the pane is being read. A terminal at a prompt, or an agent waiting for a
reply, is quiet for far longer than the settle window. A build printing continuously never goes
quiet, and a client asking for a screen must not be made to wait for a build to finish, so there is
also a ceiling: past it the capture is taken anyway, and the boundary may then be conservative — a
chunk published during the capture is already drawn but above the boundary, so a client applying
everything above it will draw that chunk twice. It is a duplicate, not a loss, and it is the reason
the boundary is described as a lower bound rather than an equality.

## 6. Snapshots are not the program

A snapshot is the pane as `capture-pane` reports it. It is **not** a snapshot of the program inside.

What survives a snapshot and a resync:

- the visible screen, with colours and attributes intact;
- the cursor's position on it;
- which buffer is showing, so a full-screen program is drawn on the right one;
- the fact that the application cursor-key mode was in use — arrow keys are sent as
  `ESC [ A` either way, so a client that adopted one mode for all of them is indistinguishable from
  one that tracked it. See §11 for the honest version of that sentence.

What does not survive, because tmux does not report it through `capture-pane`:

- **the client's own scrollback** — it is local, and a resync does not clear it (§7);
- **bracketed-paste mode**, so a paste after a resync is delivered as bytes rather than as a paste
  event;
- **mouse reporting mode**, so a program that asked for mouse events receives keystrokes until it
  next enables them;
- **the alternate screen's separate saved cursor** — where the program left the primary screen's
  cursor is not carried;
- **scroll regions, and any attribute state not visible in the characters**.

The practical shape of this: a shell at a prompt, or a program that repaints fully on input, is
restored exactly. A full-screen program that keeps state it never repaints — a text editor's undo
history, a pager's position — is restored to what it looks like, not to what it knows. This is a limit
of reading a pane rather than of this implementation, and it is recorded here rather than papered
over.

## 7. Scroll

Scroll position never crosses the protocol. It is browser state and stays in the browser.

The server is never told where a client's viewport is looking, and there is no message that could
tell it. Two people watching the same terminal are looking at different lines, and a server that knew
where either of them was would have to pick one to obey.

Four consequences, all of them deliberate:

- A resync does not clear scrollback, so the history a client has already drawn stays where it was.
- Output does not move a client's viewport. A client that has scrolled up is not yanked back down;
  it is offered a way back, and a count of what has arrived since.
- A snapshot does move it, and the two rules are one rule: the viewport belongs at the end of *the
  screen*, and only new output leaves it where a reader put it. A snapshot is the screen drawn again
  rather than a line added to it, so the client that receives one puts its viewport at the bottom —
  unconditionally, including for a reader who had scrolled up, because the rows they were reading
  have just been replaced. A client that skips this is not behind by a frame: a terminal with
  scrollback of its own can take the snapshot into its screen rows with the viewport left above them,
  and it then paints the terminal's history while tmux paints its screen.
- "Follow output" is a client-side decision with no server state behind it.

## 8. Resize

`resize` sets the canonical size, which is a property of the pty rather than of a client. The server
applies it to the runtime and then tells **every** subscriber of that project the new size; each
client draws at the size it was told rather than at the size it asked for, which is what stops a
client and the server from answering each other's sizes forever.

Requests are clamped to the bounds in §9 rather than refused, and a size that does not change the
canonical one is not applied.

A client's half of "draw at the size you were told" is the part that is easy to get wrong, because a
browser terminal has an addon whose whole purpose is to resize the terminal to fill its element. Only
a client that may resize fits; a viewer draws the size it was told, and its element becomes a window
onto a screen larger than itself, which it can scroll. Fitting a viewer has the pty's rows pushed
down into the viewer's own scrollback and leaves the box showing the blank part of the screen below
them — measured as a panel with nothing on it at all. For the controller the element's size *is* the
request, so fitting is what a controller does. `docs/MULTI_DEVICE.md` §8 has the measurement. The
other half of drawing at the size you were told is being at the right place in the terminal once you
have: §7's third consequence, which is the same defect seen from the other end.

A client must not send a resize for a soft keyboard. On a tablet the on-screen keyboard shrinks the
*visual* viewport and leaves the layout viewport alone, and resizing the pty for it would make every
program inside redraw at a size nobody is looking at — repeatedly, because the keyboard animates.
"Remeasure only when the keyboard is not covering the terminal" is a client rule, and the client is
where it is implemented.

## 9. Limits

Every one of these exists because the input is a browser, which is to say untrusted code on a machine
the server does not control. A read limit that is missing is a read limit that a stuck or hostile
client can exceed until the server runs out of memory.

| constant | value | what it bounds |
| --- | --- | --- |
| `MaxClientMessageBytes` | 160 KiB | one message read from a client, before decoding |
| `MaxInputBytes` | 48 KiB | one decoded terminal input |
| `MaxSubscriptions` | 16 | projects one connection may watch at once |
| `MaxProjectIDLen` | 64 | the identifier a client may name |
| `MaxReasonLen` | 200 | a client-supplied string the server echoes or logs |
| `MinCols` / `MinRows` | 20 / 5 | the smallest pty that may be requested |
| `MaxCols` / `MaxRows` | 500 / 300 | the largest |

`MaxClientMessageBytes` has to cover the base64 of a `MaxInputBytes` payload plus the JSON around it,
with room for the largest control message. Past it the WebSocket library closes the connection rather
than buffering the excess.

`MaxInputBytes` is far more than a prompt and more than a person types. A larger paste is sent as
several messages: the terminal receives a byte stream and does not care where the boundaries fall, so
a client that chunks a paste delivers the same bytes in the same order.

`MaxSubscriptions` is deliberately above the six the multi-project grid will need and deliberately
finite. Without a cap, a client can ask for subscriptions in a loop and make the server hold one
manager watcher, one goroutine and one output queue per iteration.

The size bounds are wide enough for any real display, including a rotated tablet and a 4K monitor at
a small font. They are not decoration: a 1×1 pty makes every program inside redraw continuously, and
a 10000-column one makes tmux's grid enormous for a window that is 200 columns wide.

### Timings

| name | value | meaning |
| --- | --- | --- |
| `Write` | 15 s | one write to one client |
| `Queue` | 10 s | how long a frame may wait for a slow client before it is dropped |
| `Ping` | 30 s | how often the server checks that a client is still there |
| `Pong` | 75 s | how long it waits for the answer before closing |
| `CloseGrace` | 2 s | how long a closing connection may take to finish |
| `ResyncPause` | 500 ms | the floor between two re-synchronisations of one subscription |
| `Input` | 10 s | one input delivery to the runtime |
| `Lookup` | 10 s | resolving a `projectId` to a runtime |
| `Shutdown` | 2 s | how long the hub gives open connections to end at server shutdown |

## 10. Backpressure

The case that decides whether a stalled tablet costs the server an unbounded buffer is a client that
has stopped reading. Four things happen, in this order, and the order is the design:

1. **Frames wait, briefly.** Outbound frames go through a queue 64 deep. A frame that has not been
   written within the queue timeout is dropped, and the connection is marked congested.
2. **The runtime stops holding output for that watcher.** Each subscriber has a bounded buffer, and
   when it fills, the manager drops output for that watcher and says so in the log. Losing a
   watcher's output is the correct trade: the alternative is holding a build's output in memory
   forever on the chance that a browser comes back to read it.
3. **The subscription re-synchronises itself.** When the manager's next chunk does not follow the one
   the subscription last sent, the subscription abandons its batch, takes a fresh snapshot, and
   continues from the new boundary. A client is not asked to fix this, because it cannot: from the
   client's side, output simply continued from a screen it had already been given.
4. **If that keeps failing, the subscription is dropped** with `stream_unstable`, after
   `maxResyncs` (8) attempts. A retry loop against a runtime that is outrunning the connection is a
   loop that never ends. The project, the session and the agent are untouched: this ends a
   subscription, not a terminal.

Underneath all of that the transport's write deadline ends a connection whose client has stopped
reading altogether — a socket that never drains cannot be written to forever — and the reason is
recorded in the log.

**Because step 3 happens on the server, a browser cannot observe a sequence gap in normal
operation.** The rule that would make it ask for a resync is a second line of defence, kept because a
client should not have to trust that the server always gets this right, and it is exercised in tests
against a fabricated gap rather than in production against a real one. What can be observed live,
from the client's side, is a `resync` — a person asking for the screen again (§11).

## 11. Reconnect and resync

A client can ask for a fresh screen at any time with `resync`. It is the same path the server takes
internally in step 3 above, so there is one implementation rather than two.

A resync for a project the connection is not subscribed to is treated as a subscribe, which is the
only sensible reading of the request: a client asking for a screen it does not have is asking for the
subscription. So "ask again" from a client never dead-ends.

What a client must do with the answer:

```text
receive snapshot (firstSeq == lastSeq == boundary)
→ draw it, at the geometry it carries
→ drop any queued output at or below the boundary
→ apply output frames above it, in order
```

Reconnecting is the same flow with a new connection: `hello`, `subscribe`, snapshot, output. Nothing
about a reconnecting client reaches the runtime, and a client that reconnects to a server that has
been restarted finds the terminal where it was left, because tmux owns the pty and outlives the
server.

## 12. What a browser cannot affect

This is the criterion the whole layering exists to satisfy, and it is checked in the browser suite as
well as in the transport tests:

- closing a browser tab, or a browser crashing, leaves the session and the Claude process running;
- a browser disconnecting and reconnecting does not change the Claude process's pid and does not
  resize the pane;
- a client that never reads a byte costs the server bounded memory and then has its connection
  closed, and the session is untouched;
- reloading the page while a program is running shows the program where it is, not a restarted one.

## 13. Logging

The list of things that may be logged about a connection is short, and it is short on purpose.

A record carries four fields and no more:

- `clientId` — the browser session;
- `projectId` — the project, where the event is about one;
- the event, which is the record's message;
- the time, which the handler adds.

What is **never** logged, at any level:

- terminal output, in either direction;
- raw input payloads, including pastes;
- the contents of a prompt sent from the Prompt Bar;
- anything a program inside the terminal printed, including anything Claude Code printed.

Phase 4 also allowed byte counts and frame counts on the disconnect record. Phase 6 removed them,
along with the counters that fed them and the peer address: see `docs/MULTI_DEVICE.md` §11 for why,
and for the test that holds every record to the four fields above.

The Prompt Bar shares the input path with the keyboard, so there is no separate place for a prompt to
be recorded. This is the same rule as `docs/CLAUDE_RUNTIME.md`'s, applied to a second way into the
same terminal.

## 14. More than one terminal

Phase 5 put several of these on screen at once, and this layer did not change to
allow it: one socket per browser carries every project's terminal, and a
subscription is per project. What the workspace added is a policy about *when* to
subscribe - the page being shown, and nothing else - and `docs/WORKSPACE.md` §8 is
that policy. Everything in this document holds per subscription.

The one thing worth repeating here, because the workspace makes it easy to run
into: **subscribing is not the only way to reach a terminal, and unsubscribing is
not a way to stop one.** Leaving a page releases subscriptions and the runtime
carries on.

## 15. What this layer does not do

Named here so that the boundary is a statement rather than an omission:

- **no notion of who may type, in the transport.** Every subscription this document describes is
  equal: several clients may attach to one runtime and each receives the same bytes, and nothing on
  the snapshot, output, batching, sequencing or recovery paths consults a lease. Phase 6 added
  authority, as a separate plane in the same package that exactly two paths ask about — `input` and
  `resize` — and `docs/MULTI_DEVICE.md` is that document. Everything else here is unchanged by it,
  including the rule that a viewer receives every byte the controller does.
- **no authentication.** The endpoint is as reachable as the server is. It is bound to a host and a
  port like every other route, and putting it on a network is a decision about the deployment, not
  something this layer decides.
- **no terminal history in SQLite.** The terminal is not persisted, and neither is a snapshot. What
  SQLite holds about a runtime is metadata — see `docs/RUNTIME.md` §6.
- **no terminal content in browser storage.** A browser keeps the screen in memory and nothing else;
  nothing here is written to `localStorage`, which is a store shared with every script on the origin
  and would outlive the tab.
- **no arbitrary command API.** There is no message that runs a command. A browser may send raw
  input to a terminal it has already subscribed to, and that is all: the input goes to whatever is
  already running in the pane, exactly as typing would.
- **no provider messages.** `provider.list`, `provider.current` and friends are reserved and
  unimplemented — see `docs/PROTOCOL.md` §13.
