import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { StatusBadge } from './StatusBadge'
import { attentionStyle } from './status'

describe('StatusBadge', () => {
  it('shows the label and carries the explanation', () => {
    const status = attentionStyle('ACTION_REQUIRED')
    const { container } = render(<StatusBadge status={status} />)

    expect(screen.getByText(status.label)).toBeInTheDocument()
    expect(container.querySelector('.status-badge')).toHaveAttribute('title', status.detail)
  })

  // The badge is the only place a tone becomes a class name. Everything that
  // shows a status goes through it, so this is the one rule that has to hold.
  it('turns a tone into a class and nothing else', () => {
    const { container } = render(<StatusBadge status={attentionStyle('WARNING')} />)
    const badge = container.querySelector('.status-badge')

    expect(badge).toHaveClass('status-badge--warning')
    expect(badge?.className).not.toMatch(/green|amber|red/i)
  })

  it('marks the dot as decoration, so it is read once', () => {
    const { container } = render(<StatusBadge status={attentionStyle('NONE')} />)
    expect(container.querySelector('.status-badge__dot')).toHaveAttribute('aria-hidden', 'true')
  })
})
