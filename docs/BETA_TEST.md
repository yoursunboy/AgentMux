# AgentMux Beta Test

This is the procedure for putting AgentMux on a Linux server and finding out whether it works, from
someone who is not the person who wrote it. It is written to be run in one sitting, top to bottom,
and every step says what you should see rather than only what to type.

`docs/DEPLOYMENT.md` is how to install it and what every setting means; `deploy/linux/README.md` is
the operator's reference for the unit. **This document is the test**, and it exists because a beta
needs a shared answer to "what does it mean that it worked", which is a question that has no answer
in an installation guide.

Nothing in this document adds a feature. Phase 7.5 changed no behaviour of the product: it added a
configuration file, a systemd unit that already existed and was extended, one health route under the
API's prefix, one read-only diagnostic, and an opt-in counter. If a scenario below fails, it is
either a deployment mistake or a real bug, and it is never "the beta half is not finished".

---

## 1. What you need

- An Ubuntu 22.04 or 24.04 server, or any distribution with systemd, sudo, and 22 open.
- A Linux account you can `sudo` from. The service runs as its own unprivileged account, which
  `install.sh` creates; you do not work as that account.
- tmux, git and the Go toolchain, **only if you are building from source** (§3). Installing a
  released bundle needs none of them — `install.sh --with-deps` installs tmux for you.
- A browser, and for §7 an iPad or a phone.

One thing you do **not** need: a Claude Code login. Most of this procedure works without one, and
the scenarios that want one say so and can be skipped. The server is not signed in to anything and
never asks.

---

## 2. Installing — the short version

On the server:

```bash
git clone https://github.com/yoursunboy/AgentMux.git
cd AgentMux
sudo deploy/linux/install.sh --with-deps
```

`--with-deps` installs tmux when it is missing. Without it, the installer still succeeds and the
dashboard reports a runtime that cannot start — which is a supported state and not a failure, so if
you want to see what that looks like first, leave it off and turn §5 into a check on the message.

What it creates, and where:

| Path | What |
| --- | --- |
| `/opt/agentmux/` | the server binary and the built UI |
| `/etc/agentmux/agentmux.yaml` | the configuration file — **this is the one you edit** |
| `/var/lib/agentmux/` | the database, and one tmux socket per project under `tmux/` |
| `/etc/systemd/system/agentmux.service` | the unit |
| `/srv/projects/` | the Projects Root: the directories AgentMux scans for projects |

The installer starts the service and then checks it. **`/health` and `/api/health` are both asked**,
and the last lines it prints should be a health body and a line saying a terminal runtime can run
here. If the second half is missing, the server is up and cannot host a terminal — which is stated
as a warning and is the single most likely thing to be wrong on a fresh machine.

Check it yourself:

```bash
systemctl status agentmux
curl -s http://127.0.0.1:8787/health; echo
curl -s http://127.0.0.1:8787/api/health; echo
```

```json
{"status":"ok","version":"0.7.5","commit":"a1b2c3d4e5f6","runtime":"available"}
{"status":"ok","version":"0.7.5","uptime":"12s","runtimeAvailable":true}
```

The first field of the first body is liveness and the last is readiness, and they are different
questions: a server on a host with no tmux answers `ok` and `"runtime":"unavailable"`, which is a
working AgentMux that cannot host a terminal. `docs/DEPLOYMENT.md` §8 is the long form.

---

## 3. Building it from source on the server

Skip this if you installed a bundle. It is what the phase brief asks for — build it and run the tests
on the server itself — and it is worth doing at least once before a beta, because a cross-compiled
binary and a natively built one are not the same artifact.

```bash
cd AgentMux
go build ./...          # compiles every package
go vet ./...            # the same checks `go test` runs, without running it
go test ./...           # the whole suite
cd web && npm ci && npm run build && cd ..
go build -o agentmux-server ./cmd/server
```

The suite includes the deployment contract test, which is the part that matters here: it reads
`deploy/linux/agentmux.service`, `deploy/linux/install.sh` and `config/agentmux.example.yaml` and
asserts they agree with the program they deploy — that every substitution the installer makes still
matches a line in the unit, that `KillMode=process` is still set, and that the example configuration
still loads through the real loader. It runs in about two seconds and needs no server and no
network.

