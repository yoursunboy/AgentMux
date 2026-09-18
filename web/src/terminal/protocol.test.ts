/**
 * The wire protocol, tested against hand-built bytes.
 *
 * Every frame in this file is assembled here rather than produced by a helper
 * the decoder and the test share, because a shared builder would agree with the
 * decoder about a wrong offset and the test would pass on a wire format no
 * server emits. The offsets are written out as literals for that reason: they
 * are the contract with internal/terminal/frame.go, and a test that restates
 * them is the only thing that catches a change to one side.
 */
import { describe, expect, it } from 'vitest'

import {
  accounts,
  decodeControlView,
  decodeFrame,
  decodeServerMessage,
  encodeInput,
  FLAG_ALTERNATE_SCREEN,
  FrameError,
  FrameType,
  FRAME_VERSION,
  isAlternate,
  isController,
  isSnapshot,
  isWaiting,
  MAX_CLIENT_ID_LENGTH,
  MAX_PROJECT_ID_LENGTH,
  newClientId,
  OUTPUT_HEADER_BYTES,
  PROTOCOL_VERSION,
  ServerMsg,
  SNAPSHOT_HEADER_BYTES,
  splitOnCharacterBoundaries,
  validClientId,
} from './protocol'

const PROJECT_ID = 'p_0123456789abcdef0123'

interface FrameSpec {
  type: number
  first: number
  last: number
  projectId?: string
  payload?: Uint8Array | string
  cols?: number
  rows?: number
  flags?: number
  version?: number
}

/**
 * frame builds one binary frame by hand.
 *
 * The uint64 words are split with a division rather than a shift, for the same
 * reason the decoder multiplies rather than shifts.
 */
function frame(spec: FrameSpec): Uint8Array {
  const id = new TextEncoder().encode(spec.projectId ?? PROJECT_ID)
  const payload =
    typeof spec.payload === 'string'
      ? new TextEncoder().encode(spec.payload)
      : (spec.payload ?? new Uint8Array(0))
  const snapshot = spec.type === FrameType.snapshot

  const head = new Uint8Array(1 + 1 + 8 + 8 + 2)
  head[0] = spec.version ?? FRAME_VERSION
  head[1] = spec.type
  writeUint64(head, 2, spec.first)
  writeUint64(head, 10, spec.last)
  head[18] = (id.byteLength >> 8) & 0xff
  head[19] = id.byteLength & 0xff

  const geometry = new Uint8Array(snapshot ? 5 : 0)
  if (snapshot) {
    const cols = spec.cols ?? 0
    const rows = spec.rows ?? 0
    geometry[0] = (cols >> 8) & 0xff
    geometry[1] = cols & 0xff
    geometry[2] = (rows >> 8) & 0xff
    geometry[3] = rows & 0xff
    geometry[4] = spec.flags ?? 0
  }

  const out = new Uint8Array(head.byteLength + id.byteLength + geometry.byteLength + payload.byteLength)
  out.set(head, 0)
  out.set(id, head.byteLength)
  out.set(geometry, head.byteLength + id.byteLength)
  out.set(payload, head.byteLength + id.byteLength + geometry.byteLength)
  return out
}

function writeUint64(target: Uint8Array, offset: number, value: number): void {
  const high = Math.floor(value / 0x1_0000_0000)
  const low = value % 0x1_0000_0000
  target[offset] = (high >>> 24) & 0xff
  target[offset + 1] = (high >>> 16) & 0xff
  target[offset + 2] = (high >>> 8) & 0xff
  target[offset + 3] = high & 0xff
  target[offset + 4] = (low >>> 24) & 0xff
  target[offset + 5] = (low >>> 16) & 0xff
  target[offset + 6] = (low >>> 8) & 0xff
  target[offset + 7] = low & 0xff
}

function text(bytes: Uint8Array): string {
  return new TextDecoder().decode(bytes)
}

