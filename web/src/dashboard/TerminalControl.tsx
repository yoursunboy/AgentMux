/**
 * Who is typing into a card's terminal, and the one button that changes it.
 *
 * # Why the console has this at all
 *
 * A console that could only watch was a console nobody could work from: the
 * person watching a project from a tablet had to walk to the machine that
 * started it in order to answer a question Claude had asked. Watching and
 * typing are the same task interrupted, and the lease already existed to decide
 * which one client may do the second of them.
 *
 * # Why it is not a second control implementation
 *
 * Every word and every decision here comes from `components/ProjectTerminal`,
 * which is where the workspace draws the same bar at panel size: what the badge
 * says, what the button says, whether the button is offered at all, and why a
 * request was refused. The console supplies the layout and nothing else. Two
 * clients that both hold the lease cannot disagree about what to call it,
 * because neither of them is the one deciding what to call it.
 *
 * The badge even reuses the panel's `.terminal__control--{tone}` classes for
 * the same reason - `controlTone`'s mapping is the shared vocabulary, and a
 * second set of colours would be a second answer to what "waiting" looks like.
 *
 * # Why this is not the whole of the panel's bar
 *
 * The connection state is not here. A card already says "Connecting…" and
 * "Terminal disconnected" at the foot of the terminal, which is where a person
 * looking at a card looks; saying it twice would be the same fact in two
 * places, and the second one is not the one that is read.
 */
import {
  controlButton,
  controlTone,
  describeControl,
  describeRefusal,
} from '../components/ProjectTerminal'
import type { TerminalSession } from '../terminal/useTerminal'

export interface TerminalControlProps {
  /** The card's session, which is where the roster and the two calls live. */
  session: TerminalSession
}

export function TerminalControl({ session }: TerminalControlProps) {
  const control = session.control

  // Nothing is drawn until the server has said something. The roster arrives
  // with the subscription's first output frame, so this is one frame of not
  // being there - and a bar that appeared with a guess in it and corrected
  // itself a moment later would be worse than one that appears a frame late.
  if (!control.view) return null

  const button = controlButton(control)
  const refusal = describeRefusal(control)
  // Only the controller is offered the queue, because only the controller can
  // answer it: everybody else is shown the same pending list in a roster they
  // have no way to act on.
  const pending = control.held ? control.view.pending : []

  // A request for control travels on the subscription, so there has to be one.
  // This is not the same question as whether the socket is up: a client whose
  // terminal has ended still has a live connection, and a lease it asked for
  // over a subscription that is gone is a lease on nothing.
  const actionable = session.status.state === 'open' && !session.ended

  return (
    <div className="viewer-control">
      <div className="viewer-control__row">
        <span
          className={`terminal__control terminal__control--${controlTone(control)}`}
          data-testid="terminal-control"
        >
          {describeControl(control)}
        </span>
        {actionable && (
          <button
            type="button"
            className="button button--small"
            onClick={button.run === 'release' ? session.releaseControl : session.requestControl}
            disabled={button.disabled}
            title={button.title}
          >
            {button.label}
          </button>
        )}
      </div>

      {pending.length > 0 && (
        <div className="viewer-control__requests" role="status">
          {pending.map((request) => (
            <span key={request.clientId} className="viewer-control__request">
              <span>{request.device} is asking for control</span>
              <button
                type="button"
                className="button button--small"
                onClick={() => session.acceptControl(request.clientId)}
              >
                Hand over
              </button>
              <button
                type="button"
                className="button button--small"
                onClick={() => session.rejectControl(request.clientId)}
              >
                Decline
              </button>
            </span>
          ))}
        </div>
      )}

      {/* A refusal is the one answer that leaves no trace in the roster - the
          project is left exactly as it was - so without this the button would
          appear to have done nothing at all. */}
      {refusal !== '' && (
        <p className="viewer-control__refusal" role="status">
          {refusal}
        </p>
      )}
    </div>
  )
}
