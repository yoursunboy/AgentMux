/**
 * Phase 7.4B-1 browser E2E - the console.
 *
 * # What this suite is
 *
 * The phase built the first screen a person reads rather than types into: one
 * page that says which projects need somebody. The claim under test is that it
 * exists, that a browser reaches it, and that it shows what the server sent.
 *
 * # Why the layout is checked here and not in jsdom
 *
 * The console's column count is decided by a hook and painted by a stylesheet,
 * and jsdom evaluates neither. The unit tests assert the number the hook
 * produces; this asserts the number the browser *painted*, by reading the
 * computed `grid-template-columns`. Those are two different claims and only one
 * of them can be made without a rendering engine - so this is where the layout
 * claim lives.
 *
 * # What is deliberately not here
 *
 * Nothing in this suite opens a terminal, and nothing types. The console has no
 * terminal in it: that is what makes it a second page rather than a mode of the
 * first, and a suite that went looking for an xterm here would be asserting the
 * thing the phase said not to build.
 */
import { chromium } from 'playwright'

import { PAGE_URL, Report, openPage, waitFor } from '../lib/harness.mjs'

const report = new Report('dashboard')

/** The console's address, on the server's own origin. */
const CONSOLE_URL = new URL('/dashboard', PAGE_URL).toString()

/**
 * columnCount reads how many tracks the browser actually painted.
 *
 * `grid-template-columns` computes to a list of used track sizes - three tracks
 * read as three lengths - so counting them is reading the layout rather than
 * reading the number the application meant to ask for.
 */
async function columnCount(page) {
  return page.evaluate(() => {
    const grid = document.querySelector('.project-grid')
    if (!grid) return 0
    const tracks = getComputedStyle(grid).gridTemplateColumns
    return tracks.split(' ').filter((track) => track.trim() !== '').length
  })
}

/**
 * layoutWidth reads the width the layout was actually computed at.
 *
 * It is not the viewport the context was asked for. Playwright's mobile
 * emulation does not report the requested width back through `innerWidth`, and
 * the column count is decided from that - so a check that printed the requested
 * number would be quoting a figure the browser never used.
 */
async function layoutWidth(page) {
  return page.evaluate(() => window.innerWidth)
}

/** cardNames reads the project names off the cards, in the order they are drawn. */
async function cardNames(page) {
  return page.evaluate(() =>
    Array.from(document.querySelectorAll('.project-card__name')).map((node) => node.textContent),
  )
}

