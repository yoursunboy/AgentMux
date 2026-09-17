# AgentMux HTTP API — Phase 2

This is the surface the Phase 2 server actually serves, with the shapes it actually returns. Every
example below was produced by running the server and calling it; none of it is aspirational. The
target for later phases is in `docs/PROTOCOL.md`.

Base path: `/api`. The server binds `127.0.0.1:8787` by default and serves the built frontend from
the same origin, so the browser only ever talks to one host.

Two facts about Phase 2 shape the whole document. The first is that **the server runs where the
sessions run**: on Windows that means inside WSL, and `/api/server` says which side it is on. The
second is that **there is no terminal stream yet**. Sessions are real, input and output are real, but
the endpoints that carry raw terminal bytes are diagnostics under `/api/debug`, off by default, and
the browser does not call them. Phase 4 replaces them with a WebSocket.

## GET /api/server

Server identity, host and runtime description, capability flags, and the configuration summary.
Contains no credentials, no tokens, and no provider keys.

This is a server running inside WSL, where the runtime works:

```json
{
  "appName": "AgentMux",
  "version": "0.1.0",
  "phase": "Phase 2 - Persistent session runtime",
  "status": "online",
  "startedAt": "2026-09-17T13:31:14Z",
  "uptimeSeconds": 0,
  "host": "linux",
  "hostArch": "amd64",
  "runtimeMode": "native",
  "runtimeOs": "linux",
  "distro": "Ubuntu-24.04",
  "pathMapper": "native",
  "environment": "wsl",
  "runtimeAvailable": true,
  "projectsRoot": "/tmp/amx-cap/projects",
  "projectsRoots": ["/tmp/amx-cap/projects"],
  "discoveryDepth": 3,
  "dataDirectory": "/tmp/amx-cap/data",
  "databasePath": "/tmp/amx-cap/data/agentmux.db",
  "webDirectory": "/mnt/d/AI/projects/2026 AgentMux/web/dist",
  "terminalRuntimeImplemented": true,
  "dependencies": [
    {
      "name": "git",
      "available": true,
      "path": "/usr/bin/git",
      "required": false,
      "probedIn": "wsl",
      "note": "Used when a new project is created with Initialize Git. Runs where the server runs."
    },
    {
      "name": "tmux",
      "available": true,
      "path": "/usr/bin/tmux",
      "required": true,
      "probedIn": "wsl",
      "note": "The persistent terminal runtime. Sessions outlive the AgentMux server."
    },
    {
      "name": "claude",
      "available": true,
      "path": "/mnt/c/Users/you/AppData/Roaming/npm/claude",
      "required": false,
      "probedIn": "wsl",
      "note": "Claude Code CLI. Required from Phase 3; this build never starts it."
    },
    {
      "name": "/bin/bash",
      "available": true,
      "path": "/bin/bash",
      "required": false,
      "probedIn": "wsl",
      "note": "The shell a terminal session runs."
    }
  ],
  "provider": { "tool": "claude", "integrated": false, "status": "not_integrated" },
  "features": {
    "projectRegistration": true,
    "projectCreation": true,
    "projectDiscovery": true,
    "gitInit": true,
    "terminal": true,
    "providerSwitch": false,
    "claudeHooks": false,
    "controllerTransfer": false
  },
  "warnings": []
}
```

Read it in this order, because each field answers a different question and a client that skips one
will offer something the server cannot do:

- `environment` is where **the server process itself** is running: `windows`, `wsl`, or `linux`.
- `host` and `runtimeMode` describe the machine it is running on. On this example the process is a
  Linux process on a Linux host, and `environment` is `wsl` because that Linux is a distribution.
- `runtimeAvailable` is whether a persistent terminal runtime can execute **here**. It is false on a
  Windows-native server no matter what is installed anywhere, because a Windows process cannot own a
  Linux process tree.
- `runtimeUnavailableReason` says what to do about it when it is false. Absent when `runtimeAvailable`
  is true.
- `terminalRuntimeImplemented` is a statement about the **build**: whether this binary contains the
  runtime at all. A build with the runtime on a Windows host has this true and `runtimeAvailable`
  false, and both facts matter.
- `features.terminal` is the single answer a client should act on. It is true only when the build has
  the runtime, the server is on the right side of the WSL boundary, **and** tmux is installed where
  sessions would run.
