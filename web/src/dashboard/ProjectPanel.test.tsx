import { act, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { makeCardSettings, makeProjectCard, makeQuietProjectCard } from '../test/controller'
import { renderWithTerminal } from '../test/dashboard'
import { CLIENT_ID, makeControlView } from '../test/terminal'
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

/** A roster naming this client, which is the one that carries the lease. */
const OURS = makeControlView({ controller: { clientId: CLIENT_ID, device: 'Chrome on Windows' } })

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

  // The card owns the session, and these are the two facts that ownership is
  // for. The viewer used to create it, which was fine while the console could
  // only watch - but the mode switch in the Runtime row drives the same
  // terminal, and a second `useTerminalSession` would be a second subscription
  // to one project and a second claim on one lease.
  it('watches the project once, however many controls the card has', () => {
    const { terminal } = renderWithTerminal(<ProjectPanel card={makeProjectCard()} />)

    expect(terminal.subscribes).toHaveLength(1)
  })

  // §7: the console types and never reshapes. `TerminalViewer` passes
  // `mayResize={false}` as well, but by then it is too late to matter - the
  // subscribe is the frame that carries a size, and the server applies it when
  // the client holds the lease. So the assertion belongs where the session is
  // made, not where it is handed on.
  it('offers no size when it subscribes, so a card cannot reshape a shared pty', () => {
    const { terminal } = renderWithTerminal(<ProjectPanel card={makeProjectCard()} />)

    expect(terminal.subscribes[0]?.size).toBeNull()
  })

  // The Runtime row, which is where a person looks to find out what a project is
  // doing - and so where the button that changes what the agent will ask them
  // belongs. It is drawn only where there is a terminal to send it to: a stopped
  // card offering a mode switch would be offering to type into nothing.
  it('offers the mode switch beside a running runtime, and not beside a stopped one', () => {
    const live = renderWithTerminal(
      <ProjectPanel card={makeProjectCard({ runtime: { status: 'running' } })} />,
    )
    expect(within(row(live.container, 'Runtime')).getByRole('button', { name: 'Mode' })).toBeVisible()
    live.unmount()

    const stopped = renderWithTerminal(
      <ProjectPanel card={makeProjectCard({ runtime: { status: 'stopped' } })} />,
    )
    expect(within(row(stopped.container, 'Runtime')).queryByRole('button', { name: 'Mode' })).toBeNull()
  })

  // And the two halves reach each other: a mode switch pressed on the card puts
  // its bytes on the same subscription the screen below is drawing. This is what
  // the session moving up to the card bought, so it is the thing to assert -
  // two sessions would each have their own subscription and this would send
  // nothing to the terminal on screen.
  it('sends the mode keystroke down the terminal the card is drawing', () => {
    const { terminal } = renderWithTerminal(<ProjectPanel card={makeProjectCard()} />)

    act(() => terminal.deliverControl(OURS))
    act(() => screen.getByRole('button', { name: 'Mode' }).click())

    expect(terminal.current()?.input).toHaveBeenCalledWith('\x1b[Z')
  })

  // §7 of the permission-mode brief: the setting sits at the right of the title,
  // beside the name of the thing it configures. It is on every card, running or
  // not, because it is about the next launch rather than the current one - which
  // is the opposite of the mode switch above it and the reason the two are named
  // differently.
  it('offers the permission control on the title row, running or not', () => {
    const live = renderWithTerminal(<ProjectPanel card={makeProjectCard()} />)
    const header = live.container.querySelector('.project-card__header')
    expect(header).toContainElement(screen.getByRole('button', { name: 'Permission' }))
    expect(header).toContainElement(screen.getByRole('heading', { level: 3 }))
    live.unmount()

    const stopped = renderWithTerminal(
      <ProjectPanel card={makeProjectCard({ runtime: { status: 'stopped' } })} />,
    )
    expect(screen.getByRole('button', { name: 'Permission' })).toBeVisible()
    stopped.unmount()
  })

  // The card draws what the server said and never a default of its own: a menu
  // that marked `manual` on a project configured with `acceptEdits` would be
  // telling somebody the wrong thing about the next launch.
  it('marks the mode the server reported, not the default', () => {
    renderWithTerminal(
      <ProjectPanel card={makeProjectCard({ settings: makeCardSettings({ permissionMode: 'acceptEdits' }) })} />,
    )

    act(() => screen.getByRole('button', { name: 'Permission' }).click())

    const chosen = screen
      .getAllByRole('menuitemradio')
      .filter((item) => item.getAttribute('aria-checked') === 'true')
    expect(chosen).toHaveLength(1)
    expect(chosen[0]).toHaveTextContent('acceptEdits')
  })

  // And the write reaches the page. A card that saved a setting and told nobody
  // would leave the dashboard showing a mode the server no longer holds.
  //
  // The second half is §10's fifth case at the level the card can see it: saving
  // a mode types nothing into the terminal and subscribes to nothing new, so
  // the agent on screen is left exactly as it was.
  it('asks the page to re-read after the setting is saved, and touches no terminal doing it', async () => {
    const onChanged = vi.fn()
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ settings: { permissionMode: 'acceptEdits' } }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }),
        ),
      ),
    )

    const { terminal } = renderWithTerminal(
      <ProjectPanel card={makeProjectCard()} onChanged={onChanged} />,
    )
    act(() => screen.getByRole('button', { name: 'Permission' }).click())
    act(() => screen.getByRole('menuitemradio', { name: /^acceptEdits/ }).click())

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1))
    expect(terminal.current()?.input).not.toHaveBeenCalled()
    expect(terminal.subscribes).toHaveLength(1)
    vi.unstubAllGlobals()
  })

  // §九: the three lifecycle requests, drawn from what the agent is actually
  // doing. The pair is chosen by the projection and never by the runtime - a
  // project whose terminal is up with a shell in it has no agent to stop, and
  // one whose agent is working has one whatever the terminal row says.
  it('offers Stop and Restart to a working agent, and Start to one that is not', () => {
    const working = renderWithTerminal(
      <ProjectPanel card={makeProjectCard({ agent: { available: true, status: 'RUNNING' } })} />,
    )
    expect(within(row(working.container, 'Agent')).getByRole('button', { name: 'Stop' })).toBeVisible()
    expect(within(row(working.container, 'Agent')).getByRole('button', { name: 'Restart' })).toBeVisible()
    expect(within(row(working.container, 'Agent')).queryByRole('button', { name: 'Start' })).toBeNull()
    working.unmount()

    // Waiting for permission is a working agent: it is blocked, not finished,
    // and the thing it is blocked on may well be worth stopping.
    const waiting = renderWithTerminal(
      <ProjectPanel
        card={makeProjectCard({ agent: { available: true, status: 'WAITING_PERMISSION' } })}
      />,
    )
    expect(within(row(waiting.container, 'Agent')).getByRole('button', { name: 'Stop' })).toBeVisible()
    waiting.unmount()

    const stopped = renderWithTerminal(
      <ProjectPanel card={makeProjectCard({ agent: { available: true, status: 'STOPPED' } })} />,
    )
    expect(within(row(stopped.container, 'Agent')).getByRole('button', { name: 'Start' })).toBeVisible()
    expect(within(row(stopped.container, 'Agent')).queryByRole('button', { name: 'Stop' })).toBeNull()
  })

  // A project no agent has ever run in is the ordinary case for a fresh
  // installation, and Start is exactly what it should offer.
  it('offers Start on a project nothing has ever run in', () => {
    const { container } = renderWithTerminal(<ProjectPanel card={makeQuietProjectCard()} />)

    expect(within(row(container, 'Agent')).getByRole('button', { name: 'Start' })).toBeVisible()
  })

  // And when the server cannot read the projection at all, the card still
  // offers Start - which is the safe half of being wrong. The server adopts a
  // process that is already running rather than launching a second one, so a
  // Start that was not needed costs a request; a Stop that was not needed
  // would cost the terminal's own bytes.
  it('offers the safe half when the server cannot say what the agent is doing', () => {
    const { container } = renderWithTerminal(
      <ProjectPanel card={makeProjectCard({ agent: { available: false } })} />,
    )

    expect(within(row(container, 'Agent')).getByRole('button', { name: 'Start' })).toBeVisible()
    expect(within(row(container, 'Agent')).queryByRole('button', { name: 'Stop' })).toBeNull()
  })

  // §16's rule about offering to start a terminal on a machine that cannot host
  // one applies to an agent for the same reason: `agent/start` there answers
  // `runtime_unavailable`, and a button that could only do that is a button
  // offering nothing.
  it('draws no lifecycle controls where a terminal cannot run', () => {
    const { container } = renderWithTerminal(
      <ProjectPanel card={makeProjectCard()} runtimeAvailable={false} />,
    )

    expect(within(row(container, 'Agent')).queryByRole('button')).toBeNull()
    expect(row(container, 'Agent')).toHaveTextContent('running')
  })

  // The claim §九 makes in the negative: these buttons are requests to the
  // server, not keystrokes. A Stop that reached the agent by typing Ctrl+C at
  // the pty would put a byte into whatever the person at the terminal was
  // doing, and would report success for a shell that happened to exit.
  it('drives the lifecycle through the endpoint and never through the terminal', async () => {
    const fetchMock = vi.fn(async () => new Response('{}', { status: 200 }))
    vi.stubGlobal('fetch', fetchMock)

    const card = makeProjectCard({ agent: { available: true, status: 'RUNNING' } })
    const { container, terminal } = renderWithTerminal(<ProjectPanel card={card} />)
    act(() => within(row(container, 'Agent')).getByRole('button', { name: 'Stop' }).click())

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))
    expect(fetchMock).toHaveBeenCalledWith(
      `/api/projects/${card.id}/runtime/agent/stop`,
      expect.objectContaining({ method: 'POST' }),
    )
    expect(terminal.current()?.input).not.toHaveBeenCalled()
    expect(terminal.subscribes).toHaveLength(1)
    vi.unstubAllGlobals()
  })
})
