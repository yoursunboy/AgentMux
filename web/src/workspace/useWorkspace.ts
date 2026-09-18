/**
 * The workspace's own state: which page is showing, which project has focus,
 * and what the current page contains.
 *
 * It is a hook rather than a component's state because three different parts of
 * the page need the same answers - the grid draws the items, the global bar
 * draws the pager, and a panel header decides whether to offer Focus - and
 * three copies of "which page is this" is two copies too many.
 *
 * Everything here is a property of one browser at one moment. The one thing
 * that outlives the tab is the page number, and it is kept in `storage.ts`.
 */
import { useCallback, useEffect, useMemo, useState } from 'react'

import type { Project } from '../api/types'
import {
  clampPage,
  members,
  pageCount,
  pageItems,
  pageOfProject,
  type WorkspaceColumns,
  type WorkspaceItem,
} from './slots'
import { readPage, writePage } from './storage'
import { useWorkspaceColumns } from './useLayout'

export interface Workspace {
  /** How many columns the grid is drawn in, and therefore how big a page is. */
  columns: WorkspaceColumns
  /** The page being shown, zero-based. */
  page: number
  /** How many pages there are. Always at least one. */
  pages: number
  /** The cells on the current page, in order, with the manager last. */
  items: WorkspaceItem[]
  /** The project shown on its own, or null while the grid is showing. */
  focused: Project | null
  goToPage(page: number): void
  nextPage(): void
  previousPage(): void
  /** focus shows one project on its own. It changes no runtime state. */
  focus(projectId: string): void
  /** unfocus returns to the grid, on the page the project is on. */
  unfocus(): void
}

/**
 * useWorkspace derives the workspace's view state from the project list.
 *
 * `projects` is every registered project, and the workspace is the subset of
 * them that holds a reserved slot. The two are different numbers and the
 * difference is the whole point: a person with fifty registered projects and
 * eight panels has eight panels, and a pager that counted registrations would
 * offer forty-two pages of nothing.
 *
 * Focus is held here rather than by the caller because entering it also decides
 * which page to come back to, and splitting those two across a boundary would
 * let a caller focus a project without the page following it.
 */
export function useWorkspace(projects: readonly Project[]): Workspace {
  const columns = useWorkspaceColumns()
  const [page, setPage] = useState<number>(() => readPage())
  const [focusedId, setFocusedId] = useState<string | null>(null)

  const open = useMemo(() => members(projects), [projects])
  const pages = pageCount(open.length, columns)

  // A page that no longer exists is not an error and not an empty workspace:
  // the last project on the last page can be removed, and a window can narrow
  // until a page holds fewer projects. Both are answered by showing a page that
  // does exist.
  const current = clampPage(page, open.length, columns)

  useEffect(() => {
    if (current !== page) setPage(current)
  }, [current, page])

  useEffect(() => {
    writePage(current)
  }, [current])

  const goToPage = useCallback((next: number) => setPage(next), [])
  const nextPage = useCallback(() => setPage((previous) => previous + 1), [])
  const previousPage = useCallback(() => setPage((previous) => Math.max(0, previous - 1)), [])

  const items = useMemo(() => pageItems(projects, columns, current), [projects, columns, current])

  // A focused project that has left the workspace - removed from another tab,
  // or archived - is not a project to show on its own. Falling back to the grid
  // is the honest answer: the grid can show that it is gone.
  const focused = useMemo(() => {
    if (focusedId === null) return null
    return projects.find((project) => project.id === focusedId) ?? null
  }, [projects, focusedId])

  const focus = useCallback(
    (projectId: string) => {
      const target = pageOfProject(projects, projectId, columns)
      if (target !== null) setPage(target)
      setFocusedId(projectId)
    },
    [projects, columns],
  )

  const unfocus = useCallback(() => setFocusedId(null), [])

  return useMemo(
    () => ({
      columns,
      page: current,
      pages,
      items,
      focused,
      goToPage,
      nextPage,
      previousPage,
      focus,
      unfocus,
    }),
    [columns, current, pages, items, focused, goToPage, nextPage, previousPage, focus, unfocus],
  )
}
