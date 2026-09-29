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
 * is built - `internal/claude/permission_test.go` renders the line and
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
      startMode === 'manual',
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
      marked === 'manual',
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

    await choose(page, 'manual')
    const restored = await waitFor(async () => (await serverMode(page)) === 'manual', 10000)
    report.check(
      'the fixture is left on the mode it started with, for the suites after this one',
      restored,
      `the settings endpoint answers ${JSON.stringify(await serverMode(page))}`,
    )

    // --- nothing threw ------------------------------------------------------

    const crashes = errors.filter((text) => text.startsWith('pageerror:'))
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
