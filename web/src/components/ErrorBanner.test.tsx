import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { ErrorBanner } from './ErrorBanner'

describe('ErrorBanner', () => {
  it('announces itself, so a failure is not only visible', () => {
    render(<ErrorBanner message="Something failed." />)
    expect(screen.getByRole('alert')).toHaveTextContent('Something failed.')
  })

  it('offers a retry only when one is possible', async () => {
    const onRetry = vi.fn()
    const { unmount } = render(<ErrorBanner message="Unreachable." onRetry={onRetry} />)

    await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(onRetry).toHaveBeenCalledOnce()

    unmount()
    render(<ErrorBanner message="Unreachable." />)
    expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument()
  })

  it('offers a dismiss only when there is something to dismiss', async () => {
    const onDismiss = vi.fn()
    const { unmount } = render(<ErrorBanner message="Bad path." onDismiss={onDismiss} />)

    await userEvent.click(screen.getByRole('button', { name: 'Dismiss' }))
    expect(onDismiss).toHaveBeenCalledOnce()

    unmount()
    render(<ErrorBanner message="Bad path." />)
    expect(screen.queryByRole('button', { name: 'Dismiss' })).not.toBeInTheDocument()
  })

  it('keeps the technical detail behind a disclosure, where it helps without shouting', async () => {
    const { container } = render(
      <ErrorBanner message="Could not reach the server." detail="network_unreachable (HTTP 0)" />,
    )

    const details = container.querySelector('details')
    expect(details).not.toBeNull()
    expect(details).not.toHaveAttribute('open')
    expect(screen.getByText('network_unreachable (HTTP 0)')).toBeInTheDocument()
  })

  it('renders no disclosure at all when there is no detail', () => {
    const { container } = render(<ErrorBanner message="Nope." />)
    expect(container.querySelector('details')).toBeNull()
  })
})
