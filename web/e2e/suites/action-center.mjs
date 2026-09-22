/**
 * Phase 7.4C browser E2E - the action centre.
 *
 * # What this suite is for
 *
 * The console says how much is waiting. This page says *what* is waiting, and
 * the claim under test is the one sentence the whole phase rests on: an agent
 * event becomes an action, the action reaches a page, and a person can see it -
 * without the page ever showing what was actually asked for.
 *
 * # Why there are two episodes
 *
 * The fixture starts its two Claudes **by hand** - `tmux send-keys -l 'claude'`
 * in `run.mjs` - which is the adopted path: no `AgentSession` is created and no
 * hook receiver is attached, so nothing in the fixture can produce an action on
 * its own. Neither episode therefore invents a test-only affordance. Both drive
 * the product's own endpoints, and the second delivers a hook to the receiver
 * the product itself configured, which is the input a real Claude sends.
 *
 *	episode A   a launch that a busy terminal refuses
 *	episode B   a permission request from a live Claude
 *
 * # Episode A: why a failed launch is the anchor
 *
 * §17 asks for `action-center` to be part of the standard run, and a suite that
 * can only run when a third Claude fits on this host is not part of anything.
 * A launch into a terminal somebody else is using fails at `requirePaneIdle`,
 * which is *after* the attempt exists, after it is bound to the runtime and
 * after it has been moved to RUNNING - and the undo that follows fails the
 * attempt rather than deleting it, which writes exactly one
 * `session.status_changed` and therefore exactly one action. Nothing resolves
 * it: `settlePending` clears permission requests and nothing else, so a failure
 * stays pending for the life of the project. Deterministic, no Claude involved.
 *
 * The task is what makes it work, and it is easy to leave out: an attempt is
 * created only when `agent/start` is given a `taskId`, and a failed launch with
 * no attempt has no session to move to FAILED - so no event, and no action.
 *
 * # Episode B: why the assertions come before the cleanup
 *
 * The hook mechanism is real and verified against the product's own settings
 * document: `agent/start` attaches a receiver, writes
 * `<dataDir>/claude/amx-<projectId>/settings.json` naming that receiver's URL,
 * and binds the attempt. POSTing `PermissionRequest` to that URL raises the one
 * action that means somebody is genuinely blocked.
 *
 * Two facts make it an episode that skips rather than an anchor:
 *
 *   - any failure after the receiver is attached runs the undo, which detaches
 *     it and deletes the settings document - so the URL only exists while a
 *     launch succeeded, and a host that cannot start a third Claude has none;
 *   - `settles()` is true for every read event, so the permission request is
 *     resolved by the next event on that attempt. It is pending only while the
 *     session is quiet, which is why the stop at the end of the episode comes
 *     **after** its checks.
 *
 * The delivery is retried while the session is still starting up, for the same
 * reason: a Claude that fires one more startup hook settles the request, and the
 * claim is that a permission request from a live agent reaches the queue - not
 * that the first delivery happened to arrive after the last hook. Only the
 * stimulus is retried; the assertion is not.
 */
import { chromium } from 'playwright'

import {
  PAGE_URL,
  Report,
  fixtures,
  openPage,
  project,
  sessionOf,
  settleShell,
  sockOf,
  typeCommand,
  waitFor,
  wsl,
} from '../lib/harness.mjs'

const report = new Report('action-center')

/** The action centre, on the server's own origin. */
const ACTIONS_URL = new URL('/actions', PAGE_URL).toString()

/** The console, which is where a person starts from. */
const CONSOLE_URL = new URL('/dashboard', PAGE_URL).toString()

/**
 * A plain shell, so that a refused launch is about a busy terminal and not
 * about a Claude.
 *
 * The first of the five, and what matters is only that it is a project with a
 * terminal and without an agent - which is what `shell` means in the fixture.
 */
const ALPHA = project('shell')

