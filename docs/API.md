# AgentMux HTTP API

This is the surface the server actually serves, with the shapes it actually returns. Every example
below was produced by running the server and calling it; none of it is aspirational. The target for
later phases is in `docs/PROTOCOL.md`.

Base path: `/api`. The server binds `127.0.0.1:8787` by default and serves the built frontend from
the same origin, so the browser only ever talks to one host.

Two facts shape the whole document. The first is that **the server runs where the sessions run**: on
Windows that means inside WSL, and `/api/server` says which side it is on. The second is that **the
terminal stream is not HTTP**. Sessions are real, input and output are real, and since Phase 3 a
project's runtime can host the real Claude Code CLI — but the bytes that carry a terminal travel over
the WebSocket at `/api/ws`, documented in `docs/TERMINAL.md`. This document covers the REST
surface only, and the diagnostic endpoints that used to stand in for the terminal are gone: `/api/debug`
is not routed, not flag-gated, and answers `404 not_found` on an ordinary build and on every other one.

## Requests that change something have to say where they came from

This applies to every endpoint below, so it is stated once, here.

**A request that carries an `Origin` header from somewhere other than this server, and is not a plain
read, is refused with `403 forbidden`.** A read — `GET`, `HEAD`, `OPTIONS` — from another origin is
still answered, and still not readable by the caller, which is what CORS is for.

The rule exists because the CORS header alone protects only the reply. Withholding
`Access-Control-Allow-Origin` stops another page from reading what this server said, and does nothing
about the request arriving: a `POST` with a simple content type — `text/plain` carrying a JSON body —
needs no preflight, so the browser delivers it and the server acts on it. Measured against a running
installation before this check existed, a page on `https://evil.example` registered a project and got
a `201 Created`.

What is accepted, and why each case is here:

| The request | Answered | Why |
| --- | --- | --- |
| No `Origin` at all | yes | Not a browser. `curl`, the recovery script, the tests and every native client send none, and requiring one would refuse all of them. |
| `Origin` matching the `Host` it was sent to | yes | The UI's own case, and the reason the check compares against the Host rather than a loopback list: a browser that reached this server at the address of the machine is not a loopback origin and is still this server's own page. |
| `Origin` listed in `server.allowedOrigins` | yes | An operator who names another origin means it. |
| Anything else, on a method that is not `GET`/`HEAD`/`OPTIONS` | **no — `403 forbidden`** | The case the rule is for. |
| Anything else, on a read | yes, unreadable | There is no effect to protect, and refusing would break a listed origin for no gain. |

It is the same policy the terminal socket applies before it upgrades, from the same function in the
server. A browser can reach this server two ways, and a check made at one of them is a check with a
way around it.

## GET /health

**Not under `/api`, and that is deliberate.** It is not part of the product's API surface, it is not
versioned with it, and a monitor should not have to track the protocol version to ask whether the
process is alive.

```json
{"status":"ok","version":"0.6.5","commit":"31e0f208a3aa","runtime":"available"}
```

| Field | Meaning |
| --- | --- |
| `status` | Liveness. `ok` when the process is serving. |
| `version` | The version this binary was built as. |
| `commit` | The git revision it was built from, or absent when it was built without the linker flags — the honest answer rather than a placeholder. |
| `runtime` | Readiness. `available` when a terminal runtime can run here, `unavailable` when it cannot — which is a working server that manages projects and hosts no terminals. |

**It always returns 200.** A supervisor, a load balancer with no body parser, or a `curl` in a health
check should not have to read a body to learn that a process is alive; `runtime` is what says whether
it is useful. It returns no credential, no filesystem detail and no secret — nothing that would make
an unauthenticated endpoint worth probing for more than this.

## GET /api/server

Server identity, host and runtime description, capability flags, and the configuration summary.
Contains no credentials, no tokens, and no provider keys.

Since Phase 6.5 the response also carries `runtimeBackend` (the backend name, `tmux`), `tmuxAvailable`
(whether a terminal runtime can run here), and a `version` and `phase` that name this build. Four
fields — `dataDirectory`, `databasePath`, `configFile` and `webDirectory` — are **withheld by
default** and appear only when debug mode is on (`-debug`, `AGENTMUX_DEBUG`, or `server.debug`). Debug
is deliberately separate from `logging.level`: an operator who turned up log verbosity has not thereby
decided to publish a map of the disk from an endpoint that has no authentication.
`docs/SECURITY.md` §7 is the full statement.

This is the installed server, running as a systemd service inside WSL, with the frontend served from
the same origin. It is one call to a real deployment, not a mock-up:

```json
{
  "appName": "AgentMux",
  "version": "0.6.5",
  "phase": "Phase 6.5 - Stabilization",
  "status": "online",
  "startedAt": "2026-09-19T01:16:03Z",
  "uptimeSeconds": 239,
  "host": "linux",
  "hostArch": "amd64",
  "runtimeMode": "native",
  "runtimeOs": "linux",
  "distro": "Ubuntu",
  "pathMapper": "native",
  "environment": "wsl",
  "runtimeBackend": "tmux",
  "tmuxAvailable": true,
  "runtimeAvailable": true,
  "projectsRoot": "/srv/projects",
  "projectsRoots": ["/srv/projects"],
  "discoveryDepth": 3,
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
      "note": "The persistent terminal runtime. Each project runs on its own tmux server."
    },
    {
      "name": "claude",
      "available": false,
      "required": false,
      "probedIn": "wsl",
      "note": "Claude Code CLI, started inside a project's runtime on request. Optional: without it a runtime still runs, it just cannot host an agent. This probe is a bare lookup on PATH; the launcher resolves the real binary and version."
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
  "tmux": {
    "available": true,
    "version": "3.4",
    "binary": "/usr/bin/tmux",
    "socketDir": "/var/lib/agentmux/tmux",
    "minimumVersion": "3.0"
  },
  "claude": {
    "type": "claude",
    "available": false,
    "binary": "claude",
    "message": "claude was not found on PATH; set terminal.claudeBinary to its full path"
  },
  "features": {
    "projectRegistration": true,
    "projectCreation": true,
    "projectDiscovery": true,
    "gitInit": true,
    "terminal": true,
    "claudeRuntime": false,
    "providerSwitch": false,
    "claudeHooks": false,
    "controllerTransfer": false
  },
  "warnings": []
}
```

