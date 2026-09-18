import { describe, expect, it } from 'vitest'

import { makeProject } from '../test/fixtures'
import type { Project } from '../api/types'
import {
  clampPage,
  firstFreeSlot,
  isMember,
  members,
  moveToSlot,
  neighbourSwap,
  pageCount,
  pageItems,
  pageOfProject,
  projectsPerPage,
  slotOrder,
} from './slots'

/** A project in the workspace, which is what a reserved slot means. */
const open = (id: string, slot: number, extra: Partial<Project> = {}): Project =>
  makeProject({ id, name: id, pinnedSlot: slot, ...extra })

/** A registered project that is not in the workspace. */
const closed = (id: string): Project => makeProject({ id, name: id, pinnedSlot: null })

describe('membership', () => {
  it('is a reserved slot and nothing else', () => {
    expect(isMember(open('p_a', 0))).toBe(true)
    expect(isMember(closed('p_a'))).toBe(false)
  })

  it('excludes registered projects that were never opened', () => {
    // Fifty registered projects and eight panels is the ordinary case, not a
    // limitation.
    const all = [open('p_a', 0), closed('p_b'), open('p_c', 1), closed('p_d')]

    expect(members(all).map((project) => project.id)).toEqual(['p_a', 'p_c'])
  })
})

describe('slot order', () => {
  it('puts reserved slots first, in slot order', () => {
    const all = [closed('p_x'), open('p_b', 1), closed('p_y'), open('p_a', 0)]

    expect(slotOrder(all).map((project) => project.id)).toEqual(['p_a', 'p_b', 'p_x', 'p_y'])
  })

  it('keeps unreserved projects in the order the server gave them', () => {
    const all = [closed('p_c'), closed('p_a'), closed('p_b')]

    expect(slotOrder(all).map((project) => project.id)).toEqual(['p_c', 'p_a', 'p_b'])
  })

  it('is total, so two projects claiming one slot still have an order', () => {
    // Two clients can pick the same free slot at the same moment. That is not a
    // state the grid has to represent, but it does have to be deterministic.
    const all = [open('p_b', 2), open('p_a', 2), open('p_c', 0)]

    expect(slotOrder(all).map((project) => project.id)).toEqual(['p_c', 'p_b', 'p_a'])
  })

  it('does not depend on status, which is the property the grid lives by', () => {
    const before = [open('p_a', 0, { status: 'running' }), open('p_b', 1, { status: 'stopped' })]
    const after = [open('p_a', 0, { status: 'error' }), open('p_b', 1, { status: 'running' })]

    expect(slotOrder(before).map((project) => project.id)).toEqual(
      slotOrder(after).map((project) => project.id),
    )
  })
})

describe('pages', () => {
  it('holds five projects and the manager on a three-column desktop', () => {
    expect(projectsPerPage(3)).toBe(5)
    expect(pageCount(5, 3)).toBe(1)
    expect(pageCount(7, 3)).toBe(2)
  })

  it('holds three on two columns, so a page is never more than two rows', () => {
    expect(projectsPerPage(2)).toBe(3)
    expect(pageCount(3, 2)).toBe(1)
    expect(pageCount(7, 2)).toBe(3)
  })

  it('holds one on a phone, and gives the manager a page of its own', () => {
    expect(projectsPerPage(1)).toBe(1)
    expect(pageCount(2, 1)).toBe(3)
    // No projects at all is still one page: the manager lives somewhere.
    expect(pageCount(0, 1)).toBe(1)
    expect(pageCount(0, 3)).toBe(1)
  })

  it('always ends a page with the Project Manager', () => {
    const all = ['p_0', 'p_1', 'p_2', 'p_3', 'p_4', 'p_5', 'p_6'].map((id, index) =>
      open(id, index),
    )

    for (const columns of [3, 2] as const) {
      for (let page = 0; page < pageCount(all.length, columns); page += 1) {
        expect(pageItems(all, columns, page).at(-1)?.kind).toBe('manager')
      }
    }
  })

  it('fills two pages with five projects and two, as the spec draws it', () => {
    const all = ['p_0', 'p_1', 'p_2', 'p_3', 'p_4', 'p_5', 'p_6'].map((id, index) =>
      open(id, index),
    )

    const first = pageItems(all, 3, 0)
    expect(first.filter((item) => item.kind === 'project')).toHaveLength(5)
    expect(first).toHaveLength(6)

    const second = pageItems(all, 3, 1)
    expect(second.map((item) => (item.kind === 'project' ? item.project.id : 'manager'))).toEqual([
      'p_5',
      'p_6',
      'manager',
    ])
  })

  it('shows one project per page in a single column', () => {
    const all = [open('p_a', 0), open('p_b', 1)]

    expect(pageItems(all, 1, 0).map((item) => item.kind)).toEqual(['project'])
    expect(pageItems(all, 1, 1).map((item) => item.kind)).toEqual(['project'])
    // Past the last project is the manager's page, which is how it keeps its
    // place at the end without a cell to sit in.
    expect(pageItems(all, 1, 2)).toEqual([{ kind: 'manager' }])
  })

  it('clamps a page that no longer exists', () => {
    // The last project on the last page can be removed, and a window can narrow
    // until a page holds fewer projects.
    expect(clampPage(9, 2, 3)).toBe(0)
    expect(clampPage(2, 7, 3)).toBe(1)
    expect(clampPage(-4, 7, 3)).toBe(0)
    expect(clampPage(Number.NaN, 7, 3)).toBe(0)
  })

  it('knows which page a project is on', () => {
    const all = ['p_0', 'p_1', 'p_2', 'p_3', 'p_4', 'p_5'].map((id, index) => open(id, index))

    expect(pageOfProject(all, 'p_4', 3)).toBe(0)
    expect(pageOfProject(all, 'p_5', 3)).toBe(1)
    expect(pageOfProject(all, 'p_5', 2)).toBe(1)
    expect(pageOfProject(all, 'p_none', 3)).toBeNull()
  })
})

