# AgentMux Client/Server Protocol

## 1. Transport

Use:

- HTTP/JSON for low-frequency operations;
- one primary WebSocket per browser client for real-time terminal/state traffic.

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

Sections 4 to 13 describe the WebSocket protocol, none of which is implemented yet — the server opens
no WebSocket in this build, and the Phase 2 endpoints listed above are REST. The controller endpoints
are Phase 5. A request to any unimplemented route returns `404 not_found`.

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

## 5. Message categories

Server → Client:

```text
server.status
project.status
controller.changed
terminal.snapshot
terminal.output
terminal.resized
provider.changed
error
```

Client → Server:

```text
project.subscribe
project.unsubscribe
terminal.input
terminal.resize
controller.acquire
controller.release
```

## 6. Terminal output

Preserve raw bytes/ANSI behavior.

Requirements:

- project identity;
- monotonic sequence;
- batching of small writes;
- avoid per-character messages.

## 7. Input

Prompt Bar and raw terminal keyboard share one controller-validated pipeline.

Server rejects Viewer input.

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

## 11. Snapshot / reconnect

V0.1 may send a full snapshot/history.

Later:

```text
lastSequence
→ incremental replay
→ snapshot fallback
```

## 12. Scroll

Scroll position never crosses the protocol.

It is local browser state.

## 13. Future provider messages

Reserved:

```text
provider.list
provider.current
provider.switch
provider.changed
```

Never include credentials.
