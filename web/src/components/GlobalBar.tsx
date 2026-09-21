import type { ServerInfo } from '../api/types'
import { DASHBOARD_PATH } from '../dashboard/route'
import { capitalise, describeHost, formatUptime } from '../lib/format'
import type { ConnectionState, ConnectionStatus } from '../terminal/client'

/** What the bar's first section says, and in which colour. */
export interface GlobalState {
  state: 'online' | 'connecting' | 'offline'
  label: string
}

/**
 * globalState folds the server's REST state and the terminal socket's state
 * into the one word the bar shows.
 *
 * # Why the socket is part of it
 *
 * "Online" is a claim about the connection this page is actually using, and the
 * connection it actually uses for its terminals is the WebSocket. A page whose
 * REST calls are fine and whose terminal socket has gone is not offline, but it
 * is not simply online either: terminals are stopped, and saying "Online" over a
 * grid of frozen panels is the one answer that would be wrong.
 *
 * The order of the tests is the order of specificity. A server that cannot be
 * reached at all is offline whatever the socket says, because the socket's
 * reconnect loop is a consequence of that and not separate news. A socket that
 * gave up is reported as the terminal's problem, in the terminal's own words,
 * because the two failures have different fixes: one is "start the server", the
 * other is "reload the page".
 *
 * A client with nothing subscribed is `idle` and holds no socket at all, which
 * is not a fault and is reported as online.
 */
export function globalState(
  server: { error: boolean; loading: boolean },
  connection: ConnectionState,
  message: string,
): GlobalState {
  if (server.error) return { state: 'offline', label: 'Offline' }
  if (server.loading) return { state: 'connecting', label: 'Connecting' }

  switch (connection) {
    case 'reconnecting':
      return { state: 'connecting', label: 'Reconnecting' }
    case 'failed':
      return { state: 'offline', label: message || 'Terminal offline' }
    case 'connecting':
      return { state: 'connecting', label: 'Connecting' }
    default:
      return { state: 'online', label: 'Online' }
  }
}

interface GlobalBarProps {
  info: ServerInfo | null
  loading: boolean
  error: string | null
  /** The shared terminal connection, which every panel on the page uses. */
  connection: ConnectionStatus
  onRetry: () => void
}

/**
 * GlobalBar is the fixed top bar: connection state, host, and provider.
 *
 * It is one line and stays one line. The provider switch is disabled and says
 * "Coming later": CC Switch is not integrated, and a control that looks live
 * but does nothing is worse than one that admits it is not ready.
 */
export function GlobalBar({ info, loading, error, connection, onRetry }: GlobalBarProps) {
  const { state, label } = globalState(
    { error: error !== null, loading },
    connection.state,
    connection.message,
  )

  return (
    <header className="global-bar" role="banner">
      <div className="global-bar__group">
        <span className={`global-bar__state global-bar__state--${state}`} aria-hidden="true">
          {'●'}
        </span>
        <span className="global-bar__label" role="status">
          {label}
        </span>
      </div>
      <span className="global-bar__divider" aria-hidden="true">
        |
      </span>

      <span className="global-bar__host" title={info ? `${info.runtimeMode} on ${info.hostArch}` : ''}>
        {describeHost(info)}
      </span>

      {info && (
        <>
          <span className="global-bar__divider" aria-hidden="true">
            |
          </span>
          <span className="global-bar__runtime" title={`Runtime mode: ${info.runtimeMode}`}>
            {capitalise(info.runtimeMode)}
          </span>
        </>
      )}

      {info && (
        <>
          <span className="global-bar__divider" aria-hidden="true">
            |
          </span>
          {info.features.terminal ? (
            <span className="global-bar__terminal" title="Terminal sessions can run on this server">
              Terminal ready
            </span>
          ) : (
            // The reason is the server's own, and it is the one that says
            // whether to start the server inside WSL or to install tmux. It is
            // shown in full in the warnings banner above; here it is the
            // tooltip, so the bar stays one line.
            <span
              className="global-bar__terminal global-bar__terminal--unavailable"
              title={info.runtimeUnavailableReason || 'This server cannot host a terminal runtime.'}
            >
              Runtime unavailable
            </span>
          )}
        </>
      )}

      <div className="global-bar__spacer" />

      {info && (
        <span className="global-bar__meta" title={`Server started ${info.startedAt}`}>
          {info.appName} {info.version} &middot; up {formatUptime(info.uptimeSeconds)}
        </span>
      )}

      <div className="global-bar__provider">
        <span className="global-bar__provider-name">Claude</span>
        <select
          className="global-bar__switch"
          disabled
          aria-label="Provider switch"
          title="CC Switch integration is not part of Phase 1."
        >
          <option>Coming later</option>
        </select>
      </div>

      {/* The console is a separate page, so this is a link and a page load
          rather than a state change: leaving the workspace closes its terminal
          socket, and the two pages share nothing that could be left out of
          step. */}
      <a className="global-bar__console" href={DASHBOARD_PATH}>
        Console
      </a>

      {error && (
        <button type="button" className="global-bar__retry" onClick={onRetry}>
          Retry
        </button>
      )}
    </header>
  )
}
