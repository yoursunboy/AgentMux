# Claude Integration Discovery

Phase 7.3A. This document answers one question: **can AgentMux learn what Claude
is doing from Claude, rather than from what Claude's terminal happens to look
like?**

It records what was measured on this host, what is documented but was not
measured, and what does not exist. Nothing here is implemented. No production
code, database, API, or frontend was changed to produce it, and no Claude process
was left running afterwards.

The short answer, stated up front so the rest can be read as support rather than
as a search: **yes, via Claude Code hooks, and hooks turn out to be a control
mechanism as well as a notification one.** That was not assumed going in — the
configuration that made it true was found in the installed settings schema, and
the control half was proved by a controlled experiment, not inferred. The long
answer is below, including the parts that are less comfortable: hook
configuration is a trust boundary, one of the five states AgentMux wants is
observable only in interactive sessions, and phrase-by-phrase reading of the
terminal remains the one thing this document refuses to call an integration.

## 1. Environment

Two Claude Code installations exist on the development host and they are not the
same build.

| | Windows | WSL |
|---|---|---|
| OS | Windows 11 Home China 10.0.26200 | Ubuntu 24.04 (WSL 2) |
| Claude Code | 2.1.278 | 2.1.274 |
| Binary | `anthropic.claude-code-2.1.278-win32-x64/resources/native-binary/claude.exe`, bundled with the VS Code extension | native `linux-x64` at `/home/sunboy/.local/bin/claude` |
| Signed in | yes — a print-mode turn ran to completion | **no** |
| Shell used for measurement | Git Bash (POSIX), PowerShell available | `bash -s` (non-login) |

Three consequences worth carrying forward.

The Windows binary is **not on `PATH`**. `where.exe claude` fails and Git Bash
does not find it either; the only working invocation is the absolute path into
the extension directory. Any launcher that assumes `claude` resolves through
`PATH` will work on a machine where Claude Code was installed by npm and fail on
one where it came with the IDE extension. AgentMux's existing launcher already
resolves the binary itself rather than trusting `PATH`, and this discovery is a
reason to keep doing that.

The WSL installation **is not signed in**, so no WSL turn could be run. Every
cross-platform claim in this document that rests on a live turn is therefore a
Windows claim. WSL is recorded honestly as *version and help text verified, turn
behaviour not verified* rather than as verified by analogy. The practical shape
of the gap is in §7.

The version numbers differ by four patch releases, and the documentation is
saturated with "requires vX.Y.Z or later" gates — including on hook behaviour
itself. Pin a version and test against the pinned one (§11).

**No credential was read, recorded, or written at any point in this phase.** No
token, API key, OAuth material, keychain entry, or configuration file containing
one was opened or printed. Where a command's output could have contained one, it
was not captured. This constraint governed the whole phase and is restated in
§8; it is not incidental.

## 2. Available Integration Mechanisms

Seven mechanisms were considered. They are not seven interchangeable options: two
of them are the same mechanism at different altitudes, one is a research preview,
and one is not an integration at all.

### 2.1 Hooks

Claude Code's hook system runs a configured handler when the agent reaches a
named lifecycle point. The authoritative list of events and handler types is not
the website — it is the **settings JSON schema shipped with the installed
binary**, which is what this discovery read. That schema names **33 events** and
**five handler types**.

The five handler types are `command`, `prompt`, `agent`, `http`, and `mcp_tool`.
This matters more than it sounds: `http` means a hook can POST its payload
directly to an AgentMux endpoint with no shell, no script file, and no
interpreter dependency, and `mcp_tool` means an AgentMux MCP server could receive
events without a listener at all. Both were verified as configurations the CLI
accepts.

Hooks are configured per settings scope, and — verified — a launcher can supply
its own with `--settings <file>` or `--settings <json>` without touching the
user's own settings. That is the property AgentMux needs: hook configuration
belongs to a runtime AgentMux started, not to the machine.

