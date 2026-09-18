# AgentMux Client/Server Protocol

This document is the original design sketch for the client/server surface, kept because it is what
the phases were planned against. **`docs/TERMINAL.md` is the reference for what was actually built** —
the real endpoint, the real message names, the real frame layout — and **`docs/MULTI_DEVICE.md` is
the reference for control**, which was sketched here in §8 and §9 and designed in full in Phase 6.
Where the two disagree, this document says so in place rather than being rewritten to match, because
the disagreements are the interesting part.

## 1. Transport

Use:

- HTTP/JSON for low-frequency operations;
- one primary WebSocket per browser client for real-time terminal/state traffic.

Both are implemented as written. The WebSocket is `GET /api/ws`; see §4 below and `docs/TERMINAL.md`.

## 2. Suggested MVP REST surface

```text
GET  /api/server
GET  /api/projects
GET  /api/projects/discover
POST /api/projects
POST /api/projects/register
POST /api/projects/:id/open
POST /api/projects/:id/start
POST /api/projects/:id/stop

POST /api/projects/:id/controller/acquire
POST /api/projects/:id/controller/release
```

Implementation status: the first five are implemented, and `GET /api/projects/:id` was added to read
one project. See `docs/API.md` for the request and response shapes this build actually serves.

The runtime endpoints took a different shape than the sketch above when Phase 2 built them. Where
this section says `POST /api/projects/:id/start` and `.../stop`, the server serves
`POST /api/projects/:id/runtime/start`, `.../runtime/stop`, `DELETE /api/projects/:id/runtime`, and
`GET /api/projects/:id/runtime`. The nesting is the point: these verbs act on the project's *runtime*,
and naming them `:id/start` left "start what?" open — the same word was being used for opening a
project in the UI, starting a session, and starting the program inside it. `POST :id/open` is not
implemented and is not planned as an endpoint; opening a project is a client-side view change, not a
server-side event, and giving it a route would make the server own something it has no state for.

Phase 3 added the third level of that nesting:
`GET /api/projects/:id/runtime/agent` and `POST .../agent/start` and `.../agent/stop`. An agent's
identity is the runtime it is in, so it is addressed through it rather than as an object of its own.

**Phase 6 did not implement the two controller endpoints, and the reason is the same one that made
`:id/open` a view change.** Control is a fact about a *connection*: a lease is held by a client
because it has a socket open, it is suspended when that socket drops, and it is released when the
client goes away. `POST /api/projects/:id/controller/acquire` names neither the socket nor the
client, so a client asking with it would be asking for a lease it has no way to hold — and every
HTTP request would arrive from a different connection, which the server has no reason to treat as
the same device. Control therefore travels on the WebSocket, where the connection is the thing
making the request: `control.request`, `control.release`, `control.accept`, `control.reject` out;
`control.granted`, `control.denied`, `control.revoked`, `control.expired`, `control.changed` back.
`docs/MULTI_DEVICE.md` is the reference for them.

**Phase 4 implemented sections 4 to 13**, with two exceptions and several deliberate departures from
the sketch. The exceptions — §8 and §9, the controller sections — were Phase 6 and are implemented
now. The endpoint is `GET /api/ws`, one socket per browser, and `docs/TERMINAL.md` documents it in
full: the client and server message tables, the binary frame layout, the snapshot boundary, the
limits, and the error codes. What follows is the sketch, annotated with what it became.

## 3. Project registration payload

Conceptual example:

```json
{
  "name": "AgentMux",
  "hostPath": "D:\\AI\\Projects\\2026 AgentMux\\AgentMux",
  "collectionPath": "D:\\AI\\Projects\\2026 AgentMux"
}
```

Server derives/validates the runtime path through HostAdapter.

## 4. WebSocket control envelope

```json
{
  "type": "project.status",
  "projectId": "p_123",
  "sequence": 42,
  "timestamp": "2026-09-17T12:00:00Z",
  "payload": {}
}
```

**As built, the envelope is thinner than this, and the sequence is not in it.** Control messages are
JSON objects with a `type` and, where they apply, a `projectId` — and nothing else. There is no
`timestamp`, because nothing consumed one; and there is no `sequence`, because the stream that needs
one is not the control stream. Sequence numbers belong to terminal output, which does not travel in
JSON at all: it travels in binary frames whose header carries the range of chunks the frame contains
(`docs/TERMINAL.md` §3). Putting a second, unrelated counter on control messages would have invited
exactly the confusion this split avoids.

The envelope is also strict in a way the sketch does not imply: a field that does not belong to the
message's type is refused rather than ignored.

