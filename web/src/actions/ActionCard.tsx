import type { ActionItem } from '../api/types'
import { StatusBadge } from '../dashboard/StatusBadge'
import { actionPath } from '../dashboard/route'
import { actionGroup, actionStyle, actionTypeLabel } from './actionStyle'

/**
 * One action, as a row.
 *
 * # What it renders, and what it cannot
 *
 * The type, the reason the server wrote, the project it belongs to, and a badge.
 * Every value is read from a named field and passed through the vocabulary
 * module; nothing is spread into the markup and nothing is interpreted. That is
 * §16 of the phase brief, and it is enforced by construction rather than by
 * care: there is no `{...action}` anywhere for a field to arrive through, and
 * the fields that must never be shown - a prompt, a tool input, a transcript, a
 * credential - have no path into this component at all.
 *
 * # Why the whole row is a link
 *
 * The row is the target. A card with a "Details" button in it would be a card
 * with two things to hit, one of which is a word nobody read, and clicking the
 * thing you are looking at is what a person does first. It is an `<a>` rather
 * than an `onClick`, so the address is visible, middle-click works, and the
 * navigation is the page load the console already chose over a client-side swap.
 */
export interface ActionCardProps {
  action: ActionItem
}

/** The glyph a row that needs somebody leads with. It is this build's, not the server's. */
const NEEDS_YOU_GLYPH = '⚠'

export function ActionCard({ action }: ActionCardProps) {
  const group = actionGroup(action.level, action.status)
  const style = actionStyle(action.level, action.status)

  // The project's name, or its id when the server could not resolve one. The id
  // is the fallback rather than a blank: an action nobody can place is an action
  // nobody can go and look at, and the id is what the console's own card is
  // labelled with.
  const project = action.projectName.trim() === '' ? action.projectId : action.projectName

  return (
    <article className={`action-card action-card--${style.tone}`}>
      <a className="action-card__link" href={actionPath(action.id)}>
        <span className="action-card__head">
          {group === 'needs-you' && (
            <span className="action-card__glyph" aria-hidden="true">
              {NEEDS_YOU_GLYPH}
            </span>
          )}
          <span className="action-card__type">{actionTypeLabel(action.type)}</span>
        </span>

        <span className="action-card__reason">{action.reason}</span>

        <span className="action-card__meta">
          <span className="action-card__project" title={action.projectId}>
            {project}
          </span>
          <StatusBadge status={style} />
        </span>
      </a>
    </article>
  )
}
