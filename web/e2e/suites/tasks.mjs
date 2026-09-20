/**
 * Phase 7.2 browser E2E - the task and agent session API.
 *
 * # What this suite is, and what it deliberately is not
 *
 * There is no Task UI in this phase, so nothing here clicks a button. The claim
 * under test is the API itself: that a browser, from the server's own origin,
 * can create a task, read it back, start an attempt at it, read that back, and
 * that none of it leaks across projects.
 *
 * The requests go through the page's own `fetch` rather than Node's, which is
 * the whole reason a browser suite exists for an API with no interface. A request
 * made from Node would prove the route answers; a request made from the page
 * proves the browser can reach it - same origin, no preflight the server does not
 * answer, no header the page is not allowed to set. Those are the failures a
 * person would meet and a Node-level test would not.
 *
 * # What it adds to the Go tests
 *
 * Those already pin the model, the lifecycle, the conditional update and every
 * route. What this adds is that the whole thing works over a real socket, through
 * the real server binary, against a database the same code path migrated from
 * nothing.
 *
 * # The fixture projects
 *
 * The runner registered them and started some of their runtimes before this
 * suite ran, and this suite does not start, stop or destroy anything: creating a
 * task is a record and must not touch a terminal, which is one of the claims
 * below.
 */
import { chromium } from 'playwright'

import { openPage, project, Report } from '../lib/harness.mjs'

const report = new Report('tasks')

/**
 * call performs an API request from inside the page.
 *
 * It returns the status and the parsed body rather than throwing, because most
 * of what this suite checks is a refusal: a helper that threw on a 409 would make
 * "the transition was refused" read as an error in the test.
 */
async function call(page, method, path, body) {
  return page.evaluate(
    async ([method, path, body]) => {
      const init = { method }
      if (body !== null) {
        init.body = JSON.stringify(body)
        init.headers = { 'Content-Type': 'application/json' }
      }
      const response = await fetch(`/api${path}`, init)
      const text = await response.text()
      let parsed = null
      try {
        parsed = text === '' ? null : JSON.parse(text)
      } catch {
        // A refusal the server did not write - a 405 from the router, say - is
        // still an answer, and the status is what the check reads.
        parsed = { raw: text }
      }
      return { status: response.status, body: parsed }
    },
    [method, path, body ?? null],
  )
}

/** errorCode reads the stable code out of a failing response. */
function errorCode(response) {
  return response.body?.error?.code ?? ''
}

/** runtimeIdOf is the runtime a project's session runs in. */
const runtimeIdOf = (entry) => `amx-${entry.id}`

async function main() {
  const browser = await chromium.launch({ channel: 'chrome', headless: true })
  try {
    // The two projects the claims are made against.
    //
    // `a` is the flagship: a real Claude session, alive while this runs, so "a
    // task is not a runtime" is asked of a runtime that genuinely exists rather
    // than of one that was never started. `b` is an ordinary shell, which makes
    // the isolation claim cross the two kinds of fixture instead of comparing a
    // project with itself.
    const a = project('claude')
    const b = project('shell')

    const only = process.env.AMX_E2E_ONLY ?? ''
    const sections = [
      ['create', creatingATask],
      ['read', readingATaskBack],
      ['lifecycle', theLifecycle],
      ['session', anAttemptAtATask],
      ['isolation', projectIsolation],
      ['separation', aTaskIsNotARuntime],
    ]

    for (const [name, section] of sections) {
      if (only !== '' && !name.includes(only)) continue
      await section(browser, a, b)
    }
  } finally {
    await browser.close()
  }

  process.exit(report.summary() ? 0 : 1)
}

