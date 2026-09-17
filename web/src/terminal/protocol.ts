/**
 * The terminal wire protocol, as a browser sees it.
 *
 * This file is a mirror of `internal/terminal/protocol.go` and
 * `internal/terminal/frame.go`. The Go files are the authority: every constant
 * here is a copy of a constant there, and where the two disagree the Go one is
 * right and this one is a bug. They are copied rather than generated because
 * the protocol is small enough to read, and because a generated client would
 * hide the one thing about it a reader most needs to see - that the version is
 * checked in both directions and that a frame's header offsets are a contract
 * rather than a detail.
 *
 * Nothing in this file touches the DOM, a socket, or React. It is the part of
 * the terminal client that can be tested by handing it bytes.
 */

/** The protocol version this client speaks. */
export const PROTOCOL_VERSION = 1

/** The only real-time endpoint the server serves. One socket per browser. */
export const ENDPOINT = '/api/ws'

/** The query parameter a client uses to state the version it speaks. */
export const PROTOCOL_PARAM = 'v'

/**
 * The WebSocket subprotocol name.
 *
 * It is offered on the handshake, and the server echoes it. Offering it is not
 * decoration: a browser that offers a subprotocol and is answered without one
 * fails the connection, so a server that did not list it would refuse every
 * browser while still answering every other client.
 */
export const SUBPROTOCOL = 'agentmux.terminal.v1'

/** Client message types: browser to server. */
export const Msg = {
  subscribe: 'subscribe',
  unsubscribe: 'unsubscribe',
  input: 'input',
  resize: 'resize',
  resync: 'resync',
  ping: 'ping',
} as const

/** Server message types: server to browser. */
export const ServerMsg = {
  hello: 'hello',
  unsubscribed: 'unsubscribed',
  resized: 'resized',
  error: 'error',
  pong: 'pong',
} as const

/**
 * The error codes the terminal layer reports.
 *
 * A separate namespace from the runtime's codes, because a client switching on
 * an error needs to know whether the protocol was spoken wrongly - a bug in
 * this file - or whether the runtime refused, which is a state of the world.
 */
export const TerminalErrorCode = {
  badMessage: 'bad_message',
  unsupported: 'unsupported',
  badProject: 'bad_project',
  tooManySubscriptions: 'too_many_subscriptions',
  notSubscribed: 'not_subscribed',
  inputTooLarge: 'input_too_large',
  streamUnstable: 'stream_unstable',
  internal: 'internal',
} as const

/** Limits. Each one is a copy of a limit the server enforces. */
export const MAX_CLIENT_MESSAGE_BYTES = 160 << 10
export const MAX_INPUT_BYTES = 48 << 10
export const MAX_SUBSCRIPTIONS = 16
export const MAX_PROJECT_ID_LENGTH = 64
export const MIN_COLS = 20
export const MIN_ROWS = 5
export const MAX_COLS = 500
export const MAX_ROWS = 300

// ---------------------------------------------------------------------------
// Server messages

export interface HelloMessage {
  type: 'hello'
  protocol: number
  clientId: string
  server: string
  version: string
}

export interface UnsubscribedMessage {
  type: 'unsubscribed'
  projectId: string
}

export interface ResizedMessage {
  type: 'resized'
  projectId: string
  cols: number
  rows: number
}

export interface ErrorMessage {
  type: 'error'
  code: string
  message: string
  projectId?: string
  about?: string
}

export interface PongMessage {
  type: 'pong'
}

export type ServerMessage =
  | HelloMessage
  | UnsubscribedMessage
  | ResizedMessage
  | ErrorMessage
  | PongMessage

/**
 * decodeServerMessage parses one control message.
 *
 * It returns null rather than throwing for anything that is not a message this
 * protocol defines. The caller treats that as a connection it cannot trust and
 * re-establishes, which is the same answer it gives a malformed frame - and it
 * is deliberately not an exception, because a parse failure here is a thing
 * that happens on the wire rather than a programming error.
 */
export function decodeServerMessage(text: string): ServerMessage | null {
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch {
    return null
  }
  if (typeof parsed !== 'object' || parsed === null) return null
  const type = (parsed as { type?: unknown }).type
  if (typeof type !== 'string') return null
  if (!Object.values(ServerMsg).includes(type as ServerMessage['type'])) return null
  return parsed as ServerMessage
}

// ---------------------------------------------------------------------------
// Binary frames

/** Binary frame types. */
export const FrameType = {
  /** Appends bytes to the terminal. */
  output: 0x01,
  /** Replaces the terminal's contents. */
  snapshot: 0x02,
} as const

