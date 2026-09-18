# AgentMux Architecture

## 1. High-level architecture

```text
iPad / Phone / PC
        │
        │ HTTPS + WebSocket
        ▼
┌─────────────────────────────┐
│       AgentMux Server       │   runs inside WSL on Windows (see §7)
│                             │
│ Project Manager             │   internal/project        — built
│ Session Manager             │   internal/session        — built in Phase 2
│ Terminal Manager            │   part of session.Manager — sequence, history
│ Terminal Transport          │   internal/terminal       — built in Phase 4
│ Workspace                   │   web/src/workspace       — built in Phase 5
│ Agent Manager               │   part of session.Manager — built in Phase 3
│ Agent Launcher              │   internal/claude         — built in Phase 3
│ Controller Manager          │   Phase 6, not stubbed
│ Provider Adapter            │   Phase 8, not stubbed
│ Host Adapter                │   internal/host           — built
│ Storage                     │   internal/storage        — built
└──────────────┬──────────────┘
               │
               ▼
         SessionBackend
               │
               ▼
             tmux
       ┌───────┼───────┐
       ▼       ▼       ▼
   Project A Project B Project C
    (shell)   (shell)   (shell)
      └─ claude   └─ claude   └─ claude    one agent per runtime, since Phase 3
```

The server's own process is the one that owns tmux, which is why the box above is inside the runtime
environment rather than beside it. Everything above `SessionBackend` is transport-agnostic; everything
below it is tmux's.

The agent is the one thing in this diagram that is **not** the server's child. It is typed into the
runtime's shell, so its parent is the pane leader inside tmux; the server recognises it by reading the
process table and never by parsing the terminal. That is what lets it outlive a restart, and it is
why the arrow above runs `shell → claude` rather than `server → claude`. See
`docs/CLAUDE_RUNTIME.md` §1 and §6.

## 2. Project vs Collection

AgentMux distinguishes:

```text
Projects Root
Collection / Group
Registered Project
```

Example:

```text
Projects Root:
D:\AI\Projects

Collection:
D:\AI\Projects\2026 AgentMux

Project:
D:\AI\Projects\2026 AgentMux\AgentMux
```

Runtime sessions attach only to registered Projects.

## 3. Architectural boundaries

### Project Manager

Owns:

- collection/project discovery;
- explicit project registration;
- project creation;
- project metadata;
- archive/open state.

Does not own terminal behavior.

### Session Manager

Owns:

- persistent runtime lifecycle;
- project-to-session mapping;
- starting/stopping/recovering sessions.

Implemented in `internal/session` as `Manager`, sitting between the HTTP handlers and the backend.
It holds every policy decision — what a session is called, where it runs, how big it is, what is
written down, how a project's status is derived — and the HTTP layer calls it rather than the backend.
Reconciliation on startup is here too.

Not yet owned here: controller leases and viewer lists. Those are properties of live connections and
are Phase 6. Phase 4 put a browser on the other end of the manager's subscriptions without giving it
either — every subscription is equal and any of them may type — and it settled the third item on that
list, scroll position, by deciding the server does not own it at all.

### SessionBackend

Abstract runtime interface. The one boundary through which AgentMux creates, talks to, and destroys a
terminal that outlives it (`internal/session/backend.go`).

V0.1:

```text
TmuxBackend
```

Future:

```text
ConPTYBackend
SSHBackend
DockerBackend
```

The interface is `Name`, `Available`, `Create`, `Exists`, `Inspect`, `List`, `Launch`, `SendInput`,
`Resize`, `Stop`, `Destroy`, `Snapshot`, `Attach`, `Close`. It is deliberately not shaped around
tmux's command set, and deliberately knows nothing about projects, HTTP, or prompts: it is handed a
name, a directory, and a size, and it carries bytes.

Full description, including the byte-fidelity contract and the tmux control-mode design:
`docs/RUNTIME.md`.

### Terminal Manager

Owns:

- canonical terminal size;
- output stream;
- output sequence;
- history/ring buffer;
- viewer subscriptions;
- controller ID/lease;
- input routing.

