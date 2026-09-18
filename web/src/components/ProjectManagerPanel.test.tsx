import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { ProjectManagerPanel } from './ProjectManagerPanel'
import { makeCandidate, makeDiscovery, makeProject } from '../test/fixtures'

function props(overrides: Partial<Parameters<typeof ProjectManagerPanel>[0]> = {}) {
  return {
    projects: [],
    projectsLoading: false,
    projectsError: null,
    discovery: null,
    discoveryLoading: false,
    discoveryError: null,
    busy: false,
    onOpenNew: vi.fn(),
    onOpenRegister: vi.fn(),
    onDiscover: vi.fn(),
    onRegisterCandidate: vi.fn(),
    onRefresh: vi.fn(),
    onOpenInWorkspace: vi.fn(),
    onRemoveFromWorkspace: vi.fn(),
    onMove: vi.fn(),
    onFocus: vi.fn(),
    ...overrides,
  }
}

/** A project that is in the workspace, which is what a reserved slot means. */
const open = (id: string, name: string, slot: number) =>
  makeProject({ id, name, pinnedSlot: slot })

/** A project that is registered and not in the workspace. */
const closed = (id: string, name: string) => makeProject({ id, name, pinnedSlot: null })

describe('ProjectManagerPanel', () => {
  it('explains the empty state and offers both ways forward', () => {
    render(<ProjectManagerPanel {...props()} />)

    expect(screen.getByText(/No projects registered yet/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '+ New Project' })).toBeEnabled()
    expect(screen.getByRole('button', { name: 'Open / Register' })).toBeEnabled()
  })

  it('shows a failure with a message rather than silently rendering nothing', () => {
    render(<ProjectManagerPanel {...props({ projectsError: new Error('the store is unavailable') })} />)

    expect(screen.getByText(/the store is unavailable/)).toBeInTheDocument()
  })
})

/**
 * The two lists are the workspace's membership model made visible: a project is
 * in the workspace exactly when it holds a slot, and everything else is
 * registered and waiting.
 */
