/**
 * The React seam over the terminal client.
 *
 * Everything here is about lifetimes rather than about terminals: one client
 * for the whole page so that two panels cannot open two sockets, one
 * subscription per project that exists exactly as long as the project is on
 * screen, and a sink that the view attaches when it has somewhere to draw.
 *
 * The one ordering fact worth stating: a subscription is not created until a
 * sink has been attached, and a sink is attached by the view's own effect,
 * which React runs before the effect that owns the subscription. That is not a
 * coincidence to be relied on silently - it is why the frames a subscription
 * receives can never arrive before there is a terminal to put them in. A
 * snapshot dropped into nothing is a terminal that stays blank for the rest of
 * the connection's life, since the server has no reason to send another.
 */
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'

import type { TerminalClient, ConnectionStatus, TerminalSubscription } from './client'
import type { ErrorMessage } from './protocol'
import type { TerminalSize } from './sizing'

/** Where a subscription's frames are drawn. */
export interface TerminalSink {
  /** draw appends terminal output. */
  draw(payload: Uint8Array): void
  /** show replaces the terminal's contents with a screen of this geometry. */
  show(payload: Uint8Array, cols: number, rows: number): void
  /** resize reports the canonical size the server applied. */
  resize(cols: number, rows: number): void
}

/** One project's terminal, as a component uses it. */
export interface TerminalSession {
  readonly projectId: string | null
  readonly status: ConnectionStatus
  /** The last error the server reported for this project, if any. */
  readonly error: ErrorMessage | null
  /** True when the server has stopped sending for this project. */
  readonly ended: boolean
  /** clearError dismisses the last error. */
  clearError(): void
  /**
   * attach registers where frames should be drawn, starting the subscription.
   * It returns the detach, which ends it.
   */
  attach(sink: TerminalSink): () => void
  /** setSize records how large the terminal is, before anything is subscribed. */
  setSize(size: TerminalSize): void
  /** input writes raw keystrokes or pasted text. */
  input(text: string): void
  /** resize asks the server for a canonical terminal size. */
  resize(cols: number, rows: number): void
  /**
   * askAgain re-establishes the terminal.
   *
   * It is one action rather than two because "show me this terminal again" is
   * one thing to want, whatever state the client is in: a subscription that was
   * dropped is re-subscribed, and a connection that gave up is retried. A UI
   * that had to choose between them would have to know which had happened, and
   * the person clicking does not care.
   */
  askAgain(): void
}

const TerminalClientContext = createContext<TerminalClient | null>(null)

/** TerminalProvider publishes the page's one terminal client. */
export function TerminalProvider({
  client,
  children,
}: {
  client: TerminalClient
  children: ReactNode
}) {
  return <TerminalClientContext.Provider value={client}>{children}</TerminalClientContext.Provider>
}

/** useTerminalClient reads the page's terminal client. */
export function useTerminalClient(): TerminalClient {
  const client = useContext(TerminalClientContext)
  if (!client) {
    throw new Error('useTerminalClient must be used inside a TerminalProvider')
  }
  return client
}

/** useConnectionStatus re-renders when the connection changes. */
export function useConnectionStatus(client: TerminalClient): ConnectionStatus {
  const [status, setStatus] = useState<ConnectionStatus>(client.status)
  useEffect(() => client.onStatusChange(setStatus), [client])
  return status
}

/**
 * useTerminalSession watches one project's terminal while it is on screen.
 *
 * `active` is what a caller uses to say "there is a terminal to watch". A
 * project whose runtime is stopped is not subscribed to, because the server
 * refuses it - and a client that subscribed anyway would spend a reconnect
 * cycle being told so.
 */
export function useTerminalSession(
  projectId: string | null,
  active: boolean,
): TerminalSession {
  const client = useTerminalClient()
  const status = useConnectionStatus(client)

  const [error, setError] = useState<ErrorMessage | null>(null)
  const [ended, setEnded] = useState(false)

  const sinkRef = useRef<TerminalSink | null>(null)
  const sizeRef = useRef<TerminalSize | null>(null)
  const subscriptionRef = useRef<TerminalSubscription | null>(null)

  const clearError = useCallback(() => setError(null), [])

  const attach = useCallback((sink: TerminalSink) => {
    sinkRef.current = sink
    return () => {
      if (sinkRef.current === sink) sinkRef.current = null
    }
  }, [])

  const setSize = useCallback((size: TerminalSize) => {
    sizeRef.current = size
  }, [])

  // The handlers are built once and delegate through refs, so that a
  // subscription is not torn down and rebuilt every time a component renders.
  const handlers = useMemo(
    () => ({
      draw: (payload: Uint8Array) => sinkRef.current?.draw(payload),
      show: (payload: Uint8Array, cols: number, rows: number) =>
        sinkRef.current?.show(payload, cols, rows),
      resize: (cols: number, rows: number) => sinkRef.current?.resize(cols, rows),
      report: (message: ErrorMessage) => setError(message),
      ended: () => setEnded(true),
    }),
    [],
  )

  useEffect(() => {
    if (!projectId || !active) return
    setError(null)
    setEnded(false)
    const subscription = client.subscribe(projectId, sizeRef.current, handlers)
    subscriptionRef.current = subscription
    return () => {
      subscriptionRef.current = null
      subscription.release()
    }
  }, [client, projectId, active, handlers])

  const input = useCallback((text: string) => subscriptionRef.current?.input(text), [])
  const resize = useCallback(
    (cols: number, rows: number) => subscriptionRef.current?.resize(cols, rows),
    [],
  )
  const askAgain = useCallback(() => {
    setEnded(false)
    setError(null)
    // A client that gave up has no subscription left to re-establish, so asking
    // again has to reach the connection itself. Read from the client rather
    // than from this component's last render, which may be several events old.
    const state = client.status.state
    if (state === 'failed' || state === 'idle') client.retry()
    subscriptionRef.current?.resync()
  }, [client])

  return useMemo(
    () => ({
      projectId,
      status,
      error,
      ended,
      clearError,
      attach,
      setSize,
      input,
      resize,
      askAgain,
    }),
    [projectId, status, error, ended, clearError, attach, setSize, input, resize, askAgain],
  )
}
