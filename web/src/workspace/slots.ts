/**
 * The workspace's slot model: which projects are in it, in what order, and
 * which page each one lands on.
 *
 * # A slot is a reservation, and a position is where it ended up
 *
 * `pinnedSlot` is the one piece of workspace layout the server keeps, and it is
 * a *reservation*: "this project sits at index 3". Membership in the workspace
 * is the same fact - a project is in the workspace exactly when it has a
 * reserved slot - which is what makes the workspace survive a reload, a second
 * device, and a server restart without a second store to keep in step.
 *
 * What is *rendered* is the resolved order: the reservations, sorted, with
 * anything unreserved after them. Reservations are therefore allowed to
 * collide (two clients can pick the same free slot at the same moment) and
 * allowed to have gaps (slot 5 reserved while 3 and 4 are not), because neither
 * is a state the grid has to represent: it draws the projects it has, in order,
 * densely.
 *
 * # Why nothing here looks at status
 *
 * A project's runtime state changes constantly, and a grid that reordered
 * itself when a runtime stopped would move the panel somebody was reading. The
 * order below depends on reservations and on the server's own list order, and
 * on nothing else. `docs/WORKSPACE.md` states that as the rule it is.
 */
import type { Project } from '../api/types'

/** How many columns the grid is laid out in. */
export type WorkspaceColumns = 1 | 2 | 3

/**
 * How many projects one page holds, per column count.
 *
 * The number is chosen so a page is never more than two rows tall, because two
 * rows is the most a terminal can be stacked in and still be a terminal rather
 * than a strip. Three columns therefore hold five projects, two columns hold
 * three, and one column holds one - which is what the UI spec asks of a phone
 * ("one project per page") and of a tablet held upright.
 *
 * The Project Manager is not counted: it takes the cell after the last project
 * on the page.
 */
export const PROJECTS_PER_PAGE: Record<WorkspaceColumns, number> = { 1: 1, 2: 3, 3: 5 }

/** One cell of the workspace. */
export type WorkspaceItem =
  | { kind: 'project'; project: Project; slot: number }
  | { kind: 'manager' }

/** projectsPerPage is how many projects fit on one page at this width. */
export function projectsPerPage(columns: WorkspaceColumns): number {
  return PROJECTS_PER_PAGE[columns]
}

/**
 * isMember reports whether a project is in the workspace.
 *
 * It is a reservation test and nothing else, so a project leaves the workspace
 * by giving its slot up - which is a server write, and the reason removing a
 * project from the grid survives a reload.
 */
export function isMember(project: Project): boolean {
  return project.pinnedSlot !== null
}

/**
 * slotOrder returns the workspace's projects in the order they are shown.
 *
 * Reserved slots come first, in slot order; everything else follows in the
 * order the server returned them, which is registration order. The sort is
 * stable and total, so two projects with the same reservation - or none - still
 * have one defined order rather than an arbitrary one.
 */
export function slotOrder(projects: readonly Project[]): Project[] {
  return projects
    .map((project, index) => ({ project, index }))
    .sort((left, right) => {
      const a = left.project.pinnedSlot
      const b = right.project.pinnedSlot
      if (a !== b) {
        if (a === null) return 1
        if (b === null) return -1
        return a - b
      }
      return left.index - right.index
    })
    .map((entry) => entry.project)
}

/** members returns only the projects that are in the workspace, in slot order. */
export function members(projects: readonly Project[]): Project[] {
  return slotOrder(projects).filter(isMember)
}

/** pageCount is how many pages the workspace has.
 *
 * There is always at least one, because the Project Manager has to live
 * somewhere: a workspace with no projects is a page showing the manager, not no
 * page at all.
 */
export function pageCount(projectCount: number, columns: WorkspaceColumns): number {
  // In a single column the manager cannot share a page with a project - there
  // is one cell - so it takes a page of its own after the last one.
  if (columns === 1) return Math.max(1, projectCount + 1)
  return Math.max(1, Math.ceil(projectCount / projectsPerPage(columns)))
}

/**
 * clampPage keeps a page index inside the workspace.
 *
 * It exists for the case a page becomes too large while the page is open - the
 * last project on the last page is removed, or the window narrows and the page
 * size drops - where the alternative is an empty page that looks like a lost
 * workspace.
 */
export function clampPage(page: number, projectCount: number, columns: WorkspaceColumns): number {
  const last = pageCount(projectCount, columns) - 1
  if (!Number.isFinite(page)) return 0
  return Math.min(last, Math.max(0, Math.trunc(page)))
}

