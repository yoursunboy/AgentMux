/**
 * The end-to-end run: provision a workspace, serve it, drive it, tear it down.
 *
 * # Why a runner rather than four scripts
 *
 * Phase 4's browser suites each built their own fixture by hand, which meant
 * they could only run on the machine where that fixture already existed. That
 * is not reproducibility; it is a memory of one afternoon. This builds the
 * fixture it needs from nothing - seven projects, their runtimes, two of them
 * hosting a real Claude - so a run either works or says what is missing.
 *
 * # What it needs
 *
 *   - a WSL distribution with tmux and the Claude Code CLI in it;
 *   - Go on the machine running this (`go build` cross-compiles the server);
 *   - Node 22+;
 *   - Chrome, which Playwright drives as a channel rather than downloading.
 *
 * # Usage
 *
 *   npm run test:e2e                     # everything
 *   npm run test:e2e -- --suites=workspace
 *   npm run test:e2e -- --keep           # leave the server and fixtures up
 *   npm run test:e2e -- --no-build       # use the binary and bundle already there
 *
 * Nothing it creates is inside the repository: the run directory is temporary
 * and removed at the end. `--keep` is the exception, and it says where.
 *
 * # When it is interrupted
 *
 * Ctrl-C, a SIGTERM from a task manager, a hung suite killed by hand: each of
 * them arrives while fixtures exist, and the default answer to any of them is
 * to stop where it stands - which leaves seven tmux servers, a server process
 * and a temporary directory behind.
 *
 * The next run then starts on top of them, and does not see them. `provision`
 * removes the run directory, which takes the socket files with it and leaves
 * the servers running with nothing on disk to name them by, so they cannot be
 * found by looking where they were made. Measured in Phase 6.5: 107 such
 * servers had accumulated across a phase of interrupted runs, none of them
 * visible in the directory that was supposed to hold them.
 *
 * So the signals are handled. The runtimes go through the API; whatever the API
 * cannot reach is ended by socket and by process; what survives that is
 * reported at the end rather than deleted on a guess.
 */
import { spawn, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  DISTRO,
  PORT,
  REPO_DIR,
  RUN_DIR,
  SERVER_URL,
  SUITE_TIMEOUT_MS,
  startServer,
} from './lib/config.mjs'

/**
 * wsl runs a script inside the distribution.
 *
 * Spelled here rather than imported from the harness, because the harness reads
 * the fixture description at load time and this file runs before there is one.
 *
 * # `--exec`, and why every call to this needs it
 *
 * Without it, `wsl.exe` does not hand the script to the distribution as an
 * argument. It joins its own command line into one string and runs that through
 * the distribution's default shell, which expands it once on the way - so what
 * arrives is not what was sent. Measured, with `p=hello; echo "p=[$p]"`:
 * `$p` arrives empty, because the shell in between had no `p`; `$HOME` arrives
 * expanded to the distribution's home directory; `$$` arrives as that shell's
 * own pid; and quoting does not help, because the expansion happens before the
 * quotes are read. `--exec` execs the program directly, and the string arrives
 * as written.
 *
 * It is written down here because the failure is silent and reads as success.
 * `... has-session ...; echo $?` was in this repository in three places; the
 * `$?` was being expanded to the intermediate shell's status before bash ever
 * ran the command, so the answer was `0` every time, including for a session
 * that did not exist. Three checks passed on a value that never depended on
 * what they were checking.
 *
 * The same trap is why `copyIntoWSL` and the suites' typing helpers put their
 * payloads through stdin rather than in an argument.
 */
function wsl(script, input) {
  const result = spawnSync('wsl', ['-d', DISTRO, '--exec', 'bash', '-lc', script], {
    encoding: 'utf8',
    input,
    env: { ...process.env, MSYS_NO_PATHCONV: '1' },
    maxBuffer: 64 * 1024 * 1024,
  })
  return result.stdout ?? ''
}

const HERE = path.dirname(fileURLToPath(import.meta.url))
const REPO_WINDOWS = path.resolve(HERE, '..', '..')
const SERVER_BINARY = path.join(REPO_WINDOWS, 'bin', 'e2e', 'agentmux-server')

/**
 * How hard the fixture tries to get a Claude up, and how long each attempt is
 * given.
 *
 * Retried rather than typed once, because the failure this covers is a startup
 * one: when this host loses an instance it is within the first seconds, before
 * any screen is drawn, and the pane is left at the prompt it was started from -
 * which is a state another attempt can start from. Measured on this host, a
 * Claude alone in a session draws its theme picker inside twenty-five seconds
 * every time, so forty is generous per attempt and three attempts are still
 * less than the ninety a single attempt was given before.
 */
