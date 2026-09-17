/**
 * A WebSocket a test drives by hand.
 *
 * Every interesting case in the terminal client is a socket that closes at an
 * awkward moment - during a handshake, between a subscribe and its snapshot,
 * after a frame that half-parsed. Waiting for a real server to produce those is
 * not a test anybody runs, so the socket is replaced with one whose every event
 * happens when the test says so.
 *
 * It deliberately does not `implements WebSocket`. Satisfying that interface
 * would mean stubbing `addEventListener` overloads and `dispatchEvent`, none of
 * which the client calls - and a double that carries methods nothing uses reads
 * like a socket that supports more than it does.
 */

export interface FakeSocketRecord {
  url: string
  protocols: string[]
  readyState: number
  binaryType: string
  sent: string[]
  closed: boolean
}

export class FakeSocket {
  /** Every socket built since the last reset, oldest first. */
  static instances: FakeSocket[] = []

  static reset(): void {
    FakeSocket.instances = []
  }

  /** The socket built most recently, which is the one under test. */
  static get last(): FakeSocket {
    const socket = FakeSocket.instances.at(-1)
    if (!socket) throw new Error('no socket has been opened')
    return socket
  }

  readonly url: string
  readonly protocols: string[]

  /** WebSocket.CONNECTING. A socket starts here and only a test moves it. */
  readyState = 0
  binaryType = 'blob'
  closed = false

  /**
   * The messages sent, as raw strings.
   *
   * Recorded as text rather than parsed, because "nothing was sent" and
   * "something was sent that does not parse" are different findings and a double
   * that parsed on arrival could not tell them apart.
   */
  readonly sent: string[] = []

  onopen: (() => void) | null = null
  onmessage: ((event: { data: unknown }) => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null

  constructor(url: string, protocols: string[] = []) {
    this.url = url
    this.protocols = protocols
    FakeSocket.instances.push(this)
  }

  send(data: string): void {
    if (this.readyState !== 1) throw new Error('the socket is not open')
    this.sent.push(data)
  }

  close(): void {
    if (this.closed) return
    this.closed = true
    this.readyState = 3
    this.onclose?.()
  }

  // -------------------------------------------------------------------------
  // What only a test does

  /** accept completes the handshake, as a server that agreed would. */
  accept(): void {
    this.readyState = 1
    this.onopen?.()
  }

  /** deliver hands the client one message. */
  deliver(data: unknown): void {
    this.onmessage?.({ data })
  }

  /** deliverText hands the client one control message. */
  deliverText(message: unknown): void {
    this.deliver(typeof message === 'string' ? message : JSON.stringify(message))
  }

  /**
   * drop ends the connection without a handshake having completed, which is what
   * a refused upgrade looks like from the browser's side.
   */
  drop(): void {
    if (this.closed) return
    this.closed = true
    this.readyState = 3
    this.onclose?.()
  }

  /** The messages sent, parsed. */
  messages(): Array<Record<string, unknown>> {
    return this.sent.map((text) => JSON.parse(text) as Record<string, unknown>)
  }

  /** The messages sent of one type. */
  messagesOfType(type: string): Array<Record<string, unknown>> {
    return this.messages().filter((message) => message.type === type)
  }
}

/**
 * socketFactory returns a factory that records into FakeSocket.instances, so a
 * test can pass it straight to createTerminalClient.
 */
export function fakeSocketFactory(): (url: string, protocols: string[]) => WebSocket {
  return (url, protocols) => new FakeSocket(url, protocols) as unknown as WebSocket
}
