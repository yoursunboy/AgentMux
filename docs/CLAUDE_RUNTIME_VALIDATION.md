# Claude Runtime Environment Validation

Phase 7.3B-0. Phase 7.3A established *what* Claude Code can report. This phase
establishes *where* it can report it — whether the mechanism discovered there
actually works on Windows, on WSL, and on pure Linux, rather than on the one
machine it happened to be measured on.

Nothing here is implemented. No production code, database, migration, API, or
frontend was changed, and no hook configuration was added to any project. Every
experiment ran in a throwaway directory and was deleted afterwards.

The short answer: **Windows is verified end to end, WSL is verified for
everything except the model turn itself, and pure Linux is not verified at all** —
because the validation host needs interactive authentication, which this phase
stops at rather than working around. One result from the WSL work is more
important than its column suggests and is worth reading even if the matrix is all
you want: hooks fire, and are delivered, *before and independently of*
authentication. That is recorded in §11 and it changes what a ClaudeAdapter can
rely on.

## 1. Environment

| Environment | Version | Result |
|---|---|---|
| Windows | 2.1.278 | **Verified** — full turn, all capabilities exercised |
| WSL (Ubuntu 24.04.4, kernel 5.15.167.4-microsoft-standard-WSL2) | 2.1.274 | **Verified for hooks, settings, session id and the stream; the model turn is not verified** — the installation is not logged in |
| Linux (`128.1.128.21`, the Phase 2.5 validation host) | — | **Not verified** — the host requires interactive authentication; see §10 |

Windows binary: `C:\Users\yours\.local\bin\claude.exe`, a native Windows PE
executable. WSL binary: a native `ELF 64-bit LSB executable, x86-64` at
`/home/sunboy/.local/share/claude/versions/2.1.274`, reached through the symlink
`/home/sunboy/.local/bin/claude`.

**One correction to Phase 7.3A.** That phase recorded that `claude` was not on
`PATH` on Windows and that only the absolute path into the VS Code extension
directory worked. That is no longer true: `claude` now resolves on `PATH` in both
PowerShell and Git Bash, to a 2.1.278 binary in the user's own `.local\bin`. The
7.3A observation was accurate when it was made — three extension versions
(2.1.274, 2.1.275, 2.1.278) are installed side by side and the `PATH` entry
appeared during that same day. It is recorded here because a launcher that
hard-codes the extension path is now working around a problem that has gone
away, and because `docs/CLAUDE_INTEGRATION.md` §1 says otherwise.

One nuance on WSL: `claude` is on `PATH` in a **login** shell and not in a
non-login shell. A launcher that runs `bash -c` rather than `bash -lc` will not
find it.

## 2. Claude Versions

```
Windows   2.1.278 (Claude Code)
WSL       2.1.274 (Claude Code)
Linux     not reached
```

The two installed versions differ by four patch releases. Since the hook and
stream surfaces are version-gated field by field, the Windows column of this
document describes 2.1.278 and the WSL column describes 2.1.274; they are not
interchangeable evidence for each other, and where a result was obtained on only
one of them it is marked as such.

## 3. Capability Matrix

Values are restricted to **Verified / Not verified / Failed / Not available**.
No cell is a guess.

| Capability | Windows | WSL | Linux |
|---|---|---|---|
| Start | **Verified** | Not verified ᴬ | Not verified ᴰ |
| Session ID | **Verified** | **Verified** | Not verified ᴰ |
| Hooks | **Verified** | **Verified** | Not verified ᴰ |
| HTTP Hook | **Verified** | **Verified** | Not verified ᴰ |
| Command Hook | **Verified** | **Verified** | Not verified ᴰ |
| stream-json | **Verified** | **Verified** | Not verified ᴰ |
| Permission | **Verified** | Not verified ᴮ | Not verified ᴰ |
| Waiting Input | **Verified** | Not verified ᴮ | Not verified ᴰ |
| Completion signals | **Verified** | Not verified ᶜ | Not verified ᴰ |

ᴬ *Start* is the model turn. Everything a session does *around* the turn —
starting, reading settings, firing hooks, allocating a session id, ending — was
verified on WSL; only the API call is blocked.

ᴮ *Permission* and *Waiting Input* both require a live interactive session, which
requires authentication. Neither could be reached on WSL.

ᶜ WSL did produce a `SessionEnd` hook and a final `result` envelope, so parts of
the completion path were observed. `Stop` — the signal that actually means "the
turn completed" — did not fire, because the turn could not complete. Recorded as
Not verified rather than as a partial success, since the matrix has no value for
partial.

