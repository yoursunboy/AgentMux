/**
 * The React seam, driven directly.
 *
 * These are the rules the frontend enforces on itself - who may type, who may
 * resize, and what a client does when it is handed the keyboard - and they are
 * asserted here rather than through a panel because a panel is a rendering of
 * them, not the place they live. The server enforces the same rules and refuses
 * anything that gets past this; the point of this layer is that nothing does.
 *
 * There is no view and no xterm in this file. The hook is the whole subject.
 */
import { act, renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { TerminalProvider, useTerminalSession, type TerminalSession } from './useTerminal'
import { ControlReason } from './protocol'
import {
  CLIENT_ID,
  makeClient,
  makeControlView,
  makeStatus,
  type ClientDouble,
} from '../test/terminal'

const PROJECT = 'p_0123456789abcdef0123'
const OTHER = 'c_ffffffffffffffff'

/** A roster naming somebody else, for the cases about not being the controller. */
const theirs = () => makeControlView({ controller: { clientId: OTHER, device: 'Safari on iPad' } })
const ours = () =>
  makeControlView({ controller: { clientId: CLIENT_ID, device: 'Chrome on Windows' } })

function render(terminal: ClientDouble, active = true) {
  return renderHook(() => useTerminalSession(PROJECT, active), {
    wrapper: ({ children }) => (
      <TerminalProvider client={terminal.client}>{children}</TerminalProvider>
    ),
  })
}

/** A session that has been subscribed and told who is in charge. */
function watching(view = theirs()): { terminal: ClientDouble; session: TerminalSession } {
  const terminal = makeClient({ status: makeStatus({ state: 'open' }) })
  const { result } = render(terminal)
  act(() => terminal.deliverControl(view))
  return { terminal, session: result.current }
}

describe('a viewer', () => {
  it('cannot type, so it does not send what it cannot type', () => {
    // The server refuses this and would drop the bytes. Sending them anyway
    // would be a message per keystroke that exists only to be refused.
    const { terminal, session } = watching()

    session.input('hello')

    expect(terminal.current()?.input).not.toHaveBeenCalled()
  })

  it('cannot resize, so it does not send its own width', () => {
    // The one that matters most: a phone measuring itself is not a person
    // asking for anything, and a viewer that sent its size would reflow the
    // pty for whoever is actually typing every time it rotated.
    const { terminal, session } = watching()

    session.resize(40, 20)

    expect(terminal.current()?.resize).not.toHaveBeenCalled()
  })

  it('knows it is a viewer rather than being told', () => {
    const { session } = watching()

    expect(session.control.held).toBe(false)
    expect(session.control.waiting).toBe(false)
  })

  it('knows when it is in the queue', () => {
    const { session } = watching(
      makeControlView({
        controller: { clientId: OTHER, device: 'Safari on iPad' },
        pending: [{ clientId: CLIENT_ID, device: 'Chrome on Windows' }],
      }),
    )

    expect(session.control.waiting).toBe(true)
    expect(session.control.held).toBe(false)
  })
})

describe('a controller', () => {
  it('types, and the bytes go out as they always did', () => {
    const { terminal, session } = watching(ours())

    session.input('ls')

    expect(terminal.current()?.input).toHaveBeenCalledWith('ls')
  })

  it('resizes', () => {
    const { terminal, session } = watching(ours())

    session.resize(100, 30)

    expect(terminal.current()?.resize).toHaveBeenCalledWith(100, 30)
  })

  it('states the size it has been drawing at, the moment it is given the keyboard', () => {
    // The pty has been shaped by whoever was typing. This client has been
    // drawing at its own size all along - a phone watching a 120-column
    // terminal - and the roster that made it the controller did not change
    // that. Nothing else would send it either: no resize event has happened,
    // so without this the phone would take control and the terminal would stay
    // the desktop's shape.
    const terminal = makeClient({ status: makeStatus({ state: 'open' }) })
    const { result } = render(terminal)
    act(() => terminal.deliverControl(theirs()))
    act(() => result.current.setSize({ cols: 44, rows: 22 }))

    act(() => terminal.deliverControl(ours()))

    expect(terminal.current()?.resize).toHaveBeenCalledWith(44, 22)
  })

  it('does not restate its size when the roster arrives again', () => {
    // The transition is what matters, not the state. A roster is broadcast
    // whenever the audience changes, and a resize per broadcast is a resize per
    // somebody opening a second tab.
    const { terminal } = watching(ours())
    act(() => terminal.deliverControl(ours()))

    expect(terminal.current()?.resize).not.toHaveBeenCalled()
  })

  it('stops typing the moment it is no longer the controller', () => {
    const { terminal, session } = watching(ours())
    act(() => terminal.deliverControl(theirs()))

    session.input('rm -rf /')

    expect(terminal.current()?.input).not.toHaveBeenCalled()
  })
})

describe('asking and answering', () => {
  it('passes each of the four actions to the subscription', () => {
    const { terminal, session } = watching()

    session.requestControl()
    session.acceptControl(OTHER)
    session.rejectControl(OTHER)
    session.releaseControl()

    expect(terminal.current()?.requestControl).toHaveBeenCalledOnce()
    expect(terminal.current()?.acceptControl).toHaveBeenCalledWith(OTHER)
    expect(terminal.current()?.rejectControl).toHaveBeenCalledWith(OTHER)
    expect(terminal.current()?.releaseControl).toHaveBeenCalledOnce()
  })

  it('keeps the reason a refusal came with, which no roster carries', () => {
    const terminal = makeClient({ status: makeStatus({ state: 'open' }) })
    const { result } = render(terminal)

    act(() =>
      terminal.deliverControl(theirs(), {
        type: 'control.denied',
        projectId: PROJECT,
        reason: ControlReason.controllerExists,
        control: theirs(),
      }),
    )

    expect(result.current.control.reason).toBe(ControlReason.controllerExists)
    expect(result.current.control.held).toBe(false)
  })

  it('says nothing about control before the server has', () => {
    const terminal = makeClient({ status: makeStatus({ state: 'open' }) })
    const { result } = render(terminal)

    expect(result.current.control.view).toBeNull()
    expect(result.current.control.held).toBe(false)
  })
})

describe('a panel that changes project', () => {
  it('forgets the roster of the project it was watching', () => {
    // Two projects have two rosters, and a session that kept the last one would
    // show the previous project's controller on the new terminal for as long as
    // the new roster takes to arrive.
    const terminal = makeClient({ status: makeStatus({ state: 'open' }) })
    const { result, rerender } = renderHook(
      ({ id }: { id: string }) => useTerminalSession(id, true),
      {
        wrapper: ({ children }) => (
          <TerminalProvider client={terminal.client}>{children}</TerminalProvider>
        ),
        initialProps: { id: PROJECT },
      },
    )
    act(() => terminal.deliverControl(ours()))
    expect(result.current.control.held).toBe(true)

    rerender({ id: 'p_ffffffffffffffffffff' })

    expect(result.current.control.view).toBeNull()
    expect(result.current.control.held).toBe(false)
  })
})