const CLAUDE_ATTEMPTS = 3
const CLAUDE_START_TIMEOUT_MS = 40_000

/** The socket of a fixture project's tmux session, before there is a fixture object. */
const sockOf = (entry) => `${RUN_DIR}/data/tmux/${entry.id}.sock`

/**
 * What the run has made and has not yet unmade.
 *
 * Kept here rather than on the fixture object because the moment it matters
 * most is the one where no fixture object exists: a signal during `provision`,
 * before the thing the suites are handed has been built. Projects are recorded
 * here as each one is registered rather than once the fixture is complete, so
 * an interrupted provisioning is left holding a list to clean up rather than
 * nothing to clean up.
 */
const state = {
  /** `{name, id, socket}` for every project this run has registered. */
  projects: [],
  /** The suite process, while one is running. */
  child: null,
  /**
   * The cleanup currently in flight, if any.
   *
   * A signal can arrive while a suite's own teardown is already running. A
   * second caller is handed this promise instead of starting a second cleanup,
   * which is what makes teardown re-entrant rather than merely repeatable.
   */
  cleaning: null,
  /** Cleanup finished and the run directory is gone, so the fallback must not fire. */
  cleaned: false,
  /** The run was asked to keep its last fixture; nothing may end that one. */
  keeping: false,
  /** A signal has been seen, so the suite loop must not start another fixture. */
  interrupted: false,
  /** How many signals have arrived. The second one stops waiting for the first. */
  signals: 0,
  /**
   * The runtimes the API could not destroy, across every teardown in the run.
   *
   * Kept after `teardown` has finished with them, because the decision they
   * drive - whether the synchronous fallback is needed - belongs to the end of
   * the run rather than to the teardown that recorded them.
   */
  failures: [],
  /** What the process should exit with. 130 for SIGINT, 143 for SIGTERM. */
  exitCode: 1,
  /** The survey has been printed, so a second caller does not print it again. */
  surveyed: false,
}

/** startClaude types the CLI into a project's session, at its shell prompt. */
function startClaude(entry) {
  const sock = sockOf(entry)
  wsl(`tmux -S ${sock} send-keys -t amx-${entry.id} -l 'claude'`)
  wsl(`tmux -S ${sock} send-keys -t amx-${entry.id} Enter`)
}

/**
 * claudeUp is the suites' own rule, spelled here for the same reason `wsl` is:
 * the harness cannot be imported before there is a fixture for it to read. A
 * session showing Claude has no shell prompt at the end of it - see `claudeIsUp`
 * in `lib/harness.mjs`, which is the one the suites use and the one this has to
 * agree with.
 */
function claudeUp(entry) {
  const text = wsl(`tmux -S ${sockOf(entry)} capture-pane -p -t amx-${entry.id}`)
  return !text.includes(`${entry.name}$`) && text.trim() !== ''
}

/** waitForClaude polls that rule, and says whether it held before the deadline. */
async function waitForClaude(entry, timeoutMs) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    if (claudeUp(entry)) return true
    if (Date.now() >= deadline) return false
    await new Promise((resolve) => setTimeout(resolve, 1000))
  }
}

/**
 * The order suites run in: cheapest and most fundamental first.
 *
 * `tasks` sits second because it is the cheapest of them and the least
 * entangled: it drives the task API and reads one runtime's state without ever
 * starting, stopping or typing into one, so nothing it does can depend on what
 * ran before it. It does leave rows behind, which is why it goes before the
 * suites that restart the server - a restart is worth surviving with real data
 * in the database.
 *
 * `dashboard-terminal` follows `dashboard` because it is the same page with a
 * terminal on it, and it is the more expensive of the two: it stops and starts a
 * runtime, reloads the page and drives a second console. Both are before
 * `controller`, which costs more than either.
 *
 * `controller-input` follows `dashboard-terminal` for the same reason one step
 * on - same page, one more thing done on it - and it sits before `controller`
 * rather than after it because of what it leaves behind. It types into the same
 * fixture project `controller` does, and it gives the keyboard back at the end
 * so that project is nobody's when it finishes. A lease whose browser has closed
 * is suspended for the grace period rather than released, so a suite that ended
 * holding one would hand `controller` a project it cannot take for eight
 * seconds. Ending with nothing held is not a courtesy; it is what makes the
 * order safe.
 *
 * `controller` is last because it is the most expensive of them: it drives two
 * browser contexts at once, waits out a control grace and restarts the server
 * underneath its own fixture. Nothing after it would benefit from that.
 *
 * `action-center` is appended after it, and is the most expensive thing any
 * suite here asks of the machine: it starts a runtime, makes a terminal busy,
 * launches an agent into it, and then - when this host has room - starts a real
 * Claude on top of the two the fixture already runs. It goes last for that
 * reason and one more: half of it is gated on that Claude coming up, so a run
 * that cannot finish it has already finished everything else.
 */
