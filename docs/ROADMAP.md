# AgentMux Roadmap

## Implementation status

As of version 0.6.5:

| Phase | Status | Note |
| --- | --- | --- |
| Phase 0 — Foundation and documentation | **Done** | Workspace, `CLAUDE.md`, `.claude/rules/`, docs, environment checks. |
| Phase 1 — Project model and server foundation | **Done** | Config, SQLite, HostAdapter, discovery, register/create API, server status, Phase 1 UI. |
| Phase 2 — Persistent session runtime | **Done** | `SessionBackend` + `TmuxBackend`, runtime manager, persistence and reconciliation, runtime API, Phase 2 UI. No WebSocket, no real terminal view. |
| Phase 2.5 — tmux stability validation and runtime isolation | **Partial** | One tmux server and socket per project; Control Monitor lifecycle; a four-environment tmux compatibility matrix. Complete except for re-running the pure-Linux columns, which need interactive authentication on the test host. See below. |
| Phase 3 — Real Claude Code runtime | **Partial** | `internal/claude` launcher, agent lifecycle in `internal/session`, agent API, real-CLI integration tests. Runtime integration is complete and verified against the real CLI; the one outstanding item is that the WSL Claude Code is not signed in, so E2E conversation is not yet possible on this host. See below. |
| Phase 4 — Web terminal | **Done** | `/api/ws` transport, snapshot and live output, raw input, Prompt Bar, resize, local scroll, reconnect and resync, touch keys. Verified in a real browser, including a tablet in emulation. See below. |
| Phase 5 — Multi-project workspace | **Done** | The workspace grid, slots, the Project Manager as the last cell, pagination, Focus and full screen, and the browser suites moved into the repository. Verified against real Chrome at four viewports. See below. |
| Phase 6 — Multi-device control | **Done** | One controller and many viewers per project, a lease with a grace period, a handshake for handing control over, and input and resize authority on the server. Verified in two real browser contexts at once — a desktop and an emulated tablet. See below. |
| Phase 6.5 — Stabilization | **Done** | Not a capability phase. systemd unit, installer, configuration file, health endpoint, log components, version reporting, a documented recovery model with a script that exercises it, and a measured performance baseline. Validated on a real Linux server. See below. |
| Phase 7.1 — Agent event foundation | **Done** | Not a capability phase, and deliberately invisible in the product. The event model, `agent_events`, one write path, the runtime bridge, and two read-only timeline endpoints. No Claude Hooks, no output parsing, no state inference, no UI. See below. |
| Phase 7.2 — Task and agent session model | **Done** | Not a capability phase either, and the last piece of Phase 7 that is not about Claude. `tasks` and `agent_sessions`, their lifecycles, the service that owns both, the eight endpoints that read and move them, and no UI. Nothing here starts a process. See below. |
| Phase 7.3A — Claude integration discovery | **Done** | Not an implementation phase. What the installed Claude Code can actually report, measured rather than assumed, and the design that follows from it. No production code, database, API, or frontend was changed. See `docs/CLAUDE_INTEGRATION.md`. |
| Phase 7.3B-0 — Runtime environment validation | **Done** | Also not an implementation phase. Whether the mechanism 7.3A found works on Windows, on WSL, and on pure Linux, measured rather than assumed. Windows verified end to end; WSL for everything but the model turn; pure Linux not at all, for want of interactive authentication. See `docs/CLAUDE_RUNTIME_VALIDATION.md`. |
| Phase 7.3B-1 — ClaudeAdapter foundation | **Done** | The adapter itself: Claude's hooks and its stream-json translated into `agent.*` events and written through the existing event service. Seven event types, one write path, no database, no API, no UI. Validated end to end against Windows Claude Code 2.1.278. See `docs/CLAUDE_ADAPTER.md`. |
| Phase 7.3B-2 — Runtime binding | **Done** | `internal/agent` connects the adapter to the product: one call starts the runtime if needed, dictates Claude's session id, attaches a hook receiver, writes the settings document, launches, and binds the attempt. Fixed the silently-refused `session.created` / `session.status_changed` payloads on the way. No schema change, no UI. See `docs/AGENT_RUNTIME_BINDING.md`. |
| Phase 7.3C-1 — Agent state projection | **Done** | `internal/agentstate` folds `agent_events` into what is true about an agent now, stored in a new `agent_states` table and read by two endpoints. Driven from inside the event service, so the projection can be deleted and rebuilt from the log at any moment. No UI, no write endpoint. See `docs/AGENT_STATE.md`. |
| Phase 7.3C-2 — Attention and action queue | **Done** | `internal/attention` answers "what needs me": a level per attempt and a queue of pending actions, both projected from the same log, with three read endpoints and no way to answer an action. See `docs/AGENT_ATTENTION.md`. |
| Phase 7.4A — Controller dashboard aggregation | **Done** | `internal/controller` joins the project, state, attention and action services into one response, with two GET routes and no writes. Four queries for any number of projects, no cache, no table. See `docs/CONTROLLER_API.md`. |
| Phase 7.4B-1 — Controller web dashboard | **Done** | The first read-only screen: `/dashboard` renders the controller aggregation as a grid of project cards, with a server bar, status tones, and a layout from three columns to one. One request, a five-second poll, no terminal and no writes. See `docs/CONTROLLER_UI.md`. |
| Phase 7.4B-2A — Terminal viewer | **Done** | The console stopped being a page of numbers: every card whose runtime is up carries the project's real terminal, drawn by the workspace's own xterm instance over the page's single existing WebSocket. Its read-only behaviour comes from the protocol's viewer position rather than from a flag, and no backend change was needed. See `docs/TERMINAL_VIEWER.md`. |
| Phase 7.4B-2B — Controller lease and input | **Done** | A client can ask to own a terminal's input, be granted it, type at it, and give it back — one controller at a time, never preempted, expiring on its own when the holder goes away. The lease is in memory only and no keystroke is ever recorded. The console types and never reshapes the pty. See `docs/TERMINAL_CONTROLLER.md`. |
| Phase 7.3B — Claude event adapter | **Partial** | The adapter and its wiring are built as 7.3B-1 and 7.3B-2. What remains is the binding's persistence across a restart, and the permission question settled before a Task's status can be derived from what Claude says. See below. |

