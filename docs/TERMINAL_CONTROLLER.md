# The Terminal Controller

Who may type into a terminal, and how that changes hands.

```text
Client ──▶ control.request ──▶ Lease (one per project) ──▶ input frames accepted
                                    │
                            granted · queued · refused
```

Every terminal has exactly one pty, and this is the rule that decides which
client's keystrokes reach it. The console and the workspace both draw the same
terminal and both speak the same protocol; what differs is only whether the
person in front of them has asked for the keyboard, and whether they were given
it.

## 1. The lease model

Control is a **lease**, not a flag: it carries who holds it, when it was granted,
and what happens when they stop answering.

A boolean "this project has a controller" answers the question only while the
answer is stale. The controller closes their laptop, and the flag sits there
saying a terminal is owned by nobody who is present — so the next person to walk
up cannot take it, and no rule says when that changes. A lease expires. That is
the difference between a fact and a guess.

```go
// internal/terminal/authority.go
type Lease struct {
    ControllerID string            // the client that holds it (c_…)
    ConnectionID string            // the connection it last arrived on
    Device       string            // derived server-side, from User-Agent
    GrantedAt    time.Time
    Suspended    bool              // the holder's connection is gone
    ExpiresAt    time.Time         // set while suspended
    Pending      []PendingRequest  // clients waiting, oldest first
}
```

**One lease per project, held in memory.** There is no table and no row
(`internal/terminal/authority.go`), and there is deliberately nothing to persist:
a lease describes a live connection, and the server that would read a row back
is not the server that wrote it. A restart therefore forgets every lease, which
is asserted rather than tolerated — `web/e2e/suites/controller.mjs` restarts the
server and checks that it hands control to nobody.

The lease holds **no terminal content of any kind**: no keystrokes, no scrollback,
no prompt, no output. It is three identifiers and three timestamps, and §5 of this
document is about the rule that keeps it that way.

## 2. Ownership

A client's identity is the same one the transport has always used: a
`sessionStorage` value under `agentmux.clientId`, of the form `c_<16 hex>`. It is
**not** regenerated per page load — a reload is the same client reconnecting, and
a second tab is a different one. That is what makes a lease survive a refresh:
the holder comes back, presents the same identifier, and is recognised
(`internal/terminal/authority.go` `Resume`).

The device label a lease carries is derived **server-side** from the request's
`User-Agent` (`internal/terminal/device.go`). A client cannot name itself
anything: the field is not on the wire and the roster's "Chrome on Windows" is a
fact the server worked out, not a claim somebody made.

Five states, three of which are what a person sees:

| State | What it is | What a viewer sees |
|---|---|---|
| **AVAILABLE** | nobody holds it | `Viewer · nobody is in control` — a Request button |
| **HELD** | one client holds it, and is connected | `Viewer · Chrome on Windows has control` |
| **REQUESTED** | somebody holds it; one or more clients are queued behind | `Waiting for control…` for the queued client |
| **SUSPENDED** | the holder's last connection dropped, and the lease is being kept for them | `Viewer · Chrome on Windows is away` |
| **EXPIRED** | a suspended lease lapsed. The project is AVAILABLE again | as AVAILABLE |

SUSPENDED is the state the brief's four names leave out and the implementation
needs, because it is the one where the roster would otherwise lie. Between a
laptop closing and its lease lapsing there is a window in which `ControllerID` is
set and nobody is home; saying "has control" during it would be the terminal
explaining its silence with something untrue. `Suspended` is that window, and it
is set by `Detach` when a client's last connection ends
(`internal/terminal/authority.go`).

**The grace period is 30 seconds** (`DefaultControlGrace`). Long enough for a
phone to change networks, a laptop to be reopened or a page to be reloaded; short
enough that somebody else can take over without going to find a person. It is the
one number in this design that is a judgement about people rather than about
machines, and the E2E fixture sets it to eight seconds so the lapse can be
watched rather than waited for.