ᴰ The Linux host was reachable on port 22 and refused nothing; it was simply not
authenticated. See §10. The WSL results mean the underlying mechanism is known to
work on a native Linux kernel with a native Linux binary, which reduces the risk
that the Linux column will differ — but that is an expectation, and this document
does not record expectations as results.

## 4. Windows

**Version and binary.** 2.1.278, `C:\Users\yours\.local\bin\claude.exe`, on
`PATH`. Nothing needed to be installed, changed, or added to `PATH` for this
phase.

**Session id — Verified.** A turn launched with
`--session-id 7b3b0a10-0000-4000-8000-0000000000a1` reported that exact UUID back
in five independent places: the `SessionStart`, `UserPromptSubmit`, `Stop` and
`SessionEnd` hook payloads, the `system/init` message, and the final `result`
message. Across the whole run there was exactly one distinct session id. This is
the §7 mapping from 7.3A confirmed on a second version.

**Settings injection — Verified.** Hook configuration was supplied with
`--settings <file>` from the throwaway directory, never from a project directory
and never merged into the user's own settings. Every declared hook fired, which
is what proves the file was read. The file was deleted with the directory.

**Command hooks — Verified.** Four events were declared and four arrived:
`SessionStart`, `UserPromptSubmit`, `Stop`, `SessionEnd`. The payloads carry
`session_id`, `cwd`, and `transcript_path` on all four, and differ elsewhere:

| Event | Additional fields observed |
|---|---|
| `SessionStart` | `source` |
| `UserPromptSubmit` | `prompt`, `prompt_id`, `permission_mode` |
| `Stop` | `background_tasks`, `effort`, `last_assistant_message`, `prompt_id`, `session_crons`, `stop_hook_active`, `permission_mode` |
| `SessionEnd` | `reason`, `prompt_id` |

Only the field *names* were recorded. Prompt text, tool input, and assistant
message content were never written to disk, which is why the probe records a key
list rather than a payload.

**HTTP hooks — Verified.** With `allowedHttpHookUrls` listing
`http://127.0.0.1:8901/*` and hooks declared at `http://127.0.0.1:8901/hook`, the
CLI delivered `POST /hook` with `Content-Type: application/json` and a JSON body
equal to the same payload a command hook receives. Three events arrived —
`UserPromptSubmit`, `Stop`, `SessionEnd`.

`SessionStart` was deliberately declared as an HTTP hook alongside the others and
**did not arrive**. That is not a defect: `SessionStart` and `Setup` accept only
`command` and `mcp_tool` handlers. It is recorded here as a reproduced negative
control — the same result 7.3A found, now confirmed on a run where the other
three events in the same configuration all arrived.

**stream-json — Verified.** `--output-format stream-json --verbose
--include-hook-events` produced `system/init` (1), `system/hook_started` (3),
`system/hook_response` (3), assistant messages (2), and `result` (1) on stdout,
with stderr empty and exit code 0. `system/init` carried `session_id`, `cwd`,
`model`, `permissionMode`, and `tools`.

Note the count: three hook lifecycles on the stream against four hook events
delivered to the endpoint. `SessionEnd` does not produce a `hook_started`, which
matches the documented exception list. An adapter that counted stream hook events
and cross-checked them against endpoint deliveries would see a permanent
discrepancy on every clean shutdown, and should expect it.

**Permission — Verified.** A prompt asking Claude to delete a file produced three
`PermissionRequest` hooks. Each carried `session_id`, `tool_name`
(`Bash`, then `PowerShell`), `tool_input`, `permission_mode`, `effort`,
`permission_suggestions`, `prompt_id`, `cwd`, and `transcript_path`. Since the
probe returned `{}` and therefore expressed no decision, the permission was not
granted and **the file was still present afterwards** — which is the check that
the observation was passive.

Worth stating plainly, because it is easy to get backwards: the *decision fields
are not in the payload*. The payload offers `permission_suggestions`; the
decision is what the hook writes to stdout. §8 of `docs/CLAUDE_INTEGRATION.md`
covers the answer half.

**Waiting Input — Verified.** In an interactive background session, `Notification`
with `notification_type: "idle_prompt"` and the message "Claude is waiting for
your input" arrived **68 seconds** after the session went idle. Phase 7.3A
measured ~55 seconds on the same host. The delay varies; it is a promptness
figure, not a contract. As in 7.3A, no `Notification` fired in print mode.

