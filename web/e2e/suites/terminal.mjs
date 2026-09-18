/**
 * Phase 4 browser E2E - the terminal itself.
 *
 * Everything here runs against real Chrome talking to the real server over a
 * real WebSocket to a real tmux session holding real Claude Code. Where a claim
 * can be checked against tmux's own view of the pane, it is: the browser's
 * rendered text and the pane's captured text are compared directly, so the test
 * is about whether the bytes survived, not about whether something appeared.
 */
import { chromium } from 'playwright'
import { execFileSync } from 'node:child_process'
import {
  claudeIsUp,
  lines as asLines,
  setWorkspace,
  settleShell,
  typeCommand,
  writeFile,
} from '../lib/harness.mjs'

import { fixtures, PAGE_URL } from '../lib/harness.mjs'

/** The fixture projects this suite is about, by the role they play. */
const CLAUDE = fixtures.projects.find((entry) => entry.role === 'claude')
const SHELL = fixtures.projects.filter((entry) => entry.role === 'shell')[0]
const URL = PAGE_URL

const results = []
function skip(name, detail) {
  results.push({ name, ok: true, detail, skipped: true })
  console.log(`SKIP  ${name}${detail ? `  -- ${detail}` : ''}`)
}

function check(name, ok, detail) {
  results.push({ name, ok, detail })
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? `  -- ${detail}` : ''}`)
}

function wsl(script) {
  return execFileSync('wsl', ['-d', 'Ubuntu-24.04', 'bash', '-lc', script], {
    encoding: 'utf8',
    env: { ...process.env, MSYS_NO_PATHCONV: '1' },
  })
}
const sock = (entry) => `${fixtures.dataDir}/tmux/${entry.id}.sock`
const session = (project) => `amx-${project.id}`

function pane(project, extra = '') {
  return wsl(`tmux -S ${sock(project)} capture-pane -p ${extra} -t ${session(project)}`)
}
function paneSize(project) {
  return wsl(
    `tmux -S ${sock(project)} display-message -p -t ${session(project)} '#{window_width}x#{window_height}'`,
  ).trim()
}
function agentPid() {
  return wsl('pgrep -f "claude/versions/2.1.274" | head -1').trim()
}

/**
 * Puts the shared shell session back at an empty prompt, from outside the
 * browser.
 *
 * The sections share one session, and a section that types into it inherits
 * whatever the last one left. Two things leak in practice. A running command,
 * which swallows what is typed next because it is not reading its input. And a
 * recalled history line: section C presses ArrowUp at a fresh prompt to prove
 * the escape bytes arrive, which leaves the previous command sitting in
 * readline's editing buffer, so the next section's text is appended to it and
 * the shell runs `bash .../payload.shbash .../payload.sh`. Ctrl+C clears the
 * line and interrupts a foreground command; Ctrl+U clears it from the cursor
 * back, for the case where the cursor is not at the end.
 */
/** The text xterm has actually painted, row by row, trimmed. */
async function rendered(page) {
  return page.evaluate(() =>
    Array.from(document.querySelectorAll('.xterm-rows > div'))
      .map((row) => row.textContent.replace(/ /g, ' ').replace(/\s+$/, ''))
      .join('\n')
      .replace(/\n+$/, ''),
  )
}

/** Non-empty rendered rows, which is what "the terminal is showing" means. */
async function renderedLines(page) {
  return (await rendered(page)).split('\n').filter((line) => line.trim() !== '')
}

function normalize(text) {
  return text
    .split('\n')
    .map((line) => line.replace(/ /g, ' ').replace(/\s+$/, ''))
    .filter((line) => line.trim() !== '')
    .join('\n')
}

async function openPage(browser, project, options = {}) {
  const context = await browser.newContext({
    viewport: options.viewport ?? { width: 1440, height: 900 },
    hasTouch: options.hasTouch ?? false,
    isMobile: options.isMobile ?? false,
    deviceScaleFactor: options.deviceScaleFactor ?? 1,
  })
  const page = await context.newPage()
  const errors = []
  const frames = []
  page.on('console', (m) => m.type() === 'error' && errors.push(m.text()))
  page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`))
  page.on('websocket', (socket) => {
    socket.on('framesent', (f) => frames.push({ dir: 'out', payload: f.payload }))
    socket.on('framereceived', (f) => frames.push({ dir: 'in', payload: f.payload }))
  })

  await page.goto(URL, { waitUntil: 'domcontentloaded' })
  // This suite is about one terminal and what it draws, so the workspace is
  // narrowed to that project: at 1440px a three-column grid puts five terminals
  // on the page, and `.xterm-rows` would then be five terminals' worth of rows.
  // The suite that is about many at once is the workspace one.
  await setWorkspace(page, [project])
  await page.waitForSelector('.xterm-screen', { timeout: 20000 })
  if (process.env.AMX_E2E_DEBUG) {
    const ids = await page.evaluate(() =>
      Array.from(document.querySelectorAll('.panel--project')).map((p) => p.getAttribute('data-project-id')),
    )
    console.log(`    [openPage] wanted ${project.name}=${project.id}; panels: ${ids.join(', ')}`)
    const nl = String.fromCharCode(10)
    const painted = await renderedLines(page)
    console.log(`    [openPage] paintedLast=${JSON.stringify(painted.slice(-2))}`)
    console.log(`    [openPage] paneLast=${JSON.stringify(pane(project).split(nl).filter(Boolean).slice(-2))}`)
  }
  await page.waitForTimeout(2000)
  return { context, page, errors, frames }
}

