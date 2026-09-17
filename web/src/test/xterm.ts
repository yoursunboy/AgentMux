/**
 * xterm.js, as much of it as TerminalView touches.
 *
 * The component under test owns an xterm instance, and xterm is a third-party
 * boundary with its own rendering, its own font measurement and its own DOM
 * renderer - none of which is this project's code. Running the real one in jsdom
 * would mean stubbing canvas contexts and matchMedia so that it can draw pixels
 * into a document nobody looks at, and the thing being asserted would be xterm's
 * behaviour rather than this component's.
 *
 * So the double is narrow on purpose: it records what it was told, in order, and
 * lets a test fire the three events the component listens for. Whether the
 * pixels are right is settled in a real browser, which is the only place that
 * question has an answer.
 *
 * Two things about it are worth knowing before reading a test that uses it:
 *
 *   - `FakeFitAddon.size` stands for what the element can actually fit. A test
 *     that changes the terminal's size keeps it in step, because in a real
 *     browser a fitted terminal and its element agree.
 *   - Only `resize` reports a change. The real addon reaches the terminal
 *     through a resize too, but its answer is stable once it is fitted, so an
 *     event from it is a no-op - and a double that fired one would make the
 *     component's own measurement look like a loop.
 */

export interface FakeTerminalEvent {
  dispose(): void
}

export class FakeTerminal {
  static instances: FakeTerminal[] = []

  static reset(): void {
    FakeTerminal.instances = []
    FakeFitAddon.size = { cols: 80, rows: 24 }
  }

  static get last(): FakeTerminal {
    const terminal = FakeTerminal.instances.at(-1)
    if (!terminal) throw new Error('no terminal has been created')
    return terminal
  }

  cols = 80
  rows = 24

  readonly options: Record<string, unknown>
  readonly unicode = { activeVersion: '6' }

  /** Everything written, in order, as the bytes handed over. */
  readonly writes: Uint8Array[] = []
  readonly resizes: Array<{ cols: number; rows: number }> = []
  readonly addons: unknown[] = []

  /**
   * Writes and resizes interleaved, in the order they happened.
   *
   * The order is the point: a snapshot written before the terminal is the right
   * shape wraps wrongly and no later output repairs it, so a test has to be able
   * to see which came first rather than only that both happened.
   */
  readonly events: string[] = []

  host: HTMLElement | null = null
  opened = false
  disposed = false
  focused = 0
  scrolledToBottom = 0

  /** The scroll state onScroll reports. A test moves it before firing onScroll. */
  readonly buffer = { active: { viewportY: 0, baseY: 0 } }

  private readonly dataListeners: Array<(data: string) => void> = []
  private readonly resizeListeners: Array<() => void> = []
  private readonly scrollListeners: Array<() => void> = []

  constructor(options: Record<string, unknown> = {}) {
    this.options = options
    FakeTerminal.instances.push(this)
  }

  loadAddon(addon: unknown): void {
    this.addons.push(addon)
  }

  open(host: HTMLElement): void {
    this.opened = true
    this.host = host
  }

  write(data: Uint8Array | string): void {
    this.events.push('write')
    this.writes.push(typeof data === 'string' ? new TextEncoder().encode(data) : data)
  }

  resize(cols: number, rows: number): void {
    const changed = this.cols !== cols || this.rows !== rows
    this.cols = cols
    this.rows = rows
    this.resizes.push({ cols, rows })
    this.events.push(`resize:${cols}x${rows}`)
    if (changed) this.fireResize()
  }

  /** fireResize reports a change to the listeners, as xterm does. */
  fireResize(): void {
    for (const listener of this.resizeListeners) listener()
  }

  focus(): void {
    this.focused += 1
  }

  scrollToBottom(): void {
    this.scrolledToBottom += 1
    // The real one moves the viewport and reports it, which is how the component
    // learns it is following again. A double that only counted the call would
    // let a "follow" button that never goes away pass its own test.
    this.buffer.active.viewportY = this.buffer.active.baseY
    for (const listener of this.scrollListeners) listener()
  }

  dispose(): void {
    this.disposed = true
  }

  onData(listener: (data: string) => void): FakeTerminalEvent {
    this.dataListeners.push(listener)
    return { dispose: () => this.remove(this.dataListeners, listener) }
  }

  onResize(listener: () => void): FakeTerminalEvent {
    this.resizeListeners.push(listener)
    return { dispose: () => this.remove(this.resizeListeners, listener) }
  }

  onScroll(listener: () => void): FakeTerminalEvent {
    this.scrollListeners.push(listener)
    return { dispose: () => this.remove(this.scrollListeners, listener) }
  }

  // -------------------------------------------------------------------------
  // What only a test does

  /** type delivers keystrokes, as xterm does once it has focus. */
  type(data: string): void {
    for (const listener of this.dataListeners) listener(data)
  }

  /** fireScroll delivers a scroll at whatever buffer state the test set up. */
  fireScroll(viewportY: number, baseY: number): void {
    this.buffer.active.viewportY = viewportY
    this.buffer.active.baseY = baseY
    for (const listener of this.scrollListeners) listener()
  }

  /** Everything written, decoded, so a test can read what the terminal shows. */
  text(): string {
    const decoder = new TextDecoder()
    return this.writes.map((chunk) => decoder.decode(chunk)).join('')
  }

  private remove<T>(list: T[], item: T): void {
    const index = list.indexOf(item)
    if (index >= 0) list.splice(index, 1)
  }
}

/**
 * FitAddon, which hands the terminal the size the element fits.
 *
 * The real addon measures glyphs, and there are no glyphs in jsdom. What it
 * produces is what every measurement in the component flows from, so a test sets
 * it here and then asserts on what the component did with it. It reports nothing
 * to the listeners: a fitted terminal does not change size, so the event the
 * real addon causes carries no news.
 */
export class FakeFitAddon {
  static size = { cols: 80, rows: 24 }

  fit(): void {
    const terminal = FakeTerminal.last
    terminal.cols = FakeFitAddon.size.cols
    terminal.rows = FakeFitAddon.size.rows
  }
}

/** Unicode11Addon, which has nothing to do in a test. */
export class FakeUnicode11Addon {}
