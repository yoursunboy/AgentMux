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
function routeFetch(routes: Record<string, Route | (() => Route)>) {
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
    const resolved = typeof route === 'function' ? route() : route
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

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('App', () => {
  it('renders the server state, the project list, and the selected project', async () => {
    const project = makeProject({ name: 'AgentMux' })
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: [project], count: 1 } },
    })

    render(<App />)

    expect(await screen.findByRole('heading', { name: 'AgentMux' })).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('Online')
    expect(screen.getByText('Windows / WSL (Ubuntu-24.04)')).toBeInTheDocument()
    // The project is listed in the manager as well as opened in the panel.
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

  it('registers a discovered candidate and opens it', async () => {
    const candidate = makeCandidate({ name: 'Found It', hostPath: 'D:\\AI\\Projects\\Found It' })
    const registered = makeProject({ id: 'p_new', name: 'Found It', hostPath: candidate.hostPath })
    // The server gains the project only once the write lands, so the list is
    // what makes the refresh visible.
    let stored = false

    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': () => ({
        body: { projects: stored ? [registered] : [], count: stored ? 1 : 0 },
      }),
      'GET /api/projects/discover': { body: makeDiscovery({ candidates: [candidate] }) },
      'POST /api/projects/register': () => {
        stored = true
        return { status: 201, body: { project: registered } }
      },
    })

    render(<App />)
    await screen.findByText(/No projects registered yet/)

    await userEvent.click(screen.getByRole('button', { name: 'Scan' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Register Found It' }))

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Found It' })).toBeInTheDocument())
    expect(screen.getByText('amx-p_new')).toBeInTheDocument()
  })

  it('creates a project from the dialog and opens it', async () => {
    const created = makeProject({ id: 'p_created', name: 'NewApp' })
    let stored = false

    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': () => ({
        body: { projects: stored ? [created] : [], count: stored ? 1 : 0 },
      }),
      'POST /api/projects': () => {
        stored = true
        return { status: 201, body: { project: created } }
      },
    })

    render(<App />)
    await screen.findByText(/No projects registered yet/)

    await userEvent.click(screen.getByRole('button', { name: '+ New' }))
    await userEvent.type(screen.getByLabelText(/Project name/), 'NewApp')
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))

    await waitFor(() => expect(screen.getByText('amx-p_created')).toBeInTheDocument())
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

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
    await screen.findByRole('heading', { name: 'AgentMux' })

    await userEvent.click(screen.getByRole('button', { name: 'Register' }))
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

    await userEvent.click(screen.getByRole('button', { name: '+ New' }))
    await userEvent.type(screen.getByLabelText(/Project name/), 'NewApp')
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))

    expect(await screen.findByText(/a folder already exists at that path/)).toBeInTheDocument()
  })

  it('starts a runtime and takes the running state from the server, not from the click', async () => {
    // The panel does not flip its own status: a session can be started from
    // another tab or survive a server restart, so the server is the only
    // truthful source for what is running.
    let running = false
    const project = makeProject({ name: 'AgentMux' })

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

    const start = screen.getByRole('button', { name: 'Start runtime' })
    expect(start).toBeEnabled()
    await userEvent.click(start)

    await waitFor(() => expect(screen.getByRole('button', { name: 'Stop runtime' })).toBeEnabled())
    expect(screen.getByRole('button', { name: 'Start runtime' })).toBeDisabled()
  })

  it('refuses to start a runtime the server says it cannot host', async () => {
    // A Windows-native server reports the runtime as unavailable. Offering the
    // control anyway would let a user press a button that cannot work, and hide
    // the one sentence that says what to do about it.
    const project = makeProject({ name: 'AgentMux' })
    routeFetch({
      'GET /api/server': { body: makeServerInfo() },
      'GET /api/projects': { body: { projects: [project], count: 1 } },
    })

    render(<App />)
    await screen.findByRole('heading', { name: 'AgentMux' })

    expect(screen.getByRole('button', { name: 'Start runtime' })).toBeDisabled()
    expect(screen.getAllByText(/requires AgentMux Server to run inside WSL/).length).toBeGreaterThan(0)
  })

  it('shows the runtime’s own reason when a start fails, inside WSL or out of tmux', async () => {
    const project = makeProject({ name: 'AgentMux' })
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

    await userEvent.click(screen.getByRole('button', { name: 'Start runtime' }))

    expect(await screen.findByText(/tmux is not installed in linux/)).toBeInTheDocument()
  })
})
