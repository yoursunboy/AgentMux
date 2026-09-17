# AgentMux

**Remote Multi-Agent Coding Workstation**

AgentMux is a self-hosted remote coding workstation for managing multiple persistent AI coding sessions from a browser, tablet, or phone.

## Status

This repository is at **version 0.1.0, end of Phase 1**. The project model and the server foundation
exist and work; the terminal does not exist yet.

| Works today | Does not exist yet |
| --- | --- |
| Server status and capability reporting | Any terminal or session runtime |
| Projects Root configuration, Windows/WSL and Linux path mapping | tmux, session start/stop, terminal output |
| Bounded discovery of project candidates | Claude Code launch, prompts, Hooks |
| Register an existing project | WebSocket traffic of any kind |
| Create a project, optionally with `git init` | CC Switch / provider switching |
| SQLite metadata store with migrations | Controller / viewer roles |
| Phase 1 workspace UI (global bar, project panel, project manager) | Codex, Gemini, OpenCode |

The product is not described here as if it were finished. `GET /api/server` reports
`terminalRuntimeImplemented: false` and `provider.integrated: false`, the UI renders no terminal, and
the provider switch is a disabled control that says "Coming later" rather than a working one. See
`docs/ROADMAP.md` for the phase table.

## Requirements

To build and run the server:

- Go 1.26 or newer;
- Git, if you want `Initialize Git` when creating a project (optional, and reported as missing rather
  than assumed).

To build the frontend:

- Node.js 22 or newer with npm;
- no global tooling: everything is a project devDependency.

Optional, and only relevant later:

- WSL2 with a distribution, for the Windows + WSL runtime mode;
- tmux and the Claude Code CLI, from Phase 2 and Phase 3.

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
| `-config <file>` | Use a specific JSON configuration file. |
| `-web-dir <dir>` | Serve the built frontend from this directory. |
| `-log-level`, `-log-format` | `debug`/`info`/`warn`/`error`, and `text`/`json`. |

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

The schema is `projects`, `settings`, and `schema_migrations`. There is no runtime table yet,
because there is no runtime state yet.

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

Each project panel displays the real AI terminal output and retains a prompt/input bar at the bottom. That is the target; today each panel states that the terminal is not available and shows the
project as the server has it recorded.

## Tests

```bash
go test ./...          # backend
cd web && npm test     # frontend (vitest)
```

The backend tests exercise real SQLite databases in temporary directories, real host adapters, and a
fully wired HTTP server through `httptest`. The frontend tests render the real components and drive
them the way a user would. Nothing in either suite touches a real project directory.

## Documentation reading order

1. `docs/PRODUCT_REQUIREMENTS.md`
2. `docs/PROJECT_MODEL.md`
3. `docs/ARCHITECTURE.md`
4. `docs/UI_SPEC.md`
5. `docs/PROTOCOL.md`
6. `docs/ROADMAP.md`
7. `docs/API.md` — what this build actually serves
8. `CLAUDE.md`
9. `.claude/rules/`

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

with reliable reconnect behavior and safe multi-device control. Phase 1 deliberately stops one step
short of it: the folder model, the store, and the management API are proven first, because a terminal
attached to the wrong directory is worse than no terminal.

CC Switch integration, Claude Hooks, state detection, notifications, and additional AI tools are later milestones.
