/**
 * A viewport for tests.
 *
 * The workspace's column count is read from `matchMedia`, which jsdom does not
 * implement at all, and from `innerWidth`, which jsdom fixes at 1024. Both have
 * to be answered for a grid test to mean anything: a test that asserted "five
 * projects and the manager are on the page" would pass or fail depending on a
 * width nobody set.
 *
 * The polyfill understands exactly one query shape - `(min-width: Npx)` - which
 * is the only shape `useLayout` asks. Anything else is a bug in the test rather
 * than something to support quietly, so it throws.
 */
import { GRID_BREAKPOINTS, columnsForWidth } from '../workspace/useLayout'

/** The width every test starts at: a desktop, which is the default layout. */
export const DEFAULT_TEST_WIDTH = 1440

let width = DEFAULT_TEST_WIDTH
const listeners = new Set<() => void>()

/** setViewportWidth changes the width and notifies anything listening. */
export function setViewportWidth(next: number): void {
  width = next
  for (const listener of listeners) listener()
}

/** viewportWidth is the width the polyfill is currently reporting. */
export function viewportWidth(): number {
  return width
}

/** resetViewport restores the default. Called between tests. */
export function resetViewport(): void {
  width = DEFAULT_TEST_WIDTH
  listeners.clear()
}

function matches(query: string): boolean {
  const match = /^\(min-width:\s*(\d+)px\)$/.exec(query.trim())
  if (!match) throw new Error(`the test viewport does not understand the query ${JSON.stringify(query)}`)
  return width >= Number(match[1])
}

/** installMatchMedia gives jsdom a viewport it can be asked about. */
export function installMatchMedia(): void {
  Object.defineProperty(window, 'innerWidth', {
    configurable: true,
    get: () => width,
  })

  window.matchMedia = ((query: string) => {
    const listenersForThis = new Set<(event: MediaQueryListEvent) => void>()
    const list = {
      get matches() {
        return matches(query)
      },
      media: query,
      onchange: null,
      addEventListener: (_type: string, listener: (event: MediaQueryListEvent) => void) => {
        listenersForThis.add(listener)
      },
      removeEventListener: (_type: string, listener: (event: MediaQueryListEvent) => void) => {
        listenersForThis.delete(listener)
      },
      addListener: (listener: (event: MediaQueryListEvent) => void) => listenersForThis.add(listener),
      removeListener: (listener: (event: MediaQueryListEvent) => void) =>
        listenersForThis.delete(listener),
      dispatchEvent: () => false,
    }
    listeners.add(() => {
      for (const listener of listenersForThis) {
        listener({ matches: matches(query), media: query } as MediaQueryListEvent)
      }
    })
    return list as unknown as MediaQueryList
  }) as typeof window.matchMedia
}

/** columnsAt is the column count a width produces, re-exported for assertions. */
export { columnsForWidth, GRID_BREAKPOINTS }
