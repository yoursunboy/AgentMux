#!/usr/bin/env bash
#
# Install AgentMux on a Linux server, as a systemd service.
#
#     sudo deploy/linux/install.sh --root /srv/projects
#
# It builds (or takes) the server and the web bundle, installs them under
# /opt/agentmux, writes a configuration file if there is not one already,
# installs the systemd unit, and verifies the result with a health check.
#
# Running it a second time is the upgrade procedure. It stops the service,
# copies the database aside, replaces the binary and the bundle, starts the
# service again and verifies it — the sequence in docs/DEPLOYMENT.md §Upgrade,
# performed rather than described.
#
# It is deliberately a script a person reads before running. It prints every
# change it makes, it never deletes a data directory, and --uninstall leaves
# your projects, your database and your configuration exactly where they are.
#
# Platform: Linux with systemd. On Windows the server belongs inside WSL2 with
# systemd enabled; this script is the same one you run there, from inside the
# distribution. There is no Windows-native path and there is not going to be
# one — see deploy/linux/README.md.

set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------

PREFIX=/opt/agentmux
DATA_DIR=/var/lib/agentmux
CONFIG_DIR=/etc/agentmux
SERVICE_USER=agentmux
SERVICE_GROUP=agentmux
PORT=8787
PROJECTS_ROOT=/srv/projects
UNIT_NAME=agentmux

SOURCE_DIR=""
BINARY=""
WEB_BUNDLE=""
WITH_DEPS=0
DO_START=1
UNINSTALL=0

# ---------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------

if [ -t 1 ]; then
  BOLD=$(printf '\033[1m'); DIM=$(printf '\033[2m'); RED=$(printf '\033[31m')
  YELLOW=$(printf '\033[33m'); RESET=$(printf '\033[0m')
else
  BOLD=""; DIM=""; RED=""; YELLOW=""; RESET=""
fi

step() { printf '\n%s==>%s %s\n' "$BOLD" "$RESET" "$1"; }
info() { printf '    %s\n' "$1"; }
note() { printf '    %s%s%s\n' "$DIM" "$1" "$RESET"; }
warn() { printf '    %s%s%s\n' "$YELLOW" "$1" "$RESET" >&2; }
die()  { printf '\n%sError:%s %s\n' "$RED" "$RESET" "$1" >&2; exit 1; }

usage() {
  cat <<'EOF'
Install AgentMux as a systemd service.

  install.sh [options]

Where things go:
  --prefix DIR     the server binary and the web bundle   (default /opt/agentmux)
  --data DIR       AgentMux's own state: database, sockets (default /var/lib/agentmux)
  --config DIR     the configuration file's directory      (default /etc/agentmux)
  --user NAME      the system account the service runs as   (default agentmux)

What gets configured, on a first install only:
  --port N         the HTTP port written into a new config file      (default 8787)
  --root PATH      the Projects Root written into a new config file  (default /srv/projects)

What gets installed:
  --from DIR       a checkout to build from (default: the one this script is in)
  --binary PATH    an already-built agentmux-server binary; skips the Go build
  --web PATH       an already-built web bundle directory (the one holding index.html)

Behaviour:
  --with-deps      install tmux with apt if it is missing. It installs tmux and
                   nothing else: the build tools are not needed, because a
                   machine without a Go or Node toolchain is served by --binary
                   and --web, which take an already-built server and bundle.
  --no-start       install and enable the unit, but do not start it
  --uninstall      stop and remove the unit; keeps the data, config and binary
  -h, --help       this text

Re-running without --uninstall upgrades in place: the service is stopped, the
database is copied aside, the binary and bundle are replaced, the service is
started again, and /health is checked.
EOF
}

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------