Then run it in the foreground, which is what you want for the first look because every startup line
goes to your terminal rather than to the journal:

```bash
./agentmux-server -beta
```

A note on `-beta`: see §10. It changes nothing you can see, and it is on here only so the counter in
§10 has something in it.

---

## 4. Getting a browser to it

The server binds `127.0.0.1` by default, and that is deliberate rather than an oversight: AgentMux
has no authentication, so who can reach the port is the whole of its access control. There are two
ways to get a browser to it, and they are not equivalent.

**A tunnel — recommended, and what this document's scenarios assume.** From your own machine:

```bash
ssh -L 8787:127.0.0.1:8787 <you>@<server>
```

Then open `http://127.0.0.1:8787/dashboard` locally. The port is never exposed, the tunnel is
encrypted, and closing the SSH session closes the access. On Windows, PuTTY does the same through
Connection → SSH → Tunnels, with source `8787` and destination `127.0.0.1:8787`.

**Binding a public interface.** Setting `server.host: 0.0.0.0` in `/etc/agentmux/agentmux.yaml` and
restarting serves the console to anyone who can reach the port, with no password. That is a real
choice an operator is allowed to make — on a private network with a firewall, for a beta — but it is
not the default and the file says so where you would change it. `docs/SECURITY.md` §1 and §13 are
the full statement; if you do this, the beta's own console is unauthenticated on your network.

---

## 5. Scenario 1 — a project, a runtime, and a tmux session

**What it proves:** the server can create a project, start a runtime in it, and the runtime is a real
tmux server on this machine rather than a state the database believes in.

### Create the project

```bash
curl -s -X POST http://127.0.0.1:8787/api/projects \
  -H 'Content-Type: application/json' \
  -d '{"name":"BetaCheck"}' | tee /tmp/betacheck.json
```

```json
{"project":{"id":"p_7f3a91c4e0b2d685","name":"BetaCheck","hostPath":"/srv/projects/BetaCheck","...":"..."}}
```

The server chose the path — `/srv/projects/BetaCheck` — because the first configured Projects Root
is the default target. Keep the id; the steps below use it.

```bash
PROJECT_ID=$(grep -o '"id":"p_[^"]*"' /tmp/betacheck.json | head -1 | cut -d'"' -f4)
echo "$PROJECT_ID"
ls -d /srv/projects/BetaCheck
```

The directory exists on disk. AgentMux created it, which is the one case where it does: registering
an *existing* folder never moves it.

### Start the runtime

```bash
curl -s -X POST "http://127.0.0.1:8787/api/projects/$PROJECT_ID/runtime/start" | tee /tmp/runtime.json
```

```json
{"runtime":{"projectId":"p_7f3a91c4e0b2d685","backend":"tmux","session":"amx-p_7f3a91c4e0b2d685","state":"RUNNING","sessionAlive":true,"cols":120,"rows":30,"startedAt":"...","updatedAt":"..."}}
```

`state` is `RUNNING` and `sessionAlive` is true. Starting a runtime that is already running returns
what is already there rather than making a second one, so this is safe to run twice — and worth
running twice, because a user who reloads a page and clicks again must not end up with two terminals
writing to one repository.

### Confirm tmux really has it

```bash
sudo -u agentmux tmux -S /var/lib/agentmux/tmux/$PROJECT_ID.sock ls
```

```
amx-p_7f3a91c4e0b2d685: 1 windows (created Mon Sep 22 10:04:11 2026)
```

**Note the `-S`.** Every project gets its own tmux *server* on its own socket, named after the
project id, and that is the whole of the fault isolation: a tmux server is identified by its socket,
so two sockets are two servers and neither can take the other down. A bare `tmux ls` would look at
the default socket, which belongs to whoever else is on this machine and has nothing to do with
AgentMux. **Never run a bare `tmux ls`, `tmux kill-server` or `pkill tmux` on a shared machine** —
the sockets AgentMux owns are the ones under `/var/lib/agentmux/tmux/`, and they are the only ones
this test should ever name.

**The session's working directory is the project directory**, not the Projects Root and not the
collection. A terminal that opens one level too high is worse than no terminal, so it is worth
checking directly:

