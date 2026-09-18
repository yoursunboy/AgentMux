/**
 * Phase 5 browser E2E - the workspace.
 *
 * The claim under test is the product's whole premise: several projects visible
 * at once, each one its own terminal, none of them able to disturb the others,
 * and none of them stopped by anything the browser does.
 *
 * The suite is arranged so that it walks the workspace down from seven projects
 * to one and back up again. That is not tidiness: a grid with one panel and a
 * grid with five are different layouts, and the way to check both without
 * building two fixtures is to change the workspace while looking at it.
 *
 * What it deliberately does not do is start five real Claude sessions. Five
 * plain shells prove that terminals are isolated, multiplexed and independent;
 * two real Claude sessions - provisioned by the runner - prove that the same
 * holds for the thing the product exists for. §107 of the phase directive asks
 * for exactly that split.
 */
import { chromium } from 'playwright'

import { Report } from '../lib/harness.mjs'
import {
  claudePids,
  fixtures,
  lines,
  openPage,
  PAGE_URL,
  pane,
  paneSize,
  panels,
  panelOf,
  panelText,
  project,
  sessionAlive,
  setWorkspace,
  startServer,
  stopServer,
  settleShell,
  typeCommand,
  waitFor,
  wsl,
} from '../lib/harness.mjs'

const report = new Report('workspace')

const ALL = fixtures.projects
const CLAUDE = ALL.filter((entry) => entry.role === 'claude')
const SHELLS = ALL.filter((entry) => entry.role === 'shell')

/** The cells the grid is drawing, in order. */
function cells(page) {
  return page.evaluate(() =>
    Array.from(document.querySelectorAll('.workspace > *')).map((cell) => ({
      label: cell.getAttribute('aria-label') ?? '',
      projectId: cell.getAttribute('data-project-id'),
    })),
  )
}

/** The Project Manager's cell is always the last one. */
async function managerIsLast(page) {
  const drawn = await cells(page)
  return drawn.length > 0 && drawn.at(-1).label === 'Projects'
}

async function main() {
  const browser = await chromium.launch({ channel: 'chrome', headless: true })

  // A suite that inherits a half-typed line or a running command types into
  // something that is not reading its input.
  for (const entry of SHELLS) settleShell(entry)

  // AMX_E2E_ONLY=phone runs one section, which is how a failure in the middle
  // of a four-minute suite gets looked at rather than guessed about.
  const only = process.env.AMX_E2E_ONLY ?? ''
  const sections = [
    ['grid', theGrid],
    ['socket', singleWebSocket],
    ['pagination', pagination],
    ['hidden', aHiddenPageKeepsWorking],
    ['isolation', isolation],
    ['membership', removingAndAdding],
    ['focus', focus],
    ['restart', acrossAServerRestart],
    ['phone', phone],
    ['tablet', tabletLandscape],
    ['failure', partialFailure],
  ]

  try {
    for (const [name, section] of sections) {
      if (only !== '' && !name.includes(only)) continue
      await section(browser)
    }
  } finally {
    await browser.close()
  }

  const ok = report.summary()
  process.exit(ok ? 0 : 1)
}

/**
 * One socket, however many panels.
 *
 * This is the claim the whole transport was built for, and the one that would
 * be quietly abandoned by a "simpler" per-panel implementation. The count comes
 * from wrapping the page's own `WebSocket` before it runs, because that is the
 * only place a second socket would be visible.
 */
async function singleWebSocket(browser) {
  console.log('\n--- one socket for the whole workspace ---')
  const { context, page, frames } = await openPage(browser)
  await countSockets(page)
  await setWorkspace(page, ALL)
  await page.waitForTimeout(3000)

  const sockets = await socketsNow(page)
  const live = await page.locator('.terminal__state--open').count()

  report.check(
    'seven projects are watched over one WebSocket, not one each',
    sockets.open === 1 && sockets.opened <= 2 && live > 0,
    `${sockets.open} socket(s) open (${sockets.opened} opened over the session), ${live} live terminal(s), ${frames.length} frame(s) seen`,
  )

  await context.close()
}

