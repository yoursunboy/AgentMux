# AgentMux HTTP API — Phase 1

This is the surface the Phase 1 server actually serves, with the shapes it actually returns. Every
example below was produced by running the server and calling it; none of it is aspirational. The
target for later phases is in `docs/PROTOCOL.md`.

Base path: `/api`. The server binds `127.0.0.1:8787` by default and serves the built frontend from
the same origin, so the browser only ever talks to one host.

## GET /api/server

Server identity, host and runtime description, capability flags, and the configuration summary.
Contains no credentials, no tokens, and no provider keys.

```json
{
  "appName": "AgentMux",
  "version": "0.1.0",
  "phase": "Phase 1 - Project model and server foundation",
  "status": "online",
  "startedAt": "2026-09-17T08:57:57Z",
  "uptimeSeconds": 9,
  "installId": "inst_8245417ba5e9186a",
  "host": "windows",
  "hostArch": "amd64",
  "runtimeMode": "wsl",
  "runtimeOs": "linux",
  "distro": "Ubuntu-24.04",
  "pathMapper": "wsl:/mnt",
  "projectsRoot": "D:\\AI\\Projects",
  "projectsRoots": ["D:\\AI\\Projects"],
  "discoveryDepth": 3,
  "dataDirectory": "C:\\Users\\you\\AppData\\Local\\AgentMux",
  "databasePath": "C:\\Users\\you\\AppData\\Local\\AgentMux\\agentmux.db",
  "webDirectory": "D:\\AI\\Projects\\2026 AgentMux\\AgentMux\\web\\dist",
  "terminalRuntimeImplemented": false,
  "dependencies": [
    {
      "name": "git",
      "available": true,
      "path": "C:\\Program Files\\Git\\mingw64\\bin\\git.exe",
      "required": false,
      "note": "Used when a new project is created with Initialize Git. Probed on the Windows host, not inside WSL."
    },
    {
      "name": "tmux",
      "available": false,
      "required": false,
      "note": "Persistent terminal runtime. Required from Phase 2. Probed on the Windows host, not inside WSL."
    }
  ],
  "provider": { "tool": "claude", "integrated": false, "status": "not_integrated" },
  "features": {
    "projectRegistration": true,
    "projectCreation": true,
    "projectDiscovery": true,
    "gitInit": true,
    "terminal": false,
    "providerSwitch": false,
    "claudeHooks": false,
    "controllerTransfer": false
  },
  "warnings": []
}
```

`installId` is an opaque identifier generated locally on first run and stored in the `settings`
table. It is not a credential and grants nothing; it exists so a log line or a bug report can name
one installation. It is the only stable per-install value this endpoint exposes.

`features` and `terminalRuntimeImplemented` exist so the client can decide what to render instead of
guessing from a version number.

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

`status` is derived on every read and never stored. Phase 1 has no session runtime, so it is always
`"stopped"`; `"running"` and `"reconnecting"` are part of the contract for later phases.

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

## Error envelope

Every failure has the same shape:

```json
{ "error": { "code": "path_not_found", "message": "…", "details": { } } }
```

`code` is a stable string a client switches on. `message` is written to be shown to a user.
`details` is present only when there is structured context.

| Code | HTTP | Meaning |
| --- | --- | --- |
| `invalid_input` | 400 | Malformed request body, for example a missing `hostPath`. |
| `invalid_project_name` | 400 | The name cannot be used as a directory name. |
| `path_not_a_directory` | 400 | The path is a file. |
| `path_is_projects_root` | 400 | The path is a Projects Root itself, not a project inside one. |
| `path_not_found` | 404 | The directory does not exist. |
| `project_not_found` | 404 | No project with that identifier. |
| `path_not_accessible` | 403 | The directory exists but cannot be read. |
| `path_outside_projects_root` | 403 | The path is not inside any configured Projects Root. |
| `project_already_registered` | 409 | That directory is already a project. |
| `path_already_exists` | 409 | The create target exists and is not empty. |
| `runtime_path_mapping_failed` | 422 | The host path has no runtime equivalent. |
| `git_unavailable` | 503 | Git is not on PATH. |
| `git_init_failed` | 500 | `git init` ran and failed. |
| `storage_failure` | 500 | The metadata store could not complete the request. |
| `not_found` | 404 | No such endpoint. |

## Not implemented

There is no WebSocket, no terminal stream, no session start or stop, no controller lease, and no
provider switching. `docs/PROTOCOL.md` sections 4 to 13 describe the agreed design for those; none
of them answers today.
