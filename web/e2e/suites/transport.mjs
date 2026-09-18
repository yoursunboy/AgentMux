/**
 * Phase 4 browser E2E - the transport itself, spoken by hand.
 *
 * The browser is the client that matters, but it is a client that behaves: it
 * reads as fast as the server can write, so it never meets the parts of the
 * transport that exist for a client that does not. This file is a second
 * client, written against the wire protocol directly, so those parts can be
 * reached:
 *
 *   - the sequence numbers, checked for continuity frame by frame rather than
 *     trusted, because "the terminal looks right" is not the same claim as
 *     "nothing was skipped on the way";
 *   - resynchronisation, asked for explicitly and checked to produce a screen
 *     taken after the request rather than a replay of what was missed;
 *   - the slow reader, which is the one case a browser cannot be made to
 *     reproduce and the one that decides whether a stalled tablet costs the
 *     server a bounded amount of memory or an unbounded one;
 *   - the limits, which are the reason a socket is not a way to run commands.
 *
 * It is written on a raw socket rather than with a WebSocket library because
 * the point of the slow-reader case is to stop reading, and a library that
 * reads for you is a library that will not let you.
 */
import net from 'node:net'
import crypto from 'node:crypto'
import { execFileSync } from 'node:child_process'

import { fixtures, PAGE_URL } from '../lib/harness.mjs'

const CLAUDE = fixtures.projects.find((entry) => entry.role === 'claude')
const SHELL = fixtures.projects.filter((entry) => entry.role === 'shell')[0]
const SOCK = `${fixtures.dataDir}/tmux/${SHELL.id}.sock`
const SESSION = `amx-${SHELL.id}`
const LOG = `${fixtures.runDir}/server.log`