A client returning inside the window is **resumed**, not re-granted: the lease is
the same lease, `GrantedAt` is unchanged, and the roster tells everybody
`resumed`. Outside it, the lease is gone and the project is free — a request in
that window is refused, and so is one in the window before it. §4 of
`docs/MULTI_DEVICE.md` has the device-side view of the same rule.

## 3. Input flow

The whole surface is the WebSocket that already exists, at `/api/ws`, speaking
`agentmux.terminal.v2`. **There are no HTTP routes for control**, and that is a
decision rather than an omission: HTTP acquire/release were designed and
deliberately not implemented (`docs/PROTOCOL.md` §3, `docs/CONTROLLER_API.md` §1).
A lease belongs to a connection — that is what suspension means — and an HTTP
request has no connection to belong to, so the resume path would have had no way
to recognise the client it was resuming.

```text
console A                          server                          console B
    │                                │                                │
    │  control.request ─────────────▶│  authority.Request             │
    │                                │  free → grant (ReasonAvailable)│
    │◀── control.granted ────────────│                                │
    │                                │── control.changed ────────────▶│
    │  input "l" ───────────────────▶│  MayInput → allowed            │
    │                                │  ──▶ pty                       │
    │                                │                                │
    │                                │◀─── control.request ───────────│
    │                                │  held → queued (ReasonQueued)  │
    │                                │── control.changed ────────────▶│ (in the roster)
    │  control.release ─────────────▶│  authority.Release             │
    │                                │── control.changed ────────────▶│ nobody in control
    │                                │◀─── control.request ───────────│
    │                                │  free → grant                  │
    │                                │── control.changed ────────────▶│
```

Four outcomes of a request, and each one is a different message:

| Outcome | Reason on the wire | What the asker is told |
|---|---|---|
| granted | `available` | `control.granted`, and it becomes the controller |
| already yours | `held` | `control.granted` again — asking twice is not an error and changed nothing |
| queued | `queued` | nothing directly; it appears in the roster as `Waiting for control…`, which every viewer receives |
| refused | `controller_exists` / `too_many_requests` | `control.denied`, with the reason |

**A refusal is a refusal; it is never a preemption.** §9 of the brief is explicit
and the server has no code that could do otherwise: nothing in `authority.go`
evicts a controller to make room. The controller leaves by releasing, by
transferring, or by its lease expiring after it has gone quiet — and the last of
those is not eviction either, because the lease has already been suspended for
thirty seconds by then and its holder is not there to be evicted from anything.

§9's suggested sentence for the refusal is `Controlled by another client`, in a
JSON body from an HTTP route. The route is not there (§3 above), so the sentence
lives in the client instead, in one place both pages read: the console and the
workspace both say **"Somebody else is using this terminal."** for
`controller_exists` (`web/src/components/ProjectTerminal.tsx` `describeRefusal`).
What matters about that sentence is that it exists at all: a refusal leaves the
roster exactly as it was, so without it the button appears to have done nothing.

The fourth gate in §4 is on the only path that reaches a pty, and it is the one
that judges every keystroke.

One asymmetry is worth stating because it looks like an oversight and is not.
`control.release` carries **no** "are you watching this" check, while
`control.request` requires one. A controller that paged away from a project is
not watching it any more and still holds its lease — and giving it up is the one
thing it must be able to do from there. Requiring it to be subscribed first would
make a lease impossible to shed from exactly the situation where shedding it is
the only way out.

## 4. The four gates

Four independent things have to be true before a keystroke reaches the pty. None
of them is a check on the others; they are four mechanisms at four layers, and
each was put there to hold when the one above it is wrong.

| # | Where | What stops it |
|---|---|---|
| 1 | `TerminalViewer` / `ProjectTerminal` | pass `interactive={held}`. `TerminalView` registers `term.onData` only while `interactive`, so a viewer's terminal has **no input handler bound at all** — a keystroke has nothing to reach, rather than a handler that declines it. The touch keys follow the same flag, because they call `session.input` directly rather than going through `onData`. |
| 2 | `useTerminalSession` | `input()` returns early unless `controlRef.current.held`. |
| 3 | `internal/terminal` | `handleInput` refuses a client that is not the lease holder with `CodeNotController` (`internal/terminal/conn.go`). |
| 4 | `internal/terminal` | `authority.MayInput` re-reads the lease from authoritative state on **every** frame — owner, suspension, expiry — so a frame that gets past 2 and 3 is still judged against the truth as the server knows it. |