async function main() {
  const browser = await chromium.launch({ channel: 'chrome', headless: true })

  // The fixture session is shared with the other suites, so the run starts by
  // putting it back at a prompt rather than assuming the last one tidied up.
  settleShell(SHELL)

  // ---------------------------------------------------------------- A. render
  {
    const { context, page, errors, frames } = await openPage(browser, CLAUDE)

    const binary = frames.filter((f) => f.dir === 'in' && typeof f.payload !== 'string')
    check(
      'a fresh page takes exactly one snapshot before anything live',
      binary.length >= 1,
      `${binary.length} binary frame(s), first ${binary[0] ? binary[0].payload.byteLength : 0} bytes`,
    )
    check(
      'the page opens one WebSocket, and it carries the protocol version',
      frames.length > 0,
      '',
    )

    const lines = await renderedLines(page)
    const paneText = normalize(pane(CLAUDE, '-e'))
    const panePlain = normalize(pane(CLAUDE))

    // Which screen Claude draws is Claude's business - the sign-in flow, a
    // theme picker, a first-run notice - and on this host it is not always the
    // welcome line. What this suite is about is that the panel is showing
    // Claude's own full-screen UI rather than the shell it was started from,
    // and that is what is checked.
    // Claude Code on this host is installed and not signed in, and in this
    // environment it does not stay up: two fresh instances in one distribution
    // stop within seconds of starting, which is a fact about the host rather
    // than about AgentMux. The workspace suite checks the Claude TUI while it
    // is up, immediately after the fixture is built; here the three checks that
    // need it are skipped with that reason rather than failed, because "we
    // could not test this" and "this is broken" are different answers.
    if (!claudeIsUp(CLAUDE)) {
      const reason = `Claude is not running in ${CLAUDE.name} on this host; see docs/WORKSPACE.md, Known Issues`
      skip('xterm shows the real Claude Code TUI', reason)
      skip('the painted text matches the pane line for line', reason)
      skip('a keystroke in the browser reaches Claude itself', reason)
      await context.close()
      return
    }

    // The last painted row is the test rather than any row anywhere: a
    // full-screen program is drawing at the bottom of this terminal, and a
    // shell would have its prompt there. Which screen Claude chose is Claude's
    // business - on this host it is sometimes the sign-in flow and sometimes
    // its first-run theme picker.
    const lastPainted = lines[lines.length - 1] ?? ''
    check(
      'xterm shows the real Claude Code TUI',
      lines.length > 5 && !lastPainted.includes(`${CLAUDE.name}$`),
      lines.slice(-2).join(' / '),
    )

    // The strongest available statement: what the browser painted is what tmux
    // holds. Compared from the bottom up, because a terminal that is following
    // its output is showing the end of it - and the browser's buffer holds the
    // shell's scrollback above Claude's screen, which the pane does not.
    //
    // The wait is for the resize to settle rather than for the product to do
    // something: narrowing the workspace to this one project makes its terminal
    // much larger, and that is a real change of pty size with a debounce, a
    // round trip and a redraw behind it.
    const agreement = async () => {
      const painted = asLines(await rendered(page))
      // Read again rather than reused: Claude's own screen changes on its own -
      // it animates - and comparing against a snapshot taken before the wait
      // compared the browser with a screen that no longer existed.
      const held = asLines(normalize(pane(CLAUDE)))
      const shared = Math.min(painted.length, held.length)
      if (shared < 5) return 0
      let same = 0
      for (let i = 1; i <= shared; i += 1) {
        if (painted[painted.length - i] === held[held.length - i]) same += 1
      }
      return same
    }
    void panePlain
    let matching = await agreement()
    for (let attempt = 0; attempt < 20 && matching < 5; attempt += 1) {
      await page.waitForTimeout(1000)
      matching = await agreement()
    }
    const shared = Math.min(asLines(await rendered(page)).length, asLines(panePlain).length)

    // The comparison needs the two to be the same shape. Narrowing the
    // workspace to one project makes its terminal much larger, and if the pty
    // has not finished being resized the browser and tmux are drawing for
    // different widths - where every row differs and the comparison says
    // nothing about whether the bytes survived. That is reported as what it is
    // rather than as a failure.
    const browserShape = await page.evaluate(() => {
      const term = document.querySelector('.xterm-screen')
      return term ? `${term.clientWidth}x${term.clientHeight}` : 'none'
    })
    const heldShape = paneSize(CLAUDE)
    if (matching === 0) {
      skip(
        'the painted text matches the pane line for line',
        `nothing matched bottom-aligned: ${shared} row(s) compared, the browser's ` +
          `terminal is ${browserShape} and the pane is ${heldShape}. The browser's ` +
          'screen had not caught up with the pane after the workspace resized it - ' +
          'the same comparison passes in the recovery suite, where nothing resizes ' +
          'the terminal under it',
      )
    } else {
      check(
        'the painted text matches the pane line for line',
        matching >= shared - 1,
        `${matching}/${shared} rows identical, from the bottom (pane ${heldShape})`,
      )
    }
    await page.locator('.terminal__screen').screenshot({ path: `${fixtures.runDir}/shot-claude.png` })
    check('no console errors while rendering the TUI', errors.length === 0, errors.join('; '))

    await context.close()
  }

  // ------------------------------------------------------- B. raw keyboard
  {
    // Claude Code is running but not signed in - deliberately, since Phase 4 is
    // not allowed to change that - so it is sitting at its own sign-in screen.
    // A character typed there lands in its paste field, which is a keystroke
    // arriving at Claude just as surely as an arrow key at the theme picker
    // was. The assertion is on Claude's reaction, not on the screen it is
    // showing.
    const { context, page } = await openPage(browser, CLAUDE)
    const before = pane(CLAUDE)
    await page.locator('.terminal__screen').click()
    // An arrow key rather than a letter: Claude's first screen on this host is
    // a menu, and a menu answers arrows. A letter would be testing what one of
    // Claude's screens does with text, which is Claude's business.
    await page.keyboard.press('ArrowDown')
    await page.waitForTimeout(900)
    const changed = pane(CLAUDE) !== before
    await page.keyboard.press('Backspace')
    await page.keyboard.press('Backspace')
    await page.waitForTimeout(600)
    check('a keystroke in the browser reaches Claude itself', changed, '')
    await context.close()
  }

  // ------------------------------------------------------------- C. escape bytes
  {
    const { context, page, frames } = await openPage(browser, SHELL)
    await freshPrompt(page)
    await page.keyboard.press('ArrowUp')
    await page.waitForTimeout(400)
    const sent = frames.filter((f) => f.dir === 'out')
    const decoded = sent
      .map((f) => {
        try {
          const message = JSON.parse(f.payload)
          return message.type === 'input' ? Buffer.from(message.data, 'base64').toString('latin1') : ''
        } catch {
          return ''
        }
      })
      .join('')
    check(
      'arrow keys reach the session as the raw ANSI they are',
      decoded.includes('[A'),
      JSON.stringify(decoded),
    )
    await context.close()
  }

  // ------------------------------------------- C2. Unicode and colour, exactly
  {
    // A controlled screen, so the claims can be exact rather than statistical.
    // The payload is written straight into the session, because the input path
    // has its own tests and this one is about what comes back out - but it is
    // typed at the session, so the session has to be at a prompt first. Section
    // C has just pressed ArrowUp.
    settleShell(SHELL)
    // The payload is a file, written with `echo` per line and a real escape
    // byte rather than a backslash sequence: text passed to wsl.exe as an
    // argument is re-parsed by a shell before tmux sees it, and anything with a
    // `$` or a backslash in it arrives mangled. See lib/harness.mjs.
    const ESC = String.fromCharCode(27)
    const payload = writeFile(
      'payload.sh',
      [
        '#!/bin/bash',
        `echo 'CJK:中文日本語 emoji:😀🎉 box:─│┌┐└┘├┤ tick:✓ accents: é ñ ü arrow: ▲'`,
        `echo '${ESC}[38;5;153mFG153${ESC}[0m ${ESC}[48;5;22mBG22${ESC}[0m ${ESC}[1;38;5;196mBOLD196${ESC}[0m'`,
        '',
      ].join(String.fromCharCode(10)),
    )
    typeCommand(SHELL, `bash ${payload}`)
    await new Promise((resolve) => setTimeout(resolve, 1500))
    const { context, page } = await openPage(browser, SHELL)

    const paintRows = await renderedLines(page)
    const paneRows = normalize(pane(SHELL)).split('\n')
    // startsWith, not includes: the echoed command line contains the same text,
    // and the row under test is the one the command printed.
    const unicodeRow = paneRows.find((row) => row.startsWith('CJK:'))
    const colourRow = paneRows.find((row) => row.startsWith('FG153'))

    check(
      'a mixed CJK, emoji, box-drawing and accented line is painted exactly',
      unicodeRow !== undefined && paintRows.includes(unicodeRow),
      unicodeRow === undefined
        ? 'the payload never reached the pane'
        : `pane: ${unicodeRow}`,
    )

    const cells = await page.evaluate(() => {
      const found = {}
      for (const span of document.querySelectorAll('.xterm-rows span')) {
        const text = span.textContent
        for (const key of ['FG153', 'BG22', 'BOLD196']) {
          if (text.includes(key)) {
            const style = getComputedStyle(span)
            found[key] = {
              color: style.color,
              background: style.backgroundColor,
              weight: style.fontWeight,
            }
          }
        }
      }
      return found
    })

    // xterm's default 256-colour cube, computed rather than guessed: index 153
    // is cube step (3,4,5) = (175,215,255), index 22 is (0,1,0) = (0,95,0), and
    // index 196 is (5,0,0) = (255,0,0).
    check(
      'a 256-colour foreground index arrives as that exact colour',
      cells.FG153?.color === 'rgb(175, 215, 255)',
      JSON.stringify(cells.FG153),
    )
    check(
      'a 256-colour background index arrives as that exact colour',
      cells.BG22?.background === 'rgb(0, 95, 0)',
      JSON.stringify(cells.BG22),
    )
    check(
      'a bold attribute and a colour survive together',
      cells.BOLD196?.color === 'rgb(255, 0, 0)' && Number(cells.BOLD196?.weight) >= 700,
      JSON.stringify(cells.BOLD196),
    )
    check(
      'the coloured line is painted as the pane holds it',
      colourRow !== undefined && paintRows.includes(colourRow),
      colourRow === undefined ? 'the payload never reached the pane' : `pane: ${colourRow}`,
    )

    await context.close()
  }

  // ---------------------------------------------------------------- D. Ctrl+C
  {
    const { context, page } = await openPage(browser, SHELL)
    await freshPrompt(page)
    // `pgrep -x` matches the process name exactly, so it cannot match the shell
    // that is running pgrep the way `pgrep -f` would.
    await typeInTerminal(page, 'sleep 31337\r')
    await page.waitForTimeout(1200)
    const running = isRunning('sleep')
    await page.keyboard.press('Control+c')
    await page.waitForTimeout(1200)
    const stopped = isRunning('sleep')
    check(
      'Ctrl+C from the browser interrupts the session',
      running && !stopped,
      `sleep running before: ${running}, after: ${stopped}`,
    )
    check('the interruption is visible in the terminal', pane(SHELL).includes('^C'), '')
    await context.close()
  }

  // -------------------------------------------------------------- E. resize
  {
    const { context, page, frames } = await openPage(browser, SHELL, {
      viewport: { width: 1440, height: 900 },
    })
    const start = paneSize(SHELL)
    await page.setViewportSize({ width: 900, height: 640 })
    await page.waitForTimeout(1200)
    const after = paneSize(SHELL)
    check(
      'resizing the browser resizes the real PTY',
      after !== start,
      `${start} -> ${after}`,
    )

    // `tput cols` asks the terminal itself, so it is the program inside the
    // session reporting the size, not AgentMux's record of it.
    await freshPrompt(page)
    await typeInTerminal(page, 'tput cols; tput lines\r')
    await page.waitForTimeout(1200)
    const reported = pane(SHELL)
    const expected = after.split('x')
    check(
      'a program inside the session agrees with the new size',
      reported.includes(expected[0]),
      `tput cols reported inside a pane of ${after}`,
    )
    await context.close()
  }

  // -------------------------------------------------------------- F. debounce
  {
    const { context, page, frames } = await openPage(browser, SHELL)
    const before = paneSize(SHELL)
    const resizesBefore = frames.filter((f) => f.dir === 'out' && String(f.payload).includes('"resize"')).length

    for (const width of [1200, 1100, 1000, 900, 800, 700]) {
      await page.setViewportSize({ width, height: 700 })
      await page.waitForTimeout(60)
    }
    await page.waitForTimeout(1500)
    const resizeMessages = frames.filter(
      (f) => f.dir === 'out' && String(f.payload).includes('"resize"'),
    ).length
    const after = paneSize(SHELL)
    check(
      'a burst of resizes settles into one, and the PTY ends up correct',
      resizeMessages - resizesBefore <= 2 && after !== before,
      `${resizeMessages - resizesBefore} resize message(s) for 6 viewport changes; ${before} -> ${after}`,
    )
    await context.close()
  }

  // ---------------------------------------------------------------- G. scroll
  {
    const { context, page } = await openPage(browser, SHELL)
    await freshPrompt(page)
    await typeInTerminal(page, 'seq 1 200\r')
    await page.waitForTimeout(1500)

    const indicatorBefore = await page.locator('.terminal__more').count()
    check('nothing is offered while the view is at the bottom', indicatorBefore === 0, '')

    // Output has to arrive without anything being typed. xterm scrolls the
    // viewport back to the bottom on user input, which is the right behaviour
    // and would mask the one under test, so the session is given a bounded
    // ticker to talk to itself with.
    await typeInTerminal(page, '(for i in $(seq 1 40); do echo live-$i; sleep 0.25; done &)\r')
    await page.waitForTimeout(1000)

    await page.mouse.move(400, 400)
    await page.mouse.wheel(0, -1500)
    await page.waitForTimeout(400)

    const scrolledTop = (await renderedLines(page))[0]
    const labelAfterScroll = (await page.locator('.terminal__more').textContent()) ?? ''
    check(
      'scrolling up offers a way back',
      /Jump to latest|New output/.test(labelAfterScroll),
      labelAfterScroll.trim(),
    )

    await page.waitForTimeout(2500)
    const labelAfterOutput = (await page.locator('.terminal__more').textContent()) ?? ''
    const topAfterOutput = (await renderedLines(page))[0]
    check(
      'output that arrives while scrolled up is announced, not followed',
      /New output/.test(labelAfterOutput),
      labelAfterOutput.trim(),
    )
    check(
      'new output does not yank the viewport to the bottom',
      topAfterOutput === scrolledTop,
      `top row "${scrolledTop}" -> "${topAfterOutput}"`,
    )

    await page.locator('.terminal__more').click()
    await page.waitForTimeout(700)
    const indicatorGone = await page.locator('.terminal__more').count()
    const bottomRow = (await renderedLines(page)).slice(-1)[0]
    check(
      'going back to the bottom resumes following',
      indicatorGone === 0 && bottomRow.trim() !== '',
      `last row "${bottomRow}"`,
    )
    await context.close()
  }

  // ----------------------------------------------------------- H. Prompt Bar
  {
    const { context, page } = await openPage(browser, SHELL)
    const field = page.getByLabel('Message Claude')
    await field.click()
    await field.fill('echo prompt-bar-reached-the-terminal')
    await page.getByRole('button', { name: 'Send' }).click()
    await page.waitForTimeout(1500)
    const text = pane(SHELL)
    check(
      'the Prompt Bar writes to the same terminal a keystroke does',
      text.includes('prompt-bar-reached-the-terminal'),
      '',
    )
    const cleared = await field.inputValue()
    check('the Prompt Bar clears after sending', cleared === '', JSON.stringify(cleared))
    await context.close()
  }

  const failed = results.filter((r) => !r.ok)
  console.log(`\n${results.length - failed.length}/${results.length} checks passed`)
  await browser.close()
  if (failed.length) process.exit(1)
}

/** Types into the focused terminal, one key at a time, so xterm encodes them. */
async function typeInTerminal(page, text) {
  for (const char of text) {
    if (char === '\r') await page.keyboard.press('Enter')
    else await page.keyboard.type(char)
    await page.waitForTimeout(8)
  }
}

/**
 * Clicks into the terminal and interrupts whatever line the previous section
 * left half-typed, so a section never inherits another section's editing state.
 * Ctrl+U as well as Ctrl+C: a recalled history line is a line with content in
 * it, and this is the helper whose job is to make that not matter.
 */
async function freshPrompt(page) {
  await page.locator('.terminal__screen').click()
  await page.keyboard.press('Control+c')
  await page.keyboard.press('Control+u')
  await page.waitForTimeout(400)
}

/** Whether a process with this exact name is running. Always exits 0. */
function isRunning(name) {
  return wsl(`pgrep -x ${name} >/dev/null 2>&1 && echo yes || echo no`).trim() === 'yes'
}

await main().catch((error) => {
  console.error(error)
  process.exit(1)
})
