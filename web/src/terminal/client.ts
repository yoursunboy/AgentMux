/**
 * The browser's half of the terminal transport: one socket, several terminals.
 *
 * # Why one socket and not one per terminal
 *
 * A socket per terminal would mean a handshake, an origin check and a reconnect
 * policy per terminal, and a browser watching six projects would run six of
 * each. The server multiplexes subscriptions on one connection and the frames
 * carry the project they belong to, so the client does the same: one socket,
 * one reconnect loop, one place that knows whether the server is there.
 *
 * # What the client is responsible for
 *
 * Three things, and they are the three that make a terminal either correct or
 * quietly wrong:
 *
 *   - Sequence continuity. Every frame is checked against the sequence the
 *     client has drawn up to, using the same rule the server defines. A frame
 *     that does not follow is not drawn, and a fresh snapshot is asked for
 *     instead. Drawing it anyway is the failure this exists to prevent: a
 *     terminal with a hole in it shows no sign of having one.
 *   - Re-establishment. Subscriptions are what the client wants, not what the
 *     socket has. A socket that drops and comes back re-subscribes everything,
 *     which is what makes a browser survive a server restart without the user
 *     doing anything.
 *   - Not lying about the connection. The state is published rather than
 *     inferred from the absence of output, because a terminal that has stopped
 *     printing and a terminal whose server has gone look identical otherwise.
 *
 * It does not know what a terminal is. Bytes are handed to a sink and text is
 * handed in by a caller; rendering, scrolling and input capture belong to the
 * view.
 */
import {
  accounts,
  decodeFrame,
  decodeServerMessage,
  encodeInput,
  ENDPOINT,
  isSnapshot,
  Msg,
  PROTOCOL_PARAM,
  PROTOCOL_VERSION,
  ServerMsg,
  SUBPROTOCOL,
  type DecodedFrame,
  type ErrorMessage,
  type HelloMessage,
} from './protocol'
import { MAX_INITIAL_ATTEMPTS, reconnectDelay } from './backoff'
import type { TerminalSize } from './sizing'

/** What the client is doing, as something a UI can render. */
export type ConnectionState = 'idle' | 'connecting' | 'open' | 'reconnecting' | 'failed'

export interface ConnectionStatus {
  state: ConnectionState
  /**
   * Why, when the answer is not obvious from the state.
   *
   * The reasons here are the client's own - a protocol the server does not
   * speak, a server that stopped answering - and are written to be read by the
   * person looking at the terminal rather than by the server's log.
   */
  message: string
  /** Consecutive failed attempts since the last successful connection. */
  attempt: number
}

/**
 * How a subscription's frames are drawn.
 *
 * `draw` and `show` are separate rather than one call with a flag because they
 * mean different things to a terminal: appending a snapshot would duplicate it,
 * and replacing the screen with an output frame would erase everything before
 * it. The server makes the same distinction in its frame types for the same
 * reason.
 */
export interface SubscriptionHandlers {
  /** draw appends terminal bytes. */
  draw(payload: Uint8Array): void
  /** show replaces the terminal's contents with a screen of this geometry. */
  show(payload: Uint8Array, cols: number, rows: number): void
  /** resize reports the canonical size the server applied to the terminal. */
  resize(cols: number, rows: number): void
  /** report describes a failure the subscription cannot recover from itself. */
  report(error: ErrorMessage): void
  /** ended reports that the server stopped sending for this project. */
  ended(): void
}

/** One project's terminal, as a caller drives it. */
export interface TerminalSubscription {
  readonly projectId: string
  /** input writes raw keystrokes or pasted text to the terminal. */
  input(text: string): void
  /** resize asks for a canonical terminal size. */
  resize(cols: number, rows: number): void
  /** resync asks to be shown the terminal again from a fresh screen. */
  resync(): void
  /** release stops watching the project. It is idempotent. */
  release(): void
}

export interface TerminalClient {
  readonly status: ConnectionStatus
  /** onStatusChange subscribes to connection changes, returning an unsubscribe. */
  onStatusChange(listener: (status: ConnectionStatus) => void): () => void
  /** subscribe starts watching a project's terminal. */
  subscribe(
    projectId: string,
    size: TerminalSize | null,
    handlers: SubscriptionHandlers,
  ): TerminalSubscription
  /** retry restarts a client that gave up. */
  retry(): void
  /** close ends the connection for good. */
  close(): void
}

