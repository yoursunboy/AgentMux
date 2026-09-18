import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { ProjectPanel, type PanelActions, type PanelPosition } from './ProjectPanel'
import { ProjectTerminal } from './ProjectTerminal'
import { makeProject } from '../test/fixtures'
import {
  makeClient,
  makeSession,
  makeStatus,
  makeTerminalError,
  type ClientDouble,
} from '../test/terminal'
import { TerminalProvider } from '../terminal/useTerminal'
import type { TerminalSession } from '../terminal/useTerminal'
import type { Project } from '../api/types'

// xterm is replaced here for the same reason it is replaced in TerminalView's
// own suite: it is a third-party renderer, and this suite is about the chrome
// around the terminal - which state is announced, which controls are offered,
// and whether anything is subscribed to at all. What the view does with the
// frames it is given is asserted where it is implemented.
vi.mock('./TerminalView', async () => {
  const double = await import('../test/terminal')
  return { TerminalView: double.FakeTerminalView }
})

interface Options {
  project?: Project
  terminalAvailable?: boolean
  terminalBlocker?: string
  busy?: boolean
  terminal?: ClientDouble
  position?: Partial<PanelPosition>
  mode?: 'grid' | 'focus' | 'fullscreen'
  canFullscreen?: boolean
}

/** The position of a lone project in a workspace of one. */
const onlySlot: PanelPosition = { slot: 0, total: 1, canMoveLeft: false, canMoveRight: false }

/** everyAction is an actions object whose calls a test can assert on. */
function everyAction(): PanelActions {
  return {
    startRuntime: vi.fn(),
    stopRuntime: vi.fn(),
    startAgent: vi.fn(),
    destroyRuntime: vi.fn(),
    focus: vi.fn(),
    leaveFocus: vi.fn(),
    toggleFullscreen: vi.fn(),
    move: vi.fn(),
    pin: vi.fn(),
    remove: vi.fn(),
    details: vi.fn(),
  }
}

/**
 * renderPanel renders the panel with the ordinary healthy case, so each test
 * states only the one fact it is about.
 *
 * The default is "this server can host a terminal": a test that wants the
 * unavailable case passes terminalAvailable: false, which is the failure this
 * panel exists to describe honestly.
 */
function renderPanel(options: Options = {}) {
  const actions = everyAction()
  const props = {
    project: options.project ?? makeProject(),
    terminalAvailable: options.terminalAvailable ?? true,
    terminalBlocker: options.terminalBlocker ?? '',
    busy: options.busy ?? false,
    position: { ...onlySlot, ...options.position },
    actions,
    mode: options.mode ?? ('grid' as const),
    canFullscreen: options.canFullscreen ?? true,
  }
  const terminal = options.terminal ?? makeClient()
  render(
    <TerminalProvider client={terminal.client}>
      <ProjectPanel {...props} />
    </TerminalProvider>,
  )
  return { ...props, actions, terminal }
}

/** openMenu opens the panel's "⋯" menu and returns it. */
async function openMenu(projectName = 'AgentMux') {
  await userEvent.click(screen.getByRole('button', { name: `Actions for ${projectName}` }))
  return screen.getByRole('menu', { name: `Actions for ${projectName}` })
}

/** menuItem finds one item in an open menu by its label. */
function menuItem(menu: HTMLElement, label: string | RegExp): HTMLElement {
  return within(menu).getByRole('menuitem', { name: label })
}

/** A project whose runtime is up, which is when there is a terminal to show. */
const running = (overrides: Partial<Project> = {}) =>
  makeProject({ status: 'running', ...overrides })

