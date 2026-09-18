# AgentMux Roadmap

## Implementation status

As of version 0.1.0:

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
