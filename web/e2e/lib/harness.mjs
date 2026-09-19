/**
 * What every browser suite is built out of.
 *
 * Four suites share one fixture, one server and one idea of what "check"
 * means, so the plumbing lives here rather than four times over. What is here
 * is deliberately mechanical: run a command in WSL, ask tmux what a pane says,
 * open a page, record a result. Everything that is a *claim* stays in the
 * suite that makes it.
 *
 * The one piece of hard-won knowledge worth reading before changing anything:
 * **text passed to `wsl.exe` as an argument does not arrive intact.** wsl.exe
 * joins its arguments and runs them through a shell, so `$(seq 1 120)` and `$i`
 * are expanded before tmux ever sees them - measured, by typing a loop and
 * reading back the malformed command line it produced. Anything that has to
 * reach a terminal verbatim goes through `typeCommand`, which writes to a file
 * and has tmux paste it.
 */
import { execFileSync, spawn } from 'node:child_process'

import { DISTRO, PORT, readFixtures, startServer as startServerIn, SERVER_URL } from './config.mjs'

/** The fixture description for this run. */
export const fixtures = readFixtures()

/** The page under test. */
export const PAGE_URL = fixtures.url ?? SERVER_URL

/** The port the server listens on, for a suite that has to restart it. */
export { PORT }

/** One fixture project by the role it plays. */
export function project(role) {
  const found = fixtures.projects.find((entry) => entry.role === role)
  if (!found) throw new Error(`the fixture has no project with the role ${JSON.stringify(role)}`)
  return found
}

/** The tmux socket path for a fixture project. */
export const sockOf = (entry) => `${fixtures.dataDir}/tmux/${entry.id}.sock`

/** The tmux session name for a fixture project. */
export const sessionOf = (entry) => `amx-${entry.id}`

/**
 * wsl runs a script inside the distribution.
 *
 * `|| true` throughout: almost everything these suites ask for is a read-only
 * probe, and a grep that finds nothing or a tmux that has gone away exits
 * non-zero. That is an answer, not a reason to throw.
 *
 * # `--exec`, and why every call to this needs it
 *
 * Without it, `wsl.exe` does not hand the script to the distribution as an
 * argument. It joins its own command line into one string and runs that through
 * the distribution's default shell, which expands it once on the way - so what
 * arrives is not what was sent. Measured, with `p=hello; echo "p=[$p]"`: `$p`
 * arrives empty, `$HOME` arrives expanded, `$$` arrives as that shell's own
 * pid, and quoting does not help, because the expansion happens before the
 * quotes are read. `--exec` execs the program directly and the string arrives
 * as written.
 *
 * It is worth knowing because the failure is silent and reads as success. The
 * `sessionAlive` below was written as `... has-session ...; echo $?`, and its
 * `$?` was expanded before bash ran anything: it answered `0` for every
 * session, including ones that did not exist. Nothing about it looked wrong.
 */
export function wsl(script, input) {
  try {
    return execFileSync('wsl', ['-d', DISTRO, '--exec', 'bash', '-lc', `${script} || true`], {
      encoding: 'utf8',
      input,
      env: { ...process.env, MSYS_NO_PATHCONV: '1' },
    })
  } catch (error) {
    // A command whose own shell was killed - `pkill -f` matching the shell that
    // is running it is the way that happens - still produced an answer worth
    // reading, and it is not a reason for a suite to stop.
    return error.stdout ?? ''
  }
}

/**
 * stopServer ends the run's server, by port rather than by name.
 *
 * `pkill -f` matches a command line, and the shell running the pkill has the
 * pattern in its own command line: it kills itself. Measured, and it looks like
 * a suite crashing for no reason. The port is the fact that matters, so it is
 * asked directly.
 */
export function stopServer() {
  const listening = wsl(
    `(ss -ltnp 2>/dev/null || netstat -ltnp 2>/dev/null) | grep ":${PORT} " || true`,
  )
  const pid = /pid=(\d+)/.exec(listening)?.[1] ?? /\s(\d+)\//.exec(listening)?.[1]
  if (pid) wsl(`kill ${pid}`)
  return pid ?? ''
}

