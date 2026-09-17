/**
 * When a measured size becomes a resize on the wire.
 *
 * The interesting rules are all about what is *not* sent: a size that has not
 * changed, a size the server already applied, and - the one that needs the most
 * care - a shorter terminal because a soft keyboard is covering it.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  clampSize,
  keyboardIsOpen,
  KEYBOARD_VIEWPORT_MARGIN_PX,
  ResizeScheduler,
  RESIZE_DEBOUNCE_MS,
  resizeDecision,
  type TerminalSize,
} from './sizing'
import { MAX_COLS, MAX_ROWS, MIN_COLS, MIN_ROWS } from './protocol'

describe('clampSize', () => {
  it('keeps a size inside the bounds the server enforces', () => {
    // The server clamps too and its answer is what counts, so the point here is
    // not to be authoritative but to avoid asking for something refused: a
    // 12-column terminal makes every program inside redraw continuously.
    expect(clampSize(MIN_COLS - 5, MIN_ROWS - 5)).toEqual({ cols: MIN_COLS, rows: MIN_ROWS })
    expect(clampSize(MAX_COLS + 500, MAX_ROWS + 500)).toEqual({ cols: MAX_COLS, rows: MAX_ROWS })
  })

  it('leaves a size inside the bounds alone', () => {
    expect(clampSize(120, 40)).toEqual({ cols: 120, rows: 40 })
  })

  it('rounds a fractional measurement', () => {
    // A fitted terminal can report a fraction of a cell. A fractional size on
    // the wire is a size no pty can be set to.
    expect(clampSize(80.4, 24.6)).toEqual({ cols: 80, rows: 25 })
  })

  it('falls back to the floor for a measurement that is not a number', () => {
    // Not a number and not finite both mean nothing was measured, and the floor
    // is the safe reading: a zero-row terminal is a pty every program inside
    // redraws for, and a hidden element measures as exactly this.
    expect(clampSize(Number.NaN, Number.NaN)).toEqual({ cols: MIN_COLS, rows: MIN_ROWS })
    expect(clampSize(Number.POSITIVE_INFINITY, Number.NEGATIVE_INFINITY)).toEqual({
      cols: MIN_COLS,
      rows: MIN_ROWS,
    })
  })

  it('clamps a measured size that is merely too large', () => {
    expect(clampSize(4000, 900)).toEqual({ cols: MAX_COLS, rows: MAX_ROWS })
    expect(clampSize(2, 1)).toEqual({ cols: MIN_COLS, rows: MIN_ROWS })
  })
})

describe('keyboardIsOpen', () => {
  it('is false wherever there is no visual viewport', () => {
    // Every desktop browser and jsdom. The guard must not change desktop
    // behaviour, and this is what makes that true rather than hoped for.
    expect(keyboardIsOpen(undefined, 800)).toBe(false)
    expect(keyboardIsOpen(800, undefined)).toBe(false)
    expect(keyboardIsOpen(undefined, undefined)).toBe(false)
  })

  it('is false for browser chrome and a scroll', () => {
    expect(keyboardIsOpen(800 - 60, 800)).toBe(false)
    expect(keyboardIsOpen(800, 800)).toBe(false)
  })

  it('is true when the viewport has lost a keyboard-sized piece', () => {
    expect(keyboardIsOpen(800 - KEYBOARD_VIEWPORT_MARGIN_PX - 1, 800)).toBe(true)
    expect(keyboardIsOpen(340, 800)).toBe(true)
  })

  it('is false for a viewport taller than the layout, which is not a keyboard', () => {
    expect(keyboardIsOpen(900, 800)).toBe(false)
  })

  it('is false for a measurement that is not a number', () => {
    expect(keyboardIsOpen(Number.NaN, 800)).toBe(false)
    expect(keyboardIsOpen(400, Number.POSITIVE_INFINITY)).toBe(false)
  })
})

describe('resizeDecision', () => {
  const size = (cols: number, rows: number): TerminalSize => ({ cols, rows })

  it('sends the first size it is told about', () => {
    expect(resizeDecision(null, size(80, 24), false)).toBe('send')
  })

  it('ignores a size that has not changed', () => {
    expect(resizeDecision(size(80, 24), size(80, 24), false)).toBe('ignore')
  })

  it('sends a real change', () => {
    expect(resizeDecision(size(80, 24), size(100, 24), false)).toBe('send')
    expect(resizeDecision(size(80, 24), size(80, 40), false)).toBe('send')
  })

  it('ignores a size that is not usable', () => {
    // A hidden element measures as zero. Sending it would make the pty one row
    // tall and every program inside redraw for it.
    expect(resizeDecision(size(80, 24), size(0, 0), false)).toBe('ignore')
    expect(resizeDecision(null, size(80, 0), false)).toBe('ignore')
  })

  describe('with a soft keyboard open', () => {
    it('ignores a change in rows alone', () => {
      // The pane is shorter because the keyboard is covering it, not because the
      // terminal has fewer rows. Sending it makes the program inside redraw for
      // a size that changes back the moment the keyboard closes - once per
      // character typed, if the keyboard's height wobbles.
      expect(resizeDecision(size(80, 24), size(80, 12), true)).toBe('ignore')
    })

    it('still sends a change in columns', () => {
      // A keyboard does not change how wide the screen is, so a columns change
      // while one is open is a real change - a rotation, or a window resize on a
      // device with a keyboard attached.
      expect(resizeDecision(size(80, 24), size(120, 12), true)).toBe('send')
    })

    it('still sends the first size, which is not a change from anything', () => {
      expect(resizeDecision(null, size(80, 12), true)).toBe('send')
    })
  })
})

describe('ResizeScheduler', () => {
  let sent: TerminalSize[]
  let keyboardOpen: boolean

  beforeEach(() => {
    vi.useFakeTimers()
    sent = []
    keyboardOpen = false
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  function scheduler(delayMs = RESIZE_DEBOUNCE_MS): ResizeScheduler {
    return new ResizeScheduler((size) => sent.push(size), {
      keyboardOpen: () => keyboardOpen,
      delayMs,
    })
  }

  it('debounces a burst into one resize', () => {
    // A window being dragged produces a measurement per frame; the terminal must
    // hear about it once, when the drag stops.
    const subject = scheduler()

    subject.request({ cols: 81, rows: 24 })
    vi.advanceTimersByTime(50)
    subject.request({ cols: 90, rows: 24 })
    vi.advanceTimersByTime(50)
    subject.request({ cols: 100, rows: 24 })

    expect(sent).toEqual([])
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)

    expect(sent).toEqual([{ cols: 100, rows: 24 }])
  })

  it('sends nothing when the burst settles on the size already sent', () => {
    const subject = scheduler()

    subject.request({ cols: 100, rows: 24 })
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(sent).toHaveLength(1)

    subject.request({ cols: 120, rows: 24 })
    vi.advanceTimersByTime(50)
    subject.request({ cols: 100, rows: 24 })
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)

    expect(sent).toHaveLength(1)
  })

  it('does not re-send a size the server applied', () => {
    // The loop this prevents: the server reports the size it applied, the client
    // draws at that size, the drawing reports a size, and the two answer each
    // other forever.
    const subject = scheduler()

    subject.adopt({ cols: 120, rows: 40 })
    subject.request({ cols: 120, rows: 40 })
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)

    expect(sent).toEqual([])
    expect(subject.lastSent).toEqual({ cols: 120, rows: 40 })
  })

  it('withholds a rows-only change while a keyboard is open, and sends it when it closes', () => {
    const subject = scheduler()

    subject.request({ cols: 80, rows: 24 })
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(sent).toEqual([{ cols: 80, rows: 24 }])

    keyboardOpen = true
    subject.request({ cols: 80, rows: 12 })
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(sent).toHaveLength(1)

    // The keyboard closes and the measurement is taken again. Because the
    // withheld size was never recorded as sent, this is a change and it goes.
    keyboardOpen = false
    subject.request({ cols: 80, rows: 24 })
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(sent).toHaveLength(1)

    subject.request({ cols: 80, rows: 30 })
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(sent).toEqual([
      { cols: 80, rows: 24 },
      { cols: 80, rows: 30 },
    ])
  })

  it('cancel drops a pending size, so a terminal being torn down sends nothing', () => {
    const subject = scheduler()

    subject.request({ cols: 100, rows: 30 })
    subject.cancel()
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS * 4)

    expect(sent).toEqual([])
  })

  it('reports the size it has sent', () => {
    const subject = scheduler()
    expect(subject.lastSent).toBeNull()

    subject.request({ cols: 100, rows: 30 })
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)

    expect(subject.lastSent).toEqual({ cols: 100, rows: 30 })
  })

  it('keeps sending after a size that was ignored', () => {
    // An ignored size must not be recorded, or the next real change would be
    // measured against something the server was never told.
    const subject = scheduler()

    subject.request({ cols: 0, rows: 0 })
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(sent).toEqual([])

    subject.request({ cols: 80, rows: 24 })
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(sent).toEqual([{ cols: 80, rows: 24 }])
  })
})
