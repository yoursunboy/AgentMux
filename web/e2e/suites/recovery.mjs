/**
 * Phase 4 browser E2E - the recovery half.
 *
 * Everything a terminal is worth having for is in this file: that the view can
 * be lost and come back without the work being lost with it. The session runs
 * in tmux, so the thing to prove is that Claude's own process is untouched by
 * anything the browser or the server does.
 *
 * The reconnect check is made to mean something by writing to the session while
 * the browser is disconnected. Text that appeared only during the outage can
 * only be on screen afterwards if a fresh screen was taken, which is precisely
 * the claim being tested.
 */
import { chromium } from 'playwright'
import { execFileSync } from 'node:child_process'
import {
  awaitProject,
  claudePids,
  fixtures,
  PAGE_URL,
  startServer,
  stopServer,
} from '../lib/harness.mjs'



/** The fixture projects this suite is about, by the role they play. */
const CLAUDE = fixtures.projects.find((entry) => entry.role === 'claude')
const SHELL = fixtures.projects.filter((entry) => entry.role === 'shell')[0]
const URL = PAGE_URL
const MARKER = 'RECONNECT-MARKER-42'

/** Comparable lines: trailing space trimmed, blank rows dropped. */
function lines(text) {
  return text
    .split('\n')
    .map((line) => line.replace(/ /g, ' ').replace(/\s+$/, ''))
    .filter((line) => line.trim() !== '')
}

