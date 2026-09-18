# AgentMux Multi-device Collaboration

One project runtime, several devices, one of them typing.

```text
        PC Chrome              iPad Safari            Phone
            │                       │                   │
            │ Controller            │ Viewer            │ Viewer
            │ (input, resize)       │ (watch)           │ (watch)
            └───────────┬───────────┴─────────┬─────────┘
                        │                     │
                        └──── one WebSocket ──┘
                                  per device
                                  │
                          AgentMux Server
                                  │
                          RuntimeManager
                                  │
                                tmux
                                  │
                            Claude Code
```

This document is the design Phase 6 was built from, written before the code and kept as the
reference for what the behaviour is and why. Where the build departs from it, the departure is
stated in place.

**What this is not.** There are no accounts here, no login, no roles, no permissions and no
directory of users. A client identifier names a *browsing session*, it is invented by the browser,
and it authorises nothing: anyone who can reach the server can claim any identifier, exactly as they
could before this phase. Control is a coordination mechanism between cooperating clients, not a
security boundary. What keeps a terminal private is still the network it is on — see §11.

---

## 1. Client

A **client** is one browser session: a tab, on a device, for as long as it is open.

```text
clientId     c_<16 hex characters>       invented by the browser
device       "iPad Safari"                derived by the server from User-Agent
createdAt    when the server first saw it
lastSeen     the most recent connection
```

The identifier is generated in the browser on first use and kept in `sessionStorage`, so a reload
keeps it and a second tab has its own. It is deliberately **not** a login: it is random, it is never
sent anywhere but this server, and it disappears when the tab does.

Why `sessionStorage` rather than memory. It is what makes a reload a *reconnection* rather than a
new device. A person who reloads the page while holding control is the same person, and the lease
model below is built to hand control back to a client that comes back within the grace period —
which only works if the browser can say "it is me again".

A `device` label is derived **by the server**, from the `User-Agent` header of the WebSocket
handshake, and it is the only thing another client ever sees about a device. It is not
client-supplied, because free text from a browser that reaches another person's screen is free text
from a browser; and it never reaches a log — see §11.

## 2. Connection

A **connection** is one WebSocket.

```text
connectionId   ws_000001     assigned by the server, monotonic
clientId       c_...         the browser session that opened it
device         derived from the handshake
```

**Client ≠ Connection.** One client has had many connections: a reload, a network change, a server
restart, a tablet waking from sleep. A client may briefly have two — the overlap between the new
socket opening and the old one being reaped — and the model has to survive that without deciding
that the person left.

This distinction is the whole reason leases are held by *clients* and presence is tracked by
*connections*. A lease that belonged to a connection would be released every time a phone changed
from wifi to cellular, which is the exact case the grace period exists for.

## 3. Viewer

What every client is when it connects, and what most clients stay.

**A viewer may:**

- receive terminal output for a project it has subscribed to;
- receive snapshots, and ask for a fresh one;
- see the project's runtime status;
- see who the controller is, on which device, and how many others are watching;
- ask for control.

**A viewer may not:**

- send terminal input;
- resize the terminal;
- grant, refuse or transfer control.

A viewer's `input` and `resize` messages are refused by the server with `not_controller`. The client
also does not send them — the Prompt Bar is disabled and the keyboard is not wired through — but
that is convenience, not authority. The server is the only thing that decides, and it decides the
same way whatever the client believes.

## 4. Controller

**A controller is a viewer that additionally holds the lease for one project.**

**A controller may:**

- everything a viewer may;
- send terminal input;
- resize the terminal;
- accept or refuse a request for control from another client;
- release control.

**One project has at most one controller.** Not per connection, not per device: per project. Two
clients cannot both be the controller of one terminal, because there is one pty and one cursor and
two sources of keystrokes interleaving is not a feature anybody can use.

Across projects a client may hold as many leases as it likes. Control of project A says nothing
about project B, and acquiring one never affects the other — see §10.

## 5. Viewer by default

**A client that opens a project is a viewer.** It is never made the controller automatically.

The reason is the shape of the product. Somebody has AgentMux open on a desktop and running Claude
in four projects; they open the same workspace on a phone to watch a build. A design that granted
control to whoever arrived last would take the keyboard away from the person at the desk every time
somebody glanced at their phone — and it would do it silently, in the middle of a sentence.

So control is asked for, and the first asker of an unclaimed project gets it. That is the only case
that is granted without a human answering.

