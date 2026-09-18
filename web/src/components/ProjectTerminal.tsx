/**
 * A project's terminal and what has to be said about it.
 *
 * The strip above the terminal is the part that matters. A terminal that has
 * stopped printing and a terminal whose server has gone away look exactly the
 * same - both are a screen that is not changing - and that difference is the
 * only thing a person needs in order to know whether to wait or to do
 * something. So the connection state is stated rather than left to be inferred
 * from the absence of output, in every display mode including the grid.
 *
 * What the grid gives up is the Redraw button, not the state: a panel three
 * hundred pixels tall spends its width on the sentence, and the action moves to
 * the panel's menu where the other occasional actions live.
 */
import { ControlReason, TerminalErrorCode } from '../terminal/protocol'
import type { ControlState, TerminalSession } from '../terminal/useTerminal'
import { TerminalView } from './TerminalView'

/** describeConnection says what the client is doing, in a few words. */
export function describeConnection(session: TerminalSession): string {
  const { state, message, attempt } = session.status
  switch (state) {
    case 'connecting':
      return 'Connecting…'
    case 'open':
      return 'Live'
    case 'reconnecting':
      return attempt > 1 ? `Reconnecting… (attempt ${attempt})` : 'Reconnecting…'
    case 'failed':
      return message || 'Not connected'
    default:
      return 'Not connected'
  }
}

/**
 * describeControl says who is in charge of this terminal, in a few words.
 *
 * It is stated for the same reason the connection state is: a terminal that
 * ignores the keyboard and a terminal that is merely quiet look identical from
 * the outside, and "somebody else is typing into this" is the one fact that
 * tells them apart. A viewer is not a broken client - it is a client without
 * the lease - and saying so is what stops the second person to open a project
 * from concluding that something is wrong.
 *
 * The empty string means the server has not described the project yet, and a
 * bar with nothing to say about control says nothing.
 */
export function describeControl(control: ControlState): string {
  const { view, held, waiting } = control
  if (held) {
    // `viewers` counts everybody watching except this client and the controller,
    // and this client *is* the controller - so it is the number of other people
    // who can see what is being typed.
    const watching = view?.viewers ?? 0
    return watching > 0 ? `You control · ${watching} watching` : 'You control'
  }
  if (waiting) return 'Waiting for control…'
  if (!view) return ''
  if (!view.controller) return 'Viewer · nobody is in control'
  // A suspended controller is the one case where the roster names somebody who
  // is not there, and saying "has control" about a device whose connection has
  // dropped would be the terminal lying about why it will not type.
  if (view.suspended) return `Viewer · ${view.controller.device} is away`
  return `Viewer · ${view.controller.device} has control`
}

/** The tone the control badge is drawn in. */
function controlTone(control: ControlState): string {
  if (control.held) return 'mine'
  if (control.waiting) return 'waiting'
  if (control.view?.suspended) return 'away'
  return control.view?.controller ? 'other' : 'free'
}

interface ControlButton {
  label: string
  /** What the button does, or why it is not offered. */
  title: string
  disabled: boolean
  run: 'request' | 'release'
}

/**
 * controlButton is the single control action on offer, given the roster.
 *
 * One button rather than a pair, because at any moment exactly one of these is
 * the useful thing to do - and a bar with two greyed-out halves is a bar nobody
 * reads.
 *
 * It says "Request control" everywhere it is offered, including against a
 * project nobody holds. The alternative - "Take control" when the lease is
 * free - reads better for about a second and then misleads: the same button,
 * pressed against a project somebody else has just claimed, does not take
 * anything, it asks, and the person who pressed it is left waiting for an
 * answer they did not know they were going to need. One word for one action is
 * worth more than the second of clarity.
 *
 * It is exported because the panel's menu offers the same action for the same
 * reason the Redraw button moves there in the grid, and two copies of this copy
 * would be two places to disagree about what "waiting" is called.
 */
export function controlButton(control: ControlState): ControlButton {
  if (control.held) {
    return {
      label: 'Release control',
      title: 'Stop being the controller. Anyone watching can then ask for it.',
      disabled: false,
      run: 'release',
    }
  }
  if (control.waiting) {
    return {
      label: 'Waiting…',
      title: 'Somebody has control and has been told you asked. They can hand it to you.',
      disabled: true,
      run: 'request',
    }
  }
  if (control.view?.suspended) {
    return {
      label: 'Request control',
      // The grace period belongs to the controller's own connection: it is
      // coming back, or it is not, and either way nothing here can hurry it.
      title: 'The controller lost its connection. Control is held for a short while in case they come back.',
      disabled: true,
      run: 'request',
    }
  }
  if (control.view?.controller) {
    return {
      label: 'Request control',
      title: `${control.view.controller.device} is using this terminal. Asking puts you in line; they decide.`,
      disabled: false,
      run: 'request',
    }
  }
  return {
    label: 'Request control',
    title: 'Nobody is typing into this terminal. Asking for it is granted at once.',
    disabled: false,
    run: 'request',
  }
}

