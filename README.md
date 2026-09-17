# AgentMux

**Remote Multi-Agent Coding Workstation**

AgentMux is a self-hosted remote coding workstation for managing multiple persistent AI coding sessions from a browser, tablet, or phone.

## Status

This repository is at **version 0.1.0, end of Phase 2**. The project model, the server foundation,
and the persistent session runtime exist and work. The terminal view does not exist yet.

| Works today | Does not exist yet |
| --- | --- |
| Server status and capability reporting | Any terminal view in the browser |
| Projects Root configuration, Windows/WSL and Linux path mapping | WebSocket traffic of any kind |
| Bounded discovery of project candidates | Claude Code launch, prompts, Hooks |
| Register an existing project | CC Switch / provider switching |
| Create a project, optionally with `git init` | Controller / viewer roles |
| SQLite metadata store with migrations | Codex, Gemini, OpenCode |
| Persistent tmux sessions that outlive the server | |
| Runtime start / stop / destroy, with reconciliation on restart | |
| Raw byte-accurate input, real PTY resize, sequenced output | |
| Phase 1 workspace UI (global bar, project panel, project manager) | |
| Phase 2 runtime controls: running/stopped, start, stop | |

The product is not described here as if it were finished. There is no terminal view — the runtime is
real, and the panel says plainly that the terminal UI arrives in Phase 4 rather than drawing an empty
rectangle. `provider.integrated` is false and the provider switch is a disabled control that says
"Coming later". See `docs/ROADMAP.md` for the phase table and `docs/RUNTIME.md` for how the runtime
works and what it does not do.

## Requirements

To build and run the server:

- Go 1.26 or newer;
- Git, if you want `Initialize Git` when creating a project (optional, and reported as missing rather
  than assumed).

To build the frontend:

- Node.js 22 or newer with npm;
- no global tooling: everything is a project devDependency.

For the session runtime:

- tmux, on the same side of the WSL boundary as the server.

**The AgentMux server runs where the sessions run.** On Windows that means inside WSL: tmux, the
shell a session hosts, and later the coding agent are all Linux processes, and a Windows-native
server cannot own them. Start the server from inside the distribution — see below. A server started
on Windows still serves the project model and reports the runtime as unavailable, with the reason in
`GET /api/server`.

## Running it

Two terminals, or two commands in one.

Backend:

```bash
go build -o bin/agentmux-server ./cmd/server
./bin/agentmux-server -projects-root "D:\AI\Projects"
```

On first run the server writes a default configuration file into its data directory and prints where
it put it, so "how do I change the Projects Root" has an answer in a file rather than in a flag you
have to remember.

Frontend, for development with hot reload:

```bash
cd web
npm install
npm run dev
```

Vite serves on `http://localhost:5173` and proxies `/api` to `http://127.0.0.1:8787`, so the browser
still sees a single origin. Point it elsewhere with `AGENTMUX_SERVER=http://host:port npm run dev`.

To let the Go server serve the UI itself, build it once and start the server with `-web-dir`:

```bash
cd web && npm run build
./bin/agentmux-server -projects-root "D:\AI\Projects" -web-dir web/dist
```

Then open `http://127.0.0.1:8787`.

### Windows: run the server inside WSL

The server is a Go binary and runs anywhere Go does, but a session runtime is a Linux process tree.
On Windows, start the server inside the distribution:

```bash
# from WSL
cd "/mnt/d/AI/Projects/2026 AgentMux/AgentMux"
go build -o bin/agentmux-server ./cmd/server
./bin/agentmux-server -projects-root "/mnt/d/AI/Projects" -web-dir web/dist
```

Or, from Windows, without leaving PowerShell:

```powershell
wsl -d Ubuntu-24.04 -- ./bin/agentmux-server -projects-root /mnt/d/AI/Projects -web-dir web/dist
```

Both are the same thing: the process that owns tmux is the process inside Linux. Pick your
distribution with `-d`; AgentMux never assumes a name, and `-runtime-distro` records the choice for
the paths it reports.

The Projects Root is spelled for the side the server runs on. A WSL server wants
`/mnt/d/AI/Projects`; a Windows server wants `D:\AI\Projects`. AgentMux maps between them for
display, so a project registered from either side shows the same directory.

## Configuration

Flags override the configuration file, which overrides the platform defaults. The flags worth
knowing:

| Flag | Meaning |
| --- | --- |
| `-projects-root <dir>` | A Projects Root to scan and manage. Repeatable for several roots. |
| `-data-dir <dir>` | Where AgentMux keeps its own state. Defaults to the platform config directory. |
| `-host`, `-port` | Listen address. Default `127.0.0.1:8787`. |
| `-runtime-mode <mode>` | `auto`, `native`, or `wsl`. |
| `-runtime-distro <name>` | The WSL distribution to use. |
| `-shell <path>` | The shell a terminal session runs. Defaults to the host's. |
| `-tmux-socket <name>` | The tmux socket AgentMux sessions live on. Default `agentmux`. |
| `-config <file>` | Use a specific JSON configuration file. |
| `-web-dir <dir>` | Serve the built frontend from this directory. |
| `-debug-api` | Enable the diagnostic endpoints under `/api/debug`. Off by default; they accept raw terminal input and output. |
| `-log-level`, `-log-format` | `debug`/`info`/`warn`/`error`, and `text`/`json`. |

`-tmux-socket` is worth knowing about: AgentMux runs its sessions on its own socket, so
`tmux ls` in your shell does not show them and your own tmux work is never touched by Destroy.
`tmux -L agentmux ls` does.