/** Creating a task: what comes back, and what does not. */
async function creatingATask(browser, a) {
  console.log('\n--- creating a task ---')
  const { context, page } = await openPage(browser)

  const created = await call(page, 'POST', `/projects/${a.id}/tasks`, {
    title: 'Implement Controller Viewer',
  })
  report.check(
    'a browser can create a task through the server it was served from',
    created.status === 201 && created.body?.task?.id?.startsWith('task_'),
    `status ${created.status}, id ${created.body?.task?.id}`,
  )

  const task = created.body?.task ?? {}
  report.check(
    'the task belongs to the project in the path and starts in CREATED',
    task.projectId === a.id && task.status === 'CREATED',
    `projectId ${task.projectId}, status ${task.status}`,
  )
  report.check(
    'the server produced the timestamps, in UTC',
    typeof task.createdAt === 'string' && task.createdAt.endsWith('Z') && task.completedAt === null,
    `createdAt ${task.createdAt}, completedAt ${task.completedAt}`,
  )

  // The title is what the person typed and it comes back unchanged. This is not
  // a security test - the Go tests cover that - it is that the round trip through
  // a real HTTP stack preserves text rather than mangling it.
  const typed = '实现控制器查看器 🚀 "quoted"'
  const odd = await call(page, 'POST', `/projects/${a.id}/tasks`, { title: typed })
  report.check(
    'a title with non-ASCII text and quotes round-trips unchanged',
    odd.status === 201 && odd.body?.task?.title === typed,
    `got ${JSON.stringify(odd.body?.task?.title)}`,
  )

  const tooLong = await call(page, 'POST', `/projects/${a.id}/tasks`, { title: 'x'.repeat(201) })
  report.check(
    'an over-long title is refused rather than stored',
    tooLong.status === 400 && errorCode(tooLong) === 'invalid_task_title',
    `status ${tooLong.status}, code ${errorCode(tooLong)}`,
  )

  const unknownProject = await call(page, 'POST', '/projects/p_ffffffffffffffffffff/tasks', {
    title: 'work',
  })
  report.check(
    'a task cannot be created under a project that does not exist',
    unknownProject.status === 404 && errorCode(unknownProject) === 'project_not_found',
    `status ${unknownProject.status}, code ${errorCode(unknownProject)}`,
  )

  await context.close()
}

/** Reading a task back, by id and in its project's listing. */
async function readingATaskBack(browser, a) {
  console.log('\n--- reading a task back ---')
  const { context, page } = await openPage(browser)

  const title = `read back ${Date.now()}`
  const created = await call(page, 'POST', `/projects/${a.id}/tasks`, { title })
  const id = created.body?.task?.id ?? ''

  const fetched = await call(page, 'GET', `/tasks/${id}`)
  report.check(
    'a task is readable by its own id',
    fetched.status === 200 && fetched.body?.task?.id === id && fetched.body?.task?.title === title,
    `status ${fetched.status}, title ${JSON.stringify(fetched.body?.task?.title)}`,
  )

  const listed = await call(page, 'GET', `/projects/${a.id}/tasks?limit=500`)
  const found = (listed.body?.tasks ?? []).some((task) => task.id === id)
  report.check(
    'the task appears in its project’s listing',
    listed.status === 200 && found,
    `status ${listed.status}, ${listed.body?.count} task(s) in the listing`,
  )

  const filtered = await call(page, 'GET', `/projects/${a.id}/tasks?status=NOPE`)
  report.check(
    'an unknown status filter is refused rather than answered with an empty list',
    filtered.status === 400 && errorCode(filtered) === 'invalid_input',
    `status ${filtered.status}, code ${errorCode(filtered)}`,
  )

  const missing = await call(page, 'GET', '/tasks/task_ffffffffffffffffffffffff')
  report.check(
    'an unknown task is a 404 rather than an empty task',
    missing.status === 404 && errorCode(missing) === 'task_not_found',
    `status ${missing.status}, code ${errorCode(missing)}`,
  )

  await context.close()
}