describe('ProjectManagerPanel workspace membership', () => {
  it('separates what is open from what is merely registered', () => {
    render(
      <ProjectManagerPanel
        {...props({ projects: [open('p_1', 'Alpha', 0), closed('p_2', 'Beta')] })}
      />,
    )

    const inWorkspace = screen.getByRole('region', { name: 'In the workspace' })
    expect(within(inWorkspace).getByText('Alpha')).toBeInTheDocument()
    expect(within(inWorkspace).queryByText('Beta')).not.toBeInTheDocument()

    const registered = screen.getByRole('region', { name: 'Registered' })
    expect(within(registered).getByText('Beta')).toBeInTheDocument()
  })

  it('says nothing is open rather than showing an empty list', () => {
    render(<ProjectManagerPanel {...props({ projects: [closed('p_1', 'Alpha')] })} />)

    expect(screen.getByText(/Nothing is open/)).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Registered' })).toBeInTheDocument()
  })

  it('numbers the slots the way a person counts', () => {
    render(
      <ProjectManagerPanel
        {...props({ projects: [open('p_1', 'Alpha', 0), open('p_2', 'Beta', 2)] })}
      />,
    )

    // Slot 3 is reserved and slots 1 and 2 are not: the numbers are reservations
    // and not positions, and showing them as stored would be a lie about where
    // the panel is.
    expect(screen.getByLabelText('Slot 1')).toHaveTextContent('1')
    expect(screen.getByLabelText('Slot 2')).toHaveTextContent('3')
  })

  it('keeps the workspace in slot order however the server listed them', () => {
    render(
      <ProjectManagerPanel
        {...props({ projects: [open('p_2', 'Beta', 1), open('p_1', 'Alpha', 0)] })}
      />,
    )

    const rows = within(screen.getByRole('region', { name: 'In the workspace' })).getAllByRole(
      'listitem',
    )
    expect(rows.map((row) => within(row).getByRole('button', { name: /^Open/ }).textContent)).toEqual(
      [expect.stringContaining('Alpha'), expect.stringContaining('Beta')],
    )
  })

  it('opens a registered project into the workspace', async () => {
    const project = closed('p_2', 'Beta')
    const onOpenInWorkspace = vi.fn()
    render(<ProjectManagerPanel {...props({ projects: [project], onOpenInWorkspace })} />)

    await userEvent.click(screen.getByRole('button', { name: 'Open Beta in the workspace' }))
    expect(onOpenInWorkspace).toHaveBeenCalledWith(project)
  })

  it('takes a project off the grid without stopping anything', async () => {
    const project = open('p_1', 'Alpha', 0)
    const onRemoveFromWorkspace = vi.fn()
    render(<ProjectManagerPanel {...props({ projects: [project], onRemoveFromWorkspace })} />)

    const remove = screen.getByRole('button', { name: 'Remove Alpha from the workspace' })
    expect(remove).toHaveAttribute('title', expect.stringContaining('keeps running'))
    await userEvent.click(remove)
    expect(onRemoveFromWorkspace).toHaveBeenCalledWith(project)
  })

  it('offers a move left only when something is to the left', async () => {
    const onMove = vi.fn()
    const first = open('p_1', 'Alpha', 0)
    render(
      <ProjectManagerPanel
        {...props({ projects: [first, open('p_2', 'Beta', 1)], onMove })}
      />,
    )

    expect(screen.getByRole('button', { name: 'Move Alpha left' })).toBeDisabled()
    await userEvent.click(screen.getByRole('button', { name: 'Move Beta left' }))
    expect(onMove).toHaveBeenCalledWith(expect.objectContaining({ id: 'p_2' }), 'left')
  })

  it('focuses a project when its row is clicked', async () => {
    const project = open('p_1', 'Alpha', 0)
    const onFocus = vi.fn()
    render(<ProjectManagerPanel {...props({ projects: [project], onFocus })} />)

    await userEvent.click(screen.getByRole('button', { name: 'Open Alpha on its own' }))
    expect(onFocus).toHaveBeenCalledWith(project)
  })

  it('disables the membership controls while a request is in flight', () => {
    render(
      <ProjectManagerPanel
        {...props({ projects: [open('p_1', 'Alpha', 0), closed('p_2', 'Beta')], busy: true })}
      />,
    )

    expect(screen.getByRole('button', { name: 'Remove Alpha from the workspace' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Open Beta in the workspace' })).toBeDisabled()
  })
})

describe('ProjectManagerPanel discovery', () => {
  it('offers a scan before one has run and says what a scan does', () => {
    render(<ProjectManagerPanel {...props()} />)

    expect(screen.getByRole('button', { name: 'Scan' })).toBeEnabled()
    expect(screen.getByText(/Scanning only suggests/)).toBeInTheDocument()
  })

  it('lists only the candidates the user does not already have', () => {
    render(
      <ProjectManagerPanel
        {...props({
          discovery: makeDiscovery({
            candidates: [
              makeCandidate({ hostPath: 'D:\\AI\\Projects\\New', registered: false }),
              makeCandidate({ hostPath: 'D:\\AI\\Projects\\Known', registered: true }),
            ],
          }),
        })}
      />,
    )

    expect(screen.getByText('AgentMux')).toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: /^Register / })).toHaveLength(1)
  })

  it('explains why a discounted folder ranks lower instead of hiding the reason', () => {
    render(
      <ProjectManagerPanel
        {...props({
          discovery: makeDiscovery({
            candidates: [makeCandidate({ nameDiscounted: true })],
          }),
        })}
      />,
    )

    expect(screen.getByText(/folder name ranks it lower/)).toBeInTheDocument()
  })

  it('registers a candidate the user picks', async () => {
    const candidate = makeCandidate()
    const onRegisterCandidate = vi.fn()
    render(
      <ProjectManagerPanel
        {...props({ discovery: makeDiscovery({ candidates: [candidate] }), onRegisterCandidate })}
      />,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Register AgentMux' }))
    expect(onRegisterCandidate).toHaveBeenCalledWith(candidate)
  })

  it('reports a root that is not available instead of failing the whole scan', () => {
    render(
      <ProjectManagerPanel
        {...props({
          discovery: makeDiscovery({ roots: [{ path: 'D:\\Missing', exists: false, error: 'not found' }] }),
        })}
      />,
    )

    expect(screen.getByText(/not found/)).toBeInTheDocument()
  })

  it('says when a scan stopped early rather than presenting a short list as complete', () => {
    render(
      <ProjectManagerPanel {...props({ discovery: makeDiscovery({ truncated: true, candidates: [] }) })} />,
    )

    expect(screen.getByText(/the scan stopped early/)).toBeInTheDocument()
  })

  it('disables the write controls while a request is in flight', () => {
    render(<ProjectManagerPanel {...props({ busy: true })} />)

    expect(screen.getByRole('button', { name: '+ New Project' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Refresh' })).toBeDisabled()
  })
})