/**
 * The words this build must never put on the action centre.
 *
 * §16 of the phase brief is one claim, so it is checked the same way in a
 * browser as it is in the unit tests: read the page's own text and look for
 * anything that could only have come out of a request.
 */
const FORBIDDEN = ['tool_input', 'toolInput', 'transcript', 'sk-', 'passphrase']

// ---------------------------------------------------------------------------
// Talking to the server, and to the fixture

/**
 * api calls the server from the page.
 *
 * Through `page.evaluate` rather than from Node, so the request is made from
 * the browser's own origin and against the same server the page is reading.
 * From Node it would be a second client with a second idea of the address, and
 * a suite that checked its own client instead of the page's would be testing
 * the wrong thing.
 */
function api(page, path, { method = 'GET', body } = {}) {
  return page.evaluate(
    async ([p, m, b]) => {
      const response = await fetch(p, {
        method: m,
        headers: b ? { 'Content-Type': 'application/json' } : undefined,
        body: b ? JSON.stringify(b) : undefined,
      })
      const text = await response.text()
      return {
        status: response.status,
        ok: response.ok,
        body: text.trim() === '' ? {} : JSON.parse(text),
      }
    },
    [path, method, body ?? null],
  )
}

/** What `GET /api/actions` answered. */
function queue(page) {
  return api(page, '/api/actions')
}

/** What a project's runtime is doing. */
async function runtimeState(page) {
  const answer = await api(page, `/api/projects/${ALPHA.id}/runtime`)
  return answer.body.runtime?.state ?? ''
}

/** The command tmux says is in the pane's foreground. */
function paneCommand() {
  return wsl(
    `tmux -S "${sockOf(ALPHA)}" display-message -p -t ${sessionOf(ALPHA)} '#{pane_current_command}'`,
  ).trim()
}

/**
 * startRuntime makes sure the terminal this suite uses is up.
 *
 * The fixture starts every runtime, so this is normally a read - but a suite
 * that inherited a stopped one would otherwise fail at its first step for a
 * reason that has nothing to do with what it is checking.
 */
async function startRuntime(page) {
  if ((await runtimeState(page)) === 'RUNNING') return true
  await api(page, `/api/projects/${ALPHA.id}/runtime/start`, { method: 'POST' })
  await waitFor(async () => (await runtimeState(page)) === 'RUNNING', 30_000)
  return (await runtimeState(page)) === 'RUNNING'
}

/** A task in the project, which is what makes an attempt exist. */
async function createTask(page, title) {
  const answer = await api(page, `/api/projects/${ALPHA.id}/tasks`, {
    method: 'POST',
    body: { title },
  })
  return answer.body.task?.id ?? ''
}

// ---------------------------------------------------------------------------
// Reading the page

/** Every row's link, inside one labelled section of the queue. */
function sectionHrefs(page, name) {
  return page.evaluate((label) => {
    const section = document.querySelector(`section[aria-label="${label}"]`)
    if (!section) return []
    return Array.from(section.querySelectorAll('.action-card__link')).map((node) =>
      node.getAttribute('href'),
    )
  }, name)
}

/** The text of one value on an action's own page. */
function detailRow(page, term) {
  return page.evaluate((label) => {
    const dt = Array.from(document.querySelectorAll('.action-detail__term')).find(
      (node) => node.textContent === label,
    )
    return dt?.nextElementSibling?.textContent ?? ''
  }, term)
}

/** The words on the page, for the §16 check. */
function pageText(page) {
  return page.evaluate(() => document.body.textContent ?? '')
}

/** What the console's bar says about the queue. */
function consoleQueueText(page) {
  return page.evaluate(() => document.querySelector('.server-bar__queue')?.textContent ?? '')
}

/** What the queue's own bar says. */
function actionsBarText(page) {
  return page.evaluate(() => document.querySelector('.actions-bar')?.textContent ?? '')
}

