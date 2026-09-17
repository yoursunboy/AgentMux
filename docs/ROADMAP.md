# AgentMux Roadmap

## Implementation status

As of version 0.1.0:

| Phase | Status | Note |
| --- | --- | --- |
| Phase 0 — Foundation and documentation | **Done** | Workspace, `CLAUDE.md`, `.claude/rules/`, docs, environment checks. |
| Phase 1 — Project model and server foundation | **Done** | Config, SQLite, HostAdapter, discovery, register/create API, server status, Phase 1 UI. |
| Phase 2 — Persistent session runtime | **Done** | `SessionBackend` + `TmuxBackend`, runtime manager, persistence and reconciliation, runtime API, Phase 2 UI. No WebSocket, no real terminal view. |
| Phase 3 and later | Not started | No Claude Code launch, no xterm.js, no provider integration. |

What this means in practice: `GET /api/server` reports `terminalRuntimeImplemented: true`, and
`features.terminal` is true only when the server is running where tmux is (`runtimeAvailable`) and tmux
is actually installed there. The UI offers Start/Stop runtime and says plainly that the terminal view
arrives in Phase 4. The provider still reports `integrated: false`. Nothing in the running product
claims to do more than the table above.

## Phase 0 — Foundation and documentation

Status: **done**.

Deliver:

- Go/React workspace skeleton;
- `CLAUDE.md`;
- `.claude/rules/`;
- architecture docs;
- environment checks.

No tmux implementation yet.

## Phase 1 — Project model and server foundation

Status: **done**. See `docs/API.md` for the endpoints this phase adds.

Goal:

Support the real folder organization model.

Deliver:

- configuration;
- SQLite;
- Projects Root configuration;
- Group/Collection-aware project model;
- bounded candidate discovery;
- explicit project registration;
- New Project in selected collection;
- Open/Register Project API;
- HostAdapter skeleton;
- basic server status.

Important:

Do not assume first-level directories are projects.

## Phase 2 — Persistent session runtime

Status: **done**. See `docs/RUNTIME.md` for the design and `docs/API.md` for the endpoints.

Goal:

A terminal that outlives the server, in the project's own directory, with byte-exact input and output.

Deliver:

- SessionBackend interface;
- TmuxBackend;
- create/list/start/stop/destroy;
- raw input;
- resize;
- output capture;
- multiple isolated sessions.

Use shell/test processes first.

What was built:

- `internal/session` — `Backend`, `Subscription`, `TmuxBackend`, and the `Manager` that owns session
  naming, canonical size, bounded output history, sequence numbers, and reconciliation.
- Live output over **tmux control mode**, chosen over `capture-pane` polling for the reasons in
  `docs/RUNTIME.md` §2. `%output` payloads are decoded as bytes; UTF-8 arrives unescaped.
- The server runs **inside WSL**; a Windows-native server reports the runtime as unavailable rather
  than proxying. Dependency probes run in the runtime environment and report `probedIn`.
- `project_runtime` table (migration `0002`) holding only what survives a restart: backend, session
  name, intent, canonical size, timestamps. No output, no lease, no viewer, no `is_running` flag.
- Reconciliation on startup: Case A rediscovered as `RUNNING`, Case B reported `STOPPED` and **not**
  auto-started, Case C recorded as an orphan and **left running**.
- Runtime API: `GET`/`POST start`/`POST stop`/`DELETE` under `/api/projects/{id}/runtime`.
- Off-by-default diagnostic endpoints under `/api/debug`, to be deleted in Phase 4.
- Frontend: Start/Stop runtime controls and an honest "Terminal UI coming in Phase 4".

Not built, deliberately: Claude Code launch, any WebSocket, any real terminal view, controller/viewer
roles, prompt bar, Claude Hooks.

## Phase 3 — Claude runtime

Deliver:

- launch Claude in registered project working directory;
- send input;
- receive real TUI output;
- disconnect leaves Claude alive;
- reconnect to existing session.

## Phase 4 — Web terminal

Deliver:

- xterm.js;
- WebSocket;
- ANSI/TUI fidelity;
- raw keyboard input;
- Prompt Bar;
- reconnect snapshot;
- touch terminal keys.

## Phase 5 — Multi-project grid

Deliver:

- ProjectPanel;
- 1×3 and 2×3 layouts;
- Project Manager;
- focus/full-screen;
- five concurrent project sessions;
- pagination foundation.

## Phase 6 — Multi-device control

Deliver:

- Viewer/Controller;
- atomic Take Control;
- 10-second lease;
- independent scroll;
- canonical PTY size;
- controller-only resize;
- software-keyboard resize protection.

## Phase 7 — Claude state awareness

Through Claude Hooks:

- READY;
- RUNNING;
- WAITING;
- COMPLETED;
- ERROR.

Terminal remains the real displayed interface.

## Phase 8 — CC Switch integration

Deliver:

- CCSwitchAdapter;
- current provider;
- provider list;
- global provider switch;
- no credential exposure;
- safe handling of active turns.

No per-project provider selection.

## Phase 9 — Runtime recovery and quality

Deliver:

- stronger reconnect;
- rolling terminal logs;
- Claude session ID tracking;
- safe resume;
- project slot persistence;
- Open VS Code;
- diagnostics.

## V1.0

Release criteria:

- Windows + WSL supported;
- Linux supported;
- collection-aware project management;
- stable multi-project terminal grid;
- safe multi-device control;
- global CC Switch provider management;
- state detection;
- robust reconnect/recovery.

## Post-V1

Only after Claude runtime is stable, add a generic ToolAdapter for:

- Codex;
- Gemini CLI;
- OpenCode;
- other terminal-based coding agents.
