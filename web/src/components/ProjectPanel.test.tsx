import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { ProjectPanel } from './ProjectPanel'
import { makeProject } from '../test/fixtures'
import type { Project } from '../api/types'

/**
 * renderPanel renders the panel with the ordinary healthy case, so each test
 * states only the one fact it is about.
 *
 * The default is "this server can host a terminal": a test that wants the
 * unavailable case passes terminalAvailable={false}, which is the failure this
 * panel exists to describe honestly.
 */
function renderPanel(overrides: Partial<Parameters<typeof ProjectPanel>[0]> = {}) {
  const props = {
    project: makeProject(),
    terminalAvailable: true,
    terminalBlocker: '',
    busy: false,
    onStartRuntime: vi.fn(),
    onStopRuntime: vi.fn(),
    ...overrides,
  }
  render(<ProjectPanel {...props} />)
  return props
}

describe('ProjectPanel', () => {
  it('names the project and its status', () => {
    renderPanel({ project: makeProject({ name: 'AgentMux', status: 'stopped' }) })

    expect(screen.getByRole('heading', { name: 'AgentMux' })).toBeInTheDocument()
    expect(screen.getByText('Stopped')).toBeInTheDocument()
  })

  it('says the terminal UI is not here yet rather than drawing an empty one', () => {
    // Phase 2 has a real, persistent runtime and no way to stream it into a
    // browser. A black rectangle that never prints anything would be a lie
    // about what this build can do, and so would silence about why.
    renderPanel()

    expect(screen.getByText('Terminal UI coming in Phase 4')).toBeInTheDocument()
    expect(screen.getByText(/Phase 3/)).toBeInTheDocument()
  })

  it('explains that the runtime is unavailable, in the server’s own words', () => {
    // The server distinguishes "start me inside WSL" from "install tmux here";
    // the panel must not paraphrase it into a generic failure.
    renderPanel({
      terminalAvailable: false,
      terminalBlocker: 'Runtime unavailable: tmux is not installed in linux.',
    })

    expect(screen.getByText('Terminal runtime unavailable')).toBeInTheDocument()
    expect(screen.getByText(/tmux is not installed in linux/)).toBeInTheDocument()
  })

  it('leaves both runtime controls disabled when the server cannot host one', () => {
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

  it('disables both controls while a request is in flight, and during a transition', () => {
    const { unmount } = render(
      <ProjectPanel
        project={makeProject({ status: 'stopped' })}
        terminalAvailable
        terminalBlocker=""
        busy
        onStartRuntime={vi.fn()}
        onStopRuntime={vi.fn()}
      />,
    )
    expect(screen.getByRole('button', { name: 'Start runtime' })).toBeDisabled()
    unmount()

    render(
      <ProjectPanel
        project={makeProject({ status: 'starting' })}
        terminalAvailable
        terminalBlocker=""
        busy={false}
        onStartRuntime={vi.fn()}
        onStopRuntime={vi.fn()}
      />,
    )
    expect(screen.getByRole('button', { name: 'Start runtime' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Stop runtime' })).toBeDisabled()
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

  it('keeps the prompt disabled, since there is nothing to send it to', () => {
    renderPanel()

    expect(screen.getByLabelText('Message Claude')).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Send' })).toBeDisabled()
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
        <ProjectPanel
          project={makeProject({ status })}
          terminalAvailable
          terminalBlocker=""
          busy={false}
          onStartRuntime={vi.fn()}
          onStopRuntime={vi.fn()}
        />,
      )
      expect(screen.getByText(label)).toBeInTheDocument()
      unmount()
    }
  })
})
