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
 */
function wsl(script, input) {
  const result = spawnSync('wsl', ['-d', DISTRO, 'bash', '-lc', script], {
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

/** The order suites run in: cheapest and most fundamental first. */
const ALL_SUITES = ['transport', 'terminal', 'recovery', 'tablet', 'workspace']

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
 * stopServer ends whatever is holding the port.
 *
 * Two ways, because one is not enough. `pkill -f` matches a command line, and a
 * pattern that is one character off silently kills nothing - measured, with a
 * pattern that looked right and matched no process at all. The port is the fact
 * that matters, so it is asked directly and whatever holds it is killed by pid.
 */
async function stopServer() {
  wsl('pkill -f "agentmux-e2e/agentmux-server"')
  await sleep(300)

  const listening = wsl(
    `(ss -ltnp 2>/dev/null || netstat -ltnp 2>/dev/null) | grep ":${PORT} " || true`,
  )
  const pid = /pid=(\d+)/.exec(listening)?.[1] ?? /\s(\d+)\//.exec(listening)?.[1]
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

/** api calls the server, failing loudly on a non-JSON or failing answer. */
async function api(pathname, init = {}) {
  const response = await fetch(`${SERVER_URL}/api${pathname}`, {
    ...init,
    headers: init.body ? { 'Content-Type': 'application/json' } : undefined,
  })
  const text = await response.text()
  const body = text.trim() === '' ? {} : JSON.parse(text)
  if (!response.ok) {
    throw new Error(`POST ${pathname} failed with ${response.status}: ${text}`)
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
  wsl(`rm -rf ${RUN_DIR} && mkdir -p ${RUN_DIR}/root ${RUN_DIR}/data`)

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
    projects.push({ ...fixture, id: created.project.id })
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
  for (const entry of projects.filter((project) => project.role === 'claude')) {
    wsl(`tmux -S ${RUN_DIR}/data/tmux/${entry.id}.sock send-keys -t amx-${entry.id} -l 'claude'`)
    wsl(`tmux -S ${RUN_DIR}/data/tmux/${entry.id}.sock send-keys -t amx-${entry.id} Enter`)
  }

  const fixtures = {
    url: URL,
    runDir: RUN_DIR,
    dataDir: `${RUN_DIR}/data`,
    repoDir: REPO_DIR,
    distro: DISTRO,
    projects: projects.map((entry) => ({ ...entry, socket: `${RUN_DIR}/data/tmux/${entry.id}.sock` })),
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
 */
async function teardown(fixtures) {
  for (const entry of fixtures.projects) {
    try {
      await api(`/projects/${entry.id}/runtime`, { method: 'DELETE' })
    } catch {
      // A project whose runtime is already gone is the state being asked for.
    }
  }
  await stopServer()
  wsl(`rm -rf ${RUN_DIR}`)
}

/** runSuite runs one suite as its own process and reports what it said. */
function runSuite(name) {
  return new Promise((resolve) => {
    console.log(`\n${'-'.repeat(72)}\n${name}\n${'-'.repeat(72)}`)
    const child = spawn(process.execPath, [path.join(HERE, 'suites', `${name}.mjs`)], {
      stdio: 'inherit',
      env: process.env,
    })
    const timer = setTimeout(() => {
      console.log(`\n${name} timed out after ${SUITE_TIMEOUT_MS}ms`)
      child.kill('SIGKILL')
    }, SUITE_TIMEOUT_MS)
    child.on('close', (code) => {
      clearTimeout(timer)
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
      step(`Fixture for ${name}`)
      const fixtures = await provision()
      console.log(
        `${fixtures.projects.length} projects, ` +
          `${fixtures.projects.filter((project) => project.role === 'claude').length} of them hosting Claude`,
      )

      const result = await runSuite(name)
      if (!result.ok) ok = false

      if (options.keep && index === options.suites.length - 1) {
        kept = fixtures
      } else {
        await teardown(fixtures)
      }
    }
  } finally {
    if (kept) {
      step(`Kept: the server is on ${SERVER_URL} and the fixtures are in ${RUN_DIR}`)
      console.log('Destroy them by running the suites again without --keep')
    } else if (options.keep) {
      step('Nothing kept: the run did not get as far as a suite')
    } else {
      step('Tearing down')
    }
  }

  console.log(`\n${'='.repeat(72)}`)
  console.log(ok ? 'every suite passed' : 'SOME SUITES FAILED')
  process.exit(ok ? 0 : 1)
}

main().catch((error) => {
  console.error(`\nthe run could not start: ${error.message}`)
  process.exit(1)
})
