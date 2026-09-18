/**
 * Phase 6 browser E2E - one runtime, several devices.
 *
 * The phase's claim is that the same project can be open on a desktop and a
 * tablet at the same time: one session, one pty, one Claude, and exactly one of
 * the two devices allowed to type into it. This file is that claim against two
 * real browser contexts on one real fixture - the desktop holding the keyboard,
 * the tablet watching, and then the other way round.
 *
 * # Two devices, stated rather than assumed
 *
 * Both contexts are given their User-Agent explicitly. The label a device is
 * shown under, on the other person's screen, is derived from that header
 * server-side, so a suite that read this machine's own Chrome build back would
 * be a suite about this machine. Stating them is also what lets the checks name
 * the two devices: `Chrome on Windows` and `Safari on iPad` are facts about the
 * fixture rather than about whoever ran it.
 *
 * # No iPad is attached to this machine
 *
 * The tablet half is Playwright's touch and mobile emulation, an iPad viewport
 * and an iPad User-Agent. Nothing in this file claims otherwise. §三十九 of the
 * directive asks for that to be said rather than implied, and this is where it
 * is said.
 *
 * # What is not here
 *
 * The wire. Every claim below is made through the interface or through tmux, and
 * the frames this file does read - what a viewer sent, and did not - are read to
 * prove an absence, which no screen can show. Message-by-message protocol
 * behaviour belongs to the terminal package's own tests; so does the shape of a
 * lease under a race (`internal/terminal/control_test.go`).
 */
import { chromium } from 'playwright'

import {
  Report,
  agentCount,
  awaitProject,
  controlBadge,
  fixtures,
  openPage,
  pane,
  paneSize,
  panelOf,
  sessionAlive,
  setWorkspace,
  settleShell,
  severConnection,
  startServer,
  stopServer,
  takeControl,
  waitFor,
  wsl,
} from '../lib/harness.mjs'

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

/** The fixture projects this suite is about: two plain shells, so typing echoes. */
const shells = fixtures.projects.filter((entry) => entry.role === 'shell')
const ALPHA = shells[0]
const BRAVO = shells[1]

const report = new Report('controller')

// ---------------------------------------------------------------------------
// Reading a device

/** Opens a browser context that says it is this device. */
async function openDevice(browser, device) {
  const opened = await openPage(browser, {
    viewport: device.viewport,
    userAgent: device.ua,
    deviceScaleFactor: device.deviceScaleFactor ?? 1,
    isMobile: device.isMobile ?? false,
    hasTouch: device.hasTouch ?? false,
  })
  return { ...device, ...opened }
}

/**
 * look puts a device's view on a project's panel.
 *
 * The workspace is a grid with a manager alongside it, so a device that has
 * walked to one project is not still looking at another. Every read below is
 * therefore aimed first: a badge read off a panel that is on another page comes
 * back empty, and "empty" and "not the controller" are the same string.
 */
async function look(device, entry) {
  const panel = await awaitProject(device.page, entry)
  await device.page.waitForTimeout(200)
  return panel
}

/** The text of the badge that says who is in charge of a project. */
async function badge(device, entry) {
  const node = controlBadge(device.page, entry)
  if ((await node.count()) === 0) return ''
  return ((await node.first().textContent().catch(() => '')) ?? '').trim()
}

/**
 * holds reports whether a badge says this device is the controller.
 *
 * The badge is "You control" on its own when nobody else is watching and
 * "You control · 3 watching" when somebody is, and both of those are the same
 * answer to the question this file asks. The two places where the count *is* the
 * claim say so in full, by comparing the whole string.
 */
const holds = (text) => text.startsWith('You control')

/** The badge for a project, aimed at first, so a stale page cannot answer. */
async function badgeAt(device, entry) {
  await look(device, entry)
  return badge(device, entry)
}

/** The one control action this device is offered, when it is looking at it. */
function controlAction(device, entry) {
  return panelOf(device.page, entry).locator('.terminal__group--actions button').first()
}