In Phase 2 the parts that exist — canonical size, output stream, output sequence, bounded in-memory
history — live inside the session manager's per-runtime state (`internal/session/buffer.go`). Viewer
subscriptions exist as `Backend.Attach`; multiple subscribers to one session each receive the same
bytes.

Phase 6 completed the list. Canonical size, viewer subscriptions and the controller lease are the hub's
business rather than the session manager's, because all three are facts about *clients* and the
session manager does not know what a client is: `internal/terminal/hub.go` holds the subscriptions,
`internal/terminal/authority.go` holds the leases, and input routing is the one line between them that
consults the second before it does the first. Scroll is still not a server concept and never will be —
`docs/TERMINAL.md` §7.

### Workspace

Owns:

- which projects are in the workspace, and in what order (a reserved slot per project);
- how many cells a page has, and which projects are on it;
- which page a browser is showing, and which project it has focused.

It is not a server component and it is not a new layer in the diagram above: it is
the client's composition of the terminal layer into a grid. Its one server-side fact
is `projects.pinned_slot`, which is stored because two devices have to agree on it;
everything else about the workspace is a property of one browser at one moment and
is stored nowhere. `docs/WORKSPACE.md` is the whole of it.

### Controller Manager

Guarantees one input controller per project.

Built in Phase 6 as `internal/terminal/authority.go` — an authority per project rather than a manager
over all of them, because the guarantee is per project and a single object holding every project's
lease would be one lock for a fact that has no cross-project meaning.

It owns two questions and answers only those:

- **may this client type** — `MayInput`;
- **may this client set the size** — `MayResize`.

They are separate methods rather than one `IsController` because the directive keeps Input Authority
and Resize Authority as separate interfaces (§十五), and because a future phase that wants to let a
viewer choose its own geometry would change one of them and not the other. The lease itself is a
`(clientId, expiresAt)` pair where `expiresAt` is set only while the holder is disconnected; the rules
are in `docs/MULTI_DEVICE.md`, and nothing about it is persisted.

### HostAdapter

All OS-specific behavior.

Initial:

```text
LinuxHostAdapter
WindowsWSLHostAdapter
```

Responsibilities:

- system information;
- path mapping;
- dependency checks;
- opening VS Code;
- configured roots.

### Provider Adapter

Future CC Switch integration.

## 4. Project identity

Display names may change.

Runtime IDs must not.

Each project receives a stable internal ID.

tmux naming:

```text
amx-{projectId}
```

Do not use project name as tmux session identity.

## 5. Path model

Store host and runtime paths separately.

Example:

```text
hostPath:
D:\AI\Projects\2026 AgentMux\AgentMux

runtimePath:
/mnt/d/AI/Projects/2026 AgentMux/AgentMux

collectionPath:
D:\AI\Projects\2026 AgentMux
```

All conversion occurs through HostAdapter/PathMapper.

## 6. Discovery model

Do not assume first-level directories are projects.

Support bounded discovery within configured roots.

Suggested depth:

```text
2–3 levels
```

Use candidate markers such as:

```text
.git
CLAUDE.md
package.json
go.mod
pyproject.toml
Cargo.toml
*.sln
```

Ignore generated/heavy directories.

Explicit registration becomes authoritative after selection.

## 7. Windows runtime

```text
Windows 11
   │
   ├─ project files on D:
   │
   └─ WSL2 Ubuntu
          │
          ├─ AgentMux Server
          ├─ tmux
          ├─ Claude Code CLI
          └─ runtime dependencies
```

AgentMux must not require VS Code.

**The server runs inside the distribution.** The process that owns tmux is the one inside WSL:

```text
Windows host                     WSL2 distribution
────────────                     ─────────────────
project files on D:  ←→ drvfs    /mnt/d/...
VS Code GUI                      AgentMux Server (linux/amd64)
browser                 ──────▶  ├─ tmux server, socket "agentmux"
                                 │  └─ pty per project session
                                 └─ runtime dependencies
```