## 5. WSL

**Version and binary — Verified.** 2.1.274, native `ELF 64-bit LSB executable,
x86-64`, on `PATH` in a login shell. `tmux 3.4` is installed at `/usr/bin/tmux`.

**Authentication — the blocker.** A turn was attempted with no credential
supplied and returned:

```
Not logged in · Please run /login
```

No account was entered, no token was read or written, and no credential was
modified. Per the standing rule this is where the turn-level checks stop, and
they are recorded as *Not verified because authentication unavailable* rather
than estimated.

**What WSL nevertheless verified.** Authentication gates the *model call*, not the
CLI's own machinery. Running the same command with `--settings`,
`--session-id`, `--output-format stream-json --verbose --include-hook-events`
produced, before the auth failure:

- a `SessionStart` **command hook that executed and whose output was captured** —
  `{"hook_name":"SessionStart:startup","output":"probe\n","exit_code":0,"outcome":"success"}`;
- the dictated UUID `7b3b0a10-0000-4000-8000-0000000000b1` in the hook payload,
  in `system/init`, in the assistant message, and in `result` — one distinct
  session id across the run;
- a full `system/init` envelope: `cwd`, `tools`, `model`, `permissionMode`,
  `claude_code_version`, `capabilities`;
- a final `result` envelope;
- an **HTTP hook** delivering `POST /hook` with `Content-Type: application/json`
  for `UserPromptSubmit` and `SessionEnd` to a listener inside WSL.

A control run with a deliberate typo (`--definitely-not-a-flag`) returned
`error: unknown option`, which shows the flag parser rejects what it does not
know — so the acceptance above is the parser genuinely taking the flags, not
ignoring them.

The practical consequence is in §11 and it is not small.

**Not verified on WSL:** the model turn, permission requests, and the idle
notification — all three need a live interactive session.

## 6. stream-json

Covered per environment in §4 and §5. The summary: verified on Windows with a
complete and successful turn; verified on WSL for everything the CLI emits
independently of the model, terminating in an authentication error.

Two shape findings that the adapter design depends on are recorded in §11,
because they are findings rather than environment results.

## 7. PermissionRequest

Verified on Windows only; see §4. On WSL the request cannot be reached without a
turn.

What the Windows result establishes for the adapter: a permission request is
observable **without the adapter deciding anything**. The probe returned `{}` —
the documented "no opinion" response — and the tool call was refused and the file
left alone. Observation and control are separable, and 7.3B can take the
observation half without taking on the trust boundary that comes with the
control half.

## 8. Waiting Input

| Environment | Produced | Delay | Condition |
|---|---|---|---|
| Windows | yes | 68 s | interactive background session |
| Windows (7.3A) | yes | ~55 s | interactive background session |
| Windows, print mode | **no** | — | never, across every `-p` run |
| WSL | not verified | — | requires authentication |

"Not produced" is not recorded as a failure: print mode has no idle wait to
report, so the absence is the correct behaviour rather than a missing capability.
The interactive case is the one AgentMux runs.

## 9. Differences

**Windows vs WSL, where both were measured.** No divergence was found in session
id handling, settings injection, command hooks, HTTP hooks, or the stream. The
dictated-UUID result is identical on both, on different versions and different
binaries. The two environments differ in what could be *tested*, not in what was
observed.

**Version difference.** The columns describe 2.1.278 and 2.1.274. The WSL stream
carried a `capabilities` array (`interrupt_receipt_v1`,
`interrupt_cancel_queued_v1`, `msg_lifecycle_v1`) that the Windows run was not
examined for. Version-gated behaviour should be detected through that array
rather than assumed to match across the two.

**Platform-specific tooling.** On Windows the permission prompt produced
`tool_name` values of `Bash` and `PowerShell`; on a Linux host the shell-tool
story is different again. An adapter must not assume a fixed tool vocabulary.

**Linux is not a measured difference at all** — it is an unmeasured column, and
the difference between it and the other two is authentication, not behaviour.

## 10. Linux

**Not verified. The host requires interactive authentication, and this phase
stopped at that step.**

What was done, so the stop is auditable: the host `128.1.128.21` was confirmed
reachable with port 22 open; PuTTY's `plink.exe` was used, since `docs/serverinfo.md`
directs that Windows' own SSH tools not be used for this host; the host key was
verified against the fingerprint already recorded in the user's own
`known_hosts` rather than trusted blindly; and key-only authentication was
attempted in batch mode, which cannot prompt for a password.

