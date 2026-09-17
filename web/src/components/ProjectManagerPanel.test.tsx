import { render, screen } from '@testing-library/react'
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
    selectedId: null,
    onSelect: vi.fn(),
    ...overrides,
  }
}

describe('ProjectManagerPanel', () => {
  it('explains the empty state and offers both ways forward', () => {
    render(<ProjectManagerPanel {...props()} />)

    expect(screen.getByText(/No projects registered yet/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '+ New' })).toBeEnabled()
    expect(screen.getByRole('button', { name: 'Register' })).toBeEnabled()
  })

  it('lists projects and reports the selection', async () => {
    const project = makeProject()
    const onSelect = vi.fn()
    render(<ProjectManagerPanel {...props({ projects: [project], onSelect })} />)

    expect(screen.getByText('AgentMux')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: /AgentMux/ }))
    expect(onSelect).toHaveBeenCalledWith(project)
  })

  it('marks the selected project without reordering the list', () => {
    const first = makeProject({ id: 'p_1', name: 'Alpha' })
    const second = makeProject({ id: 'p_2', name: 'Beta' })
    render(
      <ProjectManagerPanel {...props({ projects: [first, second], selectedId: 'p_2' })} />,
    )

    const items = screen.getAllByRole('button', { name: /Alpha|Beta/ })
    expect(items.map((item) => item.textContent)).toEqual([
      expect.stringContaining('Alpha'),
      expect.stringContaining('Beta'),
    ])
    expect(items[1]).toHaveClass('is-selected')
  })

  it('shows a failure with a message rather than silently rendering nothing', () => {
    render(<ProjectManagerPanel {...props({ projectsError: new Error('the store is unavailable') })} />)
    expect(screen.getByText(/the store is unavailable/)).toBeInTheDocument()
  })
})

describe('ProjectManagerPanel discovery', () => {
  it('offers a scan before one has run and says what a scan does', () => {
    render(<ProjectManagerPanel {...props()} />)
    expect(screen.getByRole('button', { name: 'Scan' })).toBeEnabled()
    expect(screen.getByText(/Scanning only suggests/)).toBeInTheDocument()
  })

  it('lists only the candidates the user does not already have', () => {
    const discovery = makeDiscovery({
      candidates: [
        makeCandidate({ name: 'NewOne', hostPath: 'D:\\AI\\Projects\\NewOne' }),
        makeCandidate({
          name: 'AlreadyHave',
          hostPath: 'D:\\AI\\Projects\\AlreadyHave',
          registered: true,
          projectId: 'p_1',
        }),
      ],
    })
    render(<ProjectManagerPanel {...props({ discovery })} />)

    expect(screen.getByText('NewOne')).toBeInTheDocument()
    expect(screen.queryByText('AlreadyHave')).not.toBeInTheDocument()
  })

  it('explains why a discounted folder ranks lower instead of hiding the reason', () => {
    const discovery = makeDiscovery({
      candidates: [
        makeCandidate({ name: 'Reference', nameDiscounted: true, hostPath: 'D:\\AI\\Projects\\Reference' }),
      ],
    })
    render(<ProjectManagerPanel {...props({ discovery })} />)

    expect(screen.getByText(/folder name ranks it lower/)).toBeInTheDocument()
  })

  it('registers a candidate the user picks', async () => {
    const candidate = makeCandidate({ name: 'NewOne', hostPath: 'D:\\AI\\Projects\\NewOne' })
    const onRegisterCandidate = vi.fn()
    render(<ProjectManagerPanel {...props({ discovery: makeDiscovery({ candidates: [candidate] }), onRegisterCandidate })} />)

    await userEvent.click(screen.getByRole('button', { name: 'Register NewOne' }))
    expect(onRegisterCandidate).toHaveBeenCalledWith(candidate)
  })

  it('names each candidate Register button, so two are never indistinguishable', () => {
    const discovery = makeDiscovery({
      candidates: [makeCandidate({ name: 'One' }), makeCandidate({ name: 'Two', hostPath: 'D:\\AI\\Projects\\Two' })],
    })
    render(<ProjectManagerPanel {...props({ discovery })} />)

    expect(screen.getByRole('button', { name: 'Register One' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Register Two' })).toBeInTheDocument()
  })

  it('reports a root that is not available instead of failing the whole scan', () => {
    const discovery = makeDiscovery({
      roots: [{ path: 'D:\\Missing', exists: false, error: 'does not exist' }],
      candidates: [],
      warnings: ['Projects Root D:\\Missing does not exist'],
      scannedDirectories: 0,
    })
    render(<ProjectManagerPanel {...props({ discovery })} />)

    expect(screen.getByText('D:\\Missing')).toBeInTheDocument()
    expect(screen.getByText(/— does not exist/)).toBeInTheDocument()
    expect(screen.getByText(/Nothing new found/)).toBeInTheDocument()
  })

  it('says when a scan stopped early rather than presenting a short list as complete', () => {
    const discovery = makeDiscovery({ candidates: [], truncated: true })
    render(<ProjectManagerPanel {...props({ discovery })} />)
    expect(screen.getByText(/the scan stopped early/)).toBeInTheDocument()
  })

  it('disables the write controls while a request is in flight', () => {
    render(<ProjectManagerPanel {...props({ busy: true, discovery: makeDiscovery() })} />)

    expect(screen.getByRole('button', { name: '+ New' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Register' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Refresh' })).toBeDisabled()
  })
})