const ALL_SUITES = [
  'transport',
  'tasks',
  'terminal',
  'recovery',
  'tablet',
  'workspace',
  'dashboard',
  'dashboard-terminal',
  'controller-input',
  'controller',
  'action-center',
]

/**
 * The fixtures, and the role each one plays.
 *
 * Five plain shells and two Claude sessions. §107 of the phase directive is
 * explicit that five panels should not each cost a real agent: a shell proves
 * that a terminal is isolated, multiplexed and independent, and two real Claude
 * sessions prove that the same is true of the thing the product exists for.
 */
const FIXTURES = [
  { name: 'demo-app', role: 'claude' },
  { name: 'studio', role: 'claude' },
  { name: 'alpha', role: 'shell' },
  { name: 'bravo', role: 'shell' },
  { name: 'charlie', role: 'shell' },
  { name: 'delta', role: 'shell' },
  { name: 'echo', role: 'shell' },
]

function parseArgs(argv) {
  const options = { suites: ALL_SUITES, keep: false, build: true, verbose: false }
  for (const arg of argv) {
    if (arg.startsWith('--suites=')) {
      options.suites = arg
        .slice('--suites='.length)
        .split(',')
        .map((name) => name.trim())
        .filter(Boolean)
    } else if (arg === '--keep') options.keep = true
    else if (arg === '--no-build') options.build = false
    else if (arg === '--verbose') options.verbose = true
  }
  for (const name of options.suites) {
    if (!ALL_SUITES.includes(name)) {
      throw new Error(`unknown suite ${JSON.stringify(name)}; known: ${ALL_SUITES.join(', ')}`)
    }
  }
  return options
}

/** step prints what is being done, so a failure has an obvious last line. */
function step(message) {
  console.log(`\n=== ${message} ===`)
}

/**
 * run runs a command on the machine Node is on and fails loudly.
 *
 * `shell` is off by default, because a path with a space in it - and this
 * repository's path has one - is torn apart by cmd.exe on the way through. The
 * one command that needs a shell is npm, which on Windows is a `.cmd` file and
 * cannot be spawned without one.
 */
function run(command, args, options = {}) {
  const result = spawnSync(command, args, {
    stdio: options.quiet ? 'pipe' : 'inherit',
    encoding: 'utf8',
    cwd: options.cwd ?? REPO_WINDOWS,
    shell: options.shell ?? false,
    env: { ...process.env, ...options.env },
  })
  if (result.error) {
    throw new Error(`${command} could not be run: ${result.error.message}`)
  }
  if (result.status !== 0) {
    const output = options.quiet ? `
${result.stdout ?? ''}
${result.stderr ?? ''}` : ''
    throw new Error(`${command} ${args.join(' ')} exited with ${result.status}${output}`)
  }
  return result.stdout ?? ''
}

/** buildServer cross-compiles the server for the distribution. */
function buildServer() {
  fs.mkdirSync(path.dirname(SERVER_BINARY), { recursive: true })
  run('go', ['build', '-o', SERVER_BINARY, './cmd/server'], {
    env: { GOOS: 'linux', GOARCH: 'amd64', CGO_ENABLED: '0' },
    quiet: true,
  })
}

/** buildFrontend produces the bundle the server will serve. */
function buildFrontend() {
  run('npm', ['run', 'build'], { quiet: true, shell: true, cwd: path.join(REPO_WINDOWS, 'web') })
}

/** copyIntoWSL puts a Windows file at a WSL path. */
function copyIntoWSL(windowsPath, wslPath) {
  const bytes = fs.readFileSync(windowsPath)
  wsl(`mkdir -p ${path.posix.dirname(wslPath)} && cat > ${wslPath} && chmod 755 ${wslPath}`, bytes)
}

/**
 * portHolder is the pid listening on the run's port, or null if it is free.
 *
 * Split out of `stopServer` because the survey asks the same question at the
 * end of a run, and because the answer is the only one that is reliable here:
 * `pkill -f` matches a command line, and a pattern one character off silently
 * kills nothing - measured, with a pattern that looked right and matched no
 * process at all.
 */
function portHolder() {
  const listening = wsl(
    `(ss -ltnp 2>/dev/null || netstat -ltnp 2>/dev/null) | grep ":${PORT} " || true`,
  )
  return /pid=(\d+)/.exec(listening)?.[1] ?? /\s(\d+)\//.exec(listening)?.[1] ?? null
}

