# AgentMux Claude Runtime

This document describes how AgentMux runs the **real Claude Code CLI** inside a project's persistent
runtime: which binary is found, what command line is typed, where the process ends up, how its state
is observed, and what it deliberately does not do.

It is written for someone who has to change this code, or to explain to a user why their Claude
session behaved the way it did. Everything in it was measured against a real installation — the
versions, PIDs and messages quoted below are from actual runs, not from the design.

Phase 3 built this. `docs/RUNTIME.md` describes the runtime it sits inside.

## 1. Where the Claude process runs

**Claude Code is a Linux process, and it runs in the same Linux runtime as the AgentMux server and
the tmux server that hosts it.**

```text
Windows host                       WSL distribution (Ubuntu-24.04)
────────────                       ──────────────────────────────
                                   AgentMux Server (Go, linux/amd64)
                                   ├─ tmux server  <dataDir>/tmux/p_a1b2….sock
                                   │    └─ pane leader: the shell (cwd = project runtime path)
                                   │         └─ claude                     ← the agent
                                   │              └─ helpers from its own binary
                                   └─ one Control Monitor per running runtime
```

The architecture is **not** `Windows server → wsl.exe → claude`. Nothing crosses the WSL boundary to
reach the agent: the server, the tmux server, the pane, the shell and Claude are all in one Linux
process tree, which is what makes the process-table observation in §6 a local read of `/proc` rather
than a remote query. On a Windows-native server the runtime is unavailable before any of this is
reached, and `/api/server` says so.

## 2. Binary discovery

`internal/claude` resolves the CLI. It is the only package that knows which Claude Code is installed.

1. **`terminal.claudeBinary`** from the configuration, defaulting to the bare name `claude`.
   A bare name, not an absolute path: a native install, a package manager install and a Node install
   put it in three different places, and a default that guessed one would be wrong on the other two.
2. **`exec.LookPath`** on `PATH`, inside the same environment as the server.
3. **Symlinks resolved** (`filepath.EvalSymlinks`). This step is not cosmetic. Measured on this
   machine:

   ```text
   /home/sunboy/.local/bin/claude  →  /home/sunboy/.local/share/claude/versions/2.1.274
   ```

   The configured name and the running process are the same program under two different strings, and
   §6 compares executable paths for equality.
4. **`<path> --version`**, with a 10-second timeout, parsed for the leading version token. Measured:
   `2.1.274 (Claude Code)`.

Anything but a fully successful chain means **not available**, and `Resolve` reports that as an
answer rather than an error — a server has to be able to start and say it cannot host an agent.

The result is cached (`versionCacheTTL`) so that a request for server status does not run a
subprocess every time.

### What discovery never does

- It does not read, print, or return a credential.
- It does not dump the environment, read `ANTHROPIC_API_KEY`, touch the credential store, or inspect
  CC Switch's database, providers, models or secrets.
- It does not report **whether the installation is signed in**. Authentication is Claude's own
  business: it is decided when the program starts, and the program says so on its own terminal.
  There is no `/api` field that answers "is this authenticated", because computing one would mean
  storing, caching or inferring a secret somewhere it could leak.

A launcher that probed for a token would be a second place for a secret to exist and the first place
for one to reach a log line or an API response.

## 3. Launch model

`StartAgent` resolves the spec, then:

1. **Requires the runtime to be running.** An agent with no terminal is a process nobody can see,
   interrupt or read.
2. **Adopts an existing agent if one is already in the pane.** A descendant of the pane leader whose
   `/proc/<pid>/exe` equals the resolved binary is the agent. Finding one makes the call idempotent,
   and it is also how a runtime inherited from a previous server process is picked up — the agent was
   never this process's child, so nothing has to be restored, only recognised.
3. **Requires the pane's cwd to be the project's runtime path.** A terminal attached to the wrong
   directory is worse than no terminal. See §4.
4. **Requires the pane to be idle** — the foreground process must be the shell. A command typed into
   a pane whose foreground is another program would be delivered into that program, so the launch is
   refused rather than made and hoped for (`agent_terminal_busy`).
5. **Types the command**: `backend.Launch` → tmux `send-keys` of `spec.Command`.
6. **Waits for the process to appear** (`AgentStartTimeout`, default 30 s), by reading the process
   table, not the screen.
7. **Verifies the observed cwd** and, if it disagrees, interrupts the agent and fails
   (`agent_wrong_directory`). Leaving a coding agent in a directory nobody chose is the failure this
   design exists to prevent.

The command line is exactly one shell word: `claude.Quote(path)` wraps the resolved path in single
quotes with the POSIX `'\''` escape. That matters because the command is *typed into a shell*, and
every path under `/mnt/c` is one directory away from containing a space.