export interface TerminalClientOptions {
  /**
   * Where the socket lives. Defaults to the endpoint on this origin, which in
   * development is the Vite proxy and in production is the server that served
   * this bundle.
   */
  url?: string
  /**
   * socketFactory builds the socket. It exists so that the reconnect policy,
   * the sequence rules and the framing can be tested without a browser: every
   * interesting case here is a server that closes at an awkward moment, and
   * waiting for a real one is not a test anybody runs.
   */
  socketFactory?: (url: string, protocols: string[]) => WebSocket
  /** jitter feeds the reconnect delay. Defaults to Math.random. */
  jitter?: () => number
}

/** Ping interval and the deadline for the answer. */
const PING_INTERVAL_MS = 30_000
const PONG_TIMEOUT_MS = 10_000

/**
 * How long the socket survives with nothing subscribed to it.
 *
 * The rule this exists for is React's ordering rather than the network's. A
 * workspace page change unmounts the panels that were showing and mounts the
 * ones that will be, and React does the unmounting first - so for an instant
 * nothing is subscribed, and a client that closed its socket on "the last
 * release" closed it on every page change. Measured in a browser: paging from
 * five panels to two dropped the WebSocket and opened another, and every
 * terminal in the workspace flashed through Connecting on the way.
 *
 * A page that really has no terminals still ends up holding no connection; it
 * just takes this much longer to get there, which nothing can observe.
 */
const IDLE_CLOSE_MS = 150

/** WebSocket.OPEN, spelled out because a test double need not carry the class. */
const SOCKET_OPEN = 1

/** The address of the terminal endpoint on this origin. */
export function terminalEndpointURL(): string {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${scheme}//${window.location.host}${ENDPOINT}`
}

interface Registration {
  projectId: string
  handlers: SubscriptionHandlers
  size: TerminalSize | null
  /** The sequence number of the last frame drawn. */
  lastSequence: number
  /** True between asking for a screen and being sent one. */
  awaitingSnapshot: boolean
  released: boolean
}

/**
 * createTerminalClient builds a client. It does not connect: the first
 * subscription is what opens a socket, and the last release is what closes one,
 * so a page with no terminal open holds no connection.
 */
