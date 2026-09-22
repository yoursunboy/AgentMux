import { useEffect, useState } from 'react'

import { TerminalView } from '../components/TerminalView'
import { useTerminalSession } from '../terminal/useTerminal'
import { TerminalControl } from './TerminalControl'

/**
 * One project's terminal, watched by default and typed into by one client.
 *
 * # What this is
 *
 * The console's terminal. It subscribes to a project, draws what arrives, and
 * asks for the lease before it accepts a keystroke - which is the protocol's
 * own position rather than a rule bolted onto a terminal component: a client
 * that never sends `control.request` is a viewer for the life of the
 * connection, which is what §5 of `docs/MULTI_DEVICE.md` means by "viewer by
 * default". Opening a card changes nothing on its own. It is the button that
 * does, and pressing it makes this client the one client the server will accept
 * input from.
 *
 * The four independent things that have to be true before a keystroke reaches
 * the pty are listed in `docs/TERMINAL_CONTROLLER.md` §4. The first of them -
 * that this client holds the lease at all - is what `interactive` below is.
 *
 * # Why it does not create a second socket
 *
 * It uses the page's terminal client, which the whole document shares. Opening
 * this viewer and opening the workspace's panel in the same tab share one
 * WebSocket and one client identity - subscriptions are multiplexed, and the
 * frames carry the project they belong to. §3 of the phase brief forbids a
 * second terminal socket and this is how there is not one.
 *
 * # What it types into, and what it does not
 *
 * `interactive` follows the lease; `mayResize` does not. A console may type but
 * never reshapes the shared pty - the browser that is watching a project from a
 * tablet must not reflow the terminal of whoever is working in the workspace,
 * and it does not need to: a viewer's screen scrolls to reach what does not
 * fit, which is the case `docs/TERMINAL_VIEWER.md` §5 describes. The two are
 * separate arguments on `TerminalView` for exactly this split, and the server
 * checks them with separate methods.
 *
 * # Why the touch keys follow the lease
 *
 * The workspace draws them always and disables them for a viewer. A card cannot
 * afford that: it is a few hundred pixels tall, and a row of buttons that can
 * never do anything is decoration made of controls, taking the space the
 * terminal needs. So on the console they appear with the lease and are absent
 * without it - which does mean the Escape key is not on screen for a viewer,
 * and a viewer has nothing to escape from.
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
  // The console types and never reshapes, and that is said here rather than left
  // to the props below: `mayResize` on the terminal stops this browser from
  // measuring itself, but a size can still reach the pty without being a resize
  // at all - a subscribe carries one, and the server applies it for a client
  // that holds the lease. Withholding it at the session is what makes the rule
  // structural instead of a property of which branch happens to fire.
  const session = useTerminalSession(projectID, running, { mayResize: false })

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
  // helps - or the server refused this project while there was no connection to
  // draw it on.
  //
  // "While there was no connection" is the whole of the distinction, and it is
  // the server's rather than one invented here: an error frame is a refusal of
  // *one message*, and the socket behind it stays open
  // (`internal/terminal/conn.go`; each code's own note is in
  // `terminal/protocol.ts`). A card that threw its terminal away for one would
  // go blank because a keystroke was refused - which happens when the lease
  // moves out from under a client between the roster and the keystroke, a race
  // and not a fault. `components/ProjectTerminal.tsx` has always said the
  // message and kept drawing, and this is the same answer at card size.
  if (status === 'failed' || (session.error !== null && status !== 'open')) {
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
  //
  // It does offer a release, and only to a client that holds the lease. This is
  // the one shape of "no terminal" where the connection is still up, which is
  // the one shape where the server has no reason to take the lease away: it is
  // held for a client that is present, is not suspended, and will not expire.
  // The person who was typing has stopped being able to, so the way out has to
  // be on screen.
  if (session.ended) {
    return (
      <Notice
        tone="neutral"
        title="No active terminal"
        detail="The terminal this was watching has ended."
        action={
          session.control.held
            ? { label: 'Release control', onClick: session.releaseControl }
            : undefined
        }
      />
    )
  }

  const connected = status === 'open'
  const held = session.control.held

  return (
    <div className="terminal-viewer">
      <TerminalControl session={session} />
      <TerminalView
        session={session}
        // The keystroke is accepted only by the client the server will accept it
        // from. Off, and no input handler is bound to the terminal at all - so a
        // viewer has nothing to type into rather than a terminal that ignores
        // what it is told.
        interactive={connected && held}
        // Never, held or not. See the note at the top: the console types, and
        // the size of the pty is not the console's to decide.
        mayResize={false}
        // And the touch keys follow the lease, for the reason above.
        showKeys={held}
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
      {/* The third thing a card can have to say, and the only one that is not
          about the connection at all: the server refused something this client
          sent. It goes in the same corner rather than over the whole card,
          because the terminal underneath is live and still being drawn.

          Exactly one of these three can be true at a time. The first two are
          about a connection that is not open; this one is about a connection
          that is, which is why it is the only one that can share the screen
          with a terminal. */}
      {settled && connected && session.error !== null && (
        <p className="terminal-viewer__notice" role="status">
          {session.error.message}
        </p>
      )}
    </div>
  )
}

/**
 * Notice is the one shape a viewer shows instead of a terminal.
 *
 * `action` admits an explicit `undefined` because two of its three callers
 * build the prop from a condition rather than by branching around the element,
 * and `exactOptionalPropertyTypes` distinguishes "no action" from "an action
 * that is absent". Writing the conditional `undefined` plainly is the clearer
 * of the two - the alternative is a second copy of the whole element for the
 * case where there is nothing to offer.
 */
function Notice({
  tone,
  title,
  detail,
  action,
}: {
  tone: 'neutral' | 'danger'
  title: string
  detail: string
  action?: { label: string; onClick: () => void } | undefined
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