`claudeRuntime` is false and `claude.available` is false here, and that is the honest report rather
than a defect: the service account is a system account with no Claude Code installation of its own,
and `claude.message` says exactly what to set. `terminal` is true, so this deployment is useful — it
runs terminals — and it cannot host an agent until somebody points `terminal.claudeBinary` at a
binary that account can execute. The section below on the Windows example is the other way a
capability flag goes false.

`uptimeSeconds` is a snapshot, so a later call returns a larger number; `startedAt` is a constant for
the life of the process and is what "did the service restart" is read from.

Note what is **not** here. There is no `dataDirectory`, no `databasePath`, no `configFile` and no
`webDirectory` — those four appear only in debug mode, and the paragraph above says why. The one path
that is published in every mode is `tmux.socketDir`, because it is the directory an operator looks in
to explain an orphaned session, and a diagnostic that could not name it could not explain one. That
exception is stated in full in `docs/SECURITY.md` §7 rather than left to be discovered.

Read it in this order, because each field answers a different question and a client that skips one
will offer something the server cannot do:

- `environment` is where **the server process itself** is running: `windows`, `wsl`, or `linux`.
- `host` and `runtimeMode` describe the machine it is running on. On this example the process is a
  Linux process on a Linux host, and `environment` is `wsl` because that Linux is a distribution.
- `runtimeAvailable` is whether a persistent terminal runtime can execute **here**. It is false on a
  Windows-native server no matter what is installed anywhere, because a Windows process cannot own a
  Linux process tree.
- `runtimeBackend` names which runtime implements a session — `tmux` — and `tmuxAvailable` is the
  narrower question of whether the tmux binary was found. They answer differently in the case that
  matters: a build whose backend is tmux, on a machine with no tmux, has a backend and no runtime.
- `runtimeUnavailableReason` says what to do about it when it is false. Absent when `runtimeAvailable`
  is true.
- `terminalRuntimeImplemented` is a statement about the **build**: whether this binary contains the
  runtime at all. A build with the runtime on a Windows host has this true and `runtimeAvailable`
  false, and both facts matter.
- `features.terminal` is the single answer a client should act on. It is true only when the build has
  the runtime, the server is on the right side of the WSL boundary, **and** tmux is installed where
  sessions would run.
- `features.claudeRuntime` is whether a coding agent can be **started** here. It needs the terminal
  as well as a resolved Claude Code, because an agent with no terminal is a process nobody can see,
  interrupt or read. A client offers "Start Claude" on this flag and on nothing else.
- `claude` is the answer from the code that will actually launch the agent: which binary the setting
  resolved to, its version, and the exact command line. It is reported beside the `claude` dependency
  probe rather than instead of it, and the two answer different questions — the probe says whether a
  program called `claude` is on some PATH, and this says which binary AgentMux will execute. On a
  machine with two Claude Code installations, only the second predicts behaviour.
- There is **no field anywhere in this response that says whether Claude is authenticated.** That is
  Claude's own business, it is decided when the program starts, and it says so on its own terminal.
  This server does not look, so it does not claim.
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
  "features": { "terminal": false, "claudeRuntime": false },
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

## PATCH /api/projects/{id}

**The one field of a project a client may change: where it sits in the workspace.**

A project is in the workspace exactly when it has a reserved slot, so this call is
also how a project is added and removed:

```json
{ "pinnedSlot": 3 }     reserve slot 3
{ "pinnedSlot": null }  give the slot up
```

It answers with the whole project, like every other project endpoint, and it is
`200` whether the slot was set or cleared.

| Code | HTTP | When |
| --- | --- | --- |
| `invalid_request` | 400 | The body is not JSON, carries an unknown field, or omits `pinnedSlot` altogether |
| `invalid_input` | 400 | A slot outside 0-199, with the refused value in `details.pinnedSlot` |
| `project_not_found` | 404 | No project with that identifier |

**Nothing else about a project can be written here.** A `PATCH` carrying `name`,
`archived` or `hostPath` is refused by name rather than quietly ignored, because a
general update endpoint is where every future field arrives with no rule attached
to it. Archiving, renaming and deleting are separate operations with their own
consequences and their own phases.

**It does not touch the runtime.** Removing a project from the workspace leaves its
terminal running: this writes one column of one row. `docs/WORKSPACE.md` §2 is the
model and §9 is why that half of the rule matters.

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

Since Phase 7.3B-2 it also ends observation of the agent that was in it: the hook receiver is detached
and the settings document it generated is removed. An attempt that was running is closed as `FAILED` —
the environment it ran in was removed, which is not the same as somebody stopping it. `POST
…/runtime/stop` closes it as `CANCELLED` instead, because that one is a person asking.
`docs/AGENT_RUNTIME_BINDING.md` §3 is the table.

## The agent inside a runtime

A runtime can host one coding agent — today, the Claude Code CLI. It lives at
`/api/projects/{id}/runtime/agent`, nested under the runtime for the same reason the runtime is
nested under the project: an agent's identity is the runtime it is in, and one project → one runtime
→ one interactive Claude is the model, not a limitation to be worked around.

| Call | Effect |
| --- | --- |
| `GET /api/projects/{id}/runtime/agent` | The agent's current state. Never starts anything. |
| `POST /api/projects/{id}/runtime/agent/start` | Starts it, or adopts the one already in the pane. Idempotent. |
| `POST /api/projects/{id}/runtime/agent/stop` | Interrupts it and **leaves the runtime alone**. |

**Stop is not Destroy**, and it is also not "the agent is gone". `stop` sends the same interrupt a
user would send with `Ctrl-C`, and Claude is free to take it or not: a terminal in raw mode delivers
that keystroke to the program, and the program decides. When it declines, the response says so
rather than pretending — see `requested` and `message` below and §6 of `docs/CLAUDE_RUNTIME.md`.

Starting requires the runtime to be running. An agent with no terminal is a process nobody can see,
interrupt or read, so `agent/start` on a stopped runtime fails with `runtime_not_running` rather than
creating a terminal behind the caller's back.

### GET /api/projects/{id}/runtime/agent

