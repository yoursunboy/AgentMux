import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { App } from './App'
import { makeDashboard, makeProjectCard } from './test/controller'
import { makeServerInfo } from './test/fixtures'

// The console's cards contain terminals, so this file needs the same three
// xterm doubles `components/TerminalView.test.tsx` states. Running the real one
// in jsdom would mean stubbing canvas and matchMedia so it can draw into a
// document nobody looks at.
vi.mock('@xterm/xterm', async () => {
  const double = await import('./test/xterm')
  return { Terminal: double.FakeTerminal }
})
vi.mock('@xterm/addon-fit', async () => {
  const double = await import('./test/xterm')
  return { FitAddon: double.FakeFitAddon }
})
vi.mock('@xterm/addon-unicode11', async () => {
  const double = await import('./test/xterm')
  return { Unicode11Addon: double.FakeUnicode11Addon }
})

// The application owns the page's terminal client, which the console's cards
// subscribe through. A test that let it build a real one would open a socket
// from jsdom and spend the test reconnecting to a server that is not there.
vi.mock('./terminal/client', async () => {
  const double = await import('./test/terminal')
  return { createTerminalClient: () => double.makeClient().client }
})

/**
 * Which page the application opens, at the level of the application.
 *
 * `dashboard/route.test.ts` covers the comparison itself, and the dashboard's
 * own suite covers everything below it. What is left is the wiring between
 * them, which is the one thing neither of those can check: that a location
 * actually reaches the console, and that the path it does not recognise still
 * lands on the workspace it always did.
 *
 * The terminal is not mocked here. The workspace branch is rendered by a test
 * that does not need a socket to exist, and the console branch has no terminal
 * in it at all - which is the point of the console being a separate page.
 */

/** A dashboard with one project, as the server would send it. */
const dashboard = makeDashboard({
  projects: [makeProjectCard({ name: 'checkout-service' })],
})

/**
 * routeFetch answers each path with what a server would say for it.
 *
 * A stub that answers every path with the same body is a stub that lies about
 * `/api/server`, and the workspace reads `warnings` off it - which is how the
 * first version of this file produced an uncaught TypeError in a test that was
 * passing. The paths are named so that cannot happen again.
 */
function routeFetch(routes: Record<string, unknown>) {
  const spy = vi.fn((input: RequestInfo | URL) => {
    const path = String(input).split('?')[0] ?? ''
    const body = routes[path]
    if (body === undefined) {
      return Promise.resolve(
        new Response(JSON.stringify({ error: { code: 'not_found', message: path } }), {
          status: 404,
          headers: { 'Content-Type': 'application/json' },
        }),
      )
    }
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
  })
  vi.stubGlobal('fetch', spy)
  return spy
}

/** The console, and the two reads the workspace makes for an empty workspace. */
const consoleAndWorkspace = {
  '/api/controller': dashboard,
  '/api/server': makeServerInfo(),
  '/api/projects': { projects: [], count: 0 },
}

describe('App routing', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    window.history.pushState({}, '', '/')
  })

  it('opens the console at /dashboard', async () => {
    window.history.pushState({}, '', '/dashboard')
    routeFetch(consoleAndWorkspace)

    render(<App />)

    expect(await screen.findByRole('article')).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'checkout-service' })).toBeInTheDocument()
    // The console's own bar, not the workspace's.
    expect(screen.getByRole('banner')).toHaveClass('server-bar')
  })

  it('treats a trailing slash as the same page', async () => {
    window.history.pushState({}, '', '/dashboard/')
    routeFetch(consoleAndWorkspace)

    render(<App />)
    expect(await screen.findByRole('article')).toBeInTheDocument()
  })

  // A deep link this build does not know lands on the workspace, which is what
  // the server serves at / and what it has always done.
  it('leaves an unknown path on the workspace', () => {
    window.history.pushState({}, '', '/something-else')
    routeFetch(consoleAndWorkspace)

    const { container } = render(<App />)

    expect(container.querySelector('.server-bar')).not.toBeInTheDocument()
    expect(container.querySelector('.global-bar')).toBeInTheDocument()
  })

  it('offers a way from the workspace to the console', async () => {
    window.history.pushState({}, '', '/')
    routeFetch(consoleAndWorkspace)

    render(<App />)

    expect(await screen.findByRole('link', { name: 'Console' })).toHaveAttribute(
      'href',
      '/dashboard',
    )
  })
})
