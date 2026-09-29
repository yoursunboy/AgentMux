import { act, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { makeProjectSettings, PERMISSION_MODES } from '../test/controller'
import { PermissionMenu } from './PermissionMenu'

/**
 * The menu that chooses what the *next* launch of a project asks before it acts.
 *
 * # What these tests are really about
 *
 * Three of the five named in §10 of the phase brief are about a write being a
 * write and nothing more: the mode is sent to one endpoint, and **nothing is
 * restarted**. That last one is asserted rather than assumed, because the
 * tempting implementation - call the settings endpoint and then restart the
 * agent so the change "takes" - is one line away and would terminate somebody's
 * session from a menu that never said it would.
 *
 * # The fetch double
 *
 * Every test here stubs `fetch`, because the component's whole job is one
 * request and the assertions are about which request. The spy is returned so a
 * test can say "and only that one" rather than "and at least that one".
 */

/** jsonResponse builds the Response the client will read. */
function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function mockFetch(...responses: (Response | Error)[]) {
  let call = 0
  const spy = vi.fn((_input: RequestInfo | URL, _init?: RequestInit) => {
    const next = responses[Math.min(call, responses.length - 1)]
    call += 1
    return next instanceof Error ? Promise.reject(next) : Promise.resolve(next.clone())
  })
  vi.stubGlobal('fetch', spy)
  return spy
}

/** The method and init of the nth call, which is what most assertions are about. */
function callOf(spy: ReturnType<typeof mockFetch>, n: number): [string, RequestInit] {
  const [input, init] = spy.mock.calls[n] ?? []
  return [String(input), (init ?? {}) as RequestInit]
}

/**
 * press clicks something and lets React finish rendering it.
 *
 * Without the `act` the state update is still queued when the next line runs,
 * which is how a test comes to assert against the screen as it was before the
 * press. It is the same wrapper `ProjectPanel.test.tsx` uses for the same
 * reason.
 */
function press(element: HTMLElement): void {
  act(() => {
    element.click()
  })
}

afterEach(() => {
  vi.unstubAllGlobals()
})

/** The props a card passes, with the ordinary project's values. */
function renderMenu(props: Partial<Parameters<typeof PermissionMenu>[0]> = {}) {
  return render(
    <PermissionMenu projectId="p_0123456789abcdef0123" mode="manual" available {...props} />,
  )
}

/** The button, and the three items once the menu is open. */
function openTheMenu(): HTMLElement {
  const button = screen.getByRole('button', { name: /^Permission/ })
  press(button)
  return button
}

describe('PermissionMenu', () => {
  // §10.1 - the card shows the control. It sits on the card whether or not an
  // agent is running, because it configures the next launch and not this one.
  it('shows the button on the card', () => {
    renderMenu()

    const button = screen.getByRole('button', { name: 'Permission' })
    expect(button).toBeVisible()
    expect(button).toHaveAttribute('aria-haspopup', 'menu')
  })

  // It must not open on its own: a card that had a menu open would be a card
  // covering the one below it on load.
  it('draws no menu until it is pressed', () => {
    renderMenu()

    expect(screen.queryByRole('menu')).toBeNull()
  })

  // §10.2 - the menu opens, and it names what it is choosing. The label is the
  // brief's own words, and it is a name rather than a toggle: nothing here is
  // called a switch.
  it('opens a menu naming the setting, with the three modes and the current one marked', () => {
    renderMenu({ mode: 'acceptEdits' })
    openTheMenu()

    expect(screen.getByRole('menu')).toHaveAccessibleName('Claude Permission Mode')

    const items = screen.getAllByRole('menuitemradio')
    expect(items).toHaveLength(PERMISSION_MODES.length)
    PERMISSION_MODES.forEach((mode, index) => {
      expect(items[index]).toHaveTextContent(mode)
    })

    const chosen = items.filter((item) => item.getAttribute('aria-checked') === 'true')
    expect(chosen).toHaveLength(1)
    expect(chosen[0]).toHaveTextContent('acceptEdits')
    // The mark is the same fact as `aria-checked`, drawn for somebody who is
    // looking at the menu rather than listening to it.
    expect(chosen[0]?.textContent).toContain('●')
    expect(items.filter((item) => item.getAttribute('aria-checked') === 'false')).toHaveLength(2)
  })

  // §10.3 - choosing a mode saves it. The request is the claim: the method, the
  // path, and the body.
  it('saves the chosen mode to the project settings endpoint', async () => {
    const spy = mockFetch(
      jsonResponse({ settings: makeProjectSettings({ permissionMode: 'acceptEdits' }) }),
    )

    renderMenu({ mode: 'manual' })
    openTheMenu()
    press(screen.getByRole('menuitemradio', { name: /^acceptEdits/ }))

    await waitFor(() => expect(spy).toHaveBeenCalledTimes(1))

    const [path, init] = callOf(spy, 0)
    expect(path).toBe('/api/projects/p_0123456789abcdef0123/settings')
    expect(init.method).toBe('PATCH')
    expect(JSON.parse(String(init.body))).toEqual({ permissionMode: 'acceptEdits' })

    // And the menu closes on the way, so the card is not left covered.
    expect(screen.queryByRole('menu')).toBeNull()
  })

  // §10.5, and the one worth having. A successful save is a preference stored
  // and nothing else: the agent that is running keeps the mode it started with
  // until somebody restarts it. A restart here would be this menu terminating a
  // session on its own initiative.
  it('saves without restarting, starting or stopping anything', async () => {
    const spy = mockFetch(
      jsonResponse({ settings: makeProjectSettings({ permissionMode: 'bypassPermissions' }) }),
    )

    renderMenu({ mode: 'manual' })
    openTheMenu()
    press(screen.getByRole('menuitemradio', { name: /^bypassPermissions/ }))

    await waitFor(() => expect(spy).toHaveBeenCalledTimes(1))

    // The whole set of calls, not a filter over it: a filter would pass a
    // component that made a second request under a path nobody thought to
    // exclude.
    expect(spy).toHaveBeenCalledTimes(1)
    const [path, init] = callOf(spy, 0)
    expect(init.method).toBe('PATCH')
    for (const forbidden of ['/runtime', '/agent', 'start', 'stop', 'restart', 'input']) {
      expect(path).not.toContain(forbidden)
    }
  })

  // §10.4 - the person is told what happened, and told the part that matters:
  // that it has not taken effect yet.
  it('says the mode is saved and that a restart applies it', async () => {
    mockFetch(jsonResponse({ settings: makeProjectSettings({ permissionMode: 'acceptEdits' }) }))

    renderMenu({ mode: 'manual' })
    openTheMenu()
    press(screen.getByRole('menuitemradio', { name: /^acceptEdits/ }))

    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent(
        'Permission mode updated. Restart Agent to apply.',
      ),
    )
  })

  // A chosen mode that is already in force is not a write. Sending it would be a
  // no-op request and a message claiming something changed.
  it('does not write when the mode that was chosen is the one already in force', () => {
    const spy = mockFetch(jsonResponse({ settings: makeProjectSettings() }))

    renderMenu({ mode: 'manual' })
    openTheMenu()
    press(screen.getByRole('menuitemradio', { name: /^manual/ }))

    expect(spy).not.toHaveBeenCalled()
    expect(screen.getByRole('status')).toBeEmptyDOMElement()
  })

  it('reports a refused save instead of claiming it worked', async () => {
    mockFetch(
      jsonResponse(
        { error: { code: 'invalid_input', message: '"x" is not a Claude permission mode' } },
        400,
      ),
    )

    renderMenu({ mode: 'manual' })
    openTheMenu()
    press(screen.getByRole('menuitemradio', { name: /^acceptEdits/ }))

    await waitFor(() =>
      expect(screen.getByRole('status')).toHaveTextContent('is not a Claude permission mode'),
    )
    expect(screen.getByRole('status')).not.toHaveTextContent('Permission mode updated')
  })

  // The third state of a section, the same one the other rows have: the server
  // cannot say. Showing a mode here would be this component inventing the fact
  // it exists to report, so it offers nothing and says why.
  it('offers nothing when the server cannot report the setting', () => {
    renderMenu({ available: false, mode: '' })

    const button = screen.getByRole('button', { name: 'Permission (unavailable)' })
    expect(button).toBeDisabled()
    expect(button.getAttribute('title')).toContain('cannot report')
    expect(screen.queryByRole('menu')).toBeNull()
  })

  // The menu is a thing that covers something else, so it closes the way one
  // does: a press anywhere else, or Escape - and focus goes back where it came
  // from, so tabbing continues from the button rather than the top of the page.
  it('closes on a press outside it and on Escape', async () => {
    renderMenu()
    const button = openTheMenu()
    expect(screen.getByRole('menu')).toBeInTheDocument()

    act(() => {
      document.body.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
    })
    expect(screen.queryByRole('menu')).toBeNull()

    press(button)
    expect(screen.getByRole('menu')).toBeInTheDocument()

    act(() => {
      document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    })
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull())
    expect(button).toHaveFocus()
  })

  // Reopening puts the last message away. It describes one save, and left up
  // over a menu somebody has reopened it reads as a claim about the mode the
  // menu is now showing.
  it('puts the previous message away when the menu is reopened', async () => {
    mockFetch(jsonResponse({ settings: makeProjectSettings({ permissionMode: 'acceptEdits' }) }))

    renderMenu({ mode: 'manual' })
    const button = openTheMenu()
    press(screen.getByRole('menuitemradio', { name: /^acceptEdits/ }))

    await waitFor(() => expect(screen.getByRole('status')).not.toBeEmptyDOMElement())

    press(button)
    expect(screen.getByRole('status')).toBeEmptyDOMElement()
  })
})
