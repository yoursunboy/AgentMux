/**
 * Phase 7.4B-2A browser E2E - the terminal inside the console.
 *
 * # What this suite is
 *
 * The console used to be a page of numbers: it said a runtime was up without
 * anything on it being up. This phase put the terminal itself on the card, so
 * the claim under test is that the thing on the card is the *real* terminal -
 * the same pty the workspace's panel shows and tmux holds - rather than a
 * convincing blank.
 *
 * The way to make that claim is to type into the pty from outside the browser
 * entirely, with a marker nobody could have predicted, and then read it off the
 * card. A card that fabricated output could not produce a string it was never
 * sent, and a card showing a stale screen would not show one typed after it
 * loaded.
 *
 * # "Open the project panel" in this page
 *
 * §17 of the phase brief asks the suite to open a project's panel. The console
 * has no such gesture: every registered project is a card with its terminal
 * already drawn, which is the whole point of the page - a person reads it rather
 * than operating it. So the step is the card's terminal being present and
 * live, and it is checked for a project whose runtime this suite started itself.
 *
 * # The one runtime this suite stops and starts
 *
 * The fixture starts every runtime before the suite runs, so a suite that only
 * read them would be testing the fixture. This one stops a project's runtime
 * through the API, waits for the console to notice by itself - the page polls,
 * and that poll is the thing being relied on - and starts it again. That is
 * §17's "create/start a runtime" against the only runtime lifecycle the product
 * has.
 *
 * # What is deliberately not here
 *
 * Whether the console can type. It can, from Phase 7.4B-2B on, and that is a
 * suite of its own - `controller-input` - with two browsers in it, because
 * "this client has the keyboard and that one does not" is not a question one
 * browser can be asked about itself.
 *
 * What is here is the half that is about a single page: a console that has
 * taken nothing is a viewer, and that is proved by absence - the pty never sees
 * the marker and no `input` frame is ever sent - which is why it is here rather
 * than in jsdom, where there are no frames to count. It is a weaker claim than
 * it was, and deliberately so: the point is no longer that there is no way to
 * ask, it is that opening a card does not ask. A console that claimed every
 * running project as it loaded would take the keyboard from whoever is working,
 * once per project, which is the failure this section is the guard against.
 */
import { chromium } from 'playwright'

import {
  PAGE_URL,
  Report,
  openPage,
  pane,
  paneSize,
  fixtures,
  sessionAlive,
  settleShell,
  typeCommand,
  waitFor,
} from '../lib/harness.mjs'

const report = new Report('dashboard-terminal')

/** The console's address, on the server's own origin. */
const CONSOLE_URL = new URL('/dashboard', PAGE_URL).toString()

/** A plain shell, so that what is typed into it echoes back. */
const ALPHA = fixtures.projects.find((entry) => entry.role === 'shell')

if (!ALPHA) throw new Error('the fixture has no shell project for this suite to watch')

// ---------------------------------------------------------------------------
// Reading the console

/** One project's card, addressed the way the markup names it. */
const card = (page, entry = ALPHA) =>
  page.locator(`.project-card[aria-label="${entry.name}"]`)

/** The card's terminal, which is what this phase added. */
const viewer = (page, entry = ALPHA) => card(page, entry).locator('.terminal-viewer')

/**
 * cardText is the text a card's terminal has actually painted.
 *
 * Read off `.xterm-rows` rather than off the element's text, because xterm
 * paints into rows of spans and the rows are the only place the characters are.
 */
function cardText(page, entry = ALPHA) {
  return page.evaluate((name) => {
    const found = document.querySelector(`.project-card[aria-label="${name}"]`)
    if (!found) return ''
    return Array.from(found.querySelectorAll('.xterm-rows > div'))
      .map((row) => row.textContent)
      .join('\n')
  }, entry.name)
}

/** The box the card's terminal is drawn in, in layout pixels. */
function cardBox(page, entry = ALPHA) {
  return page.evaluate((name) => {
    const found = document.querySelector(`.project-card[aria-label="${name}"] .terminal-viewer`)
    if (!found) return { width: 0, height: 0 }
    const rect = found.getBoundingClientRect()
    return { width: Math.round(rect.width), height: Math.round(rect.height) }
  }, entry.name)
}

/** What a card says instead of a terminal, when it says anything. */
async function cardNotice(page, entry = ALPHA) {
  const node = card(page, entry).locator('.terminal-viewer__title')
  if ((await node.count()) === 0) return ''
  return ((await node.first().textContent().catch(() => '')) ?? '').trim()
}

