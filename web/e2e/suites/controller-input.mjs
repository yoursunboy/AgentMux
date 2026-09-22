/**
 * Phase 7.4B-2B browser E2E - the console as a terminal controller.
 *
 * # What this suite is
 *
 * Phase 7.4B-2A put a terminal on each of the console's cards and left it
 * read-only. This phase gave the console a keyboard: a card can ask for a
 * project's lease, and the client holding that lease is the one client the
 * server will accept input from. The claim under test is that this is true end
 * to end - that what is typed into a card arrives in the pty, that a second
 * console watching the same project cannot type into it, and that the keyboard
 * changes hands when the first console gives it up.
 *
 * # Why it takes two browsers
 *
 * "This client may type and that one may not" is not a question one browser can
 * be asked about itself. The evidence is a marker that reaches the pty from one
 * device, a second marker that never reaches it from the other, and both
 * devices looking at the same terminal on the same server at the same time.
 *
 * # The two devices are stated, not read back
 *
 * Both contexts are given their User-Agent explicitly, so the label one device
 * wears on the other's screen is a fact about the fixture rather than about
 * whichever Chrome this machine happens to be running - the server derives that
 * label from the header, so a suite that let Playwright supply it would be a
 * suite about this machine. No iPad is attached to this machine: the tablet
 * half is Playwright's viewport, touch and User-Agent emulation, and §39 of the
 * phase directive asks for that to be said rather than implied.
 *
 * # The geometry never moves
 *
 * §14 of the brief gives resize to whoever holds the lease. This phase narrows
 * that on the console deliberately: a card is a few hundred pixels in a grid,
 * and a browser watching from a tablet must not reflow the terminal somebody is
 * working in. So the assertion is the strong one - not "the card resized to the
 * right shape" but "the pty kept the shape it had" - measured from tmux on
 * either side of a controller typing, with the wire read as well so a console
 * that asked and was refused fails here too.
 *
 * # What is not here
 *
 * The lease itself: who may take it, what happens when two clients ask at once,
 * what a forged client identifier does. Those are `internal/terminal`'s own
 * tests, where a race can be run a hundred times and a frame can be sent with
 * any field in it. This file is the browser half - it presses the buttons a
 * person presses and reads a terminal a person reads. The one exception is the
 * forged frame in section E, and it is here rather than there for a reason that
 * section states.
 */
import { chromium } from 'playwright'

import {
  PAGE_URL,
  Report,
  controlBadge,
  fixtures,
  openPage,
  pane,
  paneSize,
  releaseControl,
  settleShell,
  takeControl,
  waitFor,
} from '../lib/harness.mjs'

const report = new Report('controller-input')

/** The console's address, on the server's own origin. */
const CONSOLE_URL = new URL('/dashboard', PAGE_URL).toString()

/** A plain shell, so that what is typed into it echoes back and can be read. */
const ALPHA = fixtures.projects.find((entry) => entry.role === 'shell')

if (!ALPHA) throw new Error('the fixture has no shell project for this suite to type into')

/**
 * The two devices.
 *
 * The viewports are what each device actually has: a laptop window, and an iPad
 * Air in portrait. The tablet carries a scale factor because a retina screen is
 * a different number of device pixels for the same layout, and the terminal
 * measures itself in layout pixels.
 */
const DESKTOP = {
  label: 'Chrome on Windows',
  ua:
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 ' +
    '(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36',
  viewport: { width: 1440, height: 900 },
}

const TABLET = {
  label: 'Safari on iPad',
  ua:
    'Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 ' +
    '(KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1',
  viewport: { width: 820, height: 1180 },
  deviceScaleFactor: 2,
  isMobile: true,
  hasTouch: true,
}

// ---------------------------------------------------------------------------
// Reading a card

/** One project's card, addressed the way the markup names it. */
const card = (page, entry = ALPHA) =>
  page.locator(`.project-card[aria-label="${entry.name}"]`)

/** The text of the badge that says who is in charge of a card's terminal. */
async function badge(page, entry = ALPHA) {
  const node = controlBadge(page, entry)
  if ((await node.count()) === 0) return ''
  return ((await node.first().textContent().catch(() => '')) ?? '').trim()
}