/** startServer launches the run's server, detached. See lib/config.mjs. */
export function startServer() {
  return startServerIn({ runDir: fixtures.runDir, repoDir: fixtures.repoDir, dataDir: fixtures.dataDir })
}

/** wslAsync is `wsl` without blocking the event loop, for a long command. */
export function wslAsync(script) {
  return new Promise((resolve) => {
    const child = spawn('wsl', ['-d', DISTRO, '--exec', 'bash', '-lc', `${script} || true`], {
      env: { ...process.env, MSYS_NO_PATHCONV: '1' },
    })
    let out = ''
    child.stdout.on('data', (chunk) => (out += chunk))
    child.stderr.on('data', (chunk) => (out += chunk))
    child.on('close', () => resolve(out))
  })
}

/** The text tmux says a pane holds. */
export function pane(entry, extra = '') {
  return wsl(`tmux -S "${sockOf(entry)}" capture-pane -p ${extra} -t ${sessionOf(entry)}`)
}

/** The pane's size, as the pty reports it. */
export function paneSize(entry) {
  return wsl(
    `tmux -S "${sockOf(entry)}" display-message -p -t ${sessionOf(entry)} '#{window_width}x#{window_height}'`,
  ).trim()
}

/** Whether a session exists on its own socket. */
export function sessionAlive(entry) {
  return (
    wsl(`tmux -S "${sockOf(entry)}" has-session -t ${sessionOf(entry)} >/dev/null 2>&1; echo $?`).trim() ===
    '0'
  )
}

/**
 * claudeIsUp reports whether a fixture project's session is showing Claude.
 *
 * The test is the project's own shell prompt: if the pane ends in
 * `<name>$` then the session is at a shell, and whatever was started in it -
 * Claude - is no longer running. It is deliberately a test about the session
 * rather than about a process, because the process table cannot say which
 * session a Claude belongs to.
 */
export function claudeIsUp(entry) {
  const text = pane(entry)
  return !text.includes(`${entry.name}$`) && text.trim() !== ''
}

/** The pid of a running Claude, or "" when none is. */
export function agentPid() {
  return wsl('pgrep -f "claude/versions" | head -1').trim()
}

/** How many Claude processes are running. */
export function agentCount() {
  return Number(wsl('pgrep -cf "claude/versions"').trim() || '0')
}

/**
 * claudePids is every running Claude process.
 *
 * A suite compares the *count*, not one pid. On this host the CLI re-executes
 * itself as part of starting up - observed as a pid that changes every few
 * seconds while nothing about the runtime does - so a check written against a
 * single pid fails for a reason that has nothing to do with AgentMux. What
 * these checks are about is whether anything restarted a runtime, and a
 * count that does not fall answers that.
 */
export function claudePids() {
  return wsl('pgrep -f "claude/versions"').trim().split(String.fromCharCode(10)).filter(Boolean)
}

/**
 * writeFile puts a file in the run directory and returns its path.
 *
 * Through stdin, never as an argument to wsl.exe: a payload with a `$` or a
 * backslash in it does not survive that trip, which cost an afternoon to find.
 */
export function writeFile(name, body) {
  const path = `${fixtures.runDir}/${name}`
  wsl(`cat > "${path}"`, body)
  return path
}

/**
 * typeCommand types a command at a session and presses Enter.
 *
 * Through a file and tmux's own paste buffer, never as an argument to wsl.exe.
 * See the note at the top of this file: the argument route evaluates what it is
 * given.
 */
export function typeCommand(entry, text) {
  const payload = `"${fixtures.runDir}/payload.txt"`
  wsl(`cat > ${payload}`, text)
  wsl(
    `tmux -S "${sockOf(entry)}" load-buffer -b amxpay ${payload} && ` +
      `tmux -S "${sockOf(entry)}" paste-buffer -b amxpay -t ${sessionOf(entry)} -d && ` +
      `sleep 0.3 && tmux -S ${sockOf(entry)} send-keys -t ${sessionOf(entry)} Enter`,
  )
}