```bash
sudo -u agentmux tmux -S /var/lib/agentmux/tmux/$PROJECT_ID.sock display -p -t amx-$PROJECT_ID '#{pane_current_path}'
```

```
/srv/projects/BetaCheck
```

### If it did not work

- **`state: "STOPPED"`, or the start returns an error naming tmux.** Run
  `journalctl -u agentmux -n 50` and look for the dependency probe's line. On a host where tmux is
  installed but not on the service account's `PATH`, the binary can be named explicitly with
  `terminal.tmuxBinary` in the configuration file.
- **`{"error":{"code":"runtime_unavailable"…}}`** — this build has no runtime, or the host is on the
  wrong side of a boundary. `curl -s http://127.0.0.1:8787/api/server` reports the three conditions
  and which one failed.

---

## 6. Scenario 2 — the console in a browser

**What it proves:** the built frontend is being served, it can reach the API, and the project you
just made is on it.

Open `http://127.0.0.1:8787/dashboard` through the tunnel from §4.

You should see the server bar across the top reading `Online`, `Runtime: available` and a version,
and below it a card for **BetaCheck** showing `Running` with the terminal size. The browser console
should be free of errors, and the Network tab should show `/api/controller` polled every few
seconds — that polling is the console's only traffic when nothing is happening.

What to check, in order:

1. **The version in the bar matches `/health`.** It is the same string from the same build; a
   mismatch means the UI you are looking at was not built from the binary you installed.
2. **`Runtime: available`.** If it says `Unavailable`, the console is telling you what `/health`
   already did, and §5's troubleshooting applies.
3. **BetaCheck is listed and Running.** If the card says `Stopped` while §5 said `RUNNING`, the
   console is reading a different server or the runtime was stopped in between.
4. **Opening the project shows a terminal**, and the terminal shows a shell prompt at
   `/srv/projects/BetaCheck`.

Also check the failure path, which is cheap and is the part people skip: **stop the service and
reload the page.**

```bash
sudo systemctl stop agentmux
```

The console should say it cannot reach the server — in the bar, and as a message on the page — and
it should not go blank, freeze, or lose the layout you had. Start it again and reload; the project
should come back by itself with no action from you.

```bash
sudo systemctl start agentmux
```

---

## 7. Scenario 3 — the iPad

**What it proves:** the terminal is a real one on a touch device, which is the whole reason this
project exists.

On the iPad, with the tunnel from §4 open (an SSH client that supports port forwarding, or the same
tunnel from a laptop on the same network — the server does not care which device asks):

1. Open `http://127.0.0.1:8787/dashboard` in Safari.
2. Open BetaCheck's terminal.
3. Type something into the shell. **Safari on iOS puts up no keyboard until you tap the terminal**,
   and the terminal does not take the keyboard until you do — this is the single most common
   confusion, and it is not a bug.
4. Rotate the device. The terminal should re-fit to the new width rather than staying at the old
   one, and the shell inside should redraw.
5. Lock the screen for a minute, then unlock. The connection will have dropped and should
   re-establish by itself, **and the output that arrived while you were away should be there** —
   that is what the server keeps recent chunks in memory for. If the reconnect shows a blank
   terminal, that is a real bug worth reporting.

Two things behave differently from a desktop browser, and both are expected: a soft keyboard raises
no `resize` event in the same way a real one does, so the first fit can be a line or two short until
you rotate or scroll; and Safari discards background tabs aggressively, so a terminal in a tab you
have not looked at for ten minutes will reconnect when you return rather than having stayed
connected.

---

## 8. Scenario 4 — taking the keyboard

**What it proves:** the control lease works between two real devices, and the rule it enforces is
the one it claims to.

Open the same project's terminal in two clients — the desktop browser and the iPad is the honest
version. Type in one; the other should show the output and refuse the input.

What to check:

1. **One controller at a time.** The second client's input is refused, and the refusal names the
   device that holds the lease by its label — which the server derived from the User-Agent. You are
   told *who has it*, not *that you may not*.
2. **A refusal is not a fault.** The refused client's terminal is still there and still being drawn.
   Nothing disconnects, nothing goes blank.