What this means in practice: `GET /api/server` reports `terminalRuntimeImplemented: true`, and
`features.terminal` is true only when the server is running where tmux is (`runtimeAvailable`) and tmux
is actually installed there. `GET /api/server` also reports which tmux it found
(`tmux.available`, `tmux.version`, `tmux.binary`), and which Claude Code it would launch
(`claude.path`, `claude.version`, `claude.command`), with `features.claudeRuntime` saying whether one
can be started here at all. The UI offers Start/Stop runtime, and a panel for a running project holds
the terminal itself: the live screen, a Prompt Bar, a resize that follows the window, and a way to ask
for the screen again. Starting Claude is still available only through the API — the runtime panel
shows the agent's state and offers no button for it, which is a gap in the UI rather than in the
runtime. The provider still reports `integrated: false`, because Phase 8's provider switching is what
that field describes. Nothing in the running product claims to do more than the table above.

One consequence of Phase 4 being done is worth stating where the phases are listed: the terminal is
only as useful as what is inside it. The WSL Claude Code on the development host is installed but not
signed in, so a terminal opened there shows Claude Code's own sign-in screen. That is a property of
the host, not of this build — see Phase 3 — and Phase 4 is specified to leave authentication alone.

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
- Off-by-default diagnostic endpoints under `/api/debug`. **Deleted in Phase 4**, not gated: no flag,
  no environment variable, no handler.
- Frontend: Start/Stop runtime controls and an honest "Terminal UI coming in Phase 4".

Not built, deliberately: Claude Code launch, any WebSocket, any real terminal view, controller/viewer
roles, prompt bar, Claude Hooks.

## Phase 2.5 — tmux stability validation and runtime isolation

Status: **partial**. See `docs/RUNTIME.md` §12–§14 for the design, the Control Monitor, and the
compatibility matrix.

Goal:

Two questions, answered by measurement rather than by inference. First, is the server-disappearance
anomaly observed during Phase 2 a property of tmux 3.4, of WSL, or of something AgentMux does? Second,
whatever the answer, shrink the blast radius of a runtime fault to a single project.

Deliver:

- a tmux version × runtime environment comparison matrix — one unchanged stress harness across WSL2
  and pure Linux, on tmux 3.4 and a source-built 3.7c;
- one tmux server and socket per project;
- a single, server-owned Control Monitor per runtime;
- multi-socket reconciliation with explicit stale-socket and orphan handling;
- a configurable tmux binary used by every code path;
- diagnostics reporting which tmux was found.

What was built:

- **Runtime isolation.** Each project gets its own tmux server via `tmux -S
  <data-dir>/tmux/<projectId>.sock`, socket directory mode `0700`. The socket name derives from the
  stable project id, never from the display name. Verified end to end in real WSL: with three projects
  running, killing one project's server leaves the other two running and correctly reported; the same
  for destroying a runtime and for stopping the AgentMux server entirely.
- **One Control Monitor per runtime.** A single long-lived `tmux -C` client owned by the server
  runtime, not by any browser. Browsers never create or kill it, so opening the UI on a tablet cannot
  disturb a session. A monitor whose client dies re-establishes its own stream; it is not the runtime
  owner and never destroys a runtime on its way out.
- **Reconciliation across many sockets.** On startup the socket directory is scanned, each socket
  queried, and the result compared with `project_runtime`. A runtime whose server is gone stops
  reporting `RUNNING`. Orphan runtimes are recorded and left alone. Stale sockets are confirmed dead,
  re-checked after a delay, and only then removed, with a log line.
- **`tmuxBinary`.** One resolved executable is used by every path — version detection, dependency
  detection, create, list, attach, send, resize, stop, destroy, reconciliation, diagnostics, and the
  stress harness. No `exec.Command("tmux", …)` reaches `PATH` behind its back.
- **Compatibility matrix.** One harness, unchanged, four environments: **0 server deaths in 21,000
  rounds** across WSL2 and pure Linux on both tmux versions. Reported as exactly that and nothing
  stronger — not "fixed", not "100% stable". The Phase 2 anomaly did not reproduce in any environment;
  the historical record of it is preserved in `docs/RUNTIME.md` §10, with the earlier explanation
  explicitly withdrawn rather than quietly edited out.