const results = []
function check(name, ok, detail) {
  results.push({ name, ok, detail })
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? `  -- ${detail}` : ''}`)
}

// `|| true` throughout: these are all read-only probes, and a grep that finds
// nothing or a curl that cannot connect exits non-zero. That is an answer, not
// a reason to throw.
function wsl(script) {
  return execFileSync('wsl', ['-d', 'Ubuntu-24.04', 'bash', '-lc', `${script} || true`], {
    encoding: 'utf8',
    env: { ...process.env, MSYS_NO_PATHCONV: '1' },
  })
}
const sock = (entry) => `${fixtures.dataDir}/tmux/${entry.id}.sock`
const pane = (p) => wsl(`tmux -S ${sock(p)} capture-pane -p -t amx-${p.id}`)
const agentCount = () => claudePids().length
const sessionAlive = (p) =>
  wsl(`tmux -S ${sock(p)} has-session -t amx-${p.id} >/dev/null 2>&1; echo $?`).trim() === '0'

async function rendered(page) {
  return page.evaluate(() =>
    Array.from(document.querySelectorAll('.xterm-rows > div'))
      .map((row) => row.textContent.replace(/ /g, ' ').replace(/\s+$/, ''))
      .join('\n')
      .replace(/\n+$/, ''),
  )
}
const statusText = async (page) =>
  (await page.locator('.terminal__state').first().textContent()) ?? ''

/** Waits for a predicate over the page, polling, and returns whether it held. */
async function waitFor(page, predicate, timeoutMs = 25000) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (await predicate()) return true
    await page.waitForTimeout(400)
  }
  return false
}

async function openPage(browser) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  const page = await context.newPage()
  const errors = []
  page.on('pageerror', (e) => errors.push(e.message))
  await page.goto(URL, { waitUntil: 'domcontentloaded' })
  await awaitProject(page, CLAUDE)
  await page.waitForSelector('.xterm-screen', { timeout: 20000 })
  await page.waitForTimeout(2500)
  return { context, page, errors }
}

async function main() {
  const browser = await chromium.launch({ channel: 'chrome', headless: true })
  // Counted rather than identified by pid: this host's Claude re-executes
  // itself while it starts, so a pid taken a moment ago is not the pid it has
  // now. What these checks are about is whether AgentMux restarted anything.
  const claudesAtStart = agentCount()
  console.log(`Claude process(es) at the start: ${claudesAtStart}`)

  const { context, page, errors } = await openPage(browser)
  const firstPaint = await rendered(page)
  check(
    'the terminal is live before anything is done to it',
    (await statusText(page)).includes('Live'),
    await statusText(page),
  )

  // ------------------------------------------------------- 1. reload the page
  {
    await page.reload({ waitUntil: 'domcontentloaded' })
    await page.waitForSelector('.xterm-screen', { timeout: 20000 })
    await page.waitForTimeout(2500)
    const afterReload = await rendered(page)
    check(
      'reloading the page repaints the same screen from a fresh snapshot',
      afterReload === firstPaint,
      `${afterReload.split('\n').length} rows, identical: ${afterReload === firstPaint}`,
    )
    check(
      'reloading the page does not restart Claude',
      agentCount() >= claudesAtStart,
      `${claudesAtStart} Claude process(es) before, ${agentCount()} after`,
    )
  }

  // ------------------------------------------- 2. the socket drops underneath
  {
    // Two tabs, one per project, because they can prove different things. The
    // shell echoes what is written to it, so it is the one that can show text
    // arriving only during the outage. Claude's TUI does not echo, so it is the
    // one that shows a screen still matching tmux afterwards.
    const shellTab = await context.newPage()
    await shellTab.goto(URL, { waitUntil: 'domcontentloaded' })
    await awaitProject(shellTab, SHELL)
    await shellTab.waitForSelector('.xterm-screen', { timeout: 20000 })
    await shellTab.waitForTimeout(2500)

    const claudeTab = await context.newPage()
    await claudeTab.goto(URL, { waitUntil: 'domcontentloaded' })
    await awaitProject(claudeTab, CLAUDE)
    await claudeTab.waitForSelector('.xterm-screen', { timeout: 20000 })
    await claudeTab.waitForTimeout(2500)

    check(
      'both terminals are live before the outage',
      (await statusText(shellTab)).includes('Live') && (await statusText(claudeTab)).includes('Live'),
      `${await statusText(shellTab)} / ${await statusText(claudeTab)}`,
    )

    // The server going away is the honest way to lose a socket: it is what a
    // deploy, a crash or a laptop waking up looks like to the browser. It is
    // stopped by port rather than by name, because `pkill -f` matches the
    // command line of the shell running it - measured, and it looks like a
    // suite crashing for no reason.
    stopServer()
    const sawOutage = await waitFor(shellTab, async () =>
      /Reconnecting|Connecting|Could not/.test(await statusText(shellTab)),
    )
    check(
      'a dropped socket is reported rather than shown as a live terminal',
      sawOutage,
      await statusText(shellTab),
    )

    // Written straight into tmux, with no AgentMux server in the picture at
    // all. That the write lands is itself the proof that the session does not
    // depend on the server that was watching it.
    wsl(`tmux -S ${sock(SHELL)} send-keys -t amx-${SHELL.id} 'echo ${MARKER}' Enter`)
    await shellTab.waitForTimeout(1000)
    check(
      'the session keeps running, and keeps being usable, with the server down',
      pane(SHELL).includes(MARKER),
      '',
    )
    check(
      'Claude is not restarted by the server going away',
      agentCount() >= claudesAtStart,
      `${claudesAtStart} Claude process(es) before, ${agentCount()} after`,
    )

    startServer()
    const shellBack = await waitFor(shellTab, async () => (await statusText(shellTab)).includes('Live'))
    const claudeBack = await waitFor(claudeTab, async () =>
      (await statusText(claudeTab)).includes('Live'),
    )
    check(
      'both browsers reconnect on their own once the server is back',
      shellBack && claudeBack,
      `${await statusText(shellTab)} / ${await statusText(claudeTab)}`,
    )
    // Polled, not read once: the status flips to Live when the socket opens,
    // and the screen arrives in the frame after it.
    const shellCaughtUp = await waitFor(shellTab, async () =>
      (await rendered(shellTab)).includes(MARKER),
    )
    const shellPaint = await rendered(shellTab)
    check(
      'the reconnected terminal shows what happened while it was away',
      shellCaughtUp,
      `${shellPaint.split('\n').filter((l) => l.trim()).length} non-empty rows, marker present: ${shellPaint.includes(MARKER)}`,
    )

    const claudeCaughtUp = await waitFor(claudeTab, async () => {
      const painted = lines(await rendered(claudeTab))
      const held = lines(pane(CLAUDE))
      const shared = Math.min(painted.length, held.length)
      if (shared <= 5) return false
      let same = 0
      for (let i = 0; i < shared; i += 1) if (painted[i] === held[i]) same += 1
      return same >= shared - 2
    })
    const claudePaint = lines(await rendered(claudeTab))
    const claudePane = lines(pane(CLAUDE))
    const shared = Math.min(claudePaint.length, claudePane.length)
    let matching = 0
    for (let i = 0; i < shared; i += 1) if (claudePaint[i] === claudePane[i]) matching += 1
    check(
      'Claude’s screen after the reconnect still matches the pane',
      claudeCaughtUp,
      `${matching}/${shared} rows identical`,
    )
    check(
      'Claude is the same process through a server restart',
      agentCount() >= claudesAtStart,
      `${claudesAtStart} Claude process(es) before, ${agentCount()} after`,
    )
    check('the tmux session is still there after a server restart', sessionAlive(CLAUDE), '')

    // Typing after the reconnect is the other half of "recovered": a terminal
    // that paints but cannot be written to has not come back.
    // Found again rather than assumed still on screen: the tab may have been
    // paged away from while the outage was being watched, and "the terminal of
    // the project under test" is a question with an answer.
    await (await awaitProject(shellTab, SHELL)).locator('.terminal__screen').click()
    await shellTab.keyboard.press('Control+c')
    await shellTab.waitForTimeout(400)
    for (const ch of 'echo AFTER-RECONNECT\r') {
      if (ch === '\r') await shellTab.keyboard.press('Enter')
      else await shellTab.keyboard.type(ch)
      await shellTab.waitForTimeout(8)
    }
    await shellTab.waitForTimeout(1500)
    check('the reconnected terminal accepts input again', pane(SHELL).includes('AFTER-RECONNECT'), '')

    // And Claude's. It is at its sign-in screen here, because the CLI in this
    // environment is deliberately not logged in; what that screen does with a
    // typed character is put it in the paste field, which is as good a proof
    // that input arrived as the theme picker's arrow keys were.
    const before = pane(CLAUDE)
    await (await awaitProject(claudeTab, CLAUDE)).locator('.terminal__screen').click()
    // An arrow key rather than a letter. Claude's first screen on this host is
    // a menu - the sign-in choice or the theme picker - and a menu answers
    // arrows; a letter would be testing what one of Claude's screens does with
    // text, which is Claude's business.
    await claudeTab.keyboard.press('ArrowDown')
    await claudeTab.waitForTimeout(900)
    const typed = pane(CLAUDE) !== before
    check("Claude's own UI still responds to the reconnected browser", typed, '')

    await shellTab.close()
    await claudeTab.close()
  }

  // ---------------------------------------- 3. closing the browser changes nothing
  {
    const claudesDuring = agentCount()
    const paneDuring = lines(pane(CLAUDE)).join('\n')
    await context.close()
    check(
      'closing the browser leaves Claude running',
      agentCount() >= claudesDuring && agentCount() >= claudesAtStart,
      `${claudesDuring} during the outage, ${agentCount()} after`,
    )
    const paneAfter = lines(pane(CLAUDE)).join('\n')
    check(
      'and leaves the session itself intact',
      sessionAlive(CLAUDE) && paneAfter === paneDuring,
      paneAfter === paneDuring ? '' : `pane changed:\n${paneAfter.slice(-300)}`,
    )
  }

  // ------------------------------------------------- 4. the deleted debug API
  {
    const paths = [
      '/api/debug',
      '/api/debug/runtimes',
      '/api/debug/reconcile',
      `/api/debug/projects/${CLAUDE.id}/runtime/output`,
      `/api/debug/projects/${CLAUDE.id}/runtime/input`,
      `/api/debug/projects/${CLAUDE.id}/runtime/resize`,
    ]
    const codes = paths.map(
      (path) =>
        `${path}=${wsl(`curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8787${path}`).trim()}`,
    )
    check(
      'every /api/debug path answers 404 on a running server',
      codes.every((c) => c.endsWith('=404')),
      codes.join(' '),
    )

    const probe = `${fixtures.runDir}/SHOULD-NOT-EXIST`
    wsl(`rm -f ${probe}`)
    const post = wsl(
      `curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' ` +
        `-d '{"text":"touch ${probe}"}' ${PAGE_URL}/api/debug/projects/${CLAUDE.id}/runtime/input`,
    ).trim()
    const wrote = wsl(`test -e ${probe} && echo yes || echo no`).trim()
    check(
      'the removed input endpoint cannot write to a session',
      post === '404' && wrote === 'no',
      `POST returned ${post}, file created: ${wrote}`,
    )

    const unknown = wsl(
      "curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8787/api/not-a-thing",
    ).trim()
    check('an unknown API path is still a 404 JSON error, not the page', unknown === '404', unknown)
  }

  check('no uncaught page errors across the whole run', errors.length === 0, errors.join('; '))

  const failed = results.filter((r) => !r.ok)
  console.log(`\n${results.length - failed.length}/${results.length} checks passed`)
  await browser.close()
  if (failed.length) process.exit(1)
}

await main().catch((error) => {
  console.error(error)
  process.exit(1)
})
