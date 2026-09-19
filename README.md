# AgentMux

**Remote Multi-Agent Coding Workstation**

AgentMux is a self-hosted remote coding workstation for managing multiple persistent AI coding sessions from a browser, tablet, or phone.

## Status

This repository is at **version 0.6.5, end of Phase 6.5**. The project model, the server foundation,
the persistent session runtime, the real Claude Code runtime, the web terminal, and the multi-project
workspace all exist and work — and Phase 6.5 added what a deployment needs: a systemd unit, an
installer, a configuration file, a health endpoint, and a documented answer to what happens when the
server restarts.

| Works today | Does not exist yet |
| --- | --- |
| Server status and capability reporting | Claude Hooks, Waiting/Completed state detection |
| Projects Root configuration, Windows/WSL and Linux path mapping | CC Switch / provider switching |
| Bounded discovery of project candidates | Codex, Gemini, OpenCode |
| Register an existing project | **Authentication** — see `docs/SECURITY.md` §1 |
| Create a project, optionally with `git init` | Billing, accounts, multi-tenancy |
| SQLite metadata store with migrations | |
| Concurrent input authority on one project, with a lease and a handshake | |
| Runtime start / stop / destroy, with reconciliation on restart | |
| **One tmux server and socket per project**, so one project's runtime cannot take another's down | |
| Raw byte-accurate input, real PTY resize, sequenced output | |
| **The real Claude Code CLI, started in a project's own directory and observed from the process table** | |
| **Claude survives a server restart; one project, one runtime, one Claude** | |
| **A live terminal in the browser — xterm.js over `GET /api/ws`, real Claude Code TUI, raw ANSI and Unicode** | |
| **Raw keyboard input, including Ctrl+C, and a Prompt Bar that shares the same input path** | |
| **Reconnect and snapshot recovery: a fresh screen with the exact sequence where the stream resumes** | |
| **A workspace of up to five projects at once, one WebSocket, the Project Manager in the last cell** | |
| **Controller and viewer roles: typing is a lease, and input from a viewer is refused** | |
| **A systemd service with `Restart=always`, so a server that died comes back by itself** | |
| **Runtimes that survive a service restart, and reconciliation that reports what it found** | |
| **`GET /health` for a supervisor, and `GET /api/server` with version, backend and uptime** | |
| **An installer, a configuration file, and YAML as well as JSON — with one decoder, so they cannot drift** | |
| **Logs with a component on every record, and nothing a terminal printed ever written to one** | |

The product is not described here as if it were finished. A project's runtime hosts the real Claude
Code CLI, and its terminal is in the browser: the bytes are the terminal's own, the keystrokes are the
keyboard's own, and closing the tab leaves Claude running. Phase 6.5 did not add product capability —
it made what exists deployable, upgradeable, recoverable, monitorable and able to run for a long time.
What is still missing is around that edge: **no authentication**, so every browser that can reach the
port can type, and no hooks or task state, so the workspace cannot yet tell you that an agent is
waiting for you.

Phase 6.5 was validated on a real Linux server — systemd 255, tmux 3.4, Ubuntu 24.04 under WSL2 —
where the installer, the service, all three recovery cases, the upgrade path and the health endpoint
were exercised rather than argued. What could not be validated there is stated where it belongs rather
than glossed: see `docs/DEPLOYMENT.md` §Platforms and §Troubleshooting.

`provider.integrated` is false and the provider switch is a disabled control that says "Coming later".
See `docs/ROADMAP.md` for the phase table, `docs/DEPLOYMENT.md` for putting it on a server,
`docs/WORKSPACE.md` for the grid, `docs/TERMINAL.md` for the transport, and `docs/RUNTIME.md` for how
the runtime works and what it does not do.

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

## Deploying it on a server

For anything you intend to keep running, install it as a service rather than starting it by hand:

```sh
sudo ./deploy/linux/install.sh --root /srv/projects
```

That builds the server and the frontend, creates an unprivileged `agentmux` service account, installs
everything under `/opt/agentmux`, writes a configuration file, installs and enables a systemd unit,
starts it and checks `/health`.