const results = []
function check(name, ok, detail) {
  results.push({ name, ok, detail })
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? `  -- ${detail}` : ''}`)
}
function wsl(script, input) {
  return execFileSync('wsl', ['-d', 'Ubuntu-24.04', 'bash', '-lc', `${script} || true`], {
    encoding: 'utf8',
    input,
    env: { ...process.env, MSYS_NO_PATHCONV: '1' },
  })
}
const paneOf = (project) =>
  wsl(`tmux -S ${fixtures.dataDir}/tmux/${project.id}.sock capture-pane -p -t amx-${project.id}`)
/**
 * paneSize is the terminal's own dimensions, asked of the terminal.
 *
 * A message saying a terminal is a hundred columns wide and a terminal that is
 * a hundred columns wide are different claims, and only one of them can be read
 * out of a WebSocket frame.
 */
const paneSize = (project) =>
  wsl(
    `tmux -S ${fixtures.dataDir}/tmux/${project.id}.sock ` +
      `display-message -p -t amx-${project.id} '#{pane_width}x#{pane_height}'`,
  ).trim()
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const grepCount = (pattern) => Number(wsl(`grep -c "${pattern}" ${LOG}`).trim() || '0')

/**
 * typeCommand types a command into the fixture's shell.
 *
 * It is not passed as an argument, and that is the whole point. Text handed to
 * `wsl.exe` as an argument reaches a shell that evaluates it on the way, so a
 * command containing `$(seq 1 120)` arrives at the terminal already expanded -
 * as a hundred and twenty separate lines, each a syntax error, which is a great
 * deal of output that contains none of what the test asked for. The failure then
 * looks like the transport losing terminal bytes.
 *
 * Over stdin into a file, and from there into a tmux buffer, nothing evaluates
 * the text except the shell it was meant for.
 */
const PAYLOAD = `${fixtures.runDir}/payload.txt`
function typeCommand(text) {
  wsl(`cat > ${PAYLOAD}`, text)
  wsl(
    `tmux -S ${SOCK} load-buffer -b amxpay ${PAYLOAD} && ` +
      `tmux -S ${SOCK} paste-buffer -b amxpay -t ${SESSION} -d && ` +
      `sleep 0.3 && tmux -S ${SOCK} send-keys -t ${SESSION} Enter`,
  )
}

// ---------------------------------------------------------------------------
// The wire, at the byte level

/** mask applies the 4-byte masking key, which every client frame must carry. */
function mask(payload, key) {
  const out = Buffer.alloc(payload.length)
  for (let i = 0; i < payload.length; i += 1) out[i] = payload[i] ^ key[i % 4]
  return out
}

/** encodeFrame builds a masked client frame. Clients mask; servers must not. */
function encodeFrame(opcode, payload) {
  const key = crypto.randomBytes(4)
  const length = payload.length
  let header
  if (length < 126) {
    header = Buffer.alloc(2)
    header[1] = 0x80 | length
  } else if (length < 65536) {
    header = Buffer.alloc(4)
    header[1] = 0x80 | 126
    header.writeUInt16BE(length, 2)
  } else {
    header = Buffer.alloc(10)
    header[1] = 0x80 | 127
    header.writeBigUInt64BE(BigInt(length), 2)
  }
  header[0] = 0x80 | opcode
  return Buffer.concat([header, key, mask(payload, key)])
}

/**
 * takeFrames pulls every complete frame out of a buffer.
 *
 * It returns what it could not consume rather than throwing: a socket hands
 * over whatever arrived, which is regularly half a frame.
 */
function takeFrames(buffer, emit) {
  let rest = buffer
  for (;;) {
    if (rest.length < 2) return rest
    const opcode = rest[0] & 0x0f
    const masked = (rest[1] & 0x80) !== 0
    let length = rest[1] & 0x7f
    let offset = 2
    if (length === 126) {
      if (rest.length < 4) return rest
      length = rest.readUInt16BE(2)
      offset = 4
    } else if (length === 127) {
      if (rest.length < 10) return rest
      length = Number(rest.readBigUInt64BE(2))
      offset = 10
    }
    if (masked) offset += 4
    if (rest.length < offset + length) return rest
    let payload = rest.subarray(offset, offset + length)
    if (masked) payload = mask(payload, rest.subarray(offset - 4, offset))
    emit(opcode, Buffer.from(payload))
    rest = rest.subarray(offset + length)
  }
}

/** decodeFrame reads one binary terminal frame, mirroring the Go layout. */
function decodeFrame(bytes) {
  if (bytes.length < 20) return { error: `only ${bytes.length} bytes` }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.length)
  const idLength = view.getUint16(18)
  const frame = {
    version: bytes[0],
    type: bytes[1],
    first: Number(view.getBigUint64(2)),
    last: Number(view.getBigUint64(10)),
    projectId: bytes.subarray(20, 20 + idLength).toString('latin1'),
  }
  const rest = bytes.subarray(20 + idLength)
  if (frame.type === 2) {
    if (rest.length < 5) return { error: 'a snapshot header needs 5 bytes' }
    const geometry = new DataView(rest.buffer, rest.byteOffset, rest.length)
    frame.cols = geometry.getUint16(0)
    frame.rows = geometry.getUint16(2)
    frame.flags = rest[4]
    frame.payload = rest.subarray(5)
  } else if (frame.type === 1) {
    frame.payload = rest
  } else {
    return { error: `unknown frame type 0x${frame.type.toString(16)}` }
  }
  return frame
}

/**
 * connect opens a socket and speaks the handshake.
 *
 * `origin` is left unset unless a test is about the origin policy: a request
 * without one is a program rather than a page, which is what this is, and the
 * server says so in as many words.
 */
function connect({ path = '/api/ws?v=2', origin } = {}) {
  return new Promise((resolve, reject) => {
    const socket = net.connect({ host: '127.0.0.1', port: 8787 })
    let buffer = Buffer.alloc(0)
    let upgraded = false

    const client = {
      socket,
      messages: [],
      binary: [],
      /** Set when the far end has closed, which is how a stalled client ends. */
      closed: false,
      send(object) {
        socket.write(encodeFrame(0x1, Buffer.from(JSON.stringify(object))))
      },
      /** stopReading applies real TCP backpressure, which is the whole point. */
      stopReading() {
        socket.pause()
      },
      startReading() {
        socket.resume()
      },
      close() {
        socket.destroy()
      },
      get receivedBinaryBytes() {
        return client.binary.reduce((total, frame) => total + (frame.payload?.length ?? 0), 0)
      },
    }

    socket.on('connect', () => {
      const headers = [
        `GET ${path} HTTP/1.1`,
        'Host: 127.0.0.1:8787',
        'Upgrade: websocket',
        'Connection: Upgrade',
        `Sec-WebSocket-Key: ${crypto.randomBytes(16).toString('base64')}`,
        'Sec-WebSocket-Version: 13',
        'Sec-WebSocket-Protocol: agentmux.terminal.v2',
      ]
      if (origin) headers.push(`Origin: ${origin}`)
      socket.write(`${headers.join('\r\n')}\r\n\r\n`)
    })

    socket.on('data', (chunk) => {
      buffer = Buffer.concat([buffer, chunk])
      if (!upgraded) {
        const end = buffer.indexOf('\r\n\r\n')
        if (end === -1) return
        const head = buffer.subarray(0, end).toString('latin1')
        const status = Number(head.split(' ')[1])
        if (status !== 101) {
          reject(new Error(`handshake answered ${status}: ${head.split('\r\n')[0]}`))
          socket.destroy()
          return
        }
        client.handshake = head
        buffer = buffer.subarray(end + 4)
        upgraded = true
        resolve(client)
      }
      buffer = takeFrames(buffer, (opcode, payload) => {
        if (opcode === 0x9) {
          socket.write(encodeFrame(0xa, payload))
          return
        }
        if (opcode === 0x1) {
          client.messages.push(payload.toString('utf8'))
          return
        }
        if (opcode === 0x2) client.binary.push(decodeFrame(payload))
      })
    })

    socket.on('error', (error) => {
      if (!upgraded) reject(error)
    })
    socket.on('close', () => {
      client.closed = true
      if (!upgraded) reject(new Error('the socket closed during the handshake'))
    })
  })
}

/** waitUntil polls a predicate, which is how anything over a socket is observed. */
async function waitUntil(predicate, timeoutMs = 15000, intervalMs = 100) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (predicate()) return true
    await sleep(intervalMs)
  }
  return false
}

/**
 * settleShell waits until the terminal in the fixture is idle and reading.
 *
 * Section 4 deliberately fills the pty with megabytes that take a while to
 * drain, so a suite run twice in a row begins with a shell that is still busy:
 * keystrokes are typed into a process that is not reading them, and the checks
 * that follow measure the previous run rather than this one. Interrupting first
 * and waiting for an answer makes the run start from a known place. It is also
 * how a person would recover the terminal.
 */
async function settleShell(timeoutMs = 30000) {
  const nonce = `SETTLED-${Date.now().toString(36)}`
  for (let attempt = 0; attempt < 8; attempt += 1) {
    wsl(`tmux -S ${SOCK} send-keys -t ${SESSION} C-c`)
    await sleep(120)
  }
  wsl(`tmux -S ${SOCK} send-keys -t ${SESSION} C-u`)
  await sleep(200)
  typeCommand(`echo ${nonce}`)
  const idle = await waitUntil(() => paneOf(SHELL).includes(nonce), timeoutMs, 500)
  if (!idle) console.log('    ... the fixture shell did not settle')
  return idle
}

/** text is what the server said, as objects, with the liveness chatter removed. */
function text(client) {
  return client.messages
    .map((raw) => {
      try {
        return JSON.parse(raw)
      } catch {
        return { type: 'unparseable' }
      }
    })
    .filter((m) => m.type !== 'pong')
}

// ---------------------------------------------------------------------------

async function main() {
  // The terminal is shared with whatever ran last, including a previous copy of
  // this file. Nothing below means anything until it is idle.
  await settleShell()

  // ------------------------------------------------- 1. the handshake and hello
  {
    const client = await connect()
    check(
      'a non-browser client is upgraded and answered with the subprotocol it offered',
      /^HTTP\/1\.1 101/.test(client.handshake) &&
        /sec-websocket-protocol:\s*agentmux\.terminal\.v2/i.test(client.handshake),
      client.handshake.split('\r\n')[0],
    )
    const greeted = await waitUntil(() => text(client).some((m) => m.type === 'hello'))
    const hello = text(client).find((m) => m.type === 'hello') ?? {}
    check(
      'the server greets before anything else, and states its protocol',
      greeted &&
        hello.protocol === 2 &&
        typeof hello.clientId === 'string' &&
        hello.clientId !== '' &&
        typeof hello.connectionId === 'string' &&
        hello.connectionId !== '',
      `protocol ${hello.protocol}, clientId ${hello.clientId}, server ${hello.server} ${hello.version}`,
    )
    client.close()
  }

  // -------------------------------------- 2. sequence continuity, frame by frame
  {
    const client = await connect()
    await waitUntil(() => text(client).some((m) => m.type === 'hello'))
    client.send({ type: 'subscribe', projectId: SHELL.id, cols: 100, rows: 30 })

    const gotSnapshot = await waitUntil(() => client.binary.length > 0)
    const snapshot = client.binary[0]
    check(
      'subscribing is answered with a snapshot before any output',
      gotSnapshot && snapshot.type === 2 && snapshot.projectId === SHELL.id,
      `type ${snapshot?.type}, boundary ${snapshot?.last}, ${snapshot?.payload?.length} bytes`,
    )
    // The size this subscribe asked for is not asserted here, and the reason is
    // the phase's rule rather than a gap: geometry follows the lease, so this
    // client - which has not asked for control - is sent the terminal as it is.
    // Both halves of that are checked at the end of this block, where the
    // sequence numbers are no longer being counted and a resize can be allowed
    // to produce output.

    // Now make it talk, and check every frame that comes back rather than the
    // screen at the end of it. The wait is on the last line the loop prints,
    // not on a frame count: the echo of the command is itself several frames,
    // and counting those would measure the shell's prompt rather than the run.
    typeCommand('for i in $(seq 1 120); do echo continuity-$i; sleep 0.05; done')
    await waitUntil(
      () =>
        client.binary.some((f) => f.type === 1 && f.payload.toString('latin1').includes('continuity-120')),
      40000,
    )
    await sleep(300)

    const outputs = client.binary.filter((f) => f.type === 1)
    let expected = snapshot.last + 1
    let gapped = 0
    for (const frame of outputs) {
      if (frame.first !== expected) gapped += 1
      expected = frame.last + 1
    }
    check(
      'every live frame follows the one before it, with nothing skipped',
      gapped === 0 && outputs.length >= 8,
      `${outputs.length} frame(s), ${gapped} gap(s), sequence reached ${expected - 1}`,
    )
    // Stated as what the frames are rather than what they should be: the payload
    // is compared against the bytes the shell was asked to print, so an encoding
    // of the terminal's output - base64, or JSON with escapes - fails here even
    // though it would decode.
    const carried = outputs.filter((f) => f.payload.toString('latin1').includes('continuity-'))
    check(
      'the frames carry the terminal bytes themselves, not an encoding of them',
      carried.length >= 1,
      `${carried.length} of ${outputs.length} frame(s) hold terminal bytes, ${outputs.reduce((n, f) => n + f.payload.length, 0)} bytes total`,
    )

    // ------------------------------------------- 3. resync, asked for explicitly
    const mark = client.binary.length
    let boundaryBefore = 0
    for (const frame of client.binary) boundaryBefore = frame.last
    typeCommand('echo RESYNC-MARKER-7')
    await sleep(900)
    client.send({ type: 'resync', projectId: SHELL.id })

    const gotFresh = await waitUntil(
      () =>
        client.binary.slice(mark).some((f) => f.type === 2 && f.payload.includes('RESYNC-MARKER-7')),
      15000,
    )
    const fresh = client.binary.slice(mark).find((f) => f.type === 2)
    check(
      'a resync is answered with a screen taken after the request, not a replay',
      gotFresh,
      fresh
        ? `boundary ${boundaryBefore} -> ${fresh.last}, ${fresh.payload.length} bytes, marker in it: ${fresh.payload.includes('RESYNC-MARKER-7')}`
        : 'no snapshot arrived',
    )
    check(
      'the sequence moves forward across a resync rather than restarting',
      fresh !== undefined && fresh.last > boundaryBefore,
      `boundary ${boundaryBefore} -> ${fresh?.last}`,
    )

    // Output after it continues from the screen it was just given, which is
    // what makes a resync invisible to whatever is drawing it - there is no
    // seam for a client to have to know about. Observing that needs output
    // after the snapshot, and a shell at a prompt produces none, so it is asked
    // for one.
    typeCommand('echo AFTER-RESYNC-1')
    await waitUntil(
      () =>
        client.binary.some((f) => f.type === 1 && f.payload.toString('latin1').includes('AFTER-RESYNC-1')),
      20000,
    )
    const afterResync = client.binary.slice(client.binary.indexOf(fresh) + 1).filter((f) => f.type === 1)
    let next = fresh.last + 1
    let broken = 0
    for (const frame of afterResync) {
      if (frame.first !== next) broken += 1
      next = frame.last + 1
    }
    check(
      'output after a resync continues from the screen it was given',
      broken === 0 && afterResync.length >= 1,
      `${afterResync.length} frame(s) after it, starting at ${afterResync[0]?.first}, expected ${fresh.last + 1}, ${broken} discontinuity(ies)`,
    )

    // The client's own rule, applied to a real stream instead of a fabricated
    // one. The browser asks for a resync when this fires; here the firing is
    // the thing being measured.
    let last = 0
    let drift = 0
    for (const frame of client.binary) {
      if (frame.type === 2) last = frame.last
      else {
        if (frame.first !== last + 1) drift += 1
        last = frame.last
      }
    }
    check(
      'a real server stream never gives the client a reason to ask for a resync',
      drift === 0 && client.binary.length >= 10,
      `${client.binary.length} frames, ${drift} a client would have rejected`,
    )

    // ------------------------------------ the geometry, and who may ask for it
    //
    // A subscribe carries a size because a browser knows how large its viewport
    // is, and until this phase the server applied it for whoever asked. It no
    // longer does: a viewer is *shown* a terminal, and a phone opening the
    // project somebody is working in must not reflow that terminal to forty
    // columns just by looking at it. Both halves are here, on the wire, because
    // "the wrong client can reshape your terminal" is not a thing to find out
    // from a screenshot.
    //
    // It is done at the end of the block rather than at the start so that the
    // resize's own output - a shell told it has fewer columns redraws itself -
    // cannot be mistaken for the transport losing frames.
    {
      // What the terminal actually is, measured from tmux rather than assumed
      // from a fixture default. The viewer's check below is that it was sent
      // this and not the size it asked for, and both halves of that have to be
      // real numbers for the comparison to mean anything.
      const asIs = paneSize(SHELL)

      const watcher = await connect()
      await waitUntil(() => text(watcher).some((m) => m.type === 'hello'))
      watcher.send({ type: 'subscribe', projectId: SHELL.id, cols: 40, rows: 12 })
      const watched = await waitUntil(() => watcher.binary.length > 0)
      check(
        'a viewer is sent the terminal as it is, not at the size it asked for',
        watched && `${watcher.binary[0].cols}x${watcher.binary[0].rows}` === asIs && asIs !== '40x12',
        `a viewer asked for 40x12, was sent ${watcher.binary[0]?.cols}x${watcher.binary[0]?.rows}, and the pane is ${asIs}`,
      )

      // Subscribed first, because the server will not take a request from a
      // client that is not watching the terminal: a lease held by somebody who
      // cannot see what they are typing into is of no use to anybody.
      client.send({ type: 'control.request', projectId: SHELL.id })
      const granted = await waitUntil(() =>
        text(client).some((m) => m.type === 'control.granted'),
      )
      check(
        'the first client to ask for an unclaimed terminal is given it',
        granted,
        text(client)
          .filter((m) => m.type.startsWith('control.'))
          .map((m) => m.type)
          .join(',') || 'no control message at all',
      )

      // And now the same subscribe means something. The size is applied before
      // the screen is taken, so the client draws a terminal the right shape
      // once instead of drawing an old shape and reflowing it - and the pane is
      // checked too, because a message saying the terminal is 100 columns wide
      // is not the same claim as a terminal that is.
      const before = client.binary.length
      client.send({ type: 'subscribe', projectId: SHELL.id, cols: 100, rows: 30 })
      const gotFitted = await waitUntil(() =>
        client.binary.slice(before).some((f) => f.type === 2),
      )
      const fitted = client.binary.slice(before).find((f) => f.type === 2)
      check(
        'and a controller asking for a size is sent a screen that shape',
        gotFitted && fitted.cols === 100 && fitted.rows === 30,
        `${fitted?.cols}x${fitted?.rows} (asked for 100x30)`,
      )
      check(
        'and the terminal itself is that size afterwards',
        paneSize(SHELL) === '100x30',
        `the pane is ${paneSize(SHELL)}`,
      )
      check(
        'and everybody watching is told the terminal changed shape',
        await waitUntil(() =>
          text(watcher).some((m) => m.type === 'resized' && m.cols === 100 && m.rows === 30),
        ),
        text(watcher)
          .filter((m) => m.type === 'resized')
          .map((m) => `${m.cols}x${m.rows}`)
          .join(',') || 'nothing',
      )

      watcher.close()
      // Given back rather than dropped. Disconnecting and releasing are
      // different actions with different consequences, and this one is
      // deliberate: the lease is not left suspended over the sections below.
      client.send({ type: 'control.release', projectId: SHELL.id })
      check(
        'and giving it up is answered, so a client knows it is no longer typing into anything',
        await waitUntil(() => text(client).some((m) => m.type === 'control.revoked')),
        '',
      )
    }

    client.close()
  }

  // --------------------------------------------------- 4. the client that stops
  {
    // The case a browser cannot be made to produce, and the one that decides
    // whether a stalled tablet costs the server an unbounded buffer. The socket
    // is left unread, so the server's writes stop completing, its per-reader
    // queues fill, and what happens next is what is being measured: output is
    // dropped at the manager rather than held, memory stays bounded, and the
    // connection ends with a reason in the log instead of being kept open
    // forever on the chance that the client comes back.
    const produced = 64 * 1024 * 1024
    const client = await connect()
    await waitUntil(() => text(client).some((m) => m.type === 'hello'))
    client.send({ type: 'subscribe', projectId: SHELL.id, cols: 100, rows: 30 })
    await waitUntil(() => client.binary.length > 0)

    const droppedBefore = grepCount('runtime watcher is behind')
    const resyncsBefore = grepCount('terminal re-synchronising')
    const failuresBefore = grepCount('terminal socket write failed')
    const rssBefore = Number(wsl('ps -o rss= -C agentmux-server | head -1').trim())
    const command = `yes ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 | head -c ${produced}`

    client.stopReading()
    console.log(`    ... not reading; ${produced / 1048576} MiB on its way`)
    typeCommand(command)

    // The manager's own drop notice is the first thing that happens; the
    // connection giving up is the last. Both are waited for rather than slept
    // through, because the second one is the transport's write deadline and
    // that is a number this test should not have its own copy of.
    const sawDrops = await waitUntil(() => grepCount('runtime watcher is behind') > droppedBefore, 60000, 1000)
    const sawClose = await waitUntil(
      () => client.closed || grepCount('terminal socket write failed') > failuresBefore,
      60000,
      1000,
    )
    const dropped = grepCount('runtime watcher is behind') - droppedBefore
    const resyncs = grepCount('terminal re-synchronising') - resyncsBefore
    const closedBecause = grepCount('terminal socket write failed') - failuresBefore

    client.startReading()
    await sleep(3000)
    const received = client.receivedBinaryBytes
    const rssAfter = Number(wsl('ps -o rss= -C agentmux-server | head -1').trim())

    check(
      'output for a watcher that stopped reading is dropped, not buffered',
      sawDrops && dropped >= 1,
      `${dropped} drop notice(s) in the log while nothing was read`,
    )
    check(
      'a client that never reads is given a screen, not the bytes it missed',
      received < produced / 4,
      `${(produced / 1048576).toFixed(0)} MiB produced, ${(received / 1048576).toFixed(2)} MiB delivered to it`,
    )
    check(
      'and holding it costs the server a bounded amount of memory',
      rssAfter - rssBefore < 40 * 1024,
      `server RSS ${rssBefore} -> ${rssAfter} KiB (${((rssAfter - rssBefore) / 1024).toFixed(1)} MiB)`,
    )
    check(
      'a connection whose client stopped reading is ended, with the reason recorded',
      sawClose && closedBecause >= 1 && client.closed,
      client.closed ? 'the server closed the socket' : 'the socket is still open',
    )
    check(
      'the terminal re-established itself while it was behind, rather than drifting',
      resyncs >= 1,
      `${resyncs} re-synchronisation(s) logged`,
    )
    check(
      'the session and the agent outlive the client the server gave up on',
      wsl(`tmux -S ${SOCK} has-session -t ${SESSION} >/dev/null 2>&1; echo $?`).trim() === '0' &&
        wsl('pgrep -f "claude/versions/2.1.274" >/dev/null 2>&1; echo $?').trim() === '0',
      '',
    )
    client.close()
    // The megabytes are still draining through the pty. Left alone, they would
    // be the next thing anyone typing into this terminal is competing with.
    await settleShell()
  }

  // ------------------------------------------------------- 5. the origin policy
  {
    const probe = (origin) => {
      const args = [
        '-s',
        // A successful upgrade never ends as far as curl is concerned: there is
        // no length to reach and the connection is meant to stay open. The
        // deadline is what makes the answer arrive at all, and the status line
        // is printed either way.
        '-m',
        '2',
        '-o',
        '/dev/null',
        '-w',
        '%{http_code}',
        '-H',
        'Connection: Upgrade',
        '-H',
        'Upgrade: websocket',
        '-H',
        'Sec-WebSocket-Version: 13',
        '-H',
        'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==',
      ]
      if (origin) args.push('-H', `Origin: ${origin}`)
      args.push('http://127.0.0.1:8787/api/ws?v=2')
      return wsl(`curl ${args.map((a) => `'${a}'`).join(' ')}`).trim()
    }
    const withoutOrigin = probe(null)
    const sameOrigin = probe('http://127.0.0.1:8787')
    const elsewhere = probe('http://evil.example')
    check(
      'a page from another site cannot open a terminal, and is told so',
      elsewhere === '403',
      `Origin http://evil.example -> ${elsewhere}`,
    )
    check(
      'a program with no origin and the page this server serves both can',
      withoutOrigin === '101' && sameOrigin === '101',
      `no Origin -> ${withoutOrigin}, same origin -> ${sameOrigin}`,
    )
    const wrongVersion = wsl(
      "curl -s -o /dev/null -w '%{http_code}' 'http://127.0.0.1:8787/api/ws?v=99'",
    ).trim()
    check(
      'a client stating a protocol version this server does not speak is refused',
      wrongVersion === '400',
      `v=99 -> ${wrongVersion}`,
    )
  }

  // ------------------------------------------- 6. the boundary, over the socket
  {
    const client = await connect()
    await waitUntil(() => text(client).some((m) => m.type === 'hello'))

    client.send({ type: 'subscribe', projectId: '/etc' })
    const refusedPath = await waitUntil(() =>
      text(client).some((m) => m.type === 'error' && m.code === 'bad_project'),
    )
    client.send({ type: 'subscribe', projectId: 'p_00000000000000000000' })
    const refusedUnknown = await waitUntil(() =>
      text(client).some((m) => m.type === 'error' && m.projectId === 'p_00000000000000000000'),
    )
    check(
      'a client cannot name a path, nor a project the server does not know',
      refusedPath && refusedUnknown,
      text(client)
        .filter((m) => m.type === 'error')
        .map((m) => `${m.projectId || '-'}=${m.code}`)
        .join(' '),
    )

    client.send({ type: 'subscribe', projectId: SHELL.id, cols: 100, rows: 30 })
    await waitUntil(() => client.binary.length > 0)

    // Input for a project this connection never subscribed to. This is the
    // difference between a terminal and a remote shell: a socket is only a way
    // to type into a terminal somebody already opened.
    const claudeBefore = paneOf(CLAUDE)
    client.send({
      type: 'input',
      projectId: CLAUDE.id,
      data: Buffer.from('echo NOPE\n').toString('base64'),
    })
    const refusedInput = await waitUntil(
      () => text(client).some((m) => m.type === 'error' && m.code === 'not_subscribed'),
      8000,
    )
    await sleep(1500)
    check(
      'input is refused for a terminal the connection never subscribed to',
      refusedInput && paneOf(CLAUDE) === claudeBefore,
      text(client)
        .filter((m) => m.type === 'error')
        .map((m) => m.code)
        .join(','),
    )

    // The size limit, which is what stops a socket being a way to make the
    // server hold an arbitrary amount of memory. Sixty kibibytes decoded is
    // over the input limit and well under the message limit, so this is the
    // limit being tested rather than the read limit closing the connection.
    client.send({
      type: 'input',
      projectId: SHELL.id,
      data: Buffer.alloc(60 * 1024, 0x41).toString('base64'),
    })
    const tooBig = await waitUntil(
      () => text(client).some((m) => m.type === 'error' && m.code === 'input_too_large'),
      8000,
    )
    check(
      'an oversized input message is refused by name',
      tooBig,
      text(client)
        .filter((m) => m.type === 'error')
        .map((m) => m.code)
        .join(','),
    )

    // And a binary message from a client, which is not part of this protocol:
    // input is bytes but it is sent as typed, size-limited JSON.
    const before = client.messages.length
    client.socket.write(encodeFrame(0x2, Buffer.from('raw bytes')))
    const refusedBinary = await waitUntil(() => client.socket.destroyed || client.messages.length > before, 8000)
    check(
      'a binary message from a client is not accepted as input',
      refusedBinary && !paneOf(SHELL).includes('raw bytes'),
      client.socket.destroyed ? 'the connection was closed' : 'no bytes reached the session',
    )
    client.close()
  }

  const failed = results.filter((r) => !r.ok)
  console.log(`\n${results.length - failed.length}/${results.length} checks passed`)
  if (failed.length) process.exit(1)
}

await main().catch((error) => {
  console.error(error)
  process.exit(1)
})
