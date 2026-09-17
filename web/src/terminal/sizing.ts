/**
 * When a browser tells the server how large its terminal is.
 *
 * A resize is not a local event. It reaches the pty, and every full-screen
 * program inside redraws; a stream of redundant ones is visible as flicker, and
 * a resize on every layout pass is a conversation with the terminal that never
 * ends. So the decision is separated from the measurement: the component
 * measures, and everything about whether that measurement is worth sending
 * lives here, where it can be tested without a browser.
 */
import { MAX_COLS, MAX_ROWS, MIN_COLS, MIN_ROWS } from './protocol'

export interface TerminalSize {
  cols: number
  rows: number
}

/** How long a measured size must hold before it is sent. */
export const RESIZE_DEBOUNCE_MS = 200

/**
 * How much shorter than the layout viewport the visual viewport has to be
 * before it is taken as an on-screen keyboard rather than a scroll or a browser
 * chrome.
 *
 * A keyboard covers a good fraction of a phone screen; browser chrome and a
 * pinch-zoom change the visual viewport by much less. The number is the
 * boundary between the two, and it is deliberately generous in the direction
 * that ignores a real keyboard: mistaking a keyboard for chrome costs one
 * redundant resize, and the reverse costs a rotated, flickering terminal while
 * somebody types.
 */
export const KEYBOARD_VIEWPORT_MARGIN_PX = 120

/**
 * clampSize mirrors the server's bounds.
 *
 * The server clamps too, and it is the one whose answer counts, because it
 * reports back the size that was actually applied. This exists so that the
 * client does not ask for something it knows will be refused: a 12-column
 * terminal makes every program inside redraw continuously, and a 4000-column
 * one makes the server's grid enormous for a window nobody has.
 */
export function clampSize(cols: number, rows: number): TerminalSize {
  return { cols: clamp(cols, MIN_COLS, MAX_COLS), rows: clamp(rows, MIN_ROWS, MAX_ROWS) }
}

function clamp(value: number, low: number, high: number): number {
  if (!Number.isFinite(value)) return low
  return Math.min(high, Math.max(low, Math.round(value)))
}

/**
 * keyboardIsOpen reports whether the visual viewport looks like a soft keyboard
 * has taken part of it.
 *
 * It returns false wherever there is no visual viewport, which is every desktop
 * browser and jsdom, so the guard cannot change desktop behaviour.
 */
export function keyboardIsOpen(viewportHeight?: number, layoutHeight?: number): boolean {
  if (viewportHeight === undefined || layoutHeight === undefined) return false
  if (!Number.isFinite(viewportHeight) || !Number.isFinite(layoutHeight)) return false
  return layoutHeight - viewportHeight > KEYBOARD_VIEWPORT_MARGIN_PX
}

/** What to do with a measured size. */
export type ResizeDecision = 'send' | 'ignore'

/**
 * resizeDecision decides whether a measured size is worth telling the server.
 *
 * The last rule is the one that needs its reason written down. With a soft
 * keyboard open, the terminal pane is shorter because the keyboard is covering
 * it - not because there are fewer rows in the terminal. Sending that resize
 * makes the pty shorter, which makes the program inside redraw for a size that
 * is about to change back the moment the keyboard closes, once per character
 * typed if the keyboard's height wobbles. A change in columns is a different
 * statement: a keyboard does not change how wide the screen is, so a
 * columns change while one is open is real and is sent.
 */
export function resizeDecision(
  previous: TerminalSize | null,
  next: TerminalSize,
  keyboardOpen: boolean,
): ResizeDecision {
  if (next.cols <= 0 || next.rows <= 0) return 'ignore'
  if (previous && next.cols === previous.cols && next.rows === previous.rows) return 'ignore'
  if (keyboardOpen && previous && next.cols === previous.cols) return 'ignore'
  return 'send'
}

export interface ResizeSchedulerOptions {
  /** Reports whether a soft keyboard is open, read when a resize would be sent. */
  keyboardOpen: () => boolean
  delayMs?: number
}

/**
 * ResizeScheduler turns a stream of measurements into the resizes worth sending.
 *
 * It debounces on the trailing edge, so a window being dragged sends one resize
 * when it is let go rather than one per frame. The first size is not debounced -
 * a client that has just subscribed and knows how big its terminal is should
 * say so at once - but in practice the subscribe message already carries it, so
 * the first thing this sends is usually a later change.
 */
export class ResizeScheduler {
  private readonly sink: (size: TerminalSize) => void
  private readonly options: ResizeSchedulerOptions
  private readonly delayMs: number

  private pending: TerminalSize | null = null
  private timer: ReturnType<typeof setTimeout> | null = null
  private sent: TerminalSize | null = null

  constructor(
    sink: (size: TerminalSize) => void,
    options: ResizeSchedulerOptions = { keyboardOpen: () => false },
  ) {
    this.sink = sink
    this.options = options
    this.delayMs = options.delayMs ?? RESIZE_DEBOUNCE_MS
  }

  /** lastSent is the size the server has been told about, if any. */
  get lastSent(): TerminalSize | null {
    return this.sent
  }

  /**
   * adopt records a size the server applied without sending it back.
   *
   * It is what stops the acknowledgement of our own resize from becoming a
   * second resize: the server reports the size it applied, the client draws at
   * that size, the drawing reports a size, and without this the two would
   * answer each other forever.
   */
  adopt(size: TerminalSize): void {
    this.sent = size
  }

  /** request offers a measured size. It is sent only if it is worth sending. */
  request(size: TerminalSize): void {
    this.pending = size
    if (this.timer !== null) clearTimeout(this.timer)
    this.timer = setTimeout(this.fire, this.delayMs)
  }

  /** cancel drops anything pending. */
  cancel(): void {
    if (this.timer !== null) clearTimeout(this.timer)
    this.timer = null
    this.pending = null
  }

  private readonly fire = (): void => {
    this.timer = null
    const next = this.pending
    this.pending = null
    if (!next) return
    if (resizeDecision(this.sent, next, this.options.keyboardOpen()) === 'ignore') return
    this.sent = next
    this.sink(next)
  }
}