What this phase deliberately did **not** do, and Phase 4 must not assume: render a terminal, carry
WebSocket traffic, add a prompt bar, add controller/viewer roles, or integrate CC Switch. It also did
not restore the shared tmux server — per-project isolation is an architectural fault boundary, not a
workaround for a bug.

Not yet verified: the two pure-Linux matrix columns were measured earlier in the phase and were not
re-run for the final report, because the test host requires interactive authentication this
environment cannot provide. They are carried as recorded. That single item is why this phase is
Partial rather than Done; nothing was simulated or fabricated in its place.

## Phase 3 — Real Claude Code runtime

Status: **partial**. See `docs/CLAUDE_RUNTIME.md` for the design, the measurements, and the
limitations.

Goal:

Run the real Claude Code CLI inside the persistent runtime a project already has, and prove it can be
started, typed at, observed, stopped, and reconnected to — without the runtime being the thing that
holds it.

Deliver:

- launch Claude in registered project working directory;
- send input;
- receive real TUI output;
- disconnect leaves Claude alive;
- reconnect to existing session.

What was built:

- **`internal/claude`** — resolves which Claude Code is installed: the configured binary or `claude`
  on `PATH`, symlinks followed, `--version` probed, and the command line rendered as one shell-quoted
  word. It reads no credential, dumps no environment, and reports no authentication state.
- **Agent lifecycle in `internal/session`** — start, adopt, observe, interrupt. One project → one
  runtime → one interactive Claude. The agent is typed into the runtime's shell rather than executed
  by the server, which is what makes it visible, interruptible, part of the scrollback, and a
  survivor of a server restart.
- **`internal/session/agentproc.go`** — the agent's liveness is read from `/proc`: a descendant of the
  pane leader whose `/proc/<pid>/exe` is the resolved binary. The terminal's text is never parsed, and
  `pane_current_command` is never used as identity (measured: it reports `2.1.274` for the command
  AgentMux types). `StartedAt` comes from the kernel, so it survives a restart.
- **No schema change.** Agent liveness is read live from the process table. `project_runtime` still
  holds only what a restart cannot recompute.
- **Agent API** — `GET`/`POST start`/`POST stop` under `/api/projects/{id}/runtime/agent`, plus
  `claude` and `features.claudeRuntime` in `GET /api/server`.
- **Real-CLI integration tests** in `cmd/server/claude_e2e_test.go`, behind two opt-in environment
  gates so that a normal `go test ./...` starts no Claude and spends nothing.
- **A defect found and fixed by the tests**: a stop the CLI declined was reported as a stop nobody had
  asked for, because the observed-running branch cleared the request that had been recorded. A
  declined interrupt is now reported as running-and-asked, with the reason.

Interactivity is preserved: AgentMux adds no `--dangerously-skip-permissions`, no `--permission-mode`,
no `--allowedTools`, no `--model` and no `-p`, and there is a test that asserts none of them appears in
the rendered command. Claude asks the user, on a terminal the user owns.

Not built, deliberately: any WebSocket, any terminal view, a prompt bar, controller/viewer roles,
Claude Hooks, Waiting/Completed state detection, and any CC Switch adapter. The exit code is
unobtainable and is reported as unobtainable rather than guessed — see `docs/CLAUDE_RUNTIME.md` §6.

Outstanding: on this development host the WSL Claude Code resolves and launches but is **not signed
in**, so an agent reaches Claude's authentication screen rather than a conversation. That is a
Claude-side configuration and not an AgentMux defect, and nothing was copied, faked or worked around
to hide it. It is the single reason this phase is Partial rather than Done.

## Phase 4 — Web terminal

Status: **done**. See `docs/TERMINAL.md` for the protocol, the snapshot boundary, the limits and the
known fidelity limits.

Goal:

Show a project's real terminal in a browser and let a person work in it — the same bytes a terminal
emulator would get, the same keystrokes a keyboard would send — without the browser becoming a second
path to the terminal or a second owner of it.

Deliver:

- xterm.js;
- WebSocket;
- ANSI/TUI fidelity;
- raw keyboard input;
- Prompt Bar;
- reconnect snapshot;
- touch terminal keys.

What was built:

- **`internal/terminal`** — the transport. One WebSocket per browser, subscriptions multiplexed on
  it, output in binary frames and control in JSON. It subscribes to the runtime manager and has no
  tmux socket path of its own, so a browser connecting or disconnecting never touches the control
  connection. Limits on message size, input size, subscription count and terminal geometry; a
  documented set of error codes; and a logging rule that excludes terminal bytes in both directions.
- **`GET /api/ws`** — the endpoint, with an Origin policy that refuses a page from another site and an
  explicit protocol-version check that refuses a client that states a version this server does not
  speak.
- **Snapshots and the boundary** — a fresh screen for a client that has just connected or reconnected,
  carrying the highest sequence number it already contains, so the client knows exactly where the live
  stream resumes. Taken with a short settle so the boundary is usually exact; the ceiling on that
  settle is documented as a duplicate risk rather than hidden.
