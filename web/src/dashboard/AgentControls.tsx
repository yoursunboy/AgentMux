import { useState } from 'react'

import { restartAgent, startAgent, stopAgent } from '../api/client'
import { ConfirmDialog } from '../components/ConfirmDialog'
import { ErrorBanner } from '../components/ErrorBanner'
import { describeError } from '../lib/format'

/**
 * Start, Stop and Restart, for one project's agent.
 *
 * # What these are, and what they are not
 *
 * Each button is one request to one endpoint. None of them types into the
 * terminal, sends Shift+Tab, or simulates a Ctrl+C at the pty: the console
 * asking for a lifecycle change and a person pressing keys are two different
 * things, and a card that faked the second would be a card whose "Stopping…"
 * meant "some bytes were written", which is not the same as a stop.
 *
 * # The buttons a card offers, and why exactly these
 *
 *	agent running    Stop · Restart
 *	agent not        Start
 *	a request is in flight   the one button, naming what is happening
 *
 * There is no Stop to offer an agent that is not there: the server would answer
 * it successfully - stop is idempotent by design - and a button that reports
 * success for something that was not happening teaches the person reading it
 * that the card is not to be trusted. There is no Start beside a running agent
 * for the same reason in reverse: the server adopts the process rather than
 * launching a second one, so the request is harmless and the *button* is still
 * wrong, because it offers to do a thing that will not happen.
 *
 * Restart is a stop and a start in one request, and the server holds one
 * critical section across both. That is why this is one button calling one
 * endpoint rather than the two the client could have chained - a stop and a
 * start sent separately leave a gap in which a Start from another client puts
 * two Claudes in one project.
 *
 * # Asking before a restart
 *
 * A restart ends a working agent, and unlike a stop it does not wait to be
 * asked for. So it asks: `ConfirmDialog`, which is the console's own component,
 * with the same Escape and focus behaviour as the workspace's destroy dialog.
 * It is deliberately not `window.confirm` - that is a browser dialog, not a
 * console one, and it cannot say what a restart leaves alone.
 *
 * # Failure, and the one failure that is read rather than reported
 *
 * A failed request is shown here, in the card it happened to, rather than in
 * one banner at the top of the page: a stop that timed out is about one project,
 * and a page-level slot would make a second failure erase the first.
 *
 * `agent_stop_timeout` is the interesting one. It is not a bug and it is not a
 * success: the interrupt was delivered and the agent is still working, which is
 * a state the card has to draw as one. `describeError` reads the pid and the
 * grace out of the server's own details, and the card is re-read either way so
 * that what it shows next is the server's answer rather than this component's.
 */
export type Lifecycle = 'start' | 'stop' | 'restart'

/** What each request calls itself while it is happening. */
const IN_FLIGHT: Record<Lifecycle, string> = {
  start: 'Starting…',
  stop: 'Stopping…',
  restart: 'Restarting…',
}

/** One drawn button. Every field is present, so nothing is decided at the call site. */
interface Control {
  key: string
  label: string
  title: string
  disabled: boolean
  onClick: () => void
}

export interface AgentControlsProps {
  projectId: string

  /**
   * Whether there is an agent process here to stop.
   *
   * It is the card's read of the agent state projection, which is the server's
   * answer and not this component's guess - see `agentIsRunning`. It is a
   * question about which buttons to *draw*; whether a second Claude would
   * actually be started is the server's answer and not this one.
   */
  running: boolean

  /**
   * Called once a request has finished, successfully or not.
   *
   * The card is re-read rather than patched, because the agent's state is the
   * server's to report: the request may have ended an attempt, moved a project
   * to a state this client did not predict, or failed in a way that left things
   * exactly as they were. A card that kept its own answer would drift from all
   * three.
   */
  onChanged?: (() => void) | undefined
}

export function AgentControls({ projectId, running, onChanged }: AgentControlsProps) {
  const [inFlight, setInFlight] = useState<Lifecycle | null>(null)
  const [confirming, setConfirming] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)

  const run = async (what: Lifecycle): Promise<void> => {
    setConfirming(false)
    setInFlight(what)
    setFailure(null)
    try {
      if (what === 'start') await startAgent(projectId)
      else if (what === 'stop') await stopAgent(projectId)
      else await restartAgent(projectId)
    } catch (error) {
      // Kept, not re-thrown: an unhandled rejection in a click handler is a
      // failure that reaches nobody, and this one has a card to be shown in.
      setFailure(error)
    } finally {
      setInFlight(null)
      onChanged?.()
    }
  }

  const controls: Control[] = []

  if (inFlight !== null) {
    // One button, disabled, saying what is happening. Drawing the other two
    // beside it and greying them out would leave the person reading the card to
    // work out which of three buttons is the live one.
    controls.push({
      key: inFlight,
      label: IN_FLIGHT[inFlight],
      title: 'This request is in flight. The card follows the agent until it answers.',
      disabled: true,
      onClick: () => {},
    })
  } else if (running) {
    controls.push({
      key: 'stop',
      label: 'Stop',
      title: 'Interrupt Claude and leave the terminal, its scrollback and its working directory alone.',
      disabled: false,
      onClick: () => void run('stop'),
    })
    controls.push({
      key: 'restart',
      label: 'Restart',
      title: 'Stop Claude and start a new one in the same terminal, with the project’s current permission mode.',
      disabled: false,
      onClick: () => setConfirming(true),
    })
  } else {
    controls.push({
      key: 'start',
      label: 'Start',
      title: 'Start Claude Code here. Brings the terminal up as well when it is not running.',
      disabled: false,
      onClick: () => void run('start'),
    })
  }

  return (
    <>
      <span className="agent-controls" role="group" aria-label="Claude lifecycle">
        {controls.map((control) => (
          <button
            key={control.key}
            type="button"
            className="agent-control"
            title={control.title}
            disabled={control.disabled}
            onClick={control.onClick}
          >
            {control.label}
          </button>
        ))}
      </span>

      {failure !== null && (
        <ErrorBanner
          message={describeError(failure)}
          onDismiss={() => setFailure(null)}
        />
      )}

      {confirming && (
        <ConfirmDialog
          title="Restart Claude?"
          confirmLabel="Restart"
          busy={inFlight !== null}
          onCancel={() => setConfirming(false)}
          onConfirm={() => void run('restart')}
          body={
            <>
              <p>
                Claude in this project will be stopped, and a new one started in the same
                terminal.
              </p>
              <p>
                The terminal stays up, with its scrollback and its working directory, and the
                new agent is launched with the permission mode the project is configured with
                now. The attempt that is running ends and a new one begins.
              </p>
            </>
          }
        />
      )}
    </>
  )
}