**What this costs every other suite.** The browser suites written before this phase assume that a page
which opens a project can type into it, and that is no longer true of anything. Each of them asks
first — a click where there is a mouse, a tap where the context has touch — through one helper,
`takeControl` in `web/e2e/lib/harness.mjs`. Two consequences are worth knowing before reading a
failure in one of them. A second tab of one browser is a second client and is therefore a viewer even
on the machine that is typing, so a suite that wants two tabs to both type has the first one give the
lease up (`releaseControl`). And a server restart takes every lease with it, so anything that types
after one asks again — which is the behaviour rather than a workaround, and is asserted on its own in
`recovery.mjs`. The third consequence is the one that looked like a broken product rather than a
stale test, and it is in §8: a panel nobody has taken control of draws the terminal at the terminal's
size and not at its own, so a grid of five viewer panels holds five 120×30 terminals in five boxes
that fit rather less than that. A suite that wants a panel to be the shape of its own box — which is
every suite that measures geometry — has to take control of it first, and `workspace.mjs` says so
where it does it.

## 6. Lease

Control is a **lease**, not a permission and not a property of a project.

```text
projectId              which terminal this is about
controllerClientId     the browser session that holds it, or null
controllerConnectionId the connection it was granted on
device                 the label shown to everyone else
grantedAt              when it was granted
expiresAt              when a suspended lease lapses, or null
suspended              true while the controller is disconnected
```

### There is no idle timeout while connected

`expiresAt` is set **only** when the lease is suspended, and it means "if the controller has not come
back by this moment, the lease is released". A connected controller holds its lease indefinitely,
including while typing nothing for an hour.

This is a decision and it is worth stating, because a timeout looks like the safe choice. A person
watching a long build is not idle in any sense that matters, and a lease that lapsed while they read
the output would take the keyboard away between the moment they decide to type and the moment the
keystroke arrives. The failure a timeout would prevent — a controller whose connection is a black
hole, holding a terminal nobody can use — is already covered by the transport: the server pings
every 30 seconds and a connection that has not answered in 75 is reaped and its client suspended.
A second timer would duplicate that one and be wrong more often.

**That reaping is slower than a browser's own, and the difference is worth knowing.** The browser
gives up on its own server after 40 seconds — a ping every 30, a pong expected within 10 — so a
client whose network went away decides the connection is dead well before the server does. What the
server cannot do is notice a socket that is merely *silent*: there is no write outstanding, TCP has
not failed, and the connection stays open at both ends until either the transport reaps it or the
client closes it. A tablet in a tunnel is therefore held for up to about a minute and a half before
anybody is told it is away, and that is the honest cost of not inventing a second timer. It is also
why the browser suite severs the connection rather than merely taking the network away — see
`web/e2e/lib/harness.mjs`, `severConnection`.

The grace period itself is `terminal.DefaultControlGrace`: thirty seconds, chosen because the
directive gives thirty as its example and because ten does not survive a tunnel. A deployment that
wants a different number sets `server.controlGraceSeconds` in `config.json`; zero or absent means the
default, and the E2E fixture states eight so that a run does not sleep half a minute to watch a lease
lapse.

### Where the lease lives

**In memory, in the server, and nowhere else.** Nothing about a lease is written to SQLite. See §9.

## 7. Lifecycle

### Acquire

```text
viewer sends   control.request { projectId }

  no controller          → granted.  the requester is the controller
  requester is controller→ granted.  idempotent; asking twice is not an error
  controller is suspended→ denied.   controller_exists, and the roster says suspended
  another controller     → queued.   the requester is NOT granted yet; it waits
```

**Never pre-empted.** A request against a live controller does not take control from it. It joins a
queue the controller can see, and the controller decides.

**There is no `control.requested` message.** Being queued is a change to the roster, and the roster is
broadcast to everybody watching — including the controller, who is the one person who needs to know
that somebody is asking. A separate message saying the same thing would be a second source of truth
for it, and the two could disagree.

The queue is visible as `pending` in the roster, and it is bounded: past `MaxPendingRequests` a
request is refused with `too_many_requests` rather than accepted into a queue nobody will get to.

### Accept and refuse

```text
controller sends  control.accept { projectId, clientId }
   → that client becomes the controller; the old controller becomes a viewer
   → everyone subscribed is sent the new roster
   → the queue is cleared, whoever was in it

controller sends  control.reject { projectId, clientId }
   → that client is told control.denied (reason: rejected) and leaves the queue
```