/** Whether a badge says this console is the controller, count and all. */
const holds = (text) => text.startsWith('You control')

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

/**
 * The parsed messages of one type this page has sent or received.
 *
 * Both return the decoded message rather than the frame, because most of what
 * these checks say is about a field inside one - `code`, `about`, `projectId` -
 * and a helper that returned the envelope would make `messages[0].code`
 * silently undefined rather than obviously wrong.
 */
function messages(frames, dir, type) {
  const found = []
  for (const frame of frames) {
    if (frame.dir !== dir || typeof frame.payload !== 'string') continue
    try {
      const message = JSON.parse(frame.payload)
      if (message.type === type) found.push(message)
    } catch {
      // A frame this suite cannot read is not one of its messages. The protocol
      // is binary for terminal output and JSON for everything else, and the two
      // share this socket.
    }
  }
  return found
}

/** Every message of one type this page has sent, parsed. */
const sent = (frames, type) => messages(frames, 'out', type)

/** Every message of one type this page has received, parsed. */
const received = (frames, type) => messages(frames, 'in', type)

/** A marker no other part of this run could have produced. */
const marker = (what) => `${what}-${Date.now().toString(36)}-${Math.floor(Math.random() * 1e4)}`

// ---------------------------------------------------------------------------
// Driving the console

/** Opens the console and waits for its cards to be on screen. */
async function consoleReady(page) {
  await page.goto(CONSOLE_URL, { waitUntil: 'domcontentloaded' })
  return waitFor(async () => (await page.locator('.project-card').count()) > 0, 20_000)
}

/** Opens a browser context that says it is this device, on the console. */
async function openDevice(browser, device) {
  const opened = await openPage(browser, {
    viewport: device.viewport,
    userAgent: device.ua,
    deviceScaleFactor: device.deviceScaleFactor ?? 1,
    isMobile: device.isMobile ?? false,
    hasTouch: device.hasTouch ?? false,
  })
  await consoleReady(opened.page)
  // Everything this device says about control is said on the console, so the
  // frames are counted from here rather than from the beginning: `openPage`
  // lands on the workspace first, and a frame from that page would be evidence
  // about a different page.
  return { ...device, ...opened, consoleFrom: opened.frames.length }
}

/**
 * call makes an API request from the page, so it is same-origin by construction.
 *
 * The console has no controls for the runtime lifecycle and neither phase added
 * any. Starting a runtime is therefore done the way an operator would do it
 * outside the browser, which is what this is.
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
    return { state: body.runtime?.state ?? '' }
  }, entry.id)
}

/**
 * typeInto puts text into a card the way a person does: aim at the card, click
 * the terminal, then use the keyboard.
 *
 * Clicking matters even for a viewer. A terminal that refuses the keyboard
 * should refuse it after it has been focused, not only before - otherwise the
 * check would be about focus rather than about authority.
 */
async function typeInto(page, entry, text) {
  await card(page, entry).locator('.terminal__screen').click()
  await page.waitForTimeout(200)
  await page.keyboard.type(text)
  await page.keyboard.press('Enter')
  await page.waitForTimeout(1200)
}

/**
 * forgeInput sends an input frame from a page's own socket, past the client.
 *
 * §11 of the brief is that the server must not trust the frontend, and the
 * frontend is exactly what every other check in this file proves something
 * about: the viewer's keyboard is refused because no handler is bound to it,
 * which says nothing at all about what happens when a frame is sent anyway. A
 * client can send one - the socket is right there - so the only meaningful test
 * is this one, and it has to run in a browser because a browser is what holds
 * the socket and the client identifier the server is checking.
 *
 * The frame is written by hand rather than through the client's own `input()`,
 * because the client is the thing being bypassed. `data` is the same base64 the
 * protocol carries keystrokes in; the bytes are never logged, server-side, at
 * any level.
 */
function forgeInput(page, entry, text) {
  const data = Buffer.from(text, 'utf8').toString('base64')
  return page.evaluate(
    ([id, encoded]) => {
      const sockets = window.__sockets ?? []
      const socket = sockets[sockets.length - 1]
      if (!socket) return false
      socket.send(JSON.stringify({ type: 'input', projectId: id, data: encoded }))
      return true
    },
    [entry.id, data],
  )
}

