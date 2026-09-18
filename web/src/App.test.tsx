import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { App } from './App'
import {
  makeCandidate,
  makeDiscovery,
  makeProject,
  makeServerInfo,
  makeTerminalReadyServerInfo,
} from './test/fixtures'
import { setViewportWidth } from './test/layout'
import type { Project } from './api/types'

// Two boundaries are replaced for this suite, and both are ones App does not
// implement: xterm's renderer, which cannot run in jsdom and whose fidelity is
// settled in a real browser, and the socket, which a test asserting on page
// composition has no reason to open. Everything in between - the subscription,
// the sequence rules, the frames - is exercised by its own suite.
vi.mock('./components/TerminalView', async () => {
  const double = await import('./test/terminal')
  return { TerminalView: double.FakeTerminalView }
})

vi.mock('./terminal/client', async () => {
  const double = await import('./test/terminal')
  return { createTerminalClient: () => double.makeClient().client }
})

interface Route {
  status?: number
  body: unknown
}

/**
 * routeFetch answers each "METHOD /path" from a table, so a test states what the
 * server would say instead of asserting on a mocked client module.
 *
 * The method is part of the key because listing and creating share a path.
 */
function routeFetch(routes: Record<string, Route | ((init?: RequestInit) => Route)>) {
  const spy = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    const path = url.split('?')[0] ?? url
    const method = (init?.method ?? 'GET').toUpperCase()
    const route = routes[`${method} ${path}`]
    if (!route) {
      return Promise.resolve(
        new Response(JSON.stringify({ error: { code: 'not_found', message: `no route for ${method} ${path}` } }), {
          status: 404,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
    }
    const resolved = typeof route === 'function' ? route(init) : route
    return Promise.resolve(
      new Response(JSON.stringify(resolved.body), {
        status: resolved.status ?? 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
  })
  vi.stubGlobal('fetch', spy)
  return spy
}

/** A project that is in the workspace, which is what a reserved slot means. */
const openIn = (project: Project, slot = 0): Project => ({ ...project, pinnedSlot: slot })

/** The cells of the workspace, in the order they are drawn. */
function cells(): HTMLElement[] {
  return Array.from(document.querySelectorAll('.workspace > *')) as HTMLElement[]
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('App', () => {
  it('renders the server state, the project list, and the open project’s panel', async () => {
    const project = openIn(makeProject({ name: 'AgentMux' }))
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: [project], count: 1 } },
    })

    render(<App />)

    expect(await screen.findByRole('heading', { name: 'AgentMux' })).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('Online')
    expect(screen.getByText('Windows / WSL (Ubuntu-24.04)')).toBeInTheDocument()
    // The project is listed in the manager as well as opened in a panel.
    expect(within(screen.getByLabelText('Projects')).getByText('AgentMux')).toBeInTheDocument()
  })

  it('never auto-scans on load, because a scan walks the filesystem', async () => {
    const spy = routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: [], count: 0 } },
    })

    render(<App />)
    await screen.findByText(/No projects registered yet/)

    expect(spy.mock.calls.map((call) => String(call[0]))).not.toContain('/api/projects/discover')
  })

  it('reports an unreachable server instead of rendering an empty page', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))),
    )

    render(<App />)

    expect(await screen.findByRole('alert')).toHaveTextContent(/Could not reach the AgentMux server/)
    expect(screen.getByRole('status')).toHaveTextContent('Offline')
  })

  it('surfaces a failing project list rather than showing an empty one', async () => {
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': {
        status: 500,
        body: { error: { code: 'internal_error', message: 'the store is unavailable' } },
      },
    })

    render(<App />)

    expect(await screen.findByText(/the store is unavailable/)).toBeInTheDocument()
  })

  it('scans only when the user asks, and lists what it found', async () => {
    const candidate = makeCandidate({ name: 'Found It', hostPath: 'D:\\AI\\Projects\\Found It' })
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: [], count: 0 } },
      'GET /api/projects/discover': { body: makeDiscovery({ candidates: [candidate] }) },
    })

    render(<App />)
    await screen.findByText(/No projects registered yet/)

    await userEvent.click(screen.getByRole('button', { name: 'Scan' }))
    expect(await screen.findByText('Found It')).toBeInTheDocument()
  })
})

/**
 * Joining the workspace is a write of its own. A project that was created or
 * registered and then appeared nowhere would look like the action failed.
 */