There is no Docker image and this phase does not add one — the runtime is tmux driving real ptys in a
real filesystem, and a container between the two would add a layer whose failure modes are
indistinguishable from tmux's own.

On a Windows host the server belongs **inside** the WSL distribution, with systemd enabled. There is
no Windows-native tmux and there will be no Windows-native service.

| Document | What it covers |
| --- | --- |
| [`deploy/linux/README.md`](deploy/linux/README.md) | The operational guide: install, the unit, recovery, upgrade, WSL2, uninstall, troubleshooting |
| [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) | The reference behind it: every setting, the log rules, the health contract, the performance baseline |
| [`docs/BACKUP.md`](docs/BACKUP.md) | Backing up and restoring the database, and what a restore does not get you |
| [`docs/SECURITY.md`](docs/SECURITY.md) | What the absent authentication means, and the checklist before exposing the port |

`deploy/linux/recovery-test.sh` runs the three recovery cases against a real installation, and
`deploy/linux/loadtest.py` takes the performance baseline.

## Configuration

Precedence, and it is worth internalising before editing anything:

```
CLI arguments  >  AGENTMUX_* environment  >  the configuration file  >  built-in defaults
```

A flag is what a person types when they want this run to differ; an environment variable is what a
service manager sets for every run; the file persists and is edited rarely. The more specific
statement wins.

The file may be **YAML or JSON**. `.yaml` and `.yml` are read as YAML and everything else as JSON, and
there is exactly one decoder behind both: a YAML document is converted to JSON and handed to the same
unmarshaler, so the two formats cannot drift apart in key names, types or error behaviour.
`config/agentmux.example.yaml` documents every key with its default.

The flags worth knowing:

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
| `-config <file>` | Use a specific configuration file. YAML or JSON, chosen by extension. |
| `-web-dir <dir>` | Serve the built frontend from this directory. |
| `-debug` | Include this machine's filesystem layout in `GET /api/server`. Off by default; see `docs/SECURITY.md` §7. |
| `-log-level`, `-log-format` | `debug`/`info`/`warn`/`error`, and `text`/`json`. |
| `-version` | Print the version, the git commit it was built from and the phase, then exit. |

In the configuration file these are `terminal.tmuxBinary` and `terminal.tmuxSocketDir`; the
environment overrides are `AGENTMUX_TMUX_BINARY` and `AGENTMUX_TMUX_SOCKET_DIR`. The data directory is
deliberately **not** settable from the file: the default configuration file lives inside the data
directory, so a file that could move it would be deciding where it itself is. Set it with `-data-dir`
or `AGENTMUX_DATA_DIR`.

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

Logs carry a timestamp, a level, a component and an event, and never contain terminal output, anything
a user typed, a prompt, an API key, a token, a secret or a credential. `docs/DEPLOYMENT.md` §Logs has
the rule and the rotation, which under systemd is the journal's job and not AgentMux's.

## Health and server information

Two endpoints answer "is it up", and they answer different questions.

`GET /health` is the one a supervisor, a load balancer or a person with `curl` asks. It always returns
200, because a monitor should not have to parse a body to learn the process is alive:

```json
{"status":"ok","version":"0.6.5","commit":"31e0f208a3aa","runtime":"available"}
```

`status` is liveness; `runtime` is readiness, and reads `unavailable` on a host with no tmux — which
is a working server that cannot host a terminal. It carries no credential, no filesystem detail and
no secret, and it sits deliberately outside `/api`, so a monitor need not track the protocol version.

`GET /api/server` is the richer one: version, operating system, runtime backend, whether tmux is
available, and uptime, alongside the projects roots and the dependency probes. It reports the machine's
*platform* rather than its identity — the server never asks the operating system for a hostname or a
username, so there is none to withhold — and four of its own paths (the data directory, the database,
the config file and the frontend directory) are absent unless debug mode is on. The tmux socket
directory is the one path published in every mode, because it is what an orphaned session is explained
from. Turning on debug is a separate decision from turning up log verbosity, and `docs/SECURITY.md` §7
says exactly what appears and why.

Requests that change something are checked against their `Origin`: a page from another site cannot
create a project, start a runtime or open a terminal, and a request from a program — `curl`, a script —
is unaffected because it sends no `Origin` at all. `docs/SECURITY.md` §4 is the rule and the reasoning.

