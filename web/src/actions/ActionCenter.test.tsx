import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { fetchActions } from '../api/client'
import { actionPath } from '../dashboard/route'
import { makeAction, makeActionQueue, makeResolvedAction } from '../test/actions'
import { ActionCenter } from './ActionCenter'

// Only the read is replaced. Everything between it and the markup - the polling
// hook, the grouping, the vocabulary module, the components - is the real thing,
// which is what makes a test here a test of the page rather than of a mock.
vi.mock('../api/client', async () => {
  const actual = await vi.importActual<typeof import('../api/client')>('../api/client')
  return { ...actual, fetchActions: vi.fn() }
})

const load = vi.mocked(fetchActions)

/**
 * The strings this build must never put on screen.
 *
 * They are here rather than inline so that every rendering test can assert the
 * same set: §16 of the phase brief is one claim, and a claim checked in three
 * places with three different spellings is three claims.
 */
const FORBIDDEN = [
  'deploy the scratch build',
  'deploy.sh',
  'sk-test-0000',
  'prompt',
  'tool_input',
  'toolInput',
  'transcript',
  'stdout',
  'rm -rf /',
]

/** The heading of one section, found by its accessible name. */
function section(container: HTMLElement, name: string): HTMLElement {
  const found = container.querySelector(`section[aria-label="${name}"]`)
  if (!(found instanceof HTMLElement)) throw new Error(`no section named ${name}`)
  return found
}

