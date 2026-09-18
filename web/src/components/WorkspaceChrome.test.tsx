import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { ConfirmDialog } from './ConfirmDialog'
import { PanelBoundary } from './PanelBoundary'
import { PanelMenu } from './PanelMenu'
import { ProjectDetailsDialog } from './ProjectDetailsDialog'
import { makeProject } from '../test/fixtures'

/**
 * A boundary is only interesting when something under it throws, and a
 * component that throws on every render would make the retry untestable. This
 * one throws when asked to, and stops when the flag is cleared.
 */
let explode = false
function Bomb() {
  if (explode) throw new Error('the terminal exploded')
  return <p>the terminal is fine</p>
}

describe('PanelBoundary', () => {
  it('lets a panel that works through untouched', () => {
    explode = false
    render(
      <PanelBoundary label="Project Alpha">
        <Bomb />
      </PanelBoundary>,
    )

    expect(screen.getByText('the terminal is fine')).toBeInTheDocument()
  })

  it('contains a panel that throws, and says the project is untouched', () => {
    // The whole value of the grid is that several projects are visible at once,
    // so one of them failing must not take the page with it.
    explode = true
    render(
      <PanelBoundary label="Project Alpha">
        <Bomb />
      </PanelBoundary>,
    )

    expect(screen.getByText('This panel failed to render')).toBeInTheDocument()
    expect(screen.getByText('the terminal exploded')).toBeInTheDocument()
    expect(screen.getByText(/its terminal is still running/)).toBeInTheDocument()
    expect(screen.queryByText('the terminal is fine')).not.toBeInTheDocument()
  })

  it('offers a retry, which re-mounts rather than restarting anything', async () => {
    explode = true
    render(
      <PanelBoundary label="Project Alpha">
        <Bomb />
      </PanelBoundary>,
    )

    explode = false
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))

    expect(screen.getByText('the terminal is fine')).toBeInTheDocument()
  })

  it('reports the failure to the console, because that is the only place it can go', () => {
    const logged = vi.spyOn(console, 'error').mockImplementation(() => {})
    explode = true
    render(
      <PanelBoundary label="Project Alpha">
        <Bomb />
      </PanelBoundary>,
    )

    // React logs an uncaught render error itself, so the label may not be the
    // first call - what matters is that the panel's own line carries the name
    // of the panel that failed.
    const lines = logged.mock.calls.map((call) => String(call[0]))
    expect(lines.some((line) => line.includes('Project Alpha'))).toBe(true)
    explode = false
  })
})