/**
 * stopServer ends whatever is holding the port.
 *
 * Two ways, because one is not enough: the pattern, which catches the server
 * this run started, and the port itself, which catches anything else that got
 * there first.
 */
async function stopServer() {
  wsl('pkill -f "agentmux-e2e/agentmux-server"')
  await sleep(300)

  const pid = portHolder()
  if (pid) {
    wsl(`kill ${pid}`)
    await sleep(500)
  }
}

/** serve starts the server in the background and waits for it to answer. */
async function serve() {
  // A previous run that was killed rather than shut down leaves its server
  // holding the port, and the next one would fail with an address in use.
  await stopServer()

  startServer({ runDir: RUN_DIR, repoDir: REPO_DIR, dataDir: `${RUN_DIR}/data` })

  const ready = await waitForHttp(`${SERVER_URL}/api/server`, 30_000)
  if (!ready) {
    const log = wsl(`tail -40 ${RUN_DIR}/server.log`)
    throw new Error(`the server did not answer on ${SERVER_URL}. Its log says:\n${log}`)
  }
}

/** waitForHttp polls a URL until it answers. */
async function waitForHttp(url, timeoutMs) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    try {
      const response = await fetch(url)
      if (response.ok) return true
    } catch {
      // Not up yet. The loop is the retry.
    }
    await sleep(300)
  }
  return false
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

/**
 * api calls the server, failing loudly on a non-JSON or failing answer.
 *
 * A refused connection is wrapped rather than left as Node's bare "fetch
 * failed", because the caller that matters is the cleanup path: a server that
 * died mid-suite is exactly the case where seven runtimes cannot be destroyed
 * through it, and `POST /api/projects/p_x/runtime failed: connect ECONNREFUSED`
 * says which call and which address, where `fetch failed` says neither.
 */
async function api(pathname, init = {}) {
  const method = init.method ?? 'GET'
  let response
  try {
    response = await fetch(`${SERVER_URL}/api${pathname}`, {
      ...init,
      headers: init.body ? { 'Content-Type': 'application/json' } : undefined,
    })
  } catch (error) {
    throw new Error(`${method} /api${pathname}: ${error.cause?.message ?? error.message}`)
  }
  const text = await response.text()
  const body = text.trim() === '' ? {} : JSON.parse(text)
  if (!response.ok) {
    throw new Error(`${method} /api${pathname} failed with ${response.status}: ${text}`)
  }
  return body
}

/**
 * provision makes the fixture from nothing: directories, a data directory, the
 * server, then the projects and their runtimes through the API.
 *
 * Through the API rather than by writing rows, because the API is what a person
 * uses and a fixture that skipped it would be testing a state the product
 * cannot reach.
 */