- **`web/src/terminal/`** and **`TerminalView`**, **`ProjectTerminal`**, **`PromptBar`** — the client:
  xterm.js with the Unicode 11 width tables, raw input over one channel shared with the Prompt Bar,
  debounced resize, a local scroll model with a "new output" affordance rather than a viewport that is
  yanked to the bottom, reconnection, and a button that asks the server for the screen again.
- **Touch keys** — escape, tab, Ctrl+C and the four arrows, shown only where the pointer is coarse,
  each sending exactly the bytes the real key sends. A soft keyboard no longer resizes the pty.
- **The debug endpoints were deleted**, not gated — see `docs/API.md`.
- **Layout widths corrected for a tablet**: three panels now need a desktop's width, so an iPad in
  portrait gets the terminal full width (~100 columns) instead of a 470-pixel panel, and in landscape
  it gets a project panel and the manager side by side (~107 columns).

Verification: unit and integration tests in both languages, plus four browser suites driven by
Playwright against a real server, a real tmux session and a real xterm.js — protocol and backpressure
over a raw socket, the interface itself, snapshot recovery and the tablet, and the theme.

Outstanding: nothing in the phase. The limit worth repeating is that a snapshot is the pane as tmux
reports it, so a full-screen program that keeps state it never repaints is restored to what it looks
like rather than to what it knows. `docs/TERMINAL.md` §6 lists exactly what survives.

## Phase 5 — Multi-project workspace

**Status: Done.** The workspace grid, its slot model, the Project Manager as the
last cell of every page, pagination, Focus and full screen, and the browser
suites moved into the repository.

**Goal.** Turn the proven single-project terminal into a workspace: several
projects visible at once, each one its own terminal, none of them able to
disturb the others.

**Deliver.**

- `web/src/workspace/` — the slot model, the layout hook, the page preference,
  the full-screen hook, and the workspace state;
- `WorkspaceGrid`, `WorkspacePager`, `PanelMenu`, `PanelBoundary`,
  `ConfirmDialog`, `ProjectDetailsDialog`;
- a grid of up to two rows, three columns wide, holding five projects and the
  manager;
- the Project Manager's two lists — in the workspace, and registered;
- pagination, with the manager on every page;
- Focus and full screen as client layouts that restart nothing;
- `PATCH /api/projects/{id}` — the one mutable field on a project;
- `web/e2e/` — five browser suites and the runner that builds their fixture.

**What was built.** A project is in the workspace exactly when it holds a
reserved slot, and that is stored server-side, so the workspace survives a
reload, a second browser and a server restart with nothing to keep in step.
Positions do not move on their own: not when a runtime stops, not on a rename,
not on a click. A page holds at most two rows, and how many projects that is
depends on the width — five, three or one — which is why a phone shows one
project per page and gives the manager a page of its own at the end.

Beyond the viewport a page is not mounted at all: no subscriptions, no xterm
instances. The runtimes carry on, and coming back to a page takes a fresh
screen.

Two defects were found by the browser suites rather than by the unit tests, and
both are fixed: the pager counted registered projects instead of workspace
members, and the terminal client closed its socket during a page change — which
React makes look like "nothing is subscribed" for the instant between unmounting
one page and mounting the next.

**Verification.** Five browser suites, 113 checks, against real Chrome, a real
server and seven real tmux sessions of which two host the real Claude Code CLI:
`npm run test:e2e`. Four viewports are driven — 1440×900, 1180×820, 820×1180
and 390×844 — and the measured grid geometry at each is in
`docs/WORKSPACE.md` §6.

**Outstanding.** Nothing in the phase. The limits worth repeating: a physical
iPad was not used (the tablet suites are emulation, and say so), Claude Code on
this host is installed but not signed in, and concurrent input authority is not
solved — every client that can reach the server can type. That last one is
Phase 6, and `docs/WORKSPACE.md` §12 states it rather than implying otherwise.

## Phase 6 — Multi-device control

**Status: Done.** One project's terminal open on a desktop and a tablet at the same time, with exactly
one of them able to type into it, and a way to hand that over.

**Goal.** Stop one browser from being the only place a terminal lives. A project has one runtime and
one pty; several people looking at it is the point of the product, and several people typing into it
is not.

**Deliver.**

- `internal/terminal/authority.go` — the lease, one per project, and the two questions it answers:
  may this client type, may this client set the size;
- `internal/terminal/control.go` — request, grant, queue, hand over, decline, release;
- `internal/terminal/device.go` — a device label derived from the User-Agent, from a closed vocabulary;
- the roster (`control.changed`) carried by every message in the control family;
- protocol version 2, because `input` and `resize` stopped being things any subscriber may send;
- the client identifier: a browser session, kept in `sessionStorage`, stated in the handshake;
- `web/src/terminal/` — the control state, the badge, the request list, and the four places authority
  is enforced: the keyboard, the touch keys, the resize, and the fit that a viewer must not make;
- `web/e2e/suites/controller.mjs` — two browser contexts, one desktop and one emulated tablet;
- `docs/MULTI_DEVICE.md`.

**What was built, and the four decisions in it.**