export function createTerminalClient(options: TerminalClientOptions = {}): TerminalClient {
  const url = options.url ?? terminalEndpointURL()
  const factory = options.socketFactory ?? ((address: string, protocols: string[]) => new WebSocket(address, protocols))
  const jitter = options.jitter ?? Math.random

  const registrations = new Map<string, Registration>()
  const listeners = new Set<(status: ConnectionStatus) => void>()

  let socket: WebSocket | null = null
  let status: ConnectionStatus = { state: 'idle', message: '', attempt: 0 }
  let idleTimer: ReturnType<typeof setTimeout> | null = null
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null
  let pingTimer: ReturnType<typeof setInterval> | null = null
  let pongTimer: ReturnType<typeof setTimeout> | null = null
  let attempt = 0
  let everOpened = false
  let gaveUp = false
  let closed = false
  let helloReceived = false
  let fault = ''
  /** Serialises the reads of any binary frame that arrives as a Blob. */
  let blobReads: Promise<void> = Promise.resolve()

  function setStatus(state: ConnectionState, message = '', attempts = 0): void {
    if (status.state === state && status.message === message && status.attempt === attempts) return
    status = { state, message, attempt: attempts }
    for (const listener of listeners) listener(status)
  }

  function wanted(): boolean {
    return registrations.size > 0
  }

  function send(message: Record<string, unknown>): boolean {
    const current = socket
    if (!current || current.readyState !== SOCKET_OPEN) return false
    try {
      current.send(JSON.stringify(message))
      return true
    } catch {
      // A socket that throws on send is one the browser has already given up
      // on; onclose follows, and that is where the reconnection is decided.
      return false
    }
  }

  // -------------------------------------------------------------------------
  // Connection

  function address(): string {
    return `${url}?${PROTOCOL_PARAM}=${PROTOCOL_VERSION}`
  }

  function connect(): void {
    if (closed || gaveUp || socket || !wanted()) return
    clearReconnect()
    setStatus(everOpened ? 'reconnecting' : 'connecting', fault, attempt)

    let next: WebSocket
    try {
      next = factory(address(), [SUBPROTOCOL])
    } catch {
      // A URL the browser refuses to parse, or a socket the environment will
      // not open. It is the same outcome as a refused handshake and is answered
      // the same way, rather than by throwing out of an event handler.
      fault = 'the terminal socket could not be opened'
      loseConnection()
      return
    }
    // Binary frames carry terminal output. Without this the browser hands them
    // over as Blobs, which cannot be read synchronously and would put the frame
    // order at the mercy of the microtask queue.
    next.binaryType = 'arraybuffer'
    socket = next
    helloReceived = false

    next.onopen = () => {
      if (socket !== next) return
      everOpened = true
      attempt = 0
      fault = ''
      setStatus('open')
      startLiveness()
      // Every wanted terminal is asked for again. This is what a reconnect is:
      // the socket is new, so nothing is subscribed to it, and each project's
      // subscription is re-established from a fresh screen.
      for (const registration of registrations.values()) {
        registration.lastSequence = 0
        registration.awaitingSnapshot = true
        sendSubscribe(registration)
      }
    }
    next.onmessage = (event: MessageEvent) => {
      if (socket !== next) return
      disarmPongDeadline()
      handleMessage(event.data)
    }
    next.onclose = () => {
      if (socket !== next) return
      socket = null
      stopLiveness()
      loseConnection()
    }
    next.onerror = () => {
      // Every error a WebSocket reports is followed by a close, and the close is
      // where the reconnection is decided. Handling both would double the
      // attempt count for one failure.
    }
  }

  /**
   * loseConnection decides what a socket that is no longer there means.
   *
   * The distinction it draws is the whole of the reconnect policy: a client
   * that has never connected is probably pointed at something that is not
   * there and eventually has to say so, while a client that has connected once
   * is looking at a server that restarts, and a server that restarts is a
   * normal event that a browser is expected to survive.
   */
  function loseConnection(): void {
    if (closed || gaveUp) return
    if (!wanted()) {
      setStatus('idle')
      return
    }
    attempt += 1
    if (!everOpened && attempt >= MAX_INITIAL_ATTEMPTS) {
      gaveUp = true
      setStatus(
        'failed',
        fault ||
          `Could not reach the terminal server after ${MAX_INITIAL_ATTEMPTS} attempts. Check that AgentMux is running.`,
        attempt,
      )
      return
    }
    setStatus('reconnecting', fault, attempt)
    clearReconnect()
    reconnectTimer = setTimeout(() => {
      reconnectTimer = null
      connect()
    }, reconnectDelay(attempt - 1, jitter))
  }

  /**
   * untrustworthy abandons a connection that is saying something this client
   * cannot act on.
   *
   * Closing rather than ignoring is deliberate. A frame that cannot be decoded
   * or a message that is not protocol means the two ends disagree about what
   * they are speaking, and the one thing that must not happen is drawing the
   * parts that happened to parse: a terminal assembled from the frames a client
   * understood is a terminal that is wrong in a way nobody can see. The
   * reconnection that follows asks for a screen instead, which is true whatever
   * went wrong before it.
   */
  function untrustworthy(reason: string): void {
    fault = reason
    const current = socket
    if (current) {
      socket = null
      stopLiveness()
      try {
        current.close()
      } catch {
        // Already gone. The reconnect below is the same either way.
      }
    }
    loseConnection()
  }

  function closeSocket(): void {
    const current = socket
    if (!current) return
    try {
      current.close()
    } catch {
      // Nothing to do: the socket is being discarded either way.
    }
  }

  // -------------------------------------------------------------------------
  // Liveness

  function startLiveness(): void {
    stopLiveness()
    pingTimer = setInterval(() => {
      // The application-level ping is not the WebSocket ping. The transport's
      // own ping is answered by the browser without JavaScript being involved,
      // so it says nothing about whether this page is still doing anything; and
      // a server that has gone away without closing the socket leaves a browser
      // believing it is connected, which is the state a laptop that was closed
      // and reopened wakes up in.
      if (!send({ type: Msg.ping })) return
      if (pongTimer !== null) clearTimeout(pongTimer)
      pongTimer = setTimeout(() => {
        pongTimer = null
        untrustworthy('the terminal server stopped answering')
      }, PONG_TIMEOUT_MS)
    }, PING_INTERVAL_MS)
  }

  function disarmPongDeadline(): void {
    if (pongTimer !== null) {
      clearTimeout(pongTimer)
      pongTimer = null
    }
  }

  function stopLiveness(): void {
    if (pingTimer !== null) {
      clearInterval(pingTimer)
      pingTimer = null
    }
    disarmPongDeadline()
  }

  function clearReconnect(): void {
    if (reconnectTimer !== null) {
      clearTimeout(reconnectTimer)
      reconnectTimer = null
    }
  }

  /**
   * keepAlive cancels a close that was only scheduled because nothing was
   * subscribed for a moment - which is what a page change looks like.
   */
  function keepAlive(): void {
    if (idleTimer !== null) {
      clearTimeout(idleTimer)
      idleTimer = null
    }
  }

  /** closeWhenIdle ends the connection once nothing has wanted it for a moment. */
  function closeWhenIdle(): void {
    keepAlive()
    idleTimer = setTimeout(() => {
      idleTimer = null
      if (closed || wanted()) return
      clearReconnect()
      closeSocket()
      setStatus('idle')
    }, IDLE_CLOSE_MS)
  }

  // -------------------------------------------------------------------------
  // Messages

  function handleMessage(data: unknown): void {
    if (typeof data === 'string') {
      handleText(data)
      return
    }
    if (data instanceof ArrayBuffer) {
      handleBinary(data)
      return
    }
    if (typeof Blob !== 'undefined' && data instanceof Blob) {
      // Unreachable while binaryType is arraybuffer; kept because the cost of
      // being wrong about that is every frame, and the reads are chained so
      // that a Blob that does arrive cannot be drawn out of order.
      const blob = data
      blobReads = blobReads
        .then(async () => {
          handleBinary(await blob.arrayBuffer())
        })
        .catch(() => {
          // Reported through the read path, if it is reported at all.
        })
      return
    }
    untrustworthy('the server sent a frame this client cannot read')
  }

  function handleText(text: string): void {
    const message = decodeServerMessage(text)
    if (!message) {
      untrustworthy('the server sent a message this client does not understand')
      return
    }
    // The greeting is queued before anything else can be, so a message that
    // arrives without one means this is not the protocol it claims to be.
    if (!helloReceived && message.type !== ServerMsg.hello) {
      untrustworthy('the server sent a message before greeting this client')
      return
    }

    switch (message.type) {
      case ServerMsg.hello:
        handleHello(message)
        return
      case ServerMsg.resized: {
        const registration = registrations.get(message.projectId)
        if (!registration || registration.released) return
        registration.handlers.resize(message.cols, message.rows)
        return
      }
      case ServerMsg.unsubscribed: {
        const registration = registrations.get(message.projectId)
        if (!registration || registration.released) return
        registration.handlers.ended()
        return
      }
      case ServerMsg.error: {
        const registration = message.projectId ? registrations.get(message.projectId) : undefined
        if (registration && !registration.released) {
          registration.handlers.report(message)
          return
        }
        // An error about no project in particular is the connection's, not a
        // subscription's - a message the server could not parse at all.
        untrustworthy(message.message || 'the server refused a message')
        return
      }
      case ServerMsg.pong:
        return
    }
  }

  function handleHello(message: HelloMessage): void {
    helloReceived = true
    if (message.protocol !== PROTOCOL_VERSION) {
      gaveUp = true
      setStatus(
        'failed',
        `This page speaks terminal protocol ${PROTOCOL_VERSION} and the server speaks ${message.protocol}. Reload the page.`,
        0,
      )
      const current = socket
      socket = null
      stopLiveness()
      try {
        current?.close()
      } catch {
        // Nothing to add: the client has already stopped using it.
      }
    }
  }

  function handleBinary(buffer: ArrayBuffer): void {
    let frame: DecodedFrame
    try {
      frame = decodeFrame(buffer)
    } catch (error) {
      untrustworthy(error instanceof Error ? error.message : 'a frame could not be read')
      return
    }

    const registration = registrations.get(frame.projectId)
    if (!registration || registration.released) {
      // A frame for a terminal this client stopped watching. It was in flight
      // when the unsubscribe was sent, and the only thing to do with it is
      // nothing.
      return
    }

    if (isSnapshot(frame)) {
      registration.lastSequence = frame.lastSequence
      registration.awaitingSnapshot = false
      registration.handlers.show(frame.payload, frame.cols, frame.rows)
      return
    }
    if (!accounts(frame, registration.lastSequence)) {
      requestResync(registration)
      return
    }
    registration.lastSequence = frame.lastSequence
    registration.handlers.draw(frame.payload)
  }

  // -------------------------------------------------------------------------
  // Subscriptions

  function sendSubscribe(registration: Registration): void {
    const message: Record<string, unknown> = {
      type: Msg.subscribe,
      projectId: registration.projectId,
    }
    if (registration.size) {
      message.cols = registration.size.cols
      message.rows = registration.size.rows
    }
    send(message)
  }

  function requestResync(registration: Registration): void {
    // One request at a time. Several frames can arrive between asking for a
    // screen and being sent one, and every one of them is a gap by definition;
    // answering each is a request per frame for an event that one request
    // already caused.
    if (registration.awaitingSnapshot) return
    registration.awaitingSnapshot = true
    send({ type: Msg.resync, projectId: registration.projectId })
  }

  function subscribe(
    projectId: string,
    size: TerminalSize | null,
    handlers: SubscriptionHandlers,
  ): TerminalSubscription {
    const existing = registrations.get(projectId)
    const registration: Registration = existing ?? {
      projectId,
      handlers,
      size,
      lastSequence: 0,
      awaitingSnapshot: true,
      released: false,
    }
    const previousSize = registration.size
    registration.handlers = handlers
    if (size) registration.size = size
    if (!existing) registrations.set(projectId, registration)

    // Something wants a terminal again, which is the common case after a page
    // change replaced one set of panels with another.
    keepAlive()
    resetIfGaveUp()
    connect()
    if (socket && socket.readyState === SOCKET_OPEN) {
      if (existing) {
        // Already watched on this socket. Re-subscribing would be correct - the
        // server treats it as a re-synchronisation - but it costs a capture of
        // the screen, and a component that re-renders is not a reason to take
        // one. A size that has not changed is not a reason to say anything
        // either: the size this subscription was created with was carried by the
        // subscribe that created it, so the server has already applied it.
        if (size && !sameSize(previousSize, size)) {
          send({ type: Msg.resize, projectId, cols: size.cols, rows: size.rows })
        }
      } else {
        registration.lastSequence = 0
        registration.awaitingSnapshot = true
        sendSubscribe(registration)
      }
    }

    return {
      projectId,
      input(text: string): void {
        if (registration.released) return
        // A keystroke typed while the socket is down is dropped rather than
        // queued. Queueing it would deliver it after everything the terminal
        // has done since, which is a worse answer than losing it: the client is
        // showing that it is not connected, and the bytes a person types into a
        // terminal they can see is disconnected are bytes they expect to lose.
        for (const data of encodeInput(text)) {
          send({ type: Msg.input, projectId, data })
        }
      },
      resize(cols: number, rows: number): void {
        if (registration.released) return
        registration.size = { cols, rows }
        send({ type: Msg.resize, projectId, cols, rows })
      },
      resync(): void {
        if (registration.released) return
        registration.awaitingSnapshot = true
        send({ type: Msg.resync, projectId })
      },
      release(): void {
        if (registration.released) return
        registration.released = true
        registrations.delete(projectId)
        send({ type: Msg.unsubscribe, projectId })
        if (!wanted()) closeWhenIdle()
      },
    }
  }

  /**
   * sameSize reports whether two sizes are the same size.
   *
   * A missing previous size is a change, because a subscription that has never
   * been told one has not said anything to the server about its shape.
   */
  function sameSize(previous: TerminalSize | null, next: TerminalSize): boolean {
    return previous !== null && previous.cols === next.cols && previous.rows === next.rows
  }

  function resetIfGaveUp(): void {
    if (!gaveUp) return
    // A new subscription is a person asking again, which is as good a reason to
    // try as the retry button is.
    gaveUp = false
    attempt = 0
    everOpened = false
    fault = ''
  }

  return {
    get status() {
      return status
    },
    onStatusChange(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    subscribe,
    retry() {
      if (closed) return
      resetIfGaveUp()
      fault = ''
      attempt = 0
      everOpened = false
      connect()
    },
    close() {
      closed = true
      keepAlive()
      clearReconnect()
      stopLiveness()
      const current = socket
      socket = null
      registrations.clear()
      try {
        current?.close()
      } catch {
        // Nothing to do: the client is finished with it either way.
      }
      setStatus('idle')
    },
  }
}