- `terminalBlocker` is why a terminal cannot be offered here, in one field, empty when it can be. It
  is the same sentence that appears in `warnings`, promoted so a client does not reconstruct the
  diagnosis by searching a list — the machine with two problems at once is where that goes wrong.
- `dependencies[].probedIn` names the environment each probe **actually inspected**, which is not
  always the one the user is typing in. A probe reports what it found there, and reports a probe it
  could not run as a note rather than as "not installed".

The same endpoint on a Windows-native server, where the runtime is deliberately unavailable:

```json
{
  "environment": "windows",
  "runtimeAvailable": false,
  "runtimeUnavailableReason": "The terminal runtime needs the AgentMux server to run inside WSL, because tmux and the coding agent it hosts are Linux processes. Start the server from inside your distribution instead, for example: wsl -d <distribution> -- ./agentmux-server. Project management and diagnostics work either way.",
  "terminalRuntimeImplemented": true,
  "terminalBlocker": "The terminal runtime needs the AgentMux server to run inside WSL, because tmux and the coding agent it hosts are Linux processes. Start the server from inside your distribution instead, for example: wsl -d <distribution> -- ./agentmux-server. Project management and diagnostics work either way.",
  "dependencies": [
    {
      "name": "git",
      "available": true,
      "path": "C:\\Program Files\\Git\\cmd\\git.exe",
      "required": false,
      "probedIn": "windows",
      "note": "Used when a new project is created with Initialize Git. Runs where the server runs."
    },
    {
      "name": "tmux",
      "available": true,
      "path": "/usr/bin/tmux",
      "required": true,
      "probedIn": "wsl:Ubuntu-24.04",
      "note": "The persistent terminal runtime. Sessions outlive the AgentMux server."
    }
  ],
  "features": { "terminal": false },
  "warnings": [
    "The terminal runtime needs the AgentMux server to run inside WSL, because tmux and the coding agent it hosts are Linux processes. Start the server from inside your distribution instead, for example: wsl -d <distribution> -- ./agentmux-server. Project management and diagnostics work either way."
  ]
}
```

Two things in that example are the point of it. `tmux` is reported as available with a path **inside
WSL**, because that is where the probe looked and that is what it found — the field is a fact about
the distribution, not a claim about what this server can do. And `features.terminal` is still false,
because the server is on the wrong side of the boundary. Nothing here invites a user to install
anything: the fix is to start the server somewhere else.

## GET /api/projects

Registered projects, ordered by name. `?includeArchived=true` also returns archived ones.

```json
{
  "projects": [
    {
      "id": "p_7346c8fa4633a9e7b12a",
      "name": "DemoApp",
      "hostPath": "D:\\AI\\Projects\\2026 Demo Group\\DemoApp",
      "runtimePath": "/mnt/d/AI/Projects/2026 Demo Group/DemoApp",
      "collectionPath": "D:\\AI\\Projects\\2026 Demo Group",
      "status": "stopped",
      "pinnedSlot": null,
      "archived": false,
      "createdAt": "2026-09-17T08:59:53.5117104Z",
      "updatedAt": "2026-09-17T08:59:53.5117104Z",
      "lastOpenedAt": null
    }
  ],
  "count": 1
}
```

`collectionPath` is the group folder directly containing the project, and is `""` for a project
sitting directly under a Projects Root.

`status` is derived on every read and never stored. It is a projection of the runtime's state:
`"stopped"`, `"running"`, or `"reconnecting"` while a session is alive but its output stream is being
re-established. A client that shows `running` for a runtime it cannot read output from would be
claiming a terminal that is not there.

## GET /api/projects/{id}

One project by identifier, in the same shape:

```json
{ "project": { "id": "p_7346c8fa4633a9e7b12a", "name": "DemoApp", "...": "..." } }
```

An unknown identifier returns `404 project_not_found`.

## GET /api/projects/discover

Scans the configured Projects Roots. Read-only: it walks directories, reads marker filenames, and
writes nothing. Nothing found here is registered — the result is a suggestion list.

Optional query parameter `depth` (1–8, default from configuration) overrides the configured depth
for this call.

