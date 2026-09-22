/**
 * A terminal session and a terminal client, as a component sees them.
 *
 * These exist so that a component test can state the one fact it is about - a
 * connection that is reconnecting, a server that reported an error - without a
 * socket, a server or a protocol. The doubles are shallow on purpose: they
 * record what a component asked for and let a test drive the answers, and
 * everything that decides *whether* to ask lives in the client's own suite.
 */
import { createElement } from 'react'
import { vi } from 'vitest'

import type {
  ConnectionStatus,
  SubscriptionHandlers,
  TerminalClient,
  TerminalSubscription,
} from '../terminal/client'
import type { ControlMessage, ControlView, ErrorMessage } from '../terminal/protocol'
import type { ControlState, TerminalSession, TerminalSink } from '../terminal/useTerminal'

export function makeSubscription(projectId = 'p_0123456789abcdef0123'): TerminalSubscription {
  return {
    projectId,
    input: vi.fn(),
    resize: vi.fn(),
    resync: vi.fn(),
    requestControl: vi.fn(),
    releaseControl: vi.fn(),
    acceptControl: vi.fn(),
    rejectControl: vi.fn(),
    release: vi.fn(),
  }
}

/** The client identifier these doubles use, and the one a controller's roster names. */
export const CLIENT_ID = 'c_0123456789abcdef'

/** A roster with nobody in charge, which is what a project starts as. */
export function makeControlView(overrides: Partial<ControlView> = {}): ControlView {
  return { controller: null, suspended: false, expiresAt: '', viewers: 0, pending: [], ...overrides }
}

/**
 * A roster for a project this client controls.
 *
 * It is the default for a session double, because most component tests are
 * about a person using their own terminal rather than about who may use it: a
 * test that is about authority says so by overriding this.
 */
export function makeControlState(overrides: Partial<ControlState> = {}): ControlState {
  const view = makeControlView({ controller: { clientId: CLIENT_ID, device: 'Chrome on Windows' } })
  return { view, held: true, waiting: false, reason: '', ...overrides }
}

/** A connection status that has nothing to explain, which most tests want. */
export function makeStatus(overrides: Partial<ConnectionStatus> = {}): ConnectionStatus {
  return { state: 'open', message: '', attempt: 0, ...overrides }
}

export interface SessionDouble {
  session: TerminalSession
  /** The sinks the session was handed, most recent last. */
  readonly sinks: TerminalSink[]
  /** The sink the view attached, or undefined if it has not rendered one. */
  current(): TerminalSink | undefined
}

/**
 * makeSession builds a session a test can both read and drive.
 *
 * `attach` records the sink rather than ignoring it, which is what lets a test
 * deliver a frame to the view the way the client would.
 */
export function makeSession(overrides: Partial<TerminalSession> = {}): SessionDouble {
  const sinks: TerminalSink[] = []
  const session: TerminalSession = {
    projectId: 'p_0123456789abcdef0123',
    status: makeStatus(),
    error: null,
    ended: false,
    control: makeControlState(),
    clearError: vi.fn(),
    attach(sink: TerminalSink) {
      sinks.push(sink)
      return () => {
        const index = sinks.indexOf(sink)
        if (index >= 0) sinks.splice(index, 1)
      }
    },
    setSize: vi.fn(),
    input: vi.fn(),
    resize: vi.fn(),
    requestControl: vi.fn(),
    releaseControl: vi.fn(),
    acceptControl: vi.fn(),
    rejectControl: vi.fn(),
    askAgain: vi.fn(),
    ...overrides,
  }
  return { session, sinks, current: () => sinks.at(-1) }
}

export interface ClientDouble {
  client: TerminalClient
  /** Every subscribe call, in order, as the arguments it was given. */
  readonly subscribes: Array<{ projectId: string; size: unknown }>
  /** The last subscription handed out, if any. */
  current(): TerminalSubscription | undefined
  /**
   * deliverControl hands the last subscription a roster, as the client would.
   *
   * The real client parses a control message and calls these handlers, and this
   * is the same call with the parsing already done - which is what lets a
   * component test be about what the chrome does with a roster rather than about
   * how one is read off the wire.
   */
  deliverControl(view: ControlView, message?: ControlMessage): void
  /** Delivers output to the last subscription, as the client does for arriving bytes. */
  deliverOutput(payload: Uint8Array): void
  /** Hands the last subscription a fresh screen, as the server sends on subscribe. */
  deliverSnapshot(payload: Uint8Array, cols?: number, rows?: number): void
  /**
   * deliverError reports a refusal about the last subscription, as the client
   * does for an error frame that names a project.
   *
   * The distinction it exists to let a test draw is the one the server draws:
   * an error frame is a refusal of one message, and the connection behind it is
   * still open. A surface that cannot tell those apart cannot be tested for
   * telling them apart.
   */
  deliverError(message: ErrorMessage): void
  /** Ends the last subscription, as the server does when a runtime goes away. */
  deliverEnded(): void
}

/**
 * makeClient builds a client whose connection never changes.
 *
 * A test that needs the connection to change passes a status and re-renders, or
 * drives the listener itself - the point is that the component reads a value
 * rather than owning one.
 */
export function makeClient(overrides: Partial<TerminalClient> = {}): ClientDouble {
  const subscribes: Array<{ projectId: string; size: unknown }> = []
  let last: TerminalSubscription | undefined
  let handlers: SubscriptionHandlers | undefined
  let watched = ''

  const client: TerminalClient = {
    status: makeStatus(),
    clientId: CLIENT_ID,
    onStatusChange: () => () => {},
    subscribe(projectId, size, given) {
      subscribes.push({ projectId, size })
      handlers = given
      watched = projectId
      last = makeSubscription(projectId)
      return last
    },
    retry: vi.fn(),
    close: vi.fn(),
    ...overrides,
  }
  return {
    client,
    subscribes,
    current: () => last,
    deliverControl(view, message = { type: 'control.changed', projectId: watched, control: view }) {
      handlers?.control(view, message)
    },
    deliverOutput(payload) {
      handlers?.draw(payload)
    },
    deliverSnapshot(payload, cols = 80, rows = 24) {
      handlers?.show(payload, cols, rows)
    },
    deliverError(message) {
      handlers?.report(message)
    },
    deliverEnded() {
      handlers?.ended()
    },
  }
}

/** A server error about one project's terminal. */
export function makeTerminalError(overrides: Partial<ErrorMessage> = {}): ErrorMessage {
  return {
    type: 'error',
    code: 'stream_unstable',
    message: 'The terminal outran the connection.',
    projectId: 'p_0123456789abcdef0123',
    ...overrides,
  }
}

/**
 * FakeTerminalView stands in for the xterm host in a component test.
 *
 * Written with createElement rather than JSX so that it can live in a .ts file
 * and be reached from a vi.mock factory by any suite that needs it. What it
 * replaces is a third-party renderer; the wiring it is given is asserted in
 * TerminalView's own suite, against a double for xterm itself.
 */
export function FakeTerminalView({ interactive }: { interactive: boolean }) {
  return createElement('div', {
    'data-testid': 'terminal-screen',
    'data-interactive': interactive ? 'yes' : 'no',
  })
}
