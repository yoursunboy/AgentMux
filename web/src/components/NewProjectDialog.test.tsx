import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { NewProjectDialog } from './NewProjectDialog'

const ROOTS = ['D:\\AI\\Projects']

function props(overrides: Partial<Parameters<typeof NewProjectDialog>[0]> = {}) {
  return {
    roots: ROOTS,
    busy: false,
    error: null,
    onCancel: vi.fn(),
    onSubmit: vi.fn(),
    ...overrides,
  }
}

describe('NewProjectDialog', () => {
  it('focuses the name field on open', () => {
    render(<NewProjectDialog {...props()} />)
    expect(screen.getByLabelText(/Project name/)).toHaveFocus()
  })

  it('will not submit an empty name', async () => {
    const onSubmit = vi.fn()
    render(<NewProjectDialog {...props({ onSubmit })} />)

    expect(screen.getByRole('button', { name: 'Create' })).toBeDisabled()
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it('refuses a name with a path separator, which is how a name escapes the root', async () => {
    const onSubmit = vi.fn()
    render(<NewProjectDialog {...props({ onSubmit })} />)

    await userEvent.type(screen.getByLabelText(/Project name/), '..\\..\\Windows')
    expect(screen.getByRole('button', { name: 'Create' })).toBeDisabled()

    await userEvent.click(screen.getByRole('button', { name: 'Create' }))
    expect(onSubmit).not.toHaveBeenCalled()
  })

  it('refuses the characters a Windows folder name cannot contain', async () => {
    render(<NewProjectDialog {...props()} />)

    await userEvent.type(screen.getByLabelText(/Project name/), 'Bad:Name')
    expect(screen.getByRole('button', { name: 'Create' })).toBeDisabled()
  })

  it('shows the path it is about to create, so the name alone is not the whole story', async () => {
    render(<NewProjectDialog {...props()} />)

    expect(screen.getByText('-')).toBeInTheDocument()

    await userEvent.type(screen.getByLabelText(/Project name/), 'NewApp')
    expect(screen.getByText('D:\\AI\\Projects\\NewApp')).toBeInTheDocument()

    await userEvent.type(screen.getByLabelText(/Collection/), 'D:\\AI\\Projects\\2026 Group')
    expect(screen.getByText('D:\\AI\\Projects\\2026 Group\\NewApp')).toBeInTheDocument()
  })

  it('submits the collection only when one was given', async () => {
    const onSubmit = vi.fn()
    render(<NewProjectDialog {...props({ onSubmit })} />)

    await userEvent.type(screen.getByLabelText(/Project name/), 'NewApp')
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))

    expect(onSubmit).toHaveBeenCalledWith({ name: 'NewApp', initGit: false })
  })

  it('passes the collection and the git choice when they were given', async () => {
    const onSubmit = vi.fn()
    render(<NewProjectDialog {...props({ onSubmit })} />)

    await userEvent.type(screen.getByLabelText(/Project name/), 'NewApp')
    await userEvent.type(screen.getByLabelText(/Collection/), 'Group')
    await userEvent.click(screen.getByLabelText('Initialize Git'))
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))

    expect(onSubmit).toHaveBeenCalledWith({
      name: 'NewApp',
      collectionPath: 'Group',
      initGit: true,
    })
  })

  it('trims the name rather than creating a folder with trailing spaces', async () => {
    const onSubmit = vi.fn()
    render(<NewProjectDialog {...props({ onSubmit })} />)

    await userEvent.type(screen.getByLabelText(/Project name/), '  NewApp  ')
    await userEvent.click(screen.getByRole('button', { name: 'Create' }))

    expect(onSubmit).toHaveBeenCalledWith({ name: 'NewApp', initGit: false })
  })

  it('closes on Escape, which is what a keyboard user expects', async () => {
    const onCancel = vi.fn()
    render(<NewProjectDialog {...props({ onCancel })} />)

    await userEvent.keyboard('{Escape}')
    expect(onCancel).toHaveBeenCalledOnce()
  })

  it('stays open on Escape while a create is in flight', async () => {
    const onCancel = vi.fn()
    render(<NewProjectDialog {...props({ busy: true, onCancel })} />)

    await userEvent.keyboard('{Escape}')
    expect(onCancel).not.toHaveBeenCalled()
  })

  it('shows the server error in the dialog that caused it', () => {
    render(<NewProjectDialog {...props({ error: new Error('a folder already exists at that path') })} />)
    expect(screen.getByText(/a folder already exists at that path/)).toBeInTheDocument()
  })

  it('disables its controls while busy so the create cannot be sent twice', () => {
    render(<NewProjectDialog {...props({ busy: true })} />)

    expect(screen.getByLabelText(/Project name/)).toBeDisabled()
    expect(screen.getByLabelText(/Collection/)).toBeDisabled()
    expect(screen.getByLabelText('Initialize Git')).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Creating…' })).toBeDisabled()
  })
})
