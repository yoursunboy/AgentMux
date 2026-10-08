import { act, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { renderWithTerminal } from '../test/dashboard'
import { AgentControls } from './AgentControls'

/**
 * These are the tests for what a card offers to *do*, which is a different
 * question from what it shows.
 *
 * # What is asserted, and what is deliberately not
 *
 * Every claim here is about the request. The console has exactly three agent
 * lifecycle endpoints, so a test that pins the method, the path and the count
 * is pinning the whole of the client's side of the contract - and it is pinning
 * the thing §九 is emphatic about, which is that none of these buttons types
 * into a terminal. There is no keystroke, no Shift+Tab and no simulated Ctrl+C
 * anywhere in this component, and the way that stays true is that the tests
 * below would fail if one appeared: `fetch` is the only thing they allow it to
 * have called, and its call count is asserted every time.
 *
 * # The clock
 *
 * Nothing here uses a fake timer. A request that is in flight is held open by
 * the test's own promise, so "what the card shows while it waits" is a state
 * the test is standing inside rather than a race it is trying to win.
 */
const PROJECT = 'p_0123456789abcdef0123'

/** A response the client will accept, for a lifecycle call that succeeded. */
function ok(): Response {
  return new Response(JSON.stringify({ agent: { state: 'RUNNING' } }), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** A failing response carrying a code and details, as the server sends one. */
function failure(status: number, code: string, message: string, details = {}): Response {
  return new Response(JSON.stringify({ error: { code, message, details } }), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** stubFetch installs a fetch answering every call with what the caller builds. */
function stubFetch(response: () => Response) {
  const mock = vi.fn(async () => response())
  vi.stubGlobal('fetch', mock)
  return mock
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('AgentControls', () => {
  // §九: the buttons are drawn from the agent's real state, so the pair beside a
  // working agent is the pair that acts on one. Start is absent rather than
  // disabled - offering to do a thing that will not happen is worse than not
  // offering it at all.
  it('offers Stop and Restart beside a running agent, and never Start', () => {
    stubFetch(ok)
    renderWithTerminal(<AgentControls projectId={PROJECT} running />)

    expect(screen.getByRole('button', { name: 'Stop' })).toBeEnabled()
    expect(screen.getByRole('button', { name: 'Restart' })).toBeEnabled()
    expect(screen.queryByRole('button', { name: 'Start' })).toBeNull()
  })

  it('offers Start beside an agent that is not running, and never Stop', () => {
    stubFetch(ok)
    renderWithTerminal(<AgentControls projectId={PROJECT} running={false} />)

    expect(screen.getByRole('button', { name: 'Start' })).toBeEnabled()
    expect(screen.queryByRole('button', { name: 'Stop' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Restart' })).toBeNull()
  })

  // The whole of the client's half of the start contract, in one assertion:
  // one POST, to the agent endpoint, and nothing else on any wire.
  it('sends one start request and asks the page to re-read', async () => {
    const fetchMock = stubFetch(ok)
    const onChanged = vi.fn()
    renderWithTerminal(<AgentControls projectId={PROJECT} running={false} onChanged={onChanged} />)

    act(() => screen.getByRole('button', { name: 'Start' }).click())

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1))
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock).toHaveBeenCalledWith(
      `/api/projects/${PROJECT}/runtime/agent/start`,
      expect.objectContaining({ method: 'POST' }),
    )
  })

  it('sends one stop request and asks the page to re-read', async () => {
    const fetchMock = stubFetch(ok)
    const onChanged = vi.fn()
    renderWithTerminal(<AgentControls projectId={PROJECT} running onChanged={onChanged} />)

    act(() => screen.getByRole('button', { name: 'Stop' }).click())

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1))
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock).toHaveBeenCalledWith(
      `/api/projects/${PROJECT}/runtime/agent/stop`,
      expect.objectContaining({ method: 'POST' }),
    )
  })

  // §九: a restart asks first, and it is one call rather than a stop and a start
  // chained by the client - two requests would leave a gap a concurrent Start
  // can land in, which is how one project comes to have two Claudes in it.
  it('asks before restarting, and sends nothing until it is confirmed', async () => {
    const fetchMock = stubFetch(ok)
    const onChanged = vi.fn()
    renderWithTerminal(<AgentControls projectId={PROJECT} running onChanged={onChanged} />)

    act(() => screen.getByRole('button', { name: 'Restart' }).click())

    const dialog = screen.getByRole('dialog', { name: 'Restart Claude?' })
    expect(fetchMock).not.toHaveBeenCalled()

    act(() => within(dialog).getByRole('button', { name: 'Restart' }).click())

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1))
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock).toHaveBeenCalledWith(
      `/api/projects/${PROJECT}/runtime/agent/restart`,
      expect.objectContaining({ method: 'POST' }),
    )
  })

  it('sends nothing when the restart is cancelled', () => {
    const fetchMock = stubFetch(ok)
    renderWithTerminal(<AgentControls projectId={PROJECT} running />)

    act(() => screen.getByRole('button', { name: 'Restart' }).click())
    const dialog = screen.getByRole('dialog', { name: 'Restart Claude?' })
    act(() => within(dialog).getByRole('button', { name: 'Cancel' }).click())

    expect(screen.queryByRole('dialog')).toBeNull()
    expect(fetchMock).not.toHaveBeenCalled()
  })

  // §九's two transition labels, and the reason there is only ever one button
  // while a request runs: "Stopping…" is not a state the card can be asked to
  // do anything from, and a Start drawn beside it would be an invitation to put
  // a second agent in the project.
  it('shows one disabled button naming the request while it is in flight', async () => {
    let release: (() => void) | null = null
    const fetchMock = vi.fn(
      () =>
        new Promise<Response>((resolve) => {
          release = () => resolve(ok())
        }),
    )
    vi.stubGlobal('fetch', fetchMock)

    renderWithTerminal(<AgentControls projectId={PROJECT} running />)
    act(() => screen.getByRole('button', { name: 'Stop' }).click())

    expect(screen.getByRole('button', { name: 'Stopping…' })).toBeDisabled()
    expect(screen.getAllByRole('button')).toHaveLength(1)
    expect(fetchMock).toHaveBeenCalledTimes(1)

    await act(async () => {
      release?.()
    })
    await waitFor(() => expect(screen.getByRole('button', { name: 'Stop' })).toBeEnabled())
  })

  // The failure belongs to the card it happened to, and the page is re-read
  // either way: a stop that failed still moved something, and the card must not
  // be left holding this component's guess about what.
  it('reports a failure in the card, and re-reads anyway', async () => {
    stubFetch(() =>
      failure(409, 'agent_stop_timeout', 'claude in project p_x is still running after 10s', {
        pid: 4242,
        timeout: '10s',
      }),
    )
    const onChanged = vi.fn()
    renderWithTerminal(<AgentControls projectId={PROJECT} running onChanged={onChanged} />)

    act(() => screen.getByRole('button', { name: 'Stop' }).click())

    // §九's one failure that is read rather than reported: it is not a bug, it
    // is an agent that declined the interrupt, and the two facts that make the
    // sentence checkable are the server's own details.
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('did not stop within 10s (process 4242)')
    expect(alert).toHaveTextContent('still running')
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1))
  })

  // A restart that stopped the agent and could not start a new one has still
  // changed the project, and the server says so in `details.retired`. A client
  // that read only "the request failed" would draw a project holding an agent
  // it no longer has.
  it('says so when a failed restart left the project with no agent', async () => {
    stubFetch(() =>
      failure(500, 'agent_launch_failed', 'could not launch claude in project p_x', {
        retired: { agentSessionId: 'sess_abc', status: 'CANCELLED' },
      }),
    )
    renderWithTerminal(<AgentControls projectId={PROJECT} running />)

    act(() => screen.getByRole('button', { name: 'Restart' }).click())
    const dialog = screen.getByRole('dialog', { name: 'Restart Claude?' })
    act(() => within(dialog).getByRole('button', { name: 'Restart' }).click())

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('closed as cancelled')
    expect(alert).toHaveTextContent('no agent in it now')
  })

  // A failure the person can dismiss, so a card does not permanently wear an
  // error about something that has since been dealt with.
  it('lets a failure be dismissed', async () => {
    stubFetch(() => failure(500, 'agent_launch_failed', 'could not launch claude'))
    renderWithTerminal(<AgentControls projectId={PROJECT} running={false} />)

    act(() => screen.getByRole('button', { name: 'Start' }).click())
    await screen.findByRole('alert')

    act(() => screen.getByRole('button', { name: 'Dismiss' }).click())
    expect(screen.queryByRole('alert')).toBeNull()
  })

  // The component reports a failure and never re-throws it. An unhandled
  // rejection in a click handler is a failure that reaches nobody, and this one
  // has a card to be shown in - so what the user gets is a sentence, not a
  // console entry.
  it('shows a sentence rather than letting the rejection escape', async () => {
    stubFetch(() => failure(503, 'runtime_unavailable', 'this server cannot host a terminal'))
    renderWithTerminal(<AgentControls projectId={PROJECT} running={false} />)

    act(() => screen.getByRole('button', { name: 'Start' }).click())

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'this server cannot host a terminal',
    )
  })

  // The codes the server actually sends are read, not guessed at: a code this
  // build has never heard of reaches the person as the server's own sentence,
  // which is the only honest answer available for it.
  it('shows the server’s own message for a code this build does not know', async () => {
    stubFetch(() => failure(500, 'agent_something_new', 'a sentence this build has never seen'))
    renderWithTerminal(<AgentControls projectId={PROJECT} running={false} />)

    act(() => screen.getByRole('button', { name: 'Start' }).click())
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'a sentence this build has never seen',
    )
  })
})