`project.status` and `server.status` are not implemented. The UI learns project state from
`GET /api/projects`, which it already had; there was no second consumer to justify a push.

## 5. Message categories

Server → Client:

```text
server.status          not implemented
project.status         not implemented
controller.changed     as built: "control.changed"
terminal.snapshot      as built: a binary frame, type 0x02
terminal.output        as built: a binary frame, type 0x01
terminal.resized       as built: "resized"
provider.changed       Phase 8
error                  as built: "error"
hello                  as built: sent first on every connection
unsubscribed           as built: acknowledges the end of a subscription
pong                   as built: answers "ping"
control.granted        as built: this device has the lease
control.denied         as built: this device will not
control.revoked        as built: this device lost it
control.expired        as built: it was suspended and nobody came back for it
```

Client → Server:

```text
project.subscribe      as built: "subscribe"
project.unsubscribe    as built: "unsubscribe"
terminal.input         as built: "input"
terminal.resize        as built: "resize"
controller.acquire     as built: "control.request"
controller.release     as built: "control.release"
control.accept         as built: answer a request with the lease
control.reject         as built: answer a request with a refusal
resync                 as built: ask for a fresh screen
ping                   as built: application-level liveness
```

The `terminal.` prefix was dropped from the names that shipped, and the reason is that with the
prefix gone the messages read as what they are — a flat namespace of verbs, all of which are
about a terminal, since that is the only thing this socket carries. A prefix that every member of a
namespace shares is a word that carries no information. Phase 6 kept the same shape and added a
second, equally flat family under `control.`, which is where the sketch's `controller.acquire` and
`controller.release` landed.

Three messages are new relative to the sketch: `hello`, which states the protocol version before
anything else; `unsubscribed`, so a client tearing down a terminal can distinguish that from a stream
that quietly stopped; and `resync`, which is the recovery request the sketch's §11 implies but does
not name.

Phase 6 added six more, and they are worth reading as one design rather than as six names. A request
has an answer — `control.granted` or `control.denied` — and the answer goes to the asker alone,
because a refusal is a fact about one device. A *change* has no addressee: `control.changed` carries
the whole roster and is sent to every subscriber, because a viewer that does not know who is typing
cannot say so, and a controller that does not know how many are watching cannot say that either.
`control.revoked` and `control.expired` are the two ways a lease ends without the holder asking, and
they are separate messages because they call for different words on screen: one is "somebody else has
it now", the other is "you were away too long". They also travel differently, and for the same
reason. `control.revoked` goes to the device that lost the lease, which is still connected and needs
to be told. `control.expired` is broadcast, because the device it is about is the one that did not
come back — the people who need to hear it are the ones still watching a terminal whose owner has
gone.

There is no message that asks "am I the controller". The roster states who holds it, and a client
compares that with its own identifier — a field that only ever repeated the client's own answer would
be a second place for the two to disagree.

## 6. Terminal output

Preserve raw bytes/ANSI behavior.

Requirements:

- project identity;
- monotonic sequence;
- batching of small writes;
- avoid per-character messages.

Implemented as written, and the "preserve raw bytes" requirement is why output is not JSON. A JSON
envelope would mean base64 — a third more bytes and an encoding step on the hot path — so output
travels in binary frames, and the project identity and the sequence range live in the frame header
rather than around the payload. Batching happens in the runtime manager, which is the layer that
knows how much is waiting. `docs/TERMINAL.md` §3.1 has the byte layout.

## 7. Input

Prompt Bar and raw terminal keyboard share one controller-validated pipeline.

Server rejects Viewer input.

**The pipeline is shared as written, and as of Phase 6 the validation is there.** Both the terminal's
keyboard and the Prompt Bar send `input` messages, and a prompt is that message with a trailing
carriage return — there is one way into a terminal, not two. The server now has a notion of who may
type: a client holds a project's lease or it does not, an `input` for a project it does not hold is
refused with `not_controller` about `input`, and the refusal names that message so a client with
several subscriptions knows which one was refused. `docs/MULTI_DEVICE.md` §6 is the reference.

What was already enforced, and still is: input is only accepted for a project the connection has
already subscribed to (`not_subscribed`), it is size-limited, and there is no message that runs a
command.

## 8. Controller acquisition

Atomic server-side operation.

The server is authoritative.

**Implemented as written.** Acquisition is decided under the hub's lock, so two devices asking in the
same instant produce one lease and one refusal rather than two grants — which is the only thing that
makes "one controller" a fact rather than a hope. `internal/terminal/control_test.go` races them.