/** The lifecycle, over HTTP: what moves, and what cannot. */
async function theLifecycle(browser, a) {
  console.log('\n--- the lifecycle ---')
  const { context, page } = await openPage(browser)

  const created = await call(page, 'POST', `/projects/${a.id}/tasks`, { title: 'lifecycle' })
  const id = created.body?.task?.id ?? ''

  const running = await call(page, 'PATCH', `/tasks/${id}`, { status: 'RUNNING' })
  report.check(
    'a task moves from CREATED to RUNNING',
    running.status === 200 && running.body?.task?.status === 'RUNNING',
    `status ${running.status}, task status ${running.body?.task?.status}`,
  )

  const completed = await call(page, 'PATCH', `/tasks/${id}`, { status: 'COMPLETED' })
  report.check(
    'completing a task records when it completed',
    completed.status === 200 && completed.body?.task?.completedAt !== null,
    `status ${completed.status}, completedAt ${completed.body?.task?.completedAt}`,
  )

  // The one edge the directive names by hand. A task reported as finished and
  // then reported as running again would make the first report false.
  const backwards = await call(page, 'PATCH', `/tasks/${id}`, { status: 'RUNNING' })
  report.check(
    'COMPLETED is final: moving back to RUNNING is refused',
    backwards.status === 409 && errorCode(backwards) === 'invalid_status_transition',
    `status ${backwards.status}, code ${errorCode(backwards)}`,
  )

  const after = await call(page, 'GET', `/tasks/${id}`)
  report.check(
    'the refused transition changed nothing',
    after.body?.task?.status === 'COMPLETED',
    `status is now ${after.body?.task?.status}`,
  )

  const renamed = await call(page, 'PATCH', `/tasks/${id}`, { title: 'lifecycle, renamed' })
  report.check(
    'a task can be renamed without changing its status',
    renamed.status === 200 &&
      renamed.body?.task?.title === 'lifecycle, renamed' &&
      renamed.body?.task?.status === 'COMPLETED',
    `status ${renamed.status}, title ${JSON.stringify(renamed.body?.task?.title)}`,
  )

  const noDelete = await call(page, 'DELETE', `/tasks/${id}`)
  report.check(
    'there is no delete for a task',
    noDelete.status === 404 || noDelete.status === 405,
    `DELETE returned ${noDelete.status}`,
  )

  await context.close()
}

/** An attempt at a task: created with no runtime, then bound to one. */
async function anAttemptAtATask(browser, a, b) {
  console.log('\n--- an attempt at a task ---')
  const { context, page } = await openPage(browser)

  const created = await call(page, 'POST', `/projects/${a.id}/tasks`, { title: 'attempts' })
  const taskId = created.body?.task?.id ?? ''

  const first = await call(page, 'POST', `/tasks/${taskId}/sessions`)
  const session = first.body?.session ?? {}
  report.check(
    'an attempt is created with no runtime and no start time',
    first.status === 201 &&
      session.id?.startsWith('sess_') &&
      session.runtimeId === undefined &&
      session.startedAt === null,
    `status ${first.status}, runtimeId ${JSON.stringify(session.runtimeId)}, ` +
      `startedAt ${session.startedAt}`,
  )

  const running = await call(page, 'PATCH', `/sessions/${session.id}`, { status: 'RUNNING' })
  report.check(
    'starting an attempt records when it started',
    running.status === 200 &&
      running.body?.session?.status === 'RUNNING' &&
      running.body?.session?.startedAt !== null,
    `status ${running.status}, startedAt ${running.body?.session?.startedAt}`,
  )

  // The runtime is attached afterwards, and it is deliberately not required to
  // exist: a runtime is destroyed and forgotten while the record of the attempt
  // that used it remains.
  const runtimeId = runtimeIdOf(a)
  const attached = await call(page, 'PATCH', `/sessions/${session.id}`, { runtimeId })
  report.check(
    'a runtime is bound to the attempt after the fact',
    attached.status === 200 && attached.body?.session?.runtimeId === runtimeId,
    `status ${attached.status}, runtimeId ${attached.body?.session?.runtimeId}`,
  )

  const rebound = await call(page, 'PATCH', `/sessions/${session.id}`, {
    runtimeId: runtimeIdOf(b),
  })
  report.check(
    'a bound runtime cannot be replaced',
    rebound.status === 409 && errorCode(rebound) === 'status_conflict',
    `status ${rebound.status}, code ${errorCode(rebound)}`,
  )

  const ended = await call(page, 'PATCH', `/sessions/${session.id}`, { status: 'COMPLETED' })
  report.check(
    'completing an attempt records when it ended and keeps its start',
    ended.status === 200 &&
      ended.body?.session?.endedAt !== null &&
      ended.body?.session?.startedAt !== null,
    `status ${ended.status}, endedAt ${ended.body?.session?.endedAt}`,
  )

  // A second attempt is a second session. Neither replaces the other, which is
  // what this level exists for.
  const second = await call(page, 'POST', `/tasks/${taskId}/sessions`)
  const listed = await call(page, 'GET', `/tasks/${taskId}/sessions`)
  report.check(
    'a second attempt is a second attempt, and both are kept',
    second.status === 201 && listed.body?.count === 2,
    `${listed.body?.count} attempt(s) after the second one`,
  )

  const waiting = await call(page, 'PATCH', `/sessions/${second.body?.session?.id}`, {
    status: 'WAITING',
  })
  report.check(
    'WAITING is a task status and not an attempt status',
    waiting.status === 400 && errorCode(waiting) === 'invalid_input',
    `status ${waiting.status}, code ${errorCode(waiting)}`,
  )

  const noDelete = await call(page, 'DELETE', `/sessions/${session.id}`)
  report.check(
    'there is no delete for an attempt',
    noDelete.status === 404 || noDelete.status === 405,
    `DELETE returned ${noDelete.status}`,
  )

  await context.close()
}