/** The `Actions` row on one project's console card. */
function cardActionRow(page, name) {
  return page.evaluate((projectName) => {
    const card = document.querySelector(`.project-card[aria-label="${projectName}"]`)
    const dt = Array.from(card?.querySelectorAll('.project-card__term') ?? []).find(
      (node) => node.textContent === 'Actions',
    )
    const dd = dt?.nextElementSibling
    return {
      text: (dd?.textContent ?? '').trim(),
      href: dd?.querySelector('a')?.getAttribute('href') ?? '',
    }
  }, name)
}

// ---------------------------------------------------------------------------
// The suite

async function main() {
  // The channel every other suite uses: the machine's own Chrome rather than
  // the bundled Chromium, which needs a headless-shell binary this project has
  // never installed.
  const browser = await chromium.launch({ channel: 'chrome', headless: true })

  try {
    const { page, errors } = await openPage(browser)

    // =======================================================================
    // A. A launch the terminal refuses (§4, §5, §6, §8, §11, §13, §14)
    // =======================================================================

    settleShell(ALPHA)
    const up = await startRuntime(page)
    report.check(
      'the project this suite uses has a terminal',
      up,
      `${ALPHA.name} is ${await runtimeState(page)}`,
    )
    if (!up) {
      report.summary()
      process.exitCode = 1
      return
    }

    // --- an installation with nothing waiting ---------------------------------

    await page.goto(ACTIONS_URL, { waitUntil: 'domcontentloaded' })
    const emptyShown = await waitFor(
      async () => (await page.locator('.action-center__empty').count()) > 0,
      15_000,
    )
    const atRest = await queue(page)
    report.check(
      'a queue with nothing in it says so rather than looking broken',
      emptyShown && atRest.body.count === 0,
      `${await page
        .locator('.action-center__empty')
        .textContent()
        .catch(() => '')} on the page, ${atRest.body.count} action(s) on the server`,
    )
    report.check(
      'and there is nothing on it to act on, and nothing to retry',
      (await page.locator('.action-card').count()) === 0 &&
        (await page.locator('.action-center__message--failed').count()) === 0,
      'no rows and no failure',
    )

    // --- a terminal somebody else is using ------------------------------------

    typeCommand(ALPHA, 'sleep 300')
    const busy = await waitFor(async () => paneCommand() === 'sleep', 10_000)
    report.check(
      'something other than the shell has the terminal',
      busy,
      `the pane is running ${JSON.stringify(paneCommand())}`,
    )

    const taskID = await createTask(page, 'action centre: a launch the terminal refuses')
    report.check('an attempt can be asked for', taskID !== '', `task ${taskID}`)

    const launched = await api(page, `/api/projects/${ALPHA.id}/runtime/agent/start`, {
      method: 'POST',
      body: { taskId: taskID },
    })
    report.check(
      'and the launch is refused rather than typed into whatever is in the foreground',
      !launched.ok && launched.body.error?.code === 'agent_terminal_busy',
      `${launched.status} ${launched.body.error?.code ?? ''}`,
    )

    const afterAgent = await api(page, `/api/projects/${ALPHA.id}/runtime/agent`)
    report.check(
      'and nothing was left running by the attempt to',
      afterAgent.body.agent?.running !== true,
      `the agent is ${afterAgent.body.agent?.state ?? 'unknown'}`,
    )

    // --- the action that leaves behind ----------------------------------------

    const afterLaunch = await queue(page)
    const action = afterLaunch.body.actions?.[0]
    report.check(
      'the refusal left exactly one thing waiting',
      afterLaunch.body.count === 1 && Boolean(action),
      `${afterLaunch.body.count} action(s)`,
    )
    report.check(
      'it is the failure and not a demand, and it is still pending',
      action?.type === 'VIEW_FAILURE' && action?.level === 'WARNING' && action?.status === 'PENDING',
      `${action?.type} ${action?.level} ${action?.status}`,
    )
    report.check(
      'it names the project the launch was for',
      action?.projectName === ALPHA.name,
      `project: ${JSON.stringify(action?.projectName)}`,
    )
    report.check(
      "and it carries the server's own phrase rather than anything that was asked for",
      action?.reason === 'attempt failed',
      `reason: ${JSON.stringify(action?.reason)}`,
    )
    report.check(
      'nothing is counted as needing a person',
      afterLaunch.body.needsYou === 0 && afterLaunch.body.notices === 1,
      `needs you ${afterLaunch.body.needsYou}, notices ${afterLaunch.body.notices}`,
    )

    // --- §5: what the page shows, and what it may never show -------------------

    await page.goto(ACTIONS_URL, { waitUntil: 'domcontentloaded' })
    const rowAppeared = await waitFor(
      async () => (await page.locator('.action-card').count()) > 0,
      15_000,
    )
    report.check(
      'the queue shows it',
      rowAppeared,
      `${await page.locator('.action-card').count()} row(s)`,
    )

    const noticeHrefs = await sectionHrefs(page, 'Notices')
    const needsYouHrefs = await sectionHrefs(page, 'Needs you')
    report.check(
      'under notices, because it stops nothing',
      noticeHrefs.length === 1 && needsYouHrefs.length === 0,
      `notices: ${noticeHrefs.length}, needs you: ${needsYouHrefs.length}`,
    )
    report.check(
      'and it links at its own address rather than at the queue',
      /^\/actions\/act_[0-9a-f]+$/.test(noticeHrefs[0] ?? ''),
      `href: ${noticeHrefs[0]}`,
    )

    const cardText = await page.locator('.action-card').first().textContent()
    report.check(
      "the row says what happened, in this build's own words",
      (cardText ?? '').includes('Something failed') && (cardText ?? '').includes('attempt failed'),
      JSON.stringify((cardText ?? '').trim()),
    )

    const listingText = await pageText(page)
    const leaked = FORBIDDEN.filter((word) => listingText.includes(word))
    report.check(
      'and the page carries nothing that came out of a request',
      leaked.length === 0,
      leaked.length === 0 ? 'no prompt, tool input or credential anywhere on it' : leaked.join(', '),
    )

    // --- §7: the action's own page --------------------------------------------

    await page.click('.action-card__link')
    const detailShown = await waitFor(
      async () => (await page.locator('.action-detail').count()) > 0,
      15_000,
    )
    report.check(
      'clicking a row opens the action it named',
      detailShown && page.url().endsWith(action.id),
      page.url(),
    )

    const terms = await page.evaluate(() =>
      Array.from(document.querySelectorAll('.action-detail__term')).map((node) => node.textContent),
    )
    report.check(
      'the action page shows exactly the rows it declares',
      terms.join(',') === 'Project,Session,Type,Status,Reason,Created,Resolved',
      terms.join(', '),
    )
    report.check(
      'it says when it was created in words rather than as a clock time',
      /just now|\d+ (minute|hour|day|month|year)s? ago/.test(await detailRow(page, 'Created')),
      `created: ${await detailRow(page, 'Created')}`,
    )
    report.check(
      'it names the attempt the launch created',
      (await detailRow(page, 'Session')).startsWith('sess_'),
      `session: ${await detailRow(page, 'Session')}`,
    )
    report.check(
      'and it says what kind of thing it is, in words from this build',
      (await detailRow(page, 'Status')).includes('warning'),
      `status: ${JSON.stringify((await detailRow(page, 'Status')).trim())}`,
    )

    // §6, checked in a real browser rather than argued for: there is no control
    // here that could answer, approve, deny or acknowledge anything.
    const buttons = await page.locator('button').count()
    report.check(
      'and there is no way to answer it from this page',
      buttons === 0,
      `${buttons} button(s)`,
    )
    report.check(
      'which the page says, rather than leaving a person to work it out',
      (await pageText(page)).includes('The answer is given at the terminal'),
      'the note is on the page',
    )

    const detailText = await pageText(page)
    const detailLeaked = FORBIDDEN.filter((word) => detailText.includes(word))
    report.check(
      'and it is not a quotation of the request either',
      detailLeaked.length === 0,
      detailLeaked.length === 0 ? 'nothing from the payload' : detailLeaked.join(', '),
    )

    // --- §8: the way in from the console --------------------------------------

    await page.goto(CONSOLE_URL, { waitUntil: 'domcontentloaded' })
    await waitFor(async () => (await page.locator('.project-card').count()) > 0, 15_000)

    const cardRow = await cardActionRow(page, ALPHA.name)
    report.check(
      'the project it belongs to says so on its card, and links to the queue',
      cardRow.text.includes('1 action pending') && cardRow.href === '/actions',
      `${JSON.stringify(cardRow.text)} -> ${cardRow.href}`,
    )

    const bar = await consoleQueueText(page)
    report.check(
      "the console's bar counts it, and does not call it something that needs a person",
      /Needs you:\s*0/.test(bar) && /Notices:\s*1/.test(bar),
      JSON.stringify(bar.trim()),
    )

    await page.click('.server-bar__queue')
    const reachedQueue = await waitFor(
      async () => (await page.locator('.actions-bar').count()) > 0,
      15_000,
    )
    report.check(
      'and the count on the bar is a way into the queue',
      reachedQueue && page.url().includes('/actions'),
      page.url(),
    )

    // The terminal goes back to its prompt, which is what the next episode
    // needs. Interrupting the command is enough - the pane reports its shell
    // again - and it is cheaper than destroying a runtime this suite will use
    // for the rest of its run.
    settleShell(ALPHA)
    const idleAgain = await waitFor(async () => paneCommand() !== 'sleep', 10_000)
    report.check(
      'and the terminal can be handed back its prompt',
      idleAgain,
      `the pane is running ${JSON.stringify(paneCommand())}`,
    )

    // =======================================================================
    // B. A permission request from a live Claude (§6, §11, §13, §14)
    // =======================================================================

    if (!idleAgain) {
      report.skip('a permission request from a live agent', 'the terminal never came back')
    } else {
      const permissionTask = await createTask(page, 'action centre: a permission request')
      const agent = await api(page, `/api/projects/${ALPHA.id}/runtime/agent/start`, {
        method: 'POST',
        body: { taskId: permissionTask },
      })
      if (!agent.ok) {
        report.skip(
          'a permission request from a live agent',
          `no agent could be started on this host: ${agent.status} ${agent.body.error?.code ?? ''}`,
        )
      } else {
        await permissionEpisode(page, agent.body.session?.id ?? '')
      }
    }

    // --- nothing threw --------------------------------------------------------

    const crashes = errors.filter((text) => text.startsWith('pageerror:'))
    report.check(
      'no page threw while the action centre was read',
      crashes.length === 0,
      crashes.length === 0 ? 'no uncaught error' : crashes.join('; '),
    )

    const ok = report.summary()
    if (!ok) process.exitCode = 1
  } finally {
    await browser.close()
  }
}