describe('ActionCenter', () => {
  beforeEach(() => {
    load.mockReset()
  })

  afterEach(() => {
    // The page polls; a test must not leave a timer behind for the next one.
    vi.useRealTimers()
  })

  it('says it is loading before the first response arrives', () => {
    load.mockReturnValue(new Promise(() => {}))
    render(<ActionCenter />)

    expect(screen.getByRole('status')).toHaveTextContent('Loading actions…')
  })

  // §13's Empty case, and the brief's §5: a queue with nothing in it is a page
  // that says so rather than a page that looks broken. The check that matters
  // is the second one - an empty list must not read as a failure.
  it('says nothing is waiting when nothing is', async () => {
    load.mockResolvedValue(makeActionQueue([]))
    const { container } = render(<ActionCenter />)

    expect(await screen.findByText('Nothing is waiting.')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByText('Loading actions…')).not.toBeInTheDocument()
    expect(container.querySelectorAll('section')).toHaveLength(0)
  })

  it('says it could not load, and retries', async () => {
    const user = userEvent.setup()
    load.mockRejectedValueOnce(new Error('the network went away'))
    load.mockResolvedValue(makeActionQueue([]))

    render(<ActionCenter />)

    expect(await screen.findByRole('alert')).toHaveTextContent('Unable to load actions')
    expect(screen.getByText('the network went away')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Retry' }))

    await waitFor(() => expect(load).toHaveBeenCalledTimes(2))
    expect(await screen.findByText('Nothing is waiting.')).toBeInTheDocument()
  })

  // §13's Permission case: the one action that really does block work, from the
  // bar at the top to the row at the bottom.
  it('shows a permission request as something that needs somebody', async () => {
    load.mockResolvedValue(
      makeActionQueue([
        makeAction({ projectName: 'checkout-service', reason: 'permission requested' }),
      ]),
    )
    const { container } = render(<ActionCenter />)
    await screen.findByRole('main')

    const needsYou = section(container, 'Needs you')
    expect(needsYou.querySelector('.action-section__heading')).toHaveTextContent('Needs you1')
    expect(needsYou).toHaveTextContent('Claude is waiting for permission')
    // The reason is the server's own fixed phrase, not a quotation from the
    // request - which is the whole of §16.
    expect(needsYou).toHaveTextContent('permission requested')
    expect(needsYou).toHaveTextContent('checkout-service')
    expect(needsYou.querySelector('.action-card__glyph')).toBeInTheDocument()
    expect(needsYou.querySelector('.status-badge--attention')).toBeInTheDocument()

    // The headline is the server's count, on the page's own bar.
    expect(container.querySelector('.actions-bar__number--needs-you')).toHaveTextContent('1')
  })

  // §13's Multiple-and-sorting case. The server sorted these; the page shows the
  // order it was given, inside each section and across them.
  it('keeps the server order inside every section', async () => {
    load.mockResolvedValue(
      makeActionQueue([
        makeAction({ id: 'act_1111', type: 'VIEW_FAILURE', level: 'WARNING', reason: 'agent failed' }),
        makeAction({ id: 'act_2222', projectName: 'studio', reason: 'permission requested' }),
        makeAction({ id: 'act_3333', type: 'VIEW_COMPLETION', level: 'INFO', reason: 'agent completed' }),
        makeAction({ id: 'act_4444', projectName: 'alpha', reason: 'permission requested' }),
        makeResolvedAction({ id: 'act_5555' }),
      ]),
    )
    const { container } = render(<ActionCenter />)
    await screen.findByRole('main')

    const hrefs = (name: string) =>
      Array.from(section(container, name).querySelectorAll('.action-card__link')).map((node) =>
        node.getAttribute('href'),
      )

    expect(hrefs('Needs you')).toEqual([actionPath('act_2222'), actionPath('act_4444')])
    expect(hrefs('Notices')).toEqual([actionPath('act_1111'), actionPath('act_3333')])
    expect(hrefs('Settled')).toEqual([actionPath('act_5555')])

    // The two counts on the bar are pending-only and exact, so a resolved row
    // sits in the list and in neither number. That is deliberate, and this is
    // where the intention is written down.
    expect(container.querySelectorAll('.action-card')).toHaveLength(5)
  })

  it('writes the count the server sent, not the length of the list', async () => {
    load.mockResolvedValue(
      makeActionQueue([
        makeAction({ id: 'act_1111' }),
        makeResolvedAction({ id: 'act_2222' }),
      ]),
    )
    const { container } = render(<ActionCenter />)
    await screen.findByRole('main')

    const numbers = Array.from(container.querySelectorAll('.actions-bar__number')).map(
      (node) => node.textContent,
    )
    expect(numbers).toEqual(['1', '0'])
  })

  // §13's Security case, and the one that fails the moment somebody writes
  // `{...action}`: the builder carries a prompt, a command, a token and a
  // transcript, and none of them is allowed anywhere on the page.
  it('renders the fields it names and no others', async () => {
    load.mockResolvedValue(makeActionQueue([makeAction()]))
    const { container } = render(<ActionCenter />)
    await screen.findByRole('main')

    const text = container.textContent ?? ''
    for (const word of FORBIDDEN) {
      expect(text).not.toContain(word)
    }

    // The positive half of the same claim: the three values a card is allowed to
    // show, named exactly. A field added to ActionItem has to be added here too,
    // which is what makes this a test of the shape rather than of today's markup.
    expect(container.querySelector('.action-card__type')?.textContent).toBe(
      'Claude is waiting for permission',
    )
    expect(container.querySelector('.action-card__reason')?.textContent).toBe('permission requested')
    expect(container.querySelector('.action-card__project')?.textContent).toBe('AgentMux')
  })

  // The id is the fallback rather than a blank: an action nobody can place is an
  // action nobody can go and look at.
  it('falls back to the project id when the name could not be resolved', async () => {
    load.mockResolvedValue(makeActionQueue([makeAction({ projectName: '' })]))
    const { container } = render(<ActionCenter />)
    await screen.findByRole('main')

    expect(container.querySelector('.action-card__project')).toHaveTextContent(
      'p_0123456789abcdef0123',
    )
  })

  // §13's Navigation case, as far as jsdom can take it: a page load is not
  // something jsdom does, so the address is this test's claim and the click is
  // the E2E's.
  it('links each row at its own address', async () => {
    load.mockResolvedValue(makeActionQueue([makeAction({ id: 'act_abcdef12' })]))
    const { container } = render(<ActionCenter />)
    await screen.findByRole('main')

    expect(container.querySelector('.action-card__link')).toHaveAttribute(
      'href',
      '/actions/act_abcdef12',
    )
  })

  // The fourth state, and the one worth having: a refresh that failed must not
  // blank a page whose data is still the best answer available.
  //
  // The poll interval is a parameter so this can watch a real poll happen. A
  // five-second wait driven by fake timers would be a test that raced the
  // renderer rather than one that observed it.
  it('keeps showing the queue when a later refresh fails, and says it is stale', async () => {
    load.mockResolvedValueOnce(makeActionQueue([makeAction()]))
    load.mockRejectedValue(new Error('refresh failed'))

    render(<ActionCenter pollMs={20} />)
    expect(await screen.findByText('Claude is waiting for permission')).toBeInTheDocument()

    expect(await screen.findByText(/Not current/)).toBeInTheDocument()
    // The row is still on screen, which is the point of the whole state.
    expect(screen.getByText('Claude is waiting for permission')).toBeInTheDocument()
  })

  // §12: polling, not a socket. A second response replaces the first without a
  // reload, which is the behaviour - not the interval - that matters.
  it('picks up a new action on the next poll', async () => {
    load.mockResolvedValueOnce(makeActionQueue([]))
    load.mockResolvedValue(makeActionQueue([makeAction({ projectName: 'studio' })]))

    render(<ActionCenter pollMs={20} />)
    expect(await screen.findByText('Nothing is waiting.')).toBeInTheDocument()

    expect(await screen.findByText('Claude is waiting for permission')).toBeInTheDocument()
    expect(screen.queryByText('Nothing is waiting.')).not.toBeInTheDocument()
  })

  it('offers a way to the console and to the workspace', async () => {
    load.mockResolvedValue(makeActionQueue([]))
    render(<ActionCenter />)
    await screen.findByRole('main')

    const bar = screen.getByRole('banner')
    expect(within(bar).getByRole('link', { name: 'Dashboard' })).toHaveAttribute(
      'href',
      '/dashboard',
    )
    expect(within(bar).getByRole('link', { name: 'Workspace' })).toHaveAttribute('href', '/')
  })
})