```json
{
  "roots": [{ "path": "D:\\AI\\Projects", "exists": true }],
  "candidates": [
    {
      "name": "AgentMux",
      "hostPath": "D:\\AI\\Projects\\2026 AgentMux\\AgentMux",
      "runtimePath": "/mnt/d/AI/Projects/2026 AgentMux/AgentMux",
      "collectionPath": "D:\\AI\\Projects\\2026 AgentMux",
      "projectsRoot": "D:\\AI\\Projects",
      "depth": 2,
      "markers": [".git", "CLAUDE.md", "go.mod"],
      "score": 6,
      "confidence": "high",
      "nameDiscounted": false,
      "registered": false
    }
  ],
  "warnings": [],
  "scannedDirectories": 133,
  "truncated": false,
  "durationMs": 23
}
```

Reading the result:

- `depth` is how far below the root the folder sits, so `2` means it lives inside a collection.
- `markers` are the files or directories that identify it, such as `.git`, `CLAUDE.md`,
  `package.json`, or `go.mod`. Git is not required.
- `confidence` follows `score`: `high` at 5 and above, `medium` at 3 and above, `low` below that.
- `nameDiscounted` means the folder's own name (`Reference`, `Design`, `Notes`, `Archive`,
  `Documents`, `Screenshots`) lowered its rank. It lowers the score and can push a candidate to the
  bottom of the list, but it never removes one: a folder that qualifies on its own evidence is still
  suggested.
- `registered` is true when the candidate is already a project, and `projectId` names it.
- `truncated` means the scan stopped at the configured candidate or directory budget, so the list is
  incomplete by design rather than by failure.
- `warnings` reports a root that could not be scanned. One unreadable root does not fail the call.

Generated and vendored directories (`node_modules`, `dist`, `build`, `target`, `vendor`, `.venv`,
`venv`, `.cache`, `.git`) are never descended into.

## POST /api/projects/register

Adopts a directory that already exists. AgentMux never moves or copies it.

```json
{ "hostPath": "D:\\AI\\Projects\\2026 Demo Group\\DemoApp", "name": "Optional display name" }
```

`name` is optional and defaults to the folder's own name. Returns `201` with `{"project": {...}}`.

Registering the same folder twice does not create a second project. The second call returns:

```json
{
  "error": {
    "code": "project_already_registered",
    "message": "D:\\AI\\Projects\\App is already registered as \"App\"",
    "details": {
      "hostPath": "D:\\AI\\Projects\\App",
      "projectId": "p_7346c8fa4633a9e7b12a",
      "project": { "id": "p_7346c8fa4633a9e7b12a", "...": "..." }
    }
  }
}
```

with status `409`. The existing project is carried in `details` so a client can open what is already
there rather than only reporting a clash. The path is compared case-insensitively on Windows.

## POST /api/projects

Creates a new project directory. The server, not the client, decides the final path.

```json
{
  "name": "NewApp",
  "collectionPath": "D:\\AI\\Projects\\New Group",
  "projectsRoot": "D:\\AI\\Projects",
  "initGit": true
}
```

- `name` is required. It becomes the folder name, and is rejected rather than sanitised: path
  separators, `<>:"/\|?*`, control characters, leading or trailing dots, leading or trailing
  whitespace, Windows device names, and anything over 64 characters all fail with
  `400 invalid_project_name`. `..\..\Windows` is rejected as an illegal character.
- `collectionPath` is optional and must be an existing directory inside a Projects Root, because
  AgentMux never invents parent folders. Omit it to create the project directly under a root.
- `projectsRoot` selects a root when `collectionPath` is omitted; the first configured root is the
  default.
- `initGit` is optional and defaults to false. Git is initialised in the new project directory and
  never in the collection.

Returns `201` with `{"project": {...}}`.

If `initGit` fails, no project is registered, and the response says what happened to the directory:

```json
{
  "error": {
    "code": "git_init_failed",
    "message": "git init failed in D:\\AI\\Projects\\App: ...",
    "details": { "rolledBack": true }
  }
}
```

`rolledBack: true` means this request created the directory and then removed it again.
`rolledBack: false` means the directory already existed and was left exactly as it was.

## The runtime resource

A project's runtime lives at `/api/projects/{id}/runtime`, nested rather than a top-level
`/api/runtimes/{id}`. The nesting is deliberate: a runtime is not an object with its own identity
that a client looks up — its identity *is* its project, and `projectId` is the only key it has ever
had. A URL that reads as "this project's runtime" is the shape of the data.