The rejected alternative was a Windows-native server that calls `wsl.exe tmux ...` per operation. It
would put the PTY, the session lifecycle, and the ANSI byte stream on the far side of a process
boundary from the code that owns them, require a second copy of session state that can disagree with
the first, and split `SessionBackend` into two implementations of the same thing, only one of which
would be exercised day to day.

Windows keeps what is genuinely Windows': the files on `D:`, the VS Code GUI, the launch of the
distribution, and — later — a launcher.

A Windows-native server still runs and still manages projects, discovery, and the UI. It reports
`runtimeAvailable: false` with an actionable reason and serves no terminal. It does not silently
proxy. Dependency probes run in the runtime environment, not on the Windows host PATH, and
`GET /api/server` reports where each probe looked in `dependencies[].probedIn`.

The distribution name is detected at runtime (`WSL_DISTRO_NAME`, or `-runtime-distro`), never
hardcoded.

Details, including the tmux bootstrap sequence and the control-mode wire format: `docs/RUNTIME.md`.

## 8. Linux runtime

```text
Linux
  │
  ├─ AgentMux Server
  ├─ tmux
  └─ Claude Code CLI
```

Linux and Windows+WSL share one code path: the server is a Linux process in both, and it talks to a
local tmux over a private socket. `runtime.mode: native` is the same `TmuxBackend` with a different
HostAdapter.

## 9. Terminal transport

```text
pty inside tmux
→ tmux control mode ("tmux -C", %output records)
→ controlStream decoder (bytes, not text)
→ session.Manager: sequence, bounded history, subscriptions
→ terminal.Hub: frames, limits, one socket per browser
→ xterm.js
```

Clients never attach directly to tmux, and never touch a tmux socket at all. The last two stages are
Phase 4 and are described in `docs/TERMINAL.md`; the three above them are Phase 2 and are described in
`docs/RUNTIME.md`.

The direction is the load-bearing part. `terminal.Hub` may only *subscribe* to the runtime manager —
it has no tmux socket path, no session name, and no way to acquire either. A transport that opened its
own `tmux -C` connection would be a second path to the same terminal, and two paths to one terminal is
how a server ends up with a control connection per browser tab, a pane that resizes when somebody
closes a window, and output that arrives twice or not at all depending on which connection won. There
is one control connection per runtime, it belongs to the manager, and a browser is one of its readers.

The reason output is taken from tmux's control mode rather than from `capture-pane` on a timer is
recorded in `docs/RUNTIME.md` §2: polling cannot see scrollback or anything between ticks, and it
re-encodes the screen as text, losing the difference between a carriage return and a newline.
`capture-pane` is still used, but for the opposite job — a snapshot to recover with, not a stream.

## 10. Terminal state

Server-side, per runtime:

```text
projectId
sessionName        (amx-{projectId})
state              (STOPPED/STARTING/RUNNING/STOPPING/ERROR/ORPHAN)
canonicalCols
canonicalRows
outputSequence     (monotonic, per runtime)
historyBuffer      (bounded: N chunks, M bytes)
lastActivity
```

Implemented in Phase 2.

Not implemented, and not stubbed: `controllerId`, `controllerLease`, `connectedViewers`. A viewer
subscription exists as `Backend.Attach` — several may be open at once and each receives the same
bytes — and Phase 4 gave that a front end without giving it a roster. There is still no lease and no
notion of who may type. That is Phase 6.

Client-local:

```text
scroll position
font size
zoom
followOutput
grid/focus/fullscreen
```

`scroll position` and `followOutput` are Phase 4 and are held by the client, not the server — there is
no message that could carry a scroll position, and a server that knew one client's would have to pick
it over another's. `font size` and `zoom` are not implemented; the terminal fits its container rather
than being sized by hand. `grid/focus/fullscreen` is Phase 5.

## 11. WebSocket model

One main WebSocket per browser, implemented as `GET /api/ws`. Subscriptions are multiplexed on it and
the frames carry the project they belong to, so watching a second project does not mean a second
connection and a second set of failure modes.

Message categories as built:

```text
binary:  terminal output (frame type 0x01)
         terminal snapshot (frame type 0x02)
text:    hello, unsubscribed, resized, error, pong
         control.granted, control.denied, control.revoked, control.expired, control.changed
         subscribe, unsubscribe, input, resize, resync, ping
         control.request, control.release, control.accept, control.reject
```

High-volume output is batched in the runtime manager, which is the layer that knows how much is
waiting; the transport frames what it is given rather than coalescing it again.

The split between binary and text is the decision that shapes this section. Terminal output is bytes,
and a JSON envelope around a screenful of escape sequences would mean base64, a third more bytes, and
an encoding step on the hot path. Control is the opposite — rare, small, and worth being readable in a
log or in a browser's frame list. So output travels in binary frames whose header carries the project
and the range of sequence numbers it contains, and everything else is a JSON object with a `type`.

One more concept was added in Phase 6 and it is worth naming here because it cuts across the pattern
above: the **client**, as distinct from the connection. A connection is one WebSocket. A client is a
browser session — a tab, on a device — and it is named by a `clientId` the browser generates, keeps in
`sessionStorage`, and states in the handshake (`GET /api/ws?client=c_7f3a…`). It is a query parameter
rather than a message field because it has to be known before the first message: a reconnecting
client's leases are restored the moment its socket opens, which is before it has sent anything. The
`hello` the server sends back carries both — `clientId` for the session and `connectionId` for the
socket — where version 1 had one id that meant the connection. That distinction is what makes a reload
a reconnection rather than a second device, and a dropped socket an interruption rather than a
departure. It is also why §12's claim now has a second half: a reconnect restores a terminal, and it
restores a *lease*, without the client asking again — unless the reconnect took longer than the grace,
in which case the lease is gone and the client is an ordinary viewer.

`project.status` and `server.status` are not implemented, and the reason is the same for both: the UI
already learns project state from `GET /api/projects`, and there was no second consumer to justify a
push. `controller.changed` shipped in Phase 6 as `control.changed` — a roster carried alongside every
message in that family rather than a notification that something changed. `provider.changed` is
Phase 8.

A client never sends a binary frame. Raw input is bytes, but it is bytes the client is sending rather
than bytes a terminal is producing, and routing it through the same typed, size-limited, individually
rejectable channel as every other client intent is worth the base64.

The data model that makes all of this possible was already there from Phase 2 — a monotonic sequence
per runtime and a bounded history — and introducing the sequence after the fact would have been much
harder than starting with it. Phase 3 made the thing behind it worth streaming, by putting a real
Claude Code session inside the runtime. The full protocol is in `docs/TERMINAL.md`.

## 12. Reconnection

Browser disconnect must not affect runtime sessions.

```text
client reconnects
→ subscribes
→ receives snapshot/history
→ resumes live output
```

Implemented in Phase 4, with a snapshot rather than a replay. A reconnecting client is sent the
current screen and told the highest sequence number that screen already contains, and it applies
everything above that. Replay sounds cheaper, but it is only correct if the client's screen is exactly
what the server thinks it is — and the reason a client needs recovery is that it is not sure. A
snapshot cannot be wrong about what the client is showing, because it replaces it.

The same path serves a client that asks to be re-established while connected — a person pressing
"Redraw" — so there is one recovery implementation rather than two. And the server takes it
unprompted when a subscription falls behind, which is why a browser cannot normally observe a
sequence gap at all: the seam is closed before it is visible.

**The stronger version of this is also true one level down, and it predates Phase 4.** The AgentMux
server itself can stop and restart without affecting a session: tmux owns the PTYs, and it is a
separate process. On startup the server reconciles what it has recorded against what the backend
actually has and answers Case A/B/C accordingly — see §13 and `docs/RUNTIME.md` §6. A project whose
session survived is reported `RUNNING` again, without the user's work having been interrupted, and a
browser that reconnects afterwards finds the terminal where it was left.

