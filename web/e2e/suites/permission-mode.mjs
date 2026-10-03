/**
 * Phase 7.5.1 browser E2E - the console's permission mode menu.
 *
 * # What this suite is
 *
 * The phase added one control to a project card: a menu that chooses what the
 * project's *next* Claude asks before it acts. The claim under test is the whole
 * of it end to end - that the menu is on the card, that choosing from it reaches
 * the server, that the server holds what was chosen afterwards, and that
 * choosing did not disturb anything that was already running.
 *
 * # What Phase 7.5.2 changed here
 *
 * The default became `bypassPermissions`, so the two checks that pinned the
 * fixture to `manual` now pin it to `bypassPermissions` - and the last section
 * was added, because the same phase made the control's touch behaviour part of
 * the spec (§9: a 44px target, and a menu that works without a mouse). Nothing
 * was rewritten: this is the same suite with its expectations brought up to date
 * and one section longer, which is what the brief asked for.
 *
 * The restore at the end therefore puts the fixture back on `bypassPermissions`
 * rather than on `manual`. The fixture was never *configured* - it has no
 * settings row - so what the suite is restoring is the mode an unconfigured
 * project reports, and a row holding the default is indistinguishable from no
 * row through this API.
 *
 * # Why the "did not restart" half is the one that had to be in a browser
 *
 * A menu that saved a preference and a menu that saved a preference *and then
 * restarted the agent so the change took effect* look identical from the
 * client's side. They differ in what happens to somebody's work. That makes it a
 * claim about the world rather than about this process, and the evidence has to
 * come from outside the page: the runtime's own `startedAt`, the tmux session's
 * existence, and the terminal still drawing what it was drawing.
 *
 * The brief's §8 says a restart is what applies the mode, and that the console
 * must say so rather than do it. Both halves are checked here - the sentence on
 * the card, and the session that is still the same session afterwards.
 *
 * # Why it puts the mode back
 *
 * The setting outlives the page: it is a row in the server's database, and the
 * run directory that holds that database is deleted at the end of the run - but
 * a `--keep` run, or a second suite in the same run, would meet whatever this
 * one left. So the last check restores the mode the fixture started with, and
 * says why in as many words.
 *
 * # What is deliberately not here
 *
 * That the mode reaches Claude's command line. That is a claim about a process
 * rather than a screen, it needs a real launch, and it belongs where the launch
 * is built - `internal/claude/permission_test.go` renders the line,
 * `internal/agent/service_test.go` checks the mode reaches the launch, and
 * `cmd/server/claude_e2e_test.go` builds the spec the runtime would run. What
 * this file adds is that a person pressing a button produces the setting those
 * tests are given.
 */
import { chromium } from 'playwright'

import { PAGE_URL, Report, fixtures, openPage, sessionAlive, waitFor } from '../lib/harness.mjs'

const report = new Report('permission-mode')

/** The console's address, on the server's own origin. */
const CONSOLE_URL = new URL('/dashboard', PAGE_URL).toString()

/** The project this suite configures: a shell runtime the fixture started. */
const ALPHA = fixtures.projects.find((entry) => entry.role === 'shell')

/** One project's card, by the accessible name its aria-label gives it. */
const card = (page) => page.locator(`.project-card[aria-label="${ALPHA.name}"]`)

/** The card's permission button. */
const button = (page) => card(page).getByRole('button', { name: 'Permission' })

/** The open menu. */
const menu = (page) => card(page).getByRole('menu')

/**
 * The mode the menu marks as chosen, read from the menu itself.
 *
 * It is read from the mark rather than from `aria-checked` because both are
 * drawn by the same branch, and a suite that read the attribute would pass a
 * menu that had stopped drawing anything.
 */
async function markedMode(page) {
  return page.evaluate((name) => {
    const found = document.querySelector(`.project-card[aria-label="${name}"]`)
    const chosen = found?.querySelector('.permission-menu__item--chosen .permission-menu__name')
    return chosen?.textContent?.trim() ?? ''
  }, ALPHA.name)
}