Four endpoints, and the difference between the last two is the thing most worth getting right:

| Call | Effect |
| --- | --- |
| `GET /api/projects/{id}/runtime` | The current state. Never starts anything. |
| `POST /api/projects/{id}/runtime/start` | Brings the runtime up. Idempotent. |
| `POST /api/projects/{id}/runtime/stop` | Ends the program in the session and **keeps the session**. |
| `DELETE /api/projects/{id}/runtime` | Ends the session, its scrollback, and everything in it, and forgets the record. Irreversible. |

**Stop is not Destroy.** Stop is the equivalent of `Ctrl-C`: the work stops, the terminal, its
scrollback, and its shell stay exactly where they were, and the runtime reports `STOPPED` with
`sessionAlive: true`. Destroy is the equivalent of closing the window and throwing it away. A user
who has confused the two has lost work, which is why they are two buttons and not one.

### GET /api/projects/{id}/runtime

For a project that has never been started:

```json
{
  "runtime": {
    "projectId": "p_e668538cbdd903cbe5bc",
    "backend": "tmux",
    "session": "amx-p_e668538cbdd903cbe5bc",
    "state": "STOPPED",
    "sessionAlive": false,
    "cols": 120,
    "rows": 30,
    "sequence": 0,
    "startedAt": "0001-01-01T00:00:00Z",
    "updatedAt": "2026-09-17T21:31:15.09027614+08:00"
  }
}
```

A never-started project is **not** a 404. The runtime resource exists and is stopped, which gives a
client one shape to render instead of two. An unknown *project* is a 404, at every runtime endpoint.

`state` is one of `STOPPED`, `STARTING`, `RUNNING`, `STOPPING`, `ERROR`, `ORPHAN`. `ORPHAN` marks a
session that exists with no project behind it — reported, never killed, because it may hold work
somebody needs. There is no `WAITING` and no `COMPLETED`: AgentMux does not claim to know what the
program inside a session is doing, and inventing those states would be claiming exactly that.

`session` is `amx-{projectId}`, never the project name. Renaming a project does not move its session,
and two projects that happen to share a name cannot collide.

`cols` and `rows` are the canonical size. They survive a restart, so the next start is the size the
user last chose rather than a default.

`sequence` is the runtime's output counter: a per-runtime monotonic number, not a byte count. A
client that reconnects asks for what is newer than the last sequence it saw.

### POST /api/projects/{id}/runtime/start

```json
{
  "runtime": {
    "projectId": "p_e668538cbdd903cbe5bc",
    "backend": "tmux",
    "session": "amx-p_e668538cbdd903cbe5bc",
    "state": "RUNNING",
    "sessionAlive": true,
    "cols": 120,
    "rows": 30,
    "sequence": 0,
    "startedAt": "2026-09-17T21:31:15+08:00",
    "updatedAt": "2026-09-17T21:31:15.195230943+08:00"
  }
}
```

The session's working directory is always the project's `runtimePath` — the path on the side the
server runs on — never the Projects Root and never the collection. A terminal that opens one
directory too high is worse than no terminal, so this is a tested property rather than an intention.

Starting a runtime that is already running returns what is already there. A user who reloads a page
and clicks a button twice must not end up with two terminals writing to the same repository.

### POST /api/projects/{id}/runtime/stop

```json
{
  "runtime": {
    "projectId": "p_e668538cbdd903cbe5bc",
    "backend": "tmux",
    "session": "amx-p_e668538cbdd903cbe5bc",
    "state": "STOPPED",
    "sessionAlive": true,
    "cols": 120,
    "rows": 30,
    "sequence": 1,
    "startedAt": "2026-09-17T21:31:15+08:00",
    "updatedAt": "2026-09-17T21:31:15.293979767+08:00"
  }
}
```

`state: "STOPPED"` with `sessionAlive: true` is the whole distinction from Destroy in one line: the
work has stopped and the terminal has not gone anywhere.

### DELETE /api/projects/{id}/runtime

```json
{
  "runtime": {
    "projectId": "p_e668538cbdd903cbe5bc",
    "backend": "tmux",
    "session": "amx-p_e668538cbdd903cbe5bc",
    "state": "STOPPED",
    "sessionAlive": false,
    "cols": 120,
    "rows": 30,
    "sequence": 0,
    "startedAt": "0001-01-01T00:00:00Z",
    "updatedAt": "2026-09-17T21:31:15.367120956+08:00"
  }
}
```

