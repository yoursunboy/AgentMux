import type { ProjectCard as ProjectCardData } from '../api/types'
import { pendingSentence } from '../actions/actionStyle'
import { useTerminalSession } from '../terminal/useTerminal'
import { ModeSwitch } from './ModeSwitch'
import { PermissionMenu } from './PermissionMenu'
import { StatusBadge } from './StatusBadge'
import { TerminalViewer } from './TerminalViewer'
import { ACTIONS_PATH } from './route'
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
 *
 * # The two controls, and why they are named the way they are
 *
 * The title's `Permission` chooses how much the project's *next* Claude asks
 * before it acts; the Runtime row's `Mode` sends Shift+Tab to the one running
 * now. They are named differently because they do different things, and renaming
 * either to match the other would make the card claim that a keystroke is a
 * setting or that a setting takes effect the moment it is saved. See
 * `PermissionMenu`, and `docs/PERMISSION_MODE.md` for the long form.
 */
export interface ProjectPanelProps {
  card: ProjectCardData
  /**
   * Called after this card changes the project's launch settings, so the page
   * can re-read the dashboard rather than let one card hold a newer truth than
   * its neighbours.
   *
   * It is optional because a card renders without a page behind it - the unit
   * tests mount one - and a card whose setting changed is still a correct card.
   */
  onSettingsChanged?: (() => void) | undefined
}

export function ProjectPanel({ card, onSettingsChanged }: ProjectPanelProps) {
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

  // The card owns the session, and the screen below renders it. It is created
  // here rather than inside `TerminalViewer` because the Runtime row's mode
  // switch drives the same terminal, and two `useTerminalSession` calls would be
  // two subscriptions to one project and two claims on one lease.
  //
  // `mayResize: false` is the console's rule rather than this card's: a browser
  // watching a project from a tablet must not reflow the pty of whoever is
  // working in the workspace, and it does not need to - a viewer's screen
  // scrolls to reach what does not fit. `docs/TERMINAL_CONTROLLER.md` §7 is the
  // long form.
  const session = useTerminalSession(card.id, running, { mayResize: false })

  return (
    <article className="project-card" aria-label={card.name}>
      {/* The name and the one setting that belongs to the whole project. The
          button sits here rather than in the fields below because that is what
          it configures: not this attempt, not this session, but every future
          launch of this project. */}
      <div className="project-card__header">
        <h3 className="project-card__name" title={card.id}>
          {card.name}
        </h3>
        <PermissionMenu
          projectId={card.id}
          mode={card.settings.permissionMode}
          available={card.settings.available}
          onChanged={onSettingsChanged}
        />
      </div>

      {/* What the project is, above the screen: the two facts a person reads
          first, and the two the terminal below them is an answer to. */}
      <dl className="project-card__fields project-card__fields--head">
        <dt className="project-card__term">Runtime</dt>
        <dd className="project-card__value">
          <StatusBadge status={runtime} />
          {/* The keyboard's Shift+Tab, for a tablet that has neither key. It is
              drawn only when there is a terminal to send it to, so a stopped
              card offers nothing rather than a button that cannot work. */}
          {running && <ModeSwitch session={session} />}
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
        <TerminalViewer session={session} running={running} />
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
            card.actions.pending > 0 ? (
              // §8 of the phase brief: a count that is a fact becomes a count
              // that is a way in. It links to the queue rather than to one
              // action because a card carries a number, not an id - and it is
              // an `<a>`, so the destination is visible before it is clicked.
              //
              // It keeps the tone class the plain count had, so a card with
              // something waiting is still the card that draws the eye.
              <a
                className="project-card__action-link"
                href={ACTIONS_PATH}
                title={`${pendingSentence(card.actions.pending)} The queue is at /actions; the answer is given at the terminal.`}
              >
                <span className="project-card__glyph" aria-hidden="true">
                  {'⚠'}
                </span>
                {pendingSentence(card.actions.pending)}
              </a>
            ) : (
              <span className="project-card__count" title="Nothing is waiting in this project.">
                {card.actions.pending}
              </span>
            )
          ) : (
            <StatusBadge status={unavailableStyle('action queue')} />
          )}
        </dd>
      </dl>
    </article>
  )
}