An `accept` naming a client that has not asked is refused with `not_pending`. A transfer is a
handshake between two clients that both know about it, not a way to push control at somebody: a
client handed a keyboard it did not ask for is one whose next keystroke goes somewhere it did not
expect.

### Release

```text
controller sends  control.release { projectId }
   → the lease is cleared. Nobody is the controller.
   → the queue is cleared with it. Nobody is promoted.
```

Releasing is not the same as disconnecting: it is deliberate, it takes effect at once, and there is
no grace period, because the client that released is still there to be told so.

**Nobody waiting is promoted.** Stepping back is not the same as choosing a successor — that is what
`accept` is — and promoting the first in line would make releasing a way to hand somebody the
keyboard without saying so. The queue goes with the lease because a pending request is a question put
to whoever is holding it, and this leaves nobody holding it. The project is free, and the clients
that were waiting see that in the roster and may ask again.

Releasing does **not** require a subscription, unlike asking. In the workspace a panel is unmounted
whenever its page is not the current one, so a controller that paged away is no longer watching the
project and still holds the lease — and letting go is the one thing it must be able to do from there.

### Disconnect

**A controller's connection dropping does not release the lease.**

```text
controller's last connection closes
        │
        ▼
   lease suspended            expiresAt = now + grace (30s)
        │                     everyone is told the roster changed
        │
        ├── the client reconnects within the grace
        │        │
        │        ▼
        │   lease resumed     no new grant, no handshake, control simply continues
        │
        └── the grace elapses
                 │
                 ▼
            lease released    control.expired, broadcast to the project
```

Thirty seconds is long enough for a phone to change network, a laptop to be reopened, or a page to
be reloaded, and short enough that somebody else can take over without going to find a person.
It is configurable (`-control-grace`) so that a test does not have to wait half a minute to watch
it happen.

**A suspended lease still blocks a request.** During the grace period a viewer asking for control is
told `controller_exists`, with the roster showing the controller as away. The grace period exists
because the controller is probably coming back; letting the first asker take it would make the
grace period mean nothing. When it elapses, the next request succeeds.

Resuming is by `clientId`, not by connection. Any connection presenting the suspended client's
identifier resumes the lease, which is what makes a reload restore control rather than requiring a
second click.

### Server restart

A restart loses every connection and therefore every lease. It does **not** touch a runtime:
sessions, tmux and Claude all keep running, exactly as in every phase before this one.

When the server comes back, every client is a viewer. Control is re-negotiated rather than restored:
a client that held control asks again, and is granted if the lease is still free. That is ordinarily
instant — the usual case is one person with one browser — and it is safe in the case that is not,
because the server never decides on its own that somebody should have control.

Persisting the controller across a restart was considered and rejected. A lease written to disk is a
lease that outlives the process that granted it, and a controller that was a phone which has since
been closed would hold a terminal that nobody can type into and no one can revoke without a second
mechanism to revoke it.

## 8. Authority

Input and resize are **separate authorities** even though one lease answers both.

```go
// Two questions, one lease.
func (a *Authority) MayInput(projectID, clientID string) Decision
func (a *Authority) MayResize(projectID, clientID string) Decision
```

They are separate because they are going to stop being the same question. A large screen showing a
terminal and a phone typing into it is a configuration people will ask for, and a design where
`canType == canResize` cannot express it — the phone would resize the pty to 40 columns and reflow
the desktop's view. Splitting the two methods now costs one function each and means the later change
is a policy edit rather than a rewrite of every call site.

Every terminal input and every terminal resize passes through the authority. The WebSocket handler
does not call `Runtime.Input` or `Runtime.Resize` directly:

```text
browser → WebSocket → Authority.MayInput → RuntimeManager → tmux
```

This is enforced structurally: the hub's only route to `Input` and `Resize` is through methods that
consult the authority first, and there is no second path. A handler that could reach the runtime
around the gate would make the gate advisory.

### Resize

The controller's resize sets the canonical pty size, exactly as in Phase 4 — one pty, one size, and
every subscriber is told the size that was applied rather than the size they asked for.

A viewer's resize is refused with `not_controller`. It is **not** silently clamped or ignored: a
client that asked for something and was not given it should be told, and the alternative is a
terminal that never matches the shape the client thinks it has.

`resize_denied` is expressed as `error { code: "not_controller", about: "resize" }` rather than as a
new top-level message type. The protocol has one error channel with stable codes, the client already
handles it, and a second way for a request to fail would mean every client-side error path had two
shapes to consider.

### The subscribe that carries a size

