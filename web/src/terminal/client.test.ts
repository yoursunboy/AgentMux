/**
 * The terminal client, driven by a socket a test controls.
 *
 * Every case here is a socket that does something awkward at a moment that
 * matters: it closes mid-handshake, it goes away after a first successful
 * connection, it sends a frame out of sequence, or it says something that is not
 * this protocol. Those are the cases a browser meets in ordinary use and the
 * ones that decide whether a terminal is correct or quietly wrong, so they are
 * the ones worth a socket double that obeys the test.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { createTerminalClient, type ConnectionStatus, type TerminalClient } from './client'
import { MAX_INITIAL_ATTEMPTS } from './backoff'
import { FrameType, MAX_INPUT_BYTES, Msg, PROTOCOL_VERSION, ServerMsg, SUBPROTOCOL } from './protocol'
import { FakeSocket, fakeSocketFactory } from '../test/socket'

const URL = 'ws://test/api/ws'
const PROJECT = 'p_0123456789abcdef0123'
const OTHER = 'p_fedcba9876543210fedc'

/** A hello, as the server sends it. */
function hello(protocol: number = PROTOCOL_VERSION): Record<string, unknown> {
  return { type: ServerMsg.hello, protocol, clientId: 'c1', server: 'AgentMux', version: '0.1.0' }
}

/** An output frame carrying text, built by hand at the wire offsets. */
function outputFrame(
  projectId: string,
  first: number,
  last: number,
  payload = 'output',
): ArrayBuffer {
  return frame(FrameType.output, projectId, first, last, new TextEncoder().encode(payload))
}

/** A snapshot, built by hand at the wire offsets. */
function snapshotFrame(
  projectId: string,
  sequence: number,
  cols = 80,
  rows = 24,
  payload = 'screen',
  flags = 0,
): ArrayBuffer {
  return frame(
    FrameType.snapshot,
    projectId,
    sequence,
    sequence,
    new TextEncoder().encode(payload),
    { cols, rows, flags },
  )
}

function frame(
  type: number,
  projectId: string,
  first: number,
  last: number,
  payload: Uint8Array,
  geometry?: { cols: number; rows: number; flags: number },
): ArrayBuffer {
  const id = new TextEncoder().encode(projectId)
  const extra = geometry ? 5 : 0
  const bytes = new Uint8Array(20 + id.byteLength + extra + payload.byteLength)
  bytes[0] = 1
  bytes[1] = type
  writeUint64(bytes, 2, first)
  writeUint64(bytes, 10, last)
  bytes[18] = (id.byteLength >> 8) & 0xff
  bytes[19] = id.byteLength & 0xff
  bytes.set(id, 20)
  if (geometry) {
    bytes[20 + id.byteLength] = (geometry.cols >> 8) & 0xff
    bytes[20 + id.byteLength + 1] = geometry.cols & 0xff
    bytes[20 + id.byteLength + 2] = (geometry.rows >> 8) & 0xff
    bytes[20 + id.byteLength + 3] = geometry.rows & 0xff
    bytes[20 + id.byteLength + 4] = geometry.flags
  }
  bytes.set(payload, 20 + id.byteLength + extra)
  return bytes.buffer
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

function handlers() {
  return {
    draw: vi.fn(),
    show: vi.fn(),
    resize: vi.fn(),
    report: vi.fn(),
    ended: vi.fn(),
  }
}

function decodeBase64(encoded: string): string {
  const binary = atob(encoded)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)
  return new TextDecoder().decode(bytes)
}

