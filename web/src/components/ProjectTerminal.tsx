/**
 * A project's terminal and everything that has to be said about it.
 *
 * The bar above the terminal is the part that matters. A terminal that has
 * stopped printing and a terminal whose server has gone away look exactly the
 * same - both are a screen that is not changing - and the difference is the
 * only thing a person needs in order to know whether to wait or to do
 * something. So the connection state is stated rather than left to be inferred
 * from the absence of output.
 */
import { TerminalErrorCode } from '../terminal/protocol'
import type { TerminalSession } from '../terminal/useTerminal'
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

export function ProjectTerminal({ session }: { session: TerminalSession }) {
  const connected = session.status.state === 'open'
  const busy = session.status.state === 'connecting' || session.status.state === 'reconnecting'
  const notice = session.ended
    ? 'The server stopped sending this terminal.'
    : session.error
      ? session.error.message
      : ''

  return (
    <div className="panel__terminal">
      <div className="terminal__bar">
        <span className={`terminal__state terminal__state--${session.status.state}`}>
          {describeConnection(session)}
        </span>
        <button
          type="button"
          className="button button--small"
          onClick={session.askAgain}
          disabled={busy}
          title="Ask the server to send this terminal again from a fresh screen"
        >
          Redraw
        </button>
      </div>

      <TerminalView session={session} interactive={connected} />

      {notice !== '' && (
        <div className="terminal__notice" role="status">
          <span>{notice}</span>
          {(session.ended || isRecoverable(session.error)) && (
            <button type="button" className="button button--small" onClick={session.askAgain}>
              Ask again
            </button>
          )}
        </div>
      )}
    </div>
  )
}