A `subscribe` carries `cols` and `rows`, because a browser knows how large its viewport is, and the
size is applied **before** the snapshot is taken so that the client draws the terminal the right
shape once instead of drawing an old shape and reflowing it. That path asks the same question —
`MayResize` — and so it is applied only for the controller.

It is the one place the rule is easy to miss, because it does not look like a resize. It is also the
one that would matter most: a phone opening the project somebody at a desk is working in would
reflow that terminal to its own width merely by looking at it, and the person typing would watch
their screen rearrange itself for no reason they could see. A viewer is sent the screen as it is, and
that the screen is not the shape of its own box is exactly what it means to be watching somebody
else's terminal.

### The other half of it, which is in the browser

The rule above is only true if the view obeys it, and the view has an addon whose whole job is the
opposite. `FitAddon` measures the element and resizes the terminal to fill it, which is right for the
browser that owns the pty's shape and wrong for the one that does not: a viewer whose terminal is
fitted back to its own box has the pty's rows pushed down into its scrollback, and is left showing
the empty part of the screen below them. Measured in the grid, before this was fixed: a panel that
painted **nothing at all**, while `tmux` held a shell prompt four rows into a 120×30 pane. The
terminal was 17 rows and its buffer held 30, and the bottom of a shell's screen is blank.

So the fit is made only by a client that may resize — the same `held` the keyboard and the touch keys
follow — and a viewer's terminal keeps the shape the server sent it. Its element is then a window
onto a screen larger than itself, and `.terminal__screen` scrolls so that the rest of it can be
reached. A controller never overflows, because its box is what the pty was resized to.

Giving up the keyboard gives up the shape too, and taking it takes both: a client that becomes the
controller states its own size at once, and the scheduler is told to forget the size it had recorded
— otherwise a box that happened to measure what the server last chose would say nothing, and the pty
would keep a shape no screen in the room has.

The other thing the view owes this rule is where it is looking, and that half is not about the shape at
all. A snapshot *is* the present — `Screen.Render` says a client that writes one is then showing
exactly what the pane is showing — but a terminal with scrollback of its own, which every terminal here
has as soon as it has scrolled, can take the snapshot into its screen rows while its viewport sits
several rows above them. The page is then painting the terminal's history while tmux is painting its
screen. Measured in the recovery suite after a server restart: a reconnected Claude tab drawing the
splash screen that came *before* the theme picker the pane held, with twelve of the pane's fourteen
rows on the page and not one of them in the right place. New *output* must not drag a reader to the
bottom — that is the local scroll model, and it is why there is a button to jump back — but a snapshot
is not output: it is the screen drawn again, so it puts the viewport at the bottom and says so,
whoever was reading what.

### Input

A viewer's input is refused with `not_controller`, and the bytes are dropped. Nothing about them is
logged — see §11.

The client does not send input it knows will be refused, and the view makes the reason visible: the
Prompt Bar is disabled and says why.

## 9. Storage

**Nothing about control is persisted, and no new column or table was added.**

The project row gained `pinned_slot` in Phase 5 because two devices have to agree on the workspace;
that is a durable fact about a project. Control is not: it is a property of live connections, it is
meaningless when there are none, and a server that had it written down would have to decide what a
row saying "client c_9f2 is the controller" means when c_9f2 has not been seen for three days.

The database continues to hold no terminal output, no keystrokes, no prompt text and no input
history. This phase added nothing to it, which is the outcome §19 asked for.

## 10. Isolation

| | |
| --- | --- |
| Controller of A types | reaches A's terminal, and only A's |
| Viewer of A types | refused, and A's terminal does not change |
| Controller of A acquires B | does not affect A's lease |
| Viewer of A requests A | other projects' rosters do not change |
| A's controller disconnects | A's lease suspends; B's is untouched |

The authority is keyed by project. There is no global controller, no global mode, and no state that
one project's control can reach.

## 11. Security

The boundary this phase does **not** move: there is still no authentication. A client that can reach
the WebSocket can subscribe to any project the server knows, and can claim any `clientId` it likes.
Control limits what *cooperating* clients do to each other; it does not limit what an attacker does.

What is enforced:

- **`clientId` is shape-checked.** It is a bounded identifier that cannot carry a path, a control
  character, or a large string into a lookup, a log line, an error message, or another client's
  screen. It is an identifier, not a credential, and the server never treats it as one.