describe('App workspace membership', () => {
  it('registers a discovered candidate and opens a panel for it', async () => {
    const candidate = makeCandidate({ name: 'Found It', hostPath: 'D:\\AI\\Projects\\Found It' })
    const registered = makeProject({ id: 'p_new', name: 'Found It', hostPath: candidate.hostPath })
    let stored = false
    const pins: Array<{ slot: number | null }> = []

    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': () => ({
        body: {
          projects: stored ? [{ ...registered, pinnedSlot: pins[0]?.slot ?? null }] : [],
          count: stored ? 1 : 0,
        },
      }),
      'GET /api/projects/discover': { body: makeDiscovery({ candidates: [candidate] }) },
      'POST /api/projects/register': () => {
        stored = true
        return { status: 201, body: { project: registered } }
      },
      'PATCH /api/projects/p_new': (init) => {
        const { pinnedSlot } = JSON.parse(String(init?.body ?? '{}'))
        pins.push({ slot: pinnedSlot })
        return { body: { project: { ...registered, pinnedSlot } } }
      },
    })

    render(<App />)
    await screen.findByText(/No projects registered yet/)

    await userEvent.click(screen.getByRole('button', { name: 'Scan' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Register Found It' }))

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Found It' })).toBeInTheDocument())
    expect(pins).toEqual([{ slot: 0 }])
  })

  it('creates a project from the dialog and opens a panel for it', async () => {
    const created = makeProject({ id: 'p_created', name: 'NewApp' })
    let stored = false
    let pinned: number | null | undefined

    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': () => ({
        body: {
          projects: stored ? [{ ...created, pinnedSlot: pinned ?? null }] : [],
          count: stored ? 1 : 0,
        },
      }),
      'POST /api/projects': () => {
        stored = true
        return { status: 201, body: { project: created } }
      },
      'PATCH /api/projects/p_created': (init) => {
        pinned = JSON.parse(String(init?.body ?? '{}')).pinnedSlot
        return { body: { project: { ...created, pinnedSlot: pinned } } }
      },
    })

    render(<App />)
    await screen.findByText(/No projects registered yet/)

    await userEvent.click(screen.getByRole('button', { name: '+ New Project' }))
    await userEvent.type(screen.getByLabelText(/Project name/), 'NewApp')
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))

    await waitFor(() => expect(screen.getByRole('heading', { name: 'NewApp' })).toBeInTheDocument())
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(pinned).toBe(0)
  })

  it('leaves a registered project off the grid until it is opened', async () => {
    // Registration and workspace membership are two different things: fifty
    // registered projects and eight panels is the ordinary case.
    const project = makeProject({ name: 'AgentMux', pinnedSlot: null })
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: [project], count: 1 } },
    })

    render(<App />)
    await screen.findByText(/Nothing is open/)

    expect(screen.queryByRole('heading', { name: 'AgentMux' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Open AgentMux in the workspace' })).toBeInTheDocument()
  })

  it('opens a project from the manager, and takes it off the grid without stopping it', async () => {
    let slot: number | null = null
    const project = makeProject({ id: 'p_1', name: 'AgentMux', status: 'running' })
    const calls: string[] = []

    routeFetch({
      'GET /api/server': { body: makeTerminalReadyServerInfo() },
      'GET /api/projects': () => ({
        body: { projects: [{ ...project, pinnedSlot: slot }], count: 1 },
      }),
      'PATCH /api/projects/p_1': (init) => {
        slot = JSON.parse(String(init?.body ?? '{}')).pinnedSlot
        return { body: { project: { ...project, pinnedSlot: slot } } }
      },
      'POST /api/projects/p_1/runtime/stop': () => {
        calls.push('stop')
        return { body: { runtime: { projectId: 'p_1', state: 'STOPPED' } } }
      },
    })

    render(<App />)
    await screen.findByText(/Nothing is open/)

    await userEvent.click(screen.getByRole('button', { name: 'Open AgentMux in the workspace' }))
    expect(await screen.findByRole('heading', { name: 'AgentMux' })).toBeInTheDocument()
    expect(slot).toBe(0)

    await userEvent.click(screen.getByRole('button', { name: 'Remove AgentMux from the workspace' }))
    await waitFor(() => expect(screen.queryByRole('heading', { name: 'AgentMux' })).not.toBeInTheDocument())
    expect(slot).toBeNull()
    // The important half: taking a panel off the grid is not stopping anything.
    expect(calls).toEqual([])
  })
})

