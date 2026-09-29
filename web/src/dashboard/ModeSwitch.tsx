import { useCallback, useEffect, useRef, useState } from 'react'

import type { TerminalSession } from '../terminal/useTerminal'

/**
 * The keyboard's Shift+Tab, as a button.
 *
 * # Why a card needs it
 *
 * Claude Code cycles its permission mode on Shift+Tab, and a tablet has no
 * Shift and no Tab. The alternative was not "type it on a keyboard" - it was
 * walking to a machine that has one, which is the trip the console exists to
 * avoid. So the card sends the same two bytes the key sends, and the terminal
 * cannot tell the difference.
 *
 * # Why it does not draw the mode
 *
 * Nothing reports it. The mode is cycled inside Claude Code's own TUI, and the
 * one thing that knows which mode is in force is the process drawing that TUI -
 * no hook fires on a Shift+Tab, and the mode reaches AgentMux only when a
 * prompt, a notification or the end of a turn comes round. A card that drew a
 * mode would be drawing its own guess at one, which is the thing
 * `docs/CONTROLLER_UI.md` §6 refuses to do about the provider as well.
 *
 * # Why it is not an approval
 *
 * `docs/TERMINAL_CONTROLLER.md` §9 says no button acts on the agent's behalf,
 * and this is not an exception to it. Approving a prompt and choosing a mode are
 * different: an approval answers a question the agent asked about *this* work,
 * and a mode says how much the person wants to be asked at all. This button
 * sends a keystroke the person would otherwise send themselves - it is an input
 * device, in the same family as `TerminalView`'s touch keys, and it decides
 * nothing about what the agent may do.
 *
 * # Why it takes the lease rather than refusing without it
 *
 * The card already has a Request control button, and the obvious build is to
 * disable this one until that has been pressed. That is two taps for the person
 * the button is for, and the second tap has to be explained. Asking for the
 * lease on the way past is one tap and the same authority: `session.input` is
 * gated on the lease inside the hook (`useTerminalSession`), so this cannot send
 * anything a controller could not.
 *
 * # Why the wait is bounded, and why an answer ends it
 *
 * A free terminal grants the lease at once, and that is the case this is for.
 * But a terminal somebody else is using *queues* the request (`ReasonQueued`),
 * and a keystroke that landed whenever they happened to stop for lunch would
 * change the mode of work somebody else is in the middle of - long after the
 * tap that asked for it meant anything. Ten seconds is long enough for a round
 * trip and a slow grant, and short enough that a person still connects the
 * result to the tap.
 *
 * The bound is the backstop, not the rule. The rule is that the wait ends when
 * the server answers it: a grant sends the keystroke, and a refusal drops it. A
 * server that has said no is not going to say yes ten seconds later to the same
 * tap, and a button still reading "taking control" after the card has shown the
 * person the reason it was refused is a button telling a small lie.
 */
export interface ModeSwitchProps {
  /** The card's session, which is where the lease and the input call live. */
  session: TerminalSession
}

/** What a terminal sends when Shift and Tab are pressed together. */
const SHIFT_TAB = '\x1b[Z'

/**
 * How long a request for the lease is worth waiting for.
 *
 * The number is a judgement about people, like the lease's own grace period: it
 * has to outlast a round trip, and it must not outlast the person's memory of
 * pressing the button.
 */
const LEASE_WAIT_MS = 10_000

export function ModeSwitch({ session }: ModeSwitchProps) {
  // True between a tap that had no lease to send with, and one of the three ways
  // that ends: the lease arriving, the server refusing, or the wait running out.
  const [waiting, setWaiting] = useState(false)

  /**
   * The last thing the server said, as it stood when the button was tapped.
   *
   * A control message that answers a request carries a reason, and the reason
   * already on the roster is not an answer to *this* tap - it is the answer to
   * whatever came before. A card whose client had been granted or refused
   * anything earlier in the session would otherwise read that old reason as a
   * fresh refusal and cancel its own wait on the render after the tap.
   */
  const answered = useRef(session.control.reason)

  const held = session.control.held
  const reason = session.control.reason
  const connected = session.status.state === 'open' && !session.ended

  /**
   * The keystroke goes out when the lease arrives, not when the button is
   * pressed.
   *
   * This is the part that would otherwise be silently lost: `session.input`
   * returns early for a client that is not the controller, so a mode switch
   * pressed without the lease would send nothing and look like a button that
   * does not work.
   */
  useEffect(() => {
    if (!waiting) return
    if (held) {
      session.input(SHIFT_TAB)
      setWaiting(false)
      return
    }
    // A reason that was not there at the tap is the server having spoken since.
    // Only a non-empty one counts: `control.changed` carries no reason at all,
    // and it is broadcast whenever any roster moves - somebody opening the same
    // project in another tab would otherwise cancel a keystroke in flight.
    if (reason !== '' && reason !== answered.current) setWaiting(false)
  }, [waiting, held, reason, session])

  // And the wait ends on its own, so a request the server never answers cannot
  // fire a keystroke minutes later.
  useEffect(() => {
    if (!waiting) return
    const timer = setTimeout(() => setWaiting(false), LEASE_WAIT_MS)
    return () => clearTimeout(timer)
  }, [waiting])

  // A connection that went away takes any pending keystroke with it.
  useEffect(() => {
    if (!connected) setWaiting(false)
  }, [connected])

  const onClick = useCallback(() => {
    if (held) {
      session.input(SHIFT_TAB)
      return
    }
    answered.current = reason
    setWaiting(true)
    session.requestControl()
  }, [held, reason, session])

  return (
    <button
      type="button"
      className="button button--small"
      onClick={onClick}
      disabled={!connected}
      title={
        !connected
          ? 'The terminal is not connected.'
          : held
            ? "Cycle the agent's permission mode, as the terminal's Shift+Tab does."
            : 'Take control and cycle the agent’s permission mode.'
      }
    >
      {waiting ? 'Taking control…' : 'Mode'}
    </button>
  )
}