3. **Taking it over is a request, not a takeover.** Clicking to take control on the second device
   does not preempt the first; the first has to give it up, or its connection has to drop.
4. **The lease is in memory only.** There is no table, no row and no record of what was typed, and
   the log lines carry a project id and a client id and never a keystroke. `docs/TERMINAL_CONTROLLER.md`
   §5 is the reasoning.

The lease has a grace period after a connection drops, which is deliberate: the usual reason a
controller's socket closes is a network that will come back, and releasing on the disconnect would
hand the terminal to whoever asks first during the gap. So a device that has genuinely gone away
holds the keyboard for about half a minute before it is free. If you are testing the takeover path,
close the first client's tab and wait.

---

## 9. Scenario 5 — restarting the server

**What it proves:** the recovery policy, which is the property that decides whether a beta can be
restarted without losing everyone's work.

**There is a script for this**, and it is the executable form of the section below:

```bash
sudo deploy/linux/recovery-test.sh
```

It registers one project of its own, stops and starts the real unit, looks at real tmux sockets, and
prints PASS/FAIL for each case. It never touches a tmux socket it did not create, never uses the
default socket, and never touches a project other than the one it registers. Run it before reading
the rest of this section; if it is green, the manual version will be too.

The three cases, by hand:

### Case A — the service restarted, and tmux is still there

```bash
sudo systemctl restart agentmux
curl -s http://127.0.0.1:8787/api/projects | grep -o '"state":"[A-Z]*"' | head -3
```

BetaCheck is `RUNNING` again, and **the session it was running is the same session** — the shell
history, the working directory and anything running inside it survived. That is the point of
`KillMode=process` in the unit, and it is the one line whose absence would turn a restart into a
massacre: with systemd's default, the stop signal goes to every process in the unit's cgroup, and a
project's tmux server is one of those.

The same is true of `sudo systemctl stop agentmux` followed by `start`. **Stopping the service does
not stop your terminals**, which is surprising the first time and is the design.

If the network is back before the server is, the browser reconnects on its own and asks for what it
missed.

### Case B — the machine rebooted, so tmux is gone

```bash
sudo reboot
# wait, reconnect
curl -s http://127.0.0.1:8787/api/projects | grep -o '"state":"[A-Z]*"' | head -3
```

BetaCheck is `STOPPED`, and **nothing was started automatically**. This is the recovery policy and
it is deliberate rather than incomplete: a server restart is not a request to resume work, and a
beta where rebooting the host silently relaunches every agent would be a beta nobody could safely
reboot. Starting it again is one click, or:

```bash
curl -s -X POST "http://127.0.0.1:8787/api/projects/$PROJECT_ID/runtime/start"
```

### Case C — a tmux session that belongs to no project

An *orphan* is a session in AgentMux's namespace with no project row behind it — what a deleted
project's still-running session looks like. The server reports it and **leaves it running**, because
it may hold work somebody needs and killing it would be a guess. Look for it in the startup log:

```bash
journalctl -u agentmux -n 100 | grep -i orphan
```

`deploy/linux/recovery-test.sh` covers this case by creating one. If you create one by hand, use a
socket path under `/var/lib/agentmux/tmux/` and a session name starting `amx-`, and remove it
afterwards by name — never with `kill-server` on the default socket.

---

## 10. What the beta records, and how to read it

**Off by default.** Turning it on is one line in `/etc/agentmux/agentmux.yaml`:

```yaml
beta:
  enabled: true
```

then `sudo systemctl restart agentmux`. The same thing from the command line is `-beta`, or
`AGENTMUX_BETA=1` in the environment.

It records five events, and they are the whole vocabulary:

| Event | Recorded when |
| --- | --- |
| `dashboard.open` | a browser asked for the console page |
| `terminal.connect` | a browser subscribed to a project's terminal |
| `controller.request` | a client asked for a project's control lease |
| `controller.release` | a client gave one up |
| `action.view` | a browser asked for one action's page |

**What it is not, and cannot become.** A row is an event type and a timestamp:

```sql
CREATE TABLE usage_events (id TEXT PRIMARY KEY, event_type TEXT NOT NULL, created_at TEXT NOT NULL)
```