```json
{
  "agent": {
    "type": "claude",
    "available": true,
    "state": "RUNNING",
    "running": true,
    "version": "2.1.274",
    "executable": "/home/you/.local/share/claude/versions/2.1.274",
    "pid": 843814,
    "dir": "/home/you/AgentMux-Projects/checkout-service",
    "startedAt": "2026-09-18T09:12:44.312+08:00"
  }
}
```

The agent is also reported inside the runtime, under `runtime.agent`. This endpoint exists so a panel
that shows whether Claude is up has one small thing to poll instead of a whole terminal description.

What the fields mean, and which of them are evidence rather than assertion:

- `state` is `STOPPED`, `STARTING`, `RUNNING`, `STOPPING`, `EXITED` or `FAILED`.
- `running` is the boolean a client switches on, and it is read **live from the process table** every
  time it is asked for. Nothing about liveness is stored, because a stored answer to "is it running"
  is a stale answer waiting to happen.
- `dir` is the working directory the agent process is **actually in**, read from `/proc/<pid>/cwd`,
  not assumed from the request. It is how "the agent runs in the project's directory" is checked
  rather than claimed.
- `pid` and `startedAt` come from the kernel too, so they survive a server restart: an adopted
  runtime reports the agent's real process id and real start time, not the moment the new server
  noticed it.
- `requested` is present only when true. It says AgentMux asked the agent to stop, which is what
  distinguishes a stop that was asked for from an agent that went away on its own (`EXITED`).
- `exitedAt` is when the agent was last observed to be gone.
- `message` explains a state that needs explaining, and is empty when there is nothing to say.

**There is no exit code.** The shell inside the terminal is the process that reaps the agent, so its
exit status is known to that shell and to nothing else, and reading it would mean reading the
terminal's text — which this product does not do. `requested` and `exitedAt` are what can be known
honestly.

**There is no field describing authentication.** See `docs/CLAUDE_RUNTIME.md` §2.

### POST /api/projects/{id}/runtime/agent/start

```json
{
  "agent": {
    "type": "claude",
    "available": true,
    "state": "RUNNING",
    "running": true,
    "version": "2.1.274",
    "executable": "/home/you/.local/share/claude/versions/2.1.274",
    "pid": 843814,
    "dir": "/home/you/AgentMux-Projects/checkout-service",
    "startedAt": "2026-09-18T09:12:44.312+08:00"
  }
}
```

The request body is optional and has one field:

```json
{"taskId": "task_44e0c1b2…"}
```

Naming a task asks for the attempt to be recorded as well as the agent to be started, and the two are
genuinely different requests — one is "run Claude here", the other is "and this is the work it is
doing". Naming none is a complete request, and is what the workspace's Start button has always sent.

Naming one also does more than record it. AgentMux chooses a session id, passes it to Claude Code as
`--session-id`, writes a settings document naming an adapter this server is running, passes that as
`--settings`, and binds the attempt to the session. Those are what make the agent *observed* rather
than merely started:

```json
{
  "agent": { "type": "claude", "state": "RUNNING", "running": true, "pid": 843814, "…": "…" },
  "session": {
    "id": "sess_9a27…",
    "taskId": "task_44e0c1b2…",
    "runtimeId": "amx-p_6f1a…",
    "status": "RUNNING",
    "startedAt": "2026-09-20T09:00:00Z",
    "createdAt": "2026-09-20T09:00:00Z"
  }
}
```

`session` is present only when a task was named; the field is omitted otherwise.

Idempotent, with one asymmetry. If an agent is already in the pane it is adopted and returned, not
started twice — and **adopting with a `taskId` is refused** (409, `agent_already_running`). A Claude
process AgentMux did not start has a session id AgentMux never chose and no hook configuration
pointing at a receiver, so there is nothing to observe and an attempt recorded against it would be a
row with no evidence behind it. Stop the agent and start it again to have the attempt recorded.

The runtime is started if it is not already running, which is what makes one call enough to go from a
stopped project to a running agent. The budget is wider than a runtime start's because it covers one
too.

The command line is the resolved Claude Code path, plus `--session-id` and `--settings` and nothing
else. AgentMux still does not add `--dangerously-skip-permissions`, `--permission-mode`,
`--allowedTools`, `--model`, or `-p`. Claude's interactive permission model is preserved exactly as it
is, and answering Claude's prompts is the user's job on a terminal the user owns.

A `taskId` belonging to another project is refused (400, `agent_task_mismatch`). The project path and
the task's own project have to agree; a runtime id from one project under another project's history
would be a row nobody could ever interpret, in a log that cannot be corrected.

### POST /api/projects/{id}/runtime/agent/stop

Stopping an agent that AgentMux started also closes the attempt as `CANCELLED`, and the response
carries it beside the agent:

```json
{
  "agent": { "state": "STOPPED", "running": false, "…": "…" },
  "session": { "id": "sess_9a27…", "status": "CANCELLED", "endedAt": "2026-09-20T09:14:02Z", "…": "…" }
}
```

The agent declined the interrupt — a real, observed response, verbatim:

```json
{
  "agent": {
    "type": "claude",
    "available": true,
    "state": "RUNNING",
    "running": true,
    "version": "2.1.274",
    "executable": "/home/you/.local/share/claude/versions/2.1.274",
    "pid": 843814,
    "dir": "/home/you/AgentMux-Projects/checkout-service",
    "startedAt": "2026-09-18T09:12:44.312+08:00",
    "requested": true,
    "message": "the interrupt was delivered and the agent is still running; it may need a second one, or it may be waiting for a decision of its own"
  }
}
```

An honest non-stop, and the shape a client should render as "Claude is still running, and you asked
it to stop." AgentMux does not escalate to `SIGTERM` or `SIGKILL`: that would destroy the terminal
and the scrollback that the whole runtime model exists to preserve. A second `stop` is allowed and
behaves the same way.

`running`, `requested` and `message` are all in the same response on purpose. A client that reads
only `running` sees the truth, a client that reads both sees the whole truth, and neither is told a
stop succeeded when it did not.

## Event timelines

Two endpoints, and they are the only ones that answer "what happened" rather than "what is true now".
Everything above this section describes state: the runtime resource says what a session is doing at
this moment. These two describe the past, which does not change.

| Call | Effect |
| --- | --- |
| `GET /api/projects/{id}/events` | One project's timeline, across every runtime it has had. |
| `GET /api/runtime/{id}/events` | One runtime's timeline. |