## Where the data lives

| What | Where |
| --- | --- |
| SQLite database | `<data-dir>/agentmux.db` |
| Configuration file | `<data-dir>/config.json`, or `/etc/agentmux/agentmux.yaml` under `deploy/linux` |
| tmux sockets, one per project | `<data-dir>/tmux/<projectId>.sock` |
| Logs | stderr, which is the journal under systemd |

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

Each project panel displays the real AI terminal output and retains a prompt/input bar at the bottom. That is what this build does: up to five project panels and the manager on a page, each with its own live terminal, its state, its menu and its prompt bar — see `docs/WORKSPACE.md`. What the diagram above does not show is that the sixth cell is the Project Manager on every page, not only the last one.

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
end-to-end suites drive headless Chrome against a running server, real tmux sessions and the real
frontend. They are **in the repository** — `web/e2e/` — with a runner that builds the fixture they
need from nothing, so they either work or say what is missing:

```bash
cd web && npm run test:e2e
```

It needs a WSL distribution with tmux and the Claude Code CLI, Go on the machine running it, Node 22+,
and Chrome (which Playwright drives as a channel rather than downloading). `docs/ROADMAP.md` Phase 5
lists what the five suites cover and `docs/WORKSPACE.md` §6 is where the measured geometry is.

**An interrupted run cleans up after itself.** Ctrl-C, a `SIGTERM`, or a suite killed by hand arrives
while seven tmux servers and a server process exist, and stopping where it stands is what leaves them
there — the next run then removes the temporary directory, which takes the socket files with it and
leaves the servers running with nothing on disk to name them by. So the signals are handled: the
runtimes are destroyed through the API, and whatever the API cannot reach — a server that died in the
middle of a suite leaves seven runtimes no `DELETE` can be sent to — is ended by socket and by process.
A cleanup that could not reach something says which project, which runtime and what the error was
rather than reporting success; at the end of the run the leftovers are looked for and reported, and
nothing found is deleted, because a check that also deletes cannot tell this run's leftovers from
somebody else's tmux.

The deployment in `deploy/linux/` is tested the same way — against a real installation rather than in
a unit test:

```bash
sudo ./deploy/linux/recovery-test.sh   # the three recovery cases, against the running service
python3 deploy/linux/loadtest.py       # the performance baseline
```

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
11. `docs/WORKSPACE.md` — the grid, its slots, its pages, and its multi-device limit
12. `docs/MULTI_DEVICE.md` — control, the lease, and its transfer handshake
13. `docs/DEPLOYMENT.md` — putting it on a server, and what a restart does and does not bring back
14. `docs/BACKUP.md` — what to back up, what not to, and how to restore
15. `docs/SECURITY.md` — start here before the port is reachable from anywhere but this machine
16. `docs/AGENT_EVENTS.md` — what happened, as opposed to what is true now
17. `deploy/linux/README.md` — the operator's guide to the two above
18. `CLAUDE.md`
19. `.claude/rules/`

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

Phase 5 built the workspace: up to five projects on one page, each its own terminal, the Project
Manager in the last cell of every page, pages when there are more projects than that, and Focus for
working in one of them. Two defects came out of the browser suites rather than the unit tests, which
is the argument for having them.

Phase 6 adds explicit controller and viewer ownership, so the same project can be open on a PC, a
tablet and a phone at once while exactly one client controls input and resize.

Phase 6.5 added no product capability at all, deliberately. It made what was already there
deployable, upgradeable, recoverable, monitorable and able to run for a long time: a systemd unit, an
installer, a YAML-or-JSON configuration file behind one decoder, a health endpoint for a supervisor,
a component on every log record, a version and a git commit in the binary, a documented recovery model
with a script that exercises it, and a measured performance baseline. Its two honest limits are stated
in the documents rather than glossed: the in-script build path was not exercised inside the test
distribution, which has no Go or Node toolchain, and the final `claude` link in the deployment chain
was not exercised end to end on that host because the CLI belongs to a human's account and the service
account cannot reach it. Claude Hooks, state detection, notifications, provider switching, additional
AI tools, accounts and billing are later milestones.
