import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { GlobalBar } from './GlobalBar'
import { makeServerInfo } from '../test/fixtures'

describe('GlobalBar', () => {
  it('reports the connection state', () => {
    render(<GlobalBar info={makeServerInfo()} loading={false} error={null} onRetry={vi.fn()} />)
    expect(screen.getByRole('status')).toHaveTextContent('Online')
  })

  it('says connecting while the first read is in flight', () => {
    render(<GlobalBar info={null} loading error={null} onRetry={vi.fn()} />)
    expect(screen.getByRole('status')).toHaveTextContent('Connecting')
  })

  it('says offline and offers a retry when the server cannot be reached', () => {
    const onRetry = vi.fn()
    render(<GlobalBar info={null} loading={false} error="unreachable" onRetry={onRetry} />)

    expect(screen.getByRole('status')).toHaveTextContent('Offline')
    screen.getByRole('button', { name: 'Retry' }).click()
    expect(onRetry).toHaveBeenCalledOnce()
  })

  it('names the host and the runtime', () => {
    render(<GlobalBar info={makeServerInfo()} loading={false} error={null} onRetry={vi.fn()} />)
    expect(screen.getByText('Windows / WSL (Ubuntu-24.04)')).toBeInTheDocument()
    expect(screen.getByText('Wsl')).toBeInTheDocument()
  })
})

describe('GlobalBar provider switch', () => {
  it('is disabled and says it is coming later, rather than pretending to work', () => {
    render(<GlobalBar info={makeServerInfo()} loading={false} error={null} onRetry={vi.fn()} />)

    const control = screen.getByLabelText('Provider switch')
    expect(control).toBeDisabled()
    expect(control).toHaveTextContent('Coming later')
    expect(screen.getByText('Claude')).toBeInTheDocument()
  })

  it('stays disabled even when the server reports the provider as integrated', () => {
    // The UI must not enable a control on the strength of a flag it cannot yet
    // act on; Phase 1 has no provider adapter.
    const info = makeServerInfo({ provider: { tool: 'claude', integrated: true, status: 'ok' } })
    render(<GlobalBar info={info} loading={false} error={null} onRetry={vi.fn()} />)
    expect(screen.getByLabelText('Provider switch')).toBeDisabled()
  })
})