/**
 * waitForFree waits until a project's lease is nobody's.
 *
 * A lease is not released when the browser holding it closes. It is suspended
 * for the grace period and expires on its own (§16), so a suite that ran before
 * this one and drove a controller can leave a project unclaimed but not yet
 * free. Waiting that out is not a workaround - it is the behaviour, and
 * `controller`'s own suite is where it is asserted. Here it is setup, and it is
 * the reason the first check in this file is about a free terminal rather than
 * assumed to be looking at one.
 */
function waitForFree(page, entry = ALPHA, timeoutMs = 25_000) {
  return waitFor(
    async () => (await badge(page, entry)).includes('nobody is in control'),
    timeoutMs,
  )
}

// ---------------------------------------------------------------------------

async function main() {
  // The same channel every other suite uses. The bundled Chromium wants a
  // headless-shell binary this project has never installed, so a bare launch
  // fails before a page opens.
  const browser = await chromium.launch({ channel: 'chrome', headless: true })

  try {
    const desktop = await openDevice(browser, DESKTOP)
    const tablet = await openDevice(browser, TABLET)
    settleShell(ALPHA)

    // =======================================================================
    // A. Two consoles watch one terminal (§8, §13)
    // =======================================================================

    // The card draws no terminal at all while the runtime is down, and this
    // suite is about a card that is live. The fixture starts every runtime, so
    // this is normally a read - but it is a read that a suite which inherited a
    // stopped runtime would otherwise fail on without saying why.
    let running = await runtimeState(desktop.page)
    if (running.state !== 'RUNNING') {
      await call(desktop.page, `/api/projects/${ALPHA.id}/runtime/start`)
      await waitFor(async () => (await runtimeState(desktop.page)).state === 'RUNNING', 30_000)
      running = await runtimeState(desktop.page)
    }
    report.check(
      'the project this suite types into has a runtime',
      running.state === 'RUNNING',
      `${ALPHA.name} is ${running.state}`,
    )

    // Both devices now wait for the card's terminal and its roster. A roster is
    // sent with the first subscribe, so a card that has a badge has a live
    // terminal under it and the checks below are about control rather than
    // about loading.
    const live = await Promise.all([
      waitFor(async () => (await controlBadge(desktop.page, ALPHA).count()) > 0, 30_000),
      waitFor(async () => (await controlBadge(tablet.page, ALPHA).count()) > 0, 30_000),
    ])
    report.check(
      'both consoles are watching the same project with a live terminal',
      live.every(Boolean) &&
        (await cardText(desktop.page)).trim() !== '' &&
        (await cardText(tablet.page)).trim() !== '',
      `desktop badge: ${JSON.stringify(await badge(desktop.page))}, ` +
        `tablet badge: ${JSON.stringify(await badge(tablet.page))}`,
    )

    const free = await waitForFree(desktop.page)
    report.check(
      'and neither of them is in control of it yet',
      free && (await badge(tablet.page)).includes('nobody is in control'),
      `desktop badge: ${JSON.stringify(await badge(desktop.page))}, ` +
        `tablet badge: ${JSON.stringify(await badge(tablet.page))}`,
    )
    if (!free) {
      report.summary()
      await browser.close()
      process.exit(1)
    }

    // The geometry the pty has before anybody claims it. Every resize claim
    // below is measured against this rather than against a number written here,
    // because the shape a fixture pane happens to have is not this suite's
    // business - that it does not change is.
    const sizeBefore = paneSize(ALPHA)

    // =======================================================================
    // B. The desktop takes the keyboard (§8, §9, §13)
    // =======================================================================

    const beforeRequest = desktop.frames.length
    const claimed = await takeControl(desktop.page, ALPHA)
    report.check(
      'a console can ask for the terminal and be given it',
      claimed && holds(await badge(desktop.page)),
      `desktop badge: ${JSON.stringify(await badge(desktop.page))}`,
    )
    // Asked once, and only when the button was pressed. A console carries a
    // card for every project on the page, and a card that requested control as
    // it mounted would take the keyboard from whoever is working, once per
    // project, silently.
    report.check(
      'by asking once, when it was asked to',
      sent(desktop.frames.slice(beforeRequest), 'control.request').length === 1,
      `${sent(desktop.frames.slice(beforeRequest), 'control.request').length} control.request message(s)`,
    )
    // §13's other case, and the one that makes the badge worth having: the
    // second console is told who has the keyboard rather than being left to
    // guess why its own terminal will not type.
    report.check(
      'and the other console is told who has it',
      (await badge(tablet.page)) === `Viewer · ${DESKTOP.label} has control`,
      `tablet badge: ${JSON.stringify(await badge(tablet.page))}`,
    )

    // =======================================================================
    // C. What the controller types reaches the pty (§8, §10)
    // =======================================================================

    const typed = marker('TYPED')
    const beforeTyping = desktop.frames.length
    await typeInto(desktop.page, ALPHA, `echo ${typed}`)

    report.check(
      'what the controller types reaches the pty',
      pane(ALPHA).includes(typed),
      `the pty ${pane(ALPHA).includes(typed) ? 'received' : 'never saw'} ${typed}`,
    )
    report.check(
      'and it reaches it as input messages from that console',
      sent(desktop.frames.slice(beforeTyping), 'input').length > 0,
      `${sent(desktop.frames.slice(beforeTyping), 'input').length} input message(s) sent`,
    )
    // The controller's own card shows what it typed, because the pty echoes it
    // back like any other terminal. The screen and the pty are not two views of
    // two things.
    report.check(
      'and the card it was typed into shows it, because the pty echoed it',
      (await cardText(desktop.page)).includes(typed),
      `the card shows ${JSON.stringify((await cardText(desktop.page)).slice(-120))}`,
    )

    // =======================================================================
    // D. Taking the keyboard does not reshape the terminal (§14)
    // =======================================================================

    const sizeAfter = paneSize(ALPHA)
    report.check(
      'taking the keyboard on a console does not reshape the pty',
      sizeAfter === sizeBefore,
      `the pty was ${sizeBefore} and is ${sizeAfter}`,
    )
    // The other half of the same rule, and the one that would catch a console
    // that asked and was refused: nothing went out at all. `mayResize: false`
    // withholds the size on the subscribe as well as on the resize, because the
    // server applies a subscribe's size for a client that holds the lease.
    report.check(
      'because the console never asks it to: no resize message leaves the page',
      sent(desktop.frames.slice(desktop.consoleFrom), 'resize').length === 0,
      `${sent(desktop.frames.slice(desktop.consoleFrom), 'resize').length} resize message(s) sent`,
    )

    // =======================================================================
    // E. The viewer cannot type, and the server is why (§9, §10, §11)
    // =======================================================================

    const refused = marker('REFUSED')
    const beforeRefused = tablet.frames.length
    await typeInto(tablet.page, ALPHA, `echo ${refused}`)

    report.check(
      'nothing the other console types reaches the pty',
      !pane(ALPHA).includes(refused),
      `the pty ${pane(ALPHA).includes(refused) ? 'received' : 'never saw'} ${refused}`,
    )
    // Stronger than "typing does nothing": the viewer's terminal has no input
    // handler bound to it at all, so a keystroke has nothing to reach rather
    // than a handler that declines it.
    report.check(
      'because a viewer sends nothing: no input message leaves that page',
      sent(tablet.frames.slice(beforeRefused), 'input').length === 0,
      `${sent(tablet.frames.slice(beforeRefused), 'input').length} input message(s) sent`,
    )

    // That is a claim about the console. §11 is a claim about the server, and a
    // client can always send a frame the console would not have sent - so one
    // is sent, by hand, from the viewer's own socket.
    const forged = marker('FORGED')
    const beforeForged = tablet.frames.length
    const wrote = await forgeInput(tablet.page, ALPHA, `echo ${forged}\r`)
    await tablet.page.waitForTimeout(1200)

    report.check(
      'a viewer that sends an input frame anyway does not reach the pty',
      wrote && !pane(ALPHA).includes(forged),
      `the pty ${pane(ALPHA).includes(forged) ? 'received' : 'never saw'} ${forged}`,
    )
    const refusals = received(tablet.frames.slice(beforeForged), 'error')
    report.check(
      'and is told why, in the protocol’s own words, rather than ignored',
      refusals.length > 0 && refusals[0].code === 'not_controller',
      refusals.length > 0
        ? `error ${JSON.stringify(refusals[0].code)} about ${JSON.stringify(refusals[0].about)}`
        : 'no error message came back',
    )
    // A refused frame is refused, not fatal. Dropping the connection would turn
    // a viewer's stray keystroke into an outage for everybody watching, so the
    // server keeps it - and the console has to keep drawing, which is the half
    // of that rule this check found missing. A card used to answer any error by
    // replacing itself with "Unable to connect terminal", which is what a live
    // terminal looked like when one keystroke was refused: still connected, the
    // lease unmoved, and the screen gone. That is the reachable case as well as
    // the forged one - a keystroke sent in the window between the lease moving
    // and the roster saying so is refused exactly this way.
    report.check(
      'and the connection it was sent down survives, with the lease and the screen where they were',
      (await badge(desktop.page)) === 'You control · 1 watching' &&
        (await badge(tablet.page)) === `Viewer · ${DESKTOP.label} has control` &&
        (await cardText(tablet.page)).trim() !== '',
      `desktop badge: ${JSON.stringify(await badge(desktop.page))}, ` +
        `tablet badge: ${JSON.stringify(await badge(tablet.page))}, ` +
        `tablet screen: ${JSON.stringify((await cardText(tablet.page)).slice(-60))}`,
    )

    // =======================================================================
    // F. The keyboard changes hands (§9, §15)
    // =======================================================================

    const released = await releaseControl(desktop.page, ALPHA)
    report.check(
      'the controller can give the keyboard up',
      released && !holds(await badge(desktop.page)),
      `desktop badge: ${JSON.stringify(await badge(desktop.page))}`,
    )
    report.check(
      'and the console that was refused is told the terminal is free',
      await waitFor(async () => (await badge(tablet.page)).includes('nobody is in control'), 10_000),
      `tablet badge: ${JSON.stringify(await badge(tablet.page))}`,
    )

    const tookOver = await takeControl(tablet.page, ALPHA)
    report.check(
      'so the console that was refused a moment ago can now take it',
      tookOver && holds(await badge(tablet.page)),
      `tablet badge: ${JSON.stringify(await badge(tablet.page))}`,
    )
    // The line that makes section E mean something. A viewer that cannot type
    // and a browser that is broken are indistinguishable from one check; the
    // same device typing successfully once it holds the lease is what says the
    // refusal was about authority.
    const typedAgain = marker('TAKEN-OVER')
    await typeInto(tablet.page, ALPHA, `echo ${typedAgain}`)
    report.check(
      'and its typing is now what reaches the pty',
      pane(ALPHA).includes(typedAgain),
      `the pty ${pane(ALPHA).includes(typedAgain) ? 'received' : 'never saw'} ${typedAgain}`,
    )
    report.check(
      'and the first console is now the one that cannot',
      (await badge(desktop.page)) === `Viewer · ${TABLET.label} has control`,
      `desktop badge: ${JSON.stringify(await badge(desktop.page))}`,
    )

    // Given up again, so the project is nobody's when this suite ends. The next
    // suite to run takes this same project, and a lease suspended by a browser
    // that has closed is not a lease it can take.
    const handedBack = await releaseControl(tablet.page, ALPHA)
    report.check(
      'and the last console hands it back, leaving the project nobody’s',
      handedBack && !holds(await badge(tablet.page)),
      `tablet badge: ${JSON.stringify(await badge(tablet.page))}`,
    )

    // Two consoles, a refused frame and a handover: Chromium's console has
    // things to say that are not defects - a websocket that was opened and
    // closed by a navigation, a resource fetched twice. What would be a defect
    // is uncaught Javascript, which is what this reads.
    const crashes = [...desktop.errors, ...tablet.errors].filter((text) =>
      text.startsWith('pageerror:'),
    )
    report.check(
      'neither console threw while the keyboard changed hands',
      crashes.length === 0,
      crashes.length === 0
        ? `${desktop.errors.length + tablet.errors.length} console message(s), none of them uncaught`
        : crashes.join('; '),
    )

    const ok = report.summary()
    await browser.close()
    if (!ok) process.exit(1)
  } catch (error) {
    await browser.close().catch(() => {})
    throw error
  }
}

await main().catch((error) => {
  console.error(error)
  process.exit(1)
})