/**
 * pageItems returns the cells one page shows, in order.
 *
 * The Project Manager is always the last cell, on every page. When the layout
 * is a single column it cannot be a last cell - there is only one cell - so it
 * becomes a page of its own at the end, which is the same statement in a layout
 * that has no room for the other one.
 */
export function pageItems(
  projects: readonly Project[],
  columns: WorkspaceColumns,
  page: number,
): WorkspaceItem[] {
  const ordered = members(projects)
  const perPage = projectsPerPage(columns)

  if (columns === 1) {
    // One project per page, then the manager on its own final page.
    const project = ordered[page]
    if (!project) return [{ kind: 'manager' }]
    return [{ kind: 'project', project, slot: page }]
  }

  const start = page * perPage
  const items: WorkspaceItem[] = ordered
    .slice(start, start + perPage)
    .map((project, offset) => ({ kind: 'project' as const, project, slot: start + offset }))
  return [...items, { kind: 'manager' }]
}

/**
 * firstFreeSlot is the slot a project joining the workspace should take.
 *
 * It is the lowest reserved index nobody holds, which is how a new project
 * fills the first empty position rather than being appended: a workspace that
 * had a project removed from the middle should put the next one back in the
 * gap, and no existing project moves to make room.
 */
export function firstFreeSlot(projects: readonly Project[]): number {
  const taken = new Set<number>()
  for (const project of projects) {
    if (project.pinnedSlot !== null) taken.add(project.pinnedSlot)
  }
  let slot = 0
  while (taken.has(slot)) slot += 1
  return slot
}

/**
 * pageOfProject is which page a workspace member is shown on, or null when the
 * project is not in the workspace.
 *
 * Focus uses it: leaving focus should land on the page the project is part of,
 * not on whichever page happened to be open when it was entered.
 */
export function pageOfProject(
  projects: readonly Project[],
  projectId: string,
  columns: WorkspaceColumns,
): number | null {
  const index = members(projects).findIndex((project) => project.id === projectId)
  if (index === -1) return null
  return Math.floor(index / projectsPerPage(columns))
}

/** One project's reservation, as a request to the server. */
export interface SlotChange {
  projectId: string
  slot: number | null
}

/**
 * neighbourSwap is what "move left" and "move right" mean in a model where
 * position is a reservation.
 *
 * Moving a project one place along is swapping reservations with whoever is
 * next to it, so both projects are written. The alternative - renumbering
 * everything after it - would be a write per project, and would make moving one
 * panel a reason for every panel to change, which is the reshuffling this model
 * exists to avoid.
 *
 * Returns null when there is nothing to swap with: the first project moving
 * left, the last moving right, or a neighbour that is not a member and so has
 * no reservation to trade.
 */
export function neighbourSwap(
  projects: readonly Project[],
  projectId: string,
  direction: 'left' | 'right',
): SlotChange[] | null {
  const ordered = members(projects)
  const index = ordered.findIndex((project) => project.id === projectId)
  if (index === -1) return null

  const neighbourIndex = direction === 'left' ? index - 1 : index + 1
  const neighbour = ordered[neighbourIndex]
  if (!neighbour) return null

  const moved = ordered[index]
  // Both reservations are resolved to their displayed positions, so a swap
  // between two projects whose stored slots have a gap between them still
  // exchanges exactly the two places the user can see.
  return [
    { projectId: moved.id, slot: neighbourIndex },
    { projectId: neighbour.id, slot: index },
  ]
}

/**
 * moveToSlot is what "pin to slot N" means.
 *
 * If somebody already holds N they take the mover's place, so the number of
 * projects in the workspace and the set of reservations both stay the same. A
 * move to a free slot is a single write.
 *
 * The one case where there is nothing to trade is a mover that already holds
 * the slot it is moving to - which happens when two projects claim one slot and
 * the second of them is pinned to it. Then the occupant is unpinned rather than
 * given a reservation it already has, which leaves it in the workspace in the
 * order it was already in and ends the collision.
 */
export function moveToSlot(
  projects: readonly Project[],
  projectId: string,
  slot: number,
): SlotChange[] {
  const ordered = members(projects)
  const moved = ordered.find((project) => project.id === projectId)
  if (!moved) return []

  const occupant = ordered.find((project) => project.id !== projectId && project.pinnedSlot === slot)
  if (!occupant) return [{ projectId, slot }]

  const handover = moved.pinnedSlot
  const displaced: SlotChange =
    handover === null || handover === slot
      ? { projectId: occupant.id, slot: null }
      : { projectId: occupant.id, slot: handover }
  return [{ projectId, slot }, displaced]
}
