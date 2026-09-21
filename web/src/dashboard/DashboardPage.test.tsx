import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { fetchDashboard } from '../api/client'
import { makeDashboard, makeProjectCard, makeQuietProjectCard } from '../test/controller'
import { setViewportWidth } from '../test/layout'
import { DashboardPage } from './DashboardPage'

// Only the read is replaced. Everything between it and the markup - the polling
// hook, the column hook, the status mapping, the components - is the real thing,
// which is what makes a test here a test of the page rather than of a mock.
vi.mock('../api/client', async () => {
  const actual = await vi.importActual<typeof import('../api/client')>('../api/client')
  return { ...actual, fetchDashboard: vi.fn() }
})

const load = vi.mocked(fetchDashboard)

/** The row a label belongs to on a named card. */
function row(card: HTMLElement, term: string): HTMLElement {
  const dt = Array.from(card.querySelectorAll('.project-card__term')).find(
    (node) => node.textContent === term,
  )
  const dd = dt?.nextElementSibling
  if (!(dd instanceof HTMLElement)) throw new Error(`no row for ${term}`)
  return dd
}

describe('DashboardPage', () => {
  beforeEach(() => {
    load.mockReset()
  })

  afterEach(() => {
    // The page polls; a test must not leave a timer behind for the next one.
    vi.useRealTimers()
  })

  it('says it is loading before the first response arrives', () => {
    load.mockReturnValue(new Promise(() => {}))
    render(<DashboardPage />)

    expect(screen.getByRole('status')).toHaveTextContent('Loading controller…')
  })

  // §12: a failure says what failed and offers a way to try again, rather than
  // leaving a blank page.
  it('says it could not load, and retries', async () => {
    const user = userEvent.setup()
    load.mockRejectedValueOnce(new Error('the network went away'))
    load.mockResolvedValue(makeDashboard({ projects: [], count: 0 }))

    render(<DashboardPage />)

    expect(await screen.findByRole('alert')).toHaveTextContent('Unable to load dashboard')
    expect(screen.getByText('the network went away')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Retry' }))

    await waitFor(() => expect(load).toHaveBeenCalledTimes(2))
    expect(await screen.findByText('No projects')).toBeInTheDocument()
  })

  it('offers a way back to the workspace when it cannot load', async () => {
    load.mockRejectedValue(new Error('nope'))
    render(<DashboardPage />)

    expect(await screen.findByRole('link', { name: 'Back to the workspace' })).toHaveAttribute(
      'href',
      '/',
    )
  })

  // §17's Empty case: an installation with nothing registered is a console that
  // says so, with the server block still above it.
  it('says there are no projects when there are none', async () => {
    load.mockResolvedValue(makeDashboard({ projects: [], count: 0 }))
    render(<DashboardPage />)

    expect(await screen.findByText('No projects')).toBeInTheDocument()
    expect(screen.getByRole('banner')).toBeInTheDocument()
  })

  // §17's Project case: the four values of a card, from the response.
  it('renders a project with its runtime, agent, attention and actions', async () => {
    load.mockResolvedValue(
      makeDashboard({
        projects: [
          makeProjectCard({
            name: 'checkout-service',
            runtime: { status: 'running' },
            agent: { available: true, sessionId: 'sess_x', status: 'RUNNING' },
            attention: { available: true, level: 'INFO', reason: 'agent completed' },
            actions: { available: true, pending: 1 },
          }),
        ],
        count: 1,
      }),
    )
    render(<DashboardPage />)

    const card = await screen.findByRole('article')
    expect(within(card).getByRole('heading', { level: 3 })).toHaveTextContent('checkout-service')
    expect(row(card, 'Runtime')).toHaveTextContent('running')
    expect(row(card, 'Agent')).toHaveTextContent('running')
    expect(row(card, 'Attention')).toHaveTextContent('info')
    expect(row(card, 'Actions')).toHaveTextContent('1')
  })

  it('renders a project nothing has run in without inventing a status', async () => {
    load.mockResolvedValue(makeDashboard({ projects: [makeQuietProjectCard()], count: 1 }))
    render(<DashboardPage />)

    const card = await screen.findByRole('article')
    expect(row(card, 'Agent')).toHaveTextContent('none')
    expect(row(card, 'Runtime')).toHaveTextContent('stopped')
  })

  // §17's Attention case, asserted on the tone the badge was given rather than
  // on a colour: the mapping is what decides, and the stylesheet is what paints.
  it('shows ACTION_REQUIRED as needing a person, with the reason', async () => {
    load.mockResolvedValue(
      makeDashboard({
        projects: [
          makeProjectCard({
            attention: {
              available: true,
              level: 'ACTION_REQUIRED',
              reason: 'permission requested',
            },
          }),
        ],
      }),
    )
    const { container } = render(<DashboardPage />)

    await screen.findByRole('article')
    expect(container.querySelector('.status-badge--attention')).toBeInTheDocument()
    expect(screen.getByText('permission requested')).toBeInTheDocument()
  })

  // §9: the page shows the order it was given. The server sorted it, and a
  // second sort here would be a second opinion about what matters.
  it('shows the cards in the order the server sent them', async () => {
    load.mockResolvedValue(
      makeDashboard({
        projects: [
          makeProjectCard({ id: 'p_1', name: 'blocked' }),
          makeProjectCard({ id: 'p_2', name: 'working' }),
          makeProjectCard({ id: 'p_3', name: 'idle' }),
        ],
        count: 3,
      }),
    )
    render(<DashboardPage />)

    const cards = await screen.findAllByRole('article')
    expect(cards).toHaveLength(3)
    const names = screen.getAllByRole('heading', { level: 3 }).map((node) => node.textContent)
    expect(names).toEqual(['blocked', 'working', 'idle'])
  })

  // §17's Responsive case. jsdom does not evaluate media queries, so a
  // stylesheet rule could not be checked here at all; the column count is a
  // value the hook produces and the grid publishes, which is checkable - and the
  // e2e asserts the painted layout in a real browser.
  it('lays out three columns on a desktop and one on a phone', async () => {
    load.mockResolvedValue(makeDashboard())
    const { container } = render(<DashboardPage />)
    await screen.findByRole('article')

    const grid = container.querySelector('.project-grid')
    expect(grid).toHaveAttribute('data-columns', '3')

    act(() => setViewportWidth(390))
    expect(grid).toHaveAttribute('data-columns', '1')

    act(() => setViewportWidth(768))
    expect(grid).toHaveAttribute('data-columns', '3')
  })

  // The fourth state, and the one worth having: a refresh that failed must not
  // blank a page whose data is still the best answer available.
  //
  // The poll interval is a parameter so this can watch a real poll happen. A
  // five-second wait driven by fake timers would be a test that raced the
  // renderer rather than one that observed it.
  it('keeps showing the data when a later refresh fails, and says it is stale', async () => {
    load.mockResolvedValueOnce(makeDashboard())
    load.mockRejectedValue(new Error('refresh failed'))

    render(<DashboardPage pollMs={20} />)
    expect(await screen.findByRole('article')).toBeInTheDocument()

    expect(await screen.findByText(/Not current/)).toBeInTheDocument()
    // The card is still on screen, which is the point of the whole state.
    expect(screen.getAllByRole('article')).toHaveLength(1)
  })
})
