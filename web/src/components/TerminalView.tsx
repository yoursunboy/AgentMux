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
  /**
   * Whether this browser's own box decides how large the terminal is.
   *
   * It is the same authority as `interactive`, one level down: a terminal has
   * one pty and therefore one shape, and it is the controller's that wins. A
   * viewer is sent the screen as it is - a phone watching the project somebody
   * at a desk is working in is watching a terminal that is not the shape of the
   * phone - so a viewer must not fit its own terminal to its own element. Doing
   * that pulls the pty's rows down into the viewer's scrollback and leaves the
   * box showing the empty part of the screen below them, which is a viewer
   * looking at a terminal that appears to have nothing in it.
   */
  mayResize: boolean
  /**
   * The terminal's text size in pixels.
   *
   * It is a prop rather than a constant because the display modes differ in
   * exactly one way that matters to a terminal, and this is it: a grid panel is
   * a monitoring view where more rows is the point, and a full-screen one is
   * where somebody works. Changing it re-fits the terminal, which is a real
   * resize of the pty - see the effect below for why that is the right answer
   * and not a bug.
   */
  fontSize: number
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
 *
 * ANSI black and white are the palette's *darkest* and *lightest* colours,
 * assigned by luminance rather than by name. Naming them after the background
 * is the mistake this avoids: a program that prints in ANSI black is printing
 * in the colour a terminal calls black, and in a light theme that has to be
 * dark text or it is invisible. Measured, in the light theme, before this: a
 * row of ANSI-black output against the terminal's own background came out at
 * 1.3:1 - text the same colour as the paper.
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

  // Relative luminance, enough to sort two colours into darker and lighter.
  const luminance = (colour: string): number => {
    const hex = colour.replace('#', '')
    if (!/^[0-9a-f]{6}$/i.test(hex)) return 0
    const channel = (offset: number): number => parseInt(hex.slice(offset, offset + 2), 16) / 255
    return 0.2126 * channel(0) + 0.7152 * channel(2) + 0.0722 * channel(4)
  }
  const darkPalette = luminance(background) < luminance(text)

  return {
    background,
    foreground: text,
    cursor: text,
    cursorAccent: background,
    selectionBackground: read('--border-strong', '#38404d'),

    // ANSI black is the palette's darkest colour, which in a light theme is its
    // text and in a dark theme is its background. Naming it after the
    // background instead - which is what this did - makes every program that
    // prints in black invisible on a light terminal: measured, a row of
    // ANSI-black output came out at 1.3:1.
    black: darkPalette ? background : text,
    brightBlack: read('--text-muted', '#9aa4b2'),
    // ANSI white stays the foreground colour in both. A light terminal that
    // drew white as white would be drawing text the colour of the paper, which
    // is faithful and unreadable; the interface is a tool, and legibility wins.
    white: text,
    brightWhite: text,
  }
}

