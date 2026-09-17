import { useEffect, useId, useRef, useState } from 'react'

import type { Candidate, DiscoveryResult } from '../api/types'
import { describeError, shortenPath } from '../lib/format'

interface RegisterProjectDialogProps {
  discovery: DiscoveryResult | null
  discoveryLoading: boolean
  discoveryError: Error | null

  busy: boolean
  error: Error | null
  onScan: () => void
  onCancel: () => void
  onSubmit: (input: { hostPath: string; name?: string }) => void
  onRegisterCandidate: (candidate: Candidate) => void
}

/**
 * RegisterProjectDialog adopts a folder that already exists.
 *
 * It offers the discovered candidates and a path field. Discovery is a
 * suggestion list, never a decision: a folder only becomes a project when the
 * user registers it, and a folder the user types in by hand is registered
 * whether or not the scan would have suggested it.
 */
export function RegisterProjectDialog(props: RegisterProjectDialogProps) {
  const {
    discovery,
    discoveryLoading,
    discoveryError,
    busy,
    error,
    onScan,
    onCancel,
    onSubmit,
    onRegisterCandidate,
  } = props

  const [hostPath, setHostPath] = useState('')
  const [name, setName] = useState('')
  const pathId = useId()
  const nameId = useId()
  const pathRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    pathRef.current?.focus()
    // A scan on open is what makes the dialog useful immediately; it costs one
    // bounded filesystem walk.
    onScan()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape' && !busy) onCancel()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [busy, onCancel])

  const trimmedPath = hostPath.trim()
  const canSubmit = trimmedPath !== '' && !busy
  const candidates = discovery?.candidates ?? []

  return (
    <div className="dialog-backdrop" role="presentation">
      <div className="dialog dialog--wide" role="dialog" aria-modal="true" aria-labelledby={`${pathId}-title`}>
        <h2 className="dialog__title" id={`${pathId}-title`}>
          Open / Register Project
        </h2>

        <div className="dialog__form">
          <label className="field" htmlFor={pathId}>
            <span className="field__label">Folder path</span>
            <input
              id={pathId}
              ref={pathRef}
              className="field__input"
              type="text"
              value={hostPath}
              placeholder="D:\AI\Projects\2026 AgentMux\AgentMux"
              onChange={(event) => setHostPath(event.target.value)}
              disabled={busy}
              autoComplete="off"
              spellCheck={false}
            />
            <span className="field__hint">
              The folder must exist and sit inside a configured Projects Root. AgentMux never moves or
              copies it.
            </span>
          </label>

          <label className="field" htmlFor={nameId}>
            <span className="field__label">Display name (optional)</span>
            <input
              id={nameId}
              className="field__input"
              type="text"
              value={name}
              placeholder="Defaults to the folder name"
              onChange={(event) => setName(event.target.value)}
              disabled={busy}
              autoComplete="off"
            />
          </label>

          <div className="manager__section">
            <div className="manager__section-header">
              <h3>Found on this machine</h3>
              <button type="button" className="button button--quiet" onClick={onScan} disabled={busy || discoveryLoading}>
                {discoveryLoading ? 'Scanning…' : 'Scan again'}
              </button>
            </div>

            {discoveryError && <p className="notice notice--error">{describeError(discoveryError)}</p>}

            {discoveryLoading && !discovery && <p className="notice">Scanning…</p>}

            {discovery && candidates.length === 0 && (
              <p className="notice notice--quiet">
                Nothing that looks like a project was found. You can still register a folder by path.
              </p>
            )}

            {candidates.length > 0 && (
              <ul className="candidate-list">
                {candidates.map((candidate) => (
                  <li className="candidate" key={candidate.hostPath}>
                    <div className="candidate__text">
                      {/* The collection is shown as a heading so the nesting the
                          three-level model cares about is visible. */}
                      <span className="candidate__collection">
                        {candidate.collectionPath
                          ? shortenPath(candidate.collectionPath, 40)
                          : 'Projects Root'}
                      </span>
                      <span className="candidate__name" title={candidate.hostPath}>
                        {'\u2514\u2500 '}
                        {candidate.name}
                      </span>
                      <span className="candidate__markers">
                        {candidate.confidence} confidence &middot; {candidate.markers.join(', ') || 'no markers'}
                      </span>
                    </div>
                    {candidate.registered ? (
                      <span className="candidate__badge">Already registered</span>
                    ) : (
                      <button
                        type="button"
                        className="button"
                        // Named per candidate: the footer already has a Register
                        // button, and a screen reader should not hear two
                        // identical ones.
                        aria-label={`Register ${candidate.name}`}
                        disabled={busy}
                        onClick={() => onRegisterCandidate(candidate)}
                      >
                        Register
                      </button>
                    )}
                  </li>
                ))}
              </ul>
            )}

            {discovery && discovery.truncated && (
              <p className="notice notice--warning">
                The scan stopped before it finished, so this list may be incomplete.
              </p>
            )}
          </div>

          {error && <p className="notice notice--error">{describeError(error)}</p>}

          <div className="dialog__actions">
            <button type="button" className="button button--quiet" onClick={onCancel} disabled={busy}>
              Cancel
            </button>
            <button
              type="button"
              className="button button--primary"
              disabled={!canSubmit}
              onClick={() =>
                onSubmit({
                  hostPath: trimmedPath,
                  ...(name.trim() ? { name: name.trim() } : {}),
                })
              }
            >
              {busy ? 'Registering…' : 'Register'}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
