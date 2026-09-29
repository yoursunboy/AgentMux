import { act, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { ControlReason } from '../terminal/protocol'
import { useTerminalSession } from '../terminal/useTerminal'
import { renderWithTerminal } from '../test/dashboard'
import { CLIENT_ID, makeControlView, makeStatus } from '../test/terminal'
import { ModeSwitch } from './ModeSwitch'

const PROJECT = 'p_0123456789abcdef0123'

/** A roster naming this client, which is the one that carries the lease. */
const OURS = makeControlView({ controller: { clientId: CLIENT_ID, device: 'Chrome on Windows' } })
/** A roster naming somebody else, which is what a viewer is sent. */
const THEIRS = makeControlView({
  controller: { clientId: 'c_ffffffffffffffff', device: 'Safari on iPad' },
})

/**
 * Switch is the button as a card draws it, on the card's own session.
 *
 * The real `useTerminalSession` over the page's client double, rather than a
 * stand-in: the half of this button worth testing is what happens between a tap
 * and the lease, and only the real hook can tell those two apart. `input`
 * returns early for a client that is not the controller, so a switch that sent
 * on the tap would send nothing at all and look like a button that does not
 * work - a stand-in session would happily report that it had sent.
 *
 * `mayResize: false` is what the card passes; nothing here depends on it, and
 * saying otherwise would be this file testing a rule it does not own.
 */
function Switch() {
  const session = useTerminalSession(PROJECT, true, { mayResize: false })
  return <ModeSwitch session={session} />
}

describe('ModeSwitch', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  // The whole of the button, in the case it is for: the person already has the
  // keyboard, so the tap is a keystroke and nothing else.
  it('sends the bytes Shift+Tab sends, when this client has the lease', () => {
    const { terminal } = renderWithTerminal(<Switch />)

    act(() => terminal.deliverControl(OURS))
    act(() => screen.getByRole('button', { name: 'Mode' }).click())

    expect(terminal.current()?.input).toHaveBeenCalledWith('\x1b[Z')
    // And nothing was asked for. Taking the lease back from a client that
    // already holds it would be a request the server has to answer with "you
    // already have it", which is a round trip for nothing.
    expect(terminal.current()?.requestControl).not.toHaveBeenCalled()
  })

  // The tablet's actual case on a shared project: the tap is a request for the
  // lease first, and the keystroke follows the lease rather than the tap.
  it('takes control first, and sends the keystroke when the lease arrives', () => {
    const { terminal } = renderWithTerminal(<Switch />)

    act(() => terminal.deliverControl(THEIRS))
    act(() => screen.getByRole('button', { name: 'Mode' }).click())

    expect(terminal.current()?.requestControl).toHaveBeenCalledTimes(1)
    expect(terminal.current()?.input).not.toHaveBeenCalled()

    act(() => terminal.deliverControl(OURS))

    expect(terminal.current()?.input).toHaveBeenCalledTimes(1)
    expect(terminal.current()?.input).toHaveBeenCalledWith('\x1b[Z')
    // And the button is a button again, rather than a request still in flight.
    expect(screen.getByRole('button', { name: 'Mode' })).toBeInTheDocument()
  })

  it('says what it is waiting for while it waits', () => {
    const { terminal } = renderWithTerminal(<Switch />)

    act(() => terminal.deliverControl(THEIRS))
    act(() => screen.getByRole('button', { name: 'Mode' }).click())

    expect(screen.getByRole('button', { name: 'Taking control…' })).toBeInTheDocument()
  })

  // The bound on the wait, and the reason it exists: a terminal somebody else is
  // using queues the request, and a keystroke that landed whenever they happened
  // to stop for lunch would change the mode of work somebody else is in the
  // middle of - long after the tap that asked for it still meant anything.
  it('drops the keystroke when the lease does not arrive in time', async () => {
    vi.useFakeTimers()
    const { terminal } = renderWithTerminal(<Switch />)

    act(() => terminal.deliverControl(THEIRS))
    act(() => screen.getByRole('button', { name: 'Mode' }).click())

    await act(async () => {
      vi.advanceTimersByTime(10_000)
    })
    expect(screen.getByRole('button', { name: 'Mode' })).toBeInTheDocument()

    // The lease is granted after the person has stopped connecting it to
    // anything, and nothing goes out for it.
    act(() => terminal.deliverControl(OURS))
    expect(terminal.current()?.input).not.toHaveBeenCalled()
  })

  // The other way the wait ends early, and the one that keeps the button from
  // lying: a refusal is an answer. Without this the button would read "taking
  // control" for the rest of the ten seconds while the card beside it shows the
  // sentence saying somebody else is using the terminal.
  it('drops the keystroke when the server refuses the request', () => {
    const { terminal } = renderWithTerminal(<Switch />)

    act(() => terminal.deliverControl(THEIRS))
    act(() => screen.getByRole('button', { name: 'Mode' }).click())
    act(() =>
      terminal.deliverControl(THEIRS, {
        type: 'control.denied',
        projectId: PROJECT,
        control: THEIRS,
        reason: ControlReason.controllerExists,
      }),
    )

    expect(screen.getByRole('button', { name: 'Mode' })).toBeInTheDocument()

    // And the refusal is not something a later roster undoes: the lease coming
    // free afterwards is not an answer to this tap, and the mode of the next
    // person's work is not this person's to change.
    act(() => terminal.deliverControl(makeControlView()))
    act(() => terminal.deliverControl(OURS))
    expect(terminal.current()?.input).not.toHaveBeenCalled()
  })

  // A roster that carries no reason is not an answer to anything - it is the
  // server saying who is watching. It arrives whenever any client opens or
  // closes the project, including one opening it in another tab, so treating it
  // as a refusal would cancel a keystroke that is still on its way.
  it('goes on waiting when a roster arrives that answers nothing', () => {
    const { terminal } = renderWithTerminal(<Switch />)

    act(() => terminal.deliverControl(THEIRS))
    act(() => screen.getByRole('button', { name: 'Mode' }).click())

    act(() => terminal.deliverControl(makeControlView({ ...THEIRS, viewers: 3 })))

    expect(screen.getByRole('button', { name: 'Taking control…' })).toBeInTheDocument()
    act(() => terminal.deliverControl(OURS))
    expect(terminal.current()?.input).toHaveBeenCalledWith('\x1b[Z')
  })

  // A card that claimed the keyboard by drawing itself would take it from
  // whoever is working - one viewer, silently, per project on the page.
  it('asks for nothing by being rendered', () => {
    const { terminal } = renderWithTerminal(<Switch />)

    expect(terminal.current()?.requestControl).not.toHaveBeenCalled()
    expect(terminal.current()?.input).not.toHaveBeenCalled()
  })

  // There is no socket to send a keystroke down, so there is nothing the button
  // can do - and a control that cannot work is worse than one that is not there,
  // which is why this is disabled rather than hidden: the Runtime row it sits in
  // is where a person looks to find out why.
  it('is disabled while the connection is down', () => {
    renderWithTerminal(<Switch />, { status: makeStatus({ state: 'reconnecting' }) })

    expect(screen.getByRole('button', { name: 'Mode' })).toBeDisabled()
  })

  // The same, one state further on: the terminal this was watching has ended.
  // The connection is up, so the button would otherwise look live.
  it('is disabled once the subscription has ended', () => {
    const { terminal } = renderWithTerminal(<Switch />)

    act(() => terminal.deliverControl(OURS))
    act(() => terminal.deliverEnded())

    expect(screen.getByRole('button', { name: 'Mode' })).toBeDisabled()
  })
})
