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

import type { ConnectionStatus, TerminalClient, TerminalSubscription } from '../terminal/client'
import type { ErrorMessage } from '../terminal/protocol'
import type { TerminalSession, TerminalSink } from '../terminal/useTerminal'

export function makeSubscription(projectId = 'p_0123456789abcdef0123'): TerminalSubscription {
  return {
    projectId,
    input: vi.fn(),
    resize: vi.fn(),
    resync: vi.fn(),
    release: vi.fn(),
  }
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

  const client: TerminalClient = {
    status: makeStatus(),
    onStatusChange: () => () => {},
    subscribe(projectId, size) {
      subscribes.push({ projectId, size })
      last = makeSubscription(projectId)
      return last
    },
    retry: vi.fn(),
    close: vi.fn(),
    ...overrides,
  }
  return { client, subscribes, current: () => last }
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
