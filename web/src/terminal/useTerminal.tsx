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
import type { ControlMessage, ControlView, ErrorMessage } from './protocol'
import { isController, isWaiting } from './protocol'
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

/**
 * Who is in charge of a project, as a component reads it.
 *
 * `held` and `waiting` are derived here rather than sent by the server, because
 * they are questions about *this* client and only this client can answer them:
 * the wire says who the controller is, and the comparison against our own
 * identifier is ours to make. A server-computed "you are the controller" flag is
 * a flag that can be sent to the wrong recipient.
 */
export interface ControlState {
  /** The roster as the server last described it. Null until the first arrives. */
  readonly view: ControlView | null
  /** True while this client holds this project's lease. */
  readonly held: boolean
  /** True while this client has asked for it and has not been answered. */
  readonly waiting: boolean
  /**
   * Why the last control message directed at this client arrived, or the empty
   * string.
   *
   * It is the one thing a control message says that the roster does not: the
   * roster gives the state of the project, and this gives the reason it changed,
   * which is what a refusal has to be able to explain - a request turned down
   * because somebody else is using the terminal leaves no trace in a roster that
   * shows the same controller it showed before.
   */
  readonly reason: string
}

/** The state of a project nobody has said anything about yet. */
const NO_CONTROL: ControlState = { view: null, held: false, waiting: false, reason: '' }

/** One project's terminal, as a component uses it. */
export interface TerminalSession {
  readonly projectId: string | null
  readonly status: ConnectionStatus
  /** The last error the server reported for this project, if any. */
  readonly error: ErrorMessage | null
  /** True when the server has stopped sending for this project. */
  readonly ended: boolean
  /** Who is in charge of this project, and whether it is us. */
  readonly control: ControlState
  /** clearError dismisses the last error. */
  clearError(): void
  /**
   * attach registers where frames should be drawn, starting the subscription.
   * It returns the detach, which ends it.
   */
  attach(sink: TerminalSink): () => void
  /** setSize records how large the terminal is, before anything is subscribed. */
  setSize(size: TerminalSize): void
  /** input writes raw keystrokes or pasted text, if this client may type. */
  input(text: string): void
  /** resize asks the server for a canonical terminal size, if this client may. */
  resize(cols: number, rows: number): void
  /** requestControl asks to be given this project's keyboard. */
  requestControl(): void
  /** releaseControl gives it up. */
  releaseControl(): void
  /** acceptControl hands the lease to a client that has asked for it. */
  acceptControl(clientId: string): void
  /** rejectControl declines a request without giving anything up. */
  rejectControl(clientId: string): void
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
  const [control, setControl] = useState<ControlState>(NO_CONTROL)

  const sinkRef = useRef<TerminalSink | null>(null)
  const sizeRef = useRef<TerminalSize | null>(null)
  const subscriptionRef = useRef<TerminalSubscription | null>(null)
  /**
   * The roster as of the last render, for the callbacks that are not handlers.
   *
   * `input` and `resize` are called from event handlers, which see the render
   * they were created in rather than the newest state; the authority decision
   * they make has to be about now, not about whenever the last render happened.
   */
  const controlRef = useRef<ControlState>(control)

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
      control: (view: ControlView, message: ControlMessage) => {
        // The client's own identifier is read here rather than captured, and it
        // is always known by now: the greeting is the first message on the
        // socket, so no roster can arrive before the name this client compares
        // against.
        const id = client.clientId
        const next: ControlState = {
          view,
          held: isController(view, id),
          waiting: isWaiting(view, id),
          reason: 'reason' in message ? message.reason : '',
        }
        if (next.held && !controlRef.current.held) {
          // Taking control is when this browser's shape starts to matter. Until
          // now the pty has been shaped by whoever was typing, and this client
          // has been drawing at a size it had no right to ask for - so the size
          // it has is stated once, here, rather than waiting for the next time
          // something happens to be resized. It is sent directly rather than
          // through the gated resize above, which at this instant would refuse
          // it: the roster in hand is the old one.
          const size = sizeRef.current
          if (size) subscriptionRef.current?.resize(size.cols, size.rows)
        }
        controlRef.current = next
        setControl(next)
      },
      report: (message: ErrorMessage) => setError(message),
      ended: () => setEnded(true),
    }),
    [client],
  )

  useEffect(() => {
    if (!projectId || !active) return
    setError(null)
    setEnded(false)
    // Whatever was in charge of the project this hook watched last says nothing
    // about this one, and a panel that kept the previous roster would show the
    // wrong device as the controller for as long as the new one takes to arrive.
    controlRef.current = NO_CONTROL
    setControl(NO_CONTROL)
    const subscription = client.subscribe(projectId, sizeRef.current, handlers)
    subscriptionRef.current = subscription
    return () => {
      subscriptionRef.current = null
      subscription.release()
    }
  }, [client, projectId, active, handlers])

  const input = useCallback((text: string) => {
    // The view does not offer the keyboard to a viewer and the server refuses it
    // as well, so this is the third of three answers to the same question - and
    // it is the one that makes the rule true wherever input comes from, rather
    // than only from the components that remember to check.
    if (!controlRef.current.held) return
    subscriptionRef.current?.input(text)
  }, [])
  const resize = useCallback((cols: number, rows: number) => {
    // A terminal has one pty and therefore one shape, and it is the controller's
    // that wins. A viewer's size is not sent at all: it is the browser measuring
    // itself rather than a person asking for anything, so a phone that rotated
    // would otherwise be answered with a refusal it did nothing to deserve.
    if (!controlRef.current.held) return
    subscriptionRef.current?.resize(cols, rows)
  }, [])
  const requestControl = useCallback(() => subscriptionRef.current?.requestControl(), [])
  const releaseControl = useCallback(() => subscriptionRef.current?.releaseControl(), [])
  const acceptControl = useCallback(
    (clientId: string) => subscriptionRef.current?.acceptControl(clientId),
    [],
  )
  const rejectControl = useCallback(
    (clientId: string) => subscriptionRef.current?.rejectControl(clientId),
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
      control,
      clearError,
      attach,
      setSize,
      input,
      resize,
      requestControl,
      releaseControl,
      acceptControl,
      rejectControl,
      askAgain,
    }),
    [
      projectId,
      status,
      error,
      ended,
      control,
      clearError,
      attach,
      setSize,
      input,
      resize,
      requestControl,
      releaseControl,
      acceptControl,
      rejectControl,
      askAgain,
    ],
  )
}
