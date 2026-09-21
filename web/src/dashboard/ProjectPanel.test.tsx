import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { makeProjectCard, makeQuietProjectCard } from '../test/controller'
import { ProjectPanel } from './ProjectPanel'

/** The row a label belongs to, so an assertion names the value and not a position. */
function row(container: HTMLElement, term: string): HTMLElement {
  const dt = Array.from(container.querySelectorAll('.project-card__term')).find(
    (node) => node.textContent === term,
  )
  const dd = dt?.nextElementSibling
  if (!(dd instanceof HTMLElement)) throw new Error(`no row for ${term}`)
  return dd
}

describe('ProjectPanel', () => {
  it('names the project', () => {
    render(<ProjectPanel card={makeProjectCard({ name: 'checkout-service' })} />)
    expect(screen.getByRole('heading', { level: 3 })).toHaveTextContent('checkout-service')
  })

  it('shows the runtime, agent, attention and action count', () => {
    const card = makeProjectCard({
      runtime: { status: 'running' },
      agent: { available: true, sessionId: 'sess_x', status: 'WAITING_PERMISSION' },
      attention: { available: true, level: 'ACTION_REQUIRED', reason: 'permission requested' },
      actions: { available: true, pending: 2 },
    })
    const { container } = render(<ProjectPanel card={card} />)

    expect(row(container, 'Runtime')).toHaveTextContent('running')
    expect(row(container, 'Agent')).toHaveTextContent('waiting for permission')
    expect(row(container, 'Attention')).toHaveTextContent('needs you')
    expect(row(container, 'Actions')).toHaveTextContent('2')
  })

  // §16: a field the server adds later must not be rendered, and the fields that
  // must never be shown have no path in. This asserts the shape rather than
  // trusting that nobody spread an object into the markup.
  it('renders the four fields it knows and nothing else', () => {
    const { container } = render(<ProjectPanel card={makeProjectCard()} />)

    const terms = Array.from(container.querySelectorAll('.project-card__term')).map(
      (node) => node.textContent,
    )
    expect(terms).toEqual(['Runtime', 'Agent', 'Attention', 'Actions'])
  })

  // §8's rule, as a test: a status becomes a tone and the tone becomes a class.
  // Nothing in this component decides a colour.
  it('passes the tone through to the badge rather than choosing a colour', () => {
    const { container } = render(
      <ProjectPanel
        card={makeProjectCard({
          attention: { available: true, level: 'ACTION_REQUIRED', reason: 'permission requested' },
        })}
      />,
    )
    expect(container.querySelector('.status-badge--attention')).toBeInTheDocument()
  })

  it('shows the reason beside the level', () => {
    render(
      <ProjectPanel
        card={makeProjectCard({
          attention: { available: true, level: 'WARNING', reason: 'agent failed' },
        })}
      />,
    )
    expect(screen.getByText('agent failed')).toBeInTheDocument()
  })

  // The three ways a section can be empty are different, and the card says which
  // one it is rather than collapsing them into one blank.
  it('distinguishes no agent from an agent it cannot read', () => {
    const none = render(<ProjectPanel card={makeQuietProjectCard()} />)
    expect(row(none.container, 'Agent')).toHaveTextContent('none')
    expect(row(none.container, 'Attention')).toHaveTextContent('none')
    none.unmount()

    const degraded = render(
      <ProjectPanel
        card={makeProjectCard({
          agent: { available: false },
          attention: { available: false, level: '' },
          actions: { available: false, pending: 0 },
        })}
      />,
    )
    expect(row(degraded.container, 'Agent')).toHaveTextContent('unavailable')
    expect(row(degraded.container, 'Attention')).toHaveTextContent('unavailable')
    expect(row(degraded.container, 'Actions')).toHaveTextContent('unavailable')
  })

  it('shows the pending count, and marks it only when there is one', () => {
    const idle = render(
      <ProjectPanel card={makeProjectCard({ actions: { available: true, pending: 0 } })} />,
    )
    expect(row(idle.container, 'Actions')).toHaveTextContent('0')
    expect(idle.container.querySelector('.project-card__count--pending')).not.toBeInTheDocument()
    idle.unmount()

    const busy = render(
      <ProjectPanel card={makeProjectCard({ actions: { available: true, pending: 3 } })} />,
    )
    // The number is the same number; only the tone class differs, which is what
    // keeps the emphasis in the stylesheet rather than in the component.
    expect(row(busy.container, 'Actions')).toHaveTextContent('3')
    expect(busy.container.querySelector('.project-card__count--pending')).toBeInTheDocument()
  })
})
