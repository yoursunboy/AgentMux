import { act, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { ControlReason } from '../terminal/protocol'
import { makeProjectCard } from '../test/controller'
import { renderWithTerminal } from '../test/dashboard'
import { CLIENT_ID, makeControlView, makeStatus, makeTerminalError } from '../test/terminal'
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

  // The console can ask for the lease, so the claim is no longer "there is no
  // call that could send one" - it is "it does not send one unasked". The two
  // are different promises and only the second one is still true, so this says
  // the second one.
  //
  // It matters more than it looks. The console opens a card for every running
  // project on the page, and a card that requested control as it mounted would
  // take the keyboard away from whoever is working - one viewer, silently,
  // per project. Opening a terminal is not claiming it.
  it('asks for control only when it is asked to, never by opening', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    expect(terminal.current()?.requestControl).not.toHaveBeenCalled()
    expect(terminal.current()?.releaseControl).not.toHaveBeenCalled()
  })

  // And not merely before the roster arrives, either: a card that can see the
  // terminal is free still waits to be told.
  it('does not take a free terminal by being open on it', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    act(() => terminal.deliverControl(makeControlView()))

    expect(screen.getByTestId('terminal-control')).toHaveTextContent('nobody is in control')
    expect(terminal.current()?.requestControl).not.toHaveBeenCalled()
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

  // The server refuses a message by answering with an error and closing
  // nothing, deliberately - dropping a viewer's socket over one stray keystroke
  // would turn it into an outage for everybody else watching. A card that
  // answered that by throwing its terminal away would be saying "unable to
  // connect" about a connection that is up.
  //
  // It is not a hypothetical: a keystroke sent in the window between the lease
  // moving and the roster saying so is refused exactly this way. The workspace
  // has always shown the sentence and kept drawing
  // (`components/ProjectPanel.test.tsx` pins the same case), and this is the
  // console agreeing with it rather than inventing a second answer.
  it('says a refusal without throwing away a terminal that is still live', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)
    act(() => terminal.deliverSnapshot(new TextEncoder().encode('ready> ')))

    act(() =>
      terminal.deliverError(
        makeTerminalError({
          code: 'not_controller',
          message: 'you are not controlling this project',
          about: 'input',
        }),
      ),
    )

    expect(screen.getByText('you are not controlling this project')).toBeInTheDocument()
    expect(screen.queryByText('Unable to connect terminal')).not.toBeInTheDocument()
    // Still one terminal, still holding the screen it was given.
    expect(FakeTerminal.instances).toHaveLength(1)
    expect(FakeTerminal.last.text()).toContain('ready> ')
  })

  // The other side of that boundary, so the split is the one that is tested and
  // not merely the one that is written: the same error, with no connection under
  // it, is the Notice - because there the terminal is not being drawn and a
  // retry is the thing that helps.
  it('still says it cannot connect when the error arrives with no connection', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />, {
      status: makeStatus({ state: 'reconnecting' }),
    })

    act(() =>
      terminal.deliverError(
        makeTerminalError({
          code: 'stream_unstable',
          message: 'The terminal outran the connection.',
        }),
      ),
    )

    expect(screen.getByText('Unable to connect terminal')).toBeInTheDocument()
    expect(screen.getByText('The terminal outran the connection.')).toBeInTheDocument()
  })
})

/**
 * The console as a controller, which is the half of it that types.
 *
 * The viewer half is above; what is here is the client that has the lease, the
 * client that has asked for it, and the client that was refused. Every one of
 * them is reached by handing the session a roster, because that is exactly how
 * the real client learns it - `deliverControl` is the parse already done.
 */