/** Every message of one type this page has sent, as parsed objects. */
function sent(frames, type) {
  return frames.filter((frame) => {
    if (frame.dir !== 'out' || typeof frame.payload !== 'string') return false
    try {
      return JSON.parse(frame.payload).type === type
    } catch {
      return false
    }
  })
}

/** How many WebSockets the page has opened, ever. */
function socketCount(page) {
  return page.evaluate(() => (window.__sockets ?? []).length)
}

/** Waits for the console's cards to be on screen. */
async function consoleReady(page) {
  await page.goto(CONSOLE_URL, { waitUntil: 'domcontentloaded' })
  return waitFor(async () => (await page.locator('.project-card').count()) > 0, 20_000)
}

/**
 * call makes an API request from the page, so it is same-origin by construction.
 *
 * The console has no controls for the runtime lifecycle and this phase did not
 * add any - §1 of the brief stops at showing the terminal. Starting and stopping
 * a runtime is therefore done the way an operator would do it outside the
 * browser, which is what this is.
 */
function call(page, path, method = 'POST') {
  return page.evaluate(
    async ([p, m]) => {
      const response = await fetch(p, { method: m })
      return { status: response.status, body: await response.text() }
    },
    [path, method],
  )
}

/** What the API says about a project's runtime, which is what the console reads. */
function runtimeState(page, entry = ALPHA) {
  return page.evaluate(async (id) => {
    const response = await fetch(`/api/projects/${id}/runtime`)
    const body = await response.json()
    return {
      state: body.runtime?.state ?? '',
      sessionAlive: body.runtime?.sessionAlive === true,
    }
  }, entry.id)
}

/** A marker no other part of this run could have produced. */
const marker = (what) => `${what}-${Date.now().toString(36)}-${Math.floor(Math.random() * 1e4)}`

