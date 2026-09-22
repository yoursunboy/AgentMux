import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { fetchAction } from '../api/client'
import { makeAction, makeResolvedAction } from '../test/actions'
import { ActionDetail } from './ActionDetail'

// Only the read is replaced - see the same note in `ActionCenter.test.tsx`.
vi.mock('../api/client', async () => {
  const actual = await vi.importActual<typeof import('../api/client')>('../api/client')
  return { ...actual, fetchAction: vi.fn() }
})

const load = vi.mocked(fetchAction)

const ID = 'act_0123456789abcdef0123'

/**
 * A timestamp a given number of minutes ago.
 *
 * The component reads the clock itself, which is the right thing for it to do
 * and the reason a literal date cannot be used here: "10 minutes ago" is only a
 * fixed answer if the instant it is measured from is fixed too.
 */
function minutesAgo(minutes: number): string {
  return new Date(Date.now() - minutes * 60_000).toISOString()
}

/** The value half of a labelled row. */
function row(container: HTMLElement, term: string): HTMLElement {
  const dt = Array.from(container.querySelectorAll('.action-detail__term')).find(
    (node) => node.textContent === term,
  )
  const dd = dt?.nextElementSibling
  if (!(dd instanceof HTMLElement)) throw new Error(`no row for ${term}`)
  return dd
}