There is **no write endpoint**, and no method other than `GET` is routed on either path. Events are
produced by the layers that know what happened — the runtime, today — and an endpoint that accepted
one from a client would let anything that can reach this server put a row into a history that nothing
can correct afterwards.

Both take the same two query parameters and return the same envelope:

| Parameter | Meaning |
| --- | --- |
| `limit` | How many events to return. Default 50, maximum 200. |
| `before` | An event id. Return the events older than that one. |

```json
{
  "events": [
    {
      "id": "evt_1f0c4a2b9d8e7f60a1b2c3d4e5f60718",
      "projectId": "p_9a3b1c2d3e4f5061728394a5",
      "runtimeId": "amx-p_9a3b1c2d3e4f5061728394a5",
      "type": "runtime.started",
      "source": "runtime",
      "payload": { "state": "running", "cols": 120, "rows": 30 },
      "createdAt": "2026-09-20T11:04:07.318204Z"
    }
  ],
  "nextBefore": "evt_1f0c4a2b9d8e7f60a1b2c3d4e5f60718"
}
```

`runtimeId` and `payload` are absent rather than null when the event has none — a project-level event
has no runtime, and not every event carries detail. `nextBefore` is present only when more events
remain, and it is passed back as `before`.

Events are returned newest first. The ordering key is the pair `(createdAt, id)` and **not** the
timestamp alone: two events written in the same instant — which a runtime start does routinely — would
otherwise have no defined order, and paging over them would skip one or repeat one.

### Failures

| Situation | Status | Code |
| --- | --- | --- |
| The project does not exist | 404 | `project_not_found` |
| `before` is not an event id | 400 | `invalid_request` |
| `before` is a well-formed id that no event carries | 404 | `event_not_found` |
| `limit` is not a whole number, or is below 1 | 400 | `invalid_request` |
| The runtime id is not one AgentMux names | 400 | `invalid_request` |

`limit` above the maximum is clamped rather than refused, because a client asking for a thousand is
asking for as much as it can have; `nextBefore` tells it whether more remains.

The project endpoint resolves the project first, so an unknown id is a 404 about the project. The
runtime endpoint does not: a runtime is not a stored resource, so there is nothing to look up, and a
runtime id with no events is an empty timeline — which is the honest answer for a runtime that has
never started. Runtime ids are the session names AgentMux gives them (`amx-<project id>`), and a value
outside that namespace is a bad request rather than an empty list.

### What a timeline does not return

No internal path, no socket name, no host directory, no credential, and no terminal output. The six
fields above are the whole envelope, and the payload is bounded to what `docs/AGENT_EVENTS.md` §7
allows. The one failure a reader might expect to find here is not: a `runtime.error` event records
*that* an operation failed and what state it left, and never the error string, because an error string
copied into a row that can never be redacted is a string that can never be taken back.

`docs/AGENT_EVENTS.md` is the long form of this layer, including what it deliberately does not do.

## Tasks and agent sessions

Two resources, and the third thing this API knows about that is neither present state nor the past. A
task is **what somebody wants done**; an agent session is **one attempt at it**. Neither is a terminal,
a runtime, a WebSocket or a Claude process, and the distinction is the entire reason the level exists —
`docs/TASK_MODEL.md` §1–§3 is the long form, and §1 is the part worth reading before this section.

| Call | Effect |
| --- | --- |
| `POST /api/projects/{id}/tasks` | Record a piece of work somebody wants an agent to do. |
| `GET /api/projects/{id}/tasks` | One project's tasks, newest first. |
| `GET /api/tasks/{id}` | One task. |
| `PATCH /api/tasks/{id}` | Change a task's title, its status, or both. |
| `POST /api/tasks/{id}/sessions` | Start a new attempt at a task. |
| `GET /api/tasks/{id}/sessions` | One task's attempts, newest first. |
| `GET /api/sessions/{id}` | One attempt. |
| `PATCH /api/sessions/{id}` | Change an attempt's status, bind it to a runtime, or both. |

**There is no `DELETE` on either resource**, and no method other than those above is routed on any of
those paths. Nothing in this build removes a task or an attempt; a cancelled task is the record of
work somebody decided not to do, which is worth keeping, and the same argument covers an attempt that
failed. What the database would do if something ever did delete is decided by the foreign keys in
`0004` and `0005` — see `docs/TASK_MODEL.md` §5 — and this phase does not open that door.

**No endpoint here starts anything.** Creating a task starts no runtime, launches no agent and writes
no prompt. The two halves of AgentMux meet at the runtime API above, and joining them is a later
phase rather than a missing line of this one.

### A task

```json
{
  "id": "task_44e0a1b2c3d4e5f60718293a",
  "projectId": "p_9a3b1c2d3e4f5061728394a5",
  "title": "Implement Controller Viewer",
  "status": "CREATED",
  "createdAt": "2026-09-20T11:04:07.318204Z",
  "updatedAt": "2026-09-20T11:04:07.318204Z",
  "completedAt": null
}
```

Every time in the model is produced by the server, in UTC, and a client's clock is never read. A
`completedAt` of `null` is present rather than absent: the field answers "when was this completed",
and *never* is an answer that has to be sayable.

`title` is free text and is treated as such — never executed, never interpolated into a shell, never
interpreted as a path or as HTML. It is bounded at 200 characters, counted in **runes** rather than
bytes, because the bound exists to bound what a person sees and 200 bytes is a different amount of
text in every script. An empty title, one with leading or trailing whitespace, one that is not valid
UTF-8 and one carrying control characters are refused rather than quietly rewritten.

The six statuses, and the edges between them, are the whole lifecycle:

```text
from        may become
CREATED     RUNNING, CANCELLED
RUNNING     WAITING, COMPLETED, FAILED, CANCELLED
WAITING     RUNNING, COMPLETED, FAILED, CANCELLED
COMPLETED   — terminal
FAILED      — terminal
CANCELLED   — terminal
```

`AllowedTaskTransitions` in `internal/task` is the same list, and a refusal carries it in
`details.allowed` so a client renders the control the server actually enforces rather than a second
copy of the rule.

