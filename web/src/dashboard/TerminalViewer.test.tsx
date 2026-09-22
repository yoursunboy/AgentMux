import { act, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { makeProjectCard } from '../test/controller'
import { renderWithTerminal } from '../test/dashboard'
import { makeStatus } from '../test/terminal'
import { FakeTerminal } from '../test/xterm'
import { ProjectPanel } from './ProjectPanel'
import { TerminalViewer } from './TerminalViewer'

// The console's cards contain terminals, so this file states the same three
// xterm doubles `components/TerminalView.test.tsx` does. Running the real one in
// jsdom would mean stubbing canvas and matchMedia so it can draw into a document
// nobody looks at - and the question here is what this component asks the
// terminal to do, not what xterm draws.
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

const PROJECT = 'p_0123456789abcdef0123'

describe('TerminalViewer', () => {
  beforeEach(() => {
    FakeTerminal.reset()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('says the runtime is stopped rather than showing a terminal', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running={false} />)

    expect(screen.getByText('Runtime stopped')).toBeInTheDocument()
    // Nothing is subscribed, because the server refuses a subscribe to a stopped
    // project - and a client that tried would spend a reconnect cycle being told.
    expect(terminal.subscribes).toHaveLength(0)
    // And no terminal was built. A console showing a hundred stopped projects
    // should not have a hundred of them in the document.
    expect(FakeTerminal.instances).toHaveLength(0)
  })

  it('subscribes to the project it is watching', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    expect(terminal.subscribes).toHaveLength(1)
    expect(terminal.subscribes[0]?.projectId).toBe(PROJECT)
  })

  it('shows what the server sends', async () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)
    const screen = FakeTerminal.last

    // The snapshot first, then output: that is the order the server sends them,
    // and the order matters because a snapshot written after output would
    // replace the screen it had just been given.
    act(() => terminal.deliverSnapshot(new TextEncoder().encode('ready> ')))
    act(() => terminal.deliverOutput(new TextEncoder().encode('hello')))

    await waitFor(() => expect(screen.text()).toContain('ready> '))
    expect(screen.text()).toContain('hello')
  })

  // §7 of the phase brief, asserted rather than assumed: a viewer's terminal has
  // no input handler bound to it at all, so there is no path from a keystroke to
  // a socket. This is stronger than "typing does nothing" - it is "typing has
  // nothing to reach".
  it('binds no input handler to the terminal', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    expect(FakeTerminal.last.dataListenerCount).toBe(0)
    expect(terminal.current()?.input).not.toHaveBeenCalled()
  })

  it('sends nothing when a key is pressed at it anyway', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    // The terminal refuses the keystroke, and even if something bound a handler
    // the hook refuses input without the lease. Both, not either.
    act(() => FakeTerminal.last.type('rm -rf /'))

    expect(terminal.current()?.input).not.toHaveBeenCalled()
    expect(terminal.subscribes).toHaveLength(1)
  })

  it('never asks for control', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    // A viewer is the protocol's default position: a client that never sends
    // control.request is a viewer for the life of the connection, and there is
    // no call in this component that could send one.
    expect(terminal.current()?.requestControl).not.toHaveBeenCalled()
    expect(terminal.current()?.releaseControl).not.toHaveBeenCalled()
  })

  // §11: a viewer draws the screen at the size the server chose. The server
  // refuses a viewer's resize, and this is the client agreeing with it rather
  // than asking and being refused.
  it('never asks the terminal to be resized', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    expect(terminal.current()?.resize).not.toHaveBeenCalled()
  })

  it('says it is connecting before the socket is open', () => {
    renderWithTerminal(<TerminalViewer projectID={PROJECT} running />, {
      status: makeStatus({ state: 'connecting' }),
    })

    expect(screen.getByRole('status')).toHaveTextContent('Connecting…')
  })

  it('says the terminal is disconnected when the connection drops', async () => {
    vi.useFakeTimers()
    renderWithTerminal(<TerminalViewer projectID={PROJECT} running />, {
      status: makeStatus({ state: 'reconnecting' }),
    })

    // The settle delay exists so a card does not flash a warning during the
    // milliseconds a first connection takes.
    await act(async () => {
      vi.advanceTimersByTime(500)
    })

    expect(screen.getByRole('status')).toHaveTextContent('Terminal disconnected')
  })

  it('says it cannot connect, and offers a retry', async () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />, {
      status: makeStatus({ state: 'failed', message: 'The server stopped answering.' }),
    })

    expect(await screen.findByText('Unable to connect terminal')).toBeInTheDocument()
    expect(screen.getByText('The server stopped answering.')).toBeInTheDocument()

    act(() => screen.getByRole('button', { name: 'Retry' }).click())
    expect(terminal.current()?.resync).toHaveBeenCalled()
  })

  it('says there is no terminal when the server ends the subscription', async () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    act(() => terminal.deliverEnded())

    expect(await screen.findByText('No active terminal')).toBeInTheDocument()
    // It does not offer a retry: there is nothing to retry until a runtime
    // starts, and a button that cannot help is worse than none.
    expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument()
  })
})

/** Two viewers, each watching its own project, on the page's one client. */
describe('TerminalViewer, several at once', () => {
  beforeEach(() => {
    FakeTerminal.reset()
  })

  it('subscribes each project once and draws each terminal separately', () => {
    const other = 'p_fedcba9876543210fedc'
    const { terminal } = renderWithTerminal(
      <>
        <TerminalViewer projectID={PROJECT} running />
        <TerminalViewer projectID={other} running />
      </>,
    )

    const watched = terminal.subscribes.map((entry) => entry.projectId)
    expect(watched).toEqual([PROJECT, other])
    // Two projects, two terminals, one page - and one client. The console opens
    // no socket of its own: subscriptions are multiplexed on the page's.
    expect(FakeTerminal.instances).toHaveLength(2)
  })

  it('gives each card its own terminal, and neither draws the other', () => {
    const { container } = renderWithTerminal(
      <>
        <ProjectPanel card={makeProjectCard({ id: 'p_a', name: 'alpha' })} />
        <ProjectPanel card={makeProjectCard({ id: 'p_b', name: 'bravo' })} />
      </>,
    )

    expect(container.querySelectorAll('.terminal-viewer')).toHaveLength(2)
    expect(FakeTerminal.instances).toHaveLength(2)
  })

  // A stopped project gets no terminal at all, which is what keeps a console of
  // a hundred stopped projects from holding a hundred terminals.
  it('leaves a stopped project without a terminal while its neighbour has one', () => {
    renderWithTerminal(
      <>
        <ProjectPanel card={makeProjectCard({ id: 'p_a', name: 'alpha' })} />
        <ProjectPanel
          card={makeProjectCard({ id: 'p_b', name: 'bravo', runtime: { status: 'stopped' } })}
        />
      </>,
    )

    expect(FakeTerminal.instances).toHaveLength(1)
    expect(screen.getByText('Runtime stopped')).toBeInTheDocument()
  })
})
