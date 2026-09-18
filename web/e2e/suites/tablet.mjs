/**
 * Phase 4 browser E2E - the tablet, and the theme.
 *
 * Two things are settled here that the desktop suites cannot settle.
 *
 * The first is the tablet. A terminal on an iPad is not a desktop terminal at a
 * smaller width: there is no Escape key, no Ctrl, no arrows, and the keyboard
 * covers part of the screen when it is up. Each of those is a decision the code
 * makes, and each is checked here against emulation rather than against a real
 * iPad - which nobody has attached to this machine, and which this file does not
 * pretend to have.
 *
 * The second is the palette. The terminal is given its colours by reading the
 * same custom properties the rest of the interface is drawn from, and the way
 * that goes wrong is silently: a hard-coded palette looks fine on the machine
 * it was written on and is unreadable on the other theme. So both themes are
 * rendered and the contrast of the actual painted text is measured.
 */
import { chromium } from 'playwright'
import { execFileSync } from 'node:child_process'
import { awaitProject } from '../lib/harness.mjs'

import { fixtures, PAGE_URL } from '../lib/harness.mjs'

const SHELL = fixtures.projects.filter((entry) => entry.role === 'shell')[0]
const URL = PAGE_URL
const SOCK = `${fixtures.dataDir}/tmux/${SHELL.id}.sock`
const SESSION = `amx-${SHELL.id}`

/** An iPad Air, portrait. Emulated: see the header. */
const IPAD = { width: 820, height: 1180, deviceScaleFactor: 2 }