Written as a table of allowed edges rather than forbidden ones, so that a transition nobody thought
about is refused. Three consequences are worth stating because they are decisions rather than
arithmetic: `COMPLETED` is final, so a task reported as done and then reported as running again cannot
make the first report false; `CREATED` does not lead directly to `COMPLETED`, because "completed" is a
claim about work that was done and work that never started was not done; and `WAITING` exists on the
task and **not** on the session, because being blocked on a review or a decision is a long-lived
condition of a *goal*, while a runtime that is up and idle is idle, and idle is not a lifecycle state.

#### `POST /api/projects/{id}/tasks`

The project is in the path and deliberately **not** in the body. A body that also names one is an
unknown field and is refused rather than resolved by a rule nobody wrote down: two places to say which
project a task belongs to is one place too many.

```json
{ "title": "Implement Controller Viewer" }
```

Answers `201` with `{ "task": { … } }`. The task begins in `CREATED` with no attempt, and the project
is resolved before anything is written, so a task cannot be created under a project that does not
exist.

#### `GET /api/projects/{id}/tasks`

| Parameter | Meaning |
| --- | --- |
| `status` | Return only tasks in this status. One of the six, exactly. |
| `limit` | How many to return. Default 100, maximum 500. |

```json
{ "tasks": [ … ], "count": 1 }
```

Newest first, ordered by `(createdAt, id)` and **not** by the timestamp alone: two tasks created in
the same instant would otherwise have no defined order, and the tie-break is what makes the order
stable across two calls that read the same rows. The list is **capped and not paginated** — there is no
cursor, and `count` is the number of rows in this page rather than the number that exist. That is a
deliberate limit of this phase rather than an oversight; `docs/TASK_MODEL.md` §12 records it.

The project is resolved first, so an unknown id is a 404 about the project rather than a project that
exists and has nothing in it. An unknown `status` is refused with the six that exist rather than
answered with an empty list, for the same reason.

#### `GET /api/tasks/{id}`

`200` with `{ "task": { … } }`, or `404 task_not_found`.

#### `PATCH /api/tasks/{id}`

Both fields are optional and at least one must be present.

```json
{ "title": "Implement Controller Viewer", "status": "RUNNING" }
```

A field sent as `null` is refused rather than treated as "leave it alone": a request to clear a task's
title is a mistake, and reporting it as an absent field would silently accept it. A status equal to
the one already stored is a no-op that returns the task unchanged, so a client that re-sends what it
already knows is not told it lost a race.

This is the only route that moves a task, and it cannot bypass the lifecycle: the service checks the
transition against the status it reads and writes **conditionally** on that status still holding. Two
requests that race therefore apply exactly once — the loser is told `409 status_conflict` and is not
handed a state it did not ask for. `docs/TASK_MODEL.md` §10 has the mechanism.

### An agent session

```json
{
  "id": "sess_9a27f1e0d3c4b5a69788796a",
  "taskId": "task_44e0a1b2c3d4e5f60718293a",
  "status": "CREATED",
  "startedAt": null,
  "endedAt": null,
  "createdAt": "2026-09-20T11:05:12.884301Z"
}
```

`runtimeId` is **absent rather than null** when the attempt has no runtime yet, which is how every
attempt begins. Empty is a first-class value here, not a placeholder: a session is created before its
runtime is, and the window between the two calls is a real state this model names rather than hides.

An attempt has five statuses — `CREATED`, `RUNNING`, `COMPLETED`, `FAILED`, `CANCELLED` — and the same
shape of lifecycle as a task, minus `WAITING`. `startedAt` is set on entering `RUNNING` and `endedAt`
on entering a terminal status, so a session that failed before it ever ran has an end and no start:
that is the honest description of a runtime that never came up.

#### `POST /api/tasks/{id}/sessions`

A complete request with nothing in it, and the body is optional. `201` with `{ "session": { … } }`.

`runtimeId` is deliberately **not** accepted here. Creating an attempt and starting a process are two
acts that fail differently, and a single transaction across both would hold a write lock for the
length of a process launch and let a tmux that refused to start undo the record that somebody wanted
this work done at all. A caller that wants both makes two requests, and the first one is true on its
own.

A second attempt is a second session. Neither replaces the other — that is what this level is for.

#### `GET /api/tasks/{id}/sessions`

Takes `limit` (default 100, maximum 500), returns `{ "sessions": [ … ], "count": 1 }`, newest first
with the same `(createdAt, id)` ordering. The task is resolved first, so an unknown id is a 404 about
the task.

#### `GET /api/sessions/{id}`

`200` with `{ "session": { … } }`, or `404 session_not_found`.

#### `PATCH /api/sessions/{id}`

```json
{ "status": "RUNNING", "runtimeId": "amx-p_9a3b1c2d3e4f5061728394a5" }
```

**The status is applied first**, and the order is a decision rather than an accident. The status write
is the one that can lose a race; applying it first means a caller told it lost is told so *before* a
runtime has been attached on its behalf, so nothing about a refused request is left behind.

`runtimeId` binds the attempt to the runtime it ran in. It may be set **once**: a session that already
has one is refused with `409 status_conflict` rather than rebound, because the field answers "which
runtime did this attempt run in" and a field that can be rewritten stops answering it. It is not
checked for existence — a runtime can be destroyed while the attempt that used it remains, which is
the whole reason it is not a foreign key. Its shape is checked, and an id outside the `amx-` namespace
is a `400 invalid_request`.

### Failures

| Situation | Status | Code |
| --- | --- | --- |
| A title that is empty, too long, not UTF-8, or carries control characters | 400 | `invalid_task_title` |
| An unknown status value, an unknown query parameter value, or a malformed runtime id | 400 | `invalid_input` |
| A body with no `title` and no `status`, or a blank body | 400 | `invalid_request` |
| The project does not exist | 404 | `project_not_found` |
| The task does not exist | 404 | `task_not_found` |
| The session does not exist | 404 | `session_not_found` |
| A status change the lifecycle does not allow | 409 | `invalid_status_transition` |
| Somebody else changed the same row first, or a runtime is already bound | 409 | `status_conflict` |
| The server was built without the task model | 503 | `internal_error` |

An unknown status and an unavailable transition are **different answers** on purpose. The first is
`invalid_input` with the field named and the vocabulary in the message, because the caller can fix it.
The second is `invalid_status_transition` with `details.from`, `details.to` and `details.allowed`,
because the request was well-formed and the task exists — the two are simply incompatible right now,
and `400` would misreport a perfectly valid request against a task that is still running.