The 33 events are enumerated with their payloads in §3.

### 2.2 CLI machine-readable output

`claude --help` documents `--output-format text|json|stream-json` and
`--input-format text|stream-json`, plus `--include-partial-messages`,
`--include-hook-events`, `--session-id`, and `--resume`. All were exercised.

`stream-json` is newline-delimited JSON on stdout. In a verified run it emitted,
in order: a `system`/`init` message carrying `session_id`, `cwd`, `model`,
`tools`, and `permissionMode`; assistant messages including thinking blocks;
a final `result` message; and, because hook events were included, the hook
lifecycle interleaved.

`--include-hook-events` is the notable one: it surfaces `system/hook_started`
and `system/hook_response` on the same stream as the conversation. An AgentMux
that already owns the process's stdout — which it does; the Runtime Layer exists
precisely to own it — can therefore receive hook lifecycle data **through the
process it launched**, with no listener, no port, and no hook configuration
pointing anywhere. This is the single most important architectural finding of
the phase and §10 builds on it.

`--input-format stream-json` was verified working: a JSON user message written to
stdin produced a normal turn, hooks included, and `--session-id` on the same
command line dictated the session id end to end. So a fully machine-readable
**bidirectional** channel exists. That is a stronger statement than "Claude can
be observed", and it is the basis for the side-channel shape in §10.

### 2.3 Agent SDK

The SDK exists, is current, and ships in lockstep with the CLI — `@anthropic-ai/claude-agent-sdk` 0.3.278 on npm and `claude-agent-sdk` 0.2.157 on PyPI, published the same day as the 2.1.278 CLI it bundles.

It is **not** a second implementation of the agent loop, and it is **not** a way
to attach to a Claude that is already running. What it does is spawn its own
`claude` CLI subprocess and speak the CLI's stdio JSON protocol to it. The
documentation states this plainly: one agent session maps to one subprocess, and
running N concurrent sessions means N subprocesses each with its own process
tree. There is no documented way for the SDK to attach to an existing tmux
session or PTY; the only escape hatch is an internal transport interface the
reference itself flags as unstable.

It is a good client for the protocol described in §2.2, with typed messages,
`canUseTool` as a first-class permission callback, an `interrupt()` with a
receipt, and a `result` message that carries `is_error`, `subtype`, `num_turns`,
`session_id`, and `total_cost_usd`. It is not a Runtime Layer, and it is not
compatible with one that already owns a tmux pane — see §9 and §10.

There is also a licensing constraint that is easy to miss and hard to work
around: the SDK's documented authentication is `ANTHROPIC_API_KEY` from the
environment, and the documentation carries an explicit prohibition on
third-party products offering claude.ai login or subscription rate limits. The
SDK is not a drop-in replacement for "the CLI the user is already signed into."

### 2.4 MCP