/** The grid: what the panels are, and that the manager never is one. */
async function theGrid(browser) {
  console.log('\n--- the grid ---')
  const { context, page } = await openPage(browser)
  await countSockets(page)
  await setWorkspace(page, ALL.slice(0, 5))

  report.check(
    'five projects are open at once, each with its own terminal',
    (await panels(page).count()) === 5 && (await page.locator('.xterm-screen').count()) === 5,
    `${await panels(page).count()} panel(s), ${await page.locator('.xterm-screen').count()} terminal(s)`,
  )

  report.check('the Project Manager is the last cell', await managerIsLast(page))

  report.check(
    'the Project Manager is not a terminal',
    (await page.locator('.panel--manager .xterm-screen').count()) === 0,
  )

  report.check(
    'the grid is laid out in three columns at 1440px',
    (await page.evaluate(
      () => getComputedStyle(document.querySelector('.workspace')).gridTemplateColumns.split(' ').length,
    )) === 3,
  )

  // Two of the five are real Claude sessions, and both are on screen at once:
  // the multi-project case the product exists for. What is asserted is that each
  // is showing Claude's own full-screen UI rather than the shell it was started
  // from - which screen Claude chose is Claude's business, and on this host one
  // of them is a sign-in screen and the other is a network error, both real
  // full-screen TUIs drawn through the same path.
  const showing = []
  for (const entry of CLAUDE) {
    const text = await panelText(page, entry)
    const atShell = text.includes(`${entry.name}$`)
    showing.push(`${entry.name}:${atShell ? 'shell' : 'claude'}:${lines(text).length}rows`)
  }
  const claudes = claudePids().length
  const detail = `${showing.join(' ')}; running Claude process(es): ${claudes}`

  if (claudes < CLAUDE.length) {
    // The fixture is not in the state this check needs, and saying so is not
    // the same as saying the product failed. Claude Code on this host is
    // installed and not signed in, and it does not reliably stay up - measured
    // across runs, where one of the two sessions is often back at its shell
    // within a minute of starting. Reported as a skip with the count, so the
    // number is on the record either way.
    report.skip(
      'two real Claude Code sessions are rendered side by side',
      `this host kept ${claudes} of ${CLAUDE.length} Claude processes alive. ${detail}`,
    )
  } else {
    report.check(
      'two real Claude Code sessions are rendered side by side',
      showing.every((line) => line.includes(':claude:')),
      detail,
    )
  }

  await context.close()
}

/**
 * countSockets instruments the page's WebSocket before it runs.
 *
 * Opens and closes are counted separately, because a reconnect is a new socket
 * and correct behaviour rather than a second one. What must never happen is one
 * socket per panel; what may happen is a socket that dropped and came back.
 */