`runtimeId` sent as an empty string is refused rather than treated as a no-op, which is worth stating
because the mistake is an easy one to make and the failure would otherwise be silent: a session that
has no runtime would be told it now has the one it asked for, and nothing would have been recorded.

`docs/TASK_MODEL.md` is the long form of this layer: what a task is, why it is not a runtime, what the
lifecycle refuses and why, and what this phase deliberately does not do.

## Agent state

What the event log adds up to right now, as opposed to what happened — which is what the two timeline
endpoints answer. A state is one row per attempt, derived from `agent_events` as they are written, so
asking what an agent is doing costs one row rather than the whole history.

**There is no write.** Not a PATCH, not a PUT, not a POST. A state is what the events add up to, and
an endpoint that could set one would make it a second place the truth lives — and the two would
disagree the moment the next event arrived. §15 of the phase that built this is the rule, and
`docs/AGENT_STATE.md` §8 is the reasoning.

### GET /api/sessions/{id}/state

```json
{
  "agentSessionId": "sess_9a27…",
  "projectId": "p_6f1a…",
  "runtimeId": "amx-p_6f1a…",
  "status": "WAITING_PERMISSION",
  "lastEvent": "agent.permission_requested",
  "lastEventAt": "2026-09-21T09:00:12Z",
  "updatedAt": "2026-09-21T09:00:12Z"
}
```

`runtimeId` is absent until the attempt is bound to a runtime, which happens when it starts. The
statuses are `CREATED`, `RUNNING`, `WAITING_INPUT`, `WAITING_PERMISSION`, `COMPLETED`, `FAILED` and
`STOPPED` — the agent's own vocabulary, deliberately not the runtime's and not the task's.

`lastEvent` is an event **type**, never a payload. The row holds no prompt, no tool input and no
transcript path, and `docs/AGENT_STATE.md` §7 is why.

| Situation | Status | Code |
| --- | --- | --- |
| No event has produced a state for this attempt | 404 | `agent_state_not_found` |
| The attempt id is empty | 400 | `agent_state_invalid` |
| The server was built without the projection | 503 | `internal_error` |

A 404 is the **ordinary answer**, not a failure: an attempt that was created and never started has
none, and neither does an agent started without a task — there is no attempt for its events to be the
state of. `docs/AGENT_STATE.md` §6 is the list of what is not projected.

### GET /api/projects/{id}/agent-states

```json
{
  "states": [
    {
      "agentSessionId": "sess_9a27…",
      "projectId": "p_6f1a…",
      "runtimeId": "amx-p_6f1a…",
      "status": "RUNNING",
      "lastEvent": "agent.started",
      "lastEventAt": "2026-09-21T09:00:04Z",
      "updatedAt": "2026-09-21T09:00:04Z"
    }
  ],
  "count": 1
}
```

Most recently updated first. `limit` is accepted the same way it is elsewhere in this API: absent
means the service's default, and a value below 1 is a 400 rather than "no limit".

| Situation | Status | Code |
| --- | --- | --- |
| The project does not exist | 404 | `project_not_found` |
| `limit` is not a whole number, or is below 1 | 400 | `invalid_request` |
| The server was built without the projection | 503 | `internal_error` |

An unknown project is answered as a missing project rather than as a project with no agents. Those
two answers mean different things, and an empty list is the one a client cannot tell apart from a
project that exists and has nothing running.

`docs/AGENT_STATE.md` is the long form of this layer: the difference between an event and a state,
the projection, the status vocabulary and which parts of it are reachable, and how the whole table is
rebuilt from the log.

## Agent attention and actions

What needs a person, as opposed to what is happening. A state says what an agent is doing; a level
says whether anybody needs to care; an action says what they might do about it. Two agents can both be
working and only one of them be blocked — which is the difference these endpoints exist to report.

**There is no write, and there is deliberately no way to answer an action.** A `PERMISSION_REQUEST`
action records that Claude asked for something; it does not allow it, deny it, or influence it in any
way. An endpoint that answered one would make AgentMux a participant in a Claude session rather than
an observer of one, and that decision belongs to a phase with its own threat model.
`docs/AGENT_ATTENTION.md` §6 is the reasoning.

### GET /api/sessions/{id}/attention

```json
{
  "agentSessionId": "sess_9a27…",
  "projectId": "p_6f1a…",
  "level": "ACTION_REQUIRED",
  "reason": "permission requested",
  "updatedAt": "2026-09-21T09:00:12Z"
}
```

The levels are `NONE`, `ACTION_REQUIRED`, `WARNING` and `INFO` — about a person, not about the agent.
`ACTION_REQUIRED` means nothing progresses without somebody; `WARNING` means something went wrong and
can be read later; `INFO` is worth knowing; `NONE` is nothing to see.

`reason` is a short fixed phrase from the server's own vocabulary, never a quotation from a payload.
It never names a tool and never includes anything a person wrote.

| Situation | Status | Code |
| --- | --- | --- |
| The log has never mentioned this attempt | 404 | `agent_attention_not_found` |
| The attempt id is empty | 400 | `agent_attention_invalid` |
| The server was built without the projection | 503 | `internal_error` |

Every attempt gets a level from its own creation event, so a 404 means the attempt is not one this
server knows rather than that nothing has happened to it yet.

### GET /api/projects/{id}/attention

```json
{
  "attention": [
    {
      "agentSessionId": "sess_9a27…",
      "projectId": "p_6f1a…",
      "level": "WARNING",
      "reason": "attempt failed",
      "updatedAt": "2026-09-21T09:00:12Z"
    }
  ],
  "count": 1
}
```

Most recently updated first. `limit` behaves as it does elsewhere in this API.

### GET /api/projects/{id}/actions

```json
{
  "actions": [
    {
      "id": "act_3c9b…",
      "agentSessionId": "sess_9a27…",
      "projectId": "p_6f1a…",
      "type": "PERMISSION_REQUEST",
      "status": "PENDING",
      "reason": "permission requested",
      "createdAt": "2026-09-21T09:00:12Z"
    }
  ],
  "count": 1
}
```