describe('joining the workspace', () => {
  it('takes the first free slot rather than the next number', () => {
    // A project removed from the middle leaves a gap, and the next one joins in
    // it: nobody else moves to make room.
    expect(firstFreeSlot([open('p_a', 0), open('p_c', 2)])).toBe(1)
  })

  it('starts at the first slot when nothing is reserved', () => {
    expect(firstFreeSlot([closed('p_a')])).toBe(0)
    expect(firstFreeSlot([])).toBe(0)
  })
})

describe('moving within the workspace', () => {
  const three = [open('p_a', 0), open('p_b', 1), open('p_c', 2)]

  it('swaps reservations with the neighbour, and writes both', () => {
    expect(neighbourSwap(three, 'p_b', 'left')).toEqual([
      { projectId: 'p_b', slot: 0 },
      { projectId: 'p_a', slot: 1 },
    ])
    expect(neighbourSwap(three, 'p_b', 'right')).toEqual([
      { projectId: 'p_b', slot: 2 },
      { projectId: 'p_c', slot: 1 },
    ])
  })

  it('has nothing to swap with at either end', () => {
    expect(neighbourSwap(three, 'p_a', 'left')).toBeNull()
    expect(neighbourSwap(three, 'p_c', 'right')).toBeNull()
  })

  it('exchanges the places a person can see, not the numbers they were stored as', () => {
    // Slots 0 and 40 are adjacent on screen, because the grid draws the
    // projects it has and not the numbers they claimed.
    const spread = [open('p_a', 0), open('p_b', 40)]

    expect(neighbourSwap(spread, 'p_b', 'left')).toEqual([
      { projectId: 'p_b', slot: 0 },
      { projectId: 'p_a', slot: 1 },
    ])
  })

  it('does nothing for a project that is not in the workspace', () => {
    expect(neighbourSwap([...three, closed('p_d')], 'p_d', 'left')).toBeNull()
  })
})

describe('pinning to a slot', () => {
  const three = [open('p_a', 0), open('p_b', 1), open('p_c', 2)]

  it('is one write when the slot is free', () => {
    expect(moveToSlot(three, 'p_a', 5)).toEqual([{ projectId: 'p_a', slot: 5 }])
  })

  it('displaces whoever holds it, so nobody is dropped', () => {
    expect(moveToSlot(three, 'p_a', 2)).toEqual([
      { projectId: 'p_a', slot: 2 },
      { projectId: 'p_c', slot: 0 },
    ])
  })

  it('unpins the occupant when there is no other reservation to hand over', () => {
    // Two projects claiming one slot: the second of them pinned to it has
    // nothing to trade, so the first is unpinned - which leaves it in the
    // workspace in the order it was already in, and ends the collision.
    const collided = [open('p_a', 0), open('p_b', 0)]

    expect(moveToSlot(collided, 'p_b', 0)).toEqual([
      { projectId: 'p_b', slot: 0 },
      { projectId: 'p_a', slot: null },
    ])
  })

  it('does nothing for a project that is not in the workspace', () => {
    expect(moveToSlot(three, 'p_none', 0)).toEqual([])
  })
})