**Nothing is added to it.** Measured on this machine, the command typed is:

```text
'/home/sunboy/.local/share/claude/versions/2.1.274'
```

No `--dangerously-skip-permissions`, no `--permission-mode`, no `--allowedTools`, no `--model`, no
`-p`. There is a test that asserts none of those appear in the rendered command
(`TestRealClaudeIsResolvedByTheProductionAdapter`). See §8.

### Why tmux `send-keys` rather than running the CLI as a child

Because the terminal has to outlive the agent, and it has to outlive the *server*. A pane's shell is
the session's owner; the agent is a child of it. That is what lets a Claude session be interrupted
without destroying its scrollback, and what lets a restarted server find a runtime — and an agent —
it never started.

## 4. Project working directory

The agent starts in `project.runtimePath`, and this is **measured, not asserted**:

- the pane's cwd is checked before the launch;
- after the launch, `/proc/<pid>/cwd` of the *observed agent process* is compared against the
  canonicalised runtime path;
- the same value is what `AgentStatus.Dir` reports, so a client can see the evidence rather than the
  claim.

`AgentStatus.Dir` is read from the process, never from the request. The real-integration test
`TestRealClaudeRunsInTheProjectDirectory` checks the kernel's answer against the runtime's answer, and
asserts the process is a descendant of the pane leader.

## 5. Environment

The agent inherits the environment of the shell in the pane, which is the environment of the tmux
server, which is the environment the AgentMux server was started with — inside WSL, under the user
who started it.

AgentMux adds nothing to it: no injected API key, no provider variable, no model, no config path.
Claude Code uses its own user-level configuration exactly as it would in the user's own terminal,
which is the point — the agent here is the same program with the same settings as the one the user
runs by hand.

Consequently, if the WSL Claude CLI is not signed in, the agent starts, prints its own
authentication or onboarding screen, and behaves exactly as it would in a terminal. AgentMux's job is
to host that faithfully; it is not to make it differently authenticated.

## 6. Process lifecycle and observation

### The two state machines

**Runtime**: `STOPPED`, `STARTING`, `RUNNING`, `STOPPING`, `ERROR`, `ORPHAN`.
**Agent**: `STOPPED`, `STARTING`, `RUNNING`, `STOPPING`, `EXITED`, `FAILED`.

The agent exiting does **not** stop the runtime. The runtime stays `RUNNING` with its terminal and
scrollback intact; only the agent changed state. Stopping the runtime is a separate call, and
destroying the terminal is `DELETE` on the runtime.

### How "is it still running" is answered

By reading `/proc`. `internal/session/agentproc.go` scans the process table once, builds a `ppid`
map, walks the pane leader's descendants breadth-first, and compares each one's
`/proc/<pid>/exe` against the resolved binary.

**The terminal's text is never parsed.** There is no `if output contains "…" → READY` anywhere in
this codebase, and there will not be: that is a guess that breaks on every Claude Code release.

`pane_current_command` is also not used as identity. Measured on this machine:

| typed into the pane | `pane_current_command` |
| --- | --- |
| `claude` | `claude` |
| `'/home/sunboy/.local/share/claude/versions/2.1.274'` | `2.1.274` |

Both values are the kernel's `comm`, which is the basename of the string passed to `execve` — a
property of how the command was typed, not of what is running. Process identity is the honest
question, so process identity is what is asked.

### Helpers from the agent's own binary

Claude Code starts helper processes from **its own executable**. Measured in the isolation test: a
terminal held pid 843814 (parent = pane leader 843767) *and* pid 843827 (parent = 843814).

So "how many processes here are running Claude" has an answer greater than one, and the agent is the
**outermost** of them. The breadth-first walk returns the shallowest match for exactly that reason,
and the isolation test asserts that the runtime picked the outermost one rather than a helper.

### Exit codes

**There is no exit code, and there cannot be one.** The pane's shell forks and reaps the agent, so the
exit status is known to the shell and to nothing else. Reading it would mean reading the terminal —
which §23 of the Phase 3 directive forbids and this design refuses.

What is reported instead is the distinction that is actually knowable:

- **`Requested`** — AgentMux asked the agent to stop.
- **`ExitedAt`** — when the agent was last observed to be gone.

`Requested: true` with a non-running state is a stop that was asked for. `Requested: false` is an
agent that went away on its own — which is a real, observed case: Claude Code intermittently prints
`Unable to connect to Anthropic services` and exits within seconds.

### Start time

`StartedAt` is derived from the kernel's own uptime plus the process's start ticks in
`/proc/<pid>/stat` (field 22, at `USER_HZ` = 100). It is **not** when AgentMux noticed the process,
which is what makes it survive a server restart: an adopted runtime reports the agent's real start
time.