*A lease, not a flag.* Control is held per project by a client. A client that is connected and holding
it keeps it indefinitely — there is no idle timeout, because taking a terminal away from somebody who
is reading it would be a product decision nobody asked for. `expiresAt` is set only while the holder
is *away*, which is what makes the grace period a rule about disconnection rather than about silence.

*Viewer by default.* A client that opens a project is a viewer, and stays one until it asks. Nothing is
inherited, nothing is granted on arrival, and a restarted server gives the terminal to nobody — not to
the device that held it before, and not to whoever happened to be watching. That last one is the rule
that makes a restart safe: an owner invented by the server is worse than no owner.

*Transfer is a handshake.* A device that wants a terminal somebody else is typing into asks for it. The
request is put in front of the holder, who hands over or declines. Control is never taken and never
pushed. The button is *Request control* everywhere in the interface, and the directive's §十一 is
explicit that *Take Control* must not appear, because on a project somebody else is using it is not
what happens.

*Two authorities, one lease.* `MayInput` and `MayResize` are separate questions with separate answers,
and a refusal names which one it was. The geometry belongs to the terminal rather than to the browsers:
the controller's resize moves the pty and everybody is told, and a viewer's box is a viewport onto the
terminal rather than a measurement of it — resizing that box changes how much of the screen the viewer
can see and nothing about the pty. Where in that screen a browser is looking stays its own business and
never crosses the wire; the one thing that moves it is a snapshot, because a snapshot is the screen
drawn again rather than a line added to it, and a client that has just been handed the present belongs
at the end of it.

**Verification.** A whole run of the browser suites — `npm run test:e2e`, six suites, one fixture each,
182 checks passed and two skipped — over `web/e2e/suites/controller.mjs`'s 41, against a real server,
real tmux and two real browser contexts: one stating a Windows Chrome User-Agent, one an iPad Safari
User-Agent with touch and mobile emulation. The controller suite drives the ordinary cases, the four
transfer cases, project isolation, a controller that disappears, a controller whose connection is
severed, and a server that is stopped and started underneath a live browser. Beside it: transport 28,
terminal 24, recovery 26, tablet 18, workspace 45. Both skips are reported where they happen rather
than passed over, and neither is a product failure dressed as one — the terminal suite's line-for-line
comparison, which needs a pane whose size is not being changed under it, and the workspace suite's two
live Claudes, which this host keeps one of; the same line-for-line comparison is made in the recovery
suite, where nothing resizes the terminal underneath it, and there it passes. Behind them: `internal/terminal/control_test.go` — the
identifier, the device label, the queue, both authorities, transfer and release; and
`internal/terminal/authority_test.go` — the rules of the lease, including
`TestSimultaneousRequestsProduceExactlyOneController`, which is the race §十八 asks for; and
`internal/terminal/multi_device_test.go` — one controller and ten viewers on one terminal, the
thirty-second default, and a lease that does not outlive the server that granted it. The Go packages
are green uncached (`go test -count=1 ./...`, ten packages) and so is the frontend (`npm test`, 448
tests in 21 files, with `npm run typecheck` clean).

**Outstanding, and the limits worth repeating.** A physical iPad was not used: the tablet half is
Playwright's touch and mobile emulation, and the suite says so in its own header rather than implying
otherwise. `go test -race` could not be run on the development host — cgo and gcc are not available
there — so the concurrency tests run without the race detector; the race itself is exercised by
racing two askers deterministically, which is a weaker instrument than `-race` and is stated as such.
And one protocol break is deliberate: version 2 refuses a `?v=1` client rather than serving it, so a
cached page from Phase 5 must reload. That is §二十二's compatibility requirement not being met in one
place, by choice, and the alternative was a version 1 client drawing a Prompt Bar that silently does
not work.

**One gap in the interface.** A controller who closes a project's panel keeps the lease until the grace
lapses, or until they reopen the panel and release it. There is no control anywhere in the interface
that gives up a lease for a project that is not on screen. It is a gap in the UI rather than in the
server — the message exists and is handled — and it is recorded here rather than left to be discovered.

## Phase 6.5 — Stabilization: deployable, upgradeable, recoverable

**Done.** No new product capability, deliberately. The goal was that what already existed could be put
on a server and left there. Concretely:

| Deliverable | What it is |
| --- | --- |
| `deploy/linux/install.sh` | Installs, and re-run, upgrades. Prints every change before making it; `--uninstall` keeps your data. |
| `deploy/linux/agentmux.service` | The systemd unit, annotated in place. `Restart=always` and **`KillMode=process`** — the second is what makes a restart survive. |
| `deploy/linux/README.md` | The operator's guide. |
| `deploy/linux/recovery-test.sh` | The three recovery cases, run against a real installation. |
| `deploy/linux/loadtest.py` | The performance baseline, with its own dependency-free WebSocket client. |
| `config/agentmux.example.yaml` | Every setting and its default. |
| YAML configuration | YAML or JSON, behind exactly one decoder, so the two cannot drift. |
| `GET /health` | Liveness and readiness for a supervisor. Always 200; carries no credential, filesystem detail or secret. |
| `-debug` / `AGENTMUX_DEBUG` / `server.debug` | Gates the four private paths in `GET /api/server`. Separate from `logging.level` on purpose. |
| Log components | A `component` on every record, and the four forbidden things stated in the package doc. |
| `internal/version` | `0.6.5`, plus a git commit and build date injected by the linker. |