describe('terminal client', () => {
  let clients: TerminalClient[]

  beforeEach(() => {
    vi.useFakeTimers()
    FakeSocket.reset()
    clients = []
  })

  afterEach(() => {
    for (const client of clients) client.close()
    vi.useRealTimers()
  })

  /** build returns a client that records itself for cleanup. */
  function build(): TerminalClient {
    const client = createTerminalClient({
      url: URL,
      socketFactory: fakeSocketFactory(),
      // Full nominal delay, so a test advances by a number it can read off the
      // documented table rather than by a range.
      jitter: () => 1,
    })
    clients.push(client)
    return client
  }

  /** open accepts the handshake and greets the client, which is a live socket. */
  function open(socket: FakeSocket): void {
    socket.accept()
    socket.deliverText(hello())
  }

  /**
   * connected builds a client with one live subscription, which is the state
   * most of these tests start from.
   */
  function connected(projectId = PROJECT, sink = handlers()) {
    const client = build()
    const subscription = client.subscribe(projectId, { cols: 100, rows: 30 }, sink)
    const socket = FakeSocket.last
    open(socket)
    return { client, subscription, socket, sink }
  }

  // -------------------------------------------------------------------------

  describe('opening a connection', () => {
    it('opens no socket until something is watched', () => {
      // A page with no terminal on it holds no connection, which is the reason
      // the client is created eagerly and connected lazily.
      const client = build()

      expect(FakeSocket.instances).toHaveLength(0)
      expect(client.status.state).toBe('idle')
    })

    it('opens one socket for several projects', () => {
      // One handshake, one origin check, one reconnect policy - rather than one
      // of each per terminal, which is what a socket per terminal would mean.
      const client = build()
      client.subscribe(PROJECT, null, handlers())
      client.subscribe(OTHER, null, handlers())
      open(FakeSocket.last)

      expect(FakeSocket.instances).toHaveLength(1)
      expect(FakeSocket.last.messagesOfType(Msg.subscribe)).toHaveLength(2)
    })

    it('states the protocol version, and offers the subprotocol a browser needs', () => {
      const { socket } = connected()

      expect(socket.url).toBe(`${URL}?v=${PROTOCOL_VERSION}`)
      // A browser that offers a subprotocol and is answered without one fails
      // the handshake, so this is not decoration.
      expect(socket.protocols).toEqual([SUBPROTOCOL])
    })

    it('carries the size it knows into the subscribe, so the first screen is the right shape', () => {
      const { socket } = connected()

      expect(socket.messagesOfType(Msg.subscribe)[0]).toEqual({
        type: Msg.subscribe,
        projectId: PROJECT,
        cols: 100,
        rows: 30,
      })
    })

    it('subscribes without a size when none is known yet', () => {
      const client = build()
      client.subscribe(PROJECT, null, handlers())
      open(FakeSocket.last)

      expect(FakeSocket.last.messagesOfType(Msg.subscribe)[0]).toEqual({
        type: Msg.subscribe,
        projectId: PROJECT,
      })
    })

    it('publishes its state to listeners, and stops when they unsubscribe', () => {
      const client = build()
      const seen: ConnectionStatus[] = []
      const stop = client.onStatusChange((status) => seen.push(status))

      client.subscribe(PROJECT, null, handlers())
      expect(seen.at(-1)?.state).toBe('connecting')

      open(FakeSocket.last)
      expect(seen.at(-1)?.state).toBe('open')

      const count = seen.length
      stop()
      client.subscribe(OTHER, null, handlers())
      expect(seen).toHaveLength(count)
    })
  })

  // -------------------------------------------------------------------------

  describe('drawing frames', () => {
    it('shows a snapshot, with the geometry it was rendered for', () => {
      const { socket, sink } = connected()

      socket.deliver(snapshotFrame(PROJECT, 12, 120, 40, 'the screen'))

      expect(sink.show).toHaveBeenCalledTimes(1)
      const [payload, cols, rows] = sink.show.mock.calls[0]!
      expect(new TextDecoder().decode(payload)).toBe('the screen')
      expect(cols).toBe(120)
      expect(rows).toBe(40)
      expect(sink.draw).not.toHaveBeenCalled()
    })

    it('draws output that follows the snapshot', () => {
      const { socket, sink } = connected()

      socket.deliver(snapshotFrame(PROJECT, 12))
      socket.deliver(outputFrame(PROJECT, 13, 13, 'next line'))

      expect(sink.draw).toHaveBeenCalledTimes(1)
      expect(new TextDecoder().decode(sink.draw.mock.calls[0]![0])).toBe('next line')
    })

    it('draws a batched frame that accounts for a whole run of chunks', () => {
      const { socket, sink } = connected()

      socket.deliver(snapshotFrame(PROJECT, 10))
      socket.deliver(outputFrame(PROJECT, 11, 25, 'a run'))

      expect(sink.draw).toHaveBeenCalledTimes(1)
    })

    it('ignores a frame for a project it stopped watching', () => {
      // In flight when the unsubscribe was sent. The only thing to do with it is
      // nothing.
      const { socket, sink, subscription } = connected()

      subscription.release()
      socket.deliver(snapshotFrame(PROJECT, 12))

      expect(sink.show).not.toHaveBeenCalled()
    })

    it('ignores a frame for a project it never watched', () => {
      const { socket, sink } = connected()

      socket.deliver(snapshotFrame(OTHER, 12, 80, 24, 'somebody else'))

      expect(sink.show).not.toHaveBeenCalled()
    })

    it('keeps each project’s sequence separate', () => {
      const client = build()
      const first = handlers()
      const second = handlers()
      client.subscribe(PROJECT, null, first)
      client.subscribe(OTHER, null, second)
      const socket = FakeSocket.last
      open(socket)

      socket.deliver(snapshotFrame(PROJECT, 100, 80, 24, 'one'))
      socket.deliver(snapshotFrame(OTHER, 4, 80, 24, 'two'))

      expect(new TextDecoder().decode(first.show.mock.calls[0]![0])).toBe('one')
      expect(new TextDecoder().decode(second.show.mock.calls[0]![0])).toBe('two')
    })
  })

  // -------------------------------------------------------------------------

  describe('sequence gaps', () => {
    it('does not draw a frame that skips ahead, and asks for a screen instead', () => {
      // The watcher was dropped for being slow. Drawing the frames that did
      // arrive would leave a terminal with a hole in it and no sign of one.
      const { socket, sink } = connected()

      socket.deliver(snapshotFrame(PROJECT, 10))
      socket.deliver(outputFrame(PROJECT, 40, 45, 'far ahead'))

      expect(sink.draw).not.toHaveBeenCalled()
      expect(socket.messagesOfType(Msg.resync)).toEqual([{ type: Msg.resync, projectId: PROJECT }])
    })

    it('does not draw a frame that repeats what it already drew', () => {
      const { socket, sink } = connected()

      socket.deliver(snapshotFrame(PROJECT, 10))
      socket.deliver(outputFrame(PROJECT, 11, 11, 'drawn'))
      socket.deliver(outputFrame(PROJECT, 11, 11, 'again'))

      expect(sink.draw).toHaveBeenCalledTimes(1)
      expect(socket.messagesOfType(Msg.resync)).toHaveLength(1)
    })

    it('asks for a screen once, however many frames the gap produces', () => {
      // Several frames arrive between the request and the screen, and every one
      // of them is a gap by definition. One request already caused the answer.
      const { socket, sink } = connected()

      socket.deliver(snapshotFrame(PROJECT, 10))
      socket.deliver(outputFrame(PROJECT, 40, 40, 'a'))
      socket.deliver(outputFrame(PROJECT, 41, 41, 'b'))
      socket.deliver(outputFrame(PROJECT, 42, 42, 'c'))

      expect(sink.draw).not.toHaveBeenCalled()
      expect(socket.messagesOfType(Msg.resync)).toHaveLength(1)
    })

    it('resumes drawing once the screen it asked for arrives', () => {
      const { socket, sink } = connected()

      socket.deliver(snapshotFrame(PROJECT, 10))
      socket.deliver(outputFrame(PROJECT, 40, 40, 'lost'))
      socket.deliver(snapshotFrame(PROJECT, 40, 80, 24, 'fresh'))
      socket.deliver(outputFrame(PROJECT, 41, 41, 'after'))

      expect(sink.draw).toHaveBeenCalledTimes(1)
      expect(new TextDecoder().decode(sink.draw.mock.calls[0]![0])).toBe('after')
    })

    it('treats a snapshot as accounting for everything, wherever its sequence falls', () => {
      const { socket, sink } = connected()

      socket.deliver(snapshotFrame(PROJECT, 10))
      socket.deliver(snapshotFrame(PROJECT, 2, 80, 24, 'an older boundary'))

      expect(sink.show).toHaveBeenCalledTimes(2)
      expect(socket.messagesOfType(Msg.resync)).toHaveLength(0)
    })
  })

  // -------------------------------------------------------------------------

  describe('input', () => {
    it('sends typed characters as bytes on the same channel as everything else', () => {
      const { socket, subscription } = connected()

      subscription.input('a')

      const [message] = socket.messagesOfType(Msg.input)
      expect(message).toMatchObject({ type: Msg.input, projectId: PROJECT })
      expect(decodeBase64(message!.data as string)).toBe('a')
    })

    it('sends a control character unchanged', () => {
      // Ctrl+C, which is the whole reason raw input is raw.
      const { socket, subscription } = connected()

      subscription.input('')

      expect(decodeBase64(socket.messagesOfType(Msg.input)[0]!.data as string)).toBe('')
    })

    it('splits a paste that is longer than one message may carry', () => {
      const { socket, subscription } = connected()

      subscription.input('x'.repeat(MAX_INPUT_BYTES + 10))

      const messages = socket.messagesOfType(Msg.input)
      expect(messages.length).toBeGreaterThan(1)
      expect(messages.map((m) => decodeBase64(m.data as string)).join('')).toHaveLength(
        MAX_INPUT_BYTES + 10,
      )
    })

    it('drops a keystroke typed while the socket is down rather than queueing it', () => {
      // Queueing would deliver it after everything the terminal has done since,
      // which is a worse answer than losing it: the client is showing that it is
      // not connected, so bytes typed into it are bytes the person expects to
      // lose.
      const { socket, subscription } = connected()

      socket.drop()
      subscription.input('typed into nothing')

      expect(socket.messagesOfType(Msg.input)).toHaveLength(0)
      // And nothing follows it onto the next socket either.
      vi.advanceTimersByTime(250)
      open(FakeSocket.last)
      expect(FakeSocket.last.messagesOfType(Msg.input)).toHaveLength(0)
    })

    it('sends nothing after the subscription is released', () => {
      const { socket, subscription } = connected()

      subscription.release()
      subscription.input('too late')

      expect(socket.messagesOfType(Msg.input)).toHaveLength(0)
    })

    it('asks for a size, and asks again for a screen on demand', () => {
      const { socket, subscription } = connected()

      subscription.resize(140, 50)
      subscription.resync()

      expect(socket.messagesOfType(Msg.resize)).toEqual([
        { type: Msg.resize, projectId: PROJECT, cols: 140, rows: 50 },
      ])
      expect(socket.messagesOfType(Msg.resync)).toEqual([{ type: Msg.resync, projectId: PROJECT }])
    })
  })

  // -------------------------------------------------------------------------

  describe('server messages', () => {
    it('reports the size the server actually applied', () => {
      // The server clamps, so its answer is the truth rather than the request.
      const { socket, sink } = connected()

      socket.deliverText({ type: ServerMsg.resized, projectId: PROJECT, cols: 90, rows: 25 })

      expect(sink.resize).toHaveBeenCalledWith(90, 25)
    })

    it('reports that the server stopped sending for a project', () => {
      const { socket, sink } = connected()

      socket.deliverText({ type: ServerMsg.unsubscribed, projectId: PROJECT })

      expect(sink.ended).toHaveBeenCalledTimes(1)
    })

    it('reports an error about a subscription to that subscription', () => {
      const { socket, sink } = connected()

      socket.deliverText({
        type: ServerMsg.error,
        projectId: PROJECT,
        code: 'stream_unstable',
        message: 'the terminal outran the connection',
      })

      expect(sink.report).toHaveBeenCalledWith(
        expect.objectContaining({ code: 'stream_unstable', projectId: PROJECT }),
      )
    })

    it('treats an error about no project in particular as the connection’s', () => {
      const { socket } = connected()

      socket.deliverText({ type: ServerMsg.error, code: 'bad_message', message: 'unreadable' })

      expect(socket.closed).toBe(true)
    })

    it('ignores a message about a project it is not watching', () => {
      const { socket, sink } = connected()

      socket.deliverText({ type: ServerMsg.resized, projectId: OTHER, cols: 90, rows: 25 })

      expect(sink.resize).not.toHaveBeenCalled()
    })
  })

  // -------------------------------------------------------------------------

  describe('a connection it cannot trust', () => {
    it('closes on a control message that is not this protocol', () => {
      const { socket } = connected()

      socket.deliverText('this is not json')

      expect(socket.closed).toBe(true)
    })

    it('closes on a message that arrives before the greeting', () => {
      // The hello is queued before anything else can be, so a message without
      // one means this is not the protocol it claims to be.
      const client = build()
      client.subscribe(PROJECT, null, handlers())
      const socket = FakeSocket.last
      socket.accept()

      socket.deliverText({ type: ServerMsg.pong })

      expect(socket.closed).toBe(true)
    })

    it('closes on a binary frame that cannot be decoded', () => {
      const { socket, sink } = connected()

      socket.deliver(new Uint8Array(4).buffer)

      expect(sink.draw).not.toHaveBeenCalled()
      expect(sink.show).not.toHaveBeenCalled()
      expect(socket.closed).toBe(true)
    })

    it('closes on a frame whose version it does not speak', () => {
      const { socket } = connected()
      const bytes = new Uint8Array(snapshotFrame(PROJECT, 1))
      bytes[0] = 9

      socket.deliver(bytes.buffer)

      expect(socket.closed).toBe(true)
    })

    it('closes on a payload it cannot read at all', () => {
      const { socket } = connected()

      socket.deliver(1234)

      expect(socket.closed).toBe(true)
    })

    it('re-establishes after refusing a connection, rather than drawing part of one', () => {
      const { socket } = connected()
      socket.deliverText('not json')

      expect(socket.closed).toBe(true)
      vi.advanceTimersByTime(250)

      expect(FakeSocket.instances).toHaveLength(2)
    })
  })

  // -------------------------------------------------------------------------

  describe('reconnecting', () => {
    it('re-subscribes every project after a reconnect, and takes a fresh screen', () => {
      // The socket is new, so nothing is subscribed to it. Asking again is what
      // makes a browser survive a server restart with nothing clicked.
      const { client, sink } = connected()
      expect(client.status.state).toBe('open')

      FakeSocket.last.drop()
      expect(client.status.state).toBe('reconnecting')
      expect(client.status.attempt).toBe(1)

      vi.advanceTimersByTime(250)
      const next = FakeSocket.last
      expect(next).not.toBe(undefined)
      open(next)

      expect(next.messagesOfType(Msg.subscribe)).toEqual([
        { type: Msg.subscribe, projectId: PROJECT, cols: 100, rows: 30 },
      ])
      next.deliver(snapshotFrame(PROJECT, 900, 80, 24, 'after the restart'))

      expect(client.status.state).toBe('open')
      expect(new TextDecoder().decode(sink.show.mock.calls[0]![0])).toBe('after the restart')
    })

    it('keeps trying forever once it has connected, because a restart is normal', () => {
      const { client } = connected()

      for (let i = 0; i < 12; i++) {
        FakeSocket.last.drop()
        vi.advanceTimersByTime(10_000)
      }

      expect(client.status.state).not.toBe('failed')
      expect(FakeSocket.instances.length).toBeGreaterThan(MAX_INITIAL_ATTEMPTS)
    })

    it('gives up when it has never connected at all, and says so', () => {
      // Something is wrong that retrying will not fix. A tab that retried
      // forever would be a tab that never tells anyone.
      const client = build()
      client.subscribe(PROJECT, null, handlers())
      const seen: ConnectionStatus[] = []
      client.onStatusChange((status) => seen.push(status))

      for (let i = 0; i < MAX_INITIAL_ATTEMPTS; i++) {
        FakeSocket.last.drop()
        vi.advanceTimersByTime(10_000)
      }

      expect(client.status.state).toBe('failed')
      expect(client.status.message).toMatch(/Could not reach the terminal server/)
      expect(seen.some((status) => status.state === 'failed')).toBe(true)
      // And it stops: no further sockets are built.
      const count = FakeSocket.instances.length
      vi.advanceTimersByTime(60_000)
      expect(FakeSocket.instances).toHaveLength(count)
    })

    it('does not count a refused handshake twice', () => {
      // A WebSocket error is always followed by a close. Handling both would
      // double the attempt count for one failure and give up twice as fast.
      const client = build()
      client.subscribe(PROJECT, null, handlers())

      FakeSocket.last.onerror?.()
      FakeSocket.last.drop()

      expect(client.status.attempt).toBe(1)
    })

    it('retries a client that gave up', () => {
      const client = build()
      client.subscribe(PROJECT, null, handlers())
      for (let i = 0; i < MAX_INITIAL_ATTEMPTS; i++) {
        FakeSocket.last.drop()
        vi.advanceTimersByTime(10_000)
      }
      expect(client.status.state).toBe('failed')

      client.retry()

      expect(client.status.state).toBe('connecting')
      expect(FakeSocket.instances).toHaveLength(MAX_INITIAL_ATTEMPTS + 1)
    })

    it('treats a new subscription as a reason to try again', () => {
      const client = build()
      client.subscribe(PROJECT, null, handlers())
      for (let i = 0; i < MAX_INITIAL_ATTEMPTS; i++) {
        FakeSocket.last.drop()
        vi.advanceTimersByTime(10_000)
      }
      expect(client.status.state).toBe('failed')

      client.subscribe(OTHER, null, handlers())

      expect(client.status.state).toBe('connecting')
    })

    it('backs off further with each attempt', () => {
      const client = build()
      client.subscribe(PROJECT, null, handlers())

      FakeSocket.last.drop()
      vi.advanceTimersByTime(249)
      expect(FakeSocket.instances).toHaveLength(1)
      vi.advanceTimersByTime(1)
      expect(FakeSocket.instances).toHaveLength(2)

      FakeSocket.last.drop()
      vi.advanceTimersByTime(499)
      expect(FakeSocket.instances).toHaveLength(2)
      vi.advanceTimersByTime(1)
      expect(FakeSocket.instances).toHaveLength(3)

      expect(client.status.attempt).toBe(2)
    })

    it('gives up for good when the server speaks another protocol', () => {
      // Retrying cannot help: the page and the server disagree about what they
      // are speaking, and only a reload changes that.
      const client = build()
      client.subscribe(PROJECT, null, handlers())
      const socket = FakeSocket.last
      socket.accept()

      socket.deliverText(hello(PROTOCOL_VERSION + 1))

      expect(client.status.state).toBe('failed')
      expect(client.status.message).toMatch(/protocol/)
      expect(socket.closed).toBe(true)
      vi.advanceTimersByTime(60_000)
      expect(FakeSocket.instances).toHaveLength(1)
    })
  })

  // -------------------------------------------------------------------------

  describe('liveness', () => {
    it('pings, and re-establishes when the answer never comes', () => {
      // A closed laptop wakes up believing it is connected: the socket is still
      // open from the browser's point of view and the server is long gone.
      const { client } = connected()

      vi.advanceTimersByTime(30_000)
      expect(FakeSocket.last.messagesOfType(Msg.ping)).toHaveLength(1)

      vi.advanceTimersByTime(10_000)
      expect(client.status.state).toBe('reconnecting')
    })

    it('is satisfied by an answer', () => {
      const { client, socket } = connected()

      vi.advanceTimersByTime(30_000)
      socket.deliverText({ type: ServerMsg.pong })
      vi.advanceTimersByTime(10_000)

      expect(client.status.state).toBe('open')
      expect(socket.closed).toBe(false)
    })

    it('stops pinging once the subscription is gone', () => {
      const { subscription } = connected()

      subscription.release()
      vi.advanceTimersByTime(120_000)

      expect(FakeSocket.instances).toHaveLength(1)
      expect(FakeSocket.last.messagesOfType(Msg.ping)).toHaveLength(0)
    })
  })

  // -------------------------------------------------------------------------

  describe('releasing', () => {
    it('unsubscribes and closes the socket when nothing is wanted', () => {
      const { client, subscription, socket } = connected()

      subscription.release()

      expect(socket.messagesOfType(Msg.unsubscribe)).toEqual([
        { type: Msg.unsubscribe, projectId: PROJECT },
      ])
      expect(socket.closed).toBe(true)
      expect(client.status.state).toBe('idle')
    })

    it('keeps the socket while another project is still watched', () => {
      const client = build()
      const first = client.subscribe(PROJECT, null, handlers())
      client.subscribe(OTHER, null, handlers())
      const socket = FakeSocket.last
      open(socket)

      first.release()

      expect(socket.closed).toBe(false)
      expect(socket.messagesOfType(Msg.unsubscribe)).toHaveLength(1)
    })

    it('is idempotent', () => {
      const { socket, subscription } = connected()

      subscription.release()
      subscription.release()
      subscription.release()

      expect(socket.messagesOfType(Msg.unsubscribe)).toHaveLength(1)
    })

    it('closes everything when the client is closed', () => {
      const { client, socket } = connected()

      client.close()

      expect(socket.closed).toBe(true)
      expect(client.status.state).toBe('idle')
      vi.advanceTimersByTime(60_000)
      expect(FakeSocket.instances).toHaveLength(1)
    })
  })

  // -------------------------------------------------------------------------

  describe('subscribing twice to the same project', () => {
    it('reuses the subscription rather than taking a second screen', () => {
      // A component that re-renders is not a reason to capture the screen again.
      const client = build()
      client.subscribe(PROJECT, { cols: 100, rows: 30 }, handlers())
      open(FakeSocket.last)

      const second = handlers()
      client.subscribe(PROJECT, { cols: 100, rows: 30 }, second)

      expect(FakeSocket.last.messagesOfType(Msg.subscribe)).toHaveLength(1)
      // And it does not send a redundant resize either, because the size has not
      // changed.
      expect(FakeSocket.last.messagesOfType(Msg.resize)).toHaveLength(0)
    })

    it('sends a resize when the size it is given has changed', () => {
      const client = build()
      client.subscribe(PROJECT, { cols: 100, rows: 30 }, handlers())
      open(FakeSocket.last)

      client.subscribe(PROJECT, { cols: 140, rows: 30 }, handlers())

      expect(FakeSocket.last.messagesOfType(Msg.resize)).toEqual([
        { type: Msg.resize, projectId: PROJECT, cols: 140, rows: 30 },
      ])
    })

    it('draws the new handlers’ frames, not the ones it replaced', () => {
      const client = build()
      const first = handlers()
      client.subscribe(PROJECT, null, first)
      open(FakeSocket.last)
      const second = handlers()
      client.subscribe(PROJECT, null, second)

      FakeSocket.last.deliver(snapshotFrame(PROJECT, 1, 80, 24, 'for the new one'))

      expect(first.show).not.toHaveBeenCalled()
      expect(second.show).toHaveBeenCalledTimes(1)
    })
  })
})