async function provision() {
  // The suite before this one has been torn down by the time this runs, so its
  // record is finished with. Forgetting it here - rather than in `teardown` -
  // is what lets a signal during *this* provisioning clean up this run and not
  // the one that came before it.
  state.projects = []
  state.cleaning = null
  state.cleaned = false

  wsl(`rm -rf ${RUN_DIR} && mkdir -p ${RUN_DIR}/root ${RUN_DIR}/data`)

  // A short control grace, for two checks in the controller suite.
  //
  // The production default is thirty seconds, which is the right answer for a
  // person whose tablet slept and the wrong answer for a suite: a run that
  // sleeps half a minute to watch a lease lapse is a run nobody keeps. What the
  // checks are about is the shape - suspended, held for a while, then either
  // resumed or released - and eight seconds shows that shape as truthfully as
  // thirty does. The default itself is asserted in the terminal package's own
  // tests.
  //
  // Eight rather than three, because one of the two checks is a client that has
  // to reconnect on its own. A severed socket is retried on a growing delay
  // (`web/src/terminal/backoff.ts`: a quarter second, then a half, then a
  // second), so a controller that is genuinely disconnected and comes back takes
  // several seconds to do it - and a grace shorter than that would test the
  // lapse where it meant to test the resume.
  //
  // Written as the config file rather than as a flag because it *is* a
  // configuration value, and because this is the path an operator would use.
  wsl(`cat > ${RUN_DIR}/data/config.json`, JSON.stringify({ server: { controlGraceSeconds: 8 } }))

  for (const fixture of FIXTURES) {
    // A marker file, so a project directory looks like the thing discovery
    // looks for rather than an empty folder that happens to be there.
    wsl(
      `mkdir -p ${RUN_DIR}/root/${fixture.name} && ` +
        `printf '# ${fixture.name}\\n' > ${RUN_DIR}/root/${fixture.name}/CLAUDE.md`,
    )
  }

  copyIntoWSL(SERVER_BINARY, `${RUN_DIR}/agentmux-server`)
  await serve()

  const projects = []
  for (const fixture of FIXTURES) {
    const created = await api('/projects/register', {
      method: 'POST',
      body: JSON.stringify({ hostPath: `${RUN_DIR}/root/${fixture.name}` }),
    })
    const entry = { ...fixture, id: created.project.id, socket: sockOf(created.project) }
    // Recorded here, before the runtime is started rather than after. A signal
    // between these two lines arrives with a project that exists and a runtime
    // that does not - and a DELETE of a runtime that was never started is a
    // request the API answers with the stopped runtime it already has, so
    // recording early costs nothing and leaves nothing unregistered.
    state.projects.push(entry)
    projects.push(entry)
  }

  // Every runtime is started, and each one is given a workspace slot in the
  // order the fixtures are declared, so a suite can reason about positions.
  for (const [index, entry] of projects.entries()) {
    await api(`/projects/${entry.id}/runtime/start`, { method: 'POST' })
    await api(`/projects/${entry.id}`, {
      method: 'PATCH',
      body: JSON.stringify({ pinnedSlot: index }),
    })
  }

  // Claude, in the two projects that are meant to host it. Typed rather than
  // launched, which is how the product does it and what makes the agent a child
  // of the pane rather than of the server.
  //
  // One at a time, and each one waited for before the next is typed. Typing both
  // and moving on is a race twice over, and both halves of it were measured: the
  // suites ask whether Claude is up the moment they start, so a fixture that
  // returns before the screen is drawn can only answer "not yet"; and two
  // instances starting in the same second are more than this host keeps, which
  // is the Known Issue in `docs/WORKSPACE.md` and the reason the workspace suite
  // can report keeping one of two alive. Staggered, "Claude is up" becomes
  // something the fixture establishes rather than a coin flip the suite reports
  // afterwards as a skip.
  for (const entry of projects.filter((project) => project.role === 'claude')) {
    let up = false
    for (let attempt = 1; attempt <= CLAUDE_ATTEMPTS && !up; attempt += 1) {
      startClaude(entry)
      up = await waitForClaude(entry, CLAUDE_START_TIMEOUT_MS)
      if (!up) console.log(`    ${entry.name}: attempt ${attempt} did not come up`)
    }
    console.log(`    ${entry.name}: Claude ${up ? 'is up' : 'did not come up'}`)
  }

  const fixtures = {
    url: URL,
    runDir: RUN_DIR,
    dataDir: `${RUN_DIR}/data`,
    repoDir: REPO_DIR,
    distro: DISTRO,
    projects,
  }
  // Written for a person to read while debugging, and pointed at through the
  // environment for the suites, which is what makes them runnable one at a time
  // against the fixture this run built.
  const file = path.join(HERE, 'fixtures.json')
  fs.writeFileSync(file, JSON.stringify(fixtures, null, 2))
  process.env.AMX_E2E_FIXTURES = file
  return fixtures
}

/**
 * teardown ends what the run started.
 *
 * The runtimes are destroyed through the API, in the order they were created,
 * so the tmux servers go with them; the server is then killed and its directory
 * removed. A run that left seven tmux servers behind would be a run the next
 * one has to clean up after.
 *
 * # Called twice, and on purpose
 *
 * A signal can arrive while a suite's own teardown is already running, so this
 * has to be safe to call from two places at once and to do the work once.
 *
 * The project list is emptied before anything is destroyed - `splice(0)` hands
 * the caller the entries and leaves none - so a second call finds nothing to do
 * rather than issuing the same seven DELETEs a second time. A caller that
 * arrives while a cleanup is still in flight is handed the promise for the one
 * already running. Destroying a runtime that is already gone is not an error
 * even without that: the API reports a never-started runtime as stopped, and a
 * DELETE of one answers with that same stopped runtime.
 *
 * # Failures are reported, never swallowed
 *
 * This used to catch the DELETE and say nothing, on the reading that a runtime
 * which is already gone is the state being asked for. It is - but the same
 * catch also hid the case where the server had died, which is the case where
 * none of the seven can be destroyed, all seven tmux servers keep running, and
 * the run directory is about to be deleted out from under them. That is the
 * leak this file now exists to prevent, and it happened silently.
 *
 * So a failure names the project, the runtime and what the call said. There is
 * no separate runtime id to print: a runtime is addressed as
 * `/projects/{id}/runtime` and has no identity of its own, so the three things
 * that name it are the project it belongs to, the session in the backend and
 * the socket that session is on.
 */