/**
 * settleShell puts a session back at an empty prompt, from outside a browser.
 *
 * The suites share one fixture, and a suite that inherits a running command or
 * a recalled history line types into something that is not reading its input.
 * Ctrl+C clears the line and interrupts a foreground command; Ctrl+U clears it
 * from the cursor back, for when the cursor is not at the end.
 */
export function settleShell(entry) {
  const target = `-t ${sessionOf(entry)}`
  wsl(
    `tmux -S ${sockOf(entry)} send-keys ${target} C-c \\; ` +
      `send-keys ${target} C-u \\; ` +
      `send-keys ${target} C-c`,
  )
}

// ---------------------------------------------------------------------------
// Results

/** One suite's results. */
export class Report {
  constructor(name) {
    this.name = name
    this.results = []
  }

  /** check records one claim. `detail` is the evidence, present or not. */
  check(name, ok, detail = '') {
    this.results.push({ name, ok, detail, skipped: false })
    console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? `  -- ${detail}` : ''}`)
    return ok
  }

  /**
   * skip records a claim the run could not test, and why.
   *
   * A third answer rather than a quiet pass, because the two are not the same
   * thing to a reader: a check that passed is evidence, and a check that could
   * not run is the absence of evidence. The reason is printed with it, and the
   * summary counts it separately, so nobody has to guess which it was.
   */
  skip(name, reason) {
    this.results.push({ name, ok: false, detail: reason, skipped: true })
    console.log(`SKIP  ${name}  -- ${reason}`)
  }

  get passed() {
    return this.results.filter((result) => result.ok).length
  }

  get skipped() {
    return this.results.filter((result) => result.skipped).length
  }

  get failed() {
    return this.results.filter((result) => !result.ok && !result.skipped)
  }

  /** summary prints the line the runner reads. */
  summary() {
    const skipped = this.skipped > 0 ? `, ${this.skipped} skipped` : ''
    console.log(`\n${this.passed}/${this.results.length - this.skipped} checks passed${skipped}`)
    return this.failed.length === 0
  }
}

// ---------------------------------------------------------------------------
// The page

/** How long a panel may take to appear, in milliseconds. */
export const PAGE_TIMEOUT_MS = 30_000

/**
 * openPage opens the workspace and waits for it to settle.
 *
 * Every context is given the same init script, which makes a soft keyboard
 * expressible: a real one shrinks the visual viewport and leaves the layout
 * viewport alone, and Chrome will not do that on request. Only that one
 * property is faked; touch, mobile emulation and the device scale factor are
 * Playwright's.
 */
export async function openPage(browser, options = {}) {
  const context = await browser.newContext({
    viewport: options.viewport ?? { width: 1440, height: 900 },
    hasTouch: options.hasTouch ?? false,
    isMobile: options.isMobile ?? false,
    deviceScaleFactor: options.deviceScaleFactor ?? 1,
    colorScheme: options.colorScheme,
    // A device that does not state one gets Playwright's, which is this
    // machine's Chrome. Phase 6 names a device on *other people's* screens, and
    // it derives that name from this header - so a suite that is about which
    // device is which states both of them rather than reading back whatever
    // this machine happens to be running.
    userAgent: options.userAgent,
  })
  await context.addInitScript(() => {
    const viewport = new EventTarget()
    viewport.height = window.innerHeight
    viewport.width = window.innerWidth
    viewport.offsetTop = 0
    Object.defineProperty(window, 'visualViewport', { configurable: true, get: () => viewport })
    window.__softKeyboard = (open) => {
      viewport.height = window.innerHeight - (open ? 300 : 0)
      viewport.dispatchEvent(new Event('resize'))
    }

    // Every WebSocket the page opens, in the order it opened them.
    //
    // This exists for one check - the controller whose connection is severed -
    // and it records rather than intercepts: the page is handed the real socket
    // and nothing about its behaviour changes. It is here rather than in that
    // suite because an init script has to be installed before the page loads,
    // and this is where that happens.
    //
    // Why the socket has to be closed by hand at all is in the comment on
    // `severConnection` below.
    const Native = window.WebSocket
    window.__sockets = []
    window.WebSocket = new Proxy(Native, {
      construct(target, args) {
        const socket = new target(...args)
        window.__sockets.push(socket)
        return socket
      },
    })
  })

  const page = await context.newPage()
  const frames = []
  const errors = []
  page.on('console', (message) => message.type() === 'error' && errors.push(message.text()))
  page.on('pageerror', (error) => errors.push(`pageerror: ${error.message}`))
  page.on('websocket', (socket) => {
    socket.on('framesent', (frame) => frames.push({ dir: 'out', payload: frame.payload }))
    socket.on('framereceived', (frame) => frames.push({ dir: 'in', payload: frame.payload }))
  })

  await page.goto(PAGE_URL, { waitUntil: 'domcontentloaded' })
  return { context, page, frames, errors }
}

/**
 * severConnection closes a page's open socket underneath it, as a network would.
 *
 * This is for the one scenario a browser cannot be asked to produce on its own.
 * `context.setOffline(true)` stops a page from opening anything new, but it does
 * not tear down a socket that is already open: the connection stays up at both
 * ends, the server has no write outstanding and no way to notice a socket that
 * is merely silent, and nothing happens until the connection is closed by
 * something else. That is measured, not assumed - the first version of the
 * controller suite waited ninety seconds for a disconnected controller to be
 * noticed and it never was.
 *
 * Closing the socket from the page produces what the network would have
 * produced: the server sees the connection end, the client sees its socket close
 * without having asked for it, and the reconnect that follows is its own. It is
 * used together with `setOffline(true)`, which is what stops that reconnect from
 * succeeding - so the pair is a network black hole rather than a blip.
 */
export async function severConnection(page) {
  return page.evaluate(() => {
    const sockets = window.__sockets ?? []
    const socket = sockets[sockets.length - 1]
    if (!socket) return false
    socket.close()
    return true
  })
}

/**
 * openProjects puts projects into the workspace and waits for their panels.
 *
 * Membership is a server fact, so this is a click in the Project Manager rather
 * than a query parameter: the suite drives the same control a person does. A
 * project that is already open is left alone, so a suite can call this twice.
 */
export async function openProjects(page, entries) {
  for (const entry of entries) {
    const open = page.getByRole('button', { name: `Open ${entry.name} in the workspace` })
    if ((await open.count()) > 0) {
      await open.first().click()
      await page.waitForTimeout(400)
    }
  }
  await page.waitForSelector('.xterm-screen', { timeout: PAGE_TIMEOUT_MS })
  await page.waitForTimeout(2000)
}

/**
 * openProject opens one project into the workspace and returns its panel.
 *
 * It is `openProjects` for the ordinary case where a suite is about a single
 * terminal, and it leaves the workspace holding that project alone if it was
 * empty to begin with.
 */
export async function openProject(page, entry) {
  await openProjects(page, [entry])
  return panelOf(page, entry)
}

/** The panel for a project, addressed by the id the DOM carries. */
export function panelOf(page, entry) {
  return page.locator(`.panel--project[data-project-id="${entry.id}"]`)
}

/**
 * workspaceMembers reads who is in the workspace, from the manager's own list.
 *
 * The list rather than the grid, because the grid only shows one page: the
 * question "who is a member" is answered by the same panel a person would look
 * at to answer it.
 */
export async function workspaceMembers(page) {
  return page.evaluate(() => {
    const section = document.querySelector('.manager__section[aria-label="In the workspace"]')
    if (!section) return []
    return Array.from(section.querySelectorAll('.project-list__row')).map((row) => ({
      id: row.getAttribute('data-project-id') ?? '',
      name: row.querySelector('.project-list__name')?.textContent ?? '',
    }))
  })
}

/**
 * goToLastPage pages to the end of the workspace, where the manager is.
 *
 * On a single-column layout the Project Manager is a page of its own at the
 * end, so the membership controls are not on the page a suite starts on. Any
 * suite that changes membership has to go where a person would go.
 */
async function goToLastPage(page) {
  for (let attempt = 0; attempt < 20; attempt += 1) {
    const next = page.getByRole('button', { name: 'Next page' })
    if ((await next.count()) === 0 || (await next.isDisabled())) return
    await next.click()
    await page.waitForTimeout(250)
  }
}

/** goToFirstPage pages back to the start. */
async function goToFirstPage(page) {
  for (let attempt = 0; attempt < 20; attempt += 1) {
    const previous = page.getByRole('button', { name: 'Previous page' })
    if ((await previous.count()) === 0 || (await previous.isDisabled())) return
    await previous.click()
    await page.waitForTimeout(250)
  }
}

/**
 * setWorkspace makes the workspace hold exactly these projects.
 *
 * Membership is a server fact that outlives a browser context, so a suite that
 * changes it changes it for every suite that runs afterwards. Each section
 * therefore states the workspace it wants rather than assuming the one it
 * inherited - which is also what makes a section runnable on its own.
 *
 * Every pass re-reads the list rather than working from a stale snapshot: a
 * removal changes how many pages there are, and therefore where the manager is.
 */
export async function setWorkspace(page, entries) {
  const wanted = new Set(entries.map((entry) => entry.id))

  for (let pass = 0; pass < 12; pass += 1) {
    await goToLastPage(page)
    const members = await workspaceMembers(page)
    const extra = members.filter((member) => !wanted.has(member.id))
    const missing = entries.filter((entry) => !members.some((member) => member.id === entry.id))
    if (process.env.AMX_E2E_DEBUG) {
      const pager = await page.locator('.pager__count').textContent().catch(() => '(none)')
      console.log(`    [setWorkspace] pass ${pass}: pager ${pager}, ${members.length} member(s), ${extra.length} to remove, ${missing.length} to add`)
    }
    if (extra.length === 0 && missing.length === 0) break

    for (const member of extra) {
      await page.getByRole('button', { name: `Remove ${member.name} from the workspace` }).click()
      await page.waitForTimeout(350)
      await goToLastPage(page)
    }
    for (const entry of missing) {
      const open = page.getByRole('button', { name: `Open ${entry.name} in the workspace` })
      if ((await open.count()) > 0) {
        await open.first().click()
        await page.waitForTimeout(350)
      }
      await goToLastPage(page)
    }
  }

  await goToFirstPage(page)

  // Settle: every terminal that is on the page connected, and the layout the
  // change produced has been measured and sent. A section that reads a screen
  // immediately after this would be reading one mid-redraw.
  await waitFor(
    async () => {
      const panels = await page.locator('.panel--project').count()
      if (panels === 0) return true
      return (await page.locator('.terminal__state--open').count()) === panels
    },
    20_000,
  )
  await page.waitForTimeout(2500)
}

/**
 * awaitProject finds a project's panel, paging the workspace if it has to.
 *
 * This is what a suite reaches for instead of "click the project in the list".
 * The workspace is a server fact, so every fixture project may already be a
 * member when the page loads - and only the current page's members have panels
 * at all, since a page nobody is looking at holds no subscriptions. A suite
 * that wants one project's terminal therefore pages to it rather than opening
 * it.
 *
 * It starts from the *first* page and works forward, because the page a page
 * loads on is remembered per browser and shared between tabs: a second tab
 * opens wherever the first one was left, and a search that only went forwards
 * from there would miss everything before it.
 */
export async function awaitProject(page, entry) {
  const panel = panelOf(page, entry)

  for (let attempt = 0; attempt < 20; attempt += 1) {
    const previous = page.getByRole('button', { name: 'Previous page' })
    if ((await previous.count()) === 0 || (await previous.isDisabled())) break
    await previous.click()
    await page.waitForTimeout(200)
  }

  for (let attempt = 0; attempt < 20; attempt += 1) {
    if ((await panel.count()) > 0) return panel
    const next = page.getByRole('button', { name: 'Next page' })
    if ((await next.count()) === 0 || (await next.isDisabled())) break
    await next.click()
    await page.waitForTimeout(400)
  }

  await page.waitForSelector(`.panel--project[data-project-id="${entry.id}"]`, {
    state: 'attached',
    timeout: PAGE_TIMEOUT_MS,
  })
  return panel
}

/** Every project panel on the page, in the order they are drawn. */
export function panels(page) {
  return page.locator('.panel--project')
}

/** The badge that says who is in charge of a project's terminal. */
export function controlBadge(page, entry) {
  return panelOf(page, entry).locator('[data-testid="terminal-control"]')
}

/**
 * takeControl makes this browser the controller of one project's terminal.
 *
 * Phase 6 made the keyboard something a client asks for. A page that opens a
 * project is a viewer, and it stays one - no keystroke reaches the pty, no
 * resize is sent, the Prompt Bar is disabled - until it asks and is given the
 * lease. Every suite written before that phase assumes typing works on arrival,
 * and this is what they call to get back to the state they were written
 * against.
 *
 * It presses the button a person would press rather than reaching past it into
 * the client, so what it proves is that the button works. The viewer case, and
 * the handover between two devices, is what `controller.mjs` is for; this
 * helper is the one line that keeps the rest of the suites about terminals.
 */
export async function takeControl(page, entry) {
  const badge = controlBadge(page, entry)
  const mine = async () => ((await badge.textContent().catch(() => '')) ?? '').startsWith('You control')

  // The badge appears with the first roster, which is sent before the first
  // frame of the terminal - so waiting for it here costs nothing on the path
  // where control is already held, and is what makes the button below exist.
  await waitFor(async () => (await badge.count()) > 0, 10_000)
  if (await mine()) return true

  const button = panelOf(page, entry).getByRole('button', { name: 'Request control' })
  if ((await button.count()) === 0) return false
  await button.first().click()

  return waitFor(mine, 10_000)
}

/**
 * releaseControl gives up the lease this browser is holding.
 *
 * It exists because Phase 6 made a lease exclusive, and two tabs of one browser
 * are two clients: the second is a viewer even on the machine the first is
 * typing from. A suite that wants a second tab to be able to type has to have
 * the first give it up, and pressing the button is how that is done - closing
 * the tab instead would leave the lease suspended for the grace period, which
 * is a different behaviour and would make the wait below a race against a timer
 * rather than a test of anything.
 */
export async function releaseControl(page, entry) {
  const badge = controlBadge(page, entry)
  const mine = async () => ((await badge.textContent().catch(() => '')) ?? '').startsWith('You control')

  if (!(await mine())) return true
  const button = panelOf(page, entry).getByRole('button', { name: 'Release control' })
  if ((await button.count()) === 0) return false
  await button.first().click()

  return waitFor(async () => !(await mine()), 10_000)
}

/** The text xterm has actually painted, row by row, trimmed. */
export async function rendered(page, selector = '.xterm-rows') {
  return page.evaluate((sel) => {
    return Array.from(document.querySelectorAll(`${sel} > div`))
      .map((row) => row.textContent.replace(/ /g, ' ').replace(/\s+$/, ''))
      .join('\n')
      .replace(/\n+$/, '')
  }, selector)
}

/** Non-empty rendered rows, which is what "the terminal is showing" means. */
export async function renderedLines(page, selector) {
  return (await rendered(page, selector)).split('\n').filter((line) => line.trim() !== '')
}

/** The painted text of one panel, which is how isolation is checked. */
export async function panelText(page, entry) {
  return page.evaluate((id) => {
    const panel = document.querySelector(`.panel--project[data-project-id="${id}"]`)
    if (!panel) return ''
    return Array.from(panel.querySelectorAll('.xterm-rows > div'))
      .map((row) => row.textContent)
      .join('\n')
  }, entry.id)
}

/**
 * normalize makes two texts comparable the way a person would compare them:
 * trailing space trimmed, blank rows dropped.
 */
export function normalize(text) {
  return text
    .split('\n')
    .map((line) => line.replace(/ /g, ' ').replace(/\s+$/, ''))
    .filter((line) => line.trim() !== '')
    .join('\n')
}

/** lines is `normalize` without the join, for comparisons by row. */
export function lines(text) {
  return text
    .split('\n')
    .map((line) => line.replace(/ /g, ' ').replace(/\s+$/, ''))
    .filter((line) => line.trim() !== '')
}

/** waitFor polls until `probe` is true, or gives up. */
export async function waitFor(probe, timeoutMs = 20_000, intervalMs = 250) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    if (await probe()) return true
    if (Date.now() > deadline) return false
    await new Promise((resolve) => setTimeout(resolve, intervalMs))
  }
}