async function main() {
  // The same channel every other suite uses. The bundled Chromium wants a
  // headless-shell binary this project has never installed, so a bare launch
  // fails before a page opens.
  const browser = await chromium.launch({ channel: 'chrome', headless: true })

  try {
    const { page, frames, errors } = await openPage(browser)
    settleShell(ALPHA)

    // =======================================================================
    // A. A runtime is started, and the console shows its terminal
    // =======================================================================

    // Stopped first, so that the terminal this section reads is one that came
    // back from this suite's own start rather than one the fixture left running.
    const stopped = await call(page, `/api/projects/${ALPHA.id}/runtime/stop`)
    report.check(
      "the project's runtime can be stopped",
      stopped.status === 200,
      `POST runtime/stop answered ${stopped.status}`,
    )
    // A stop is not a destroy, and that distinction is the product's rather than
    // this suite's: `Stop` interrupts what is running and keeps the session —
    // its scrollback with it — so a stopped runtime can be started again without
    // losing the screen, and `DELETE /runtime` is the call that ends a session.
    // `Runtime.SessionAlive` says the same thing in the API's own words, and the
    // console's runtime row words it "stopped" while the session is still there.
    //
    // The first version of this file asserted the opposite — that the session
    // ends — and was wrong about the product rather than finding a defect in it.
    const stoppedNow = await runtimeState(page)
    report.check(
      'and the runtime reports itself stopped, with its session kept',
      stoppedNow.state === 'STOPPED' && stoppedNow.sessionAlive,
      `state: ${stoppedNow.state}, session alive: ${stoppedNow.sessionAlive}`,
    )

    await consoleReady(page)
    const missing = await waitFor(
      async () => (await cardNotice(page)) === 'Runtime stopped',
      20_000,
      300,
    )
    report.check(
      'a stopped runtime is a card saying so, with no terminal in it',
      missing && (await viewer(page).locator('.xterm-screen').count()) === 0,
      `the card says ${JSON.stringify(await cardNotice(page))} and holds ` +
        `${await viewer(page).locator('.xterm-screen').count()} terminal(s)`,
    )

    const started = await call(page, `/api/projects/${ALPHA.id}/runtime/start`)
    report.check(
      'it can be started again',
      started.status === 200 && (await waitFor(async () => sessionAlive(ALPHA), 20_000, 300)),
      `POST runtime/start answered ${started.status}; the session is ` +
        `${sessionAlive(ALPHA) ? 'up' : 'missing'}`,
    )
    settleShell(ALPHA)

    // The card notices by itself: the page polls the dashboard, and a console
    // that only updated on reload would be one nobody could leave open.
    const arrived = await waitFor(
      async () => (await viewer(page).locator('.xterm-screen').count()) > 0,
      30_000,
      400,
    )
    report.check(
      'the console grows a terminal on the card without being reloaded',
      arrived,
      arrived ? 'the card holds a live terminal' : 'no terminal appeared on the card',
    )

    // And it is the real one: a marker typed into the pty from outside the
    // browser, with nothing but tmux in the path.
    const hello = marker('CONSOLE')
    await settleShell(ALPHA)
    typeCommand(ALPHA, `echo ${hello}`)
    const echoed = await waitFor(async () => (await cardText(page)).includes(hello), 25_000, 400)
    report.check(
      'what is typed into the pty outside the browser appears on the card',
      echoed,
      echoed
        ? `the card shows ${hello}`
        : `the card is showing ${JSON.stringify((await cardText(page)).slice(-200))}`,
    )

    // §8 and §13 of the phase brief: one socket for the page, however many
    // terminals the page contains. Seven cards, one connection - a socket per
    // project would be seven, and would be the second terminal transport the
    // brief forbids.
    const cards = await page.locator('.project-card').count()
    report.check(
      'the page carries every project on one websocket, not one each',
      (await socketCount(page)) === 1,
      `${cards} card(s) over ${await socketCount(page)} socket(s)`,
    )

    // =======================================================================
    // B. A console that has taken nothing is a viewer (§7, §15)
    // =======================================================================

    // Clicked before it is typed at, deliberately: a terminal that refuses the
    // keyboard should refuse it once it has focus, not only before. Otherwise
    // the check would be about focus rather than about authority.
    const typed = marker('TYPED')
    const framesBefore = frames.length
    await viewer(page).locator('.terminal__screen').click()
    await page.waitForTimeout(300)
    await page.keyboard.type(`echo ${typed}`)
    await page.keyboard.press('Enter')
    await page.waitForTimeout(1200)

    report.check(
      'nothing typed at a console terminal reaches the pty',
      !pane(ALPHA).includes(typed),
      `the pty ${pane(ALPHA).includes(typed) ? 'received' : 'never saw'} ${typed}`,
    )
    report.check(
      'because the client never sends it: no input message leaves the page',
      sent(frames.slice(framesBefore), 'input').length === 0,
      `${sent(frames.slice(framesBefore), 'input').length} input message(s) sent`,
    )
    // The card offers the button - it has to, or nobody could ever take the
    // keyboard from a console - and this is the check that it is offered and
    // not taken. Six other projects' cards are open on this page at this moment,
    // and a version of this component that requested control as it mounted would
    // send one frame per card the instant the page loaded.
    report.check(
      'and opening a card is not claiming it: no control.request is sent',
      sent(frames, 'control.request').length === 0,
      `${sent(frames, 'control.request').length} control.request message(s) sent, ` +
        `over ${cards} card(s) opened`,
    )
    report.check(
      'a terminal nobody has claimed draws no touch keys either',
      (await viewer(page).locator('.terminal__keys').count()) === 0,
      '',
    )

    // =======================================================================
    // C. Two consoles watch one terminal (§10)
    // =======================================================================

    const second = await openPage(browser)
    await consoleReady(second.page)
    const secondReady = await waitFor(
      async () => (await second.page.locator('.project-card .xterm-screen').count()) > 0,
      30_000,
      400,
    )
    report.check(
      'a second browser reading the console finds the same terminals',
      secondReady,
      `${await second.page.locator('.project-card .xterm-screen').count()} terminal(s) on the second console`,
    )

    const shared = marker('SHARED')
    await settleShell(ALPHA)
    typeCommand(ALPHA, `echo ${shared}`)
    const bothSaw = await waitFor(
      async () =>
        (await cardText(page)).includes(shared) && (await cardText(second.page)).includes(shared),
      25_000,
      400,
    )
    report.check(
      'two consoles are watching one terminal rather than two that agree',
      bothSaw && sessionAlive(ALPHA),
      `both cards show ${shared}: ${bothSaw}; one session for both: ${sessionAlive(ALPHA)}`,
    )

    // =======================================================================
    // D. A viewer's own shape does not move the pty (§11)
    // =======================================================================

    // And this card has not taken the lease, so nothing measures on its behalf
    // either. A console that *has* taken it is the case below this one's
    // opposite number: `controller-input` checks that the pty does not move for
    // that client either, which is a stronger claim and needs a second browser
    // to state.
    const ptyBefore = paneSize(ALPHA)
    const boxBefore = await cardBox(page)
    await page.setViewportSize({ width: 980, height: 760 })
    await page.waitForTimeout(2500)
    const boxAfter = await cardBox(page)
    report.check(
      'a console may be reshaped without moving the terminal everybody shares',
      paneSize(ALPHA) === ptyBefore && boxAfter.width !== boxBefore.width,
      `the pty stayed ${ptyBefore} (now ${paneSize(ALPHA)}); the card's screen went ` +
        `${boxBefore.width}x${boxBefore.height} -> ${boxAfter.width}x${boxAfter.height}`,
    )
    report.check(
      'and it never asks the server to resize: no resize message is sent',
      sent(frames, 'resize').length === 0,
      `${sent(frames, 'resize').length} resize message(s) sent`,
    )
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.waitForTimeout(800)

    // =======================================================================
    // E. A reload is a reconnection (§17)
    // =======================================================================

    const afterReloadMarker = marker('RELOADED')
    await settleShell(ALPHA)
    typeCommand(ALPHA, `echo ${afterReloadMarker}`)

    await page.reload({ waitUntil: 'domcontentloaded' })
    const reloaded = await waitFor(
      async () => (await viewer(page).locator('.xterm-screen').count()) > 0,
      30_000,
      400,
    )
    report.check(
      'a reloaded console draws its terminals again',
      reloaded,
      reloaded ? 'a terminal is on the card' : 'the card has no terminal after the reload',
    )

    const caughtUp = await waitFor(
      async () => (await cardText(page)).includes(afterReloadMarker),
      25_000,
      400,
    )
    report.check(
      'and the screen it comes back with is the live one, not the one it left',
      caughtUp,
      caughtUp
        ? `the card shows ${afterReloadMarker}, typed while the page was gone`
        : 'the terminal came back but did not show what happened meanwhile',
    )
    report.check(
      'a reload reconnects rather than opening a second connection for the page',
      (await socketCount(page)) === 1,
      `${await socketCount(page)} socket(s) opened by this page`,
    )

    // =======================================================================
    // F. A stopped runtime takes its terminal away, and gives it back
    // =======================================================================

    const stoppedAgain = await call(page, `/api/projects/${ALPHA.id}/runtime/stop`)
    const wentAway = await waitFor(
      async () => (await cardNotice(page)) === 'Runtime stopped',
      25_000,
      400,
    )
    report.check(
      'stopping a runtime takes the terminal off the card while the page is open',
      stoppedAgain.status === 200 &&
        wentAway &&
        (await viewer(page).locator('.xterm-screen').count()) === 0,
      `the card says ${JSON.stringify(await cardNotice(page))} and holds ` +
        `${await viewer(page).locator('.xterm-screen').count()} terminal(s)`,
    )

    // The neighbour is untouched, which is what makes this a per-project
    // lifecycle rather than a page-wide one. A card whose runtime is up keeps
    // its terminal while the one beside it loses its own.
    const neighbour = fixtures.projects.find(
      (entry) => entry.role === 'shell' && entry.id !== ALPHA.id,
    )
    const neighbourIntact =
      neighbour === undefined ||
      (await waitFor(
        async () => (await viewer(page, neighbour).locator('.xterm-screen').count()) > 0,
        20_000,
        400,
      ))
    report.check(
      "and leaves its neighbours' terminals alone",
      neighbourIntact,
      `${neighbour?.name ?? 'no neighbour'} still holds a terminal: ${neighbourIntact}`,
    )

    await call(page, `/api/projects/${ALPHA.id}/runtime/start`)
    settleShell(ALPHA)
    const backAgain = await waitFor(
      async () => (await viewer(page).locator('.xterm-screen').count()) > 0,
      30_000,
      400,
    )
    const finalMarker = marker('RESTARTED')
    typeCommand(ALPHA, `echo ${finalMarker}`)
    const liveAgain = await waitFor(
      async () => (await cardText(page)).includes(finalMarker),
      25_000,
      400,
    )
    report.check(
      'starting it again brings the terminal back, live',
      backAgain && liveAgain,
      `a terminal is on the card: ${backAgain}; it shows ${finalMarker}: ${liveAgain}`,
    )

    // =======================================================================
    // G. Nothing threw
    // =======================================================================

    const crashes = [...errors, ...second.errors].filter((text) => text.startsWith('pageerror:'))
    report.check(
      'no console threw while it was read, reloaded and had a runtime taken away',
      crashes.length === 0,
      crashes.length === 0
        ? `${errors.length + second.errors.length} console message(s), none of them uncaught`
        : crashes.join('; '),
    )

    const ok = report.summary()
    if (!ok) process.exitCode = 1
  } finally {
    await browser.close()
  }
}

await main().catch((error) => {
  console.error(error)
  process.exit(1)
})
