import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { makeControllerServer, makeModelBinding } from '../test/controller'
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
})
