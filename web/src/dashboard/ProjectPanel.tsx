import type { ProjectCard as ProjectCardData } from '../api/types'
import { StatusBadge } from './StatusBadge'
import { absentStyle, agentStyle, attentionStyle, runtimeStyle, unavailableStyle } from './status'

/**
 * One project, as a card.
 *
 * # What it renders, and what it cannot
 *
 * Four rows: the terminal, the agent, whether anybody is needed, and how much is
 * waiting. Every value is read from a named field of the card and passed through
 * a status mapping; nothing is spread into the markup and nothing is
 * interpreted.
 *
 * That is §16 of the phase brief, and it is enforced by construction rather than
 * by care. A field the server adds in a later phase is not rendered here,
 * because there is no `{...card}` anywhere for it to arrive through - and the
 * fields that must never be shown, a prompt or a transcript or a credential,
 * have no path into this component at all.
 *
 * # The three ways a row can be empty
 *
 * They look similar and mean different things, so they are rendered
 * differently:
 *
 *	`agent: null`                  nothing has ever run here
 *	`agent.available: false`       the server cannot say
 *	a status this build does not know   the server said something newer than this build
 *
 * The first is ordinary, the second is a deployment fact, and the third is
 * shown rather than hidden. Collapsing them into one blank would make a
 * dashboard that reports "fine" about a server it cannot read.
 */
export interface ProjectPanelProps {
  card: ProjectCardData
}

export function ProjectPanel({ card }: ProjectPanelProps) {
  const runtime = runtimeStyle(card.runtime.status)

  const agent = !card.agent
    ? absentStyle('agent')
    : card.agent.available
      ? agentStyle(card.agent.status ?? '')
      : unavailableStyle('agent state projection')

  const attention = !card.attention
    ? absentStyle('level of attention')
    : card.attention.available
      ? attentionStyle(card.attention.level)
      : unavailableStyle('attention projection')

  // The reason is the server's own short phrase - "permission requested" and the
  // like - and never a quotation from a payload. It is shown beside the badge
  // because "needs you" without a why is a badge somebody has to go and look up.
  const reason = card.attention?.available ? card.attention.reason : undefined

  return (
    <article className="project-card" aria-label={card.name}>
      <h3 className="project-card__name" title={card.id}>
        {card.name}
      </h3>

      <dl className="project-card__fields">
        <dt className="project-card__term">Runtime</dt>
        <dd className="project-card__value">
          <StatusBadge status={runtime} />
        </dd>

        <dt className="project-card__term">Agent</dt>
        <dd className="project-card__value">
          <StatusBadge status={agent} />
        </dd>

        <dt className="project-card__term">Attention</dt>
        <dd className="project-card__value">
          <StatusBadge status={attention} />
          {reason !== undefined && reason !== '' && (
            <span className="project-card__reason">{reason}</span>
          )}
        </dd>

        <dt className="project-card__term">Actions</dt>
        <dd className="project-card__value">
          {card.actions.available ? (
            <span
              className={
                card.actions.pending > 0
                  ? 'project-card__count project-card__count--pending'
                  : 'project-card__count'
              }
              title={
                card.actions.pending === 0
                  ? 'Nothing is waiting in this project.'
                  : `${card.actions.pending} ${card.actions.pending === 1 ? 'action is' : 'actions are'} waiting.`
              }
            >
              {card.actions.pending}
            </span>
          ) : (
            <StatusBadge status={unavailableStyle('action queue')} />
          )}
        </dd>
      </dl>
    </article>
  )
}