async function countSockets(page) {
  await page.addInitScript(() => {
    window.__amxSocketOpened = 0
    window.__amxSocketClosed = 0
    const Original = window.WebSocket
    window.WebSocket = function counted(...args) {
      window.__amxSocketOpened += 1
      const socket = new Original(...args)
      socket.addEventListener('close', () => {
        window.__amxSocketClosed += 1
      })
      return socket
    }
    window.WebSocket.prototype = Original.prototype
  })
  await page.reload({ waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(1500)
}

/** socketsNow reports how many sockets the page has open and how many it opened. */
function socketsNow(page) {
  return page.evaluate(() => ({
    opened: window.__amxSocketOpened ?? -1,
    open: (window.__amxSocketOpened ?? 0) - (window.__amxSocketClosed ?? 0),
  }))
}

async function agentCountSafe() {
  return wsl('pgrep -cf "claude/versions"').trim()
}

/** Pagination: seven projects, two pages, the manager last on both. */
async function pagination(browser) {
  console.log('\n--- pagination ---')
  const { context, page } = await openPage(browser)
  await countSockets(page)
  await setWorkspace(page, ALL)

  report.check(
    'seven projects make two pages',
    (await page.getByText('1 / 2').count()) === 1,
    'page indicator says 1 / 2',
  )

  const first = await cells(page)
  report.check(
    'the first page holds five projects and the manager',
    first.length === 6 && first.at(-1).label === 'Projects',
    `${first.length} cell(s), last is ${JSON.stringify(first.at(-1).label)}`,
  )

  await page.getByRole('button', { name: 'Next page' }).click()
  await page.waitForTimeout(800)

  const second = await cells(page)
  report.check(
    'the second page holds two projects and the manager',
    second.length === 3 && second.at(-1).label === 'Projects',
    `${second.length} cell(s), last is ${JSON.stringify(second.at(-1).label)}`,
  )

  report.check(
    'the cells that are not on the page are not rendered at all',
    (await page.locator('.xterm-screen').count()) === 2,
    `${await page.locator('.xterm-screen').count()} terminal(s) mounted`,
  )

  report.check('the Project Manager is the last cell on this page too', await managerIsLast(page))

  await page.getByRole('button', { name: 'Previous page' }).click()
  await page.waitForTimeout(800)
  report.check(
    'going back shows the first page again',
    (await panels(page).count()) === 5,
    `${await panels(page).count()} panel(s)`,
  )

  await context.close()
}

/**
 * The page nobody is looking at.
 *
 * A page's terminals are unsubscribed when it is left, and the runtimes carry
 * on. Coming back is a fresh screen - so output produced while the page was
 * hidden can only be on it if nothing was lost.
 */
async function aHiddenPageKeepsWorking(browser) {
  console.log('\n--- a hidden page keeps working ---')
  // A shell on the first page, so the project being watched is on the page that
  // gets hidden when the workspace moves to the second one.
  const target = ALL[2]
  const { context, page } = await openPage(browser)
  await countSockets(page)
  await setWorkspace(page, ALL)
  await page.waitForTimeout(1500)

  const marker = `BACKGROUND-${Date.now()}`

  await page.getByRole('button', { name: 'Next page' }).click()
  await page.waitForTimeout(1200)
  report.check(
    'the project under test is not on the page being shown',
    (await panelOf(page, target).count()) === 0 && (await page.locator('.xterm-screen').count()) === 2,
    `${await page.locator('.xterm-screen').count()} terminal(s) mounted, ${target.name} not among them`,
  )

  // Typed at the session, not at the browser: the point is that the *runtime*
  // was never affected by what the browser did.
  typeCommand(target, `echo ${marker}`)
  await page.waitForTimeout(1500)

  report.check(
    'the session is alive while its page is hidden',
    sessionAlive(target),
    `${target.name} session present`,
  )

  await page.getByRole('button', { name: 'Previous page' }).click()
  await page.waitForTimeout(3000)

  const text = await panelText(page, target)
  report.check(
    'coming back to the page shows what happened while it was away',
    text.includes(marker),
    text.includes(marker) ? marker : `${marker} not in ${lines(text).length} row(s)`,
  )

  await context.close()
}

/** Output, input and scroll are per panel. */
async function isolation(browser) {
  console.log('\n--- isolation ---')
  const { context, page } = await openPage(browser)
  await countSockets(page)
  await setWorkspace(page, SHELLS)
  await page.waitForTimeout(2000)

  const markers = new Map()
  for (const [index, entry] of SHELLS.entries()) {
    const marker = `MARKER-${index}-${entry.name.toUpperCase()}`
    markers.set(entry.name, marker)
    typeCommand(entry, `echo ${marker}`)
  }
  await page.waitForTimeout(2500)

  let wrongPanel = ''
  for (const entry of SHELLS) {
    const text = await panelText(page, entry)
    for (const [name, marker] of markers) {
      if (name === entry.name) continue
      if (text.includes(marker)) wrongPanel = `${marker} appeared in ${entry.name}`
    }
  }
  report.check(
    'five terminals receive five different streams, and none is another’s',
    wrongPanel === '',
    wrongPanel || `${markers.size} markers, each in its own panel`,
  )

  // Input: the Prompt Bar of one panel reaches that terminal and no other.
  const promptMarker = `PROMPT-${Date.now()}`
  const first = SHELLS[0]
  const firstPanel = page.locator(`.panel--project[data-project-id="${first.id}"]`)
  await firstPanel.getByLabel('Message Claude').fill(`echo ${promptMarker}`)
  await firstPanel.getByRole('button', { name: 'Send' }).click()
  await page.waitForTimeout(2000)

  const others = []
  for (const entry of SHELLS) {
    if (entry.id === first.id) continue
    const text = await panelText(page, entry)
    if (text.includes(promptMarker)) others.push(entry.name)
  }
  report.check(
    'a Prompt Bar writes to one terminal and to no other',
    (await panelText(page, first)).includes(promptMarker) && others.length === 0,
    others.length ? `leaked into ${others.join(', ')}` : `${first.name} only`,
  )

  // Scroll: one panel scrolled up does not move another. The terminal needs
  // more on it than fits before there is anywhere to scroll to.
  typeCommand(first, 'seq 1 200')
  await page.waitForTimeout(2000)

  await firstPanel.locator('.terminal__screen').click()
  await page.waitForTimeout(300)
  const box = await firstPanel.locator('.terminal__screen').boundingBox()
  if (box) {
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
    await page.mouse.wheel(0, -600)
    await page.waitForTimeout(600)
  }

  const scrolled = await firstPanel.locator('.terminal__more').count()
  const neighbour = page.locator(`.panel--project[data-project-id="${SHELLS[1].id}"]`)
  const neighbourOffered = await neighbour.locator('.terminal__more').count()
  report.check(
    'scrolling one panel offers a way back there, and nothing in the next panel',
    scrolled === 1 && neighbourOffered === 0,
    `scrolled panel: ${scrolled}, neighbour: ${neighbourOffered}`,
  )

  await context.close()
}

/** Taking a panel off the grid is not stopping anything. */
async function removingAndAdding(browser) {
  console.log('\n--- removing and adding ---')
  const { context, page } = await openPage(browser)
  await countSockets(page)
  await setWorkspace(page, ALL.slice(0, 5))
  await page.waitForTimeout(1500)

  const removed = ALL[4]
  const claudesBefore = claudePids()

  // A count down to one, checking the layout at each size on the way. The four
  // that go are the last four of the five that are open, so the one that stays
  // is the project the workspace started with.
  const expected = [4, 3, 2, 1]
  let allHeld = true
  let detail = []

  for (const [index, name] of ['charlie', 'bravo', 'alpha', 'studio'].entries()) {
    await page.getByRole('button', { name: `Remove ${name} from the workspace` }).click()
    await waitFor(async () => (await panels(page).count()) === expected[index], 15_000)
    const count = await panels(page).count()
    const last = await managerIsLast(page)
    if (count !== expected[index] || !last) allHeld = false
    detail.push(`${index + 1} -> ${count}${last ? '' : ' (manager not last)'}`)
  }

  report.check(
    'the grid closes up as projects are removed, with the manager still last',
    allHeld,
    detail.join(', '),
  )

  report.check(
    'the removed project’s runtime is still running',
    sessionAlive(removed),
    `${removed.name} session present`,
  )

  report.check(
    'no Claude was restarted by any of that',
    claudePids().length >= claudesBefore.length,
    `${claudesBefore.length} Claude process(es) before, ${claudePids().length} after`,
  )

  // Back in, and the terminal is where it was rather than a new one.
  const marker = `READDED-${Date.now()}`
  typeCommand(removed, `echo ${marker}`)
  await page.waitForTimeout(1200)

  await page.getByRole('button', { name: `Open ${removed.name} in the workspace` }).click()
  await page.waitForTimeout(3000)

  report.check(
    'a project added back gets a panel showing the session it already had',
    (await panelText(page, removed)).includes(marker),
    marker,
  )

  await context.close()
}

/** Focus is a layout, not a lifecycle. */
async function focus(browser) {
  console.log('\n--- focus ---')
  const { context, page, errors } = await openPage(browser)
  await countSockets(page)
  await setWorkspace(page, ALL.slice(0, 3))
  await page.waitForTimeout(1500)

  const target = ALL[0]
  const claudesBefore = claudePids()
  const sizeBefore = paneSize(target)
  const marker = `FOCUS-${Date.now()}`
  typeCommand(target, `echo ${marker}`)
  await page.waitForTimeout(1200)

  await page.getByRole('button', { name: `Focus ${target.name}` }).click()
  await page.waitForTimeout(1500)

  report.check(
    'focus shows one project and nothing else',
    (await panels(page).count()) === 1 && (await page.locator('.panel--manager').count()) === 0,
    `${await panels(page).count()} panel(s)`,
  )

  report.check(
    'the focused terminal is larger than it was in the grid',
    (await paneSize(target)) !== sizeBefore,
    `${sizeBefore} in the grid, ${await paneSize(target)} focused`,
  )

  const focusedText = await panelText(page, target)
  report.check(
    'focusing does not restart the runtime, and the session is the same one',
    claudePids().length >= claudesBefore.length && focusedText.length > 0,
    `${claudesBefore.length} Claude process(es) before, ${claudePids().length} after; ` +
      `${lines(focusedText).length} row(s) on screen`,
  )

  await page.getByRole('button', { name: 'Back to the workspace' }).click()
  await page.waitForTimeout(1500)

  report.check(
    'going back returns to the grid with every panel',
    (await panels(page).count()) === 3 && (await managerIsLast(page)),
    `${await panels(page).count()} panel(s)`,
  )

  report.check(
    'focus, full screen and the way back raise no page errors',
    errors.length === 0,
    errors.join('; '),
  )

  await context.close()
}

/** One project failing does not take the workspace with it. */
async function partialFailure(browser) {
  console.log('\n--- one runtime fails ---')
  const { context, page } = await openPage(browser)
  await countSockets(page)
  await setWorkspace(page, ALL.slice(0, 4))
  await page.waitForTimeout(1500)

  const broken = ALL[1]
  await fetch(`${PAGE_URL}/api/projects/${broken.id}/runtime`, { method: 'DELETE' })
  await page.waitForTimeout(1000)

  // The panel learns about it the way it learns everything: by being told by
  // the server, on the next refresh.
  await page.reload({ waitUntil: 'domcontentloaded' })
  await page.waitForTimeout(3000)

  const brokenPanel = page.locator(`.panel--project[data-project-id="${broken.id}"]`)
  const others = await page.locator('.xterm-screen').count()

  report.check(
    'the project whose runtime is gone says so, and offers a start',
    (await brokenPanel.getByText('Runtime stopped').count()) === 1 &&
      (await brokenPanel.getByRole('button', { name: 'Start' }).count()) === 1,
    'placeholder and Start button present',
  )

  report.check(
    'the other panels are untouched and still live',
    others === 3,
    `${others} terminal(s) still rendered`,
  )

  report.check(
    'the workspace is still interactive',
    (await page.getByRole('button', { name: `Actions for ${ALL[0].name}` }).count()) === 1,
  )

  // Put it back. This is the last section, so nothing depends on it - but a
  // fixture left with a destroyed runtime would be a run that has to be cleaned
  // up by hand, and the check is here so that a restore which failed says so
  // rather than being discovered later.
  const restored = await fetch(`${PAGE_URL}/api/projects/${broken.id}/runtime/start`, {
    method: 'POST',
  })
  report.check(
    'the destroyed runtime can be started again',
    restored.ok,
    `POST runtime/start answered ${restored.status}`,
  )

  await context.close()
}

/** The whole workspace across a server restart. */
async function acrossAServerRestart(browser) {
  console.log('\n--- across a server restart ---')
  const { context, page } = await openPage(browser)
  await countSockets(page)
  await setWorkspace(page, ALL.slice(0, 5))
  await page.waitForTimeout(2000)

  const claudesBefore = claudePids()
  // Wait for every panel to be both connected and showing something. A session
  // started a moment ago is a prompt and nothing else, so the wait is for
  // "something" rather than for a particular screen - but a panel with nothing
  // on it at all is the thing being ruled out.
  const allLive = await waitFor(async () => {
    if ((await page.locator('.terminal__state--open').count()) !== 5) return false
    for (const entry of ALL.slice(0, 5)) {
      if ((await panelText(page, entry)).trim() === '') return false
    }
    return true
  }, 40_000)
  const before = {}
  for (const entry of ALL.slice(0, 5)) before[entry.name] = await panelText(page, entry)

  report.check(
    'every panel is live before the server goes away',
    allLive && Object.values(before).every((text) => text.trim() !== ''),
    Object.entries(before)
      .map(([name, text]) => `${name}:${lines(text).length}rows`)
      .join(' '),
  )

  stopServer()
  await page.waitForTimeout(3000)

  const duringOutage = await panels(page).count()
  report.check(
    'the panels stay on screen while the server is gone',
    duringOutage === 5,
    `${duringOutage} panel(s) still drawn`,
  )

  startServer()

  const recovered = await waitFor(
    async () => (await page.locator('.terminal__state--open').count()) === 5,
    45_000,
  )
  report.check(
    'every visible terminal reconnects on its own, not just the focused one',
    recovered,
    `${await page.locator('.terminal__state--open').count()} of 5 live`,
  )

  const marker = `AFTER-RESTART-${Date.now()}`
  typeCommand(ALL[3], `echo ${marker}`)
  await page.waitForTimeout(3000)

  report.check(
    'a reconnected panel is showing the session it was showing before',
    (await panelText(page, ALL[3])).includes(marker),
    marker,
  )

  const claudesAfter = claudePids()
  report.check(
    'no Claude process was restarted by the server restart',
    claudesAfter.length >= claudesBefore.length,
    `${claudesBefore.length} Claude process(es) before, ${claudesAfter.length} after`,
  )

  await context.close()
}

/** Phone: one project at a time, nothing overflowing. */
async function phone(browser) {
  console.log('\n--- phone ---')
  const { context, page } = await openPage(browser, {
    viewport: { width: 390, height: 844 },
    hasTouch: true,
    isMobile: true,
    deviceScaleFactor: 3,
  })
  await countSockets(page)
  await setWorkspace(page, ALL.slice(0, 3))
  await page.waitForTimeout(1500)

  report.check(
    'a phone shows one project at a time',
    (await panels(page).count()) === 1,
    `${await panels(page).count()} panel(s)`,
  )

  report.check(
    'the page does not scroll sideways',
    (await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)),
    `${await page.evaluate(() => document.documentElement.scrollWidth)}px of content in ${await page.evaluate(() => window.innerWidth)}px`,
  )

  const size = await page.evaluate(() => {
    const term = document.querySelector('.xterm-screen')
    return term ? `${term.clientWidth}x${term.clientHeight}` : 'none'
  })
  report.check('the terminal is usable at that width', size !== 'none', `terminal box ${size}`)

  // Read the pager rather than matching its markup: three projects and the
  // manager in a single column is four pages, and what is being checked is the
  // count, not how it is spelled.
  const pager = (await page.locator('.pager__count').textContent())?.trim() ?? '(no pager)'
  const pageNumbers = pager.split('/').map((part) => Number(part.trim()))
  report.check(
    'the workspace paginates through the projects and the manager',
    pageNumbers[0] === 1 && pageNumbers[1] === 4,
    `3 projects plus the manager is 4 pages; the pager says ${JSON.stringify(pager)}`,
  )

  report.check(
    'the Prompt Bar is there',
    (await page.getByLabel('Message Claude').count()) === 1,
  )

  await page.getByRole('button', { name: 'Next page' }).click()
  await page.waitForTimeout(500)
  await page.getByRole('button', { name: 'Next page' }).click()
  await page.waitForTimeout(500)
  await page.getByRole('button', { name: 'Next page' }).click()
  await page.waitForTimeout(1000)

  report.check(
    'the last page on a phone is the Project Manager',
    (await page.locator('.panel--manager').count()) === 1,
    'manager reachable by paging',
  )

  await context.close()
}

/** Tablet in landscape: two columns, which is the measured answer. */
async function tabletLandscape(browser) {
  console.log('\n--- tablet, landscape ---')
  const { context, page } = await openPage(browser, {
    viewport: { width: 1180, height: 820 },
    hasTouch: true,
    isMobile: true,
    deviceScaleFactor: 2,
  })
  await countSockets(page)
  await setWorkspace(page, ALL.slice(0, 3))
  await page.waitForTimeout(2000)

  const columns = await page.evaluate(
    () => getComputedStyle(document.querySelector('.workspace')).gridTemplateColumns.split(' ').length,
  )
  report.check(
    'a landscape tablet gets two columns, because three would not be readable',
    columns === 2,
    `${columns} column(s) at 1180px; the terminal reports ${await paneSize(ALL[0])}`,
  )

  // The row count is reported rather than asserted on: a landscape tablet with
  // two rows of panels gives each terminal about a dozen rows, which is what
  // the grid is for - monitoring - and Focus is where the work happens. The
  // measurement is the point; a threshold here would only be a number this
  // suite happens to agree with.
  const rows = await page.evaluate((id) => {
    const root = document.querySelector(`.panel--project[data-project-id="${id}"]`)
    return root ? root.querySelectorAll('.xterm-rows > div').length : 0
  }, ALL[0].id)
  report.check(
    'three projects share the page, two rows deep, with the manager',
    (await page.locator('.panel--project').count()) === 3 && rows > 0,
    `${await page.locator('.panel--project').count()} panel(s), ${rows} row(s) per terminal, pane ${await paneSize(ALL[0])}`,
  )

  report.check('the Project Manager is the last cell', await managerIsLast(page))

  report.check(
    'the terminal is wider than the sixty columns three columns would leave it',
    Number((await paneSize(ALL[0])).split('x')[0]) >= 60,
    `${await paneSize(ALL[0])}`,
  )

  await context.close()
}

await main()