Gates 1 and 2 are the same idea one level apart: one is about what exists in the
DOM, the other about what the hook will send. Gate 4 is the one that holds when
the first three are wrong, and it is the one a client cannot influence at all —
which is the next section.

## 5. Security

**The server does not trust the frontend, and does not need to.**

The identity a control decision is made against is `c.clientID` — the identifier
the *connection* registered with when it connected. It is never read from the
frame being processed:

- `control.request` and `control.release` are refused outright if they carry any
  field but `projectId` (`clientMessage.hasExtraFields`, `internal/terminal/protocol.go`).
  There is no `clientId` field for one to arrive in.
- `input` and `resize` are checked the same way.
- The only message that carries a `clientId` is `control.accept` / `control.reject`,
  where it **names** the pending client whose request is being answered. It is
  validated as an identifier shape, it is refused if it names the sender, and it
  is never a credential: the sender's own authority to answer is checked
  separately, against the lease (`internal/terminal/control.go`).

Every input frame is therefore judged on four things, all server-side:

1. the connection is subscribed to that project (`c.watches`);
2. the connection's client identifier is the lease's `ControllerID`;
3. the lease is not `Suspended` — a connection presenting an identifier whose
   holder is recorded as away is treated exactly like a stranger, because a
   suspended lease cannot be typed from by definition;
4. the lease has not expired (`ExpiresAt`).

**A refused frame is refused, not fatal.** The server answers with an `error`
carrying `not_controller` and closes nothing: dropping the connection would turn
one stray keystroke into an outage for everybody watching. That the connection
survives, and the lease is where it was, is asserted in
`web/e2e/suites/controller-input.mjs` — which also forges an input frame past the
client, because every other check there is a fact about the console and §11 is a
fact about the server. A frontend claim proved by testing the frontend proves
nothing about what happens when the frontend is bypassed, and the socket is right
there.

### What is recorded

Control events are logged, at `Info`, by `internal/terminal/authority.go`:

| Line | When |
|---|---|
| `control requested` | a client asks |
| `control granted` | the lease is given |
| `control denied` | a request is refused, with its reason |
| `control released` | the holder gives it up |
| `control transferred` | the holder hands it to a queued client |
| `control request refused` | the holder declines a queued client |
| `control suspended while the controller is away` | the holder's last connection ends |
| `control resumed after reconnect` | the holder comes back inside the grace |
| `control expired` | a suspended lease lapses |

Each carries a `projectId` and a `clientId`, and nothing else.

**Nobody's keystrokes are logged, at any level, ever.** §12 of the brief forbids
it and the reason is not tidiness: a terminal is where people type passwords,
tokens and private code, and a log is the easiest place for one to end up
somewhere nobody meant it to be. The refusal path says so explicitly — a refused
keystroke is still a keystroke, so `handleInput` logs the fact of the refusal
with no bytes and no count of them (`internal/terminal/conn.go`). The same rule
is why the lease has no field to put them in.

## 6. Multi client

Several browsers may watch one terminal. Exactly one of them may type into it,
and the others are told who that is — which is the whole reason the roster is
broadcast to every viewer rather than only to the controller.

- **The queue is bounded.** `MaxPendingRequests = 8`, because a pending request
  is an entry in a roster that is sent to every viewer, and without a bound a
  client could ask in a loop and make everybody else's roster grow.
- **Handover is explicit.** A controller answers a queued request with
  `control.accept` (transfer) or `control.reject`. Both are the holder's
  decisions; neither is available to anybody else.
- **A second asker is queued, not refused.** Asking while somebody has the lease
  is not an error — it is a request, and it is how the handover above can happen
  at all.
