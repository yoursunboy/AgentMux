/**
 * The terminal itself: xterm.js, wired to a session.
 *
 * # What this component does and does not decide
 *
 * It draws bytes and it reports what the person did. It does not decide what a
 * frame means - that is the client's, because a gap has to be detected in one
 * place - and it does not decide whether a resize is worth sending, which is in
 * sizing.ts. What is left here is the part that genuinely belongs to a view:
 * measuring the element, moving the viewport, and telling the difference
 * between "output arrived" and "output arrived while you were reading something
 * further up".
 *
 * # Why the scroll position is nobody else's business
 *
 * Where the viewport is looking is a property of this browser and this moment:
 * two people watching one terminal are looking at different lines, and a server
 * that knew where either of them was would have to pick one. So nothing here is
 * ever sent. The only thing that leaves this component is the size, and that is
 * sent because a pty has exactly one and the program inside it draws for it.
 */
import { useCallback, useEffect, useRef, useState } from 'react'
import { FitAddon } from '@xterm/addon-fit'
import { Unicode11Addon } from '@xterm/addon-unicode11'
import { Terminal } from '@xterm/xterm'

import '@xterm/xterm/css/xterm.css'

import { clampSize, keyboardIsOpen, ResizeScheduler, type TerminalSize } from '../terminal/sizing'
import type { TerminalSession, TerminalSink } from '../terminal/useTerminal'

interface TerminalViewProps {
  session: TerminalSession
  /**
   * Whether the terminal accepts typing. It is false while the connection is
   * down, so that keystrokes are refused by the thing that knows they would be
   * lost rather than disappearing silently.
   */
  interactive: boolean
}

/**
 * The keys a soft keyboard does not have.
 *
 * A tablet's keyboard offers letters and punctuation and very little else, and
 * a terminal is driven by the keys it does not offer: escape to leave a mode,
 * tab to complete, Ctrl+C to interrupt, arrows to move through a menu. Each
 * entry here sends exactly the bytes the real key sends, through the same input
 * path a keystroke takes, so this is not a second way of talking to the
 * terminal - it is the same way, from a place a finger can reach.
 *
 * The row is in the document for every browser and is shown only where there is
 * no keyboard to speak of. See `.terminal__keys` in the stylesheet.
 */
const TOUCH_KEYS: ReadonlyArray<{ label: string; name: string; bytes: string }> = [
  { label: 'esc', name: 'Escape', bytes: '' },
  { label: 'tab', name: 'Tab', bytes: '\t' },
  { label: '^C', name: 'Interrupt', bytes: '' },
  { label: '↑', name: 'Up arrow', bytes: '[A' },
  { label: '↓', name: 'Down arrow', bytes: '[B' },
  { label: '←', name: 'Left arrow', bytes: '[D' },
  { label: '→', name: 'Right arrow', bytes: '[C' },
]
/**
 * themeFromDocument reads the palette the rest of the interface is drawn in.
 *
 * The terminal needs one palette of its own to give xterm, and hard-coding it
 * would mean a second copy of the colours that silently stops matching the
 * first. The fallbacks are the dark theme's values, which is what a stylesheet
 * that has not been applied yet looks like.
 */
function themeFromDocument(): Record<string, string> {
  const styles =
    typeof getComputedStyle === 'function' ? getComputedStyle(document.documentElement) : null
  const read = (name: string, fallback: string): string => {
    const value = styles?.getPropertyValue(name)?.trim()
    return value ? value : fallback
  }
  const text = read('--text', '#e6e9ef')
  const background = read('--bg-sunken', '#0b0d11')
  return {
    background,
    foreground: text,
    cursor: text,
    cursorAccent: background,
    selectionBackground: read('--border-strong', '#38404d'),
    black: background,
    brightBlack: read('--text-muted', '#9aa4b2'),
    white: text,
    brightWhite: text,
  }
}