/** The version stamped into every binary frame. */
export const FRAME_VERSION = 1

/** Snapshot flags. */
export const FLAG_ALTERNATE_SCREEN = 0x01

/** Header sizes, excluding the project identifier. */
export const OUTPUT_HEADER_BYTES = 1 + 1 + 8 + 8 + 2
export const SNAPSHOT_HEADER_BYTES = OUTPUT_HEADER_BYTES + 2 + 2 + 1

/** Frame decoding error codes, mirroring the Go decoder's sentinel errors. */
export type FrameErrorCode =
  | 'short_frame'
  | 'frame_version'
  | 'frame_type'
  | 'frame_project_id'
  | 'frame_sequence'

/** A frame that cannot be drawn. */
export class FrameError extends Error {
  readonly code: FrameErrorCode

  constructor(code: FrameErrorCode, message: string) {
    super(message)
    this.name = 'FrameError'
    this.code = code
  }
}

export interface DecodedFrame {
  type: number
  /**
   * The run of chunks an output frame accounts for. On a snapshot both are the
   * boundary: the screen renders the terminal as of that sequence number.
   *
   * They are read as a uint64 and held as a JavaScript number. A sequence
   * counts chunks published since a runtime's control connection started, so
   * reaching 2^53 of them would take longer than the machine the server runs on
   * will exist; the conversion is lossless for every value that can occur.
   */
  firstSequence: number
  lastSequence: number
  projectId: string
  /** Set on a snapshot, zero on output. */
  cols: number
  rows: number
  flags: number
  /** The terminal bytes: escape sequences and text. */
  payload: Uint8Array
}

/** isSnapshot reports whether a frame replaces the screen rather than appending. */
export function isSnapshot(frame: DecodedFrame): boolean {
  return frame.type === FrameType.snapshot
}

/** isAlternate reports whether a snapshot came from the alternate screen. */
export function isAlternate(frame: DecodedFrame): boolean {
  return (frame.flags & FLAG_ALTERNATE_SCREEN) !== 0
}

/**
 * accounts reports whether a frame follows the sequence a client has drawn up
 * to, or has lost something.
 *
 * It is the whole of the gap rule, and it is worth stating why it is worth
 * stating: a client that is too strict re-synchronises constantly and one that
 * is too loose draws a terminal with a hole in it and never finds out. A
 * snapshot accounts for everything, because it is a fresh statement about the
 * whole terminal rather than a continuation of anything.
 */
export function accounts(frame: DecodedFrame, last: number): boolean {
  if (isSnapshot(frame)) return true
  return frame.firstSequence === last + 1
}

/**
 * decodeFrame parses one binary frame.
 *
 * It reads the whole frame rather than returning a header to be followed by a
 * payload, because the payload is everything to the end: there is no length
 * field to skip to.
 */
export function decodeFrame(data: ArrayBuffer | Uint8Array): DecodedFrame {
  const bytes = data instanceof Uint8Array ? data : new Uint8Array(data)

  if (bytes.byteLength < OUTPUT_HEADER_BYTES) {
    throw new FrameError('short_frame', `frame is ${bytes.byteLength} bytes, shorter than its header`)
  }
  if (bytes[0] !== FRAME_VERSION) {
    throw new FrameError('frame_version', `unknown frame version ${bytes[0]}`)
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength)
  const firstSequence = readUint64(view, 2)
  const lastSequence = readUint64(view, 10)
  if (lastSequence < firstSequence) {
    throw new FrameError('frame_sequence', `sequence range ${firstSequence}..${lastSequence} is inverted`)
  }

  const idLength = view.getUint16(18)
  if (idLength === 0 || idLength > MAX_PROJECT_ID_LENGTH) {
    throw new FrameError('frame_project_id', `project identifier length ${idLength} is not usable`)
  }
  const body = bytes.subarray(OUTPUT_HEADER_BYTES)
  if (body.byteLength < idLength) {
    throw new FrameError(
      'short_frame',
      `frame declares ${idLength} bytes of identifier and carries ${body.byteLength}`,
    )
  }

  const frame: DecodedFrame = {
    type: bytes[1],
    firstSequence,
    lastSequence,
    projectId: ascii(body.subarray(0, idLength)),
    cols: 0,
    rows: 0,
    flags: 0,
    payload: new Uint8Array(0),
  }

  const rest = body.subarray(idLength)
  switch (frame.type) {
    case FrameType.output:
      frame.payload = rest
      return frame
    case FrameType.snapshot: {
      if (rest.byteLength < 5) {
        throw new FrameError(
          'short_frame',
          `a snapshot header needs 5 bytes and the frame carries ${rest.byteLength}`,
        )
      }
      const geometry = new DataView(rest.buffer, rest.byteOffset, rest.byteLength)
      frame.cols = geometry.getUint16(0)
      frame.rows = geometry.getUint16(2)
      frame.flags = rest[4]
      frame.payload = rest.subarray(5)
      return frame
    }
    default:
      throw new FrameError('frame_type', `unknown frame type 0x${frame.type.toString(16)}`)
  }
}