MCP is a tool-calling protocol, not a session-lifecycle protocol. A server can
be told the project root (Claude Code sets `CLAUDE_PROJECT_DIR` in the spawned
server's environment) and can answer `roots/list`, but **it is not documented to
receive the session id**, and nothing in the protocol tells a server that the
agent started, is waiting, or finished. A server can infer that its own process
was spawned, which is process lifetime, not session state.

The exception is **Channels**, an MCP server that pushes events *into* a running
session. It is a research preview, requires the `--channels` flag at startup,
requires Anthropic auth, is unavailable on Bedrock/Vertex/Foundry, and must be
enabled by an organization administrator. It also points the wrong way: it
carries events into Claude, not out of it. Noted, not recommended.

Two hook events — `Elicitation` and `ElicitationResult` — do cover MCP's own
request-for-input path, described in §3.

### 2.5 Plugin / extension surface

The VS Code extension bundles the binary; it is a delivery mechanism, not an
integration surface. Plugins package skills, agents, commands, and hooks —
anything a plugin could offer AgentMux, hooks already offer without the packaging.

### 2.6 Environment variables

`CLAUDE_CODE_SESSION_ID` appears in the documentation as an environment variable
available to hook commands. It **could not be read empirically on this host**:
reading it required a shell expansion the tooling declined, and the alternative
required an approval that was not pursued. It is recorded here as
*documented but not verified*. The session id is available by better means
anyway — every hook payload carries `session_id`, and `system/init` carries it,
and `--session-id` lets AgentMux choose it (§4). Nothing in the recommended
design depends on this variable.

### 2.7 Terminal parsing

Not an integration. See §11 for the explicit statement.

## 3. Event Capabilities

The interactive Claude Code session offers 33 hook events. Listing them is less
useful than grouping them by whether they bear on AgentMux's five states —
READY, RUNNING, WAITING, COMPLETED, ERROR — so that is how they are presented.
The names are the schema's names.

**Session and turn boundaries.** `SessionStart`, `SessionEnd`, `UserPromptSubmit`,
`UserPromptExpansion`, `Stop`, `StopFailure`, `MessageDisplay`.

**Tool lifecycle.** `PreToolUse`, `PostToolUse`, `PostToolUseFailure`,
`PostToolBatch`, `PermissionRequest`, `PermissionDenied`, `Elicitation`,
`ElicitationResult`.

**Needs the user.** `Notification` — the one that matters most, see §4.

**Subagents and teams.** `SubagentStart`, `SubagentStop`, `TaskCreated`,
`TaskCompleted`, `TeammateIdle`.

**Compaction and model.** `PreCompact`, `PostCompact`, `PreModelSwitch`,
`PostModelSwitch`.

**Filesystem and environment.** `FileChanged`, `CwdChanged`, `DirectoryAdded`,
`WorktreeCreate`, `WorktreeRemove`, `ConfigChange`, `InstructionsLoaded`.

**Setup.** `Setup`.

Every hook payload carries a common core — `session_id`, `transcript_path`,
`cwd`, `hook_event_name` — plus event-specific fields. The ones with direct
bearing on AgentMux are `prompt` and `prompt_id` (UserPromptSubmit),
`tool_name` / `tool_input` / `tool_use_id` (the tool events),
`permission_mode` (SessionStart), `last_assistant_message` and
`stop_hook_active` (Stop), `source` and `reason` (SessionEnd), and
`notification_type` with `message` (Notification). A representative payload
captured during discovery, for the event that carries the most weight here:

```
{"hook_event_name":"Notification","notification_type":"idle_prompt",
 "message":"Claude is waiting for your input","session_id":"<uuid>",
 "cwd":"<discovery temp dir>","prompt_id":"<uuid>","transcript_path":"<path>"}
```

**Hooks are both a notification and a control mechanism.** A hook can observe,
and it can also decide. For `PreToolUse`, returning
`hookSpecificOutput.permissionDecision` of `allow` or `deny` decides the tool
call; for `PermissionRequest`, returning
`hookSpecificOutput.decision.behavior` answers Claude's own permission dialog;
exit code 2 blocks. This is not a subtle point and it was not taken on
documentation. A controlled A/B pair was run in which the only difference between
the two arms was what the hook wrote to stdout: with a hook returning `{}` the
tool call was **refused**; with a hook returning the documented decision JSON the
same tool call was **granted**. Same prompt, same settings, same permissions,
same binary. The hook's stdout is what changed the outcome.

That result is what makes §10's "side-channel adapter" more than a listener. It
also makes hook configuration a genuine trust boundary, which §8 treats as the
most serious security finding of the phase.

## 4. Session Correlation

AgentMux needs a deterministic answer to "which AgentSession is this event
about?" — not a heuristic.

**Every hook payload carries `session_id`, and `transcript_path` names the
transcript file.** Verified across every hook captured during discovery.

**AgentMux can dictate the session id at launch.** `--session-id <uuid>` was
verified to set the id that the session then uses everywhere: the `system/init`
message on the machine-readable stream reported back exactly the UUID on the
command line, and the hooks fired during that run carried the same value.

That is the whole correlation problem solved one level above the wire: the
launcher generates a UUID, hands it to Claude, and every subsequent event — hook
payload or stream message — is self-identifying. No matching on `cwd`, no
matching on start time, no ambiguity when two sessions run in the same directory.

One operational constraint was found by hitting it: **`--session-id` refuses an
id already in use**, with an explicit error naming the collision. A launcher must
therefore generate a fresh UUID per launch rather than reusing one, and must
treat a collision as a launch failure rather than retrying blindly. Since
AgentMux's ids are random, collisions are not a practical concern; the reuse case
is a resumable-session case, and resumption wants `--resume` anyway.

The mapping AgentMux should record at launch is therefore
`AgentSession.id ↔ Claude session UUID ↔ tmux session`, all three known before
the first event arrives.

## 5. Runtime Correlation

Session correlation tells AgentMux *which* session an event belongs to. Runtime
correlation tells it *where* that session is running, which is a different
question and is answered by the launch environment rather than by the payload.

**`cwd` is in every hook payload** and in `system/init`. It is the directory
Claude was launched in, which for an AgentMux runtime is the project's working
directory.

**`transcript_path` is in every hook payload** and is an absolute path under
Claude's own configuration directory, namespaced by a slug derived from the
project path. Useful for correlation and for later transcript reading; not a
path AgentMux should write to.

**MCP servers get `CLAUDE_PROJECT_DIR`** — the project root, set by Claude Code
in the spawned server's environment. Not applicable to the recommended design,
but it is the documented answer for that surface.

Given §4, runtime correlation does not need to be inferred from any of these.
AgentMux knows which tmux pane it launched, knows the UUID it dictated, and
therefore knows which runtime a given `session_id` belongs to without reading
`cwd` at all. `cwd` and `transcript_path` are available as corroboration and for
diagnostics. The important structural point is that **the correspondence is
established at launch, not reconstructed from events** — which keeps faith with
Phase 7.2's rule that state is not re-derived by querying the event log.

## 6. Windows and WSL

**Windows.** Full turn behaviour verified against 2.1.278. The binary is not on
`PATH` (§1). Hooks fire; `--settings` injection works; `--session-id` dictation
works; `--output-format stream-json` and `--input-format stream-json` both work;
a `command` hook runs through the configured shell (Git Bash was used, and the
handler takes an explicit `shell` field). An `http` hook delivered its payload to
a `127.0.0.1` listener with the expected headers and body — but only after the
URL was listed in `allowedHttpHookUrls`, and an unlisted URL is silently not
delivered rather than erroring loudly. That silent-drop behaviour is a real
operational hazard and belongs in any runbook for the adapter.

**WSL.** Version and help text verified; **turn behaviour not verified**, because
the WSL installation is not signed in and signing in is an interactive step this
phase was not permitted to take. What can be said without guessing: the CLI is
native `linux-x64`, the hook system is the same subsystem, and the settings
schema is the same — so the mechanism is expected to behave identically, but
*expected* is the honest word. This is the largest single unverified area in the
document and §12 schedules it as the first task of 7.3B rather than assuming it.

WSL 1 is documented as unsupported; WSL 2 only. Git for Windows is optional but
its absence removes the Bash tool on Windows, which changes hook `shell`
behaviour — worth knowing before configuring a `command` hook on a Windows host.

## 7. Linux

The target deployment for AgentMux is a Linux server, and the pure-Linux column
of this discovery is the thinnest. The mechanism is platform-independent — hooks
are a CLI feature, not a Windows one — but the measurement was taken on Windows
and WSL-without-auth. The one Linux-specific fact that is firm: the CLI ships a
native `linux-x64` build, and the WSL binary launched and reported its version
without any compatibility shim.

The Phase 2.5 compatibility matrix already records the pure-Linux columns as
outstanding for the same reason — they need interactive authentication on the
test host. That work is not duplicated here; §12 makes re-running the hook
experiments on Linux part of 7.3B.

## 8. Security

Five findings, ordered by how much they should change behaviour.

**Hook configuration is a trust boundary, and it is now a two-way one.** §3
proved a hook can grant a permission that would otherwise be refused. Anything
that can write hook configuration for a session can therefore change what that
session is allowed to do. Hook configuration must be supplied per-runtime via
`--settings` and owned by AgentMux, never merged into a user's global settings,
and the settings file must not be world-writable. This is the finding with the
most consequence in the whole document.

**Hook payloads are untrusted input.** They carry `prompt`, `tool_input`,
`last_assistant_message`, and, on `MessageDisplay`, message text — all
model-and-user-derived. They must be treated exactly as Phase 7.2 treats a task
title: as data. Not rendered as HTML, not interpolated into a shell, not written
into a log line that something else parses. Payloads should be truncated at a
documented limit before storage.

**Payloads can contain secrets.** `tool_input` is the arguments to a tool call;
a tool call can contain a credential. The adapter must not copy payloads
wholesale into `agent_events`, and any diagnostic logging must go through the
same redaction rule the rest of the project uses.

**No credential was read, recorded, or written in this phase** — no token, API
key, OAuth material, or keychain entry, and no output that could have contained
one was captured. Where such output was possible, the run was arranged so it was
not produced. The standing rule is unchanged: credentials do not go into
documents, source, `.env`, config files, fixtures, scripts, Git, command lines,
process arguments, or logs. If a future step needs authentication, it stops at
the interactive prompt and reports that authentication is required; it does not
store the credential.

**The HTTP hook's silent drop is a security-adjacent footgun.** A URL not present
in `allowedHttpHookUrls` does not produce an error at the hook site; the event
simply does not arrive. A misconfigured adapter therefore looks like a quiet
agent rather than a broken one. The adapter must distinguish "no events" from
"events not delivered" — for instance by requiring a `SessionStart` acknowledgement
before a runtime is considered observed.

## 9. Architecture Options

Three shapes were on the table, plus the possibility that none of them is right.

**Option A — AgentMux → tmux → CLI, with hooks.** The existing runtime launches
Claude in a tmux pane exactly as it does today; hook configuration is injected
with `--settings`; events arrive at an AgentMux endpoint.

**Option B — AgentMux → Agent SDK → Claude.** AgentMux stops launching a PTY and
instead calls `query()`, receiving typed messages directly.

**Option C — Option A plus a side-channel adapter.** The runtime keeps owning the
pane, and additionally consumes the machine-readable stream the CLI already
emits on its own stdout (`--include-hook-events`) together with the hook
endpoint, so structure arrives on two independent paths.

Option B is where the research pointed hardest *against* the obvious. The SDK
spawns and supervises its own subprocess over stdio pipes; it does not attach to
an existing PTY, and the documentation describes one session as one subprocess
with its own process tree. Adopting it would mean replacing the Runtime Layer's
process ownership rather than integrating with it — the exact thing this phase
was told not to do — and it would additionally require AgentMux to hold its own
API-key credentials, because the SDK's documented auth path is an API key and
offering subscription login through a third-party product is explicitly
disallowed. Option B is not merely more work; it is a different product with a
different billing relationship to Anthropic.

The comparison, using only the four permitted values and no scoring:

| | Hooks | Agent SDK | CLI machine-readable | MCP | Terminal parsing |
|---|---|---|---|---|---|
| Structured | Supported | Supported | Supported | Partial | Not available |
| Real-time | Supported | Supported | Supported | Partial | Supported |
| Waiting for input | Partial | Supported | Partial | Not available | Not available |
| Permission request | Supported | Supported | Partial | Partial | Not available |
| Completion | Partial | Supported | Supported | Not available | Not available |
| Session ID | Supported | Supported | Supported | Unknown | Not available |
| Windows + WSL | Supported | Supported | Supported | Supported | Partial |
| Linux | Unknown | Supported | Unknown | Supported | Supported |
| Complexity | Low | High | Medium | High | — |

Read the table with its footnotes, because several cells are doing work.

*Terminal parsing is "Supported" for real-time and Linux and "Not available"
everywhere else.* That row is the point: parsing the terminal gives you text
promptly on any platform and tells you nothing structured about any of the five
states. It is not a lesser integration; it is not an integration.

*Hooks are "Partial" on waiting-for-input* because the idle notification fires
in interactive sessions and was not observed in print mode (§11). AgentMux runs
interactive sessions, so this is adequate for AgentMux and would not be for a
`-p`-driven design.

*Hooks are "Partial" on completion* because `Stop` marks the end of a turn, not
the end of the work, and there is no hook that means "the task is done" — see
§11.

*Linux is "Unknown" for hooks and CLI streams* purely because it was not
measured on this host (§7). It is not a suspicion that Linux behaves
differently.

## 10. Recommended Architecture

**Option C: keep the Runtime Layer exactly as it is, and add a ClaudeAdapter that
owns agent lifecycle and events.**

The division the directive drew holds cleanly here. Runtime owns the process,
the pane, input, resize, and output. Agent Integration owns what the agent *is
doing*. Nothing in this design asks the Runtime Layer to change: it launches
Claude in tmux today and it would launch Claude in tmux afterwards, with two
additions that are launch arguments, not architecture — a generated `--settings`
file, and a `--session-id` UUID AgentMux chose.

The shape, end to end:

```
Claude Code (in tmux, launched by the Runtime Layer)
   │
   │  hook payloads ─────────────► ClaudeAdapter  ─┐
   │  (--settings, http or command handler)        │
   │                                               ├─► event.Service ─► agent_events
   │  machine-readable stream ──► ClaudeAdapter  ─┘        (the only write path)
   │  (--include-hook-events on stdout)
   │
   └── raw terminal output ──────► Runtime Layer (unchanged; never parsed by the adapter)
```

Why the two input paths rather than one. The hook endpoint carries the events
that matter for state — `Notification`, `SessionStart`, `Stop`, `SessionEnd`,
`PermissionRequest` — and can be acknowledged, retried, and reasoned about. The
machine-readable stream rides on the process AgentMux already owns, costs no
configuration, works even if the hook endpoint is unreachable, and carries
`system/init` with `session_id`, `cwd`, and `model` plus a `result` message that
marks turn completion. They are not redundant in a wasteful sense: one is a
push channel and the other is a pull channel over ownership, and the failure
modes do not overlap. The adapter treats the hook channel as authoritative for
state transitions and the stream as authoritative for session identity and turn
completion, and records which channel a given event came from.

Why not the SDK. Restated because it is the decision most likely to be
questioned later: it cannot attach to the pane AgentMux owns, adopting it means
replacing the Runtime Layer, and its documented auth model requires credentials
AgentMux would have to hold. It remains a reasonable choice for a *different*
product that never had a tmux Runtime Layer. It is not this one.

Why the adapter writes through `event.Service` and nothing else. Phase 7.1
established one write path to `agent_events` and Phase 7.2 kept it. The adapter
is a producer of events, not a second writer. It does not open the database, and
Claude is never given access to the database, the API, or anything but its own
settings file and stdin.

Why this does not violate the Phase 7.2 rule against re-deriving state. The rule
forbids recomputing a task's status by querying the event log. This design does
not do that: hook events arrive as *new observations* and move a session's status
through the existing service, exactly as an API caller does today. The event log
remains a record, not an input.

The mapping from Claude's vocabulary to AgentMux's, with the reliability of each
row grounded in §3 and §11:

| Claude signal | AgentMux event | Reliability |
|---|---|---|
| `SessionStart` (with dictated `session_id`) | `session.started` | Verified |
| `UserPromptSubmit` | `session.turn_started` | Verified |
| `PreToolUse` / `PostToolUse` | `session.tool_used` | Verified |
| `Notification` / `permission_prompt` | `session.permission_requested` | Verified (interactive) |
| `Notification` / `idle_prompt` | `session.waiting_for_input` | Verified (interactive) |
| `PermissionRequest` + decision output | `session.permission_answered` | Verified, controlled pair |
| `Stop` | `session.turn_completed` | Verified |
| `system/init` on the stream | session identity binding | Verified |
| `result` message on the stream | `session.turn_completed` | Verified |
| `StopFailure` | `session.failed` | **Not observed** |
| `SessionEnd` | `session.ended` | Verified |
| *process exited* | **not** `session.completed` | See §11 |
| *terminal text* | nothing. ever. | Not an integration |

Note the two rows that are deliberately not mapped. `process exited` is not
completion: a process exit is a fact about a process, and a session can exit
after a successful turn, after a refusal, after a crash, or because someone
closed a pane. Mapping it to `COMPLETED` would put a claim in the record that
nothing observed. It maps to `session.ended` with a reason, and the state
machine decides what that means — which is what Phase 7.2's session statuses
were built for. And `StopFailure` is `Not observed`: an attempt to provoke it
with a deliberately invalid model name was handled locally and the turn
succeeded anyway, so no payload was captured and none is described.

## 11. Known Limitations

**"Waiting for user input" is observable, and it is observable only in
interactive sessions.** Claude Code emits `Notification` with
`notification_type: "idle_prompt"` and the message "Claude is waiting for your
input" roughly 55 seconds after the session goes idle. This was captured
directly. In print mode it never fired — across every `-p` run performed, the
count was zero, while `PermissionRequest` fired normally in the same runs. The
distinction is real and it is documented as interactive-only.

This does not block AgentMux, because AgentMux's runtime is an interactive
Claude in a tmux pane, which is exactly the case where the notification fires.
It would block a design built on `-p`. It is stated prominently because it is the
single fact most likely to be discovered late if the runtime model ever changes.

**The `Notification` event covers two different things.** `notification_type` is
`permission_prompt` for a permission request and `idle_prompt` for a wait for
input; `auth_success` and `elicitation_dialog` are also in the vocabulary.
Permission and waiting must be distinguished by reading that field, not by
treating `Notification` as one signal. A permission request arrives *both* as a
`Notification` with `permission_prompt` and as a `PermissionRequest` hook; the
latter is the more useful of the two because it is the one that can be answered.

**Process exit is not task completion.** Stated in §10's table and repeated here
because it is the most common way this class of integration goes wrong. `Stop`
fires at the end of a turn while the process stays alive; `SessionEnd` fires when
the session ends and carries a `reason`. Neither is "the work is done", and
there is no hook that means "the work is done" — that judgement belongs to
AgentMux's own task lifecycle, not to Claude.

**Terminal parsing is not reliable enough for primary AgentMux state detection.**
This is the explicit finding the phase asked for. Whatever a regex over the pane
could find — a spinner, a prompt marker, a coloured line — there is a
machine-readable signal for the same fact (§3, §10), and the text form is
version-specific, locale-specific, width-dependent, and confounded by anything
else that writes to the pane. Terminal parsing is not a fallback for the states
in §3; it is a different and worse instrument. The Runtime Layer will continue to
carry raw output to the browser because that is its job, and the adapter will not
read it.

**Several things are documented but were not verified on this host**, and are
recorded as such rather than smoothed over: WSL turn behaviour (§6), pure Linux
(§7), `CLAUDE_CODE_SESSION_ID` (§2.6), `StopFailure` (§10), and the `mcp_tool`
and `agent` hook handler types (accepted by the schema, not exercised).

**One anomaly was explained rather than left open.** An HTTP handler registered
for `SessionStart` never fired while HTTP handlers for other events did.
Cross-referencing the documentation gives the reason: `SessionStart` and `Setup`
support only `command` and `mcp_tool` handlers. It is a documented constraint,
not a defect.

**The machine-readable stream carries no stability guarantee.** It is not
labelled experimental and it is not labelled stable either — a genuine gap in
the documentation. What the reference does show is version gating on individual
fields and a `capabilities` array that exists precisely so consumers can detect
features instead of comparing version strings. Treat the stream as version-gated
and evolving, pin a version, and feature-detect where the surface allows it.

**Hook delivery can fail silently.** An `http` hook whose URL is not in
`allowedHttpHookUrls` produces no error at the hook site; the event does not
arrive. A misconfigured adapter resembles a quiet agent. §8's acknowledgement
requirement is the mitigation.

**`--session-id` refuses an id already in use.** Operational, not a limitation of
the design, but a launcher must generate per launch and treat a collision as
failure.

## 12. Phase 7.3B Implementation Plan

Not started, not stubbed, nothing in the current build anticipates it. This is
what 7.3B would do, in the order that retires risk fastest.

**1. Close the platform gap first.** Re-run the core hook experiment on pure
Linux and on a signed-in WSL, since those are the deployment targets and they are
the thinnest measurements in this document (§6, §7). Everything below is worth
less if the mechanism behaves differently there.

**2. `internal/claudeadapter`.** A package following the project's existing
shape — model, repository, service, errors — that receives Claude events and
produces AgentMux events. It owns no process and opens no database; it calls
`event.Service`, which remains the only write path to `agent_events`.

**3. Hook configuration generation, owned per runtime.** A settings document
built at launch and passed with `--settings`, covering the events in §10's
mapping table. The settings file lives with the runtime's own state, is not
merged into user settings, and is removed with the runtime. The `http` handler is
preferred where it applies, with the URL allowlist written into the same
document, and a `command` handler as the fallback where it does not —
`SessionStart` being the known case, since it does not accept an HTTP handler.

**4. Session identity binding at launch.** Generate the UUID, pass
`--session-id`, record `AgentSession.id ↔ Claude UUID ↔ tmux session` before the
first event, and treat an id collision as a launch failure.

**5. The set of AgentMux events.** `session.started`, `session.turn_started`,
`session.tool_used`, `session.permission_requested`, `session.permission_answered`,
`session.waiting_for_input`, `session.turn_completed`, `session.failed`,
`session.ended`. Written through `event.Service` with source `agent`, with the
existing `sessionEventPayload` shape, and with payload truncation and the
project's redaction rule applied (§8).

**6. Status transitions.** Map §10's signals onto the Phase 7.2 session and task
lifecycles using the existing controlled transitions, and change no transition
rule. The `COMPLETED → RUNNING` prohibition stands. Task status is not re-derived
by querying events.

**7. The stream channel.** Consume `--include-hook-events` and
`--output-format stream-json` from the process the Runtime Layer already owns,
for session identity and turn completion. This is additive to the hook channel,
not a replacement for it, and the Runtime Layer's ownership of that output is not
changed by reading it.

**8. The silent-drop guard.** A `SessionStart` acknowledgement before a runtime
is considered observed, so that a broken hook configuration reads as broken
rather than as idle (§8).

**9. Tests.** Unit tests over the mapping table using captured payloads with all
secrets absent; an integration test that launches a real Claude on Linux and
asserts the event sequence for a turn; a test that a permission-request event is
recorded without being answered, since the adapter observes and does not decide.

**10. Documentation.** `docs/API.md` for any new endpoint the hook receiver
needs, and a runbook for the failure modes in §11.

**Deliberately out of scope for 7.3B:** answering permission prompts on the
user's behalf (the adapter observes; deciding is a later phase with its own
threat model), any UI, any dashboard, and any change to the Runtime Layer's
process, pane, or output handling.