describe('frame headers', () => {
  it('sizes the two headers as the Go constants do', () => {
    // 1 version + 1 type + 8 first + 8 last + 2 identifier length.
    expect(OUTPUT_HEADER_BYTES).toBe(20)
    // and a snapshot adds 2 cols + 2 rows + 1 flags.
    expect(SNAPSHOT_HEADER_BYTES).toBe(25)
  })

  it('reads an output frame', () => {
    const decoded = decodeFrame(frame({ type: FrameType.output, first: 7, last: 7, payload: 'hi' }))

    expect(decoded.type).toBe(FrameType.output)
    expect(decoded.firstSequence).toBe(7)
    expect(decoded.lastSequence).toBe(7)
    expect(decoded.projectId).toBe(PROJECT_ID)
    expect(text(decoded.payload)).toBe('hi')
    expect(isSnapshot(decoded)).toBe(false)
  })

  it('reads a run of chunks as one frame', () => {
    // The server batches: several chunks are published as one frame whose
    // sequence range is the whole run.
    const decoded = decodeFrame(frame({ type: FrameType.output, first: 12, last: 40, payload: 'x' }))

    expect(decoded.firstSequence).toBe(12)
    expect(decoded.lastSequence).toBe(40)
  })

  it('reads a snapshot, geometry and all', () => {
    const decoded = decodeFrame(
      frame({
        type: FrameType.snapshot,
        first: 100,
        last: 100,
        cols: 120,
        rows: 40,
        flags: FLAG_ALTERNATE_SCREEN,
        payload: '[2J',
      }),
    )

    expect(isSnapshot(decoded)).toBe(true)
    expect(decoded.cols).toBe(120)
    expect(decoded.rows).toBe(40)
    expect(decoded.flags).toBe(FLAG_ALTERNATE_SCREEN)
    expect(isAlternate(decoded)).toBe(true)
    expect(text(decoded.payload)).toBe('[2J')
  })

  it('reads a snapshot with no payload at all', () => {
    // An empty alternate screen is a real state: a full-screen program that has
    // just started has cleared the screen and drawn nothing yet.
    const decoded = decodeFrame(
      frame({ type: FrameType.snapshot, first: 3, last: 3, cols: 80, rows: 24, payload: '' }),
    )

    expect(decoded.payload.byteLength).toBe(0)
  })

  it('carries a payload of every byte value without changing it', () => {
    const payload = new Uint8Array(256)
    for (let i = 0; i < 256; i++) payload[i] = i

    const decoded = decodeFrame(frame({ type: FrameType.output, first: 1, last: 1, payload }))

    expect(Array.from(decoded.payload)).toEqual(Array.from(payload))
  })

  it('reads sequences above 2^31 as positive numbers', () => {
    // The reason readUint64 multiplies instead of shifting: a JavaScript bitwise
    // operator coerces to int32, and a runtime that has been producing output
    // for a few days passes this number.
    const decoded = decodeFrame(
      frame({ type: FrameType.output, first: 3_000_000_000, last: 3_000_000_001 }),
    )

    expect(decoded.firstSequence).toBe(3_000_000_000)
    expect(decoded.lastSequence).toBe(3_000_000_001)
  })

  it('reads a sequence above 2^32 without losing it', () => {
    // The high word is exercised here and not above: a sequence that has rolled
    // past 2^32 is one a long-lived runtime reaches, and 2^53 is the last value
    // JavaScript can hold exactly, so this is the boundary of the conversion.
    const boundary = 2 ** 53
    const decoded = decodeFrame(
      frame({ type: FrameType.output, first: boundary, last: boundary + 1 }),
    )

    expect(decoded.firstSequence).toBe(boundary)
    expect(decoded.lastSequence).toBe(boundary + 1)
  })

  it('accepts an ArrayBuffer as well as a view', () => {
    const bytes = frame({ type: FrameType.output, first: 1, last: 1, payload: 'ok' })
    const decoded = decodeFrame(bytes.buffer.slice(0) as ArrayBuffer)

    expect(text(decoded.payload)).toBe('ok')
  })

  it('reads a view that does not start at the beginning of its buffer', () => {
    // A subarray of a larger buffer is what a batching reader produces, and an
    // offset ignored here would decode somebody else's bytes.
    const bytes = frame({ type: FrameType.output, first: 5, last: 5, payload: 'slice' })
    const padded = new Uint8Array(bytes.byteLength + 8)
    padded.set(bytes, 8)

    const decoded = decodeFrame(padded.subarray(8))

    expect(decoded.firstSequence).toBe(5)
    expect(text(decoded.payload)).toBe('slice')
  })
})