async function teardown(reason) {
  if (state.cleaning) return state.cleaning

  state.cleaning = (async () => {
    const failures = []
    for (const entry of state.projects.splice(0)) {
      try {
        await api(`/projects/${entry.id}/runtime`, { method: 'DELETE' })
      } catch (error) {
        failures.push({ entry, error })
      }
    }

    await stopServer()
    wsl(`rm -rf ${RUN_DIR}`)

    // Recorded on the run, not only printed: whether the fallback is needed at
    // the end of the run is decided by whether anything is in here.
    state.failures.push(...failures)

    if (failures.length > 0) {
      console.log(`\n  cleanup could not reach ${failures.length} runtime(s) (${reason}):`)
      for (const { entry, error } of failures) {
        console.log(`    project id   ${entry.id}  (${entry.name})`)
        console.log(`    runtime      amx-${entry.id}  on ${entry.socket}`)
        console.log(`    cleanup err  ${error.message}`)
      }
      console.log('  their tmux servers are named again at the end of the run.')
    }
  })()

  return state.cleaning
}

/**
 * hardStop ends the run without waiting for anything.
 *
 * # Why it is synchronous
 *
 * It runs from the `exit` handler, where an `await` would never be reached -
 * the process is already on its way out - so every step here is a blocking
 * `spawnSync` through the same `wsl` helper the rest of the file uses.
 *
 * # Why it exists at all, given teardown
 *
 * Because the API teardown is the path that is unavailable exactly when it is
 * needed. A server that died mid-suite cannot destroy the runtimes it was
 * managing, and a tmux server whose socket file has been removed cannot be
 * found by its socket or by the API - which is how the Phase 6.5 leak worked:
 * the socket files went with `rm -rf`, and the servers kept running with
 * nothing left to name them by. So this looks for them by process as well.
 *
 * # What it will not touch
 *
 * It never runs `pkill tmux` or `killall tmux`, and it never uses the default
 * tmux socket. Whoever is running this has tmux sessions of their own on this
 * machine, and a pattern wide enough to be convenient is a pattern that ends
 * work this run has no business ending. Every kill below names either a socket
 * path inside the run's own directory or a process whose command line contains
 * one.
 */
function hardStop({ quiet = false } = {}) {
  if (state.keeping) {
    // Not a failure and not a fallback: the person asked for the fixture to
    // stay, and ending it here would be doing the opposite of what they said.
    state.cleaned = true
    if (!quiet) console.log('  --keep: the fixture is left up, as asked')
    return
  }

  if (state.child && state.child.exitCode === null && state.child.signalCode === null) {
    // First, because it holds a browser and is driving a fixture that is about
    // to stop existing.
    state.child.kill('SIGKILL')
  }

  wsl(
    [
      '# The runtimes, by socket, for the servers whose socket file is still there.',
      `for s in ${RUN_DIR}/data/tmux/*.sock; do`,
      '  [ -S "$s" ] || continue',
      '  tmux -S "$s" kill-server 2>/dev/null',
      'done',
      '',
      '# And by process, for a server whose socket file is already gone: the case',
      '# a `rm -rf` of the run directory leaves behind.',
      'for p in /proc/[0-9]*; do',
      '  pid=${p#/proc/}',
      '  [ -r "$p/cmdline" ] || continue',
      '  # This shell\'s own command line contains the path being matched - the',
      '  # script being run is one of its arguments - so it is the one process the',
      '  # pattern below would otherwise find and kill.',
      '  [ "$pid" = "$$" ] && continue',
      '  c=$(tr "\\0" " " < "$p/cmdline")',
      `  case "$c" in *"-S ${RUN_DIR}/data/tmux/"*) kill "$pid" 2>/dev/null ;; esac`,
      'done',
      '',
      '# The server, by pattern and then by the port it holds.',
      'pkill -f "agentmux-e2e/agentmux-server" 2>/dev/null',
      'sleep 0.4',
      // `$( ( ... ) )` and not `$(( ... )`. Bash reads `$((` as an arithmetic
      // expansion, and only re-reads it as command substitution when it reaches
      // the end without finding the `))` that would close it. Measured: with no
      // second parenthesis anywhere after it this line happens to work, and the
      // moment one appears it becomes arithmetic instead - `(echo three) | cat`
      // as an expression, then `missing )` and an empty result. The spacing is
      // the whole difference, so it is spelled out rather than left to luck.
      `pid=$( (ss -ltnp 2>/dev/null || netstat -ltnp 2>/dev/null) | grep ":${PORT} " | grep -o "pid=[0-9]*" | head -1 | cut -d= -f2 )`,
      '[ -n "$pid" ] && kill "$pid" 2>/dev/null',
      'exit 0',
    ].join('\n'),
  )

  // A blocking sleep rather than `await sleep`: this may be running inside the
  // exit handler, where an await is scheduled but never resumed.
  sleepSync(400)
  wsl(`rm -rf ${RUN_DIR}`)
  state.cleaned = true
}

