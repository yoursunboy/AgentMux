import { screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { makeProjectCard, makeQuietProjectCard } from '../test/controller'
import { renderWithTerminal } from '../test/dashboard'
import { ProjectPanel } from './ProjectPanel'
import { ACTIONS_PATH } from './route'
// xterm is a third-party boundary with its own renderer, and running the real
// one in jsdom would mean stubbing canvas and matchMedia so it can draw into a
// document nobody looks at. The console's cards contain terminals now, so this
// file states the same three doubles `components/TerminalView.test.tsx` does.
vi.mock('@xterm/xterm', async () => {
  const double = await import('../test/xterm')
  return { Terminal: double.FakeTerminal }
})
vi.mock('@xterm/addon-fit', async () => {
  const double = await import('../test/xterm')
  return { FitAddon: double.FakeFitAddon }
})
vi.mock('@xterm/addon-unicode11', async () => {
  const double = await import('../test/xterm')
  return { Unicode11Addon: double.FakeUnicode11Addon }
})

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
    renderWithTerminal(<ProjectPanel card={makeProjectCard({ name: 'checkout-service' })} />)
    expect(screen.getByRole('heading', { level: 3 })).toHaveTextContent('checkout-service')
  })

  it('shows the runtime, agent, attention and action count', () => {
    const card = makeProjectCard({
      runtime: { status: 'running' },
      agent: { available: true, sessionId: 'sess_x', status: 'WAITING_PERMISSION' },
      attention: { available: true, level: 'ACTION_REQUIRED', reason: 'permission requested' },
      actions: { available: true, pending: 2 },
    })
    const { container } = renderWithTerminal(<ProjectPanel card={card} />)

    expect(row(container, 'Runtime')).toHaveTextContent('running')
    expect(row(container, 'Agent')).toHaveTextContent('waiting for permission')
    expect(row(container, 'Attention')).toHaveTextContent('needs you')
    expect(row(container, 'Actions')).toHaveTextContent('2')
  })

  // §16: a field the server adds later must not be rendered, and the fields that
  // must never be shown have no path in. This asserts the shape rather than
  // trusting that nobody spread an object into the markup.
  it('renders the four fields it knows and nothing else', () => {
    const { container } = renderWithTerminal(<ProjectPanel card={makeProjectCard()} />)

    const terms = Array.from(container.querySelectorAll('.project-card__term')).map(
      (node) => node.textContent,
    )
    expect(terms).toEqual(['Runtime', 'Agent', 'Attention', 'Actions'])
  })

  // §8's rule, as a test: a status becomes a tone and the tone becomes a class.
  // Nothing in this component decides a colour.
  it('passes the tone through to the badge rather than choosing a colour', () => {
    const { container } = renderWithTerminal(
      <ProjectPanel
        card={makeProjectCard({
          attention: { available: true, level: 'ACTION_REQUIRED', reason: 'permission requested' },
        })}
      />,
    )
    expect(container.querySelector('.status-badge--attention')).toBeInTheDocument()
  })

  it('shows the reason beside the level', () => {
    renderWithTerminal(
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
    const none = renderWithTerminal(<ProjectPanel card={makeQuietProjectCard()} />)
    expect(row(none.container, 'Agent')).toHaveTextContent('none')
    expect(row(none.container, 'Attention')).toHaveTextContent('none')
    none.unmount()

    const degraded = renderWithTerminal(
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

  // §8 of the phase brief: a count that is a fact becomes a way in. Zero stays a
  // number, because there is nothing to go and look at.
  it('shows the pending count, and turns it into a link only when there is one', () => {
    const idle = renderWithTerminal(
      <ProjectPanel card={makeProjectCard({ actions: { available: true, pending: 0 } })} />,
    )
    expect(row(idle.container, 'Actions')).toHaveTextContent('0')
    expect(idle.container.querySelector('.project-card__action-link')).not.toBeInTheDocument()
    idle.unmount()

    const busy = renderWithTerminal(
      <ProjectPanel card={makeProjectCard({ actions: { available: true, pending: 3 } })} />,
    )
    expect(row(busy.container, 'Actions')).toHaveTextContent('3 actions pending')

    // It is an anchor rather than a button with an onClick, so the destination
    // is visible before it is clicked - and the destination is the queue, not
    // one action: a card carries a count, not an id.
    const link = busy.container.querySelector('.project-card__action-link')
    expect(link).toBeInTheDocument()
    expect(link).toHaveAttribute('href', ACTIONS_PATH)
  })

  // The sentence is shared with the queue page rather than written out twice, so
  // the card and the page cannot come to disagree about the same number.
  it('says the count in the singular when there is one', () => {
    const one = renderWithTerminal(
      <ProjectPanel card={makeProjectCard({ actions: { available: true, pending: 1 } })} />,
    )
    expect(row(one.container, 'Actions')).toHaveTextContent('1 action pending')
  })
})