describe('frame refusals', () => {
  const refusals: Array<[string, () => Uint8Array, string]> = [
    [
      'a frame shorter than its header',
      () => new Uint8Array(OUTPUT_HEADER_BYTES - 1),
      'short_frame',
    ],
    [
      'an unknown frame version',
      () => frame({ type: FrameType.output, first: 1, last: 1, version: 9 }),
      'frame_version',
    ],
    [
      'an unknown frame type',
      () => frame({ type: 0x7f, first: 1, last: 1 }),
      'frame_type',
    ],
    [
      'a sequence range that runs backwards',
      () => frame({ type: FrameType.output, first: 9, last: 4 }),
      'frame_sequence',
    ],
    [
      'a zero-length project identifier',
      () => frame({ type: FrameType.output, first: 1, last: 1, projectId: '' }),
      'frame_project_id',
    ],
    [
      'a project identifier longer than the limit',
      () =>
        frame({
          type: FrameType.output,
          first: 1,
          last: 1,
          projectId: 'p'.repeat(MAX_PROJECT_ID_LENGTH + 1),
        }),
      'frame_project_id',
    ],
    [
      'an identifier length longer than the frame',
      () => {
        const bytes = frame({ type: FrameType.output, first: 1, last: 1 })
        // Claim far more identifier than the frame carries. The length is the
        // only thing a decoder reads before it trusts the body, so it is the one
        // field an attacker controls without also controlling the bytes.
        bytes[18] = 0
        bytes[19] = 60
        return bytes
      },
      'short_frame',
    ],
    [
      'a snapshot with nowhere to put its geometry',
      () => {
        // A snapshot that ends at the identifier: 20 + 22 bytes and no header.
        const spec = frame({ type: FrameType.output, first: 1, last: 1 })
        spec[1] = FrameType.snapshot
        return spec
      },
      'short_frame',
    ],
    [
      'a snapshot with only part of its geometry',
      () => {
        const base = frame({ type: FrameType.output, first: 1, last: 1 })
        const bytes = new Uint8Array(base.byteLength + 4)
        bytes.set(base, 0)
        bytes[1] = FrameType.snapshot
        return bytes
      },
      'short_frame',
    ],
  ]

  for (const [name, build, code] of refusals) {
    it(`refuses ${name}`, () => {
      let thrown: unknown
      try {
        decodeFrame(build())
      } catch (error) {
        thrown = error
      }
      expect(thrown).toBeInstanceOf(FrameError)
      expect((thrown as FrameError).code).toBe(code)
    })
  }
})

describe('the gap rule', () => {
  const output = (first: number, last: number) =>
    decodeFrame(frame({ type: FrameType.output, first, last }))

  it('accounts for a frame that continues where the last one stopped', () => {
    // The rule counts chunks, not frames: a batched frame reports the run it
    // carried, so the next one starts one past its end.
    expect(accounts(output(11, 20), 10)).toBe(true)
  })

  it('reports a gap when a frame starts past where the client is', () => {
    // The watcher was dropped for being slow. The bytes in between are gone and
    // no later frame will carry them.
    expect(accounts(output(21, 30), 10)).toBe(false)
  })

  it('reports a gap when a frame repeats what the client already drew', () => {
    expect(accounts(output(5, 10), 10)).toBe(false)
  })

  it('treats a snapshot as accounting for everything, whatever it says', () => {
    // A snapshot is a fresh statement about the whole terminal, so it is not a
    // continuation of anything and cannot be out of order.
    const snapshot = decodeFrame(
      frame({ type: FrameType.snapshot, first: 900, last: 900, cols: 80, rows: 24 }),
    )
    expect(accounts(snapshot, 10)).toBe(true)
    expect(accounts(snapshot, 0)).toBe(true)
  })

  it('accounts for the first frame of a fresh subscription', () => {
    expect(accounts(output(1, 1), 0)).toBe(true)
  })
})

describe('control messages', () => {
  it('reads the greeting', () => {
    const message = decodeServerMessage(
      JSON.stringify({
        type: 'hello',
        protocol: PROTOCOL_VERSION,
        clientId: 'c_0123456789abcdef',
        connectionId: 'ws_000001',
        device: 'Chrome on Windows',
        server: 'AgentMux',
        version: '0.1.0',
      }),
    )

    expect(message).toEqual({
      type: 'hello',
      protocol: PROTOCOL_VERSION,
      clientId: 'c_0123456789abcdef',
      connectionId: 'ws_000001',
      device: 'Chrome on Windows',
      server: 'AgentMux',
      version: '0.1.0',
    })
  })

  it('reads each message this protocol defines', () => {
    for (const type of Object.values(ServerMsg)) {
      expect(decodeServerMessage(JSON.stringify({ type }))?.type).toBe(type)
    }
  })

  const refusals: Array<[string, string]> = [
    ['text that is not JSON', 'not json at all'],
    ['a JSON string rather than an object', '"hello"'],
    ['a JSON array', '[{"type":"hello"}]'],
    ['null', 'null'],
    ['a number', '42'],
    ['an object with no type', '{"clientId":"c1"}'],
    ['an object whose type is not a string', '{"type":7}'],
    ['a type this protocol does not define', '{"type":"shutdown"}'],
  ]

  for (const [name, payload] of refusals) {
    it(`refuses ${name}`, () => {
      // Null rather than an exception: a parse failure arrives on the wire, and
      // the caller answers it the same way it answers a malformed frame - by
      // re-establishing - which is not what an exception out of a message
      // handler would do.
      expect(decodeServerMessage(payload)).toBeNull()
    })
  }
})