export function TerminalView({ session, interactive, mayResize, fontSize }: TerminalViewProps) {
  const hostRef = useRef<HTMLDivElement | null>(null)
  const termRef = useRef<Terminal | null>(null)
  /** The fit addon, kept so a font change can re-measure against it. */
  const fitRef = useRef<FitAddon | null>(null)
  /** The resize scheduler and the measure it is fed, for the font effect. */
  const schedulerRef = useRef<ResizeScheduler | null>(null)
  const measureRef = useRef<(() => TerminalSize) | null>(null)
  /** Whether new output should bring the viewport back to the bottom. */
  const followingRef = useRef(true)
  /**
   * Whether this browser may set the terminal's shape, read inside the effect.
   *
   * It is a ref for the reason every other authority decision here is: the
   * measurement is taken from a ResizeObserver and a resize event, which run
   * outside any render and would otherwise close over the value the terminal
   * was built with.
   */
  const mayResizeRef = useRef(mayResize)

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
      fontSize,
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
    fitRef.current = fitAddon
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
     *
     * The fit is the whole of what a controller's browser contributes to the
     * pty's shape, and it is not made at all for a viewer. A viewer's terminal
     * is as large as the terminal is, which the server said when it sent the
     * screen; measuring the box on top of that would be this browser overruling
     * the shape it was handed. What a viewer reports is therefore the size the
     * terminal already has, which the scheduler finds nothing new in.
     */
    const measure = (): TerminalSize => {
      if (mayResizeRef.current) {
        try {
          fitAddon.fit()
        } catch {
          // A hidden element has no size to fit to. The next measurement, taken
          // when it is visible, is the one that counts.
        }
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
    schedulerRef.current = scheduler
    measureRef.current = measure

    const initial = measure()
    session.setSize(initial)
    // The subscribe that follows carries this size, so the first screen the
    // server renders is already the right shape. Recording it here is also what
    // stops the same size being sent again as soon as anything is measured.
    scheduler.adopt(initial)

    const sink: TerminalSink = {
      draw(payload) {
        // Read before writing: writing while the viewport is up is what makes
        // output unread, and afterwards is too late to tell.
        markUnread()
        term.write(payload)
      },
      show(payload, cols, rows) {
        // Size first, then draw. The screen was rendered for this geometry, so
        // writing it into a differently shaped terminal wraps it wrongly and no
        // later output can repair that.
        if (cols > 0 && rows > 0) {
          // The shape the terminal now has, whoever chose it. For a controller
          // it is the one this browser asked for, echoed back; for a viewer it
          // is the only shape the screen ever comes in. Recording it is what
          // stops the resize below being measured as news and sent as a resize
          // the server would refuse.
          scheduler.adopt({ cols, rows })
          if (term.cols !== cols || term.rows !== rows) term.resize(cols, rows)
        }
        term.write(payload)
        // And then the viewport, which is the half of this that is easy to
        // leave undone - and was left undone.
        //
        // A snapshot *is* the present. It is what the pane is showing now, and
        // the contract `Screen.Render` documents is that a client which writes
        // one is then showing exactly what the pane is showing. But a terminal
        // that has scrollback of its own - which every terminal here has as soon
        // as it has scrolled - can take the snapshot into its screen rows while
        // its viewport sits somewhere above them, and then the page is painting
        // the terminal's history while tmux is painting its screen. Measured in
        // the recovery suite: a reconnected Claude tab drawing "Welcome to
        // Claude Code v2.1.274" while the pane held the theme picker that came
        // after it, with twelve of the pane's fourteen rows on the page but not
        // one of them in the right place. That is not a client a frame behind;
        // it is one looking at the wrong part of the terminal, and it stays
        // there until something scrolls it.
        //
        // Unconditionally, rather than only when this browser was following the
        // output. The snapshot has just replaced the screen, so a scroll
        // position held from before it points at rows that are no longer
        // underneath it. Reading scrollback deliberately is a real thing to
        // want, and it is not what this would interrupt: nothing else here moves
        // the viewport, and a snapshot arrives only when a connection is
        // established or a screen is asked for again - both of which are
        // moments at which the live screen is what was asked for.
        term.scrollToBottom()
        followingRef.current = true
        setFollowing(true)
        // Nothing is waiting to be read either, for the same reason: the count
        // is of lines that arrived while the viewport was up, and the viewport
        // is not up any more.
        setUnread(false)
      },
      resize(cols, rows) {
        // The server's answer is the truth, not the request: the backend may
        // have clamped it. Recording it as sent is what keeps our own
        // acknowledgement from being echoed back as another resize.
        scheduler.adopt({ cols, rows })
        if (term.cols === cols && term.rows === rows) return

        // Read before resizing: xterm reports the scroll the resize causes, and
        // that report arrives with the viewport already moved - so asking
        // afterwards is asking a question that has already been answered wrong.
        const wasFollowing = followingRef.current
        term.resize(cols, rows)
        // A resize is not a reason to stop following. Without this a panel that
        // grew - because the workspace around it changed - could sit showing an
        // older part of its own scrollback while the program inside redrew at
        // the bottom, which looks exactly like a terminal that has stopped.
        if (wasFollowing) {
          term.scrollToBottom()
          followingRef.current = true
          setFollowing(true)
        }
      },
    }

    const detach = session.attach(sink)

    /**
     * markUnread records that output arrived while the viewport was up.
     *
     * It asks the buffer rather than the last event, so it is right whichever
     * gesture moved the viewport - and right on the first frame after a wheel,
     * before any event has been delivered.
     */
    const markUnread = (): void => {
      const buffer = term.buffer.active
      if (buffer.viewportY >= buffer.baseY) return
      setUnread(true)
    }

    /**
     * syncFollowing reads where the viewport actually is.
     *
     * It reads the buffer rather than trusting an event, because the two ways a
     * person scrolls do not arrive the same way. xterm reports its own scrolling
     * through onScroll, which covers the keyboard and anything that moves the
     * buffer - and it does not cover the browser scrolling the viewport element
     * for a mouse wheel. Measured: the wheel moves scrollTop and fires no event
     * this component ever saw, so somebody scrolling up with a mouse was still
     * being treated as following, and never got the way back to the bottom.
     */
    const syncFollowing = (): void => {
      const buffer = term.buffer.active
      const atBottom = buffer.viewportY >= buffer.baseY
      followingRef.current = atBottom
      setFollowing(atBottom)
      if (atBottom) setUnread(false)
    }

    const onData = term.onData((data) => session.input(data))
    // xterm reports a resize for every resize call, including the ones this
    // component makes itself. The scheduler is what decides whether any of them
    // is worth telling the server about.
    const onResize = term.onResize(() => scheduler.request(measure()))
    const onScroll = term.onScroll(syncFollowing)

    // The other half of the same fact. `.xterm-viewport` is xterm's own
    // scrolling element - FitAddon reaches for it by the same selector - and its
    // scroll event is the one the wheel produces.
    const viewport = host.querySelector('.xterm-viewport')
    const onViewportScroll = (): void => syncFollowing()
    viewport?.addEventListener('scroll', onViewportScroll, { passive: true })

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
      viewport?.removeEventListener('scroll', onViewportScroll)
      observer?.disconnect()
      scheduler.cancel()
      onData.dispose()
      onResize.dispose()
      onScroll.dispose()
      detach()
      termRef.current = null
      fitRef.current = null
      schedulerRef.current = null
      measureRef.current = null
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

  useEffect(() => {
    const wasResizing = mayResizeRef.current
    mayResizeRef.current = mayResize
    if (mayResize === wasResizing) return
    if (!mayResize) return

    // Being given the keyboard is also being given the shape. Until now this
    // browser has been drawing whatever the terminal is; from here the terminal
    // is whatever this browser's box is, and that is a real change to the pty
    // that the person who just asked for control is owed at once. The scheduler
    // is told to forget the size it had recorded, because the recorded size is
    // the one the server chose and the measurement below would otherwise have
    // to differ from it by luck to be sent at all.
    schedulerRef.current?.forget()
    const measure = measureRef.current
    if (measure) schedulerRef.current?.request(measure())
  }, [mayResize])

  useEffect(() => {
    const term = termRef.current
    if (!term || term.options.fontSize === fontSize) return

    term.options.fontSize = fontSize
    // A different text size is a different number of columns and rows in the
    // same box, so this is a real change of terminal geometry and not a
    // cosmetic one: the pty is genuinely a new shape and the program inside it
    // should be told. It goes through the same scheduler as every other
    // measurement, so a person dragging a window between two sizes still sends
    // one resize rather than one per pixel.
    const measure = measureRef.current
    if (measure) schedulerRef.current?.request(measure())
  }, [fontSize])

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
