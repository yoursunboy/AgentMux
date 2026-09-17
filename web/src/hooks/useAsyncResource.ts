import { useCallback, useEffect, useRef, useState } from 'react'

/** The state of one asynchronous read. */
export interface AsyncState<T> {
  data: T | null
  error: Error | null
  loading: boolean
  /** Re-runs the read. Used by the retry control and after a write. */
  reload: () => void
}

/**
 * useAsyncResource runs an async read and tracks its state.
 *
 * It exists so that every panel reports loading, failure, and success the same
 * way. A component that calls fetch() itself tends to end up with a
 * console.log and no message for the user, which is exactly what this avoids.
 *
 * `key` is the restart signal: change it and the read runs again. It is a
 * string rather than a dependency array so the effect's dependency list keeps
 * a fixed size, which React requires.
 *
 * The result is applied only if the effect is still current, so a slow
 * response cannot overwrite a newer one.
 */
export function useAsyncResource<T>(
  load: (signal: AbortSignal) => Promise<T>,
  key = '',
): AsyncState<T> {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<Error | null>(null)
  const [loading, setLoading] = useState(true)
  const [nonce, setNonce] = useState(0)

  // The loader is held in a ref so an inline arrow function does not restart
  // the read on every render; `key` and `nonce` are the explicit signals.
  const loadRef = useRef(load)
  loadRef.current = load

  useEffect(() => {
    const controller = new AbortController()
    let active = true

    setLoading(true)
    setError(null)

    loadRef.current(controller.signal).then(
      (result) => {
        if (!active) return
        setData(result)
        setLoading(false)
      },
      (cause: unknown) => {
        if (!active) return
        // An aborted request is not a failure: the component moved on.
        if (cause instanceof DOMException && cause.name === 'AbortError') return
        setError(cause instanceof Error ? cause : new Error(String(cause)))
        setLoading(false)
      },
    )

    return () => {
      active = false
      controller.abort()
    }
  }, [key, nonce])

  const reload = useCallback(() => setNonce((n) => n + 1), [])
  return { data, error, loading, reload }
}
