# AgentMux

**Remote Multi-Agent Coding Workstation**

AgentMux is a self-hosted remote coding workstation for managing multiple persistent AI coding sessions from a browser, tablet, or phone.

## Status

This repository is at **version 0.1.0, end of Phase 4**. The project model, the server foundation, the
persistent session runtime, the real Claude Code runtime, and the web terminal all exist and work.

| Works today | Does not exist yet |
| --- | --- |
| Server status and capability reporting | Controller / viewer roles, or any lease on typing |
| Projects Root configuration, Windows/WSL and Linux path mapping | Claude Hooks, Waiting/Completed state detection |
| Bounded discovery of project candidates | CC Switch / provider switching |
| Register an existing project | Codex, Gemini, OpenCode |
| Create a project, optionally with `git init` | The multi-project 1×3 / 2×3 grid |
| SQLite metadata store with migrations | Multi-device control |
| Persistent tmux sessions that outlive the server | Authentication |
| Runtime start / stop / destroy, with reconciliation on restart | |
| **One tmux server and socket per project**, so one project's runtime cannot take another's down | |
| Raw byte-accurate input, real PTY resize, sequenced output | |
| **The real Claude Code CLI, started in a project's own directory and observed from the process table** | |
| **Claude survives a server restart; one project, one runtime, one Claude** | |
| **A live terminal in the browser — xterm.js over `GET /api/ws`, real Claude Code TUI, raw ANSI and Unicode** | |
| **Raw keyboard input, including Ctrl+C, and a Prompt Bar that shares the same input path** | |
| **Reconnect and snapshot recovery: a fresh screen with the exact sequence where the stream resumes** | |
| **Window resize, local scroll that is never yanked to the bottom, and touch keys on a tablet** | |

The product is not described here as if it were finished. A project's runtime hosts the real Claude
Code CLI, and its terminal is now in the browser: the bytes are the terminal's own, the keystrokes are
the keyboard's own, and closing the tab leaves Claude running. What is missing is around the edge of
that — no controller or viewer roles, so every browser that can reach the server can type; no
authentication; and one project on screen at a time, because the grid is Phase 5.

One thing about the development host rather than this build: the WSL Claude Code is installed but
**not signed in**, so a terminal opened on it shows Claude Code's own sign-in screen rather than a
conversation. That is Phase 3's outstanding item, and Phase 4 was specified to leave authentication
alone — nothing was copied, faked, or worked around to hide it. Everything the terminal does is
verified with a real shell in a real tmux session; the Claude TUI itself is verified as far as Claude's
own sign-in screen, which is a real full-screen TUI and exercises the same paths.

`provider.integrated` is false and the provider switch is a disabled control that says "Coming later".
See `docs/ROADMAP.md` for the phase table, `docs/TERMINAL.md` for the terminal protocol, and
`docs/RUNTIME.md` for how the runtime works and what it does not do.

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

**Which tmux.** AgentMux is tested against tmux **3.4** (the version Ubuntu 24.04 ships, and the
version inside the WSL distributions this project is developed on) and tmux **3.7c** (the latest
stable release at the time of the Phase 2.5 measurements). Both were run side by side through the same
stress harness — see the compatibility matrix in `docs/RUNTIME.md` §14. Neither is *required*:
AgentMux does not refuse to start on a different version, and it reports the one it found in
`GET /api/server` (`tmux.version`, alongside `tmux.binary`). If you are installing tmux for AgentMux
on Windows + WSL, **3.4 or newer from your distribution is the supported and tested choice**; there is
no measured advantage to building a newer one, and AgentMux never treats "newest" as better. Windows
has no native tmux, which is the whole reason the server belongs inside WSL.

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
| `-tmux-binary <path>` | The tmux executable every project's runtime runs. Default: `tmux` on `PATH`. Set it to `/usr/bin/tmux` or a user-local path to pin the exact binary. |
| `-tmux-socket-dir <dir>` | Directory holding one tmux socket per project. Default `<data-dir>/tmux`. |
| `-tmux-socket <name>` | **Deprecated, no effect.** Accepted and warned about, never silently reinterpreted. See below. |
| `-config <file>` | Use a specific JSON configuration file. |
| `-web-dir <dir>` | Serve the built frontend from this directory. |
| `-log-level`, `-log-format` | `debug`/`info`/`warn`/`error`, and `text`/`json`. |

