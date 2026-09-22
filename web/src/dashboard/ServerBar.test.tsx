import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { makeControllerServer, makeModelBinding, makeQueueSummary } from '../test/controller'
import { ServerBar } from './ServerBar'

describe('ServerBar', () => {
  it('reports the server as online', () => {
    render(<ServerBar server={makeControllerServer()} />)
    expect(screen.getByRole('status')).toHaveTextContent('Online')
  })

  it('says the runtime is available when it is', () => {
    render(<ServerBar server={makeControllerServer({ runtimeAvailable: true })} />)
    expect(screen.getByText('available')).toBeInTheDocument()
  })

  // The reason is the server's own, and it is the one a person can act on:
  // start the server inside WSL, or install tmux. It is a tooltip, so the bar
  // stays one bar.
  it('carries the server reason when the runtime is not available', () => {
    render(
      <ServerBar
        server={makeControllerServer({
          runtimeAvailable: false,
          runtimeUnavailableReason: 'The terminal runtime needs the AgentMux server to run inside WSL.',
        })}
      />,
    )
    expect(screen.getByText('unavailable')).toHaveAttribute(
      'title',
      'The terminal runtime needs the AgentMux server to run inside WSL.',
    )
  })

  // §6: a model nothing reports is Unknown, not a guess. A name that came from
  // nowhere is a name somebody would act on.
  it('says Unknown when no model binding is known', () => {
    render(<ServerBar server={makeControllerServer()} model={null} />)
    expect(screen.getByText('Unknown')).toBeInTheDocument()
  })

  it('shows the binding when there is one', () => {
    render(
      <ServerBar server={makeControllerServer()} model={makeModelBinding({ model: 'deepseek flash' })} />,
    )
    expect(screen.getByText(/deepseek flash/)).toBeInTheDocument()
    expect(screen.queryByText('Unknown')).not.toBeInTheDocument()
  })

  // §5: the control is shown and is not connected to anything. A button that
  // looked live would be a promise the build cannot keep.
  it('shows a switch that does nothing, and says why', () => {
    render(<ServerBar server={makeControllerServer()} />)
    const button = screen.getByRole('button', { name: 'Switch' })

    expect(button).toBeDisabled()
    expect(button).toHaveAttribute('title', expect.stringContaining('Phase 8'))
  })

  it('offers a way back to the workspace', () => {
    render(<ServerBar server={makeControllerServer()} />)
    expect(screen.getByRole('link', { name: 'Workspace' })).toHaveAttribute('href', '/')
  })

  // §8 of the phase brief: the bar carries what is waiting, and a way to go and
  // look at it. Both numbers come from the dashboard response, so the bar and
  // the cards below it are one answer that can be wrong in one way.
  it('says how much is waiting, and links to it', () => {
    const { container } = render(
      <ServerBar server={makeControllerServer()} queue={makeQueueSummary({ needsYou: 2, notices: 5 })} />,
    )

    const queue = screen.getByRole('link', { name: /Needs you/ })
    expect(queue).toHaveAttribute('href', '/actions')
    expect(queue).toHaveTextContent('Needs you: 2')
    expect(queue).toHaveTextContent('Notices: 5')
    expect(container.querySelector('.server-bar__queue-count--needs-you')).toHaveTextContent('2')
  })

  it('does not shout about a count of zero', () => {
    const { container } = render(<ServerBar server={makeControllerServer()} queue={makeQueueSummary()} />)

    expect(container.querySelector('.server-bar__queue-count--needs-you')).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Needs you/ })).toHaveTextContent('Needs you: 0')
  })

  // A bar that could not read the queue says nothing rather than saying zero,
  // because a zero is a claim and this bar is not in a position to make it.
  it('says nothing about the queue when it has none', () => {
    const { container } = render(<ServerBar server={makeControllerServer()} />)

    expect(container.querySelector('.server-bar__queue')).not.toBeInTheDocument()
  })

  // The hazard this element was designed around, pinned rather than remembered:
  // `web/e2e/suites/dashboard.mjs` clicks `.server-bar__link`, and Playwright's
  // strict mode throws when one selector matches two elements. A second link
  // sharing that class would break a suite about something else.
  it('keeps the workspace link the only server-bar__link on the bar', () => {
    const { container } = render(
      <ServerBar server={makeControllerServer()} queue={makeQueueSummary({ needsYou: 1 })} />,
    )

    expect(container.querySelectorAll('.server-bar__link')).toHaveLength(1)
  })
})