- **`device` is the server's reading of the `User-Agent`**, not client-supplied text.
- **A log record carries four fields**: `clientId`, `projectId`, the event, and the time. Nothing
  else. Phase 4 allowed byte and frame counts on the disconnect record; this phase removed them,
  and the atomics that fed them, along with the peer address that had been on every line a
  connection wrote. The reasoning is the same one that produced the rule: the shortest list of
  fields is the one that cannot grow, by accident or by convenience, into a field that carries what
  somebody typed. `TestALogRecordCarriesOnlyTheFourFields` walks the paths this phase added and
  fails on any attribute outside those two keys.
- **No prompt, no keystroke, and no output ever reaches a log**, including in the refusal paths this
  phase added. A refused keystroke is logged as the fact of the refusal against a project — never
  as bytes, never as a count of them.
- **No IP address is shown to any client, or written to a log.** A device is "iPad Safari" and
  nothing more. The server does not keep the peer address in its connection record either: a field
  whose only reader is a log line is a field the terminal has no use for.

## 12. Protocol

Additions to the Phase 4/5 wire protocol. Everything in `docs/TERMINAL.md` §2 still holds: one
socket per browser, binary frames for output, text frames carrying JSON for everything else.

**Client → server**

| message | fields | meaning |
| --- | --- | --- |
| `control.request` | `projectId` | ask for the lease |
| `control.release` | `projectId` | give up the lease |
| `control.accept` | `projectId`, `clientId` | hand the lease to that client |
| `control.reject` | `projectId`, `clientId` | refuse that client's request |

**Server → client**

| message | fields | meaning |
| --- | --- | --- |
| `control.granted` | `projectId`, `control` | you hold the lease |
| `control.denied` | `projectId`, `reason`, `control` | your request was refused |
| `control.revoked` | `projectId`, `control` | you no longer hold the lease |
| `control.expired` | `projectId`, `control` | a suspended lease lapsed |
| `control.changed` | `projectId`, `control` | the roster changed |

`control.changed` is the broadcast that keeps every viewer's roster correct, and it is the message
`docs/PROTOCOL.md` §5 has reserved as `controller.changed` since Phase 0.

Every `control.*` message carries the roster, including the ones directed at a single client — which
is why there is no separate message for "somebody is asking": the queue is in the roster.

**The roster**, carried by every `control.*` message:

```json
{
  "controller": { "clientId": "c_9f2…", "device": "Safari on iPad" },
  "suspended": false,
  "expiresAt": null,
  "viewers": 2,
  "pending": [ { "clientId": "c_41b…", "device": "Chrome on Android" } ]
}
```

`controller` is `null` when nobody holds the lease. `viewers` counts the connections watching the
project that are neither the recipient's own nor the controller's — it is built per recipient,
because "how many others are watching" is not a property of the project. There is no field for "are
you the controller": a client compares `controller.clientId` with its own identifier, and one
comparison that cannot disagree with itself is better than a field that can.

`expiresAt` is present **only while the lease is suspended**. A connected controller holds its lease
for as long as it is there, so there is no moment to name.

The roster is broadcast on every change to it: a grant, a release, a transfer, a suspension, a
resume, an expiry — and also when the audience changes, which is when a client starts watching or
stops. A count that only updated on control events would show a controller "2 viewers" long after one
of them closed its tab.

**Version.** `protocolVersion` is raised to 2. The bump is not for the new messages — those are
additive — but because control changes what an *existing* message does: in Phase 5 any client
watching a terminal could type into it, and in Phase 6 it cannot until it asks. A Phase 5 client
against this server would find its Prompt Bar refusing everything with no way to ask, which is a
client drawing a state that is not true, and that is what the version check exists to catch. Both
ends refuse cleanly and tell the person to reload.

`hello` changes shape with it — `clientId` now means the browser session and `connectionId` the
socket:

```json
{ "type": "hello", "protocol": 2,
  "clientId": "c_9f2…", "connectionId": "ws_000003",
  "device": "iPad Safari", "server": "AgentMux", "version": "0.1.0" }
```

**New error codes.** `not_controller` (this message needs control and this client does not have it)
and `not_pending` (an accept or reject named a client that has not asked). Both carry `about`, the
message type that failed, as every terminal error does.

## 13. Client state

Control state is **per project**, and there is no global mode anywhere in the client.

```text
per project:  'controller' | 'viewer' | 'unknown'
```

`unknown` is the state before the server has said, which is what a panel shows while it is
connecting. It is not `viewer`: a viewer is a fact the server stated, and a client that assumed it
would draw a Request Control button that might be wrong.

