import { useEffect, useId, useMemo, useRef, useState } from 'react'

import { describeError } from '../lib/format'

interface NewProjectDialogProps {
  /** The Projects Roots, in order. The first is the default. */
  roots: string[]

  busy: boolean
  error: Error | null
  onCancel: () => void
  onSubmit: (input: { name: string; collectionPath?: string; initGit: boolean }) => void
}

/**
 * NewProjectDialog creates a project directory.
 *
 * The dialog shows the final path it is about to create, because the
 * collection layer means the name alone does not say where the folder lands,
 * and the server - not the browser - decides that path.
 */
export function NewProjectDialog({ roots, busy, error, onCancel, onSubmit }: NewProjectDialogProps) {
  const [name, setName] = useState('')
  const [collection, setCollection] = useState('')
  const [initGit, setInitGit] = useState(false)
  const nameId = useId()
  const collectionId = useId()
  const gitId = useId()
  const nameRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    nameRef.current?.focus()
  }, [])

  // Escape closes the dialog, which is what a keyboard user expects.
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape' && !busy) onCancel()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [busy, onCancel])

  const trimmedName = name.trim()
  const trimmedCollection = collection.trim()

  // A preview only. The server validates and creates; this exists so the user
  // can see which folder the name lands in before committing.
  const finalPath = useMemo(() => {
    if (!trimmedName) return ''
    const separator = (trimmedCollection || roots[0] || '').includes('\\') ? '\\' : '/'
    const parent = trimmedCollection || roots[0] || ''
    return parent ? `${parent.replace(/[\\/]+$/, '')}${separator}${trimmedName}` : trimmedName
  }, [trimmedName, trimmedCollection, roots])

  const nameLooksRejected = trimmedName !== '' && /[\\/:*?"<>|]/.test(trimmedName)
  const canSubmit = trimmedName !== '' && !nameLooksRejected && !busy

  return (
    <div className="dialog-backdrop" role="presentation">
      <div className="dialog" role="dialog" aria-modal="true" aria-labelledby={`${nameId}-title`}>
        <h2 className="dialog__title" id={`${nameId}-title`}>
          New Project
        </h2>

        <form
          className="dialog__form"
          onSubmit={(event) => {
            event.preventDefault()
            if (!canSubmit) return
            onSubmit({
              name: trimmedName,
              ...(trimmedCollection ? { collectionPath: trimmedCollection } : {}),
              initGit,
            })
          }}
        >
          <label className="field" htmlFor={collectionId}>
            <span className="field__label">Collection (optional)</span>
            <input
              id={collectionId}
              className="field__input"
              type="text"
              list="agentmux-roots"
              value={collection}
              placeholder={roots[0] ?? 'Projects Root'}
              onChange={(event) => setCollection(event.target.value)}
              disabled={busy}
            />
            <datalist id="agentmux-roots">
              {roots.map((root) => (
                <option value={root} key={root} />
              ))}
            </datalist>
            <span className="field__hint">
              The group folder to create the project inside. Leave empty to create it directly under
              the Projects Root.
            </span>
          </label>

          <label className="field" htmlFor={nameId}>
            <span className="field__label">Project name</span>
            <input
              id={nameId}
              ref={nameRef}
              className="field__input"
              type="text"
              value={name}
              onChange={(event) => setName(event.target.value)}
              disabled={busy}
              autoComplete="off"
              spellCheck={false}
            />
            <span className="field__hint">
              Used as the folder name. Path separators, and the characters {'\\ / : * ? " < > |'}, are
              not allowed.
            </span>
          </label>

          <div className="field">
            <span className="field__label">Final path</span>
            <code className="field__readonly">{finalPath || '-'}</code>
            <span className="field__hint">
              The server creates this folder. It refuses if the folder already exists and is not empty.
            </span>
          </div>

          <label className="checkbox" htmlFor={gitId}>
            <input
              id={gitId}
              type="checkbox"
              checked={initGit}
              onChange={(event) => setInitGit(event.target.checked)}
              disabled={busy}
            />
            <span>Initialize Git</span>
          </label>

          {error && <p className="notice notice--error">{describeError(error)}</p>}

          <div className="dialog__actions">
            <button type="button" className="button button--quiet" onClick={onCancel} disabled={busy}>
              Cancel
            </button>
            <button type="submit" className="button button--primary" disabled={!canSubmit}>
              {busy ? 'Creating…' : 'Create'}
            </button>
          </div>
        </form>
      </div>
    </div>
  )
}