describe('ProjectPanel', () => {
  describe('the project itself', () => {
    it('names the project and its status', () => {
      renderPanel({ project: makeProject({ name: 'AgentMux', status: 'stopped' }) })

      expect(screen.getByRole('heading', { name: 'AgentMux' })).toBeInTheDocument()
      expect(screen.getByText('Stopped')).toBeInTheDocument()
    })

    it('keeps the header to a name, a state and two buttons', () => {
      // The grid's header is the difference between a panel that shows a
      // terminal and one that shows a header with a terminal under it. Nothing
      // in it may be a path, a session name or a timestamp.
      renderPanel({ project: running({ name: 'AgentMux' }) })

      const header = screen.getByRole('heading', { name: 'AgentMux' }).closest('header')
      expect(header).not.toBeNull()
      expect(header?.textContent).toContain('Running')
      expect(header?.textContent).not.toMatch(/p_[0-9a-f]{20}/)
      expect(header?.textContent).not.toMatch(/amx-/)
      expect(header?.textContent).not.toMatch(/[A-Z]:\\|\/mnt\//)
    })

    it('describes every runtime state in words, not just a glyph', () => {
      const cases: Array<[Project['status'], string]> = [
        ['starting', 'Starting'],
        ['stopping', 'Stopping'],
        ['error', 'Error'],
        ['orphan', 'Orphaned session'],
      ]

      for (const [status, label] of cases) {
        const { unmount } = render(
          <TerminalProvider client={makeClient().client}>
            <ProjectPanel
              project={makeProject({ status })}
              terminalAvailable
              terminalBlocker=""
              busy={false}
              position={onlySlot}
              actions={everyAction()}
              mode="grid"
              canFullscreen
            />
          </TerminalProvider>,
        )
        expect(screen.getByText(label)).toBeInTheDocument()
        unmount()
      }
    })
  })

  describe('the runtime controls, which live in the menu', () => {
    it('leaves them disabled when the server cannot host a runtime', async () => {
      renderPanel({ terminalAvailable: false, project: running() })
      const menu = await openMenu()

      expect(menuItem(menu, 'Stop runtime')).toBeDisabled()
      expect(menuItem(menu, 'Destroy runtime…')).toBeDisabled()
      expect(menuItem(menu, 'Start Claude here')).toBeDisabled()
    })

    it('offers a stop, and no start, while the runtime is running', async () => {
      const props = renderPanel({ project: running() })
      const menu = await openMenu()

      const stop = menuItem(menu, 'Stop runtime')
      expect(stop).toBeEnabled()
      await userEvent.click(stop)
      expect(props.actions.stopRuntime).toHaveBeenCalledOnce()
    })

    it('treats a reconnecting runtime as up, because the session outlives the server', async () => {
      renderPanel({ project: makeProject({ status: 'reconnecting' }) })
      const menu = await openMenu()

      expect(menuItem(menu, 'Stop runtime')).toBeEnabled()
    })

    it('disables the runtime actions while a request is in flight', async () => {
      renderPanel({ project: running(), busy: true })
      const menu = await openMenu()

      expect(menuItem(menu, 'Stop runtime')).toBeDisabled()
    })

    it('disables them during a transition', async () => {
      renderPanel({ project: makeProject({ status: 'starting' }) })
      const menu = await openMenu()

      expect(menuItem(menu, 'Stop runtime')).toBeDisabled()
    })
  })

  describe('destroying a runtime', () => {
    it('asks first, and names what will be lost', async () => {
      const props = renderPanel({ project: running({ id: 'p_abc', name: 'Accounting' }) })
      const menu = await openMenu('Accounting')
      await userEvent.click(menuItem(menu, 'Destroy runtime…'))

      const dialog = screen.getByRole('dialog', { name: /Destroy Accounting/ })
      expect(within(dialog).getByText(/amx-p_abc/)).toBeInTheDocument()
      expect(props.actions.destroyRuntime).not.toHaveBeenCalled()

      await userEvent.click(within(dialog).getByRole('button', { name: 'Destroy runtime' }))
      expect(props.actions.destroyRuntime).toHaveBeenCalledOnce()
    })

    it('cancels on Escape without destroying anything', async () => {
      const props = renderPanel({ project: running() })
      const menu = await openMenu()
      await userEvent.click(menuItem(menu, 'Destroy runtime…'))

      await userEvent.keyboard('{Escape}')
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
      expect(props.actions.destroyRuntime).not.toHaveBeenCalled()
    })
  })

  describe('leaving and entering the workspace', () => {
    it('offers to remove the panel, and not to stop anything', async () => {
      const props = renderPanel({ project: running() })
      const menu = await openMenu()

      // The two are different sentences on purpose: taking a panel off the grid
      // leaves the terminal running, and the label says so.
      expect(menuItem(menu, 'Remove from workspace')).toHaveAttribute(
        'title',
        expect.stringContaining('keeps running'),
      )
      await userEvent.click(menuItem(menu, 'Remove from workspace'))
      expect(props.actions.remove).toHaveBeenCalledOnce()
      expect(props.actions.stopRuntime).not.toHaveBeenCalled()
      expect(props.actions.destroyRuntime).not.toHaveBeenCalled()
    })

    it('offers a focus button in the grid', async () => {
      const props = renderPanel({ project: running({ name: 'Accounting' }) })

      await userEvent.click(screen.getByRole('button', { name: 'Focus Accounting' }))
      expect(props.actions.focus).toHaveBeenCalledOnce()
    })

    it('offers the way back, and no focus button, once focused', async () => {
      const props = renderPanel({ project: running({ name: 'Accounting' }), mode: 'focus' })

      expect(screen.queryByRole('button', { name: 'Focus Accounting' })).not.toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: 'Back to the workspace' }))
      expect(props.actions.leaveFocus).toHaveBeenCalledOnce()
    })

    it('offers full screen when the browser can do it', async () => {
      const props = renderPanel({ project: running({ name: 'A' }), mode: 'focus' })

      await userEvent.click(screen.getByRole('button', { name: 'Full screen' }))
      expect(props.actions.toggleFullscreen).toHaveBeenCalledOnce()
    })

    it('does not offer full screen where the browser cannot do it', async () => {
      const props = renderPanel({
        project: running({ name: 'B' }),
        mode: 'focus',
        canFullscreen: false,
      })

      expect(screen.queryByRole('button', { name: 'Full screen' })).not.toBeInTheDocument()
      const menu = await openMenu('B')
      expect(within(menu).queryByRole('menuitem', { name: 'Full screen' })).not.toBeInTheDocument()
      expect(props.actions.toggleFullscreen).not.toHaveBeenCalled()
    })
  })

  describe('moving within the workspace', () => {
    it('offers a move left when there is something to its left', async () => {
      const props = renderPanel({
        project: running(),
        position: { slot: 1, total: 3, canMoveLeft: true, canMoveRight: true },
      })
      const menu = await openMenu()

      await userEvent.click(menuItem(menu, 'Move left'))
      expect(props.actions.move).toHaveBeenCalledWith('left')
    })

    it('offers a move right when there is something to its right', async () => {
      const props = renderPanel({
        project: running(),
        position: { slot: 1, total: 3, canMoveLeft: true, canMoveRight: true },
      })
      const menu = await openMenu()

      await userEvent.click(menuItem(menu, 'Move right'))
      expect(props.actions.move).toHaveBeenCalledWith('right')
    })

    it('disables the direction with nothing beyond it', async () => {
      renderPanel({
        project: running(),
        position: { slot: 0, total: 3, canMoveLeft: false, canMoveRight: true },
      })
      const menu = await openMenu()

      expect(menuItem(menu, 'Move left')).toBeDisabled()
      expect(menuItem(menu, 'Move right')).toBeEnabled()
    })

    it('names the slot it would pin to, counting from one', async () => {
      const props = renderPanel({
        project: running(),
        position: { slot: 2, total: 3, canMoveLeft: true, canMoveRight: false },
      })
      const menu = await openMenu()

      await userEvent.click(menuItem(menu, 'Pin to slot 3'))
      expect(props.actions.pin).toHaveBeenCalledWith(2)
    })
  })

  describe('when there is no terminal to show', () => {
    it('explains that the server cannot host one, in the server’s own words', () => {
      // The server distinguishes "start me inside WSL" from "install tmux here";
      // the panel must not paraphrase it into a generic failure.
      renderPanel({
        terminalAvailable: false,
        terminalBlocker: 'tmux runtime requires AgentMux Server to run inside WSL.',
      })

      expect(screen.getByText('Terminal runtime unavailable')).toBeInTheDocument()
      expect(
        screen.getByText('tmux runtime requires AgentMux Server to run inside WSL.'),
      ).toBeInTheDocument()
      expect(screen.queryByRole('button', { name: 'Start' })).not.toBeInTheDocument()
    })

    it('says the runtime is stopped rather than drawing an empty terminal', async () => {
      const props = renderPanel({ project: makeProject({ status: 'stopped' }) })

      expect(screen.getByText('Runtime stopped')).toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: 'Start' }))
      expect(props.actions.startRuntime).toHaveBeenCalledOnce()
    })

    it('subscribes to nothing while the runtime is down', () => {
      const terminal = makeClient()
      renderPanel({ project: makeProject({ status: 'stopped' }), terminal })

      expect(terminal.subscribes).toHaveLength(0)
    })

    it('subscribes to nothing when the server cannot host a terminal', () => {
      const terminal = makeClient()
      renderPanel({ terminalAvailable: false, project: running(), terminal })

      expect(terminal.subscribes).toHaveLength(0)
    })
  })

  describe('when there is a terminal', () => {
    it('watches the project, and hands the view a live terminal', () => {
      const terminal = makeClient({ status: makeStatus({ state: 'open' }) })
      renderPanel({ project: running({ id: 'p_live' }), terminal })

      expect(terminal.subscribes.map((call) => call.projectId)).toEqual(['p_live'])
      expect(screen.getByTestId('terminal-screen')).toBeInTheDocument()
    })

    it('announces the connection, because a stalled terminal and a gone server look the same', () => {
      const terminal = makeClient({
        status: makeStatus({ state: 'reconnecting', message: '', attempt: 2 }),
      })
      renderPanel({ project: running(), terminal })

      expect(screen.getByText('Reconnecting… (attempt 2)')).toBeInTheDocument()
    })

    it('says why when it has given up, rather than showing a blank screen', () => {
      const terminal = makeClient({
        status: makeStatus({
          state: 'failed',
          message: 'Could not reach the terminal server.',
          attempt: 5,
        }),
      })
      renderPanel({ project: running(), terminal })

      expect(screen.getByText('Could not reach the terminal server.')).toBeInTheDocument()
    })

    it('refuses typing while the connection is not open', () => {
      const terminal = makeClient({ status: makeStatus({ state: 'connecting' }) })
      renderPanel({ project: running(), terminal })

      expect(screen.getByTestId('terminal-screen')).toHaveAttribute('data-interactive', 'no')
    })

    it('offers a redraw in the menu rather than in a grid panel’s header', async () => {
      const terminal = makeClient({ status: makeStatus({ state: 'open' }) })
      renderPanel({ project: running(), terminal })

      const menu = await openMenu()
      await userEvent.click(menuItem(menu, 'Redraw terminal'))
      // The action reached the subscription, which is what a redraw is.
      expect(terminal.current()?.resync).toHaveBeenCalledOnce()
    })
  })

  describe('the Prompt Bar', () => {
    it('is disabled while this project has no terminal, even if the socket is open', () => {
      const terminal = makeClient({ status: makeStatus({ state: 'open' }) })
      renderPanel({ project: makeProject({ status: 'stopped' }), terminal })

      expect(screen.getByLabelText('Message Claude')).toBeDisabled()
    })

    it('is enabled once there is a live terminal', () => {
      const terminal = makeClient({ status: makeStatus({ state: 'open' }) })
      renderPanel({ project: running(), terminal })

      expect(screen.getByLabelText('Message Claude')).toBeEnabled()
    })
  })

})

