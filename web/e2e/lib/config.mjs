/**
 * Where the end-to-end run keeps its things.
 *
 * The server, the tmux sessions and the fixture projects all live inside WSL,
 * because that is where a runtime can exist at all. Node runs on Windows. So
 * every path in this directory is a WSL path, and the only Windows paths are
 * the ones a browser is pointed at.
 *
 * Nothing here writes inside the repository: the run directory is a temporary
 * one, recreated by `run.mjs` and removed by it. A suite that left state behind
 * would fail the next run for a reason that had nothing to do with the code.
 */
import { spawn } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

/** The WSL distribution the runtime lives in. */
export const DISTRO = process.env.AMX_E2E_DISTRO ?? 'Ubuntu-24.04'

/**
 * The run directory, as WSL sees it.
 *
 * Outside the repository on purpose. It holds a data directory, a projects
 * root, seven tmux sockets and a server log; none of that is source, and a
 * stray `git add -A` in a tired moment should not be able to commit it.
 */
export const RUN_DIR = process.env.AMX_E2E_DIR ?? '/tmp/agentmux-e2e'

/**
 * Where the fixture description lives.
 *
 * Written by the runner into the e2e directory, which is a Windows path - the
 * suites read it with Node, not inside the distribution, so it belongs on the
 * side that reads it.
 */
export const FIXTURES_PATH = process.env.AMX_E2E_FIXTURES ?? fileURLToPath(new URL('../fixtures.json', import.meta.url))

/** The repository root, as WSL sees it. */
export const REPO_DIR =
  process.env.AMX_E2E_REPO ?? '/mnt/d/AI/Projects/2026 AgentMux/AgentMux'

/**
 * The address a browser reaches the server on.
 *
 * Named SERVER_URL rather than URL, which is what it wants to be called. A
 * module-level `const URL` shadows the global constructor for the whole module
 * - including for code above it in the file, which fails at import time with
 * "Cannot access 'URL' before initialization" or "URL is not a constructor",
 * depending on which side of the declaration it is on. Both were met while
 * writing this.
 */
export const SERVER_URL = process.env.AMX_E2E_URL ?? 'http://127.0.0.1:8787'

/** The port the server listens on. */
export const PORT = Number(process.env.AMX_E2E_PORT ?? /:(\d+)/.exec(SERVER_URL)?.[1] ?? 8787)

/** How long a suite may take before the runner gives up on it. */
export const SUITE_TIMEOUT_MS = Number(process.env.AMX_E2E_SUITE_TIMEOUT ?? 900_000)

/**
 * The fixture description, as the suites read it.
 *
 * The file rather than the environment because it is structured - seven
 * projects, each with an id, a name, a directory and a socket - and flattening
 * that into variables would be a second shape to keep in step with the first.
 */
export function readFixtures() {
  const file = process.env.AMX_E2E_FIXTURES ?? FIXTURES_PATH
  if (!fs.existsSync(file)) {
    throw new Error(
      `no fixture description at ${file}. Run the suites through \`npm run test:e2e\`, ` +
        'which builds one, rather than running a suite directly.',
    )
  }
  return JSON.parse(fs.readFileSync(file, 'utf8'))
}

/**
 * startServer launches the server and leaves it running.
 *
 * # Why this is not a backgrounded shell job
 *
 * `wsl.exe` is what keeps a WSL distribution alive, and a command that
 * backgrounds its work and exits takes the distribution with it - measured:
 * `( setsid foo & )`, run from Node, leaves no process and an empty log, while
 * the same command typed by hand at a shell works, because that shell's own
 * wsl.exe is still attached.
 *
 * So the server runs in the *foreground* of its own wsl.exe, which is left
 * detached and unreferenced on the Windows side. The distribution therefore has
 * a client for as long as the server runs, and stopping the server stops it.
 * Its output is redirected inside the distribution, because the Windows side of
 * this pipe is deliberately ignored.
 */
export function startServer({ runDir, repoDir, dataDir, port = PORT }) {
  const command =
    `exec ${runDir}/agentmux-server ` +
    `-host 127.0.0.1 -port ${port} ` +
    `-projects-root ${runDir}/root -data-dir ${dataDir} ` +
    `-web-dir "${repoDir}/web/dist" -log-level debug ` +
    `>> ${runDir}/server.log 2>&1`

  const child = spawn('wsl', ['-d', DISTRO, '--', 'bash', '-lc', command], {
    detached: true,
    stdio: 'ignore',
    env: { ...process.env, MSYS_NO_PATHCONV: '1' },
  })
  child.unref()
  return child.pid
}

/** A local scratch path on the machine Node is running on. */
export function scratchPath(name) {
  return path.join(os.tmpdir(), 'agentmux-e2e', name)
}
