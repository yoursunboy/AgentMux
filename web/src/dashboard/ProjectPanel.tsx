import type { ProjectCard as ProjectCardData } from '../api/types'
import { StatusBadge } from './StatusBadge'
import { TerminalViewer } from './TerminalViewer'
import { absentStyle, agentStyle, attentionStyle, runtimeStyle, unavailableStyle } from './status'

/**
 * One project, as a card.
 *
 * # What it renders, and what it cannot
 *
 * Two status rows, the project's terminal, and two more status rows — the
 * runtime and the agent above the screen, attention and its action count below
 * it. Every value is read from a named field of the card and passed through a
 * status mapping; nothing is spread into the markup and nothing is
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

  // Whether there is a terminal to watch. It is the same value the Runtime row
  // shows, so a card cannot say the runtime is stopped and show a live terminal
  // at the same time.
  const running = card.runtime.status === 'running'

  return (
    <article className="project-card" aria-label={card.name}>
      <h3 className="project-card__name" title={card.id}>
        {card.name}
      </h3>

      {/* What the project is, above the screen: the two facts a person reads
          first, and the two the terminal below them is an answer to. */}
      <dl className="project-card__fields project-card__fields--head">
        <dt className="project-card__term">Runtime</dt>
        <dd className="project-card__value">
          <StatusBadge status={runtime} />
        </dd>

        <dt className="project-card__term">Agent</dt>
        <dd className="project-card__value">
          <StatusBadge status={agent} />
        </dd>
      </dl>

      {/* The screen. It takes whatever height is left rather than a fixed one,
          so a card is as tall as its panel and the terminal inside it is as tall
          as the card. §12 of the phase brief asks for flex rather than a
          pixel height, and the reason is rows: a grid that stretched its cards
          to a number would be a grid that clipped one. */}
      <div className="project-card__screen">
        <TerminalViewer projectID={card.id} running={running} />
      </div>

      {/* What it needs, below the screen - the reason somebody is looking at the
          card in the first place. */}
      <dl className="project-card__fields project-card__fields--foot">
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