describe('ProjectTerminal', () => {
  /**
   * The strip above a terminal is rendered by ProjectTerminal rather than by
   * the panel, so its own suite can drive a session directly.
   */
  function renderTerminal(session: TerminalSession, showRedraw = true) {
    render(
      <TerminalProvider client={makeClient().client}>
        <ProjectTerminal session={session} fontSize={11} showRedraw={showRedraw} />
      </TerminalProvider>,
    )
  }

  it('says nothing when nothing is wrong', () => {
    renderTerminal(makeSession({ status: makeStatus({ state: 'open' }) }).session)

    expect(screen.getByText('Live')).toBeInTheDocument()
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })

  it('leaves the redraw button out when told to', () => {
    renderTerminal(makeSession({ status: makeStatus({ state: 'open' }) }).session, false)

    expect(screen.queryByRole('button', { name: 'Redraw' })).not.toBeInTheDocument()
    expect(screen.getByText('Live')).toBeInTheDocument()
  })

  it('shows the server’s message and offers to ask again when asking could help', async () => {
    // A terminal stopped because it outran the connection is the case this is
    // for: nothing is wrong with the terminal or the project, and asking for it
    // again is exactly the right response.
    const double = makeSession({ error: makeTerminalError(), status: makeStatus() })
    renderTerminal(double.session)

    expect(screen.getByText('The terminal outran the connection.')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Ask again' }))
    expect(double.session.askAgain).toHaveBeenCalledOnce()
  })

  it('shows a message with no way to ask again when asking cannot help', () => {
    // A project the server does not know is not fixed by asking twice.
    const double = makeSession({
      error: makeTerminalError({ code: 'bad_project', message: 'no such project' }),
    })
    renderTerminal(double.session)

    expect(screen.getByText('no such project')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Ask again' })).not.toBeInTheDocument()
  })

  it('says so when the server stopped sending, and offers to ask again', async () => {
    const double = makeSession({ ended: true })
    renderTerminal(double.session)

    expect(screen.getByText('The server stopped sending this terminal.')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Ask again' }))
    expect(double.session.askAgain).toHaveBeenCalledOnce()
  })
})
