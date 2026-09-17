interface ErrorBannerProps {
  message: string
  /** Details are shown in a disclosure: useful when reporting a problem. */
  detail?: string
  onDismiss?: () => void
  onRetry?: () => void
}

/**
 * ErrorBanner reports a failure that has no place of its own on the page.
 *
 * Every failure in this UI ends up in one of these, so nothing is silently
 * swallowed and nothing is only written to the console.
 */
export function ErrorBanner({ message, detail, onDismiss, onRetry }: ErrorBannerProps) {
  return (
    <div className="error-banner" role="alert">
      <span className="error-banner__icon" aria-hidden="true">
        {'⚠'}
      </span>
      <div className="error-banner__body">
        <p className="error-banner__message">{message}</p>
        {detail && (
          <details className="error-banner__detail">
            <summary>Details</summary>
            <code>{detail}</code>
          </details>
        )}
      </div>
      <div className="error-banner__actions">
        {onRetry && (
          <button type="button" className="button" onClick={onRetry}>
            Retry
          </button>
        )}
        {onDismiss && (
          <button type="button" className="button button--quiet" onClick={onDismiss}>
            Dismiss
          </button>
        )}
      </div>
    </div>
  )
}