describe('the client identifier', () => {
  it('accepts what the server issues', () => {
    expect(validClientId(newClientId())).toBe(true)
  })

  it('refuses anything that is not the shape the server accepts', () => {
    // A shape check, not a permission - but it is what stops a stored value
    // from being offered as an identifier when it is really a path, or a
    // sentence, or another protocol's idea of an identifier.
    for (const value of [
      '',
      'c_',
      'c_ABCDEF',
      'c_0123-4567',
      '0123456789abcdef',
      'c_../../etc/passwd',
      `c_${'a'.repeat(MAX_CLIENT_ID_LENGTH)}`,
    ]) {
      expect(validClientId(value)).toBe(false)
    }
  })

  it('invents a different one every time', () => {
    const seen = new Set<string>()
    for (let i = 0; i < 200; i++) seen.add(newClientId())

    expect(seen.size).toBe(200)
  })
})

describe('reading a roster', () => {
  const roster = { controller: null, suspended: false, expiresAt: '', viewers: 0, pending: [] }

  it('reads one with nobody in charge', () => {
    expect(decodeControlView(roster)).toEqual(roster)
  })

  it('reads the controller, the queue and the count', () => {
    const view = decodeControlView({
      controller: { clientId: 'c_aaaa', device: 'Safari on iPad' },
      suspended: true,
      expiresAt: '2026-09-18T12:00:00Z',
      viewers: 2,
      pending: [{ clientId: 'c_bbbb', device: 'Chrome on Android' }],
    })

    expect(view).toEqual({
      controller: { clientId: 'c_aaaa', device: 'Safari on iPad' },
      suspended: true,
      expiresAt: '2026-09-18T12:00:00Z',
      viewers: 2,
      pending: [{ clientId: 'c_bbbb', device: 'Chrome on Android' }],
    })
  })

  it('refuses anything it could not draw a header from', () => {
    // The failure this prevents is a bar assembled from the fields that
    // happened to parse, which can name the wrong device as the one typing.
    for (const raw of [
      null,
      'a roster',
      {},
      { ...roster, suspended: 'yes' },
      { ...roster, viewers: 'two' },
      { ...roster, controller: { clientId: 'c_aaaa' } },
      { ...roster, controller: { device: 'iPad' } },
      { ...roster, pending: 'none' },
      { ...roster, pending: [{ clientId: 'c_bbbb' }] },
    ]) {
      expect(decodeControlView(raw)).toBeNull()
    }
  })

  it('treats a missing queue as no queue', () => {
    // It is the one field with a safe default: a client that has no queue and a
    // client that was not told about one draw the same header.
    const view = decodeControlView({
      controller: null,
      suspended: false,
      viewers: 1,
    })

    expect(view?.pending).toEqual([])
    expect(view?.expiresAt).toBe('')
  })

  it('answers who is in charge from this client’s own identifier', () => {
    const held = decodeControlView({ ...roster, controller: { clientId: 'c_aaaa', device: 'iPad' } })

    expect(isController(held, 'c_aaaa')).toBe(true)
    expect(isController(held, 'c_bbbb')).toBe(false)
    expect(isController(null, 'c_aaaa')).toBe(false)
    expect(isController(held, null)).toBe(false)
  })

  it('answers whether this client is in the queue', () => {
    const view = decodeControlView({
      ...roster,
      pending: [{ clientId: 'c_bbbb', device: 'Chrome on Android' }],
    })

    expect(isWaiting(view, 'c_bbbb')).toBe(true)
    expect(isWaiting(view, 'c_aaaa')).toBe(false)
    expect(isWaiting(null, 'c_bbbb')).toBe(false)
  })
})