It stopped for two independent reasons. `plink` cannot read the user's
`id_ed25519_yunxiao` key at all — `Unable to use key file … (OpenSSH SSH-2
private key (new format))` — and that key is scoped to Aliyun Codeup, not to this
host, so it would not be the right credential even if it could be read.

No password was entered, stored, passed on a command line, written to a file, or
recorded anywhere. `docs/serverinfo.md` names interactive authentication as the
expected path for this machine and instructs that a non-interactive environment
stop and report rather than store a credential; that is what happened.

**What would close this column:** an interactive PuTTY session to the host, or an
authorized key for `dbroot@128.1.128.21` placed in the user's own SSH
configuration. Either makes the Linux column measurable in one pass of the same
experiments, using the `agentmux-test-*` socket prefix and the isolation rules
`docs/serverinfo.md` requires. Nothing in the Windows or WSL results suggests the
Linux column will differ, but that remains an expectation.

## 11. Findings beyond the matrix

These are not environment results. They are properties of the mechanism that this
phase discovered while validating, and they change the adapter design.

**1. Hooks fire without authentication.** Proven on WSL: with no credentials at
all, a `SessionStart` command hook executed, its stdout was captured, and an HTTP
hook for `UserPromptSubmit` and `SessionEnd` was delivered — while the model turn
failed with `authentication_failed`. Hooks are wired into the CLI's session
lifecycle, not into a successful API call. An adapter therefore observes session
start, prompt submission, and session end even when the account is broken, which
is exactly when an operator most wants to be told something.

**2. `result.subtype` is not a completion signal — `is_error` is.** The failed WSL
turn produced:

```json
{"subtype":"success", "is_error":true, "terminal_reason":"api_error",
 "result":"Not logged in · Please run /login", "num_turns":1}
```

`subtype` was `"success"` on a run that did not succeed. Keying completion on
`subtype` would record an authentication failure as a completed turn. A
completion check must read `is_error`, and `terminal_reason` is the field that
says why. This corrects the plain reading of `docs/CLAUDE_INTEGRATION.md` §3 and
§10, where the `result` message was listed as marking turn completion without
that qualification.

**3. Hook output is carried into the stream verbatim.** The
`system/hook_response` message includes an `output` / `stdout` field containing
what the hook printed. A hook that echoes anything sensitive puts it on the
stream that the adapter is reading. The redaction rule in
`docs/CLAUDE_INTEGRATION.md` §8 must apply to the stream as well as to the
endpoint, and hooks written for AgentMux should print nothing they would not
write to a log.

**4. The same session id reaches the adapter through every path.** Verified
independently on Windows 2.1.278 and WSL 2.1.274: hook payloads, `system/init`,
the final `result`, and (in 7.3A) the `--session-id` echo all agree, and a run
contains exactly one distinct id. The binding in 7.3A §4 holds on both.

**5. `SessionStart` does not accept an HTTP handler.** Reproduced on Windows as a
negative control in a configuration where three other events were delivered
normally. An adapter must use a `command` (or `mcp_tool`) handler for
`SessionStart`, which means the hook configuration is not uniform across events.

## 12. Method and cleanup

Every experiment ran from a throwaway directory — the Windows equivalent of
`/tmp/agentmux-claude-validation` and `/tmp/agentmux-claude-validation` inside
WSL. Nothing was written to `internal/`, `web/`, `migrations/`, or any project
directory, and the settings file used for hook injection was passed with
`--settings` rather than placed in a project.

The probe records a payload's **shape**, not its content: event name, session id,
working directory, tool name, notification type, and a sorted list of top-level
keys. Prompt text, tool input, and assistant message content were deliberately
not persisted. All experimental logging was confined to the throwaway directories.

Cleanup, verified rather than assumed: both directories deleted; the Claude
transcript directories the runs created deleted; the Windows HTTP listener
stopped and ports 8901/8902 confirmed closed; the background Claude session
stopped and removed and `claude agents --json` confirmed no session of this
phase's remains. The two interactive sessions that `claude agents --json` still
lists belong to the user, in their own project directories, and were left alone.
No tmux command was run at any point in this phase; the default socket was never
touched and no `agentmux-test-*` session was created, because no host was
reached that has tmux to run.

No credential was displayed, captured, or stored. No API key, token, or password
appears in this document, in the experiment logs, or in the commit that carries
them.
