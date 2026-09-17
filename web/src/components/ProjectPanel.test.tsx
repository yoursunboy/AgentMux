import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { ProjectPanel } from './ProjectPanel'
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
  const props = {
    project: options.project ?? makeProject(),
    terminalAvailable: options.terminalAvailable ?? true,
    terminalBlocker: options.terminalBlocker ?? '',
    busy: options.busy ?? false,
    onStartRuntime: vi.fn(),
    onStopRuntime: vi.fn(),
  }
  const terminal = options.terminal ?? makeClient()
  render(
    <TerminalProvider client={terminal.client}>
      <ProjectPanel {...props} />
    </TerminalProvider>,
  )
  return { ...props, terminal }
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

    it('shows the session name the project will use, derived from its id', () => {
      renderPanel({ project: makeProject({ id: 'p_abc123', name: 'Renamed By The User' }) })

      expect(screen.getByText('amx-p_abc123')).toBeInTheDocument()
      expect(screen.queryByText(/amx-Renamed/)).not.toBeInTheDocument()
    })

    it('shows both paths, because the host path is not the runtime path under WSL', () => {
      renderPanel({
        project: makeProject({
          hostPath: 'D:\\AI\\Projects\\App',
          runtimePath: '/mnt/d/AI/Projects/App',
        }),
      })

      expect(screen.getByText('D:\\AI\\Projects\\App')).toBeInTheDocument()
      expect(screen.getByText('/mnt/d/AI/Projects/App')).toBeInTheDocument()
    })

    it('says Never rather than showing an epoch for a project never opened', () => {
      renderPanel({ project: makeProject({ lastOpenedAt: null }) })

      const term = screen.getByText('Last opened')
      expect(term.nextElementSibling).toHaveTextContent('Never')
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
              onStartRuntime={vi.fn()}
              onStopRuntime={vi.fn()}
            />
          </TerminalProvider>,
        )
        expect(screen.getByText(label)).toBeInTheDocument()
        unmount()
      }
    })
  })

  describe('the runtime controls', () => {
    it('leaves both disabled when the server cannot host one', () => {
      renderPanel({ terminalAvailable: false })

      expect(screen.getByRole('button', { name: 'Start runtime' })).toBeDisabled()
      expect(screen.getByRole('button', { name: 'Stop runtime' })).toBeDisabled()
    })

    it('offers a start, and no stop, while the runtime is stopped', async () => {
      const props = renderPanel({ project: makeProject({ status: 'stopped' }) })

      const start = screen.getByRole('button', { name: 'Start runtime' })
      expect(start).toBeEnabled()
      expect(screen.getByRole('button', { name: 'Stop runtime' })).toBeDisabled()

      await userEvent.click(start)
      expect(props.onStartRuntime).toHaveBeenCalledOnce()
      expect(props.onStopRuntime).not.toHaveBeenCalled()
    })

    it('offers a stop, and no start, while the runtime is running', async () => {
      // `running` and `reconnecting` are both up: the session outlives the
      // server, so a runtime whose output is not being read yet is still running.
      const props = renderPanel({ project: makeProject({ status: 'reconnecting' }) })

      expect(screen.getByRole('button', { name: 'Start runtime' })).toBeDisabled()
      const stop = screen.getByRole('button', { name: 'Stop runtime' })
      expect(stop).toBeEnabled()

      await userEvent.click(stop)
      expect(props.onStopRuntime).toHaveBeenCalledOnce()
    })

    it('disables both while a request is in flight, and during a transition', () => {
      const { unmount } = render(
        <TerminalProvider client={makeClient().client}>
          <ProjectPanel
            project={makeProject({ status: 'stopped' })}
            terminalAvailable
            terminalBlocker=""
            busy
            onStartRuntime={vi.fn()}
            onStopRuntime={vi.fn()}
          />
        </TerminalProvider>,
      )
      expect(screen.getByRole('button', { name: 'Start runtime' })).toBeDisabled()
      unmount()

      render(
        <TerminalProvider client={makeClient().client}>
          <ProjectPanel
            project={makeProject({ status: 'starting' })}
            terminalAvailable
            terminalBlocker=""
            busy={false}
            onStartRuntime={vi.fn()}
            onStopRuntime={vi.fn()}
          />
        </TerminalProvider>,
      )
      expect(screen.getByRole('button', { name: 'Start runtime' })).toBeDisabled()
      expect(screen.getByRole('button', { name: 'Stop runtime' })).toBeDisabled()
    })
  })

  describe('when there is no terminal to show', () => {
    it('explains that the server cannot host one, in the server’s own words', () => {
      // The server distinguishes "start me inside WSL" from "install tmux here";
      // the panel must not paraphrase it into a generic failure.
      renderPanel({
        terminalAvailable: false,
        terminalBlocker: 'Runtime unavailable: tmux is not installed in linux.',
      })

      expect(screen.getByText('Terminal runtime unavailable')).toBeInTheDocument()
      expect(screen.getByText(/tmux is not installed in linux/)).toBeInTheDocument()
    })

    it('says the terminal is stopped rather than drawing an empty one', () => {
      // A black rectangle that never prints anything would be a lie about what
      // is happening, and so would silence about why.
      renderPanel({ project: makeProject({ status: 'stopped' }) })

      expect(screen.getByText('This project’s terminal is stopped')).toBeInTheDocument()
      expect(screen.queryByTestId('terminal-screen')).not.toBeInTheDocument()
    })

    it('subscribes to nothing while the runtime is down', () => {
      // The server refuses a terminal for a project that is not running, and a
      // client that subscribed anyway would spend a reconnect cycle being told
      // so.
      const { terminal } = renderPanel({ project: makeProject({ status: 'stopped' }) })

      expect(terminal.subscribes).toHaveLength(0)
    })

    it('subscribes to nothing when the server cannot host a terminal', () => {
      const { terminal } = renderPanel({ terminalAvailable: false, project: running() })

      expect(terminal.subscribes).toHaveLength(0)
    })
  })

  describe('when there is a terminal', () => {
    it('watches the project, and hands the view a live terminal', () => {
      const { terminal } = renderPanel({ project: running({ id: 'p_abc' }) })

      expect(terminal.subscribes).toHaveLength(1)
      expect(terminal.subscribes[0]!.projectId).toBe('p_abc')
      expect(screen.getByTestId('terminal-screen')).toHaveAttribute('data-interactive', 'yes')
    })

    it('announces the connection, because a stalled terminal and a gone server look the same', () => {
      const cases: Array<[ReturnType<typeof makeStatus>, string]> = [
        [makeStatus({ state: 'connecting' }), 'Connecting…'],
        [makeStatus({ state: 'open' }), 'Live'],
        [makeStatus({ state: 'reconnecting', attempt: 3 }), 'Reconnecting… (attempt 3)'],
      ]

      for (const [status, label] of cases) {
        const { unmount } = render(
          <TerminalProvider client={makeClient({ status }).client}>
            <ProjectPanel
              project={running()}
              terminalAvailable
              terminalBlocker=""
              busy={false}
              onStartRuntime={vi.fn()}
              onStopRuntime={vi.fn()}
            />
          </TerminalProvider>,
        )
        expect(screen.getByText(label)).toBeInTheDocument()
        unmount()
      }
    })

    it('says why when it has given up, rather than showing a blank screen', () => {
      renderPanel({
        project: running(),
        terminal: makeClient({
          status: makeStatus({
            state: 'failed',
            message: 'Could not reach the terminal server after 5 attempts.',
          }),
        }),
      })

      expect(screen.getByText(/Could not reach the terminal server/)).toBeInTheDocument()
    })

    it('refuses typing while the connection is not open', () => {
      renderPanel({
        project: running(),
        terminal: makeClient({ status: makeStatus({ state: 'reconnecting', attempt: 1 }) }),
      })

      expect(screen.getByTestId('terminal-screen')).toHaveAttribute('data-interactive', 'no')
    })

    it('offers a redraw, and does not offer it while it is already trying', async () => {
      const client = makeClient({ status: makeStatus({ state: 'reconnecting', attempt: 1 }) })
      const { unmount } = render(
        <TerminalProvider client={client.client}>
          <ProjectPanel
            project={running()}
            terminalAvailable
            terminalBlocker=""
            busy={false}
            onStartRuntime={vi.fn()}
            onStopRuntime={vi.fn()}
          />
        </TerminalProvider>,
      )

      const redraw = screen.getByRole('button', { name: 'Redraw' })
      expect(redraw).toBeDisabled()
      unmount()

      render(
        <TerminalProvider client={makeClient().client}>
          <ProjectPanel
            project={running()}
            terminalAvailable
            terminalBlocker=""
            busy={false}
            onStartRuntime={vi.fn()}
            onStopRuntime={vi.fn()}
          />
        </TerminalProvider>,
      )
      expect(screen.getByRole('button', { name: 'Redraw' })).toBeEnabled()
    })
  })

  describe('the Prompt Bar', () => {
    it('is disabled while this project has no terminal, even if the socket is open', () => {
      // One socket serves every project, so an open connection says nothing
      // about whether this one has a terminal. Accepting the text would swallow
      // it.
      renderPanel({ project: makeProject({ status: 'stopped' }) })

      expect(screen.getByLabelText('Message Claude')).toBeDisabled()
      expect(screen.getByRole('button', { name: 'Send' })).toBeDisabled()
    })

    it('is enabled once there is a live terminal', () => {
      renderPanel({ project: running() })

      expect(screen.getByLabelText('Message Claude')).toBeEnabled()
    })
  })

  describe('a failure the subscription reports', () => {
    it('shows the server’s message and offers to ask again when asking could help', async () => {
      // A terminal stopped because it outran the connection is the case this is
      // for: nothing is wrong with the terminal or the project, and asking for
      // it again is exactly the right response.
      const double = makeSession({ error: makeTerminalError(), status: makeStatus() })
      render(<ProjectTerminalHarness session={double.session} />)

      expect(screen.getByText('The terminal outran the connection.')).toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: 'Ask again' }))

      expect(double.session.askAgain).toHaveBeenCalledOnce()
    })

    it('shows a message with no way to ask again when asking cannot help', () => {
      // A project the server does not know is not fixed by asking twice.
      const double = makeSession({
        error: makeTerminalError({ code: 'bad_project', message: 'no such project' }),
      })
      render(<ProjectTerminalHarness session={double.session} />)

      expect(screen.getByText('no such project')).toBeInTheDocument()
      expect(screen.queryByRole('button', { name: 'Ask again' })).not.toBeInTheDocument()
    })

    it('says so when the server stopped sending, and offers to ask again', async () => {
      const double = makeSession({ ended: true })
      render(<ProjectTerminalHarness session={double.session} />)

      expect(screen.getByText('The server stopped sending this terminal.')).toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: 'Ask again' }))

      expect(double.session.askAgain).toHaveBeenCalledOnce()
    })

    it('says nothing when nothing is wrong', () => {
      const double = makeSession()
      render(<ProjectTerminalHarness session={double.session} />)

      expect(screen.queryByRole('status')).not.toBeInTheDocument()
    })
  })
})

/**
 * ProjectTerminalHarness renders the terminal chrome around a session a test
 * built, so that a state the real client reaches only after several events can
 * be stated directly.
 */
function ProjectTerminalHarness({ session }: { session: TerminalSession }) {
  return (
    <TerminalProvider client={makeClient().client}>
      <ProjectTerminal session={session} />
    </TerminalProvider>
  )
}