In the configuration file these are `terminal.tmuxBinary` and `terminal.tmuxSocketDir`; the
environment overrides are `AGENTMUX_TMUX_BINARY` and `AGENTMUX_TMUX_SOCKET_DIR`.

**One project, one tmux server, one socket.** Since Phase 2.5 each project's runtime gets its own
tmux server, addressed as `tmux -S <data-dir>/tmux/<projectId>.sock`. tmux identifies a server by its
socket path, so two paths are two servers, and a project's runtime cannot take another project's down:
destroying one runtime, or losing its server, leaves the others untouched. The socket name comes from
the stable project id and never from the display name, which changes.

That replaced a single shared socket, which is what `-tmux-socket` used to select. The setting is
still accepted so an existing configuration file does not fail to load, and the server logs a
deprecation warning naming the replacement — it is **not** reinterpreted as a socket directory or as a
socket name, because either reading would move every runtime to a path you never named. Remove it.

Because sessions live on AgentMux's own per-project sockets, `tmux ls` in your shell does not show
them and your own tmux work is never touched by Destroy. To look at one, name its socket:
`tmux -S <data-dir>/tmux/<projectId>.sock ls`.

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
| tmux sockets, one per project | `<data-dir>/tmux/<projectId>.sock` |
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

Each project panel displays the real AI terminal output and retains a prompt/input bar at the bottom. That is the target. Today one project panel is on screen at a time — its runtime's state, its Start and Stop controls, its live terminal, and its prompt bar — and the grid of six is Phase 5.

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
needs tmux and it skips when tmux is not there. The terminal transport suite is different again: it
opens real WebSocket connections against a real server, and one of its cases measures what happens to
a client that stops reading in the middle of a burst. On Windows both mean running from inside WSL:

```bash
# from WSL, with the repository on /mnt
cd "/mnt/d/AI/Projects/2026 AgentMux/AgentMux"
go test ./internal/session/
go test ./internal/terminal/
```

A Windows shell can cross-compile a test binary for the distribution when Go is not installed inside
it:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o /tmp/session.test ./internal/session/
wsl -d Ubuntu-24.04 -- /tmp/session.test -test.v
```

**The browser is a test target too, and it is not covered by the two commands above.** A terminal that
is correct over a socket can still be drawn wrong: the fidelity this phase promises is about pixels,
Unicode widths and touch targets, and those need a real browser attached to a real xterm.js. The
end-to-end suites drive headless Chrome against a running server, a running tmux session and the real
frontend, and they live outside the repository because they need a browser and a fixture rather than
because they are optional — `docs/ROADMAP.md` Phase 4 lists what they cover and `docs/TERMINAL.md` §6
is where the fidelity limits they measure against are stated.

## Documentation reading order

1. `docs/PRODUCT_REQUIREMENTS.md`
2. `docs/PROJECT_MODEL.md`
3. `docs/ARCHITECTURE.md`
4. `docs/UI_SPEC.md`
5. `docs/PROTOCOL.md` — the original client/server sketch, annotated with what was built
6. `docs/ROADMAP.md`
7. `docs/RUNTIME.md` — how sessions actually run, and what is known to be fragile
8. `docs/API.md` — what this build actually serves
9. `docs/CLAUDE_RUNTIME.md` — how the real Claude Code CLI is resolved, launched, observed and stopped
10. `docs/TERMINAL.md` — the terminal protocol, the snapshot boundary, and its fidelity limits
11. `CLAUDE.md`
12. `.claude/rules/`

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
runtime that loses output is worse than no terminal view. Phase 3 proved the real Claude Code CLI
inside that runtime: resolved by the code that launches it, started in the project's own directory
with no auto-approval flags added, observed from the process table rather than from the screen, and
still running after the server it was started under has gone. See `docs/CLAUDE_RUNTIME.md`.

Phase 4 completed that chain in the browser. The WebSocket carries the runtime's own bytes rather than
a rendering of them, the terminal is a real xterm.js rather than a `<pre>`, and the terminal's owner is
still tmux: a browser connecting, disconnecting or crashing does not touch the control connection, and
the terminal survives the AgentMux server being restarted underneath it. The two things this phase
deliberately did not do are the ones that would have made it a different phase — no controller or
viewer roles, and no multi-project grid.

Phase 5 expands the proven terminal into the multi-project 1×3 / 2×3 workspace grid with the Project
Manager occupying the final slot. Claude Hooks, state detection, notifications, provider switching,
and additional AI tools are later milestones.