**Phase 6 added a second thing to restore, and a rule about not restoring it.** A reconnecting client
gets its terminal back and, if it held a project's lease, gets that back too — provided it comes back
inside the grace period. If it does not, the lease lapsed while it was away, and it returns as a viewer
with no way to have known: the roster it is handed says somebody else has it, or nobody does. What it
is *not* given is a lease because it used to have one. Nothing about the previous holder survives on
the server, and a restarted server is the extreme case of that: it knows nothing at all and hands the
terminal to nobody, not even to the only device watching. Concretely — `stopServer(); startServer()`
under a live browser — is `web/e2e/suites/controller.mjs` section G, and the unit-level version is
`internal/terminal/multi_device_test.go`.

## 13. Persistence

SQLite stores metadata, not endless terminal logs.

Initial tables:

```text
projects
project_runtime
settings
collections (optional)
```

A separate `collections` table is optional; collection path may initially be stored on Project records.

Created so far: `projects`, `settings`, `schema_migrations` (the migration bookkeeping table), and —
from Phase 2 — `project_runtime`. Collection membership is a column on `projects`, which is why
registering an existing project is a single insert and finding a project's collection needs no join.

`project_runtime` stores only what cannot be answered after a restart: the owning backend, the session
name, the user's intent, the canonical size, and the timestamps. It stores **no terminal output at any
granularity**, no controller lease, no viewer, no scroll position, and no `is_running` boolean —
liveness is asked of the runtime, which is the only thing that knows. The full list of what is stored
and what is deliberately not is the header of `migrations/0002_project_runtime.sql` and
`docs/RUNTIME.md` §6.

Phase 4 added a live terminal and did not add a table. A snapshot is taken from tmux when it is asked
for and is not written down; the browser keeps the screen in memory and nothing else, so nothing about
a terminal reaches `localStorage` or the database. That is the same rule as before, applied to a
feature that would have made it easy to break.

Migrations are embedded SQL files applied in order and recorded by name, so a future schema change is
a new numbered file rather than an edit to an applied one.

## 14. Security

Do not expose provider secrets.

**Terminal input is not a command channel.** There is no message in the WebSocket protocol that runs a
command: a browser may send raw keystrokes to a terminal it has already subscribed to, and those bytes
go to whatever is already running in the pane, exactly as typing would. Nothing accepts a filesystem
path from a client either — a subscribe names a `projectId`, which the server resolves against the
projects it already knows.

**A page from another origin cannot open a terminal.** The WebSocket handshake is checked against the
request's own host and against `server.allowedOrigins`; a missing `Origin` is allowed because browsers
always send one, so a request without one is a program rather than a page. This is the check that stops
a site the user is visiting from connecting to a local AgentMux server.

**Nothing sensitive is logged.** Terminal output, raw input, and the contents of a Prompt Bar prompt are
never written to the log at any level, in either direction. The permitted fields are listed in
`docs/TERMINAL.md` §13.

**The control path has its own, narrower rule, and it is narrower on purpose.** §三十四 of the phase
directive allows exactly four fields on a control log record — `clientId`, `projectId`, `event`,
`timestamp` — and `internal/terminal` writes no others. `event` is the record's own message, from a
closed vocabulary of plain phrases ("control granted", "control suspended while the controller is
away", "control expired"); the timestamp is the logger's; and the identifier is a random string rather
than a credential, so a line is a sentence about a lease and never a sentence about a person.
`TestALogRecordCarriesOnlyTheFourFields` walks the whole control path — grant, queue, refusal,
declined handover, accepted handover, release, suspension, resume and lapse — and fails the build if a
fifth field appears. That rule breaks in practice not by decision but by somebody adding the thing
that would have been useful while debugging, which is why the test exists rather than the intention.

**A device is named, not identified.** The roster carries a label derived server-side from the
User-Agent header, from a closed vocabulary (`Chrome on Windows`, `Safari on iPad`), and an
unrecognised header becomes `Unknown device` rather than being passed through. The interface shows that
label; it never shows an address, and there is no field in any message that could carry one.

Recommended early deployment:

```text
Tailscale/private network
→ AgentMux
```

The terminal endpoint has no authentication of its own — it is as reachable as the server is — so the
network boundary above is what keeps it private, and that is a deployment decision rather than
something this layer decides.