describe('App layout', () => {
  const seven = Array.from({ length: 7 }, (_, index) =>
    makeProject({ id: `p_${index}`, name: `Project ${index + 1}`, pinnedSlot: index }),
  )

  it('keeps the Project Manager in the last cell of the page', async () => {
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: seven.slice(0, 2), count: 2 } },
    })

    render(<App />)
    await screen.findByRole('region', { name: 'Projects' })

    const drawn = cells()
    expect(drawn.at(-1)).toHaveAttribute('aria-label', 'Projects')
  })

  it('shows five projects and the manager on the first of two pages', async () => {
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: seven, count: 7 } },
    })

    render(<App />)
    await screen.findByRole('status', { name: '' })

    expect(screen.getByText('1 / 2')).toBeInTheDocument()
    const drawn = cells()
    expect(drawn).toHaveLength(6)
    expect(drawn.at(-1)).toHaveAttribute('aria-label', 'Projects')
    expect(within(drawn[0] as HTMLElement).getByRole('heading')).toHaveTextContent('Project 1')
  })

  it('puts the two remaining projects and the manager on the second page', async () => {
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: seven, count: 7 } },
    })

    render(<App />)
    await userEvent.click(await screen.findByRole('button', { name: 'Next page' }))

    expect(screen.getByText('2 / 2')).toBeInTheDocument()
    const drawn = cells()
    expect(drawn).toHaveLength(3)
    expect(within(drawn[0] as HTMLElement).getByRole('heading')).toHaveTextContent('Project 6')
    expect(drawn.at(-1)).toHaveAttribute('aria-label', 'Projects')
  })

  it('counts the workspace, not the registry, when it works out how many pages there are', async () => {
    // Fifty registered projects and eight panels is the ordinary case. A pager
    // that counted registrations would offer forty-two pages of nothing - which
    // it did, until a browser test on a phone found it.
    const open = makeProject({ id: 'p_open', name: 'AgentMux', pinnedSlot: 0 })
    const registered = Array.from({ length: 6 }, (_, index) =>
      makeProject({ id: `p_closed_${index}`, name: `Closed ${index}`, pinnedSlot: null }),
    )
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: [open, ...registered], count: 7 } },
    })

    render(<App />)
    await screen.findByRole('heading', { name: 'AgentMux' })

    expect(screen.queryByRole('group', { name: 'Workspace pages' })).not.toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Registered' })).toBeInTheDocument()
  })

  it('shows one project per page on a phone, with the manager on a page of its own', async () => {
    setViewportWidth(390)
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: seven.slice(0, 2), count: 2 } },
    })

    render(<App />)
    await screen.findByRole('heading', { name: 'Project 1' })

    // One column, so a page is one project - and the manager cannot be a last
    // cell in a single-cell page, so it is a page of its own at the end.
    expect(screen.getByText('1 / 3')).toBeInTheDocument()
    expect(cells()).toHaveLength(1)
  })

  it('does not move a project when its runtime state changes', async () => {
    // The property a grid of terminals lives or dies by: the panel somebody is
    // reading stays where it is.
    let running = false
    const beta = makeProject({ id: 'p_b', name: 'Beta', pinnedSlot: 1, status: 'stopped' })
    const alpha = makeProject({ id: 'p_a', name: 'Alpha', pinnedSlot: 0, status: 'running' })
    const gamma = makeProject({ id: 'p_c', name: 'Gamma', pinnedSlot: 2, status: 'running' })

    routeFetch({
      'GET /api/server': { body: makeTerminalReadyServerInfo() },
      'GET /api/projects': () => ({
        body: {
          projects: [
            alpha,
            { ...beta, status: running ? 'running' : 'error' },
            gamma,
          ],
          count: 3,
        },
      }),
      'POST /api/projects/p_b/runtime/start': () => {
        running = true
        return { body: { runtime: { projectId: 'p_b', state: 'RUNNING' } } }
      },
    })

    render(<App />)
    await screen.findByRole('heading', { name: 'Beta' })

    const names = () => cells().slice(0, 3).map((cell) => within(cell).getAllByRole('heading')[0]?.textContent)
    expect(names()).toEqual(['Alpha', 'Beta', 'Gamma'])

    await userEvent.click(screen.getByRole('button', { name: 'Start' }))

    await waitFor(() =>
      expect(within(cells()[1] as HTMLElement).getByText('Running')).toBeInTheDocument(),
    )
    expect(names()).toEqual(['Alpha', 'Beta', 'Gamma'])
  })

  it('switches the grid to two columns below the three-column breakpoint', async () => {
    setViewportWidth(1180)
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: seven, count: 7 } },
    })

    render(<App />)
    await screen.findByText('1 / 3')

    // Two columns hold three projects, so seven of them are three pages.
    expect(document.querySelector('.workspace')).toHaveStyle({
      gridTemplateColumns: 'repeat(2, minmax(0, 1fr))',
    })
  })
})