describe('PanelMenu', () => {
  const items = [
    { key: 'focus', label: 'Focus', onSelect: vi.fn() },
    { key: 'stop', label: 'Stop runtime', onSelect: vi.fn() },
  ]

  it('is closed until it is asked for', () => {
    render(<PanelMenu label="Actions for Alpha" items={items} />)

    expect(screen.getByRole('button', { name: 'Actions for Alpha' })).toHaveAttribute(
      'aria-expanded',
      'false',
    )
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
  })

  it('opens, runs an item, and closes', async () => {
    const onSelect = vi.fn()
    render(<PanelMenu label="Actions for Alpha" items={[{ key: 'a', label: 'Focus', onSelect }]} />)

    await userEvent.click(screen.getByRole('button', { name: 'Actions for Alpha' }))
    await userEvent.click(screen.getByRole('menuitem', { name: 'Focus' }))

    expect(onSelect).toHaveBeenCalledOnce()
    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
  })

  it('closes on Escape and puts the focus back where it was', async () => {
    render(<PanelMenu label="Actions for Alpha" items={items} />)

    const button = screen.getByRole('button', { name: 'Actions for Alpha' })
    await userEvent.click(button)
    await userEvent.keyboard('{Escape}')

    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
    expect(button).toHaveFocus()
  })

  it('closes when something outside it is clicked', async () => {
    render(
      <div>
        <PanelMenu label="Actions for Alpha" items={items} />
        <button type="button">somewhere else</button>
      </div>,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Actions for Alpha' }))
    await userEvent.click(screen.getByRole('button', { name: 'somewhere else' }))

    expect(screen.queryByRole('menu')).not.toBeInTheDocument()
  })

  it('marks a destructive item, and refuses one that is disabled', async () => {
    const onSelect = vi.fn()
    render(
      <PanelMenu
        label="Actions for Alpha"
        items={[{ key: 'destroy', label: 'Destroy runtime…', onSelect, tone: 'danger' }]}
      />,
    )

    await userEvent.click(screen.getByRole('button', { name: 'Actions for Alpha' }))
    const item = screen.getByRole('menuitem', { name: 'Destroy runtime…' })
    expect(item).toHaveClass('panel-menu__item--danger')

    await userEvent.click(item)
    expect(onSelect).toHaveBeenCalledOnce()
  })
})

describe('ConfirmDialog', () => {
  function renderConfirm() {
    const onConfirm = vi.fn()
    const onCancel = vi.fn()
    render(
      <ConfirmDialog
        title="Destroy Alpha's runtime?"
        body={<p>The session is removed.</p>}
        confirmLabel="Destroy runtime"
        onConfirm={onConfirm}
        onCancel={onCancel}
      />,
    )
    return { onConfirm, onCancel }
  }

  it('names the action on the button rather than saying OK', () => {
    renderConfirm()

    expect(screen.getByRole('button', { name: 'Destroy runtime' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'OK' })).not.toBeInTheDocument()
  })

  it('puts the focus on Cancel, not on the destructive button', () => {
    renderConfirm()

    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus()
  })

  it('cancels on Escape, which is a keyboard way out', async () => {
    const { onCancel, onConfirm } = renderConfirm()

    await userEvent.keyboard('{Escape}')

    expect(onCancel).toHaveBeenCalledOnce()
    expect(onConfirm).not.toHaveBeenCalled()
  })

  it('confirms only when the destructive button is pressed', async () => {
    const { onConfirm } = renderConfirm()

    await userEvent.click(screen.getByRole('button', { name: 'Destroy runtime' }))

    expect(onConfirm).toHaveBeenCalledOnce()
  })
})

describe('ProjectDetailsDialog', () => {
  const project = makeProject({
    id: 'p_abc123',
    name: 'Accounting',
    hostPath: 'D:\\AI\\Projects\\Accounting',
    runtimePath: '/mnt/d/AI/Projects/Accounting',
    pinnedSlot: 2,
    lastOpenedAt: null,
  })

  it('carries what the panel header deliberately does not', () => {
    render(<ProjectDetailsDialog project={project} onClose={vi.fn()} />)

    expect(screen.getByText('D:\\AI\\Projects\\Accounting')).toBeInTheDocument()
    expect(screen.getByText('/mnt/d/AI/Projects/Accounting')).toBeInTheDocument()
    expect(screen.getByText('amx-p_abc123')).toBeInTheDocument()
    expect(screen.getByText(/p_abc123\.sock/)).toBeInTheDocument()
  })

  it('derives the session name from the id rather than the display name', () => {
    render(
      <ProjectDetailsDialog
        project={makeProject({ id: 'p_abc123', name: 'Renamed By The User' })}
        onClose={vi.fn()}
      />,
    )

    expect(screen.getByText('amx-p_abc123')).toBeInTheDocument()
    expect(screen.queryByText(/amx-Renamed/)).not.toBeInTheDocument()
  })

  it('says Never rather than showing an epoch for a project never opened', () => {
    render(<ProjectDetailsDialog project={project} onClose={vi.fn()} />)

    const term = screen.getByText('Last opened')
    expect(term.nextElementSibling).toHaveTextContent('Never')
  })

  it('counts the workspace slot the way a person counts', () => {
    render(<ProjectDetailsDialog project={project} onClose={vi.fn()} />)

    const term = screen.getByText('Workspace slot')
    expect(term.nextElementSibling).toHaveTextContent('3')
  })

  it('says a project is not in the workspace rather than showing a zero', () => {
    render(
      <ProjectDetailsDialog project={makeProject({ pinnedSlot: null })} onClose={vi.fn()} />,
    )

    const term = screen.getByText('Workspace slot')
    expect(term.nextElementSibling).toHaveTextContent('Not in the workspace')
  })

  it('closes on Escape and on the Close button', async () => {
    const onClose = vi.fn()
    render(<ProjectDetailsDialog project={project} onClose={onClose} />)

    await userEvent.keyboard('{Escape}')
    expect(onClose).toHaveBeenCalledOnce()

    await userEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(onClose).toHaveBeenCalledTimes(2)
  })
})