The session is really gone, and the endpoint is idempotent: destroying a runtime that is not there
succeeds. The response is the runtime as it now is, so a client has one shape to render after every
one of the four calls rather than four.

## Diagnostic endpoints (`/api/debug`)

**These are not a product API.** They exist because Phase 2 has no terminal stream, and the runtime
has to be usable and verifiable by something. They are registered only when the server is started
with `-debug-api`, so an ordinary installation has no route that accepts raw terminal input at all —
registering them and refusing inside the handler would leave the surface present and one flag away
from live. When the flag is absent the paths are not routed and answer `404 not_found`, exactly like
any other unknown path.

They are unauthenticated, like the rest of the Phase 2 API, and they do not belong on a machine
anyone else can reach. They are expected to be deleted or explicitly marked debug-only once Phase 4
lands a real terminal; if they are still here unmarked after that, that is a bug.

### POST /api/debug/projects/{id}/runtime/input

Sends input to a session's terminal. Four fields, and which one you use is the whole question:

- `keys` is a list of **command lines**, each typed into the shell and then run.
- `text` is a string delivered as its UTF-8 bytes, with no carriage return.
- `bytes` is a base64 string delivered exactly as given.
- `enter` appends a carriage return to whichever of `text` or `bytes` was given.

```json
{ "keys": ["printf \"\\033[32mgreen\\033[0m 中文\\n\""] }
```

```json
{ "bytes": "Aw==" }
```

Giving `text` and `bytes` in one body is a `400 invalid_request` rather than a guess. They are two
fields, not a sequence, so their order is undefined, and an endpoint that silently picked one would
send whoever wrote the call looking in the wrong place.

The response is the runtime, so an input call doubles as a state check.

`text` and `bytes` exist as separate fields on purpose. Text is what a person types; bytes are what a
terminal receives, and a caller that wants to deliver an escape sequence, a control character, or a
byte that is not valid UTF-8 has to be able to say so exactly. `Aw==` is a literal `0x03`, not a
Ctrl-C that a text layer decided about. Merging them into one string field would quietly restrict the
runtime to the subset of input that survives a round trip through JSON text, and the runtime has no
concept of a prompt, a command, or a message — it carries bytes, and the endpoint is named for what
it carries.

### GET /api/debug/projects/{id}/runtime/output?since=<sequence>

```json
{
  "projectId": "p_f6f7223e1d32eaecfe36",
  "sequence": 3,
  "chunks": [
    {
      "projectId": "p_f6f7223e1d32eaecfe36",
      "sequence": 1,
      "data": "cHJpbnRmICJcMDMzWzMybWdyZWVuXDAzM1swbSDUuK3mlodcbiINCg==",
      "timestamp": "2026-09-17T21:31:43.601048118+08:00"
    }
  ]
}
```

`chunks` are the chunks newer than `since`, oldest first, and `sequence` is the newest one included.
Each chunk's `data` is base64 of the **exact bytes the terminal produced**. Nothing is stripped,
trimmed, re-encoded, or normalised: ANSI escapes are in there, `\r` and `\n` are distinguished, a
progress bar redrawn in place arrives as its own chunks in the order it was drawn, and invalid UTF-8
stays invalid. A client that wants plain text does that conversion, on purpose, where it can see it
happening.

Output is buffered in memory and bounded. It is not written to SQLite, and it does not survive a
server restart — the pane's own scrollback in tmux does, which is what a reconnecting client gets.

### POST /api/debug/projects/{id}/runtime/resize

```json
{ "cols": 100, "rows": 30 }
```

This changes the real PTY, not a number in a record: a program inside the session that asks with
`tput cols` reports the new width. A size that cannot be used returns `400 invalid_terminal_size`
rather than being clamped to something that can.

### GET /api/debug/runtimes

Every session on AgentMux's socket, and every session on it that no project claims.

```json
{
  "backend": "tmux",
  "sessions": [
    {
      "name": "amx-p_b5492c6f2c2a15c70cc3",
      "dir": "/tmp/amx-cap2/projects/no-runtime",
      "cols": 120,
      "rows": 30,
      "createdAt": "2026-09-17T21:31:44+08:00"
    }
  ],
  "orphans": [
    {
      "session": "amx-orphan77",
      "dir": "/tmp/amx-cap2/projects",
      "cols": 80,
      "rows": 24,
      "projectId": "orphan77"
    }
  ]
}
```

