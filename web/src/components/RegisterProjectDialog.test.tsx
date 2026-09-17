import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { RegisterProjectDialog } from './RegisterProjectDialog'
import { makeCandidate, makeDiscovery } from '../test/fixtures'

function props(overrides: Partial<Parameters<typeof RegisterProjectDialog>[0]> = {}) {
  return {
    discovery: null,
    discoveryLoading: false,
    discoveryError: null,
    busy: false,
    error: null,
    onScan: vi.fn(),
    onCancel: vi.fn(),
    onSubmit: vi.fn(),
    onRegisterCandidate: vi.fn(),
    ...overrides,
  }
}

describe('RegisterProjectDialog', () => {
  it('focuses the path field and scans on open, so the dialog is useful immediately', () => {
    const onScan = vi.fn()
    render(<RegisterProjectDialog {...props({ onScan })} />)

    expect(screen.getByLabelText(/Folder path/)).toHaveFocus()
    expect(onScan).toHaveBeenCalledOnce()
  })

  it('will not submit an empty path', async () => {
    const onSubmit = vi.fn()
    render(<RegisterProjectDialog {...props({ onSubmit })} />)

    expect(screen.getByRole('button', { name: 'Register' })).toBeDisabled()
    await userEvent.click(screen.getByRole('button', { name: 'Register' }))
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it('submits a typed path without a display name unless one was given', async () => {
    const onSubmit = vi.fn()
    render(<RegisterProjectDialog {...props({ onSubmit })} />)

    await userEvent.type(screen.getByLabelText(/Folder path/), 'D:\\AI\\Projects\\App')
    await userEvent.click(screen.getByRole('button', { name: 'Register' }))

    expect(onSubmit).toHaveBeenCalledWith({ hostPath: 'D:\\AI\\Projects\\App' })
  })

  it('normalises a pasted path with trailing whitespace', async () => {
    const onSubmit = vi.fn()
    render(<RegisterProjectDialog {...props({ onSubmit })} />)

    await userEvent.type(screen.getByLabelText(/Folder path/), '  D:\\AI\\Projects\\App  ')
    await userEvent.type(screen.getByLabelText(/Display name/), '  My App  ')
    await userEvent.click(screen.getByRole('button', { name: 'Register' }))

    expect(onSubmit).toHaveBeenCalledWith({ hostPath: 'D:\\AI\\Projects\\App', name: 'My App' })
  })

  it('shows a suggestion under the collection it sits in, which is the model that matters', () => {
    const discovery = makeDiscovery({
      candidates: [
        makeCandidate({
          name: 'AgentMux',
          collectionPath: 'D:\\AI\\Projects\\2026 Apps',
          hostPath: 'D:\\AI\\Projects\\2026 Apps\\AgentMux',
        }),
      ],
    })
    render(<RegisterProjectDialog {...props({ discovery })} />)

    expect(screen.getByText('D:\\AI\\Projects\\2026 Apps')).toBeInTheDocument()
    expect(screen.getByText('└─ AgentMux')).toBeInTheDocument()
  })

  it('says a project sits directly under a root when it has no collection', () => {
    const discovery = makeDiscovery({ candidates: [makeCandidate({ collectionPath: '' })] })
    render(<RegisterProjectDialog {...props({ discovery })} />)

    expect(screen.getByText('Projects Root')).toBeInTheDocument()
  })

  it('marks an already-registered candidate instead of offering it again', () => {
    const discovery = makeDiscovery({
      candidates: [makeCandidate({ name: 'Known', registered: true, projectId: 'p_1' })],
    })
    render(<RegisterProjectDialog {...props({ discovery })} />)

    expect(screen.getByText('Already registered')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Register Known' })).not.toBeInTheDocument()
  })

  it('registers a candidate the user picks', async () => {
    const candidate = makeCandidate({ name: 'NewOne' })
    const onRegisterCandidate = vi.fn()
    render(<RegisterProjectDialog {...props({ discovery: makeDiscovery({ candidates: [candidate] }), onRegisterCandidate })} />)

    await userEvent.click(screen.getByRole('button', { name: 'Register NewOne' }))
    expect(onRegisterCandidate).toHaveBeenCalledWith(candidate)
  })

  it('says a scan found nothing rather than showing an empty list', () => {
    render(<RegisterProjectDialog {...props({ discovery: makeDiscovery({ candidates: [] }) })} />)

    expect(screen.getByText(/Nothing that looks like a project was found/)).toBeInTheDocument()
    expect(screen.getByText(/still register a folder by path/)).toBeInTheDocument()
  })

  it('warns that a truncated scan is an incomplete list', () => {
    render(<RegisterProjectDialog {...props({ discovery: makeDiscovery({ truncated: true }) })} />)

    expect(screen.getByText(/may be incomplete/)).toBeInTheDocument()
  })

  it('reports a failed scan without losing the path field', () => {
    render(<RegisterProjectDialog {...props({ discoveryError: new Error('scan blew up') })} />)

    expect(screen.getByText(/scan blew up/)).toBeInTheDocument()
    expect(screen.getByLabelText(/Folder path/)).toBeEnabled()
  })

  it('can be told to scan again', async () => {
    const onScan = vi.fn()
    render(<RegisterProjectDialog {...props({ onScan })} />)
    onScan.mockClear()

    await userEvent.click(screen.getByRole('button', { name: 'Scan again' }))
    expect(onScan).toHaveBeenCalledOnce()
  })

  it('closes on Escape, which is what a keyboard user expects', async () => {
    const onCancel = vi.fn()
    render(<RegisterProjectDialog {...props({ onCancel })} />)

    await userEvent.keyboard('{Escape}')
    expect(onCancel).toHaveBeenCalledOnce()
  })

  it('stays open on Escape while a registration is in flight', async () => {
    const onCancel = vi.fn()
    render(<RegisterProjectDialog {...props({ busy: true, onCancel })} />)

    await userEvent.keyboard('{Escape}')
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('disables its controls while busy so the register cannot be sent twice', () => {
    render(<RegisterProjectDialog {...props({ busy: true })} />)

    expect(screen.getByLabelText(/Folder path/)).toBeDisabled()
    expect(screen.getByLabelText(/Display name/)).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Registering…' })).toBeDisabled()
  })
})