A panel that is not subscribed has no control state at all, because control follows a subscription.
A project on a hidden workspace page is not being watched and therefore cannot be controlled from
that page — which is consistent rather than an omission: you cannot type into a terminal that is not
on your screen.

**Focus and full screen change nothing.** They are layout. A viewer that goes full screen is a
viewer with a bigger terminal, and a controller that goes full screen is still the controller. No
display mode can acquire or lose control.

## 14. What the UI shows

Panel header, compact, in every mode. One badge, and what it can say:

```text
controller, alone:           You control
controller, watched:         You control · 1 watching      (or · 3 watching)
viewer, somebody holds it:   Viewer · Chrome on Windows has control
viewer, that device is away: Viewer · Safari on iPad is away
viewer, nobody holds it:     Viewer · nobody is in control
viewer, waiting:             Waiting for control…
```

The device name is the label the server derived, so what a person reads is which *device* has the
terminal rather than which person — there is no person in this design, only a browser session. The
count appears only for the controller, only when somebody is watching, and counts the *others*: a
controller with no audience does not need to be told it has none, and does not need to be counted
among its own audience.

The badge is not a field the server sends about the recipient. `control` names who holds the lease;
a client compares that with its own identifier, which is what makes this device's own row read as
*You control* and everybody else's read as a device name. A message carrying an `isController` flag
would be a second place for the two to disagree.

Beside it, one button: **Request control**, or **Release control** for whoever holds it, or
**Waiting…** while an answer is owed. One button rather than a pair, because at any moment exactly
one of those is the useful thing to do. It is in the panel's bar in every mode — the grid included,
where the bar is narrow but this is the action a panel cannot be without — and the grid's panel menu
carries the same action under the same words, because a menu is the only thing in a grid header with
room to grow. **Redraw** is the opposite case: it is in the bar in focus and full screen and in the
menu alone in the grid.

While a request is outstanding, the controller is shown the row it has to answer —
`Safari on iPad is asking for control`, with **Hand over** and **Decline** — and the requester's badge
reads *Waiting for control…* while its button reads *Waiting…* and is disabled. The request is a row
in the controller's view of the project rather than a notification anywhere else, because the answer
belongs beside the terminal it is about.

The words are chosen to be true. There is no "Take control" anywhere: nothing in this design takes
control from anybody, and a button that says it would be describing a feature that does not exist.
A viewer **requests**; a controller **releases** or **accepts**.

The Prompt Bar is disabled for a viewer and says why, rather than accepting text that would be
refused.

## 15. Limits

- One controller per project, enforced by a single lock in one place. Two simultaneous requests for
  an unclaimed project are resolved by that lock; exactly one is granted and the other is told the
  roster. This is the race §18 of the phase directive asks to be tested, and it is tested by
  issuing the two requests from two goroutines and asserting that exactly one `granted` was sent.
- Control is not transferred by disconnecting deliberately: releasing and disconnecting are
  different actions with different consequences.
- Nothing in this design reads terminal output to decide anything. Authority comes from the control
  plane and from nowhere else — an output containing the word "permission" changes nothing.

## 16. Known limitations

1. **No authentication.** Anyone who can reach the server can claim any `clientId`, and therefore
   can become the controller of anything. The network is the boundary; the design of this phase does
   not change that and does not pretend to.
2. **Control is cooperative.** A client that lies — that ignores `not_controller` and keeps sending
   input — is refused by the server every time, so the terminal is safe. But a client can still
   claim a `clientId` that is not its own and thereby *resume* a lease that is not its own. There is
   no identifier here that cannot be forged, because forging one requires nothing an attacker would
   need to steal.
3. **Two tabs in one browser are two clients.** They share `sessionStorage` only within a tab, so
   they will not fight over a lease; but the second tab is a viewer even on the machine that holds
   control.
4. **A lease is lost on server restart** and re-negotiated. Usually instant, but it is a
   re-negotiation rather than a restoration, and a client that does not ask again stays a viewer.
5. **The grace period is per client, not per project.** A client holding control of four projects
   that disconnects suspends four leases, all with the same timer.
6. **A lease cannot be given up from a panel that is not on screen.** `control.release` exists, the
   server handles it, and the control a person presses is in the project's own panel — so a
   controller who closes that panel, or pages away from it, holds the lease until the grace lapses
   or until they come back and release it. If they are still connected, that lease never lapses on
   its own, because there is no idle timeout (see §6). It is a gap in the interface rather than in
   the server, and it is the one place where "the controller keeps control until somebody asks" is
   inconvenient rather than correct.