/**
 * sleepSync blocks this thread.
 *
 * Only for `hardStop`, and only because there is nowhere else to wait from:
 * give a process that has just been signalled a moment to actually die before
 * the directory underneath it is removed.
 */
function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms)
}

/**
 * survey looks for what the run left behind, and says so.
 *
 * This is the check that would have caught the Phase 6.5 leak while it was
 * happening rather than three weeks later. It runs after the cleanup, so
 * anything it finds is either something the cleanup could not reach or
 * something that was never the run's - and the two are not told apart by
 * guessing.
 *
 * It reports and stops there. A warning that also cleans up is a warning nobody
 * reads, and the case where it matters is the case where the thing found was
 * not this run's to remove.
 */
function survey() {
  if (state.surveyed) return
  state.surveyed = true

  // The lines carry no leading whitespace of their own; the indent is added
  // once, below, where nothing can trim it off. They used to carry it, and the
  // whole-output `trim()` that was here took it from whichever line came first
  // - so the first thing found was reported flush against the margin and the
  // rest were not.
  const found = wsl(
    [
      'for p in /proc/[0-9]*; do',
      '  pid=${p#/proc/}',
      '  [ -r "$p/cmdline" ] || continue',
      '  [ "$pid" = "$$" ] && continue',
      '  c=$(tr "\\0" " " < "$p/cmdline")',
      `  case "$c" in *"${RUN_DIR}/data/tmux/"*) echo "still running:  pid $pid  $c" ;; esac`,
      'done',
      `ls -1 ${RUN_DIR}/data/tmux/*.sock 2>/dev/null | sed 's/^/still on disk:  /'`,
      'exit 0',
    ].join('\n'),
  )
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)

  const holder = portHolder()
  if (holder) found.push(`still listening: port ${PORT}  pid ${holder}`)

  if (found.length === 0) {
    console.log('  nothing this run made is still running')
    return
  }

  console.log(`\n  ${found.length} thing(s) from this run are still up:`)
  for (const line of found) console.log(`    ${line}`)
  console.log(
    `  Nothing outside ${RUN_DIR} and port ${PORT} was looked at, and nothing was removed.`,
  )
  console.log('  If these are this run\'s, they can be ended the same way:')
  console.log(`    tmux -S ${RUN_DIR}/data/tmux/<project-id>.sock kill-server`)
}

/**
 * interrupt ends the run the way a clean finish would, on a signal.
 *
 * # The two signals
 *
 * The first one is a person asking the run to stop, and it is answered by
 * ending what the run started before the process goes. The second is a person
 * saying they have stopped waiting for the first - so it does the parts that
 * are instant and leaves, because a run that cannot be interrupted is worse
 * than one that leaves a socket behind and says which.
 *
 * The API teardown is bounded for the same reason. Twenty seconds is generous
 * for seven DELETEs and a kill by port; past it the synchronous fallback takes
 * over, which is the path that works when the server is the thing that is
 * stuck.
 */
async function interrupt(signal) {
  state.signals += 1
  state.exitCode = signal === 'SIGINT' ? 130 : 143

  if (state.signals > 1) {
    console.log(`\n${signal} again: stopping now`)
    hardStop({ quiet: true })
    process.exit(state.exitCode)
  }

  console.log(`\n${signal}: ending the run, and what it started`)
  state.interrupted = true

  // On a terminal this is already done: Ctrl-C goes to the whole foreground
  // process group and the suite gets its own copy. It is here for the signal
  // aimed at this process alone - a `kill` from a task manager, which the suite
  // never sees.
  if (state.child && state.child.exitCode === null) state.child.kill('SIGTERM')

  const finished = await Promise.race([
    teardown(signal).then(() => true),
    sleep(20_000).then(() => false),
  ])
  if (!finished) {
    console.log('  the API teardown did not finish in 20s; ending what it could not reach')
  }

  // Unconditionally, not only on failure. `provision` may still be part-way
  // through when the signal lands and can leave a server running behind a
  // teardown that has already been and gone; this is the pass that catches it,
  // and it is cheap because it only looks where the run has been.
  hardStop()
  survey()
  process.exit(state.exitCode)
}

process.on('SIGINT', () => void interrupt('SIGINT'))
process.on('SIGTERM', () => void interrupt('SIGTERM'))

// The last resort, for the paths that reach neither the loop's own teardown nor
// `interrupt`: a suite process that throws in a way that escapes `main`, an
// unhandled rejection, a `process.exit` from somewhere unexpected. It does
// nothing when cleanup has already happened, which is the normal end of a run.
process.on('exit', () => {
  if (!state.cleaned) hardStop({ quiet: true })
})

