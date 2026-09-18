/**
 * How wide the workspace is, in columns.
 *
 * # Why this is not only a CSS question
 *
 * The grid's column count decides how many rows a page is, and therefore how
 * many projects a page holds, so a layout that reflowed without the page model
 * knowing would show five projects in a grid three of them fit in. The number
 * has to be readable from JavaScript, which is what `matchMedia` is for.
 *
 * The breakpoints are the same numbers the stylesheet uses, and they are
 * exported so a test can assert the two agree rather than trust that they do.
 * The layout is mobile-first in both places: one column by default, then two,
 * then three.
 *
 * # Where the numbers come from
 *
 * They are the measured width at which a grid panel still holds a usable number
 * of terminal columns, not round numbers picked for tidiness. A panel's usable
 * width is about a third of the viewport minus the chrome around it, and a
 * monospace cell at the grid's font size is about 6.6px: three columns are
 * worth having from roughly 1340px, where each terminal gets its mid-sixties in
 * columns. Two columns are worth having from 900px, below which a panel would
 * be under sixty columns even at two - and there the answer is one column,
 * which is what a phone and an upright tablet get.
 * `docs/WORKSPACE.md` has the measurements.
 */
import { useEffect, useState } from 'react'

import type { WorkspaceColumns } from './slots'

/** The viewport widths at which the grid gains a column. */
export const GRID_BREAKPOINTS = {
  /** From here the grid is three columns: 5 projects and the manager. */
  three: 1340,
  /** From here it is two columns: 3 projects and the manager. */
  two: 900,
} as const

/** columnsForWidth answers the layout question without a browser. */
export function columnsForWidth(width: number): WorkspaceColumns {
  if (!Number.isFinite(width) || width <= 0) return 3
  if (width >= GRID_BREAKPOINTS.three) return 3
  if (width >= GRID_BREAKPOINTS.two) return 2
  return 1
}

/** The media queries this hook listens to, in the order they are checked. */
const QUERIES: ReadonlyArray<{ columns: WorkspaceColumns; query: string }> = [
  { columns: 3, query: `(min-width: ${GRID_BREAKPOINTS.three}px)` },
  { columns: 2, query: `(min-width: ${GRID_BREAKPOINTS.two}px)` },
]

/**
 * useWorkspaceColumns reports the grid's column count, and re-renders when it
 * changes.
 *
 * Three things can change it, and all three matter: the window being resized, a
 * tablet being turned on its side, and a split-screen or a zoom changing the
 * layout width. A media query listener is the one mechanism that answers all
 * three, because it is the browser's own definition of the same question the
 * stylesheet asks.
 *
 * Where there is no `matchMedia` - a test environment, or a very old browser -
 * the answer is three columns, which is the desktop layout the app is designed
 * around. Guessing a narrower one would make a page hold fewer projects than
 * the grid has room for, and guessing is what this hook exists to avoid.
 */
export function useWorkspaceColumns(): WorkspaceColumns {
  const [columns, setColumns] = useState<WorkspaceColumns>(() =>
    typeof window === 'undefined' || typeof window.matchMedia !== 'function'
      ? 3
      : columnsForWidth(window.innerWidth),
  )

  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return

    const lists = QUERIES.map(({ columns: value, query }) => ({
      value,
      list: window.matchMedia(query),
    }))

    const update = (): void => {
      const matched = lists.find((entry) => entry.list.matches)
      setColumns(matched ? matched.value : 1)
    }

    update()
    for (const entry of lists) entry.list.addEventListener('change', update)
    return () => {
      for (const entry of lists) entry.list.removeEventListener('change', update)
    }
  }, [])

  return columns
}
