import { useEffect, useState } from 'react'

import { TerminalView } from '../components/TerminalView'
import { useTerminalSession } from '../terminal/useTerminal'

/**
 * One project's terminal, watched and never typed into.
 *
 * # What this is
 *
 * A viewer. It subscribes to a project's terminal, draws what arrives, and has
 * no way to send anything back. That is not a restriction bolted onto a terminal
 * component - it is the protocol's default position: a client that never sends
 * `control.request` is a viewer for the life of the connection, which is what
 * §5 of `docs/MULTI_DEVICE.md` means by "viewer by default".
 *
 * So there is nothing here that asks for control, nothing that sends input, and
 * nothing that sends a resize. Four independent things would have to fail before
 * a keystroke could reach the pty, and they are listed in
 * `docs/TERMINAL_VIEWER.md` §3.
 *
 * # Why it does not create a second socket
 *
 * It uses the page's terminal client, which the whole document shares. Opening
 * this viewer and opening the workspace's panel in the same tab share one
 * WebSocket and one client identity - subscriptions are multiplexed, and the
 * frames carry the project they belong to. §3 of the phase brief forbids a
 * second terminal socket and this is how there is not one.
 *
 * # Why a stopped runtime has no terminal in it
 *
 * `running` is false when the project's runtime is down, so nothing is
 * subscribed - the server refuses a subscribe to a stopped project, and a client
 * that tried would spend a reconnect cycle being told so. The xterm instance is
 * not created either: a console showing a hundred stopped projects should not
 * have a hundred terminals in it.
 */
export interface TerminalViewerProps {
  /** The project whose terminal this is. */
  projectID: string

  /**
   * Whether there is a terminal to watch.
   *
   * It comes from the project's runtime status in the dashboard response, which
   * is the same value the card above shows - so a card that says the runtime is
   * stopped and a viewer that shows nothing are never disagreeing.
   */
  running: boolean

  /** The terminal's text size, in pixels. */
  fontSize?: number
}

/** DefaultTerminalFontSize is smaller than the workspace's: this is a card. */
const DefaultTerminalFontSize = 11

export function TerminalViewer({
  projectID,
  running,
  fontSize = DefaultTerminalFontSize,
}: TerminalViewerProps) {
  const session = useTerminalSession(projectID, running)

  // The same "wait a moment before saying it failed" that a spinner needs
  // everywhere: the first frames of a connection take a few milliseconds, and a
  // card that flashed "unable to connect" during them would be wrong more often
  // than right.
  const [settled, setSettled] = useState(false)
  const status = running ? session.status.state : 'idle'
  useEffect(() => {
    if (status !== 'connecting') {
      setSettled(true)
      return
    }
    setSettled(false)
    const timer = setTimeout(() => setSettled(true), 400)
    return () => clearTimeout(timer)
  }, [status])

  if (!running) {
    return <Notice tone="neutral" title="Runtime stopped" detail="Nothing is running in this project." />
  }

  // Two ways a terminal cannot be shown, and one shape for both. The connection
  // itself gave up - which is the client's own news, and the only case a retry
  // helps - or the server refused this project, which carries its own reason.
  if (status === 'failed' || session.error !== null) {
    const detail = session.error?.message || session.status.message || 'The connection is not up.'
    return (
      <Notice
        tone="danger"
        title="Unable to connect terminal"
        detail={detail}
        action={{ label: 'Retry', onClick: session.askAgain }}
      />
    )
  }

  // The server told this connection the subscription is over: the terminal it
  // was watching went away. It is not a failure of the connection, which is why
  // it does not offer a retry - there is nothing to retry until a runtime starts.
  if (session.ended) {
    return <Notice tone="neutral" title="No active terminal" detail="The terminal this was watching has ended." />
  }

  return (
    <div className="terminal-viewer">
      <TerminalView
        session={session}
        // Never. This is the whole of the viewer: no keystroke is accepted by
        // the terminal, and no input handler is even bound to it.
        interactive={false}
        // Never. A viewer draws the screen at the size the server chose; its own
        // box has nothing to do with the pty. The server refuses a viewer's
        // resize anyway - this is the client agreeing with it.
        mayResize={false}
        // And no touch keys either. Each of them calls the session directly, so
        // a terminal that may not be typed into does not draw them - a row of
        // buttons that can never do anything is decoration made of controls,
        // and on a card it is space the terminal needs.
        showKeys={false}
        fontSize={fontSize}
      />
      {!settled && (
        <p className="terminal-viewer__notice" role="status">
          Connecting…
        </p>
      )}
      {settled && (status === 'reconnecting' || status === 'idle') && (
        <p className="terminal-viewer__notice terminal-viewer__notice--warning" role="status">
          Terminal disconnected
        </p>
      )}
    </div>
  )
}

/** Notice is the one shape a viewer shows instead of a terminal. */
function Notice({
  tone,
  title,
  detail,
  action,
}: {
  tone: 'neutral' | 'danger'
  title: string
  detail: string
  action?: { label: string; onClick: () => void }
}) {
  return (
    <div className={`terminal-viewer__placeholder terminal-viewer__placeholder--${tone}`}>
      <p className="terminal-viewer__title">{title}</p>
      <p className="terminal-viewer__detail">{detail}</p>
      {action && (
        <button type="button" className="button button--small" onClick={action.onClick}>
          {action.label}
        </button>
      )}
    </div>
  )
}