async function main() {
  // The same channel every other suite uses: the machine's own Chrome rather
  // than the bundled Chromium. The bundle needs a headless-shell binary that
  // this project has never installed, so a bare launch fails before a page
  // opens - which is how the first version of this file failed, and the reason
  // it is written the way the others are.
  const browser = await chromium.launch({ channel: 'chrome', headless: true })

  try {
    const desktop = await openPage(browser)
    const { page } = desktop

    // --- it exists, and a browser can reach it ------------------------------

    await page.goto(CONSOLE_URL, { waitUntil: 'domcontentloaded' })

    const loaded = await waitFor(async () => (await page.locator('.server-bar').count()) > 0, 15000)
    report.check(
      'the console answers at /dashboard',
      loaded,
      loaded ? CONSOLE_URL : 'the server bar never appeared - a deep link that 404s would do this',
    )
    if (!loaded) {
      report.summary()
      return
    }

    // --- the server section -------------------------------------------------

    const status = await page.locator('.server-bar__label').textContent()
    report.check('it reports the server state', status?.trim() === 'Online', `server: ${status}`)

    const runtime = await page.locator('.server-bar__item').first().textContent()
    report.check(
      'it says whether a terminal can run on this host',
      /Runtime:\s*(available|unavailable)/.test(runtime ?? ''),
      `runtime: ${runtime?.trim()}`,
    )

    // §6: no provider integration reports a model, so the console says Unknown
    // rather than guessing. A guess here is a name somebody would act on.
    const model = await page.locator('.server-bar__model').textContent()
    report.check(
      'it does not invent a model binding',
      model?.trim() === 'Unknown',
      `model: ${model?.trim()}`,
    )

    // §5: the switch is shown and is not wired to anything.
    const switchDisabled = await page.locator('.server-bar__switch').isDisabled()
    report.check(
      'the provider switch is present and inert',
      switchDisabled,
      switchDisabled ? 'disabled, as Phase 8 is where it would do something' : 'it is enabled',
    )

    // --- the project cards --------------------------------------------------

    const names = await cardNames(page)
    const expected = await page.evaluate(async () => {
      const response = await fetch('/api/projects')
      const body = await response.json()
      return (body.projects ?? []).map((project) => project.name).sort()
    })

    report.check(
      'there is a card for every registered project',
      names.length === expected.length,
      `${names.length} card(s) for ${expected.length} project(s)`,
    )
    report.check(
      'and they are the projects the server has',
      [...names].sort().join(',') === expected.join(','),
      names.join(', '),
    )

    // The four values §7 of the phase brief puts on a card.
    const rows = await page.evaluate(() =>
      Array.from(document.querySelectorAll('.project-card').values())
        .slice(0, 1)
        .map((card) =>
          Array.from(card.querySelectorAll('.project-card__term')).map((node) => node.textContent),
        )
        .flat(),
    )
    report.check(
      'a card carries the runtime, the agent, the attention and the action count',
      rows.join(',') === 'Runtime,Agent,Attention,Actions',
      rows.join(', '),
    )

    // --- the order the server sent ------------------------------------------

    const serverOrder = await page.evaluate(async () => {
      const response = await fetch('/api/controller')
      const body = await response.json()
      return (body.projects ?? []).map((card) => card.name)
    })
    report.check(
      'the console draws the cards in the order the server sorted them',
      names.join(',') === serverOrder.join(','),
      `${names.join(', ')} against ${serverOrder.join(', ')}`,
    )

    // --- the layout ---------------------------------------------------------

    const desktopColumns = await columnCount(page)
    report.check(
      'a desktop paints three columns',
      desktopColumns === 3,
      `${desktopColumns} column(s) at ${await layoutWidth(page)}px`,
    )

    const tablet = await openPage(browser, { viewport: { width: 1024, height: 768 } })
    await tablet.page.goto(CONSOLE_URL, { waitUntil: 'domcontentloaded' })
    await waitFor(async () => (await tablet.page.locator('.project-card').count()) > 0, 15000)
    const tabletColumns = await columnCount(tablet.page)
    report.check(
      'a tablet on its side paints three columns',
      tabletColumns === 3,
      `${tabletColumns} column(s) at ${await layoutWidth(tablet.page)}px`,
    )

    const phone = await openPage(browser, {
      viewport: { width: 390, height: 844 },
      hasTouch: true,
      isMobile: true,
    })
    await phone.page.goto(CONSOLE_URL, { waitUntil: 'domcontentloaded' })
    await waitFor(async () => (await phone.page.locator('.project-card').count()) > 0, 15000)
    const phoneColumns = await columnCount(phone.page)
    report.check(
      'a phone paints one column',
      phoneColumns === 1,
      `${phoneColumns} column(s) at ${await layoutWidth(phone.page)}px`,
    )

    const phoneWidth = await layoutWidth(phone.page)
    const contentWidth = await phone.page.evaluate(() => document.documentElement.scrollWidth)
    report.check(
      'and a phone does not have to scroll sideways to read it',
      contentWidth <= phoneWidth + 1,
      `${contentWidth}px of content in ${phoneWidth}px`,
    )

    // --- the way back -------------------------------------------------------

    await page.goto(CONSOLE_URL, { waitUntil: 'domcontentloaded' })
    await waitFor(async () => (await page.locator('.project-card').count()) > 0, 15000)
    await page.click('.server-bar__link')
    const backOnWorkspace = await waitFor(
      async () => (await page.locator('.global-bar').count()) > 0,
      15000,
    )
    report.check(
      'the workspace link goes back to the workspace',
      backOnWorkspace,
      backOnWorkspace ? 'the global bar is on screen' : 'it stayed on the console',
    )

    // --- nothing threw ------------------------------------------------------

    const crashes = [
      ...desktop.errors,
      ...tablet.errors,
      ...phone.errors,
    ].filter((text) => text.startsWith('pageerror:'))
    report.check(
      'no page threw while the console was read',
      crashes.length === 0,
      crashes.length === 0 ? 'no uncaught error on any of the three devices' : crashes.join('; '),
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