There is no path, no query string, no project, no session, no device and no client identifier, so no
query against this table can answer "who did what" — not even by accident. In particular **no
terminal output, no input, no prompt and no Claude output**, and there is no column one could go in.
The guarantee is the schema rather than a filter: the way to keep that promise is a table with
nowhere to put those things, not a cleaner looking for four kinds of string.

There is also no deduplication and no session, deliberately. Opening the console three times is
three rows and there is no way to tell whether that was one person or three. **The number counts
occurrences, not people**, and an identifier that could answer "how many people" is an identifier —
which is why this is a table an operator can turn on without having to think about it for long.

### Reading the events

There is **no HTTP endpoint** that returns them. Query the database:

```bash
sudo -u agentmux sqlite3 /var/lib/agentmux/agentmux.db \
  "SELECT event_type, COUNT(*) FROM usage_events GROUP BY event_type ORDER BY 2 DESC;"
```

```
dashboard.open|14
terminal.connect|9
controller.request|6
controller.release|5
action.view|2
```

The last week of a beta, by day:

```bash
sudo -u agentmux sqlite3 /var/lib/agentmux/agentmux.db \
  "SELECT substr(created_at,1,10) AS day, COUNT(*) FROM usage_events GROUP BY day ORDER BY day;"
```

If `sqlite3` is not installed, `apt install sqlite3` — or read the same rows through any SQLite
client, including one on a laptop against a copy of the file. The database is a single ordinary
file and copying it while the server runs is safe.

**The rows are deletable without loss.** Nothing derives anything from this table and nothing reads
it to decide anything, so a beta that ends by truncating it has lost a count and nothing else.

---

## 11. The diagnostic endpoint

`GET /api/debug/runtime` exists **only when `server.debug` is on**. It is what to look at when a
runtime will not start and the question is which piece is missing:

```bash
# with server.debug: true in the config, and a restart
curl -s http://127.0.0.1:8787/api/debug/runtime; echo
```

```json
{"runtimeCount":1,"tmuxAvailable":true,"activeSessions":1,"websocketConnections":0,"subscriptions":0}
```

Four counts and a boolean. On an ordinary installation the path is **not routed at all** and answers
`404 not_found` like any path that does not exist — which is worth checking once, because it is the
property that makes the endpoint acceptable:

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8787/api/debug/runtime
404
```

It carries no project name, no identifier, no path, no prompt and no byte of terminal output, and
that is the whole set of fields it has rather than a filter over a richer one. A test asserts the
encoded key set exactly and greps the bytes for the forbidden words, so a field added later fails
the build.

**Turn debug off when you are done.** With it on, `GET /api/server` also publishes the four paths to
the server's own files, which is what makes it useful for a bug report and is not what you want
standing on a port you have exposed. `docs/SECURITY.md` §7 is the full statement.

---

## 12. What to report

A beta report that is useful says what you did, what you expected, and what happened. For anything
that fails, these four commands carry most of the answer:

```bash
systemctl status agentmux
journalctl -u agentmux -n 200 --no-pager
curl -s http://127.0.0.1:8787/api/server
curl -s http://127.0.0.1:8787/api/health
```

**Before you send any of it, read it.** The journal and `/api/server` are written not to contain a
credential, a prompt or anything you typed — §10 above and `docs/SECURITY.md` §6 are where that is
promised and how it is kept — but a screenshot of a terminal window shows the terminal, and that is
yours to check.

That is also why the four commands above are the ones named: they are the whole diagnostic surface,
and none of them shows the screen.

---

## 13. What this beta is not

Stated plainly, because a beta's scope is the thing most likely to be misread:

- **No authentication, and no multi-user support.** One server, one account, one set of projects.
  Who can reach the port is the whole of the access control.
- **No CC Switch control and no model switching.** The console shows the provider block; it does not
  change it.
- **No notifications.** Nothing is pushed anywhere. An agent that needs you is visible only when you
  look at the console.
- **No mobile app.** The iPad works because the console is a web page, and that is the whole of the
  mobile story.
- **No automatic resumption after a reboot.** See Case B above; it is a decision, not a gap.

If a scenario needs one of those to pass, it is not a scenario in this document.