`/proc/uptime` has two decimal places, so boot time derived from it is quantised to 10 ms. Boot time
is therefore computed **once per process** and cached; two readings a millisecond apart would
otherwise disagree by up to 10 ms, and a client that read a start time twice and got two answers
would be right to distrust both. Cross-process comparisons (a test reading `/proc` separately from
the runtime) need a ±20 ms tolerance for the same reason.

### Stopping

`StopAgent` is `tmux send-keys C-c` — the same keystroke a user would send.

**Ctrl-C does not necessarily end Claude.** Claude Code holds the pty in raw mode, so `0x03` arrives
as a *keystroke* rather than as `SIGINT`, and the CLI decides what it means. Measured: after a 4-second
settle on the first-run screen, `C-c` left the process alive past 10 seconds. When the interrupt is
sent ~23 ms after launch — before terminal handling is installed — it does take.

AgentMux does not escalate to `SIGTERM`/`SIGKILL`, deliberately: that would destroy the terminal and
the scrollback that the whole runtime model exists to preserve. Instead it says so, and the status
answer is honest about it:

> the interrupt was delivered and the agent is still running; it may need a second one, or it may be
> waiting for a decision of its own

So `agentStatus` reports `Running: true`, `State: AgentRunning`, `Requested: true` and a non-empty
`Message`. A second `StopAgent` is allowed and behaves the same way. The deterministic unit test
`TestAgentThatDeclinesTheInterruptIsReportedAsStillRunning` covers this without needing a real CLI.

An agent that keeps running after a declared stop is reported as exactly that — not as a stop nobody
asked for, and not as an error.

### What is written to the log

Only metadata: `projectId`, `session`, `runtimeId`, `agent`, `event`, `pid`, `bytes`, `duration`,
`exitCode`. Never a prompt, never a Claude answer, never raw terminal content. A Claude session is
private; the log is not the place it becomes public.

## 7. Persistence across a server restart

Claude is not a child of the AgentMux server, so a restart does not touch it.

On startup the runtime reconciles: for each registered project it finds the tmux socket, asks whether
the session is alive, and re-reads the process table for the agent. A runtime that was `RUNNING`
comes back `RUNNING`, and so does its agent — with the **same PID** and the **same `StartedAt`**,
because both are read from the kernel rather than remembered. Measured in
`TestRealClaudeSurvivesAServerRestart`: identical PID, `Dir` unchanged, start times agreeing within
20 ms.

Nothing about the agent is stored to be restored. There is no serialised agent state and no SQLite
schema change: liveness is read live from the process table, because a record of a running process is
a stale answer waiting to happen.

## 8. Permissions

**Claude Code's interactive permission model is preserved exactly as it is.**

AgentMux does not add `--dangerously-skip-permissions`, or `--permission-mode`, or `--allowedTools`,
or any equivalent auto-approval. It does not answer Claude's permission prompts, and it does not
detect them by reading the screen. When Claude asks the user something, it is asking the user, on a
terminal the user owns, and the answer is the user's to give.

This is a product decision, not a limitation of the implementation. A mux that auto-approved its
agents would be a mux that runs arbitrary commands on the user's machine on the user's behalf without
the user. Phase 4's terminal is the interface for answering those prompts.

The real-integration test `TestRealClaudeAnswersAPrompt` therefore asserts only that the agent is
still running and that the terminal produced output; whether a file appeared is **logged**, not
asserted, because whether a tool call is auto-approved is the user's configuration and not AgentMux's
to require.

## 9. Real integration tests

The Claude tests live in `cmd/server/claude_e2e_test.go` — package `main`, so that they exercise the
**production** agent adapter (`agentSpecs`) rather than a test double.

They are gated, because a test that starts a real Claude session costs real API credit and needs a
real installation:

| Gate | Effect |
| --- | --- |
| `AGENTMUX_TEST_REAL_CLAUDE=1` | Gate 1. Resolution, project cwd, Unicode, isolation, server restart, reported state, stop honesty. |
| `AGENTMUX_TEST_REAL_CLAUDE_PROMPT=1` | Gate 2. Also sends a prompt (`TestRealClaudeAnswersAPrompt`). Implies nothing — it must be set explicitly. |

Both are also skipped unless `GOOS == "linux"` and tmux is on `PATH`. So a normal `go test ./...` —
on any machine, including a developer's Windows box — **starts no Claude and costs nothing**.

Also configurable: `AGENTMUX_TEST_TMUX_BINARY`, `AGENTMUX_TEST_SHELL`.

