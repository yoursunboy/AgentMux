# AgentMux Architecture

## 1. High-level architecture

```text
iPad / Phone / PC
        │
        │ HTTPS + WebSocket      (WebSocket is Phase 4; this build is REST only)
        ▼
┌─────────────────────────────┐
│       AgentMux Server       │   runs inside WSL on Windows (see §7)
│                             │
│ Project Manager             │   internal/project        — built
│ Session Manager             │   internal/session        — built in Phase 2
│ Terminal Manager            │   part of session.Manager — sequence, history
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

Not yet owned here: controller leases, viewer lists, and scroll positions. Those are properties of
live connections and belong to Phase 4's WebSocket layer.

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
bytes. Controller leases, viewer roles, and scroll are Phase 6 and are not stubbed.

### Controller Manager

Guarantees one input controller per project.

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
→ (Phase 4: WebSocket)
→ (Phase 4: xterm.js)
```

Clients never attach directly to tmux, and — from Phase 4 — never touch a tmux socket at all.

Phase 2 implements the first three stages and stops there. There is no WebSocket and no xterm.js.
The reason output is taken from tmux's control mode rather than from `capture-pane` on a timer is
recorded in `docs/RUNTIME.md` §2: polling cannot see scrollback or anything between ticks, and it
re-encodes the screen as text, losing the difference between a carriage return and a newline.

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
bytes — but there is no roster, no lease, and no notion of who may type. That is Phase 6.

Client-local:

```text
scroll position
font size
zoom
followOutput
grid/focus/fullscreen
```

All Phase 4 or later, and all held by the client, not the server.

## 11. WebSocket model

Prefer one main WebSocket per browser.

Logical message categories:

```text
terminal.output
terminal.input
terminal.resize
project.status
controller.acquire
controller.release
controller.changed
server.status
provider.changed
```

Batch high-volume output.

**Not implemented yet; this is Phase 4.** No WebSocket endpoint exists and nothing streams to a
browser. What the phases so far provide is the data model that makes it possible: every output chunk
carries a monotonic `sequence` per runtime, and a bounded history is kept, so a reconnecting client can
be given what it missed rather than a redraw of the current screen. Introducing the sequence after the
fact is much harder than starting with it — and Phase 3 made the thing behind it worth streaming, by
putting a real Claude Code session inside the runtime.

## 12. Reconnection

Browser disconnect must not affect runtime sessions.

```text
client reconnects
→ subscribes
→ receives snapshot/history
→ resumes live output
```

**The stronger version of this is already true in Phase 2, one level down.** The AgentMux server
itself can stop and restart without affecting a session: tmux owns the PTYs, and it is a separate
process. On startup the server reconciles what it has recorded against what the backend actually has
and answers Case A/B/C accordingly — see §13 and `docs/RUNTIME.md` §6. A project whose session
survived is reported `RUNNING` again, without the user's work having been interrupted.

The browser-facing half of this flow — subscribe, snapshot, resume — is Phase 4.

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

Migrations are embedded SQL files applied in order and recorded by name, so a future schema change
is a new numbered file rather than an edit to an applied one.

## 14. Security

Do not expose provider secrets.

Recommended early deployment:

```text
Tailscale/private network
→ AgentMux
```