describe('TerminalViewer, holding the lease', () => {
  beforeEach(() => {
    FakeTerminal.reset()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  /** A roster naming this client, which is what the server sends a controller. */
  const OURS = makeControlView({ controller: { clientId: CLIENT_ID, device: 'Chrome on Windows' } })
  /** A roster naming somebody else, which is what a viewer is sent. */
  const THEIRS = makeControlView({
    controller: { clientId: 'c_ffffffffffffffff', device: 'Safari on iPad' },
  })

  it('says whose turn it is, and offers the one useful action for it', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    act(() => terminal.deliverControl(THEIRS))
    expect(screen.getByTestId('terminal-control')).toHaveTextContent(
      'Viewer · Safari on iPad has control',
    )
    expect(screen.getByRole('button', { name: 'Request control' })).toBeEnabled()

    act(() => terminal.deliverControl(OURS))
    expect(screen.getByTestId('terminal-control')).toHaveTextContent('You control')
    expect(screen.getByRole('button', { name: 'Release control' })).toBeEnabled()
  })

  it('sends one request when the button is pressed, and nothing before it', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    act(() => terminal.deliverControl(THEIRS))
    expect(terminal.current()?.requestControl).not.toHaveBeenCalled()

    act(() => screen.getByRole('button', { name: 'Request control' }).click())
    expect(terminal.current()?.requestControl).toHaveBeenCalledTimes(1)
  })

  // The lease is what makes a keystroke reach a socket, and this is the whole of
  // it: xterm has no input handler bound while the client is a viewer and has
  // exactly one the moment it becomes the controller. Asserting the count rather
  // than "typing does nothing" is the stronger claim - a viewer has nothing for a
  // keystroke to reach, not a handler that declines it.
  it('binds an input handler for the controller and none for a viewer', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    expect(FakeTerminal.last.dataListenerCount).toBe(0)

    act(() => terminal.deliverControl(OURS))
    expect(FakeTerminal.last.dataListenerCount).toBe(1)

    act(() => FakeTerminal.last.type('ls\r'))
    expect(terminal.current()?.input).toHaveBeenCalledWith('ls\r')

    // And giving the lease up takes the handler away again, rather than leaving a
    // terminal that would still send. `input()` refuses without the lease as
    // well, so this is the second of two independent stops rather than the only
    // one - but it is the one that exists in the DOM.
    act(() => terminal.deliverControl(THEIRS))
    expect(FakeTerminal.last.dataListenerCount).toBe(0)
  })

  it('draws the touch keys with the lease and not without it', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    act(() => terminal.deliverControl(THEIRS))
    expect(screen.queryByRole('button', { name: 'Escape' })).toBeNull()

    act(() => terminal.deliverControl(OURS))
    expect(screen.getByRole('button', { name: 'Escape' })).not.toBeNull()
  })

  // The console types and never reshapes. `ProjectTerminal` passes the same
  // expression for `interactive` and for `mayResize`, so the risk here is a
  // later edit making the card match the panel by habit.
  //
  // The assertion is not "no resize is ever sent" as an accident of which branch
  // fires - it is that this client never names a geometry at all, which is what
  // the session is told to withhold and is why a size reaches the wire in only
  // one of the two places it otherwise would.
  it('takes the lease without ever naming a shape for the terminal', async () => {
    vi.useFakeTimers()
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    // The server sends the screen and its geometry together on subscribe, and
    // that geometry is the pty's. This card's box had nothing to do with it.
    act(() => terminal.deliverSnapshot(new TextEncoder().encode('ready> '), 120, 30))
    await act(async () => {
      terminal.deliverControl(OURS)
      vi.advanceTimersByTime(1000)
    })

    // It has the keyboard...
    expect(FakeTerminal.last.dataListenerCount).toBe(1)
    // ...and it has no shape. Not "the right shape": none. A size stated here
    // would have to be right about a terminal this browser is not measuring, and
    // being right is not the same as not saying it - the pty is shared with
    // whoever is working in the workspace, and a card does not get a vote.
    expect(terminal.current()?.resize).not.toHaveBeenCalled()
    // This is the other way a size reaches the pty: the server applies the size
    // a subscribe carries when that client holds the lease, so withholding it is
    // as much a part of the rule as withholding the resize.
    expect(terminal.subscribes[0]?.size).toBeNull()
    // What it draws at is still the pty's shape, which is the size it was sent.
    expect(FakeTerminal.last.resizes.map((size) => `${size.cols}x${size.rows}`)).toEqual(['120x30'])
  })

  it('says why a request was refused, since the roster will not', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    // A refusal leaves the project exactly as the roster already described it,
    // so this sentence is the only thing that can explain the button appearing
    // to have done nothing.
    act(() =>
      terminal.deliverControl(THEIRS, {
        type: 'control.denied',
        projectId: PROJECT,
        control: THEIRS,
        reason: ControlReason.controllerExists,
      }),
    )

    expect(screen.getByRole('status')).toHaveTextContent('Somebody else is using this terminal.')
  })

  it('offers the queue to the client that can answer it, and to nobody else', () => {
    const asking = { clientId: 'c_1111111111111111', device: 'Safari on iPad' }
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    // A viewer is shown the same pending list in a roster it has no way to act
    // on, so it is not offered the buttons.
    act(() => terminal.deliverControl(makeControlView({ ...THEIRS, pending: [asking] })))
    expect(screen.queryByRole('button', { name: 'Hand over' })).toBeNull()

    act(() => terminal.deliverControl(makeControlView({ ...OURS, pending: [asking] })))
    expect(screen.getByText('Safari on iPad is asking for control')).toBeInTheDocument()

    act(() => screen.getByRole('button', { name: 'Hand over' }).click())
    expect(terminal.current()?.acceptControl).toHaveBeenCalledWith(asking.clientId)
  })

  // The one shape of "no terminal" where the connection is still up, and so the
  // one where the server has no reason to take the lease away: it is held for a
  // client that is present, is not suspended, and will not expire. Without this
  // button the person who was typing has no way to stop being the controller.
  it('lets a controller whose terminal ended give the lease up', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    act(() => terminal.deliverControl(OURS))
    act(() => terminal.deliverEnded())

    expect(screen.getByText('No active terminal')).toBeInTheDocument()
    act(() => screen.getByRole('button', { name: 'Release control' }).click())
    expect(terminal.current()?.releaseControl).toHaveBeenCalledTimes(1)
  })

  // And a viewer in the same position is offered nothing, because it is holding
  // nothing: a Release button for a client with no lease would be a button that
  // tells the server about a fact it already has.
  it('offers no such thing to a viewer whose terminal ended', () => {
    const { terminal } = renderWithTerminal(<TerminalViewer projectID={PROJECT} running />)

    act(() => terminal.deliverControl(THEIRS))
    act(() => terminal.deliverEnded())

    expect(screen.getByText('No active terminal')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Release control' })).toBeNull()
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