export function TerminalView({ session, interactive }: TerminalViewProps) {
  const hostRef = useRef<HTMLDivElement | null>(null)
  const termRef = useRef<Terminal | null>(null)
  /** Whether new output should bring the viewport back to the bottom. */
  const followingRef = useRef(true)

  const [following, setFollowing] = useState(true)
  const [unread, setUnread] = useState(false)

  const jumpToLatest = useCallback(() => {
    const term = termRef.current
    if (!term) return
    term.scrollToBottom()
    setUnread(false)
    term.focus()
  }, [])

  useEffect(() => {
    const host = hostRef.current
    if (!host) return

    const term = new Terminal({
      fontFamily:
        'ui-monospace, "Cascadia Mono", "SF Mono", Menlo, Consolas, "Liberation Mono", monospace',
      fontSize: 13,
      lineHeight: 1.2,
      cursorBlink: true,
      scrollback: 5000,
      // The Unicode 11 addon needs the proposed API to install its width table.
      allowProposedApi: true,
      // Option as Meta, which is what a terminal on a Mac is expected to do.
      macOptionIsMeta: true,
      theme: themeFromDocument(),
    })
    const fitAddon = new FitAddon()
    term.loadAddon(fitAddon)
    const unicode = new Unicode11Addon()
    term.loadAddon(unicode)
    try {
      term.unicode.activeVersion = '11'
    } catch {
      // An xterm build without the proposed API. Every ASCII terminal is
      // unaffected, which is the case that matters most of the time.
    }

    term.open(host)
    termRef.current = term

    /**
     * measure fits the terminal to its element and reports the size.
     *
     * It is the only place a size is produced, so the debounce in the scheduler
     * sees every change and no change is measured twice by two code paths.
     */
    const measure = (): TerminalSize => {
      try {
        fitAddon.fit()
      } catch {
        // A hidden element has no size to fit to. The next measurement, taken
        // when it is visible, is the one that counts.
      }
      return clampSize(term.cols, term.rows)
    }

    const scheduler = new ResizeScheduler(
      (size) => {
        session.setSize(size)
        session.resize(size.cols, size.rows)
      },
      {
        keyboardOpen: () => keyboardIsOpen(window.visualViewport?.height, window.innerHeight),
      },
    )

    const initial = measure()
    session.setSize(initial)
    // The subscribe that follows carries this size, so the first screen the
    // server renders is already the right shape. Recording it here is also what
    // stops the same size being sent again as soon as anything is measured.
    scheduler.adopt(initial)

    const sink: TerminalSink = {
      draw(payload) {
        term.write(payload)
        markUnread()
      },
      show(payload, cols, rows) {
        // Size first, then draw. The screen was rendered for this geometry, so
        // writing it into a differently shaped terminal wraps it wrongly and no
        // later output can repair that.
        if (cols > 0 && rows > 0 && (term.cols !== cols || term.rows !== rows)) {
          term.resize(cols, rows)
        }
        term.write(payload)
        markUnread()
      },
      resize(cols, rows) {
        // The server's answer is the truth, not the request: the backend may
        // have clamped it. Recording it as sent is what keeps our own
        // acknowledgement from being echoed back as another resize.
        scheduler.adopt({ cols, rows })
        if (term.cols !== cols || term.rows !== rows) term.resize(cols, rows)
      },
    }

    const detach = session.attach(sink)

    /** markUnread records that output arrived while the viewport was up. */
    const markUnread = (): void => {
      if (followingRef.current) return
      setUnread(true)
    }

    const onData = term.onData((data) => session.input(data))
    // xterm reports a resize for every resize call, including the ones this
    // component makes itself. The scheduler is what decides whether any of them
    // is worth telling the server about.
    const onResize = term.onResize(() => scheduler.request(measure()))
    const onScroll = term.onScroll(() => {
      const buffer = term.buffer.active
      const atBottom = buffer.viewportY >= buffer.baseY
      followingRef.current = atBottom
      setFollowing(atBottom)
      if (atBottom) setUnread(false)
    })

    const observer =
      typeof ResizeObserver === 'undefined'
        ? null
        : new ResizeObserver(() => scheduler.request(measure()))
    observer?.observe(host)

    // A window resize changes the font metrics without necessarily changing the
    // element's pixel size, and a soft keyboard changes the visual viewport
    // without changing either. Both are a reason to measure again; neither is a
    // reason to send anything, which is the scheduler's decision.
    //
    // The visual viewport's scroll event is also listened for, because it fires
    // as a keyboard opens and - more importantly - as it closes, and the
    // measurement taken while it was open is the one sizing.ts deliberately
    // refuses to send.
    const remeasure = (): void => scheduler.request(measure())
    window.addEventListener('resize', remeasure)
    window.visualViewport?.addEventListener('resize', remeasure)
    window.visualViewport?.addEventListener('scroll', remeasure)

    term.focus()

    return () => {
      window.removeEventListener('resize', remeasure)
      window.visualViewport?.removeEventListener('resize', remeasure)
      window.visualViewport?.removeEventListener('scroll', remeasure)
      observer?.disconnect()
      scheduler.cancel()
      onData.dispose()
      onResize.dispose()
      onScroll.dispose()
      detach()
      termRef.current = null
      term.dispose()
    }
    // Nothing is listed as a dependency on purpose. The session's callbacks are
    // stable, and everything else here is a DOM object this effect owns for as
    // long as the component exists: rebuilding the terminal on a render would
    // throw away its scrollback and take a fresh screen from the server each
    // time. The component is keyed by project, so a different project is a
    // different terminal rather than a change to this one.
  }, [])

  useEffect(() => {
    const term = termRef.current
    if (!term) return
    // Refusing the keystroke at the terminal is better than accepting it and
    // dropping it: somebody typing into a terminal that is not connected should
    // see that nothing is happening, rather than watch the characters appear
    // and never arrive.
    term.options.disableStdin = !interactive
  }, [interactive])

  return (
    <>
      <div className="terminal">
        <div
          className="terminal__screen"
          ref={hostRef}
          // A terminal is not reachable by keyboard navigation until it is
          // clicked, which is how every terminal works and is also what keeps the
          // page's tab order short.
          onMouseDown={() => termRef.current?.focus()}
          data-testid="terminal-screen"
        />
        {!following && (
          <button type="button" className="terminal__more" onClick={jumpToLatest}>
            <span aria-hidden="true">↓</span> {unread ? 'New output' : 'Jump to latest'}
          </button>
        )}
      </div>
      <div className="terminal__keys" role="group" aria-label="Terminal keys">
        {TOUCH_KEYS.map((key) => (
          <button
            key={key.name}
            type="button"
            className="button button--small"
            aria-label={key.name}
            disabled={!interactive}
            onClick={() => {
              session.input(key.bytes)
              // The button took the focus, and a tablet's keyboard follows the
              // focus. Handing it back is what makes pressing one of these one
              // keystroke rather than the end of typing.
              termRef.current?.focus()
            }}
          >
            {key.label}
          </button>
        ))}
      </div>
    </>
  )
}