**The one setting that changes what the product does is `KillMode=process`.** The default,
`control-group`, signals every process in the unit's cgroup on stop, and a project's tmux server is in
that cgroup — AgentMux starts it as a child and tmux detaches by forking, which changes the terminal
but not the cgroup. Under the default, `systemctl restart agentmux` would kill every session and the
server would then report every runtime stopped, having destroyed them itself.

**Validated on a real Linux server**, not argued: systemd 255, tmux 3.4, Ubuntu 24.04 under WSL2.
Install, start, stop, restart, all three recovery cases, the upgrade path, the health endpoint, the
WebSocket terminal round trip, and the frontend served by the server. The measured baseline is in
`docs/DEPLOYMENT.md` §14, Performance baseline: at 5 projects and 10 viewers the server used 1.0 % of
one core and 23 MB at peak.

**Two limits, stated rather than glossed.** The installer's in-script build path (`go build`,
`npm run build`) could not be exercised inside the test distribution, which has no Go or Node
toolchain; both builds run on the Windows side and the installer's `--binary` and `--web` paths were
used instead. And the final `claude` link in `AgentMux Server → WSL → tmux → Claude` was not exercised
end to end on that host: the CLI is a per-user install under a human's home directory, which the
unprivileged service account cannot traverse, and that is the correct arrangement rather than a
defect. Everything up to it — the runtime, the pty, the terminal, the WebSocket — was verified end to
end.

## Phase 7 — Claude state awareness

Through Claude Hooks:

- READY;
- RUNNING;
- WAITING;
- COMPLETED;
- ERROR.

Terminal remains the real displayed interface.

### Phase 7.1 — Agent event foundation

Status: **done**. It is the layer the rest of Phase 7 stands on, and it is
deliberately invisible: nothing a user does looks different after it, and that is
what a foundation is for.

Deliver:

- an event model — an event records that something happened, and is not a status
  (`internal/event`);
- `agent_events`, with the indexes the timeline queries read
  (`migrations/0003_agent_events.sql`);
- one write path, `Service.CreateEvent`, so a later hook, a later user action and
  the runtime today all pass the same validation;
- the runtime bridge: `runtime.started`, `runtime.stopped`, `runtime.error`,
  `runtime.destroyed`, emitted after the state has already changed and never
  involved in it;
- `GET /api/projects/{id}/events` and `GET /api/runtime/{id}/events`, read-only,
  newest first, keyset-paginated;
- `docs/AGENT_EVENTS.md`.

**Not in this sub-phase, and not stubbed:** Claude Hooks, terminal output parsing,
pattern matching against a pane, state inference, a task model, notifications,
token or model accounting, automatic decisions, and any user interface. The only
consumer is the API, which exists so the data model can be exercised before
anything is built on it.

The boundary this sub-phase exists to establish is the one between what is true
now and what happened:

```text
RuntimeManager   owns  what is true now      Runtime.State, project_runtime
Event Service    owns  what happened         agent_events, append-only
```

A later phase that reads events to decide whether a runtime is running has made
the mistake the boundary prevents. `docs/AGENT_EVENTS.md` §1 and §5 are the long
form, and §8 lists what is deliberately absent.

### Phase 7.2 — Task and agent session model

Status: **done**. It is the second half of the foundation Phase 7 stands on, and it
is as invisible as the first: no screen in the product changed, and nothing a
person can do in the interface is different.

Deliver:

- the task model — what somebody wants an agent to do, and its status
  (`internal/task`);
- the agent session model — one attempt at a task, and its status
  (`internal/task/session.go`);
- `tasks` and `agent_sessions`, with their indexes
  (`migrations/0004_tasks.sql`, `migrations/0005_agent_sessions.sql`);
- one service that owns both lifecycles, so a status changes in one place and
  cannot be moved anywhere the graph does not allow;
- eight endpoints under `/api` for reading and moving them;
- `docs/TASK_MODEL.md`.

The relationship, which is the whole of the phase:

```text
Project → Task → AgentSession → Runtime → AgentEvent
```

**A task is not a runtime.** "Is the terminal up?" is a runtime question with a
present-tense answer that changes while you look at it; "was this work finished?"
is a task question whose answer stays true after every process involved has
exited. The session is the level that makes the two compatible rather than merely
different, and it is what allows a task to have been attempted twice, in two
different runtimes, one of which no longer exists. That is why
`agent_sessions.runtime_id` is a column and **not** a foreign key.

**Nothing in this sub-phase starts a process.** Creating a task starts no runtime,
launches no agent and writes no prompt; a session is created with no runtime and
one is attached afterwards by a request. The two halves of AgentMux meet at the
runtime API, which already existed, and joining them is 7.3 rather than a missing
line of this one.

**A status changes because a request changed it, and for no other reason.** The
service never reads the event log to work out where a task stands. Deriving a
status from events would make the log authoritative and the task a cache of it,
and the first time the two disagreed there would be no way to say which was right.

