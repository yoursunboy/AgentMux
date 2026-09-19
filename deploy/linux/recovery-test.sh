#!/usr/bin/env bash
#
# §十二 runtime recovery — the three cases, run against a real installation.
#
#     sudo deploy/linux/recovery-test.sh
#
# This is the executable form of §Recovery in deploy/linux/README.md: it stops
# and starts the real systemd unit, looks at real tmux sockets, and prints what
# each case actually did rather than what it is supposed to do. It is a test for
# a machine where you have just installed AgentMux, and it leaves the
# installation as it found it except for one project it registers.
#
# Case A  service restart, tmux alive   -> runtime running again, adopted
# Case B  tmux gone (what a reboot is)  -> runtime reported stopped
# Case C  a tmux socket with no project -> ORPHAN, reported and left running
#
# It never touches a tmux socket it did not create, never uses the default tmux
# socket, and never touches a project other than the one it registered.

set -euo pipefail

PORT=${PORT:-8787}
UNIT=${UNIT:-agentmux}
DATA_DIR=${DATA_DIR:-/var/lib/agentmux}
BASE="http://127.0.0.1:${PORT}"

PASS=0
FAIL=0

step() { printf '\n==> %s\n' "$1"; }
ok()   { PASS=$((PASS + 1)); printf '    \033[32mPASS\033[0m %s\n' "$1"; }
bad()  { FAIL=$((FAIL + 1)); printf '    \033[31mFAIL\033[0m %s\n' "$1"; }

check() { # check <condition-description> <actual> <expected>
  if [ "$2" = "$3" ]; then ok "$1: $2"; else bad "$1: got '$2', want '$3'"; fi
}

api() { curl -fsS --max-time 10 "$@"; }

# json_string pulls a named string field out of a response without needing jq,
# which is not installed everywhere. It is deliberately literal about the
# `"name":"value"` shape rather than a general parser: it is used here on
# responses produced by this server, for fields this server emits as strings.
#
# It always succeeds, including when the field is absent - it is called inside
# command substitutions under `set -e`, where a grep that found nothing would
# otherwise end the script silently.
json_string() { # json_string <field> < json
  grep -o "\"$1\":\"[^\"]*\"" | head -1 | cut -d'"' -f4 || true
}

[ "$(id -u)" -eq 0 ] || { echo "run this with sudo"; exit 1; }
command -v curl >/dev/null || { echo "curl is required"; exit 1; }

api "$BASE/health" >/dev/null || { echo "AgentMux is not answering $BASE/health"; exit 1; }

SERVICE_GROUP=$(id -gn agentmux)

# ---------------------------------------------------------------------------
step "Registering a project to test with"

PROJECT_DIR=/srv/projects/recovery-test
mkdir -p "$PROJECT_DIR"
chown -R agentmux:"$SERVICE_GROUP" "$PROJECT_DIR"

# Registering is idempotent from this script's point of view: a second run finds
# the project already there, which the endpoint reports as 409 with the existing
# project in the body. Both answers name the same project, so both are read the
# same way rather than treating the re-run as an error.
PROJECT=$(curl -sS --max-time 10 -X POST "$BASE/api/projects/register" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"recovery-test\",\"hostPath\":\"$PROJECT_DIR\"}" 2>/dev/null || true)
PROJECT_ID=$(printf '%s' "$PROJECT" | json_string id)

if [ -z "$PROJECT_ID" ]; then
  PROJECT_ID=$(api "$BASE/api/projects" | tr '{' '\n' | grep -F "$PROJECT_DIR" | json_string id)
fi
[ -n "$PROJECT_ID" ] || { echo "could not register or find the test project"; exit 1; }
echo "    project $PROJECT_ID at $PROJECT_DIR"

SOCKET="$DATA_DIR/tmux/${PROJECT_ID}.sock"

runtime_state() { api "$BASE/api/projects/$PROJECT_ID/runtime" | json_string state; }

# ---------------------------------------------------------------------------
step "Case A — service restart, tmux alive"

api -X POST "$BASE/api/projects/$PROJECT_ID/runtime/start" >/dev/null
sleep 1
check "runtime state before the restart" "$(runtime_state)" "RUNNING"
[ -S "$SOCKET" ] && ok "tmux socket exists at $SOCKET" || bad "no tmux socket at $SOCKET"