describe('App focus', () => {
  it('shows one project on its own and comes back to the grid', async () => {
    const alpha = makeProject({ id: 'p_a', name: 'Alpha', pinnedSlot: 0, status: 'stopped' })
    const beta = makeProject({ id: 'p_b', name: 'Beta', pinnedSlot: 1, status: 'stopped' })
    routeFetch({
      'GET /api/server': { body: makeTerminalReadyServerInfo() },
      'GET /api/projects': { body: { projects: [alpha, beta], count: 2 } },
    })

    render(<App />)
    await screen.findByRole('heading', { name: 'Alpha' })

    await userEvent.click(screen.getByRole('button', { name: 'Open Alpha on its own' }))
    expect(screen.getByRole('heading', { name: 'Alpha' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Beta' })).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Projects')).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Back to the workspace' }))
    expect(screen.getByRole('heading', { name: 'Beta' })).toBeInTheDocument()
    expect(screen.getByLabelText('Projects')).toBeInTheDocument()
  })
})

describe('App runtime actions', () => {
  it('takes the running state from the server, not from the click', async () => {
    let running = false
    const project = makeProject({ name: 'AgentMux', pinnedSlot: 0 })

    routeFetch({
      'GET /api/server': { body: makeTerminalReadyServerInfo() },
      'GET /api/projects': () => ({
        body: { projects: [{ ...project, status: running ? 'running' : 'stopped' }], count: 1 },
      }),
      [`POST /api/projects/${project.id}/runtime/start`]: () => {
        running = true
        return { body: { runtime: { projectId: project.id, state: 'RUNNING' } } }
      },
    })

    render(<App />)
    await screen.findByRole('heading', { name: 'AgentMux' })

    const start = screen.getByRole('button', { name: 'Start' })
    expect(start).toBeEnabled()
    await userEvent.click(start)

    await waitFor(() => expect(screen.getByText('Running', { selector: '.panel__status' })).toBeInTheDocument())
  })

  it('refuses to offer a start the server says it cannot host', async () => {
    // A Windows-native server reports the runtime as unavailable. Offering the
    // control anyway would let a user press a button that cannot work, and hide
    // the one sentence that says what to do about it.
    const project = makeProject({ name: 'AgentMux', pinnedSlot: 0 })
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: [project], count: 1 } },
    })

    render(<App />)
    await screen.findByRole('heading', { name: 'AgentMux' })

    expect(screen.getByText('Terminal runtime unavailable')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Start' })).not.toBeInTheDocument()
    expect(screen.getAllByText(/requires AgentMux Server to run inside WSL/).length).toBeGreaterThan(0)
  })

  it('shows the runtime’s own reason when a start fails, inside WSL or out of tmux', async () => {
    const project = makeProject({ name: 'AgentMux', pinnedSlot: 0 })
    routeFetch({
      'GET /api/server': { body: makeTerminalReadyServerInfo() },
      'GET /api/projects': { body: { projects: [project], count: 1 } },
      [`POST /api/projects/${project.id}/runtime/start`]: {
        status: 503,
        body: {
          error: {
            code: 'runtime_unavailable',
            message: 'Runtime unavailable: tmux is not installed in linux.',
          },
        },
      },
    })

    render(<App />)
    await screen.findByRole('heading', { name: 'AgentMux' })

    await userEvent.click(screen.getByRole('button', { name: 'Start' }))

    expect(await screen.findByText(/tmux is not installed in linux/)).toBeInTheDocument()
  })
})

describe('App dialogs', () => {
  it('explains a duplicate registration and leaves the message where it happened', async () => {
    const project = makeProject({ name: 'AgentMux' })
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: [project], count: 1 } },
      'GET /api/projects/discover': { body: makeDiscovery({ candidates: [] }) },
      'POST /api/projects/register': {
        status: 409,
        body: {
          error: {
            code: 'project_already_registered',
            message: 'that folder is already registered',
            details: { hostPath: project.hostPath },
          },
        },
      },
    })

    render(<App />)
    await screen.findByRole('region', { name: 'Projects' })

    await userEvent.click(screen.getByRole('button', { name: 'Open / Register' }))
    const dialog = await screen.findByRole('dialog')
    await userEvent.type(screen.getByLabelText(/Folder path/), project.hostPath)
    await userEvent.click(within(dialog).getByRole('button', { name: 'Register' }))

    const message = await within(dialog).findByText(/already registered/)
    expect(message).toBeInTheDocument()
    expect(dialog).toBeInTheDocument()
  })

  it('shows a create failure in the dialog that caused it, not only in the console', async () => {
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: [], count: 0 } },
      'POST /api/projects': {
        status: 400,
        body: { error: { code: 'invalid_input', message: 'a folder already exists at that path' } },
      },
    })

    render(<App />)
    await screen.findByText(/No projects registered yet/)

    await userEvent.click(screen.getByRole('button', { name: '+ New Project' }))
    await userEvent.type(screen.getByLabelText(/Project name/), 'NewApp')
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))

    expect(await screen.findByText(/a folder already exists at that path/)).toBeInTheDocument()
  })
})