**Pending first, then newest first.** A list ordered only by time would put a settled action above one
that is still waiting, and what a reader of a queue is looking for is what is still waiting.

The types are `PERMISSION_REQUEST`, `VIEW_FAILURE` and `VIEW_COMPLETION` — every one of them something
a person *looks at*. The statuses are `PENDING`, `RESOLVED` and `EXPIRED`; `RESOLVED` is reached when
a later event shows the attempt moved on, and `EXPIRED` is in the vocabulary with nothing producing it.

`resolvedAt` is absent while an action is pending and carries the time of the event that resolved it
afterwards.

| Situation | Status | Code |
| --- | --- | --- |
| The project does not exist | 404 | `project_not_found` |
| `limit` is not a whole number, or is below 1 | 400 | `invalid_request` |
| The server was built without the projection | 503 | `internal_error` |

`docs/AGENT_ATTENTION.md` is the long form: the four readings of one log, the action lifecycle, why an
action's id is derived from its event, and why the loop closes at the terminal rather than here.

## The controller dashboard

One response for a console, instead of the seven it would otherwise have to call and join itself. The
join happens here, once, in four queries rather than four per project.

It is a **read model**: no table, no cache, no state that survives a request, and every route is a
GET. `docs/CONTROLLER_API.md` is the long form.

### GET /api/controller

```json
{
  "server": {
    "status": "online",
    "runtimeAvailable": true,
    "version": "0.6.5",
    "uptimeSeconds": 4211
  },
  "projects": [
    {
      "id": "p_6f1a…",
      "name": "checkout-service",
      "runtime": { "status": "running" },
      "agent": {
        "available": true,
        "sessionId": "sess_9a27…",
        "status": "WAITING_PERMISSION",
        "lastEvent": "agent.permission_requested",
        "updatedAt": "2026-09-21T09:00:12Z"
      },
      "attention": { "available": true, "level": "ACTION_REQUIRED", "reason": "permission requested" },
      "actions": { "available": true, "pending": 1 },
      "updatedAt": "2026-09-21T09:00:12Z"
    }
  ],
  "count": 1
}
```

Cards come back **most in need of attention first**: `ACTION_REQUIRED`, then `WARNING`, then what is
running, then what has finished, then everything else. Within that, most recently changed first.

Each section speaks its own layer's vocabulary and none is translated — `runtime.status` is the
project model's, `agent.status` is the agent state projection's, `attention.level` is the attention
projection's. A client that knows one endpoint's vocabulary should not have to learn a second spelling
of it here.

**Null and unavailable are different** and the response distinguishes them:

| | Means |
| --- | --- |
| `"agent": null` | nothing has ever run in this project |
| `"agent": {"available": false}` | the server cannot answer — no state projection is wired |

A missing or failing projection degrades its own section rather than failing the request. A dashboard
that shows five of seven things is worth more than an error page.

### GET /api/controller/projects

The same cards without the server block, for a client that already knows what server it is talking to:
`{"projects": [...], "count": n}`.

| Situation | Status | Code |
| --- | --- | --- |
| The project list could not be read | 500 | `controller_unavailable` |
| The server was built without the aggregation | 503 | `internal_error` |

## GET /api/ws

**The one real-time endpoint.** Phase 4's terminal is served here and nowhere else: one WebSocket per
browser, with subscriptions multiplexed on it, so watching a second project does not mean a second
connection. The protocol is binary frames for terminal output and snapshots, JSON for control, and it
is specified in full — frames, messages, error codes, limits and timings — in **`docs/TERMINAL.md`**.

What belongs here, in the document of what this server serves, is the shape of the endpoint rather
than its wire format:

| | |
| --- | --- |
| URL | `GET /api/ws`, optionally `?v=<protocol>` |
| Subprotocol | `agentmux.terminal.v2` |
| A client names | a `projectId` it is subscribing to, and which client it is — and nothing else |
| A client cannot name | a filesystem path, a session name, a socket, or a command |
| Origin | checked against the request's own host and `server.allowedOrigins`; a missing `Origin` is allowed, because a browser always sends one |

It is not a REST resource and has no JSON body: its failures are protocol errors on the connection
(`docs/TERMINAL.md` §4.3) once it is open, and an ordinary HTTP status when the handshake itself is
refused — `403 forbidden` for an Origin that is not allowed, `400 invalid_request` for a protocol
version this server does not speak.

Two properties are worth stating in this document because they are what a reader of the REST API would
otherwise assume and be wrong about. **Nothing here accepts a command.** A browser sends raw
keystrokes to a terminal it has already subscribed to, and they go to whatever is running in the pane
exactly as typing would; there is no message that runs a program. And **nothing here can create or
destroy a runtime.** Subscribing to a project that is not running is refused; starting one is
`POST /api/projects/{id}/runtime/start` above.

### Who may type: the controller lease

Phase 7.4B-2B added the one piece of authority the terminal needed and the REST API has no shape for:
**which client may send input to a project's terminal.** It is deliberately not an endpoint. There is
no `GET /api/runtime/{id}/controller`, no `POST .../controller/request` and no
`POST .../controller/release`, because a lease is a property of a live connection rather than of a
resource — it names a *connected client*, expires when that client goes away, and is meaningless to
anybody who is not holding the socket it was granted on. Serving it over HTTP would mean inventing a
second identity for a browser that already has one.

So a client asks on the socket it is already holding, with `control.request` and `control.release`,
and the server answers with `control.changed` broadcasts and `control.denied` refusals. Three
properties of it belong in this document:

- **one controller at a time, and never preempted.** A request for a terminal somebody else holds is
  refused, not queued behind a takeover. The refusal names the holder by the device label the server
  derived from User-Agent, so the person asking is told who has it rather than that they may not;
- **identity comes from the connection, not the frame.** The server never reads a client id out of a
  message body; the lease is granted to the client that owns the socket the request arrived on;
- **input is refused without it.** A client without the lease that sends an `input` frame, or a
  `resize`, is refused with an error naming the message it was about — and the connection stays open.
  A refusal is not a fault: the terminal is still there and still being drawn.

The lease lives in memory only. There is no table, no row, and no record of what was typed —
`docs/TERMINAL_CONTROLLER.md` §5 is the reasoning, and the log lines carry a project id and a client
id and never a keystroke.