/** The Prompt Bar's field, which is where a viewer is told it may not type. */
function promptField(device, entry) {
  return panelOf(device.page, entry).getByLabel('Message Claude')
}

/** The box the terminal is drawn in, in layout pixels. */
function screenBox(device, entry) {
  return device.page.evaluate((id) => {
    const screen = document.querySelector(
      `.panel--project[data-project-id="${id}"] .terminal__screen`,
    )
    if (!screen) return { width: 0, height: 0 }
    const rect = screen.getBoundingClientRect()
    return { width: Math.round(rect.width), height: Math.round(rect.height) }
  }, entry.id)
}

/** What a panel says about its own connection. */
async function status(device, entry) {
  const node = panelOf(device.page, entry).locator('.terminal__state').first()
  if ((await node.count()) === 0) return ''
  return ((await node.textContent().catch(() => '')) ?? '').trim()
}

/** The text one panel has painted, which is what "showing it" means. */
function renderedText(device, entry) {
  return device.page.evaluate((id) => {
    const panel = document.querySelector(`.panel--project[data-project-id="${id}"]`)
    if (!panel) return ''
    return Array.from(panel.querySelectorAll('.xterm-rows > div'))
      .map((row) => row.textContent)
      .join('\n')
  }, entry.id)
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

/**
 * typeInto puts text into a terminal the way a person does: aim at the panel,
 * click the terminal, then use the keyboard.
 *
 * Clicking matters even for a viewer. A terminal that refuses the keyboard
 * should refuse it after it has been focused, not only before - otherwise the
 * check would be about focus rather than about authority.
 */
async function typeInto(device, entry, text) {
  const panel = await look(device, entry)
  await panel.locator('.terminal__screen').click()
  await device.page.waitForTimeout(200)
  await device.page.keyboard.type(text)
  await device.page.keyboard.press('Enter')
  await device.page.waitForTimeout(1200)
}

async function main() {
  const browser = await chromium.launch({ channel: 'chrome', headless: true })
  console.log(`Claude process(es) at the start: ${agentCount()}`)

  const desktop = await openDevice(browser, DESKTOP)
  await desktop.page.waitForSelector('.xterm-screen', { timeout: 30_000 })

  // Membership is a server fact, so narrowing it here narrows it for the tablet
  // that opens next as well - which is what makes a second device arrive at the
  // same project without opening anything.
  await setWorkspace(desktop.page, [ALPHA])
  await look(desktop, ALPHA)
  settleShell(ALPHA)

  // =========================================================================
  // A. A second device is a viewer (§二十七 case 1, §二十九, §十)
  // =========================================================================

  report.check(
    'a device that opens a terminal nobody holds is a viewer, and says so',
    (await badge(desktop, ALPHA)) === 'Viewer · nobody is in control',
    `badge: ${JSON.stringify(await badge(desktop, ALPHA))}`,
  )

  let asked = true
  try {
    await panelOf(desktop.page, ALPHA)
      .getByRole('button', { name: 'Request control' })
      .first()
      .click({ timeout: 10_000 })
  } catch {
    asked = false
  }
  report.check(
    'asking for a terminal nobody is using is granted at once',
    asked && (await waitFor(async () => (await badge(desktop, ALPHA)) === 'You control', 10_000)),
    `badge: ${JSON.stringify(await badge(desktop, ALPHA))}`,
  )

  const tablet = await openDevice(browser, TABLET)
  await look(tablet, ALPHA)
  await tablet.page.waitForSelector('.xterm-screen', { timeout: 30_000 })

  await waitFor(
    async () => (await badge(tablet, ALPHA)) === `Viewer · ${DESKTOP.label} has control`,
    15_000,
  )
  report.check(
    'a second device opening the same project is a viewer, and is told whose turn it is',
    (await badge(tablet, ALPHA)) === `Viewer · ${DESKTOP.label} has control`,
    `tablet badge: ${JSON.stringify(await badge(tablet, ALPHA))}`,
  )
  report.check(
    'and the device holding it is told how many are watching',
    (await badge(desktop, ALPHA)) === 'You control · 1 watching',
    `desktop badge: ${JSON.stringify(await badge(desktop, ALPHA))}`,
  )

  // §二十九, the half about the viewer: nothing it does reaches the pty.
  const viewerMarker = `VIEWER-${Date.now().toString(36)}`
  const tabletTyped = tablet.frames.length
  await typeInto(tablet, ALPHA, `echo ${viewerMarker}`)
  report.check(
    'nothing a viewer types reaches the terminal',
    !pane(ALPHA).includes(viewerMarker),
    `the pty ${pane(ALPHA).includes(viewerMarker) ? 'received' : 'never saw'} ${viewerMarker}`,
  )
  report.check(
    'because it is refused where it is typed: the client never sends it',
    sent(tablet.frames.slice(tabletTyped), 'input').length === 0,
    `${sent(tablet.frames.slice(tabletTyped), 'input').length} input message(s) sent`,
  )

  const viewerPrompt = promptField(tablet, ALPHA)
  const viewerPlaceholder = String(await viewerPrompt.getAttribute('placeholder'))
  report.check(
    'and the viewer is told why, in the panel it is looking at',
    (await viewerPrompt.isDisabled()) && viewerPlaceholder.includes(`only ${DESKTOP.label} can type`),
    `disabled: ${await viewerPrompt.isDisabled()}, placeholder: ${JSON.stringify(viewerPlaceholder)}`,
  )
  report.check(
    'with the keys a tablet has no other way to send refused alongside it',
    await panelOf(tablet.page, ALPHA)
      .locator('.terminal__keys button[aria-label="Tab"]')
      .isDisabled(),
    '',
  )

  // §三十二, and the point of the whole phase: two devices, one runtime.
  const shared = `ONE-PTY-${Date.now().toString(36)}`
  await typeInto(desktop, ALPHA, `echo ${shared}`)
  const bothSaw = await waitFor(
    async () =>
      (await renderedText(tablet, ALPHA)).includes(shared) &&
      (await renderedText(desktop, ALPHA)).includes(shared),
    15_000,
    400,
  )
  await look(tablet, ALPHA)
  report.check(
    'the two devices are looking at one terminal, not two that happen to match',
    bothSaw && sessionAlive(ALPHA),
    `both screens show ${shared}: ${bothSaw}; one tmux session for both: ${sessionAlive(ALPHA)}`,
  )

  // §十: a device name and a count of watchers, and nothing else about them.
  const exposed = await desktop.page.evaluate(() => document.body.innerText)
  const leaked = [
    ['an IPv4 address', /\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b/],
    ['a client identifier', /\bc_[0-9a-f]{8,}\b/],
  ].filter(([, pattern]) => pattern.test(exposed))
  report.check(
    'the interface names the device and counts the watchers, and shows nothing else about them',
    leaked.length === 0,
    leaked.length === 0
      ? 'no address and no client identifier anywhere in the page'
      : `the page shows ${leaked.map(([what]) => what).join(' and ')}`,
  )

  // =========================================================================
  // B. Asking, and handing over (§二十七 case 2, §十二)
  // =========================================================================

  await panelOf(tablet.page, ALPHA)
    .getByRole('button', { name: 'Request control' })
    .first()
    .click()
  const queued = await waitFor(
    async () => (await badge(tablet, ALPHA)) === 'Waiting for control…',
    10_000,
    300,
  )
  report.check(
    'asking for a terminal somebody is using queues rather than takes',
    queued,
    `tablet badge: ${JSON.stringify(await badge(tablet, ALPHA))}`,
  )
  report.check(
    'and the viewer may not ask again while it waits',
    await controlAction(tablet, ALPHA).isDisabled(),
    '',
  )

  await look(desktop, ALPHA)
  const requests = panelOf(desktop.page, ALPHA).locator('.terminal__requests')
  const rowText = ((await requests.count()) > 0 ? await requests.textContent() : '') ?? ''
  report.check(
    'the controller is told who is asking, and is the one who decides',
    rowText.includes(`${TABLET.label} is asking for control`) &&
      (await panelOf(desktop.page, ALPHA).getByRole('button', { name: 'Hand over' }).count()) === 1 &&
      (await panelOf(desktop.page, ALPHA).getByRole('button', { name: 'Decline' }).count()) === 1,
    JSON.stringify(rowText.trim()),
  )

  await panelOf(desktop.page, ALPHA).getByRole('button', { name: 'Hand over' }).click()
  const handedOver = await waitFor(
    async () =>
      (await badge(tablet, ALPHA)) === 'You control · 1 watching' &&
      (await badge(desktop, ALPHA)) === `Viewer · ${TABLET.label} has control`,
    15_000,
    300,
  )
  report.check(
    'handing it over makes the asker the controller, and the giver a viewer',
    handedOver,
    `tablet: ${JSON.stringify(await badge(tablet, ALPHA))}, ` +
      `desktop: ${JSON.stringify(await badge(desktop, ALPHA))}`,
  )

  const giverPlaceholder = String(await promptField(desktop, ALPHA).getAttribute('placeholder'))
  report.check(
    'and the device that gave it up is refused its own keyboard',
    (await promptField(desktop, ALPHA).isDisabled()) &&
      giverPlaceholder.includes(`only ${TABLET.label} can type`),
    `placeholder: ${JSON.stringify(giverPlaceholder)}`,
  )

  // §二十九 in the direction that matters: the controller types and it lands,
  // the viewer types and it does not - both read off the pty rather than the
  // screen.
  const controllerMarker = `CONTROLLER-${Date.now().toString(36)}`
  await typeInto(tablet, ALPHA, `echo ${controllerMarker}`)
  report.check(
    'the controller types and it reaches the terminal',
    pane(ALPHA).includes(controllerMarker),
    '',
  )

  const desktopTyped = desktop.frames.length
  const refusedMarker = `REFUSED-${Date.now().toString(36)}`
  await typeInto(desktop, ALPHA, `echo ${refusedMarker}`)
  report.check(
    'and the viewer, now on the other side of the handover, still cannot',
    !pane(ALPHA).includes(refusedMarker) &&
      sent(desktop.frames.slice(desktopTyped), 'input').length === 0,
    `${sent(desktop.frames.slice(desktopTyped), 'input').length} input message(s) sent`,
  )

  // =========================================================================
  // C. Resize follows the keyboard (§二十八)
  // =========================================================================

  const ptyBefore = paneSize(ALPHA)
  await look(desktop, ALPHA)
  const desktopBefore = await screenBox(desktop, ALPHA)
  await desktop.page.setViewportSize({ width: 1100, height: 780 })
  await desktop.page.waitForTimeout(2500)
  await look(desktop, ALPHA)
  const desktopAfter = await screenBox(desktop, ALPHA)
  report.check(
    'a viewer may reshape its own window; the terminal everybody shares does not move',
    paneSize(ALPHA) === ptyBefore && desktopAfter.width !== desktopBefore.width,
    `pty ${ptyBefore} -> ${paneSize(ALPHA)}; the viewer's own screen ` +
      `${desktopBefore.width}x${desktopBefore.height} -> ${desktopAfter.width}x${desktopAfter.height}`,
  )

  await look(tablet, ALPHA)
  const tabletBefore = await screenBox(tablet, ALPHA)
  await tablet.page.setViewportSize({ width: 900, height: 1200 })
  const resized = await waitFor(async () => paneSize(ALPHA) !== ptyBefore, 20_000, 400)
  await look(tablet, ALPHA)
  const tabletAfter = await screenBox(tablet, ALPHA)
  report.check(
    'the controller reshaping its own window does move it, for everybody',
    resized,
    `pty ${ptyBefore} -> ${paneSize(ALPHA)} as the tablet's screen went ` +
      `${tabletBefore.width}x${tabletBefore.height} -> ${tabletAfter.width}x${tabletAfter.height}`,
  )

  // =========================================================================
  // D. One project's controller is not another's (§三十)
  // =========================================================================

  // Both devices are told the workspace they are to hold, rather than one of
  // them being told and the other being expected to notice. Membership is a
  // server fact, but a page learns about a change by making one.
  await setWorkspace(desktop.page, [ALPHA, BRAVO])
  await setWorkspace(tablet.page, [ALPHA, BRAVO])
  await look(desktop, BRAVO)
  await look(tablet, BRAVO)

  // Re-aimed, every one of them. A read is scoped to a panel, and only the
  // current page's panels exist - so asking the tablet about Alpha while it is
  // showing Bravo answers "no badge", which is also the answer for "nobody has
  // it". The two have to be told apart, which means walking to each project.
  const tabletFree = await badgeAt(tablet, BRAVO)
  const tabletHeld = await badgeAt(tablet, ALPHA)
  report.check(
    'a project nobody has claimed reads as free, beside one this device holds',
    tabletFree === 'Viewer · nobody is in control' && holds(tabletHeld),
    `bravo: ${JSON.stringify(tabletFree)}, alpha: ${JSON.stringify(tabletHeld)}`,
  )

  await takeControl(desktop.page, BRAVO)
  report.check(
    'one device may control two projects at once',
    holds(await badge(desktop, BRAVO)),
    `desktop on bravo: ${JSON.stringify(await badge(desktop, BRAVO))}`,
  )

  const tabletAgain = await badgeAt(tablet, ALPHA)
  const desktopViewer = await badgeAt(desktop, ALPHA)
  report.check(
    'and claiming one leaves the other exactly as it was',
    holds(tabletAgain) && desktopViewer === `Viewer · ${TABLET.label} has control`,
    `alpha on the tablet: ${JSON.stringify(tabletAgain)}, ` +
      `on the desktop: ${JSON.stringify(desktopViewer)}`,
  )
  await look(desktop, ALPHA)

  // =========================================================================
  // E. A controller that goes away is held, then let go (§二十七 case 3, §八)
  // =========================================================================

  await look(tablet, ALPHA)
  report.check(
    'the tablet still holds the lease before it disappears',
    holds(await badge(tablet, ALPHA)),
    `tablet badge: ${JSON.stringify(await badge(tablet, ALPHA))}`,
  )

  const claudesBefore = agentCount()
  await tablet.context.close()
  await look(desktop, ALPHA)

  // Polled hard: the grace this run is configured with is several seconds, and
  // "held for a while" is a state that has to be caught while it is happening.
  const held = await waitFor(
    async () => (await badge(desktop, ALPHA)) === `Viewer · ${TABLET.label} is away`,
    15_000,
    150,
  )
  report.check(
    'a controller whose connection drops is held, rather than given to whoever is watching',
    held,
    `desktop badge: ${JSON.stringify(await badge(desktop, ALPHA))}`,
  )
  report.check(
    'and nobody may take it while it is being held',
    await controlAction(desktop, ALPHA).isDisabled(),
    `the control button is ${(await controlAction(desktop, ALPHA).isDisabled()) ? 'disabled' : 'enabled'}`,
  )

  const lapsed = await waitFor(
    async () => (await badge(desktop, ALPHA)) === 'Viewer · nobody is in control',
    25_000,
    300,
  )
  report.check(
    'when the controller does not come back, the lease lapses and the project is free',
    lapsed,
    `desktop badge: ${JSON.stringify(await badge(desktop, ALPHA))}`,
  )
  report.check(
    'and none of it touched the runtime',
    sessionAlive(ALPHA) && agentCount() >= claudesBefore,
    `${agentCount()} Claude process(es); the session is alive: ${sessionAlive(ALPHA)}`,
  )

  await takeControl(desktop.page, ALPHA)
  report.check(
    'the device that waited the grace out can then take it',
    holds(await badge(desktop, ALPHA)),
    `desktop badge: ${JSON.stringify(await badge(desktop, ALPHA))}`,
  )

  // =========================================================================
  // F. A drop is not a departure (§二十七 case 4, §八)
  // =========================================================================

  const observer = await openDevice(browser, TABLET)
  await look(observer, ALPHA)
  await waitFor(
    async () => (await badge(observer, ALPHA)) === `Viewer · ${DESKTOP.label} has control`,
    15_000,
    300,
  )
  report.check(
    'a device that comes back later sees who has the terminal now',
    (await badge(observer, ALPHA)) === `Viewer · ${DESKTOP.label} has control`,
    `badge: ${JSON.stringify(await badge(observer, ALPHA))}`,
  )

  // The network goes away, and this is the one place in the suite where the
  // browser is not enough on its own.
  //
  // `setOffline(true)` blocks new connections but does not tear down one that is
  // already open, so on its own it produces a server that goes on believing a
  // controller is present - measured, at ninety seconds, before the first
  // version of this section was rewritten. That is not a defect in the server: a
  // silent socket is a silent socket, and a laptop whose Wi-Fi died looks the
  // same from a server's chair. So the drop is made in two halves that together
  // are what a network going away is: the live connection is closed underneath
  // the client, and the network is taken away so the reconnect cannot succeed.
  const desktopAtDrop = desktop.frames.length
  await desktop.context.setOffline(true)
  report.check(
    'the desktop is genuinely disconnected, rather than merely offline',
    await severConnection(desktop.page),
    'the page had a live socket and it was closed underneath the client',
  )

  // Polled fast: the desktop starts reconnecting within a few hundred
  // milliseconds, and the moment the server suspends the lease is a moment that
  // has to be caught while it is happening.
  const noticed = await waitFor(
    async () => (await badge(observer, ALPHA)) === `Viewer · ${DESKTOP.label} is away`,
    20_000,
    50,
  )
  report.check(
    'a controller that loses its connection is held for it, not handed to the watcher',
    noticed && !holds(await badge(observer, ALPHA)),
    `the watcher sees: ${JSON.stringify(await badge(observer, ALPHA))}`,
  )
  report.check(
    'and the watcher cannot take what is being held for somebody else',
    await controlAction(observer, ALPHA).isDisabled(),
    `the watcher's control button is ` +
      `${(await controlAction(observer, ALPHA).isDisabled()) ? 'disabled' : 'enabled'}`,
  )

  // Left offline long enough for the client to have tried and failed several
  // times, which is what makes the next two checks about the lease rather than
  // about a connection that never actually broke.
  await observer.page.waitForTimeout(1500)
  report.check(
    'and it is still being held while the client tries and fails to come back',
    (await badge(observer, ALPHA)) === `Viewer · ${DESKTOP.label} is away`,
    `the watcher sees: ${JSON.stringify(await badge(observer, ALPHA))}`,
  )

  await desktop.context.setOffline(false)
  const returned = await waitFor(
    async () =>
      (await badge(observer, ALPHA)) === `Viewer · ${DESKTOP.label} has control` &&
      holds(await badge(desktop, ALPHA)),
    30_000,
    200,
  )
  report.check(
    'and gets it back the moment its connection does, without asking again',
    returned && sent(desktop.frames.slice(desktopAtDrop), 'control.request').length === 0,
    `the watcher sees: ${JSON.stringify(await badge(observer, ALPHA))}, ` +
      `the controller sees: ${JSON.stringify(await badge(desktop, ALPHA))}, ` +
      `${sent(desktop.frames.slice(desktopAtDrop), 'control.request').length} request(s) sent while away`,
  )

  // The same claim by the other route a connection is lost, and this one is
  // exact: a reload. A tab that is killed and restored keeps its client
  // identifier in sessionStorage, so this is a reconnection rather than a second
  // viewer - and the lease it resumes is one it never released.
  await desktop.page.reload({ waitUntil: 'domcontentloaded' })
  await look(desktop, ALPHA)
  const afterReload = await waitFor(
    async () => holds(await badge(desktop, ALPHA)),
    20_000,
    300,
  )
  report.check(
    'reloading the page is a reconnection, not a new device asking for control',
    afterReload,
    `desktop badge: ${JSON.stringify(await badge(desktop, ALPHA))}`,
  )

  // =========================================================================
  // G. A restarted server does not hand out what it no longer remembers (§三十三)
  // =========================================================================

  const claudesAtRestart = agentCount()
  stopServer()
  await look(desktop, ALPHA)
  const sawOutage = await waitFor(
    async () => /Reconnecting|Connecting|Could not/.test(await status(desktop, ALPHA)),
    30_000,
    400,
  )
  report.check(
    'a browser notices the server going away rather than showing a live terminal',
    sawOutage,
    `status: ${JSON.stringify(await status(desktop, ALPHA))}`,
  )

  // Written straight into tmux, with no AgentMux server in the picture at all.
  // That it lands is itself the proof that the session does not depend on the
  // server that was watching it.
  const whileDown = `WHILE-DOWN-${Date.now().toString(36)}`
  wsl(
    `tmux -S ${fixtures.dataDir}/tmux/${ALPHA.id}.sock ` +
      `send-keys -t amx-${ALPHA.id} 'echo ${whileDown}' Enter`,
  )
  await desktop.page.waitForTimeout(800)

  startServer()
  const backUp = await waitFor(async () => /Live/.test(await status(desktop, ALPHA)), 40_000, 400)
  const caughtUp = await waitFor(
    async () => (await renderedText(desktop, ALPHA)).includes(whileDown),
    20_000,
    400,
  )
  report.check(
    'the terminal comes back to the browser, showing what happened while the server was down',
    backUp && caughtUp,
    `status: ${JSON.stringify(await status(desktop, ALPHA))}, ` +
      `the marker from the outage is on screen: ${caughtUp}`,
  )
  report.check(
    'and the runtime outlived the restart entirely',
    sessionAlive(ALPHA) && agentCount() >= claudesAtRestart,
    `${claudesAtRestart} Claude process(es) before, ${agentCount()} after`,
  )

  // The watcher is aimed at the same project before it is asked about it. Its
  // panel survived the outage - a browser that unmounted the terminal on
  // disconnect would have nothing to show when the server came back - but the
  // question is asked of a panel either way, and a panel that is on another page
  // answers it with an empty string.
  await look(observer, ALPHA)
  const nobody = await waitFor(
    async () =>
      (await badge(desktop, ALPHA)) === 'Viewer · nobody is in control' &&
      (await badge(observer, ALPHA)) === 'Viewer · nobody is in control',
    25_000,
    300,
  )
  report.check(
    'a restarted server has no controller for the project, and gives it to nobody',
    nobody,
    `desktop: ${JSON.stringify(await badge(desktop, ALPHA))}, ` +
      `the watcher: ${JSON.stringify(await badge(observer, ALPHA))}`,
  )

  await takeControl(desktop.page, ALPHA)
  report.check(
    'so the device that wants it asks again, and is given it at once',
    holds(await badge(desktop, ALPHA)),
    `desktop badge: ${JSON.stringify(await badge(desktop, ALPHA))}`,
  )

  // Two of this suite's own sections take the network away and stop the server,
  // so Chromium's console has things to say that are not defects: a resource it
  // could not fetch, a socket that was closed underneath the page. What would be
  // a defect is uncaught Javascript, which is what this reads.
  const crashes = [...desktop.errors, ...observer.errors].filter((text) =>
    text.startsWith('pageerror:'),
  )
  report.check(
    'neither device threw while the network and the server were taken away',
    crashes.length === 0,
    crashes.length === 0
      ? `${desktop.errors.length + observer.errors.length} console message(s), none of them uncaught`
      : crashes.join('; '),
  )

  const ok = report.summary()
  await browser.close()
  if (!ok) process.exit(1)
}

await main().catch((error) => {
  console.error(error)
  process.exit(1)
})