describe('ActionDetail', () => {
  beforeEach(() => {
    load.mockReset()
  })

  it('says it is loading before the response arrives', () => {
    load.mockReturnValue(new Promise(() => {}))
    render(<ActionDetail id={ID} />)

    expect(screen.getByRole('status')).toHaveTextContent('Loading action…')
  })

  it('says it could not read this action, and retries', async () => {
    const user = userEvent.setup()
    load.mockRejectedValueOnce(new Error('no such action'))
    load.mockResolvedValue(makeAction())

    render(<ActionDetail id={ID} />)

    expect(await screen.findByRole('alert')).toHaveTextContent('Unable to load this action')
    expect(screen.getByText('no such action')).toBeInTheDocument()

    await user.click(screen.getByRole('button', { name: 'Retry' }))

    await waitFor(() => expect(load).toHaveBeenCalledTimes(2))
    expect(await screen.findByRole('heading', { level: 1 })).toHaveTextContent(
      'Claude is waiting for permission',
    )
  })

  it('asks for the action it was given the id of', async () => {
    load.mockResolvedValue(makeAction())
    render(<ActionDetail id={ID} />)
    await screen.findByRole('heading', { level: 1 })

    expect(load).toHaveBeenCalledWith(ID, expect.anything())
  })

  // §7 of the phase brief, as a set rather than as a picture: the seven rows,
  // in this order. A field added to the response has to be added here too, or
  // this fails - which is what makes it a test of the shape.
  it('shows exactly the seven rows it declares', async () => {
    load.mockResolvedValue(makeAction())
    const { container } = render(<ActionDetail id={ID} />)
    await screen.findByRole('heading', { level: 1 })

    const terms = Array.from(container.querySelectorAll('.action-detail__term')).map(
      (node) => node.textContent,
    )
    expect(terms).toEqual(['Project', 'Session', 'Type', 'Status', 'Reason', 'Created', 'Resolved'])
  })

  // §7's mock, literally: "10 minutes ago" rather than a clock time.
  it('says how long ago it was created, in words', async () => {
    load.mockResolvedValue(makeAction({ createdAt: minutesAgo(10) }))
    const { container } = render(<ActionDetail id={ID} />)
    await screen.findByRole('heading', { level: 1 })

    expect(row(container, 'Created')).toHaveTextContent('10 minutes ago')
    // Pending, so there is nothing to resolve it and the row says so rather
    // than showing an epoch or a blank.
    expect(row(container, 'Resolved')).toHaveTextContent('-')
  })

  it('says when it was resolved once it has been', async () => {
    load.mockResolvedValue(
      makeResolvedAction({ createdAt: minutesAgo(60), resolvedAt: minutesAgo(5) }),
    )
    const { container } = render(<ActionDetail id={ID} />)
    await screen.findByRole('heading', { level: 1 })

    expect(row(container, 'Resolved')).toHaveTextContent('5 minutes ago')
    expect(row(container, 'Status')).toHaveTextContent('resolved')
  })

  it('names the session the action is about, which is what makes it findable', async () => {
    load.mockResolvedValue(makeAction())
    const { container } = render(<ActionDetail id={ID} />)
    await screen.findByRole('heading', { level: 1 })

    expect(row(container, 'Session')).toHaveTextContent('sess_0123456789abcdef0123')
    expect(row(container, 'Type')).toHaveTextContent('PERMISSION_REQUEST')
    expect(row(container, 'Status')).toHaveTextContent('needs you')
  })

  it('falls back to the project id when the name could not be resolved', async () => {
    load.mockResolvedValue(makeAction({ projectName: '' }))
    const { container } = render(<ActionDetail id={ID} />)
    await screen.findByRole('heading', { level: 1 })

    expect(row(container, 'Project')).toHaveTextContent('p_0123456789abcdef0123')
  })

  // §6 and §16 together, as the single claim they are: this page has no way to
  // answer an action, and no way to show what was asked. The absence of a
  // control is asserted rather than described, because a description would not
  // fail when somebody added one.
  it('offers no way to answer, approve, deny or acknowledge', async () => {
    load.mockResolvedValue(makeAction())
    render(<ActionDetail id={ID} />)
    await screen.findByRole('heading', { level: 1 })

    expect(screen.queryAllByRole('button')).toHaveLength(0)
    expect(screen.getByText(/The answer is given at the terminal/)).toBeInTheDocument()
  })

  it('never renders what was actually asked for', async () => {
    load.mockResolvedValue(makeAction())
    const { container } = render(<ActionDetail id={ID} />)
    await screen.findByRole('heading', { level: 1 })

    const text = container.textContent ?? ''
    for (const word of ['deploy the scratch build', 'deploy.sh', 'sk-test-0000', 'tool_input', 'transcript', 'stdout']) {
      expect(text).not.toContain(word)
    }
  })

  it('offers a way back to the queue and to the project', async () => {
    load.mockResolvedValue(makeAction())
    render(<ActionDetail id={ID} />)
    await screen.findByRole('heading', { level: 1 })

    expect(screen.getByRole('link', { name: /All actions/ })).toHaveAttribute('href', '/actions')
    // The workspace has no URL-addressed project, so this is the workspace root
    // and not the project: a link carrying an id nothing read would be a link
    // that appeared to work.
    expect(screen.getByRole('link', { name: 'Open project' })).toHaveAttribute('href', '/')
  })

  it('keeps showing the action when a later refresh fails, and says it is stale', async () => {
    load.mockResolvedValueOnce(makeAction())
    load.mockRejectedValue(new Error('refresh failed'))

    render(<ActionDetail id={ID} pollMs={20} />)
    expect(await screen.findByRole('heading', { level: 1 })).toBeInTheDocument()

    expect(await screen.findByText(/Not current/)).toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent(
      'Claude is waiting for permission',
    )
  })

  // A newer server that grows a type must still be readable here rather than
  // rendering an empty heading over an empty page.
  it('shows a type and a status it does not know rather than blanking them', async () => {
    load.mockResolvedValue(makeAction({ type: 'SOMETHING_NEW', status: 'SOMETHING_NEW' }))
    const { container } = render(<ActionDetail id={ID} />)

    expect(await screen.findByRole('heading', { level: 1 })).toHaveTextContent('SOMETHING_NEW')
    expect(row(container, 'Status')).toHaveTextContent('SOMETHING_NEW')
    expect(within(row(container, 'Status')).getByText('SOMETHING_NEW')).toBeInTheDocument()
  })
})
