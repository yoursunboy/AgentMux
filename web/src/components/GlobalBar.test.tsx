import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { GlobalBar, globalState } from './GlobalBar'
import { makeServerInfo } from '../test/fixtures'
import { makeStatus } from '../test/terminal'

/** A connection with nothing to say, which is the ordinary case. */
const open = makeStatus({ state: 'open' })

describe('GlobalBar', () => {
  it('reports the connection state', () => {
    render(
      <GlobalBar
        info={makeServerInfo()}
        loading={false}
        error={null}
        connection={open}
        onRetry={vi.fn()}
      />,
    )
    expect(screen.getByRole('status')).toHaveTextContent('Online')
  })

  it('says connecting while the first read is in flight', () => {
    render(
      <GlobalBar info={null} loading error={null} connection={open} onRetry={vi.fn()} />,
    )
    expect(screen.getByRole('status')).toHaveTextContent('Connecting')
  })

  it('says offline and offers a retry when the server cannot be reached', () => {
    const onRetry = vi.fn()
    render(
      <GlobalBar
        info={null}
        loading={false}
        error="unreachable"
        connection={open}
        onRetry={onRetry}
      />,
    )

    expect(screen.getByRole('status')).toHaveTextContent('Offline')
    screen.getByRole('button', { name: 'Retry' }).click()
    expect(onRetry).toHaveBeenCalledOnce()
  })

  it('names the host and the runtime', () => {
    render(
      <GlobalBar
        info={makeServerInfo()}
        loading={false}
        error={null}
        connection={open}
        onRetry={vi.fn()}
      />,
    )
    expect(screen.getByText('Windows / WSL (Ubuntu-24.04)')).toBeInTheDocument()
    expect(screen.getByText('Wsl')).toBeInTheDocument()
  })

  it('stays one line: no page controls and no second bar', () => {
    // The pager is a strip of its own below this, and only when there is more
    // than one page. The bar's height is fixed by the stylesheet.
    render(
      <GlobalBar
        info={makeServerInfo()}
        loading={false}
        error={null}
        connection={open}
        onRetry={vi.fn()}
      />,
    )
    expect(screen.queryByRole('group', { name: 'Workspace pages' })).not.toBeInTheDocument()
  })
})

describe('GlobalBar provider switch', () => {
  it('is disabled and says it is coming later, rather than pretending to work', () => {
    render(
      <GlobalBar
        info={makeServerInfo()}
        loading={false}
        error={null}
        connection={open}
        onRetry={vi.fn()}
      />,
    )

    const control = screen.getByLabelText('Provider switch')
    expect(control).toBeDisabled()
    expect(control).toHaveTextContent('Coming later')
    expect(screen.getByText('Claude')).toBeInTheDocument()
  })

  it('stays disabled even when the server reports the provider as integrated', () => {
    // The UI must not enable a control on the strength of a flag it cannot yet
    // act on; there is no provider adapter.
    const info = makeServerInfo({ provider: { tool: 'claude', integrated: true, status: 'ok' } })
    render(
      <GlobalBar
        info={info}
        loading={false}
        error={null}
        connection={open}
        onRetry={vi.fn()}
      />,
    )
    expect(screen.getByLabelText('Provider switch')).toBeDisabled()
  })
})

/**
 * The bar's first word is a claim about the connection this page is using, and
 * the connection this page uses for its terminals is the WebSocket. These are
 * the cases where the two disagree.
 */
describe('globalState', () => {
  const server = { error: false, loading: false }

  it('is online when the server answered and the terminals are connected', () => {
    expect(globalState(server, 'open', '')).toEqual({ state: 'online', label: 'Online' })
  })

  it('is online when there is no terminal open at all', () => {
    // A page with nothing subscribed holds no socket, and that is not a fault.
    expect(globalState(server, 'idle', '')).toEqual({ state: 'online', label: 'Online' })
  })

  it('says reconnecting when the socket is coming back', () => {
    // The server is up and its REST calls work; the terminals are not connected,
    // and saying "Online" over a grid of frozen panels would be the wrong news.
    expect(globalState(server, 'reconnecting', '')).toEqual({
      state: 'connecting',
      label: 'Reconnecting',
    })
  })

  it('reports the terminal’s own reason when it gave up', () => {
    expect(globalState(server, 'failed', 'This page speaks protocol 2.')).toEqual({
      state: 'offline',
      label: 'This page speaks protocol 2.',
    })
  })

  it('says the terminal is offline rather than showing an empty label', () => {
    expect(globalState(server, 'failed', '')).toEqual({
      state: 'offline',
      label: 'Terminal offline',
    })
  })

  it('is offline when the server cannot be reached, whatever the socket says', () => {
    // The socket's reconnect loop is a consequence of this, not separate news.
    expect(globalState({ error: true, loading: false }, 'reconnecting', '')).toEqual({
      state: 'offline',
      label: 'Offline',
    })
  })

  it('is connecting while the first read is in flight', () => {
    expect(globalState({ error: false, loading: true }, 'idle', '')).toEqual({
      state: 'connecting',
      label: 'Connecting',
    })
  })
})