/**
 * readUint64 reads a big-endian uint64 as a number.
 *
 * The high word is multiplied rather than shifted, because a JavaScript bitwise
 * operator coerces its operands to int32 and would silently produce a negative
 * number for any sequence above 2^31 - which a runtime that has been producing
 * output for a few days can reach.
 */
function readUint64(view: DataView, offset: number): number {
  const high = view.getUint32(offset)
  const low = view.getUint32(offset + 4)
  return high * 0x1_0000_0000 + low
}

/**
 * ascii decodes a run of bytes that the protocol defines as ASCII.
 *
 * A project identifier is `p_` and twenty lowercase hex digits, so it is never
 * anything else. Using the text decoder here would decode a malformed header
 * into replacement characters and produce an identifier that looks like a
 * project and is not one.
 */
function ascii(bytes: Uint8Array): string {
  let out = ''
  for (let i = 0; i < bytes.length; i++) out += String.fromCharCode(bytes[i]!)
  return out
}

// ---------------------------------------------------------------------------
// Terminal input

const encoder = new TextEncoder()

/**
 * splitOnCharacterBoundaries cuts bytes into pieces of at most `limit`, never
 * in the middle of a character.
 *
 * The rule matters because the pieces are sent separately and the terminal
 * receives them as one stream. A cut inside a multi-byte UTF-8 sequence turns
 * one character into two invalid ones, and the program at the other end prints
 * the replacement characters rather than the letter somebody typed. A byte that
 * is not a continuation byte starts a character, which is what makes a cut
 * point findable without decoding anything.
 */
export function splitOnCharacterBoundaries(bytes: Uint8Array, limit: number): Uint8Array[] {
  if (limit <= 0) throw new RangeError('the chunk limit must be positive')
  if (bytes.byteLength <= limit) return bytes.byteLength === 0 ? [] : [bytes]

  const pieces: Uint8Array[] = []
  let start = 0
  while (start < bytes.byteLength) {
    let end = Math.min(start + limit, bytes.byteLength)
    if (end < bytes.byteLength) {
      // Walk back off any continuation byte, so the cut lands where the next
      // character begins.
      while (end > start && (bytes[end]! & 0xc0) === 0x80) end--
      if (end === start) {
        // One character longer than the limit. Nothing can be done about it but
        // to send it whole: a split would corrupt it, and the server's own
        // limit is what would then refuse it. Taking the character means
        // stepping over the continuation bytes that follow the lead byte at
        // `start`, which is also what guarantees the loop advances - a fallback
        // that left `end` where it was would spin forever on this input.
        end = start + 1
        while (end < bytes.byteLength && (bytes[end]! & 0xc0) === 0x80) end++
      }
    }
    pieces.push(bytes.subarray(start, end))
    start = end
  }
  return pieces
}

/**
 * base64 encodes bytes, in chunks.
 *
 * The chunking is not an optimisation. `String.fromCharCode(...bytes)` passes
 * one argument per byte, and a paste of any size at all exceeds the argument
 * count a JavaScript engine will accept - which fails as a stack overflow in
 * the one code path a user reaches by pasting something large.
 */
export function base64Encode(bytes: Uint8Array): string {
  let binary = ''
  const chunk = 0x8000
  for (let i = 0; i < bytes.byteLength; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk))
  }
  return btoa(binary)
}

/**
 * encodeInput turns typed text into the `data` fields of one or more input
 * messages.
 *
 * It returns an array because a paste can exceed what one message may carry.
 * The terminal does not care where the boundaries fall - what it receives is a
 * byte stream - and a client that splits a paste delivers the same bytes in the
 * same order.
 */
export function encodeInput(text: string, limit: number = MAX_INPUT_BYTES): string[] {
  const bytes = encoder.encode(text)
  return splitOnCharacterBoundaries(bytes, limit).map(base64Encode)
}