while [ $# -gt 0 ]; do
  case "$1" in
    --prefix)     PREFIX=${2:?--prefix needs a directory}; shift 2 ;;
    --data)       DATA_DIR=${2:?--data needs a directory}; shift 2 ;;
    --config)     CONFIG_DIR=${2:?--config needs a directory}; shift 2 ;;
    --user)       SERVICE_USER=${2:?--user needs a name}; SERVICE_GROUP=$SERVICE_USER; shift 2 ;;
    --port)       PORT=${2:?--port needs a number}; shift 2 ;;
    --root)       PROJECTS_ROOT=${2:?--root needs a directory}; shift 2 ;;
    --from)       SOURCE_DIR=${2:?--from needs a directory}; shift 2 ;;
    --binary)     BINARY=${2:?--binary needs a file}; shift 2 ;;
    --web)        WEB_BUNDLE=${2:?--web needs a directory}; shift 2 ;;
    --with-deps)  WITH_DEPS=1; shift ;;
    --no-start)   DO_START=0; shift ;;
    --uninstall)  UNINSTALL=1; shift ;;
    -h|--help)    usage; exit 0 ;;
    *)            die "unknown argument: $1 (try --help)" ;;
  esac
done

for value in "$PREFIX" "$DATA_DIR" "$CONFIG_DIR"; do
  case "$value" in
    /*) ;;
    *) die "$value must be an absolute path" ;;
  esac
done

HEALTH_URL="http://127.0.0.1:${PORT}/health"
API_HEALTH_URL="http://127.0.0.1:${PORT}/api/health"

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------

require_root() {
  [ "$(id -u)" -eq 0 ] || die "this script makes system-level changes; run it with sudo"
}

require_systemd() {
  command -v systemctl >/dev/null 2>&1 ||
    die "systemctl not found. AgentMux's unit file is a systemd unit; on a machine without systemd, run the server under whatever supervisor you do have — see docs/DEPLOYMENT.md §Without systemd."
  [ -d /run/systemd/system ] ||
    die "systemd is not running as this machine's init (no /run/systemd/system).

  Inside WSL2 this means systemd is off for the distribution. Turn it on in
  Windows, in %USERPROFILE%\\.wslconfig:

      [boot]
      systemd=true

  then 'wsl --shutdown' and start the distribution again. See deploy/linux/README.md §WSL2."
  info "systemd $(systemctl --version | head -1 | awk '{print $2}') is running"
}

# have reports whether a command is on PATH.
have() { command -v "$1" >/dev/null 2>&1; }

# apt_install installs packages, and is only ever called when --with-deps was
# given. A deployment script that silently runs apt on somebody's server is a
# script that surprises them, which is why installing anything is opt-in.
apt_install() {
  have apt-get || die "cannot install $* automatically: apt-get is not on this machine. Install it, then run this script again."
  info "installing: $*"
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "$@" >/dev/null
}

# ---------------------------------------------------------------------------
# Where the artifacts come from
# ---------------------------------------------------------------------------

# HERE is deploy/linux, so the checkout is two levels up.
HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
CHECKOUT=$(cd -- "$HERE/../.." && pwd)

build_server() {
  local out=$1
  have go || die "the Go toolchain is not on PATH. Either install it, or build the server elsewhere and pass --binary."
  [ -f "$CHECKOUT/go.mod" ] || die "--from/--binary: $CHECKOUT does not look like an AgentMux checkout (no go.mod)"

  local commit build_date ldflags
  commit=$(git -C "$CHECKOUT" rev-parse HEAD 2>/dev/null || true)
  build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  ldflags="-X github.com/kutonlagos/agentmux/internal/version.Commit=${commit}"
  ldflags="$ldflags -X github.com/kutonlagos/agentmux/internal/version.BuildDate=${build_date}"

  info "building the server${commit:+ (${commit:0:12})}"
  ( cd "$CHECKOUT" && go build -trimpath -ldflags "$ldflags" -o "$out" ./cmd/server )
}

build_web() {
  local out=$1
  have npm || die "npm is not on PATH. Either install Node, or build the frontend elsewhere and pass --web."

  info "building the frontend"
  ( cd "$CHECKOUT/web" && npm ci --silent && npm run build --silent )

  [ -d "$CHECKOUT/web/dist" ] || die "the frontend build produced no web/dist"
  cp -a "$CHECKOUT/web/dist" "$out"
}

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------

uninstall() {
  step "Removing the AgentMux service"

  if systemctl list-unit-files "${UNIT_NAME}.service" >/dev/null 2>&1 &&
     [ -f "/etc/systemd/system/${UNIT_NAME}.service" ]; then
    systemctl stop "$UNIT_NAME" 2>/dev/null || true
    systemctl disable "$UNIT_NAME" 2>/dev/null || true
    rm -f "/etc/systemd/system/${UNIT_NAME}.service"
    systemctl daemon-reload
    info "removed /etc/systemd/system/${UNIT_NAME}.service"
  else
    info "no unit is installed"
  fi

  # Sessions are not the service's to end. Stopping the unit detaches the
  # server; the tmux servers it started keep running, with whatever is in them,
  # exactly as they would if the server had been stopped for an upgrade. That is
  # the same property that makes a restart safe, seen from the other side.
  local sockets="$DATA_DIR/tmux"
  if [ -d "$sockets" ] && [ -n "$(ls -A "$sockets" 2>/dev/null || true)" ]; then
    warn "tmux sessions from this installation are still running."
    note "Sockets: $sockets"
    note "They hold whatever was in those terminals. End them with, for each socket:"
    note "  tmux -S <socket> kill-server"
  fi

  step "Kept"
  info "data      $DATA_DIR"
  info "config    $CONFIG_DIR"
  info "binary    $PREFIX"
  note "Nothing was deleted. Remove those three directories by hand if you mean to."
}

# ---------------------------------------------------------------------------
# The install itself
# ---------------------------------------------------------------------------

install_agentmux() {
  # --- prerequisites ------------------------------------------------------

  step "Checking prerequisites"

  if have tmux; then
    info "tmux $(tmux -V | awk '{print $2}') at $(command -v tmux)"
  elif [ "$WITH_DEPS" -eq 1 ]; then
    apt_install tmux
  else
    die "tmux is not installed, and AgentMux's runtime is tmux.

  Install it with:  sudo apt-get install -y tmux

  or run this script with --with-deps to have it do that. It is not done for
  you by default, because a deployment script that runs apt without being asked
  is a deployment script that changes a machine you did not hand it."
  fi

  # The browser suite and the runtime both need the CLI, but a server without
  # it is a working server: it manages projects and hosts terminals, and simply
  # has no agent to put in one. So this is a warning, not a failure.
  if have claude; then
    info "Claude Code CLI at $(command -v claude)"
  else
    warn "the 'claude' CLI is not on this account's PATH."
    note "AgentMux will run and manage projects, and its terminals will have no agent."
    note "It is resolved for the '${SERVICE_USER}' account, so install it there, not in yours."
  fi

  # --- artifacts ----------------------------------------------------------

  step "Preparing the build"

  local staging
  staging=$(mktemp -d)

  # The trap is placed here rather than at the top of the script for a reason
  # that cost an install to find: an EXIT trap set before this function was
  # called runs the moment the function's frame ends, because bash fires EXIT
  # traps when a function returns, not only when the script does. At that point
  # `staging` is a local that no longer exists, and under `set -u` the cleanup
  # itself fails with "staging: unbound variable" — turning a successful install
  # into exit code 1. Setting it inside the frame, after the variable it names,
  # keeps the two in the same scope.
  trap 'rm -rf "${staging:-}"' EXIT

  if [ -n "$BINARY" ]; then
    [ -f "$BINARY" ] || die "--binary $BINARY does not exist"
    cp -a "$BINARY" "$staging/agentmux-server"
    info "using the server binary at $BINARY"
  else
    SOURCE_DIR=${SOURCE_DIR:-$CHECKOUT}
    CHECKOUT=$SOURCE_DIR
    build_server "$staging/agentmux-server"
  fi
  chmod 0755 "$staging/agentmux-server"

  if [ -n "$WEB_BUNDLE" ]; then
    [ -f "$WEB_BUNDLE/index.html" ] || die "--web $WEB_BUNDLE has no index.html; it should be the directory the frontend built into"
    cp -a "$WEB_BUNDLE" "$staging/web"
    info "using the web bundle at $WEB_BUNDLE"
  else
    build_web "$staging/web"
  fi

  # --- the service account ------------------------------------------------

  step "Service account"

  if id -u "$SERVICE_USER" >/dev/null 2>&1; then
    info "the account ${SERVICE_USER} already exists"
  else
    # A login shell, because a terminal is what this account is for: tmux runs
    # one in every session, and an account with /usr/sbin/nologin has no
    # terminal to host.
    #
    # No sudo, no supplementary groups: the service runs unprivileged, which is
    # the first line of docs/SECURITY.md §2.
    useradd --system --create-home --shell /bin/bash "$SERVICE_USER"
    info "created the account ${SERVICE_USER}"
  fi
  SERVICE_GROUP=$(id -gn "$SERVICE_USER")

  # --- directories --------------------------------------------------------

  step "Directories"

  install -d -m 0755 -o root        -g root          "$PREFIX"
  install -d -m 0755 -o root        -g root          "$CONFIG_DIR"
  install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$DATA_DIR"

  info "prefix    $PREFIX"
  info "config    $CONFIG_DIR"
  info "data      $DATA_DIR  ${DIM}(0750 ${SERVICE_USER}:${SERVICE_GROUP})${RESET}"

  # --- an upgrade is a stop, a backup, a replace, a start -----------------

  # Whether this is an upgrade is a question about the machine, not about
  # whether the service happens to be up. This asked `systemctl is-active`,
  # which reads a server that is down - crashed, stopped by hand, or not yet
  # started since a reboot - as a fresh install: the copy below was skipped
  # without a word and the binary was replaced underneath a database written to
  # since the last upgrade. The case where a copy matters most was the one case
  # that did not take one.
  local upgrading=0
  if [ -f "$PREFIX/agentmux-server" ] || [ -f "/etc/systemd/system/${UNIT_NAME}.service" ]; then
    upgrading=1
  fi

  if [ "$upgrading" -eq 1 ]; then
    step "Stopping the service"
    systemctl stop "$UNIT_NAME" 2>/dev/null || true
    info "stopped"

    # A cold copy, which is what makes it a valid one: the service is down, so
    # nothing is writing. What makes it a *complete* one is that the three
    # files are copied as a set. In WAL mode a commit appends to the `-wal` and
    # the database file is only brought up to date at a checkpoint, so a `.db`
    # copied without the `-wal` beside it is consistent as of the last
    # checkpoint and quietly missing every transaction after it - the failure
    # docs/BACKUP.md §3.3 exists to describe. A clean stop checkpoints and
    # removes the `-wal`, so in the ordinary case the last two do not exist and
    # this is a one-file copy; they are copied when they are there, which is
    # the crash and the hard stop, and that is exactly when they hold something.
    # docs/BACKUP.md §3 has the hot-copy procedure for a backup taken without
    # stopping anything.
    local db="$DATA_DIR/agentmux.db"
    if [ -f "$db" ]; then
      local stamp backup suffix copied=0
      stamp=$(date -u +%Y%m%dT%H%M%SZ)
      backup="${db}.pre-upgrade-${stamp}"
      for suffix in "" "-wal" "-shm"; do
        [ -f "${db}${suffix}" ] || continue
        cp -a "${db}${suffix}" "${backup}${suffix}"
        copied=$((copied + 1))
      done
      info "database copied to $(basename "$backup")  ${DIM}(${copied} file(s))${RESET}"
      note "$backup"
      [ "$copied" -gt 1 ] && note "the -wal is part of the copy: without it the database is short its last commits"
    else
      info "no database yet; nothing to copy"
    fi
  fi

  # --- files --------------------------------------------------------------

  step "Installing files"

  # The binary is replaced by rename rather than written over, so that a server
  # still running from the old inode (a straggler the stop did not catch) can
  # finish rather than reading a half-written file.
  install -m 0755 "$staging/agentmux-server" "$PREFIX/agentmux-server.new"
  mv -f "$PREFIX/agentmux-server.new" "$PREFIX/agentmux-server"
  info "$PREFIX/agentmux-server"

  rm -rf "$PREFIX/web"
  mkdir -p "$PREFIX/web"
  cp -a "$staging/web" "$PREFIX/web/dist"
  info "$PREFIX/web/dist"

  # --- configuration ------------------------------------------------------

  local config_file="$CONFIG_DIR/agentmux.yaml"
  if [ -f "$config_file" ]; then
    info "keeping the existing $config_file"
  else
    step "Writing $config_file"

    cat > "$config_file" <<EOF
# AgentMux configuration. Written by deploy/linux/install.sh; every key and its
# default are documented in config/agentmux.example.yaml in the repository.
#
# Read by the service through AGENTMUX_CONFIG in /etc/systemd/system/agentmux.service.
# That unit also sets AGENTMUX_DATA_DIR, and the environment wins over this file
# — so a dataDir key here would be ignored. See docs/DEPLOYMENT.md §Configuration.

server:
  # Loopback only. AgentMux has no authentication: who can reach this port is
  # the whole of its access control. See docs/SECURITY.md §1 before changing it.
  host: 127.0.0.1
  port: ${PORT}

logging:
  level: info
  format: text

projects:
  roots:
    - ${PROJECTS_ROOT}

# Beta usage metrics. Off by default and left off here: the installer writes a
# working deployment, not one that collects. Turning it on records five events —
# a page opened, a terminal connected, a keyboard taken, a keyboard given back,
# one action read — as a row of {event type, timestamp} and nothing else. No
# terminal output, no input, no prompt, no project or session name, and no
# identifier that could tell two people apart. See docs/BETA_TEST.md.
beta:
  enabled: false
EOF
    chmod 0640 "$config_file"
    chown root:"$SERVICE_GROUP" "$config_file"
    info "wrote $config_file"
    note "port ${PORT}, Projects Root ${PROJECTS_ROOT}"
    if [ ! -d "$PROJECTS_ROOT" ]; then
      warn "${PROJECTS_ROOT} does not exist."
      note "That is a warning rather than a failure — a root can be mounted later —"
      note "but discovery will find nothing until it does. Create it with:"
      note "  sudo mkdir -p ${PROJECTS_ROOT} && sudo chown ${SERVICE_USER}:${SERVICE_GROUP} ${PROJECTS_ROOT}"
    fi
  fi

  # The config directory is root-owned and the file is group-readable, so the
  # service can read its configuration and cannot rewrite it. That is on
  # purpose: a setting that changes itself is a setting nobody can reason about.
  chown root:"$SERVICE_GROUP" "$CONFIG_DIR"
  chmod 0750 "$CONFIG_DIR"

  # --- the unit -----------------------------------------------------------

  step "Installing the systemd unit"

  local unit_src="$HERE/${UNIT_NAME}.service"
  [ -f "$unit_src" ] || die "cannot find ${UNIT_NAME}.service beside this script"
  local unit_dst="/etc/systemd/system/${UNIT_NAME}.service"

  # The shipped unit has the default paths written into it, comments included,
  # and they are substituted rather than templated so that the file on disk is
  # readable on its own — an operator debugging a unit should not have to find
  # the script that generated it.
  sed -e "s#/opt/agentmux#${PREFIX}#g" \
      -e "s#/etc/agentmux#${CONFIG_DIR}#g" \
      -e "s#/var/lib/agentmux#${DATA_DIR}#g" \
      -e "s#^User=.*#User=${SERVICE_USER}#" \
      -e "s#^Group=.*#Group=${SERVICE_GROUP}#" \
      "$unit_src" > "$unit_dst"
  chmod 0644 "$unit_dst"
  info "$unit_dst"

  systemctl daemon-reload
  systemctl enable "$UNIT_NAME" >/dev/null 2>&1
  info "enabled at boot"

  # --- start and verify ---------------------------------------------------

  if [ "$DO_START" -eq 1 ]; then
    step "Starting"
    systemctl restart "$UNIT_NAME"
    verify_health || exit 1
  else
    step "Not started"
    info "--no-start was given. Start it with: sudo systemctl start ${UNIT_NAME}"
  fi

  # --- what to do next ----------------------------------------------------

  step "$([ "$upgrading" -eq 1 ] && echo 'Upgraded' || echo 'Installed')"

  printf '    %s\n' "status    systemctl status ${UNIT_NAME}"
  printf '    %s\n' "logs      journalctl -u ${UNIT_NAME} -f"
  printf '    %s\n' "health    curl -s ${HEALTH_URL}"
  printf '    %s\n' "config    ${config_file}"

  if [ "$upgrading" -eq 1 ]; then
    printf '\n    Runtimes were left as they were. A project whose tmux session\n'
    printf '    survived the restart is running again; one whose session did not\n'
    printf '    is reported stopped, and starting it is yours to ask for.\n'
  fi
}

# verify_health waits for /health and prints what it said.
#
# It retries rather than asking once, because the server has a database to open
# and a host to probe before it answers, and it reports the unit's own state
# when it gives up — a health check that failed and a service that failed to
# start are the same thing to a reader and different things to a fixer.
#
# It asks /api/health as well, once the first has answered. The two routes are
# one expression in the server and both are expected to be there, so an install
# where one is missing is an install whose binary is not the one this script
# documents — which is worth catching here, at the terminal where somebody is
# still watching, rather than from a client that quietly reports the deployment
# as down.
verify_health() {
  step "Verifying"

  local attempt body
  for attempt in $(seq 1 30); do
    body=$(fetch_health || true)
    if [ -n "$body" ]; then
      info "$HEALTH_URL answered:"
      printf '    %s%s%s\n' "$BOLD" "$body" "$RESET"

      # Readiness is separate from liveness, and worth saying out loud: a
      # service that is up on a host with no tmux is a working AgentMux that
      # cannot host a terminal, and that is exactly the state a person
      # installing it is most likely to be in.
      case "$body" in
        *'"runtime":"available"'*)
          info "a terminal runtime can run here" ;;
        *)
          warn "the server is up but cannot host a terminal here."
          note "Run 'journalctl -u ${UNIT_NAME} -n 30' — the reason is in the startup lines." ;;
      esac

      # The API's own health route. It is not retried: the server that answered
      # the line above is the server that serves this one.
      if [ -n "$(fetch_health "$API_HEALTH_URL" /api/health || true)" ]; then
        info "$API_HEALTH_URL answered"
      else
        warn "${API_HEALTH_URL} did not answer."
        note "The server is running, so this build predates that route. Clients that"
        note "check it will report the deployment as down; upgrade the binary."
      fi
      return 0
    fi
    sleep 1
  done

  warn "the service did not answer ${HEALTH_URL} within 30 seconds."
  printf '\n' >&2
  systemctl --no-pager --full status "$UNIT_NAME" >&2 || true
  printf '\n' >&2
  journalctl -u "$UNIT_NAME" -n 30 --no-pager >&2 || true
  return 1
}

# fetch_health asks a health endpoint, using whatever HTTP client is here.
#
# The URL defaults to the liveness route and the path to /health, so the
# ordinary call is argument-free; the caller that needs the API's route passes
# both, because the /dev/tcp fallback below has to write the path into a
# request by hand and cannot take it apart from the URL.
#
# curl and wget are both common and neither is guaranteed on a minimal server,
# so the fallback is bash's own /dev/tcp rather than a dependency.
fetch_health() {
  local url=${1:-$HEALTH_URL} path=${2:-/health}

  # stderr is discarded rather than passed through. This is called in a retry
  # loop while the service is binding its port, so the first few attempts
  # failing is the normal case - and curl's own `-S` prints "Failed to connect"
  # for each one, which reads as an install that went wrong directly above the
  # line saying it went right. Nothing is lost by dropping it: the caller treats
  # an empty answer as "not up yet", and if it never comes up the failure path
  # prints the unit status and the journal, which say more than curl does.
  if have curl; then
    curl -fsS --max-time 5 "$url" 2>/dev/null
  elif have wget; then
    wget -qO- --timeout=5 "$url" 2>/dev/null
  else
    exec 3<>"/dev/tcp/127.0.0.1/${PORT}" || return 1
    printf 'GET %s HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n' "$path" >&3
    local line
    while IFS= read -r line <&3; do
      case "$line" in
        '{'*) printf '%s' "$line" ;;
      esac
    done
    exec 3<&- 3>&-
  fi
}

# ---------------------------------------------------------------------------
# Go
# ---------------------------------------------------------------------------

require_root

if [ "$UNINSTALL" -eq 1 ]; then
  uninstall
  exit 0
fi

require_systemd
install_agentmux