What the sketch does not say, and what the product needed, is that a device does not simply *take*
control. It asks for it: `control.request`. If nobody holds the project's lease the server grants it
on the spot (`control.granted`, reason `available`), which is the case the sketch describes. If
somebody does hold it, the request is queued and put in front of that person, who answers with
`control.accept` or `control.reject` — a transfer is a handshake between two devices that both know
about it, never a lease pushed at somebody. The interface says *Request control* everywhere, and
never *Take Control*, because on a project somebody else is typing into, taking is not what happens.

A client never becomes the controller by arriving, by reconnecting, or by being the only one left.
`docs/MULTI_DEVICE.md` §5 has the rules and §12 the four transfer cases.

## 9. Controller lease

Store:

```text
controllerId
leaseExpiresAt
```

Temporary disconnect grace:

approximately 10 seconds.

**Implemented with the same shape, a different number, and no storage at all.** The lease is
identified by the client's identifier and lives in memory; `leaseExpiresAt` is set only while the
lease is *suspended*, which is the disconnected case, and is empty the rest of the time — a lease
held by a device that is connected does not expire, because there is no reason to take a terminal
away from somebody who is looking at it.

The grace is thirty seconds, not ten (`terminal.DefaultControlGrace`, configurable through
`server.controlGraceSeconds`). §八 of the phase directive gave thirty as the example and the product
documentation says thirty; a person whose tablet slept on a train should find their terminal where
they left it, and ten seconds does not survive a tunnel.

Nothing about the lease is written to disk. §十九 of the directive asks for that, and the reason is
that the alternative is a database that has to be reconciled against reality every time the server
starts — a lease is a fact about a running process and stops being true when that process stops. A
restarted server therefore knows nothing, and what it must not do is invent an owner: see §三十三 and
`docs/MULTI_DEVICE.md` §10. What a roster row carries is one identifier, a device label derived
server-side from the User-Agent, and a timestamp. Never an address, never anything typed.

## 10. Terminal resize

Only Controller resize requests are accepted.

Viewer resize is ignored.

**Implemented with the controller exception again, and with one addition.** A resize sets the pty's
canonical size — there is one pty, so there is one size — and the server then tells *every*
subscriber of that project the new size, not only the client that asked. Each client draws at the
size it was told rather than the size it requested, which is what stops a client and the server from
answering each other's sizes forever. Requests are clamped to bounds rather than refused.

**Phase 6 supplied the exception.** A resize from a client that does not hold the project's lease is
refused with `not_controller` about `resize` and never reaches the pty, so the geometry follows the
keyboard: one device decides how wide the terminal is, and everybody else draws what it decided.
That is what makes a viewer that reshapes its own window a change to its own screen and to nothing
else — §二十八, verified in `web/e2e/suites/controller.mjs` by reading the pty's size rather than the
screen's.

A client is also expected to suppress resizes caused by a tablet's on-screen keyboard, which shrinks
the visual viewport without changing the layout one. That is a client rule; the server cannot tell
the difference, and resizing the pty for an animating keyboard would make every program inside
redraw continuously at a size nobody is looking at.

## 11. Snapshot / reconnect

V0.1 may send a full snapshot/history.

Later:

```text
lastSequence
→ incremental replay
→ snapshot fallback
```

**The "later" path was not taken, and the reason is worth recording because it looks like the obvious
optimisation.** A reconnecting client is given a fresh snapshot rather than a replay of what it
missed. Replay sounds cheaper, but it is only correct if the client's screen is exactly what the
server thinks it is, and the whole reason a client asks for a snapshot is that it is not sure. A
snapshot cannot be wrong about what the client is showing, because it replaces it.

The piece the sketch is right about is that the client must be able to tell where the snapshot ends
and the stream resumes. That is the boundary: the snapshot carries the highest sequence number it
already contains, and everything above it is applied afterwards. `docs/TERMINAL.md` §3.2 states
exactly what the boundary does and does not claim — in particular that a chunk published during the
capture may be drawn twice rather than lost.

Incremental replay remains available to a later phase, and the information needed for it — the
bounded history and the per-runtime sequence — is already there. It was not needed to make recovery
correct, so it was not built.

## 12. Scroll

Scroll position never crosses the protocol.

It is local browser state.

Implemented as written, and there is no message that could carry it. Two people watching one terminal
are looking at different lines, and a server that knew where either of them was would have to pick
one to obey. The consequences are in `docs/TERMINAL.md` §7: a resync does not clear a client's
scrollback, and arriving output does not move a client's viewport.

## 13. Future provider messages

Reserved:

```text
provider.list
provider.current
provider.switch
provider.changed
```

Never include credentials.

Still reserved and still unimplemented — Phase 8. Nothing in Phase 4, Phase 5 or Phase 6
touched the provider path.