/**
 * isRecoverable reports whether asking again could help.
 *
 * A terminal that was stopped because it outran the connection is the case this
 * exists for: the subscription is gone but nothing is wrong with the terminal
 * or the project, and asking for it again is exactly the right response -
 * particularly once whatever was producing that much output has finished.
 */
function isRecoverable(error: { code: string } | null): boolean {
  if (!error) return false
  return error.code === TerminalErrorCode.streamUnstable || error.code === TerminalErrorCode.internal
}

/**
 * promptBarBlockedReason says why the Prompt Bar cannot be typed into.
 *
 * It is written to finish the sentence the field's placeholder starts, so it is
 * a phrase rather than a sentence: "Message Claude… (only iPad can type)". The
 * remedy is named where this client can perform it, because a person who cannot
 * type does not know whether they are looking at a fault or at somebody else's
 * turn.
 */
export function promptBarBlockedReason(session: TerminalSession): string {
  const { view, waiting } = session.control
  if (waiting) return 'waiting for control'
  if (!view) return 'checking who has control'
  if (!view.controller) return 'request control to type here'
  if (view.suspended) return `only ${view.controller.device} can type; it is away`
  return `only ${view.controller.device} can type`
}

/**
 * describeRefusal says why a request for control was turned down.
 *
 * Only the refusals that leave no trace in the roster are here, because those
 * are the ones nothing else on screen can explain. A request that is queued, or
 * granted, or handed over shows up as the roster changing; a request refused
 * because somebody else is typing leaves the project exactly as it was, and
 * without this the button would appear to have done nothing at all.
 */
function describeRefusal(control: ControlState): string {
  if (control.held || control.waiting) return ''
  switch (control.reason) {
    case ControlReason.controllerExists:
      return 'Somebody else is using this terminal.'
    case ControlReason.tooManyRequests:
      return 'Too many people are already waiting for this terminal.'
    case ControlReason.rejected:
      return 'The controller declined your request.'
    default:
      return ''
  }
}

interface ProjectTerminalProps {
  session: TerminalSession
  /** The terminal's text size, which is the one thing a display mode changes. */
  fontSize: number
  /** False in the grid, where the action lives in the panel's menu instead. */
  showRedraw?: boolean
}

export function ProjectTerminal({ session, fontSize, showRedraw = true }: ProjectTerminalProps) {
  const connected = session.status.state === 'open'
  const busy = session.status.state === 'connecting' || session.status.state === 'reconnecting'

  const control = session.control
  const button = controlButton(control)
  const controlLabel = describeControl(control)
  const refusal = describeRefusal(control)
  // Only the controller is offered the queue, because only the controller can
  // answer it: everybody else is shown the same pending list in a roster they
  // have no way to act on.
  const pending = control.held ? (control.view?.pending ?? []) : []

  const notice = session.ended
    ? 'The server stopped sending this terminal.'
    : session.error
      ? session.error.message
      : ''
  // A refusal is the weakest thing that can be said here, so it gives way to the
  // two that mean the terminal itself is not working.
  const remark = notice !== '' ? notice : refusal

  return (
    <div className="panel__terminal">
      <div className="terminal__bar">
        <span className="terminal__group terminal__group--state">
          <span className={`terminal__state terminal__state--${session.status.state}`}>
            {describeConnection(session)}
          </span>
          {controlLabel !== '' && (
            <span
              className={`terminal__control terminal__control--${controlTone(control)}`}
              data-testid="terminal-control"
            >
              {controlLabel}
            </span>
          )}
        </span>
        <span className="terminal__group terminal__group--actions">
          {connected && (
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
          {showRedraw && (
            <button
              type="button"
              className="button button--small"
              onClick={session.askAgain}
              disabled={busy}
              title="Ask the server to send this terminal again from a fresh screen"
            >
              Redraw
            </button>
          )}
        </span>
      </div>

      {pending.length > 0 && (
        <div className="terminal__requests" role="status">
          {pending.map((request) => (
            <span key={request.clientId} className="terminal__request">
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

      {/* The keyboard, the touch keys and the terminal's size all follow from
          this one value. A viewer that could still drag its own width would
          reflow the pty for whoever is actually typing. */}
      <TerminalView
        session={session}
        interactive={connected && control.held}
        mayResize={connected && control.held}
        fontSize={fontSize}
      />

      {remark !== '' && (
        <div className="terminal__notice" role="status">
          <span>{remark}</span>
          {notice !== '' && (session.ended || isRecoverable(session.error)) && (
            <button type="button" className="button button--small" onClick={session.askAgain}>
              Ask again
            </button>
          )}
        </div>
      )}
    </div>
  )
}