### POST /api/debug/reconcile

Runs the startup reconciliation on demand and reports what it found:

```json
{
  "Running": ["p_b5492c6f2c2a15c70cc3"],
  "Stopped": ["p_f6f7223e1d32eaecfe36"],
  "Orphans": [
    {
      "session": "amx-orphan77",
      "dir": "/tmp/amx-cap2/projects",
      "cols": 80,
      "rows": 24,
      "projectId": "orphan77"
    }
  ]
}
```

The keys are Go field names because the report is a Go struct being printed, and this is a
diagnostic. It answers three questions that a restart has to answer, and the answer to all three is
deliberately conservative:

- **Running** — the record says the project has a runtime and the session is really there. Adopt it.
- **Stopped** — the record exists and the session does not. Report `STOPPED` and **start nothing**. A
  server restart is not a request to resume work.
- **Orphans** — a session in AgentMux's namespace with no project behind it. Report it and **leave it
  running**. It may hold work somebody needs, and killing it would be a guess.

## Error envelope

Every failure has the same shape:

```json
{ "error": { "code": "path_not_found", "message": "…", "details": { } } }
```

`code` is a stable string a client switches on. `message` is written to be shown to a user.
`details` is present only when there is structured context.

| Code | HTTP | Meaning |
| --- | --- | --- |
| `invalid_request` | 400 | A body that cannot be decoded, or one carrying two fields whose order is undefined. |
| `invalid_input` | 400 | Malformed request body, for example a missing `hostPath`. |
| `invalid_project_name` | 400 | The name cannot be used as a directory name. |
| `invalid_terminal_size` | 400 | A terminal size that cannot be used, such as `0x0`. |
| `path_not_a_directory` | 400 | The path is a file. |
| `path_is_projects_root` | 400 | The path is a Projects Root itself, not a project inside one. |
| `path_not_found` | 404 | The directory does not exist. |
| `project_not_found` | 404 | No project with that identifier. |
| `runtime_not_found` | 404 | The runtime resource does not exist. |
| `not_found` | 404 | No such endpoint. |
| `path_not_accessible` | 403 | The directory exists but cannot be read. |
| `path_outside_projects_root` | 403 | The path is not inside any configured Projects Root. |
| `project_already_registered` | 409 | That directory is already a project. |
| `path_already_exists` | 409 | The create target exists and is not empty. |
| `runtime_already_running` | 409 | Start was called for a runtime that is already running. |
| `runtime_not_running` | 409 | Input, resize, or stop was called for a runtime that is not running. |
| `runtime_path_mapping_failed` | 422 | The host path has no runtime equivalent. |
| `runtime_unavailable` | 503 | This server cannot host a terminal runtime at all. |
| `runtime_backend_unavailable` | 503 | The backend exists but cannot run here — tmux is missing. |
| `git_unavailable` | 503 | Git is not on PATH. |
| `git_init_failed` | 500 | `git init` ran and failed. |
| `runtime_start_failed` | 500 | The session could not be created. |
| `runtime_stop_failed` | 500 | The session could not be stopped. |
| `runtime_destroy_failed` | 500 | The session could not be removed. |
| `runtime_input_failed` | 500 | Input could not be delivered. |
| `runtime_resize_failed` | 500 | The terminal could not be resized. |
| `runtime_backend_failure` | 500 | The backend failed in a way it did not classify. |
| `storage_failure` | 500 | The metadata store could not complete the request. |

`runtime_not_running` is a `409` and not a `404`: the runtime exists, and it is in a state that makes
the call meaningless. The two are different bugs to a client, and only one of them is worth retrying.

## Not implemented

There is no WebSocket, no terminal stream, no xterm.js, no prompt bar, and no provider switching.
Starting Claude Code, resuming a Claude session, Hooks, controller leases, and Waiting/Completed
state detection are all Phase 3 or later; `docs/PROTOCOL.md` sections 4 to 13 describe the agreed
design for them, and none of them answers today. Phase 3 must not be started from this document:
what exists here is what Phase 2 built, and `docs/ROADMAP.md` is where the next phase is defined.