```bash
# inside WSL, from the repository root
AGENTMUX_TEST_REAL_CLAUDE=1 go test -v -run TestRealClaude ./cmd/server/
```

### What they cover

| Test | Gate | Asserts |
| --- | --- | --- |
| `TestRealClaudeIsResolvedByTheProductionAdapter` | 1 | Version/executable shapes; the command is the quoted path and contains none of the forbidden flags; the rendered spec carries no credential-looking string |
| `TestRealClaudeRunsInTheProjectDirectory` | 1 | The runtime's answer agrees with `/proc/<pid>/exe` and `/proc/<pid>/cwd`; it is a descendant of the pane leader; it is the outermost process running that binary; a snapshot is non-empty and valid UTF-8 |
| `TestRealClaudeTerminalCarriesUnicode` | 1 | A 44-byte UTF-8 line (`你好，世界 — ünïcødé 🚀 ✓ Ω`) crosses the terminal byte-for-byte, then real Claude starts in the same session |
| `TestRealClaudeIsIsolatedBetweenProjects` | 1 | Two projects get distinct sockets, PIDs and directories; neither agent is in the other's tree; destroying one leaves the other running |
| `TestRealClaudeSurvivesAServerRestart` | 1 | After a restart the agent is the same process — same PID, same directory, `StartedAt` within ±20 ms |
| `TestRealClaudeReportedStateMatchesTheProcessTable` | 1 | Over 12 s at 250 ms, the reported liveness never disagrees with `/proc`; if it ended, it is `EXITED` and `Requested == false` |
| `TestRealClaudeStopIsReportedHonestly` | 1 | A declared stop is reflected in the status; a declined interrupt is reported as running-and-asked |
| `TestRealClaudeAnswersAPrompt` | 2 | The terminal grows after a prompt; the agent is still running |

**They do not assert that the agent stays up.** Claude Code intermittently aborts with
`UNKNOWN_CERTIFICATE_VERIFICATION_ERROR` within seconds on this network, so "it stayed up" is not a
property of the code under test. `startRealClaude` retries up to three times, and the state test
asserts only that the report matches the kernel at every step.

### Fixtures, and the rule that matters

Each test creates a throwaway project under `os.TempDir()/agentmux-claude-test/<label>/project`, with
`README.md`, `CLAUDE.md` and `sample.txt`, registers its cleanup, and calls
`assertOutsideSourceTree` — **the agent must never be able to modify the AgentMux repository
itself**, and the test refuses to start one that could.

## 10. HTTP surface

| Method | Path | Effect |
| --- | --- | --- |
| `GET` | `/api/projects/{id}/runtime/agent` | The agent's status. A small thing for a panel to poll without re-reading the whole runtime. |
| `POST` | `/api/projects/{id}/runtime/agent/start` | Start it, or adopt the one already there. |
| `POST` | `/api/projects/{id}/runtime/agent/stop` | Interrupt it, and leave the runtime alone. |

The agent is also reported inside the runtime, under `runtime.agent`.

Error codes: `agent_unavailable`, `agent_launch_failed`, `agent_stop_failed`,
`agent_wrong_directory`, `agent_terminal_busy`. See `docs/API.md`.

No response body contains a credential, a token, an environment dump, or any part of Claude's
configuration.

## 11. Known limitations

1. **No exit code.** The pane's shell reaps the agent, so its status is unobtainable without reading
   the terminal. AgentMux reports *requested stop* vs *unobserved exit* instead. See §6.
2. **Ctrl-C can be declined.** It is a keystroke into a raw-mode TUI, not a signal. AgentMux reports
   the refusal honestly and does not escalate, because escalation would destroy the terminal. See §6.
3. **Start times agree to ~20 ms across processes** because of `/proc/uptime`'s quantisation, not
   because anything is imprecise about the agent.
4. **The agent is identified as the outermost process running the resolved binary.** Helpers spawned
   from the same binary are correctly not mistaken for it, but an agent that re-execs itself into a
   *differently pathed* copy would not be recognised.
5. **One agent per runtime.** One project → one runtime → one interactive Claude. Running Claude A
   and Claude B in the same project is not supported and is refused by the idle-pane and
   already-adopted checks.
6. **Claude Code must be installed and signed in inside WSL.** AgentMux does not install it, does not
   configure a provider for it, and does not copy credentials into it. On the machine this was
   developed on, the WSL CLI resolves and launches but is **not signed in**, so an agent reaches
   Claude's authentication screen rather than a conversation. That is the one thing standing between
   this integration and an end-to-end conversation, and it is a Claude-side configuration, not an
   AgentMux defect.
7. **Windows-native servers cannot host an agent at all** — the runtime is unavailable there, and the
   error says which side of the boundary the server is on.
