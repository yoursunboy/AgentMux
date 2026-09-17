import type { ServerInfo } from '../api/types'
import { capitalise, describeHost, formatUptime } from '../lib/format'

interface GlobalBarProps {
  info: ServerInfo | null
  loading: boolean
  error: string | null
  onRetry: () => void
}

/**
 * GlobalBar is the fixed top bar: connection state, host, and provider.
 *
 * The provider switch is disabled and says "Coming later". CC Switch is not
 * integrated in Phase 1, and a control that looks live but does nothing is
 * worse than one that admits it is not ready.
 */
export function GlobalBar({ info, loading, error, onRetry }: GlobalBarProps) {
  const state = error ? 'offline' : loading ? 'connecting' : 'online'
  const label = state === 'online' ? 'Online' : state === 'connecting' ? 'Connecting' : 'Offline'

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

      {error && (
        <button type="button" className="global-bar__retry" onClick={onRetry}>
          Retry
        </button>
      )}
    </header>
  )
}
