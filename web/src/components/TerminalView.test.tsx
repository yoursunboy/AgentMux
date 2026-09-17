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

function renderView(interactive = true) {
  const double = makeSession()
  const view = render(<TerminalView session={double.session} interactive={interactive} />)
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

      rerender(<TerminalView session={session} interactive />)
      expect(FakeTerminal.last.options.disableStdin).toBe(false)
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
