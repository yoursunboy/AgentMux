import type { StatusStyle } from './status'

/**
 * One status, as a word and a dot.
 *
 * It is the only thing in the console that renders a status, so there is one
 * place a tone becomes a class name and one place to change if the palette or
 * the markup moves. It knows nothing about which status it is showing: it is
 * handed a `StatusStyle` and renders it, which is what keeps the mapping in
 * `status.ts` rather than spread across the panels.
 */
export interface StatusBadgeProps {
  /** The status, already resolved to a tone and a label. */
  status: StatusStyle
}

export function StatusBadge({ status }: StatusBadgeProps) {
  return (
    <span className={`status-badge status-badge--${status.tone}`} title={status.detail}>
      <span className="status-badge__dot" aria-hidden="true">
        {'●'}
      </span>
      <span className="status-badge__label">{status.label}</span>
    </span>
  )
}
