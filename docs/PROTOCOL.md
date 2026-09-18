# AgentMux Client/Server Protocol

This document is the original design sketch for the client/server surface, kept because it is what
the phases were planned against. **`docs/TERMINAL.md` is the reference for what was actually built** —
the real endpoint, the real message names, the real frame layout. Where the two disagree, this
document says so in place rather than being rewritten to match, because the disagreements are the
interesting part.

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

**Phase 4 implemented sections 4 to 13**, with two exceptions and several deliberate departures from
the sketch. The endpoint is `GET /api/ws`, one socket per browser, and `docs/TERMINAL.md` documents it
in full: the client and server message tables, the binary frame layout, the snapshot boundary, the
limits, and the error codes. What follows is the sketch, annotated with what it became.

The two exceptions are the controller sections, §8 and §9, which are Phase 6 and are still not
implemented: there is no roster, no lease, and no notion of who may type. The departures are called
out in §4, §5 and §11.

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
controller.changed     Phase 6
terminal.snapshot      as built: a binary frame, type 0x02
terminal.output        as built: a binary frame, type 0x01
terminal.resized       as built: "resized"
provider.changed       Phase 8
error                  as built: "error"
hello                  as built: sent first on every connection
unsubscribed           as built: acknowledges the end of a subscription
pong                   as built: answers "ping"
```

Client → Server:

```text
project.subscribe      as built: "subscribe"
project.unsubscribe    as built: "unsubscribe"
terminal.input         as built: "input"
terminal.resize        as built: "resize"
controller.acquire     Phase 6
controller.release     Phase 6
resync                 as built: ask for a fresh screen
ping                   as built: application-level liveness
```

The `terminal.` prefix was dropped from the names that shipped, and the reason is that with the
prefix gone the messages read as what they are — a flat namespace of seven verbs, all of which are
about a terminal, since that is the only thing this socket carries. A prefix that every member of a
namespace shares is a word that carries no information.

Three messages are new relative to the sketch: `hello`, which states the protocol version before
anything else; `unsubscribed`, so a client tearing down a terminal can distinguish that from a stream
that quietly stopped; and `resync`, which is the recovery request the sketch's §11 implies but does
not name.

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

**The pipeline is shared as written; the validation is not there yet.** Both the terminal's keyboard
and the Prompt Bar send `input` messages, and a prompt is that message with a trailing carriage
return — there is one way into a terminal, not two. What does not exist is a controller: every
subscription is equal, and any subscriber may type. Rejecting Viewer input requires a notion of who
is a viewer, which is Phase 6's lease and not something this phase could have half-built.

What *is* enforced: input is only accepted for a project the connection has already subscribed to
(`not_subscribed`), it is size-limited, and there is no message that runs a command. A browser can
send keystrokes to a terminal it is watching, exactly as typing into it would.

## 8. Controller acquisition

Atomic server-side operation.

The server is authoritative.

## 9. Controller lease

Store:

```text
controllerId
leaseExpiresAt
```

Temporary disconnect grace:

approximately 10 seconds.

## 10. Terminal resize

Only Controller resize requests are accepted.

Viewer resize is ignored.

**Implemented with the controller exception again, and with one addition.** A resize sets the pty's
canonical size — there is one pty, so there is one size — and the server then tells *every*
subscriber of that project the new size, not only the client that asked. Each client draws at the
size it was told rather than the size it requested, which is what stops a client and the server from
answering each other's sizes forever. Requests are clamped to bounds rather than refused.

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

Still reserved and still unimplemented — Phase 8. Nothing in Phase 4 or Phase 5
touched the provider path.