# The socket is this project's own tmux server. Its name is recorded so that the
# same server can be identified after the restart: a config that restarted it
# would produce a different server with the same socket path.
SERVER_PID_BEFORE=$(tmux -S "$SOCKET" display -p '#{pid}' 2>/dev/null || echo "?")
echo "    tmux server pid before: $SERVER_PID_BEFORE"

systemctl restart "$UNIT"
sleep 2

check "runtime state after the restart" "$(runtime_state)" "RUNNING"
SERVER_PID_AFTER=$(tmux -S "$SOCKET" display -p '#{pid}' 2>/dev/null || echo "?")
echo "    tmux server pid after:  $SERVER_PID_AFTER"
check "the tmux server survived the restart" "$SERVER_PID_AFTER" "$SERVER_PID_BEFORE"

# ---------------------------------------------------------------------------
step "Case B — tmux gone, which is what a reboot leaves"

# A reboot ends every process; this is the same end, without rebooting the
# machine. The tmux server is killed directly, then the service is restarted
# the way it would be by Restart=always coming back after boot.
tmux -S "$SOCKET" kill-server 2>/dev/null || true
sleep 1

systemctl restart "$UNIT"
sleep 2

check "runtime state after tmux was lost" "$(runtime_state)" "STOPPED"
[ -S "$SOCKET" ] && bad "a socket is still present at $SOCKET" || ok "the socket is gone"

# Nothing was restarted on the server's own initiative: a runtime comes back
# when a person asks for it, and never because the server noticed it was gone.
api -X POST "$BASE/api/projects/$PROJECT_ID/runtime/start" >/dev/null
sleep 1
check "a runtime starts again on request" "$(runtime_state)" "RUNNING"

# ---------------------------------------------------------------------------
step "Case C — a tmux socket that belongs to no project"

# A socket this server would have made, for a project id it does not know: a
# database restored from a backup taken before the project existed, or a project
# deleted while its session ran. Two details matter.
#
# The name: the id is a well-formed project id - `p_` and twenty hex characters,
# which is what `project.ValidID` accepts - because that is the case being
# tested. The server reports every unclaimed socket it finds, whatever its
# filename looks like: `wellFormed` is carried into the log line and decides
# nothing. An earlier version of this test used a UUID and passed, which is how
# that was found out; the assertion below now reads the flag back rather than
# assuming which branch was taken.
#
# The owner: the socket is created as the service account, because that is who
# owns every socket in this directory in a real installation. A socket made by
# root is unreadable to the server, which is a different case with a different
# (and correct) answer - "could not be classified; left untouched".
ORPHAN_ID=p_000000000000000000ff
ORPHAN_SESSION="amx-${ORPHAN_ID}"
ORPHAN_SOCKET="$DATA_DIR/tmux/${ORPHAN_ID}.sock"

runuser -u agentmux -- tmux -S "$ORPHAN_SOCKET" new-session -d -s "$ORPHAN_SESSION" 'sleep 600'
ok "started an unregistered session $ORPHAN_SESSION, owned by agentmux"

systemctl restart "$UNIT"
sleep 2

if journalctl -u "$UNIT" --since "-3min" --no-pager | grep -q "belongs to no registered project"; then
  ok "the orphan was reported in the journal"
  journalctl -u "$UNIT" --since "-3min" --no-pager | grep "belongs to no registered project" | tail -1 | sed 's/^/      /'
else
  bad "no orphan warning in the journal"
fi

# The line carries the server's own classification of the filename, and reading
# it back is what makes this case a test of the classification rather than of
# the warning. `wellFormed=true` says the server agreed the socket looked like
# one of its own and reported it anyway, which is the whole point of case C.
if journalctl -u "$UNIT" --since "-3min" --no-pager | grep -q "wellFormed=true"; then
  ok "the server classified the socket as well-formed and still left it alone"
else
  bad "the journal does not show a well-formed classification for $ORPHAN_ID"
fi

if tmux -S "$ORPHAN_SOCKET" has-session -t "$ORPHAN_SESSION" 2>/dev/null; then
  ok "the orphan session was left running"
else
  bad "the orphan session was killed"
fi

tmux -S "$ORPHAN_SOCKET" kill-server 2>/dev/null || true
ok "cleaned up the orphan"

# ---------------------------------------------------------------------------
step "Result"

curl -s "$BASE/health"; echo
printf '\n    %d passed, %d failed\n' "$PASS" "$FAIL"

# The test project is left registered and running: it is evidence of what just
# happened, and removing it is a deliberate act rather than a side effect.
[ "$FAIL" -eq 0 ]
