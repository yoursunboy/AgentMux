import type { ControllerServer, ModelBinding } from '../api/types'
import { WORKSPACE_PATH } from './route'

/**
 * The console's own top bar.
 *
 * # Why it is not the workspace's bar
 *
 * The workspace's bar reports the terminal: which host answered, which tmux is
 * installed, whether a session can run here, and whether the socket is up. None
 * of that is what this page is about. This page is about what needs a person,
 * and the three things above a grid of cards are the server's identity, whether
 * the machine can run an agent at all, and which model the agent is using.
 *
 * They are two bars because they are two questions. Making one bar answer both
 * would mean a component that had to be told which page it was on, which is the
 * thing the two pages exist to avoid.
 */
export interface ServerBarProps {
  server: ControllerServer

  /**
   * Which model the agent is bound to, or null when nothing knows.
   *
   * It is null today, and that is the honest answer: the provider integration
   * is Phase 8 and the controller's server block does not carry it. §6 of the
   * phase brief asks for `Unknown` rather than a guess, and it is right - a
   * model name that came from nowhere is a name somebody would act on.
   */
  model?: ModelBinding | null
}

export function ServerBar({ server, model = null }: ServerBarProps) {
  const online = server.status === 'online'

  return (
    <header className="server-bar" role="banner">
      <div className="server-bar__group">
        <span
          className={`server-bar__dot server-bar__dot--${online ? 'online' : 'offline'}`}
          aria-hidden="true"
        >
          {'●'}
        </span>
        <span className="server-bar__label" role="status">
          {online ? 'Online' : server.status}
        </span>
      </div>

      <span className="server-bar__divider" aria-hidden="true">
        |
      </span>

      <span className="server-bar__item">
        Runtime:{' '}
        {server.runtimeAvailable ? (
          <span className="server-bar__ok">available</span>
        ) : (
          // The reason is the server's own, and it is the one a person can act
          // on: start the server inside WSL, or install tmux. It is a tooltip
          // here rather than a line, so the bar stays one bar.
          <span
            className="server-bar__no"
            title={server.runtimeUnavailableReason || 'This server cannot host a terminal runtime.'}
          >
            unavailable
          </span>
        )}
      </span>

      <span className="server-bar__divider" aria-hidden="true">
        |
      </span>

      <span className="server-bar__item server-bar__item--model">
        CC Switch:{' '}
        {model ? (
          <span className="server-bar__model">
            {model.tool} &rarr; {model.model}
          </span>
        ) : (
          <span className="server-bar__model server-bar__model--unknown" title="No provider integration reports a model binding yet.">
            Unknown
          </span>
        )}
        {/*
          The control is here and it does nothing, deliberately. §5 of the phase
          brief asks for the button to be shown and not wired, and the alternative
          - hiding it until Phase 8 - would make the bar's shape change under the
          people using it. A disabled control that says why is honest; a control
          that appeared to switch something would not be.
        */}
        <button
          type="button"
          className="server-bar__switch"
          disabled
          title="Provider switching is Phase 8. This control is not connected to anything."
        >
          Switch
        </button>
      </span>

      <div className="server-bar__spacer" />

      <span className="server-bar__meta" title={`AgentMux ${server.version}`}>
        {server.version}
      </span>

      <a className="server-bar__link" href={WORKSPACE_PATH}>
        Workspace
      </a>
    </header>
  )
}
