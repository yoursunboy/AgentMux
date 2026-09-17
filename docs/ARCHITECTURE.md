# AgentMux Architecture

## 1. High-level architecture

```text
iPad / Phone / PC
        │
        │ HTTPS + WebSocket
        ▼
┌─────────────────────────────┐
│       AgentMux Server       │
│                             │
│ Project Manager             │
│ Session Manager             │
│ Terminal Manager            │
│ Controller Manager          │
│ Provider Adapter            │
│ Host Adapter                │
│ Storage                     │
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
     Claude    Claude    Claude
```

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

### SessionBackend

Abstract runtime interface.

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

### Terminal Manager

Owns:

- canonical terminal size;
- output stream;
- output sequence;
- history/ring buffer;
- viewer subscriptions;
- controller ID/lease;
- input routing.

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

Where the server actually runs: this diagram shows the target, with the server inside the runtime
where tmux and the CLI live. The Phase 1 server runs on the Windows host instead, and reaches the
runtime only through the HostAdapter, which maps every host path to its runtime path. That choice
keeps a drive letter out of the business logic and lets `runtime.mode: native` run the same code on a
Linux host. It is a deliberate starting point, not a settled answer: when the session runtime lands
in Phase 2, the process that owns tmux has to be the one inside WSL, and this section is where that
decision is recorded.

## 8. Linux runtime

```text
Linux
  │
  ├─ AgentMux Server
  ├─ tmux
  └─ Claude Code CLI
```

## 9. Terminal transport

```text
Claude
→ tmux pane
→ tmux integration/control layer
→ Terminal Manager
→ WebSocket
→ xterm.js
```

Clients never attach directly to tmux.

## 10. Terminal state

Server-side:

```text
projectId
canonicalCols
canonicalRows
outputSequence
historyBuffer
controllerId
controllerLease
connectedViewers
lastActivity
```

Client-local:

```text
scroll position
font size
zoom
followOutput
grid/focus/fullscreen
```

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

## 12. Reconnection

Browser disconnect must not affect runtime sessions.

```text
client reconnects
→ subscribes
→ receives snapshot/history
→ resumes live output
```

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

What Phase 1 creates: `projects`, `settings`, and `schema_migrations` (the migration bookkeeping
table). There is no `project_runtime` table and no `collections` table. Collection membership is a
column on `projects`, which is why registering an existing project is a single insert and finding a
project's collection needs no join. `project_runtime` arrives with the session runtime in Phase 2,
when there is runtime state worth writing down.

Migrations are embedded SQL files applied in order and recorded by name, so a future schema change
is a new numbered file rather than an edit to an applied one.

## 14. Security

Do not expose provider secrets.

Recommended early deployment:

```text
Tailscale/private network
→ AgentMux
```