/** runSuite runs one suite as its own process and reports what it said. */
function runSuite(name) {
  return new Promise((resolve) => {
    console.log(`\n${'-'.repeat(72)}\n${name}\n${'-'.repeat(72)}`)
    const child = spawn(process.execPath, [path.join(HERE, 'suites', `${name}.mjs`)], {
      stdio: 'inherit',
      env: process.env,
    })
    // Recorded so that a signal arriving while the suite runs can end it. With
    // `stdio: 'inherit'` a Ctrl-C reaches it anyway, but a signal aimed at this
    // process alone does not, and the suite would keep driving a browser
    // against a fixture that is being torn down underneath it.
    state.child = child
    const timer = setTimeout(() => {
      console.log(`\n${name} timed out after ${SUITE_TIMEOUT_MS}ms`)
      child.kill('SIGKILL')
    }, SUITE_TIMEOUT_MS)
    child.on('close', (code) => {
      clearTimeout(timer)
      state.child = null
      resolve({ name, ok: code === 0 })
    })
  })
}

async function main() {
  const options = parseArgs(process.argv.slice(2))

  if (options.build) {
    step('Building the server and the frontend')
    buildServer()
    buildFrontend()
  }

  let ok = true
  let kept = null
  try {
    // A fixture per suite, not one for the whole run.
    //
    // The suites share a workspace and they mutate it: one narrows it to a
    // single project, another stops a runtime, a third types Ctrl+C into a
    // shell. Against one fixture, each suite starts from whatever the last one
    // left behind - which showed up as failures in suites that pass on their
    // own, the least useful kind of red there is. Rebuilding costs a few
    // seconds and makes a suite say the same thing in a run as it does alone.
    for (const [index, name] of options.suites.entries()) {
      // A signal during the suite before this one has already ended its
      // fixture. Building another now would put a new server on the port the
      // cleanup is trying to free, so the loop stops at the next boundary.
      if (state.interrupted) break

      step(`Fixture for ${name}`)
      const fixtures = await provision()
      console.log(
        `${fixtures.projects.length} projects, ` +
          `${fixtures.projects.filter((project) => project.role === 'claude').length} of them hosting Claude`,
      )

      const result = await runSuite(name)
      if (!result.ok) ok = false

      if (state.interrupted) break

      if (options.keep && index === options.suites.length - 1) {
        kept = fixtures
        // Set here rather than from the flag: `--keep` is about the *last*
        // fixture, so an interruption during an earlier suite must still be
        // allowed to end what that suite made.
        state.keeping = true
      } else {
        await teardown(`after ${name}`)
      }
    }
  } finally {
    if (kept) {
      state.cleaned = true
      step(`Kept: the server is on ${SERVER_URL} and the fixtures are in ${RUN_DIR}`)
      console.log('Destroy them by running the suites again without --keep')
    } else {
      if (options.keep) step('Nothing kept: the run did not get as far as a suite')
      else step('Tearing down')

      // The last teardown normally ran in the loop above. This is for the paths
      // that leave it: a suite that threw, a signal mid-suite, a fixture built
      // by a run that had not yet reached its first suite. Running it twice
      // costs nothing, which is what the emptying of the project list buys.
      await teardown('at the end of the run')

      // The API is the path that works, and this is the path for when it did
      // not: a server that died mid-suite leaves seven tmux servers that no
      // DELETE can reach, and their sockets are about to go with the run
      // directory. Cheap when there is nothing to do - one directory and one
      // port - and the difference between an interrupted run the next one can
      // start on top of and one it cannot.
      if (state.failures.length > 0) hardStop({ quiet: true })

      // Only now, and only once the cleanup above has been given its chance:
      // `hardStop` is the fallback for cleanup that did not happen, and it must
      // not fire behind one that did.
      state.cleaned = true
      survey()
    }
  }

  console.log(`\n${'='.repeat(72)}`)
  if (state.interrupted) {
    // Not a failure: a suite that was still running when the signal arrived
    // reports a non-zero code, and calling that a red run would be reporting
    // the interruption as a result.
    console.log('the run was interrupted; what it started has been ended')
    process.exit(state.exitCode)
  }
  console.log(ok ? 'every suite passed' : 'SOME SUITES FAILED')
  process.exit(ok ? 0 : 1)
}

main().catch((error) => {
  // Reached only for something that escaped `main`'s own `finally` - so either
  // a fixture that could not be built or a teardown that threw. Anything left
  // running by it is ended by the `exit` handler above.
  console.error(`\nthe run stopped early: ${error.message}`)
  process.exit(1)
})