describe('splitting input on character boundaries', () => {
  it('returns nothing for no bytes', () => {
    expect(splitOnCharacterBoundaries(new Uint8Array(0), 8)).toEqual([])
  })

  it('returns the whole thing when it fits', () => {
    const bytes = new TextEncoder().encode('hello')
    const pieces = splitOnCharacterBoundaries(bytes, 8)

    expect(pieces).toHaveLength(1)
    expect(text(pieces[0]!)).toBe('hello')
  })

  it('cuts plain ASCII at the limit', () => {
    const pieces = splitOnCharacterBoundaries(new TextEncoder().encode('abcdef'), 4)

    expect(pieces.map(text)).toEqual(['abcd', 'ef'])
  })

  it('never cuts inside a multi-byte character', () => {
    // Four three-byte characters with a limit of 4: a naive split would put one
    // byte of the second character in the first piece, and the terminal would
    // print replacement characters rather than the letters somebody typed.
    const bytes = new TextEncoder().encode('日本語字')
    const pieces = splitOnCharacterBoundaries(bytes, 4)

    expect(pieces.map((piece) => piece.byteLength)).toEqual([3, 3, 3, 3])
    expect(pieces.map(text).join('')).toBe('日本語字')
  })

  it('sends a character longer than the limit whole rather than corrupting it', () => {
    // Nothing can be done with a limit below one character but to exceed it: a
    // split would corrupt the character, and the server's own limit is what
    // would refuse it either way.
    const bytes = new TextEncoder().encode('😀')
    const pieces = splitOnCharacterBoundaries(bytes, 2)

    expect(pieces).toHaveLength(1)
    expect(text(pieces[0]!)).toBe('😀')
  })

  it('makes progress when every character is longer than the limit', () => {
    // The fallback has to advance, or a limit below one character's width is an
    // infinite loop rather than a wrong answer.
    const source = '😀😀😀😀'
    const pieces = splitOnCharacterBoundaries(new TextEncoder().encode(source), 1)

    expect(pieces.map(text)).toEqual(['😀', '😀', '😀', '😀'])
    expect(pieces.map(text).join('')).toBe(source)
  })

  it('keeps the bytes in order across many pieces', () => {
    const source = 'x'.repeat(3) + 'é'.repeat(50) + 'y'.repeat(3)
    const bytes = new TextEncoder().encode(source)
    const pieces = splitOnCharacterBoundaries(bytes, 7)

    expect(pieces.length).toBeGreaterThan(10)
    expect(pieces.map(text).join('')).toBe(source)
    // Every piece must itself be valid text, which is the property a bad cut
    // breaks.
    for (const piece of pieces) expect(text(piece)).not.toContain('�')
  })

  it('refuses a limit that could never terminate', () => {
    expect(() => splitOnCharacterBoundaries(new Uint8Array(4), 0)).toThrow(RangeError)
    expect(() => splitOnCharacterBoundaries(new Uint8Array(4), -1)).toThrow(RangeError)
  })
})

describe('encoding input', () => {
  /** decodeBase64 turns a sent `data` field back into the text it carries. */
  function decodeBase64(encoded: string): string {
    const binary = atob(encoded)
    const bytes = new Uint8Array(binary.length)
    for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)
    return new TextDecoder().decode(bytes)
  }

  it('sends nothing for empty text', () => {
    expect(encodeInput('')).toEqual([])
  })

  it('carries the bytes of a keystroke', () => {
    const [message] = encodeInput('\r')

    expect(decodeBase64(message!)).toBe('\r')
  })

  it('carries escape sequences unchanged', () => {
    // How Ctrl+C, the arrow keys and a paste reach the terminal: the browser
    // hands over bytes and nothing here may reinterpret them.
    const [message] = encodeInput('')

    expect(decodeBase64(message!)).toBe('')
  })

  it('splits a paste that is longer than one message may carry', () => {
    const paste = 'a'.repeat(100)
    const messages = encodeInput(paste, 30)

    expect(messages).toHaveLength(4)
    expect(messages.map(decodeBase64).join('')).toBe(paste)
  })

  it('does not corrupt a multi-byte character at a split', () => {
    const paste = 'é'.repeat(40)
    const messages = encodeInput(paste, 9)

    expect(messages.length).toBeGreaterThan(1)
    for (const message of messages) expect(decodeBase64(message)).not.toContain('�')
    expect(messages.map(decodeBase64).join('')).toBe(paste)
  })

  it('encodes a large paste without exhausting the argument list', () => {
    // The reason base64Encode chunks: String.fromCharCode(...bytes) passes one
    // argument per byte, and this is the size a person reaches by pasting.
    const paste = 'x'.repeat(200_000)
    const messages = encodeInput(paste)

    expect(messages.length).toBeGreaterThan(1)
    expect(messages.map(decodeBase64).join('').length).toBe(200_000)
  })
})