- **What is shared is the pty and nothing else.** Scroll position never crosses
  the wire, viewport size never crosses the wire (§7), and client identity is per
  tab.

The scenario §15 of the brief asks for is exactly
`web/e2e/suites/controller-input.mjs`: two consoles on one project, A takes
control, A types a marker the pty receives, B's typed marker never reaches it, B
forges a frame and the server refuses it, A releases, B takes control and now B's
typing is what arrives. The last step is what makes the fourth one mean
something: a viewer that cannot type and a browser that is broken look identical
from a single check, and the same device succeeding once it holds the lease is
what says the refusal was about authority.

## 7. Resize authority

`MayInput` and `MayResize` are separate methods sharing one implementation
(`authority.may`), and the split exists for a configuration people will ask for:
a phone typing into a terminal drawn on a desktop. A design where
`canType == canResize` cannot express it — the phone would reflow the pty to its
own width and reshape the desktop's screen.

**On the console the answer is no, always.** `TerminalViewer` passes
`mayResize: false` and never varies it, because a card is a few hundred pixels in
a grid and a browser that is only watching must not reshape the terminal somebody
is working in. §14 of the brief gives resize to the controller; this narrows that
on the console deliberately, and the narrowing is structural rather than a prop
that happens to be false:

| Path a size could take | How it is closed |
|---|---|
| `fit()` → `session.resize()` | `mayResize: false` makes `TerminalView` never call `fit()`, and `useTerminalSession.resize()` returns early on the option |
| the size a **subscribe** carries | withheld at the session (`subscribe(projectId, mayResize ? size : null)`), because the server applies a subscribe's size for a client that holds the lease (`internal/terminal/conn.go`) |
| the size stated when the lease is **granted** | `useTerminalSession` skips it under the same option |
| a `resize` frame sent anyway | `authority.MayResize` refuses it with `CodeNotController` |

The second and third are the ones worth naming, because neither is a `resize`
frame and a rule written only against `resize` would miss both. The E2E suite
measures the pane from tmux on either side of a controller typing, and reads the
wire for a `resize` frame, so a console that asked and was refused fails there
too.

The workspace is unchanged by any of this: its panel measures itself and resizes
when it holds the lease, which is what it has always done.

## 8. Tests

| Where | What it holds |
|---|---|
| `internal/terminal/authority_test.go` | the lease lifecycle: grant, release, transfer, refusal reasons, the bound on the queue, suspension and expiry, resume inside the grace, a denied request being recorded |
| `internal/terminal/control_test.go` | the wire: which frame each outcome produces, who is told, and what a forged or malformed frame is answered with |
| `internal/terminal/multi_device_test.go` | two connections on one project, one controller, and a race between two askers |
| `web/src/terminal/useTerminal.test.tsx` | the React seam: who may type, who may resize, what a client does when it is handed the keyboard, and a controller that holds the lease and still names no geometry |
| `web/src/dashboard/TerminalViewer.test.tsx` | the console's card: no handler bound while a viewer, exactly one while the controller, the refusal sentence, the queue offered only to the holder, a release offered to a controller whose terminal ended |
| `web/e2e/suites/controller.mjs` | two real browser contexts on one terminal: the handover, the suspension and the resume, a restarted server |
| `web/e2e/suites/controller-input.mjs` | typing into the console: the marker reaching the pty from the controller, the geometry not moving, the viewer refused, a forged frame refused by the server, and the handover |

## 9. What is not here

Each is a later phase's subject rather than an omission, and §24 of the phase
brief is where the list comes from:

- **permission auto-approval** — nothing on a terminal approves anything;
- **action execution** — a controller types; no button acts on the agent's behalf;
- **push notifications** — nothing leaves the browser;
- **CC Switch control** — no second control surface;
- **a mobile app** — the console is the same page at a narrower width;
- **HTTP control routes** — designed, and deliberately not built (§3);
- **more than one controller** — the lease is exclusive by construction;
- **controller-driven resize on the console** — §7, and it is a choice.
