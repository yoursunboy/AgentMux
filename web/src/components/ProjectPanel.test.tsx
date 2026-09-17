import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { ProjectPanel } from './ProjectPanel'
import { makeProject } from '../test/fixtures'

describe('ProjectPanel', () => {
  it('names the project and its status', () => {
    render(<ProjectPanel project={makeProject({ name: 'AgentMux', status: 'stopped' })} />)

    expect(screen.getByRole('heading', { name: 'AgentMux' })).toBeInTheDocument()
    expect(screen.getByText('Stopped')).toBeInTheDocument()
  })

  it('says the terminal is not available instead of drawing an empty one', () => {
    // Phase 1 has no session runtime. A black rectangle that never prints
    // anything would be a lie about what this build can do.
    render(<ProjectPanel project={makeProject()} />)

    expect(screen.getByText('Terminal not available yet')).toBeInTheDocument()
    expect(screen.getByText(/no session runtime/)).toBeInTheDocument()
  })

  it('shows the session name the project will use, derived from its id', () => {
    render(<ProjectPanel project={makeProject({ id: 'p_abc123', name: 'Renamed By The User' })} />)

    expect(screen.getByText('amx-p_abc123')).toBeInTheDocument()
    expect(screen.queryByText(/amx-Renamed/)).not.toBeInTheDocument()
  })

  it('shows both paths, because the host path is not the runtime path under WSL', () => {
    render(
      <ProjectPanel
        project={makeProject({
          hostPath: 'D:\\AI\\Projects\\App',
          runtimePath: '/mnt/d/AI/Projects/App',
        })}
      />,
    )

    expect(screen.getByText('D:\\AI\\Projects\\App')).toBeInTheDocument()
    expect(screen.getByText('/mnt/d/AI/Projects/App')).toBeInTheDocument()
  })

  it('says Never rather than showing an epoch for a project never opened', () => {
    render(<ProjectPanel project={makeProject({ lastOpenedAt: null })} />)

    const term = screen.getByText('Last opened')
    expect(term.nextElementSibling).toHaveTextContent('Never')
  })

  it('keeps the prompt disabled, since there is nothing to send it to', () => {
    render(<ProjectPanel project={makeProject()} />)

    expect(screen.getByLabelText('Message Claude')).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Send' })).toBeDisabled()
  })
})