const results = []
function check(name, ok, detail) {
  results.push({ name, ok, detail })
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? `  -- ${detail}` : ''}`)
}
function wsl(script, input) {
  return execFileSync('wsl', ['-d', 'Ubuntu-24.04', 'bash', '-lc', `${script} || true`], {
    encoding: 'utf8',
    input,
    env: { ...process.env, MSYS_NO_PATHCONV: '1' },
  })
}

/**
 * typeCommand types a command into the fixture's shell.
 *
 * The text goes over stdin into a file and from there into a tmux buffer, rather
 * than being passed as an argument: text handed to `wsl.exe` as an argument
 * reaches a shell that evaluates it on the way, so anything containing a `$(...)`
 * is expanded before it is typed. See the same helper in gap.js.
 */
const PAYLOAD = `${fixtures.runDir}/payload.txt`
function typeCommand(text) {
  wsl(`cat > ${PAYLOAD}`, text)
  wsl(
    `tmux -S ${SOCK} load-buffer -b amxpay ${PAYLOAD} && ` +
      `tmux -S ${SOCK} paste-buffer -b amxpay -t ${SESSION} -d && ` +
      `sleep 0.3 && tmux -S ${SOCK} send-keys -t ${SESSION} Enter`,
  )
}
const pane = () => wsl(`tmux -S ${SOCK} capture-pane -p -t ${SESSION}`)
const paneSize = () =>
  wsl(
    `tmux -S ${SOCK} display-message -p -t ${SESSION} '#{window_width}x#{window_height}'`,
  ).trim()

/**
 * settleShell waits until the fixture's terminal is idle and reading.
 *
 * The stress suite fills this pty with megabytes that take a while to drain, and
 * a run that starts while that is still happening types into a shell that is not
 * reading: the check would then be measuring the previous run. Interrupting
 * first and waiting for an answer is also how a person would recover it.
 */
async function settleShell() {
  const nonce = `SETTLED-${Date.now().toString(36)}`
  for (let attempt = 0; attempt < 8; attempt += 1) {
    wsl(`tmux -S ${SOCK} send-keys -t ${SESSION} C-c`)
    await new Promise((r) => setTimeout(r, 120))
  }
  wsl(`tmux -S ${SOCK} send-keys -t ${SESSION} C-u`)
  await new Promise((r) => setTimeout(r, 200))
  typeCommand(`echo ${nonce}`)
  for (let attempt = 0; attempt < 40; attempt += 1) {
    if (pane().includes(nonce)) return true
    await new Promise((r) => setTimeout(r, 500))
  }
  console.log('    ... the fixture shell did not settle')
  return false
}

/** The text xterm has actually painted, row by row, trimmed. */
async function rendered(page) {
  return page.evaluate(() =>
    Array.from(document.querySelectorAll('.xterm-rows > div'))
      .map((row) => row.textContent.replace(/ /g, ' ').replace(/\s+$/, ''))
      .join('\n')
      .replace(/\n+$/, ''),
  )
}

/** Every input message the page has sent, decoded back to the bytes it carried. */
function sentBytes(frames) {
  return frames
    .filter((f) => f.dir === 'out' && typeof f.payload === 'string')
    .map((f) => {
      try {
        const message = JSON.parse(f.payload)
        return message.type === 'input' ? Buffer.from(message.data, 'base64').toString('latin1') : ''
      } catch {
        return ''
      }
    })
    .join('')
}

/** Anything xterm painted that is not the default colour, for the theme checks. */
const PALETTE_PROBE = () => {
  const root = getComputedStyle(document.documentElement)
  const read = (name) => root.getPropertyValue(name).trim()
  const screen = document.querySelector('.terminal__screen')
  const rows = document.querySelector('.xterm-rows')
  let painted = ''
  for (const span of document.querySelectorAll('.xterm-rows span')) {
    if (span.textContent.trim()) {
      painted = getComputedStyle(span).color
      break
    }
  }
  return {
    text: read('--text'),
    background: read('--bg-sunken'),
    screenBackground: screen ? getComputedStyle(screen).backgroundColor : '',
    rowsColor: rows ? getComputedStyle(rows).color : '',
    painted,
  }
}

/** The channels of a colour, whichever of the two ways CSS writes it. */
function channels(color) {
  const hex = color.match(/^#([0-9a-f]{3}|[0-9a-f]{6})$/i)
  const rgb = color.match(/^rgba?\(([^)]+)\)$/)
  if (hex) {
    const value = hex[1].length === 3 ? hex[1].replace(/./g, (c) => c + c) : hex[1]
    return [0, 2, 4].map((i) => parseInt(value.slice(i, i + 2), 16))
  }
  if (rgb) return rgb[1].split(',').map((p) => Number(p.trim())).slice(0, 3)
  return null
}

/**
 * The tokens in the stylesheet are written as hex and the browser reports what
 * it computed as rgb(), so the two have to be compared as colours rather than
 * as strings - otherwise the check fails on the notation and says nothing about
 * whether the terminal is using the palette.
 */
function sameColor(a, b) {
  const [x, y] = [channels(a), channels(b)]
  return x !== null && y !== null && x.every((channel, i) => channel === y[i])
}

/** WCAG relative luminance, so "readable" is a number rather than an opinion. */
function luminance(color) {
  const rgb = channels(color)
  if (rgb === null) return null
  const channel = (v) => {
    const s = v / 255
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
  }
  return 0.2126 * channel(rgb[0]) + 0.7152 * channel(rgb[1]) + 0.0722 * channel(rgb[2])
}

function contrast(a, b) {
  const [x, y] = [luminance(a), luminance(b)]
  if (x === null || y === null) return 0
  return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05)
}

async function openPage(browser, options = {}) {
  const context = await browser.newContext({
    viewport: options.viewport ?? { width: 1440, height: 900 },
    hasTouch: options.hasTouch ?? false,
    isMobile: options.isMobile ?? false,
    deviceScaleFactor: options.deviceScaleFactor ?? 1,
    colorScheme: options.colorScheme,
  })
  await context.addInitScript(() => {
    // A soft keyboard shrinks the visual viewport and leaves the layout
    // viewport alone. Chrome will not do that for a script, so the one property
    // the code reads is made controllable: everything else about the emulation
    // is Playwright's real touch and mobile emulation.
    const viewport = new EventTarget()
    viewport.height = window.innerHeight
    viewport.width = window.innerWidth
    viewport.offsetTop = 0
    Object.defineProperty(window, 'visualViewport', { configurable: true, get: () => viewport })
    window.__softKeyboard = (open) => {
      viewport.height = window.innerHeight - (open ? 300 : 0)
      viewport.dispatchEvent(new Event('resize'))
    }
  })
  const page = await context.newPage()
  const frames = []
  const errors = []
  page.on('pageerror', (e) => errors.push(e.message))
  page.on('websocket', (socket) => {
    socket.on('framesent', (f) => frames.push({ dir: 'out', payload: f.payload }))
    socket.on('framereceived', (f) => frames.push({ dir: 'in', payload: f.payload }))
  })
  await page.goto(URL, { waitUntil: 'domcontentloaded' })
  await awaitProject(page, SHELL)
  await page.waitForSelector('.xterm-screen', { timeout: 20000 })
  await page.waitForTimeout(2500)
  return { context, page, frames, errors }
}

async function main() {
  const browser = await chromium.launch({ channel: 'chrome', headless: true })
  await settleShell()

  // ------------------------------------------------------- A. the iPad, emulated
  {
    const { context, page, frames, errors } = await openPage(browser, {
      viewport: { width: IPAD.width, height: IPAD.height },
      deviceScaleFactor: IPAD.deviceScaleFactor,
      isMobile: true,
      hasTouch: true,
    })

    const coarse = await page.evaluate(() => matchMedia('(pointer: coarse)').matches)
    check(
      'the emulation is a touch device as far as the page is concerned',
      coarse,
      `pointer: coarse -> ${coarse}`,
    )

    // The row of keys the soft keyboard does not have. A tablet's keyboard has
    // letters; a terminal is mostly driven by what it does not have.
    const visible = await page.locator('.terminal__keys').isVisible()
    const labels = await page.locator('.terminal__keys button').evaluateAll((nodes) =>
      nodes.map((n) => n.getAttribute('aria-label')),
    )
    check(
      'the keys a soft keyboard lacks are offered, and only on a touch device',
      visible && labels.length >= 5,
      `visible: ${visible}, ${labels.length} key(s): ${labels.join(', ')}`,
    )

    const heights = await page
      .locator('.terminal__keys button')
      .evaluateAll((nodes) => nodes.map((n) => Math.round(n.getBoundingClientRect().height)))
    check(
      'every one of them is large enough to hit with a finger',
      heights.every((h) => h >= 40),
      `heights ${heights.join(', ')}px`,
    )

    const overflow = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      innerWidth: window.innerWidth,
    }))
    check(
      'the page does not scroll sideways on a tablet',
      overflow.scrollWidth <= overflow.innerWidth + 1,
      `${overflow.scrollWidth}px of content in ${overflow.innerWidth}px of viewport`,
    )

    // The size the terminal actually gets, which is the thing a person feels.
    // Eighty columns is where a full-screen program stops truncating its own
    // interface, and twenty rows is about the least a prompt box and a few lines
    // of answer fit in - below either, a terminal is a viewer rather than a
    // terminal.
    const atLoad = paneSize()
    const [cols, rows] = atLoad.split('x').map(Number)
    const box = await page.evaluate(() => {
      const rect = document.querySelector('.terminal__screen').getBoundingClientRect()
      return { width: Math.round(rect.width), height: Math.round(rect.height) }
    })
    check(
      'the terminal is given a size a full-screen program is designed for',
      cols >= 80 && rows >= 20,
      `${atLoad} (${cols} columns, ${rows} rows) in an ${IPAD.width}x${IPAD.height} viewport, screen box ${box.width}x${box.height}px`,
    )

    // Typing with the soft keyboard: tap the terminal to focus it, then type as
    // a keyboard would.
    await page.locator('.terminal__screen').tap()
    await page.waitForTimeout(300)
    await page.keyboard.press('Control+c')
    await page.waitForTimeout(400)
    await page.keyboard.type('echo ipad-touch-typing')
    await page.keyboard.press('Enter')
    await page.waitForTimeout(1200)
    check(
      'a tap focuses the terminal and typing reaches the session',
      pane().includes('ipad-touch-typing'),
      '',
    )

    // And the keys themselves, each checked as the exact bytes it must send -
    // an Escape that sent the word "Escape" would pass a weaker test.
    const before = frames.length
    const expected = [
      ['Escape', ''],
      ['Tab', '\t'],
      ['Interrupt', ''],
      ['Up arrow', '[A'],
    ]
    for (const [name] of expected) {
      await page.locator(`.terminal__keys button[aria-label="${name}"]`).tap()
      await page.waitForTimeout(120)
    }
    await page.waitForTimeout(300)
    const sent = sentBytes(frames.slice(before))
    check(
      'tapping the key row sends the raw bytes those keys are',
      expected.every(([, bytes]) => sent.includes(bytes)),
      JSON.stringify(sent),
    )

    const focused = await page.evaluate(() => document.activeElement?.className ?? '')
    check(
      'the focus goes back to the terminal, so the keyboard stays up',
      focused.includes('xterm-helper-textarea'),
      `active element: ${focused || 'none'}`,
    )

    // The soft keyboard. It covers part of the screen without changing the
    // layout viewport, and resizing the pty for it would make every program
    // inside redraw at a size nobody is looking at - repeatedly, since the
    // keyboard animates.
    const sizeBefore = paneSize()
    await page.evaluate(() => window.__softKeyboard(true))
    await page.waitForTimeout(1500)
    const sizeWithKeyboard = paneSize()
    await page.evaluate(() => window.__softKeyboard(false))
    await page.waitForTimeout(1000)
    check(
      'a soft keyboard does not resize the pty',
      sizeWithKeyboard === sizeBefore,
      `${sizeBefore} -> ${sizeWithKeyboard} with the keyboard up`,
    )

    // ...but the screen it was hiding is still there, and the terminal is
    // measured again once it goes away.
    await page.setViewportSize({ width: IPAD.width, height: 900 })
    await page.waitForTimeout(1500)
    const sizeAfter = paneSize()
    check(
      'and a real change of size still resizes it',
      sizeAfter !== sizeBefore,
      `${sizeBefore} -> ${sizeAfter} after the viewport itself changed`,
    )
    check('no page errors on the tablet', errors.length === 0, errors.join('; '))
    await context.close()
  }

  // Turned on its side the same tablet has room for a project panel and the
  // manager at once, and the terminal must not be what pays for the company.
  {
    const { context, page, errors } = await openPage(browser, {
      viewport: { width: IPAD.height, height: IPAD.width },
      deviceScaleFactor: IPAD.deviceScaleFactor,
      isMobile: true,
      hasTouch: true,
    })
    const size = paneSize()
    const [cols, rows] = size.split('x').map(Number)
    const panels = await page.evaluate(
      () => getComputedStyle(document.querySelector('.workspace')).gridTemplateColumns.split(' ').length,
    )
    // Columns are asserted and rows are reported. A landscape tablet with two
    // rows of panels gives each terminal about ten rows, which is what a grid
    // is for - watching, not working - and Focus is where sustained work
    // happens. A rows threshold here would only be a number this file happens
    // to agree with; the measurement is the evidence.
    check(
      'and on its side, with the manager alongside, it is still a real terminal',
      cols >= 80,
      `${size} (${cols} columns, ${rows} rows) with ${panels} panel(s) side by side`,
    )
    check('no page errors in landscape', errors.length === 0, errors.join('; '))
    await context.close()
  }

  // ------------------------------------------- B. Redraw, from the panel itself
  {
    // The one resync a person can ask for without reloading. It is worth
    // checking against a live socket rather than a double: the message has to
    // survive the client, reach the server, and come back as a screen.
    const { context, page, frames, errors } = await openPage(browser)
    const snapshotsBefore = frames.filter(
      (f) => f.dir === 'in' && typeof f.payload !== 'string' && f.payload[1] === 2,
    ).length

    typeCommand('echo REDRAW-MARKER-9')
    await page.waitForTimeout(900)
    // In the grid the Redraw action is in the panel's menu: a grid header has
    // room for a name, a state and two buttons, and the panel that has room for
    // more is the one in focus.
    await page.getByRole('button', { name: `Actions for ${SHELL.name}` }).click()
    await page.getByRole('menuitem', { name: 'Redraw terminal' }).click()

    let snapshotsAfter = snapshotsBefore
    const deadline = Date.now() + 15000
    while (Date.now() < deadline) {
      snapshotsAfter = frames.filter(
        (f) => f.dir === 'in' && typeof f.payload !== 'string' && f.payload[1] === 2,
      ).length
      if (snapshotsAfter > snapshotsBefore) break
      await page.waitForTimeout(300)
    }
    check(
      'Redraw asks the server for the terminal and is sent a fresh screen',
      snapshotsAfter > snapshotsBefore,
      `${snapshotsBefore} snapshot(s) before, ${snapshotsAfter} after`,
    )

    const painted = await rendered(page)
    check(
      'and the screen that comes back is the terminal as it is now',
      painted.includes('REDRAW-MARKER-9'),
      painted.split('\n').filter((l) => l.trim()).slice(-1)[0],
    )
    check('no page errors while redrawing', errors.length === 0, errors.join('; '))
    await context.close()
  }

  // --------------------------------------------------- C. the terminal is themed
  {
    const schemes = {}
    for (const scheme of ['dark', 'light']) {
      const { context, page } = await openPage(browser, { colorScheme: scheme })
      schemes[scheme] = await page.evaluate(PALETTE_PROBE)
      // The terminal's *default* text colour, which is the one this palette
      // sets. `painted` is whatever a program happened to put on the first
      // span - on this fixture a pale blue the shell's prompt uses, which is a
      // colour chosen for a dark background and unreadable on a light one. That
      // is the program's choice and not the palette's: rewriting what a program
      // paints is exactly what a terminal must not do.
      schemes[scheme].contrast = contrast(
        schemes[scheme].rowsColor,
        schemes[scheme].screenBackground,
      )
      await context.close()
    }

    check(
      'the terminal is painted in the palette the page is using, not one of its own',
      !sameColor(schemes.dark.text, schemes.light.text) &&
        !sameColor(schemes.dark.background, schemes.light.background) &&
        sameColor(schemes.dark.rowsColor, schemes.dark.text) &&
        sameColor(schemes.light.rowsColor, schemes.light.text),
      `dark ${schemes.dark.rowsColor} on ${schemes.dark.background}; light ${schemes.light.rowsColor} on ${schemes.light.background}`,
    )
    check(
      'and the text it paints is readable in both of them',
      schemes.dark.contrast >= 3 && schemes.light.contrast >= 3,
      `contrast dark ${schemes.dark.contrast.toFixed(1)}:1, light ${schemes.light.contrast.toFixed(1)}:1` +
        ` (dark: ${schemes.dark.painted} on ${schemes.dark.screenBackground}; ` +
        `light: ${schemes.light.painted} on ${schemes.light.screenBackground})`,
    )
  }

  const failed = results.filter((r) => !r.ok)
  console.log(`\n${results.length - failed.length}/${results.length} checks passed`)
  await browser.close()
  if (failed.length) process.exit(1)
}

await main().catch((error) => {
  console.error(error)
  process.exit(1)
})