## The diagnostic endpoints are gone

Phase 2 and Phase 3 exposed a set of off-by-default endpoints under `/api/debug` — raw terminal
input, buffered output, a resize, a session listing, an on-demand reconcile. They existed because
there was no terminal stream, and the runtime had to be exercisable by something.

**Phase 4 deleted them.** Not gated them, not marked them debug-only: deleted. There is no
`-debug-api` flag, no `AGENTMUX_DEBUG_API` environment variable, and no handler behind any
`/api/debug/...` path. Every one of those paths answers `404 not_found` on every build, exactly like
any other unknown path.

That is the honest shape of the change. A flag that registers the routes and refuses inside the
handler leaves the surface present and one configuration line away from live, and a route that
accepts raw terminal input and writes it into a session is not a thing to leave standing next to the
real one. The terminal's input and output now travel over the WebSocket at `/api/ws`, where
they are subject to a subscription that the server resolves from a `projectId` — see
`docs/TERMINAL.md` for the protocol and `docs/PROTOCOL.md` for what the surface is meant to become.

The one thing that was not deleted is reconciliation, which was never a debug concern: it runs on
startup, as it must, and it reports what it found to the log. Its three answers are unchanged and are
worth stating, because they are the whole of the restart policy:

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
| `forbidden` | 403 | The request came from a place this server will not serve. Two things return it: the `/api/ws` handshake, and any request that would change something, for an Origin that is neither the server's own nor in `server.allowedOrigins`. See the section at the top of this document. |
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
| `agent_unavailable` | 503 | No coding agent can be hosted here: none is installed, none could be resolved, or the terminal backend cannot report the process a session runs. |
| `agent_launch_failed` | 500 | The agent was started and never appeared as a process. |
| `agent_stop_failed` | 500 | The agent could not be interrupted. |
| `agent_wrong_directory` | 422 | The agent is not where the project is, or started somewhere else. Refused, not warned about. |
| `agent_terminal_busy` | 409 | The terminal's foreground process is not the shell, so a command typed at it would go to another program. |
| `invalid_event` | 400 | An event the event model refuses: a malformed source, a type that is not a dotted name, or a payload that is over the bound or names a credential. Nothing writes an event from a request in this build, so it is reachable only through a query parameter. |
| `event_not_found` | 404 | An event id that no stored event carries, which is what a pagination cursor naming nothing is. |
| `storage_failure` | 500 | The metadata store could not complete the request. |

`runtime_not_running` is a `409` and not a `404`: the runtime exists, and it is in a state that makes
the call meaningless. The two are different bugs to a client, and only one of them is worth retrying.

`agent_unavailable` is a `503` for the same reason `runtime_unavailable` is: it is a statement about
this machine rather than about anything the caller sent, and the message says what to do about it. A
client that saw `500` there would report a bug where the honest answer is "not on this machine".

## Not implemented

There is no provider switching and no Waiting/Completed state detection. There is
no Task UI: the task and session endpoints above exist and the frontend has types
and functions for them, and no screen shows either. `docs/PROTOCOL.md` sections 4
to 13 describe the agreed design for the rest, and none of them answers today.

Claude Hooks are read — Phase 7.3B-1 built an adapter that receives them and
records `agent.*` events — and **since Phase 7.3B-2 the server runs one.**
`POST /api/projects/{id}/runtime/agent/start` starts a runtime if it needs one,
launches Claude under a session id AgentMux chose, attaches a receiver and binds
the attempt. The adapter is no longer something a client has to imagine: starting
an agent is what constructs one, and `docs/AGENT_RUNTIME_BINDING.md` is the whole
of that chain.

Since Phase 7.3C-1 what that adapter reports is also readable as a **current
state** rather than only as a history: `GET /api/sessions/{id}/state` and
`GET /api/projects/{id}/agent-states` answer from a projection of the log, and
`docs/AGENT_STATE.md` is that layer. The two timeline endpoints remain the way to
read what happened; the two state endpoints are the way to read what is true now, and the attention
and action endpoints are the way to read what needs a person.

What is still not implemented, and is not stubbed:

- **No permission control.** `agent.permission_requested` is recorded and read by
  nobody. Every response the hook receiver sends has an empty body, so AgentMux
  cannot influence a decision Claude makes.
- **No task status change.** A task's status is only ever moved by `PATCH
  /api/tasks/{id}`, by a person. An attempt ending is not the work ending, and
  nothing derives either from an event.
- **No `agent.completed` or `agent.failed` on the path a session actually runs
  on.** They come from the CLI's `result` envelope, which the stream-json half of
  the adapter reads — and the runtime hosts Claude as a TUI in a tmux pane, so
  nothing feeds it. The five hook events are what a real session produces.
  `docs/AGENT_RUNTIME_BINDING.md` §5.
- **No binding that survives a server restart**, and no reconciliation of an
  attempt a restart left `RUNNING`. §6 of the same document.
- **No notification, no dashboard, no Task UI.** The task and session endpoints
  exist and the frontend has types and functions for them; no screen shows either,
  and nothing pushes anything anywhere.

The event timelines record what happened and nothing reads them to decide anything — in particular the
task service does not, and a task's status is never derived from its events. There is no endpoint that
turns an event into a task, a notification, or a state.

What this build serves is the project model, the runtime endpoints, the agent inside one, the terminal,
the event timelines, and — since Phase 7.2 — the tasks and agent sessions those are about. Since Phase
7.3B-2 those two halves are joined: `POST /api/projects/{id}/runtime/agent/start` with a `taskId`
records an attempt, and its events are in the timeline beside it. A project's runtime can host the real
Claude Code CLI, started by the server, the server reports honestly whether it is running,
`GET /api/ws` shows it to you as the terminal it is, `GET /api/projects/{id}/events` says what has
happened to it over time, and `POST /api/projects/{id}/tasks` records that somebody wants something
done. `docs/ROADMAP.md` is where the next phase is defined, `docs/CLAUDE_RUNTIME.md` describes how the
agent is resolved, launched, and observed, `docs/TERMINAL.md` is the terminal protocol,
`docs/AGENT_EVENTS.md` is the event layer, `docs/AGENT_RUNTIME_BINDING.md` is the join between the
agent and the task, and `docs/TASK_MODEL.md` is the task layer.