/** Nothing leaks between projects. */
async function projectIsolation(browser, a, b) {
  console.log('\n--- project isolation ---')
  const { context, page } = await openPage(browser)

  const mine = await call(page, 'POST', `/projects/${a.id}/tasks`, { title: `A only ${Date.now()}` })
  const theirs = await call(page, 'POST', `/projects/${b.id}/tasks`, {
    title: `B only ${Date.now()}`,
  })

  const listedForB = await call(page, 'GET', `/projects/${b.id}/tasks?limit=500`)
  const leaked = (listedForB.body?.tasks ?? []).some(
    (task) => task.id === mine.body?.task?.id || task.projectId === a.id,
  )
  report.check(
    'one project’s listing does not contain another’s tasks',
    listedForB.status === 200 && !leaked,
    `${listedForB.body?.count} task(s) in B's listing, leaked: ${leaked}`,
  )

  // A task is reached by its own id, so it is readable wherever the id is known
  // - that is what a url is. What must hold is that it still says which project
  // it belongs to, wherever it is read from.
  const fetched = await call(page, 'GET', `/tasks/${mine.body?.task?.id}`)
  report.check(
    'a task carries its project with it wherever it is read',
    fetched.status === 200 && fetched.body?.task?.projectId === a.id,
    `projectId ${fetched.body?.task?.projectId}, want ${a.id}`,
  )

  report.check(
    'the two projects’ tasks are different tasks',
    mine.body?.task?.id !== theirs.body?.task?.id,
    `${mine.body?.task?.id} vs ${theirs.body?.task?.id}`,
  )

  await context.close()
}

/**
 * A task is not a runtime.
 *
 * §二十九 says creating a task must not start anything, and §五 is why: a task is
 * the goal, a runtime is one way of working on it, and a task that needed a
 * process to exist would not be a goal at all.
 */
async function aTaskIsNotARuntime(browser, a) {
  console.log('\n--- a task is not a runtime ---')
  const { context, page } = await openPage(browser)

  const before = await call(page, 'GET', `/projects/${a.id}/runtime`)
  report.check(
    'the fixture project has a runtime to ask about',
    before.status === 200 && typeof before.body?.runtime?.state === 'string',
    `status ${before.status}, state ${before.body?.runtime?.state}`,
  )

  const created = await call(page, 'POST', `/projects/${a.id}/tasks`, { title: 'starts nothing' })
  const taskId = created.body?.task?.id ?? ''

  const after = await call(page, 'GET', `/projects/${a.id}/runtime`)
  report.check(
    'creating a task does not start, stop or destroy a runtime',
    before.body?.runtime?.state === after.body?.runtime?.state &&
      before.body?.runtime?.sessionAlive === after.body?.runtime?.sessionAlive,
    `state ${before.body?.runtime?.state} -> ${after.body?.runtime?.state}, ` +
      `alive ${before.body?.runtime?.sessionAlive} -> ${after.body?.runtime?.sessionAlive}`,
  )

  const sessions = await call(page, 'GET', `/tasks/${taskId}/sessions`)
  report.check(
    'a new task has no attempts until one is asked for',
    sessions.status === 200 && sessions.body?.count === 0,
    `${sessions.body?.count} attempt(s)`,
  )

  // The tasks recorded by this suite are still there and still reachable, which
  // is the other half of "a task outlives the process": whatever happens to a
  // runtime, the record of the work is not what goes with it.
  const listed = await call(page, 'GET', `/projects/${a.id}/tasks?limit=500`)
  report.check(
    'the project’s tasks survive whatever happens to its runtime',
    listed.status === 200 && (listed.body?.count ?? 0) > 0,
    `${listed.body?.count} task(s) recorded`,
  )

  await context.close()
}

await main()