What was built: four packages' worth of model and none of it a UI. The task and
session lifecycles as a table of allowed edges rather than forbidden ones, so a
transition nobody thought about is refused; `COMPLETED` final, so a task reported
as done and then reported as running again cannot make the first report false;
`WAITING` on the task and deliberately **not** on the session; a conditional
`UPDATE … WHERE id = ? AND status = ?` instead of a lock, so two racing requests
apply exactly once and the loser is told it lost; identifiers from
`internal/idgen`, which this phase factored out of the two packages that had
already written it twice; and four event types rather than one per status,
written through Phase 7.1's existing `event.Service`.

Verified by `go test ./...` across the model, the service, the storage layer and
the HTTP surface; a new browser suite, `web/e2e/suites/tasks.mjs`, driving the API
from a real page against a real server; and the migration path from a Phase 7.1
database asserted to upgrade without disturbing `projects`, `project_runtime` or
`agent_events`.

**Not in this sub-phase, and not stubbed:** Claude Hooks, the Claude Agent SDK,
terminal output parsing, prompt parsing, AI state inference, notifications,
token or model statistics, automatic retry, AI summarization, multi-agent
orchestration, user accounts, billing, and any Task UI. The frontend gained types
and API functions and no screens. `docs/TASK_MODEL.md` §8 is the long form.

The two things this phase is **not** yet able to do, stated rather than implied:
a task list is capped and not paginated (§12 of the same document), and nothing
joins a task to a running Claude — a task exists, and a session names the runtime
it ran in, and no code yet connects the two.

### Phase 7.3A — Claude integration discovery

Status: **done**. Not an implementation phase, and the last phase of Phase 7
that produces no running code. Its question was whether AgentMux can learn what
Claude is doing from Claude rather than from what Claude's terminal looks like,
and its answer is yes — through Claude Code hooks, which turn out to be a control
mechanism as well as a notification one, and through the machine-readable JSON
stream the CLI already writes to the stdout the Runtime Layer already owns.

The findings are long enough to have their own document rather than a section
here: `docs/CLAUDE_INTEGRATION.md`. What belongs in the roadmap is what the
discovery changed. It ruled out the Agent SDK, not on effort but on shape — the
SDK spawns and supervises its own subprocess over stdio and cannot attach to the
tmux pane AgentMux owns, so adopting it would mean replacing the Runtime Layer
rather than integrating with it, and it would require AgentMux to hold its own
API credentials. It established that terminal parsing is not a fallback for any
of the five states but a different and worse instrument. It found that hook
configuration is a trust boundary, since a hook can grant a permission that would
otherwise be refused. And it found that "waiting for user input" is observable,
but only in interactive sessions — which is the case AgentMux runs and would
not be the case for a `-p`-driven design.

Nothing in this phase is implemented, and nothing in the build anticipates it.

### Phase 7.3B — Claude event adapter

**Partly built.** Phase 7.1 built the record of what happened and 7.2 built the
record of what is wanted. What remains is the part Phase 7 was named for: the
agent reporting its own state, so that a session's status stops being something a
caller sets by hand and becomes something Claude says. That is where the five
statuses at the top of this section — READY, RUNNING, WAITING, COMPLETED, ERROR —
are meant to come from, and it is where a task and a runtime are finally joined.

The shape is decided and recorded in `docs/CLAUDE_INTEGRATION.md` §10: a
`ClaudeAdapter` that receives Claude's hook events and the CLI's machine-readable
stream, writes AgentMux events through the existing `event.Service`, and leaves
the Runtime Layer's process, pane, input, and output handling exactly as it is.

Sub-phase **7.3B-0, runtime environment validation, is done** and changed one
thing the plan assumed. Claude's hooks, its settings injection, its session-id
dictation and its JSON stream all work *without authentication* — verified on
WSL with no credentials at all — so an adapter observes a session starting and
ending even when the account behind it is broken. The same work corrected a
phase-7.3A claim about `PATH` and found that `result.subtype` can read
`success` on a failed turn. `docs/CLAUDE_RUNTIME_VALIDATION.md` is the record;
Windows is verified end to end, WSL for everything but the model turn, and pure
Linux not at all, because that host needs interactive authentication.

Sub-phase **7.3B-1, the adapter foundation, is done**. `internal/claude` now
receives Claude's hooks over an HTTP endpoint, reads the CLI's stream-json for
the turn's outcome, translates both into seven `agent.*` event types, and writes
them through the event service into `agent_events`. It adds no migrations, no
endpoints, and no interface; it starts nothing and decides nothing. It was
validated end to end against Windows Claude Code 2.1.278 with a real database,
and `docs/CLAUDE_ADAPTER.md` is the long form, including §8's account of what the
validation did not cover.

Sub-phase **7.3B-2, the runtime binding, is done**. `internal/agent` is the
coordinator that connects the adapter to the product, and it is the only place in
the build that decides which Claude session is which attempt. One call —
`POST /api/projects/{id}/runtime/agent/start`, optionally naming a task — starts
the runtime if it is not running, mints the session id AgentMux dictates on the
command line, attaches a hook receiver, writes the settings document naming it,
launches Claude, and binds the attempt. On the way it fixed a defect it was
looking straight at: `session.created` and `session.status_changed` had been
silently refused by the event service since Phase 7.2, because their payloads
named a field `sessionId` and any field ending in `sessionid` is refused as a
session credential. **Those two events had never been written.** The field is now
`agentSession` and a test runs the real checker over the real builders.
`docs/AGENT_RUNTIME_BINDING.md` is the long form.

