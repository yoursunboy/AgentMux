/**
 * The terminal view, against a double for xterm.
 *
 * xterm is mocked here, and the reason is worth stating rather than assumed:
 * what this component owns is the wiring - which bytes go to the terminal, in
 * what order, at what size, and what the scroll position means - and xterm owns
 * the rendering. Running the real one in jsdom would mean stubbing canvas
 * contexts and matchMedia so that it can draw into a document nobody looks at,
 * and the assertions would be about xterm rather than about this file. Whether
 * the pixels are right is settled in a real browser.
 */
import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { TerminalView } from './TerminalView'
import { FakeFitAddon, FakeTerminal } from '../test/xterm'
import { makeSession } from '../test/terminal'

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

const encoder = new TextEncoder()

function renderView(interactive = true, fontSize = 11, mayResize = interactive) {
  const double = makeSession()
  const view = render(
    <TerminalView
      session={double.session}
      interactive={interactive}
      mayResize={mayResize}
      fontSize={fontSize}
    />,
  )
  return { ...double, ...view, view }
}

/** advance runs the debounce out, so a requested resize is actually sent. */
function settleResize(): void {
  act(() => {
    vi.advanceTimersByTime(1000)
  })
}

describe('TerminalView', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    FakeTerminal.reset()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('opens an xterm in the element it renders', () => {
    renderView()

    expect(FakeTerminal.last.opened).toBe(true)
    expect(FakeTerminal.last.host).toBe(screen.getByTestId('terminal-screen'))
  })

  it('registers the fit and unicode addons', () => {
    // The unicode one is what makes a wide character occupy two cells rather
    // than one, which is the difference between a table that lines up and one
    // that does not.
    renderView()

    const addons = FakeTerminal.last.addons
    expect(addons.some((addon) => addon instanceof FakeFitAddon)).toBe(true)
    expect(FakeTerminal.last.unicode.activeVersion).toBe('11')
  })

  it('tells the session its size before anything is subscribed', () => {
    // The subscribe that follows carries it, so the first screen the server
    // renders is already the right shape rather than resized a moment later.
    FakeFitAddon.size = { cols: 120, rows: 40 }

    const { session } = renderView()

    expect(session.setSize).toHaveBeenCalledWith({ cols: 120, rows: 40 })
  })

  it('hands the terminal to the session, which is what starts the subscription', () => {
    const { sinks } = renderView()

    expect(sinks).toHaveLength(1)
  })

  // The session's size is what this client states when it is given the keyboard
  // (`useTerminal`'s control handler), and the two below are where a client that
  // never fits learns it. Without them a viewer records the 80x24 xterm is born
  // with and keeps it while the pane is something else entirely - so the moment
  // it asks for control it states a size the terminal is not, and the pty
  // everybody is watching is reshaped to match a browser that was only looking.
  it('records the size the server says the terminal is, not the one xterm starts at', () => {
    const { session, current } = renderView(false, 11, false)

    act(() => current()!.show(encoder.encode('ready'), 120, 30))

    expect(session.setSize).toHaveBeenCalledWith({ cols: 120, rows: 30 })
  })

  it('and the size the server applied when somebody else reshaped the pane', () => {
    // Every watcher is told the size that was applied, not only the client that
    // asked for it (`conn.go`'s applySize broadcasts it), which is how a client
    // that is not typing learns the terminal has changed shape underneath it.
    const { session, current } = renderView(false, 11, false)

    act(() => current()!.resize(100, 40))

    expect(session.setSize).toHaveBeenCalledWith({ cols: 100, rows: 40 })
  })

  describe('drawing', () => {
    it('appends output', () => {
      const { current } = renderView()

      act(() => current()!.draw(encoder.encode('hello')))

      expect(FakeTerminal.last.text()).toBe('hello')
    })

    it('writes bytes rather than text, so an escape sequence survives', () => {
      // The client hands over the frame's payload exactly as it arrived. A view
      // that decoded it to a string and back would corrupt anything the decoder
      // did not understand, and a terminal's output is exactly that.
      const { current } = renderView()

      act(() => current()!.draw(new Uint8Array([0x1b, 0x5b, 0x33, 0x31, 0x6d, 0x41])))

      expect(FakeTerminal.last.text()).toBe('[31mA')
    })

    it('draws a snapshot after resizing to the geometry it was rendered for', () => {
      // Order matters: a screen rendered for 132 columns drawn into an 80-column
      // terminal wraps wrongly, and no later output can repair that.
      const { current } = renderView()
      expect(FakeTerminal.last.cols).toBe(80)

      // The element fits the geometry the server applied, which is the case a
      // snapshot is normally received in - the size came from this browser.
      FakeFitAddon.size = { cols: 132, rows: 50 }
      act(() => current()!.show(encoder.encode('wide screen'), 132, 50))

      expect(FakeTerminal.last.events).toEqual(['resize:132x50', 'write'])
      expect(FakeTerminal.last.text()).toBe('wide screen')
    })

    it('does not resize when the screen already fits', () => {
      FakeFitAddon.size = { cols: 80, rows: 24 }
      const { current } = renderView()

      act(() => current()!.show(encoder.encode('same shape'), 80, 24))

      expect(FakeTerminal.last.events).toEqual(['write'])
    })

    it('returns the viewport to the bottom when a snapshot changes the shape', () => {
      // The case a viewer meets and a controller does not: a page is sent the
      // terminal at *the terminal's* size rather than at its own, so reconnecting
      // to one larger than the page was drawing resizes into a screen the page
      // did not choose. Order matters as much as the scroll does - the rows go in
      // at the new shape, and the viewport is put at the bottom afterwards rather
      // than before, because the write can move it again.
      const { current } = renderView()
      expect(FakeTerminal.last.cols).toBe(80)

      FakeFitAddon.size = { cols: 132, rows: 50 }
      act(() => current()!.show(encoder.encode('wide screen'), 132, 50))

      expect(FakeTerminal.last.scrolledToBottom).toBe(1)
      expect(screen.queryByRole('button', { name: /Jump to latest|New output/ })).not.toBeInTheDocument()
    })

    it('anchors the viewport even when the shape did not change', () => {
      // The half of the rule a shape change does not cover, and the one that was
      // missing. A snapshot is the present: it is what the pane is showing now.
      // A terminal with scrollback of its own can take one into its screen rows
      // with the viewport left above them, and then the page paints the
      // terminal's history while tmux paints the screen. Measured in a browser as
      // a reconnected tab drawing the splash screen that came *before* what the
      // pane held.
      //
      // Not the rule the local scroll model keeps for output. Writing output
      // while somebody is reading further up must not drag them to the bottom,
      // and that is `draw`; a snapshot arrives when a connection is established
      // or a screen is asked for again, which are the moments the live screen is
      // what was asked for.
      const { current } = renderView()
      act(() => FakeTerminal.last.fireScroll(0, 200))

      act(() => current()!.show(encoder.encode('the screen as it is now'), 80, 24))

      expect(FakeTerminal.last.scrolledToBottom).toBe(1)
      expect(screen.queryByRole('button', { name: /Jump to latest|New output/ })).not.toBeInTheDocument()
    })

    it('adopts the size the server reports, so the acknowledgement is not echoed back', () => {
      // Without this the two answer each other forever: the server applies a
      // size, the client draws at it, the drawing reports a size.
      const { current, session } = renderView()
      FakeFitAddon.size = { cols: 100, rows: 30 }

      act(() => current()!.resize(100, 30))

      expect(FakeTerminal.last.cols).toBe(100)
      expect(FakeTerminal.last.rows).toBe(30)

      // The resize that xterm reports back for this change is not sent on.
      settleResize()
      expect(session.resize).not.toHaveBeenCalled()
    })
  })

  describe('the shape the terminal has', () => {
    it('keeps the shape the server sent while this browser is only watching', () => {
      // The case a viewer is in and a controller never sees. The screen arrives
      // at the terminal's size, which is not this browser's - and a viewer that
      // fitted its own element anyway would push the pty's rows down into its
      // scrollback and be left looking at the empty part of the screen below
      // them. Measured in the grid as a panel with nothing on it while tmux
      // held a shell prompt.
      const { current, session, rerender } = renderView(true, 11, false)

      act(() => current()!.show(encoder.encode('somebody else’s screen'), 100, 30))
      expect(FakeTerminal.last.cols).toBe(100)

      // Anything that measures - here a change of text size - must leave that
      // shape alone. The element fits 80x24.
      rerender(<TerminalView session={session} interactive mayResize={false} fontSize={12.5} />)
      settleResize()

      expect(FakeTerminal.last.cols).toBe(100)
      expect(FakeTerminal.last.rows).toBe(30)
      expect(session.resize).not.toHaveBeenCalled()
    })

    it('states its own size the moment it is given the keyboard', () => {
      // Being given the keyboard is being given the shape. The box here fits
      // exactly the size the server last applied, which is the case that would
      // say nothing at all unless the scheduler is told to forget what it had
      // recorded - the recorded size was the server's, not this browser's.
      const { current, session, rerender } = renderView(true, 11, false)

      act(() => current()!.show(encoder.encode('somebody else’s screen'), 80, 24))
      settleResize()
      vi.mocked(session.resize).mockClear()

      rerender(<TerminalView session={session} interactive mayResize fontSize={11} />)
      settleResize()

      expect(session.resize).toHaveBeenCalledWith(80, 24)
    })
  })

  describe('input', () => {
    it('sends a keystroke to the session', () => {
      const { session } = renderView()

      act(() => FakeTerminal.last.type('a'))

      expect(session.input).toHaveBeenCalledWith('a')
    })

    it('sends a control character unchanged', () => {
      // Ctrl+C is the reason raw input is raw.
      const { session } = renderView()

      act(() => FakeTerminal.last.type(''))

      expect(session.input).toHaveBeenCalledWith('')
    })

    it('refuses typing at the terminal while the connection is down', () => {
      // Better than accepting it and dropping it: somebody typing into a
      // terminal that is not connected should see that nothing is happening.
      const { rerender, session } = renderView(false)

      expect(FakeTerminal.last.options.disableStdin).toBe(true)

      rerender(<TerminalView session={session} interactive mayResize fontSize={11} />)
      expect(FakeTerminal.last.options.disableStdin).toBe(false)
    })

    it('opens at the text size it was given', () => {
      renderView(true, 9.5)

      expect(FakeTerminal.last.options.fontSize).toBe(9.5)
    })

    it('re-measures when the display mode changes the text size', () => {
      // A different text size is a different number of columns in the same box,
      // so it is a real change of geometry and the pty has to be told. It goes
      // through the same debounce as every other measurement.
      const { rerender, session } = renderView(true, 11)
      expect(FakeTerminal.last.options.fontSize).toBe(11)

      FakeFitAddon.size = { cols: 100, rows: 30 }
      rerender(<TerminalView session={session} interactive mayResize fontSize={12.5} />)

      expect(FakeTerminal.last.options.fontSize).toBe(12.5)
      settleResize()
      expect(session.resize).toHaveBeenCalledWith(100, 30)
    })

    it('does nothing when the text size has not changed', () => {
      const { rerender, session } = renderView(true, 11)
      settleResize()
      vi.mocked(session.resize).mockClear()

      rerender(<TerminalView session={session} interactive mayResize fontSize={11} />)
      settleResize()

      expect(session.resize).not.toHaveBeenCalled()
    })

    it('offers the keys a soft keyboard does not have', () => {
      // A tablet's keyboard offers letters. A terminal is mostly driven by the
      // keys it does not offer, and every one of these has to send the bytes
      // the real key sends - not a description of them.
      const { session } = renderView()

      const expected: Array<[string, string]> = [
        ['Escape', ''],
        ['Tab', '\t'],
        ['Interrupt', ''],
        ['Up arrow', '[A'],
        ['Down arrow', '[B'],
        ['Left arrow', '[D'],
        ['Right arrow', '[C'],
      ]
      for (const [name, bytes] of expected) {
        act(() => {
          fireEvent.click(screen.getByRole('button', { name }))
        })
        expect(session.input).toHaveBeenLastCalledWith(bytes)
      }
    })

    it('hands the focus back to the terminal after one of those keys', () => {
      // The button takes the focus, and a tablet's keyboard follows the focus,
      // so without this an Escape would close the keyboard and pressing a key
      // would mean the end of typing rather than one keystroke.
      renderView()
      const before = FakeTerminal.last.focused

      act(() => {
        fireEvent.click(screen.getByRole('button', { name: 'Escape' }))
      })

      expect(FakeTerminal.last.focused).toBe(before + 1)
    })

    it('refuses those keys while the connection is down, like any other typing', () => {
      renderView(false)

      expect(screen.getByRole('button', { name: 'Interrupt' })).toBeDisabled()
      expect(screen.getByRole('button', { name: 'Escape' })).toBeDisabled()
    })

    it('draws those keys unless it is told not to, which is what a viewer says', () => {
      // Two halves, and both are the point. The workspace passes no showKeys at
      // all, so the default has to be the keys it has always drawn - a prop
      // added for somebody else must not quietly change the page it was not
      // added for.
      //
      // The other half is the console's: a card passes showKeys={held}, so a
      // client without the lease passes showKeys={false} (docs/TERMINAL_VIEWER.md
      // §3). Disabling these keys would not do.
      // Each one calls session.input directly rather than through onData, so a
      // viewer's copy of them would be a second input path - one the missing
      // onData handler does not intercept. Not drawing them is the only version
      // of that which is structural.
      const { session } = makeSession()

      const shown = render(
        <TerminalView session={session} interactive={false} mayResize={false} fontSize={11} />,
      )
      expect(screen.queryByRole('button', { name: 'Escape' })).not.toBeNull()
      shown.unmount()

      render(
        <TerminalView
          session={session}
          interactive={false}
          mayResize={false}
          showKeys={false}
          fontSize={11}
        />,
      )
      expect(screen.queryByRole('button', { name: 'Escape' })).toBeNull()
    })
  })

  describe('resize', () => {
    it('sends a resize that has been measured, once the measurement settles', () => {
      // A window being dragged produces a measurement per frame; the terminal
      // must hear about it once, when the drag stops.
      const { session } = renderView()
      expect(session.resize).not.toHaveBeenCalled()

      FakeFitAddon.size = { cols: 132, rows: 50 }
      act(() => {
        window.dispatchEvent(new Event('resize'))
      })

      expect(session.resize).not.toHaveBeenCalled()
      settleResize()

      expect(session.resize).toHaveBeenCalledWith(132, 50)
      expect(session.setSize).toHaveBeenCalledWith({ cols: 132, rows: 50 })
    })

    it('sends one resize for a burst of measurements', () => {
      const { session } = renderView()

      for (const cols of [100, 110, 120, 132]) {
        FakeFitAddon.size = { cols, rows: 50 }
        act(() => {
          window.dispatchEvent(new Event('resize'))
        })
      }
      settleResize()

      expect(session.resize).toHaveBeenCalledTimes(1)
      expect(session.resize).toHaveBeenCalledWith(132, 50)
    })

    it('sends nothing when a resize settles on the size already in use', () => {
      const { session } = renderView()

      FakeFitAddon.size = { cols: 80, rows: 24 }
      act(() => {
        window.dispatchEvent(new Event('resize'))
      })
      settleResize()

      expect(session.resize).not.toHaveBeenCalled()
    })
  })

  describe('the local scroll model', () => {
    it('starts at the bottom and offers nothing', () => {
      renderView()

      expect(screen.queryByRole('button', { name: /Jump to latest|New output/ })).not.toBeInTheDocument()
    })

    it('offers a way back once the viewport has been scrolled up', () => {
      renderView()

      act(() => FakeTerminal.last.fireScroll(0, 200))

      expect(screen.getByRole('button', { name: /Jump to latest/ })).toBeInTheDocument()
    })

    it('says new output arrived rather than silently pulling the viewport down', () => {
      // Being yanked to the bottom while reading something further up is the
      // behaviour this exists to prevent.
      const { current } = renderView()
      act(() => FakeTerminal.last.fireScroll(0, 200))

      act(() => current()!.draw(encoder.encode('a new line')))

      expect(screen.getByRole('button', { name: /New output/ })).toBeInTheDocument()
      expect(FakeTerminal.last.text()).toBe('a new line')
    })

    it('goes back to the bottom, and stops offering, when asked', () => {
      const { current } = renderView()
      act(() => FakeTerminal.last.fireScroll(0, 200))
      act(() => current()!.draw(encoder.encode('a new line')))

      // fireEvent rather than userEvent: this suite runs on fake timers, and
      // userEvent's own delays would need the clock advanced underneath it for
      // every interaction.
      fireEvent.click(screen.getByRole('button', { name: /New output/ }))

      expect(FakeTerminal.last.scrolledToBottom).toBe(1)
      expect(screen.queryByRole('button', { name: /Jump to latest|New output/ })).not.toBeInTheDocument()
    })

    it('treats a scroll back to the bottom as following again', () => {
      renderView()

      act(() => FakeTerminal.last.fireScroll(0, 200))
      act(() => FakeTerminal.last.fireScroll(200, 200))

      expect(screen.queryByRole('button', { name: /Jump to latest|New output/ })).not.toBeInTheDocument()
    })

    it('offers the way back when the browser scrolls the viewport, as a wheel does', () => {
      // xterm reports its own scrolling through onScroll, and a mouse wheel is
      // not its own scrolling: the browser moves the viewport element and xterm
      // says nothing. Measured in a real browser, where the wheel moved
      // scrollTop and this component never heard about it - so a mouse user
      // scrolling up was still being treated as following, and never got the
      // way back to the bottom.
      renderView()

      act(() => FakeTerminal.last.fireViewportScroll(120, 400))

      expect(screen.getByRole('button', { name: /Jump to latest/ })).toBeInTheDocument()
    })

    it('marks output that arrives while a wheel has scrolled the viewport up', () => {
      const { current } = renderView()

      act(() => FakeTerminal.last.fireViewportScroll(120, 400))
      act(() => current()!.draw(encoder.encode('a new line')))

      expect(screen.getByRole('button', { name: /New output/ })).toBeInTheDocument()
    })

    it('never tells the session where the viewport is', () => {
      // Two people watching one terminal are looking at different lines, and a
      // server that knew where either of them was would have to pick one.
      const { session, current } = renderView()

      act(() => FakeTerminal.last.fireScroll(0, 200))
      act(() => current()!.draw(encoder.encode('output')))

      expect(session.input).not.toHaveBeenCalled()
      expect(session.resize).not.toHaveBeenCalled()
    })
  })

  it('tears the terminal down when it goes away', () => {
    const { view } = renderView()

    view.unmount()

    expect(FakeTerminal.last.disposed).toBe(true)
  })
})