**The Projects Root is the one setting that matters.** AgentMux never invents one and never scans
outside the configured roots. The default on Windows is `D:\AI\Projects`. On Linux it is
`/mnt/d/AI/Projects` when that directory exists, otherwise `$HOME/Projects`, otherwise `/Projects`.
A request naming a path outside every configured root is refused with `path_outside_projects_root`
rather than quietly accepted.

Logs never contain API keys, tokens, secrets, or credentials.

## Where the data lives

| What | Where |
| --- | --- |
| SQLite database | `<data-dir>/agentmux.db` |
| Configuration file | `<data-dir>/config.json` |
| Logs | stderr |

The default data directory is `%AppData%\AgentMux` on Windows and `$XDG_CONFIG_HOME/AgentMux` (or
`~/.config/AgentMux`) on Linux, both taken from the platform's own configuration directory. Nothing
is written into your project folders: AgentMux reads them and records their paths, and the only write
it ever performs is the directory you explicitly ask it to create.

The schema is `projects`, `settings`, `project_runtime`, and `schema_migrations`.

`project_runtime` holds only what has to survive a restart: which backend, which session name, the
last known state, the canonical terminal size, and when it changed. It is not a log and not a buffer.
Terminal output lives in tmux's own scrollback and in memory for as long as the server is running;
nothing about it is written to SQLite, because a database row per chunk of terminal output would be
a write-amplifying copy of something tmux already keeps.

## Recommended local folder layout

The broader project collection root is:

```text
D:\AI\Projects
```

A collection/group folder may contain both development projects and non-code materials.

For AgentMux itself:

```text
D:\AI\Projects\
└─ 2026 AgentMux\
   ├─ Reference\
   ├─ Design\
   ├─ Notes\
   ├─ Archive\
   └─ AgentMux\
      ├─ .git\
      ├─ CLAUDE.md
      ├─ .claude\
      ├─ docs\
      └─ source...
```

The actual Git repository and Claude working directory is:

```text
D:\AI\Projects\2026 AgentMux\AgentMux
```

WSL runtime equivalent:

```text
/mnt/d/AI/Projects/2026 AgentMux/AgentMux
```

The outer `2026 AgentMux` folder is a collection/group folder, not the code repository. This is why
discovery does not treat first-level directories as projects: `AgentMux` is a project, and
`2026 AgentMux` is the collection it sits in.

Do not place a `CLAUDE.md` in the outer collection folder unless you intentionally want those parent rules inherited.

## Primary technology

Initial AI tool:

- Claude Code CLI

Initial persistent terminal runtime:

- tmux

Backend:

- Go

Frontend:

- React + TypeScript
- xterm.js (from Phase 4)

Storage:

- SQLite

Client:

- responsive Web / PWA

## Core UI

Top bar:

```text
● Online | Windows / WSL | Claude → DeepSeek Flash | Switch ▼
```

In this build the top bar reads `● Online | Windows / WSL (Ubuntu-24.04) | WSL | Claude [Coming later ▼]`.

Workspace:

```text
┌──────────────┬──────────────┬──────────────┐
│ Project A    │ Project B    │ Project C    │
│ Terminal     │ Terminal     │ Terminal     │
├──────────────┼──────────────┼──────────────┤
│ Project D    │ Project E    │ PROJECTS     │
│ Terminal     │ Terminal     │ + New / Open │
└──────────────┴──────────────┴──────────────┘
```

Each project panel displays the real AI terminal output and retains a prompt/input bar at the bottom. That is the target. Today each panel shows the runtime's real state — running or stopped, with Start and Stop — and says that the terminal view arrives in Phase 4.

## Tests

```bash
go test ./...          # backend
cd web && npm test     # frontend (vitest)
```

The backend tests exercise real SQLite databases in temporary directories, real host adapters, and a
fully wired HTTP server through `httptest`. The frontend tests render the real components and drive
them the way a user would. Nothing in either suite touches a real project directory, and nothing
writes outside a `t.TempDir()`.

The session suite is different from the rest: it drives a **real tmux** on a socket of its own, so it
needs tmux and it skips when tmux is not there. On Windows that means running it from inside WSL:

```bash
# from WSL, with the repository on /mnt
cd "/mnt/d/AI/Projects/2026 AgentMux/AgentMux"
go test ./internal/session/
```

A Windows shell can cross-compile the test binary for the distribution when Go is not installed
inside it:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/session.test ./internal/session/
wsl -d Ubuntu-24.04 -- /tmp/session.test -test.v
```

## Documentation reading order

1. `docs/PRODUCT_REQUIREMENTS.md`
2. `docs/PROJECT_MODEL.md`
3. `docs/ARCHITECTURE.md`
4. `docs/UI_SPEC.md`
5. `docs/PROTOCOL.md`
6. `docs/ROADMAP.md`
7. `docs/RUNTIME.md` — how sessions actually run, and what is known to be fragile
8. `docs/API.md` — what this build actually serves
9. `CLAUDE.md`
10. `.claude/rules/`

## Development principle

Do not attempt the entire product in one pass.

The first implementation milestone is to prove this chain:

```text
Browser
→ WebSocket
→ Go server
→ tmux
→ real Claude Code terminal
```

with reliable reconnect behavior and safe multi-device control. Phase 1 proved the folder model, the
store, and the management API first, because a terminal attached to the wrong directory is worse than
no terminal. Phase 2 proved the persistent runtime — sessions that outlive the server, byte-accurate
input, a real PTY — without putting a terminal in the browser yet, because a terminal view over a
runtime that loses output is worse than no terminal view.

CC Switch integration, Claude Hooks, state detection, notifications, and additional AI tools are later milestones.