/**
 * The mode the server holds for this project, asked for directly.
 *
 * The page cannot be the witness to its own write: a menu that stored the value
 * in component state would look identical on screen. This asks the endpoint the
 * setting belongs to.
 */
async function serverMode(page) {
  return page.evaluate(async (id) => {
    const response = await fetch(`/api/projects/${id}/settings`)
    if (!response.ok) return `HTTP ${response.status}`
    const body = await response.json()
    return body?.settings?.permissionMode ?? ''
  }, ALPHA.id)
}

/**
 * The runtime's own account of itself, which is where a restart would show.
 *
 * The response is the API's named envelope rather than the runtime bare - every
 * response in this API is - so the unwrapping is here instead of in the two
 * callers.
 */
async function runtimeOf(page) {
  return page.evaluate(async (id) => {
    const response = await fetch(`/api/projects/${id}/runtime`)
    if (!response.ok) return { state: `HTTP ${response.status}` }
    const body = await response.json()
    return body?.runtime ?? { state: 'no runtime in the response' }
  }, ALPHA.id)
}

/**
 * The Runtime row's badge, as the card draws it.
 *
 * It reads the badge's label rather than the row's text: the row also holds the
 * mode switch's button, so `textContent` is the status and a control run
 * together - which is what a first version of this measured, and why it is
 * spelled out here.
 */
async function runtimeRow(page) {
  return page.evaluate((name) => {
    const found = document.querySelector(`.project-card[aria-label="${name}"]`)
    if (!found) return ''
    const term = Array.from(found.querySelectorAll('.project-card__term')).find(
      (node) => node.textContent === 'Runtime',
    )
    const label = term?.nextElementSibling?.querySelector('.status-badge__label')
    return label?.textContent?.trim() ?? ''
  }, ALPHA.name)
}

/** Choose a mode from the open menu, by the name the item carries. */
async function choose(page, mode) {
  await card(page)
    .getByRole('menuitemradio', { name: new RegExp(`^${mode}`) })
    .click()
}