**What remains in 7.3B is one decision and one gap.** The gap: the binding is in
memory, so a server restart loses it — Claude outlives the server, so the attempt
it was running is not finished, and nothing reconciles it. Closing that means a
migration for a `claude_session_id` column and a reconciliation step at boot.
The decision: whether a task's status can be derived from Claude's events at all,
which is now closer than it was — the attempt is bound and the events are routed
— but still blocked on `PermissionRequest` being record-only, since making it
actionable turns AgentMux from an observer of a Claude session into a
participant in one. `docs/AGENT_RUNTIME_BINDING.md` §6 and
`docs/CLAUDE_ADAPTER.md` §10 are the lists.

## Phase 7.3C — Agent state

**Partly built.** 7.3B made the agent report what it is doing; 7.3C makes that
report answerable as a question. Sub-phase **7.3C-1, the projection, is done**:
`internal/agentstate` folds the event log into an `agent_states` row per attempt,
driven from inside the event service so there is no path to a state that skips
the log. Two endpoints read it and nothing writes it —
`GET /api/sessions/{id}/state` and `GET /api/projects/{id}/agent-states`.
`docs/AGENT_STATE.md` is the long form.

Sub-phase **7.3C-2, attention and actions, is done**. The same log now answers
"does anybody need to care", as a level per attempt and a queue of pending
actions, with three read endpoints and deliberately no way to answer one — an
action records that Claude asked for something and decides nothing.
`docs/AGENT_ATTENTION.md` is the long form.

**What remains in 7.3C is reach, not mechanism.** Three of the seven statuses
cannot occur as things stand: `WAITING_INPUT` because no event produces it, and
`COMPLETED` / `FAILED` because the events that would come from the CLI's `result`
envelope are on a stream the runtime does not feed (7.3B-2 finding 3). An agent
started without a task is not projected at all, because the adapter knows the
attempt and does not put it in the payload. All three close the same way — more
of what Claude says reaching the log — and none of them is a change to the
projection, which already maps whatever arrives. §6 of `docs/AGENT_STATE.md` is
the list.

## Phase 7.4 — Client surfaces

**Partly built.** 7.3C made the system know what needs a person; 7.4 is how a
person is told. Sub-phase **7.4A, the backend aggregation, is done**:
`internal/controller` joins the project, state, attention and action services
into one response so that a console reads one endpoint rather than seven and
joins nothing itself. Four queries for any number of projects, no cache, no
table, two GET routes. `docs/CONTROLLER_API.md` is the long form, and the
TypeScript types for the response are in `web/src/api/types.ts` with nothing
reading them yet — the same position the task endpoints were in after Phase 7.2.

Sub-phase **7.4B-1, the console foundation, is done**: `/dashboard` renders the
aggregation as a grid of project cards, sorted the way the server sorted them,
with a server bar above and three columns down to one. It is read-only — no
input, no Claude control, no permission action, and a provider switch that is
present and inert. `docs/CONTROLLER_UI.md` is the long form.

Sub-phase **7.4B-2A, the terminal viewer, is done**. A card whose runtime is up
now carries the project's actual terminal, live: the same `TerminalView`, the
same xterm.js, the same `agentmux.terminal.v2` subscription over the page's one
existing WebSocket. Nothing was added to the backend, because nothing needed to
be — a console is a **viewer** in the protocol's existing sense, which is what
makes it read-only by construction rather than by a flag: no input handler is
bound to its terminal, no touch keys are drawn, and the server refuses a viewer's
input and resize on the paths it already refused them.
`docs/TERMINAL_VIEWER.md` is the long form.

Sub-phase **7.4B-2B, the controller lease, is done**, and it is the piece that
makes "viewer" a state a client can leave. A **lease** records which client
currently owns a project's terminal input: it is asked for with
`control.request` on the socket the client already holds, granted to whoever asks
first when the terminal is free, and never taken from a client that has it. A
request that loses is refused with the holder's device name, not queued behind a
takeover. The lease lives in memory — no table, no row, and no keystroke ever
written anywhere — is keyed on the `sessionStorage` identity that survives a
reload, and is released when the holder says so or expires when the holder's
connection goes away. Input and resize are separate authorities and the console
keeps only the first: **a console types and never reshapes the shared pty**.
`docs/TERMINAL_CONTROLLER.md` is the long form, and `docs/TERMINAL_VIEWER.md` §3
is what a client without the lease still cannot do.

**What remains in 7.4 is depth, not surface.** The queue is a count rather than
a list, so nothing shows *which* actions are waiting; the console can take a
terminal's keyboard but still cannot answer a permission prompt from it, nor
start or stop a runtime, nor be told anything it is not currently looking at —
those are later phases' subjects; and the attention projection's known limits — a
failed project staying at `WARNING` until something runs in it again, and an
agent started without a task being invisible — are inherited here unchanged.

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