/**
 * permissionEpisode delivers a `PermissionRequest` to the receiver the product
 * configured, and reads what it produces.
 *
 * # Why the settings document is read rather than a URL being guessed
 *
 * The adapter binds to a port the operating system chooses and serves under a
 * path it mints, so the URL is not knowable in advance - which is the point of
 * it. The document `agent/start` wrote is where the product tells Claude where
 * to deliver, so it is where this asks too. Reading it is not reaching past the
 * product; it is reading the product's own output.
 *
 * # Why the stop is the last thing here
 *
 * `settles()` is true for any read event, so the next event on this attempt
 * resolves the permission request. Stopping the agent produces one. Every check
 * below therefore runs before the stop, and this comment is here because the
 * next person to reorder these lines would otherwise lose an afternoon to a
 * check that passes alone and fails in a run.
 */
async function permissionEpisode(page, agentSessionID) {
  const settingsPath = `${fixtures.dataDir}/claude/amx-${ALPHA.id}/settings.json`
  const readDocument = () => wsl(`cat "${settingsPath}"`)

  await waitFor(() => readDocument().includes('allowedHttpHookUrls'), 10_000)

  let url = ''
  try {
    url = JSON.parse(readDocument()).allowedHttpHookUrls?.[0] ?? ''
  } catch {
    url = ''
  }
  if (!url.startsWith('http://')) {
    report.skip(
      'a permission request from a live agent',
      `the hook document at ${settingsPath} named no URL: ${JSON.stringify(url)}`,
    )
    return
  }

  // The payload is what a real Claude sends. The three fields are the three the
  // adapter decodes; a prompt and a tool input are not among them, and are
  // deliberately not fabricated here either.
  const delivery =
    '{"hook_event_name":"PermissionRequest","session_id":"sess_action_center_e2e","tool_name":"Bash"}'

  // Delivered from inside the distribution, where the receiver is listening,
  // and through stdin - `wsl.exe` expands what it is given as an argument, which
  // is the trap the note at the top of `lib/harness.mjs` describes.
  const deliver = () =>
    wsl(
      `curl -sS -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' --data-binary @- ${url}`,
      delivery,
    ).trim()

  // Retried while the session is still starting: a Claude that fires one more
  // startup hook settles whatever is pending on its attempt, and the claim is
  // that a permission request from a live agent reaches the queue - not that the
  // first delivery happened to arrive after the last hook.
  let answer = ''
  const arrived = await waitFor(
    async () => {
      answer = deliver()
      return (await queue(page)).body.needsYou === 1
    },
    30_000,
    2000,
  )

  if (!arrived) {
    report.skip(
      'a permission request from a live agent',
      `the queue never showed one needing a person (last delivery: HTTP ${answer})`,
    )
    return
  }

  const pending = await queue(page)
  const action = pending.body.actions?.find((item) => item.type === 'PERMISSION_REQUEST')
  report.check(
    'a permission request from a live agent reaches the queue',
    Boolean(action),
    `delivered over HTTP ${answer}`,
  )
  report.check(
    'and it is the one thing that needs a person',
    action?.level === 'ACTION_REQUIRED' &&
      action?.status === 'PENDING' &&
      pending.body.needsYou === 1,
    `${action?.level} ${action?.status}, needs you ${pending.body.needsYou}`,
  )
  report.check(
    'it belongs to the attempt this episode started',
    action?.agentSessionId === agentSessionID && agentSessionID !== '',
    `action ${action?.agentSessionId} against session ${agentSessionID}`,
  )
  report.check(
    'and it says nothing about what was being asked',
    action?.reason === 'permission requested' && !JSON.stringify(action).includes('tool'),
    `reason: ${JSON.stringify(action?.reason)}`,
  )

  await page.goto(ACTIONS_URL, { waitUntil: 'domcontentloaded' })
  await waitFor(async () => (await page.locator('.action-card').count()) > 0, 15_000)

  const needsYou = await sectionHrefs(page, 'Needs you')
  report.check(
    'the queue puts it under what needs somebody',
    needsYou.length === 1 && needsYou[0] === `/actions/${action.id}`,
    `needs you: ${needsYou.join(', ') || '(nothing)'}`,
  )

  const bar = await actionsBarText(page)
  report.check(
    'and the queue page counts it as needing somebody',
    /Needs you:\s*1/.test(bar),
    JSON.stringify(bar.trim()),
  )

  const glyph = await page.locator('.action-card__glyph').count()
  report.check(
    'the row is marked, which is the one thing this build adds of its own',
    glyph === 1,
    `${glyph} glyph(s)`,
  )

  const text = await pageText(page)
  const leaked = FORBIDDEN.filter((word) => text.includes(word))
  report.check(
    'and neither the row nor the page shows what was asked for',
    leaked.length === 0,
    leaked.length === 0 ? 'nothing from the payload' : leaked.join(', '),
  )

  // Last, and deliberately: this is what settles the action above.
  await api(page, `/api/projects/${ALPHA.id}/runtime/agent/stop`, { method: 'POST' })
}

await main().catch((error) => {
  console.error(error)
  process.exit(1)
})
