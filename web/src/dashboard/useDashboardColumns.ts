/**
 * How many project cards fit across the console, in columns.
 *
 * # Why the breakpoints are not the workspace's
 *
 * `workspace/useLayout` puts three terminal panels across a grid from 1340px,
 * because a terminal panel has to hold a usable number of *monospace columns* -
 * about sixty-six of them, at roughly 6.6px each. A project card holds a name, a
 * status word and a count. It is a fraction of the width for the same
 * usefulness, so three of them fit far earlier.
 *
 * The numbers here are chosen so that the devices §10 of the phase brief names
 * get the layout it asks for:
 *
 *	phone, upright (390–430)   one column
 *	tablet, upright (768–834)  three columns, which is the preference it states
 *	tablet, sideways (1024)    three columns
 *	desktop (1280 and up)      three columns
 *
 * The tier between a phone and an upright tablet exists because a phone on its
 * side, or a small tablet upright, is a real width that is neither. Two columns
 * is what it is for.
 *
 * # Why this is a number in JavaScript at all
 *
 * The grid could be pure CSS, and for a single-column-versus-three decision it
 * would be. It is not, for two reasons. The console is the layout a future
 * phase will put a terminal panel inside, and that panel will have to know how
 * wide it is for the same reason the workspace does. And a rule that is only in
 * a stylesheet cannot be asserted by a test: jsdom does not evaluate media
 * queries, so "three columns on a desktop, one on a phone" would be a claim
 * nothing checked. As a hook it is covered by the same viewport polyfill the
 * workspace's tests use.
 *
 * The stylesheet and this file use the same numbers, and the constant is
 * exported so a test can assert the two agree rather than trust that they do.
 */
import { useEffect, useState } from 'react'

/** The viewport widths at which the console's grid gains a column. */
export const DASHBOARD_BREAKPOINTS = {
  /** From here the grid is three columns, which every large device gets. */
  three: 768,
  /** From here it is two. Below it, one. */
  two: 600,
} as const

/** How many columns the console's grid shows. */
export type DashboardColumns = 1 | 2 | 3

/** dashboardColumnsForWidth answers the layout question without a browser. */
export function dashboardColumnsForWidth(width: number): DashboardColumns {
  if (!Number.isFinite(width) || width <= 0) return 3
  if (width >= DASHBOARD_BREAKPOINTS.three) return 3
  if (width >= DASHBOARD_BREAKPOINTS.two) return 2
  return 1
}

/** The media queries this hook listens to, in the order they are checked. */
const QUERIES: ReadonlyArray<{ columns: DashboardColumns; query: string }> = [
  { columns: 3, query: `(min-width: ${DASHBOARD_BREAKPOINTS.three}px)` },
  { columns: 2, query: `(min-width: ${DASHBOARD_BREAKPOINTS.two}px)` },
]

/**
 * useDashboardColumns reports the grid's column count, and re-renders when it
 * changes.
 *
 * It follows `workspace/useLayout` exactly, including the fallback: where there
 * is no `matchMedia` the answer is three columns, which is the layout the
 * console is designed around. Guessing narrower would make a page hold fewer
 * cards than the grid has room for.
 */
export function useDashboardColumns(): DashboardColumns {
  const [columns, setColumns] = useState<DashboardColumns>(() =>
    typeof window === 'undefined' || typeof window.matchMedia !== 'function'
      ? 3
      : dashboardColumnsForWidth(window.innerWidth),
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