async function main() {
  const browser = await chromium.launch({ channel: 'chrome', headless: true })

  try {
    const { page, errors } = await openPage(browser)

    // --- the control is on the card -----------------------------------------

    await page.goto(CONSOLE_URL, { waitUntil: 'domcontentloaded' })
    const drawn = await waitFor(async () => (await card(page).count()) > 0, 15000)
    if (!drawn) {
      report.check('the console draws a card for the fixture project', false, ALPHA.name)
      report.summary()
      return
    }

    const before = await runtimeOf(page)
    const runningBefore = before.state === 'RUNNING'
    report.check(
      'the fixture project has a runtime for the menu to leave alone',
      runningBefore,
      `runtime state ${before.state}`,
    )

    report.check(
      'the card offers a Permission control beside the project name',
      (await button(page).count()) === 1,
      `${await button(page).count()} control(s)`,
    )

    const inHeader = await page.evaluate((name) => {
      const found = document.querySelector(`.project-card[aria-label="${name}"]`)
      const header = found?.querySelector('.project-card__header')
      return header !== null && header?.querySelector('.permission-menu') !== null
    }, ALPHA.name)
    report.check(
      'and it sits on the title row, which is what it configures',
      inHeader,
      inHeader ? 'inside .project-card__header' : 'the control is elsewhere on the card',
    )

    // The runtime row still carries the keyboard, which is a different act with
    // a different name. Two controls both called Mode would be a card claiming a
    // keystroke and a setting are the same thing.
    const modeButtons = await card(page).getByRole('button', { name: 'Mode', exact: true }).count()
    report.check(
      'the mode switch keeps its own name, so the two controls cannot be confused',
      modeButtons === 1,
      `${modeButtons} button(s) named exactly "Mode"`,
    )

    // --- the menu -----------------------------------------------------------

    const startMode = await serverMode(page)
    report.check(
      'a project nobody has configured is on the default mode',
      startMode === 'bypassPermissions',
      `the server holds ${JSON.stringify(startMode)}`,
    )

    report.check(
      'the menu is closed until it is pressed',
      (await menu(page).count()) === 0,
      'no menu on the page before the press',
    )

    await button(page).click()
    const opened = await waitFor(async () => (await menu(page).count()) === 1, 5000)
    report.check(
      'pressing it opens a menu named for the setting',
      opened && (await menu(page).getAttribute('aria-label')) === 'Claude Permission Mode',
      opened ? await menu(page).getAttribute('aria-label') : 'no menu appeared',
    )

    const items = await card(page).getByRole('menuitemradio').allTextContents()
    report.check(
      'the menu offers exactly the three modes this build supports',
      items.length === 3 &&
        ['manual', 'acceptEdits', 'bypassPermissions'].every((mode, index) =>
          items[index]?.includes(mode),
        ),
      items.join(' | '),
    )

    const marked = await markedMode(page)
    report.check(
      'and it marks the one that is in force',
      marked === 'bypassPermissions',
      `marked ${JSON.stringify(marked)}`,
    )

    // --- choosing -----------------------------------------------------------

    await choose(page, 'acceptEdits')

    const saved = await waitFor(async () => (await serverMode(page)) === 'acceptEdits', 10000)
    report.check(
      'choosing a mode stores it on the server',
      saved,
      `the settings endpoint answers ${JSON.stringify(await serverMode(page))}`,
    )

    const said = (await card(page).locator('.permission-menu__status').textContent())?.trim() ?? ''
    report.check(
      'and the card says what would apply it',
      said === 'Permission mode updated. Restart Agent to apply.',
      JSON.stringify(said),
    )

    // --- and nothing restarted ----------------------------------------------

    const after = await runtimeOf(page)

    report.check(
      'saving a mode does not restart the runtime it will affect',
      after.startedAt === before.startedAt,
      `startedAt ${before.startedAt} -> ${after.startedAt}`,
    )
    report.check(
      'and the runtime is still up, in the state it was in',
      after.state === before.state,
      `${before.state} -> ${after.state}`,
    )
    report.check(
      'and the tmux session behind it was never replaced',
      await sessionAlive(ALPHA),
      `session ${ALPHA.id} on its own socket`,
    )
    report.check(
      'and the card still draws it as running',
      (await runtimeRow(page)).startsWith('running'),
      `Runtime row reads ${JSON.stringify(await runtimeRow(page))}`,
    )

    // --- what the card shows is the server's value --------------------------

    await page.reload({ waitUntil: 'domcontentloaded' })
    await waitFor(async () => (await card(page).count()) > 0, 15000)

    await button(page).click()
    await waitFor(async () => (await menu(page).count()) === 1, 5000)
    const markedAfterReload = await markedMode(page)
    report.check(
      'a reload draws the stored mode rather than a remembered one',
      markedAfterReload === 'acceptEdits',
      `marked ${JSON.stringify(markedAfterReload)}`,
    )

    // --- and it can be put back ---------------------------------------------

    // Put back on the default. The fixture was never configured, so this writes
    // the first row it has ever had; a row that holds the default is the same
    // thing as no row as far as every read in this API is concerned, which is
    // what makes this a restore rather than a change.
    await choose(page, 'bypassPermissions')
    const restored = await waitFor(async () => (await serverMode(page)) === 'bypassPermissions', 10000)
    report.check(
      'the fixture is left on the mode it started with, for the suites after this one',
      restored,
      `the settings endpoint answers ${JSON.stringify(await serverMode(page))}`,
    )

    // --- the iPad -----------------------------------------------------------

    /*
     * §9 of the phase brief, checked the only way a CSS rule can be: in a
     * browser, on a device the page believes is a touch device.
     *
     * The media query is confirmed before anything is measured. `@media
     * (pointer: coarse)` is what the 44px rule is written under, and Chrome
     * decides that from the emulation rather than from `hasTouch` alone - so a
     * suite that skipped this line could measure a fine pointer, find the rule
     * did not apply, and report a failure about the CSS when the fault was the
     * emulation.
     *
     * The three items are measured as well as the button, because they are what
     * a thumb actually picks. A menu whose entries are smaller than the control
     * that opened it has moved the hard part down a level.
     */
    const ipad = await openPage(browser, {
      viewport: { width: 820, height: 1180 },
      deviceScaleFactor: 2,
      isMobile: true,
      hasTouch: true,
    })
    const touch = ipad.page
    await touch.goto(CONSOLE_URL, { waitUntil: 'domcontentloaded' })
    const drawnOnIPad = await waitFor(async () => (await card(touch).count()) > 0, 15000)
    report.check(
      'the console draws the card on a tablet, so the control can be reached at all',
      drawnOnIPad,
      drawnOnIPad ? 'the card is there at 820x1180' : 'no card on the iPad viewport',
    )

    const coarse = await touch.evaluate(() => matchMedia('(pointer: coarse)').matches)
    report.check(
      'the emulation is a touch device as far as the page is concerned',
      coarse,
      `pointer: coarse -> ${coarse}`,
    )

    const buttonBox = await button(touch).boundingBox()
    report.check(
      'the Permission button is at least 44px tall for a thumb',
      (buttonBox?.height ?? 0) >= 44,
      `${buttonBox?.height ?? 0}px tall`,
    )

    await button(touch).tap()
    await waitFor(async () => (await menu(touch).count()) === 1, 5000)

    const itemBoxes = await touch.evaluate((name) => {
      const found = document.querySelector(`.project-card[aria-label="${name}"]`)
      return Array.from(found?.querySelectorAll('.permission-menu__item') ?? []).map((item) => ({
        name: item.querySelector('.permission-menu__name')?.textContent?.trim() ?? '',
        height: item.getBoundingClientRect().height,
      }))
    }, ALPHA.name)
    report.check(
      'and so is every mode inside it, so the hard part is not one level down',
      itemBoxes.length === 3 && itemBoxes.every((item) => item.height >= 44),
      itemBoxes.map((item) => `${item.name} ${Math.round(item.height)}px`).join(', '),
    )

    // Tapping through a real round trip rather than only measuring: a target of
    // the right size that nothing can be done with is still a target nobody can
    // use. This changes the mode and changes it back, so the fixture is left
    // where the section above left it.
    await card(touch)
      .getByRole('menuitemradio', { name: /^acceptEdits/ })
      .tap()
    const tapped = await waitFor(async () => (await serverMode(touch)) === 'acceptEdits', 10000)
    report.check(
      'a tap chooses a mode, with no mouse involved',
      tapped,
      `the settings endpoint answers ${JSON.stringify(await serverMode(touch))}`,
    )

    await button(touch).tap()
    await waitFor(async () => (await menu(touch).count()) === 1, 5000)
    await card(touch)
      .getByRole('menuitemradio', { name: /^bypassPermissions/ })
      .tap()
    const putBack = await waitFor(async () => (await serverMode(touch)) === 'bypassPermissions', 10000)
    report.check(
      'and the fixture is left on the default once more after the iPad section',
      putBack,
      `the settings endpoint answers ${JSON.stringify(await serverMode(touch))}`,
    )

    await ipad.context.close()

    // --- nothing threw ------------------------------------------------------

    const crashes = [...errors, ...ipad.errors].filter((text) => text.startsWith('pageerror:'))
    report.check(
      'no page threw while the menu was used',
      crashes.length === 0,
      crashes.length === 0 ? 'no uncaught error' : crashes.join('; '),
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
