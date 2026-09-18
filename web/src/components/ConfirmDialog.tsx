/**
 * A confirmation for the one action in the workspace that cannot be undone.
 *
 * Stopping a runtime is not destructive - the terminal stays, with its
 * scrollback, and starting it again resumes in the same place - so it does not
 * ask. Destroying one removes the session and everything in it, and that is a
 * different sentence to read before it happens rather than after.
 *
 * The dialog is a real one: Escape closes it, the buttons are reachable by
 * keyboard, focus goes to the cancel button rather than the destructive one,
 * and the destructive button says what it does rather than "OK".
 */
import { useEffect, useRef, type ReactNode } from 'react'

interface ConfirmDialogProps {
  title: string
  /** What will happen, in the server's or the caller's own words. */
  body: ReactNode
  /** The label on the button that goes ahead. It names the action. */
  confirmLabel: string
  onConfirm: () => void
  onCancel: () => void
  busy?: boolean
}

export function ConfirmDialog({
  title,
  body,
  confirmLabel,
  onConfirm,
  onCancel,
  busy = false,
}: ConfirmDialogProps) {
  const cancelRef = useRef<HTMLButtonElement | null>(null)

  useEffect(() => {
    cancelRef.current?.focus()
    const onKeyDown = (event: KeyboardEvent): void => {
      if (event.key === 'Escape') onCancel()
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [onCancel])

  return (
    <div className="dialog-backdrop" onMouseDown={(event) => event.target === event.currentTarget && onCancel()}>
      <div className="dialog" role="dialog" aria-modal="true" aria-label={title}>
        <h2 className="dialog__title">{title}</h2>
        <div className="dialog__body">{body}</div>
        <div className="dialog__actions">
          <button type="button" className="button" ref={cancelRef} onClick={onCancel} disabled={busy}>
            Cancel
          </button>
          <button
            type="button"
            className="button button--danger"
            onClick={onConfirm}
            disabled={busy}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  )
}
