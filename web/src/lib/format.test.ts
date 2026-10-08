import { describe, expect, it } from 'vitest'

import { ApiError, ErrorCodes } from '../api/client'
import {
  capitalise,
  describeError,
  describeHost,
  describeProjectLocation,
  describeStatus,
  formatRelativeTime,
  formatTimestamp,
  formatUptime,
  pluralise,
  shortenPath,
  statusGlyph,
} from './format'
import { makeProject, makeServerInfo } from '../test/fixtures'

describe('formatUptime', () => {
  it('uses the largest sensible unit', () => {
    expect(formatUptime(0)).toBe('0s')
    expect(formatUptime(45)).toBe('45s')
    expect(formatUptime(60)).toBe('1m')
    expect(formatUptime(600)).toBe('10m')
    expect(formatUptime(3600)).toBe('1h 0m')
    expect(formatUptime(90000)).toBe('1d 1h')
  })

  it('refuses to invent a value for nonsense input', () => {
    expect(formatUptime(-1)).toBe('-')
    expect(formatUptime(Number.NaN)).toBe('-')
  })
})

describe('formatTimestamp', () => {
  it('names the absence of a value rather than showing an epoch', () => {
    expect(formatTimestamp(null)).toBe('Never')
    expect(formatTimestamp(undefined)).toBe('Never')
    expect(formatTimestamp('')).toBe('Never')
  })

  it('does not claim a date it could not parse', () => {
    expect(formatTimestamp('not a date')).toBe('Unknown')
  })
})

describe('formatRelativeTime', () => {
  // A fixed instant, because the boundaries - the place this kind of function is
  // actually wrong - are exactly what a fixed clock lets a test reach.
  const now = new Date('2026-09-21T12:00:00Z')
  const ago = (ms: number) => new Date(now.getTime() - ms).toISOString()

  it('names the absence of a value rather than showing an epoch', () => {
    expect(formatRelativeTime(null, now)).toBe('Unknown')
    expect(formatRelativeTime(undefined, now)).toBe('Unknown')
    expect(formatRelativeTime('', now)).toBe('Unknown')
  })

  // A time that came from nowhere must not read as "just now".
  it('does not claim a time it could not parse', () => {
    expect(formatRelativeTime('not a date', now)).toBe('not a date')
  })

  it('calls anything within the minute just now', () => {
    expect(formatRelativeTime(ago(0), now)).toBe('just now')
    expect(formatRelativeTime(ago(59_000), now)).toBe('just now')
  })

  // A clock slightly behind the server's is ordinary, and "in 3 seconds" about
  // something that has already happened would be a confusing thing to read.
  it('calls a time slightly in the future just now too', () => {
    expect(formatRelativeTime(new Date(now.getTime() + 3_000).toISOString(), now)).toBe('just now')
  })

  // §7 of the phase brief asks for this sentence, so it is the one asserted.
  it('writes the units it was asked for, and gets the singular right', () => {
    expect(formatRelativeTime(ago(60_000), now)).toBe('1 minute ago')
    expect(formatRelativeTime(ago(10 * 60_000), now)).toBe('10 minutes ago')
    expect(formatRelativeTime(ago(3 * 3_600_000), now)).toBe('3 hours ago')
    expect(formatRelativeTime(ago(2 * 86_400_000), now)).toBe('2 days ago')
    expect(formatRelativeTime(ago(90 * 86_400_000), now)).toBe('3 months ago')
    expect(formatRelativeTime(ago(800 * 86_400_000), now)).toBe('2 years ago')
  })

  it('uses the largest unit that still says something', () => {
    expect(formatRelativeTime(ago(59 * 60_000), now)).toBe('59 minutes ago')
    expect(formatRelativeTime(ago(60 * 60_000), now)).toBe('1 hour ago')
    expect(formatRelativeTime(ago(23 * 3_600_000), now)).toBe('23 hours ago')
    expect(formatRelativeTime(ago(24 * 3_600_000), now)).toBe('1 day ago')
  })
})

describe('describeHost', () => {
  it('names the distribution, because several may be installed', () => {
    expect(describeHost(makeServerInfo())).toBe('Windows / WSL (Ubuntu-24.04)')
  })

  it('omits the distribution when there is none to name', () => {
    // `delete` rather than `{ distro: undefined }`: the absent key is the case
    // under test, and exactOptionalPropertyTypes distinguishes the two.
    const info = makeServerInfo()
    delete info.distro
    expect(describeHost(info)).toBe('Windows / WSL')
  })

  it('reports a plain host when the runtime is the host', () => {
    const info = makeServerInfo({ host: 'linux', runtimeOs: 'linux', runtimeMode: 'native' })
    delete info.distro
    expect(describeHost(info)).toBe('Linux')
  })

  it('says so rather than guessing before the server answers', () => {
    expect(describeHost(null)).toBe('Unknown host')
  })
})

describe('status presentation', () => {
  it('gives each state a label and a glyph', () => {
    expect(describeStatus('stopped')).toBe('Stopped')
    expect(describeStatus('running')).toBe('Running')
    expect(describeStatus('reconnecting')).toBe('Reconnecting')

    expect(statusGlyph('stopped')).toBe('\u25cb')
    expect(statusGlyph('running')).toBe('\u25cf')
    expect(statusGlyph('reconnecting')).toBe('\u25cc')
  })
})

describe('shortenPath', () => {
  it('leaves a short path alone', () => {
    expect(shortenPath('D:\\AI\\Projects\\App', 40)).toBe('D:\\AI\\Projects\\App')
  })

  it('keeps the tail, which is what identifies the folder', () => {
    const long = 'D:\\AI\\Projects\\2026 AgentMux\\Archive\\Old\\Deeply\\Nested\\AgentMux'
    const short = shortenPath(long, 40)
    expect(short.length).toBeLessThan(long.length)
    expect(short.endsWith('AgentMux')).toBe(true)
    expect(short.startsWith('...')).toBe(true)
  })

  it('handles POSIX separators as well as Windows ones', () => {
    const short = shortenPath('/mnt/d/AI/Projects/2026 AgentMux/AgentMux', 24)
    expect(short.endsWith('AgentMux')).toBe(true)
    expect(short).toContain('/')
  })
})

describe('describeProjectLocation', () => {
  it('names the collection a project sits in', () => {
    expect(describeProjectLocation(makeProject())).toContain('Collection:')
  })

  it('says when a project sits directly under a root', () => {
    expect(describeProjectLocation(makeProject({ collectionPath: '' }))).toBe(
      'Directly under a Projects Root',
    )
  })
})

describe('describeError', () => {
  it('tells the user the server is unreachable rather than showing a promise rejection', () => {
    const message = describeError(new ApiError(0, 'network_unreachable', 'nope'))
    expect(message).toContain('Could not reach the AgentMux server')
  })

  it('adds the guidance the server message cannot carry', () => {
    expect(
      describeError(new ApiError(403, ErrorCodes.outsideProjectsRoot, 'D:\\Other is outside the roots')),
    ).toContain('Add it as a Projects Root on the server')
  })

  it('names the conflicting path for a duplicate', () => {
    const message = describeError(
      new ApiError(409, ErrorCodes.alreadyRegistered, 'already registered', {
        hostPath: 'D:\\AI\\Projects\\App',
      }),
    )
    expect(message).toContain('D:\\AI\\Projects\\App')
  })

  it('explains a missing git instead of reporting a bare failure', () => {
    expect(describeError(new ApiError(503, ErrorCodes.gitUnavailable, 'no git'))).toContain(
      'Install Git',
    )
  })

  it('says what happened to the folder after a failed git init, rather than guessing', () => {
    const rolledBack = describeError(
      new ApiError(500, ErrorCodes.gitInitFailed, 'git init failed in D:\\App', { rolledBack: true }),
    )
    expect(rolledBack).toContain('git init failed in D:\\App')
    expect(rolledBack).toContain('was removed again')

    const kept = describeError(
      new ApiError(500, ErrorCodes.gitInitFailed, 'git init failed in D:\\App', { rolledBack: false }),
    )
    expect(kept).toContain('left exactly as it was')
    expect(kept).not.toContain('removed')
  })

  it('falls back to the message for a code it does not know', () => {
    expect(describeError(new ApiError(400, 'something_new', 'A specific message.'))).toBe(
      'A specific message.',
    )
  })

  it('handles a non-API error without swallowing it', () => {
    expect(describeError(new Error('plain failure'))).toBe('plain failure')
  })

  // §九's one lifecycle answer that is read rather than reported. It is not a
  // failure of the request: the interrupt was delivered, the agent declined it,
  // and the state the card shows next has to be "still running". The pid and
  // the grace are the server's own details, and they are what make the sentence
  // checkable rather than reassuring.
  it('reads a stop timeout as a state, with the two facts the server measured', () => {
    const message = describeError(
      new ApiError(409, ErrorCodes.agentStopTimeout, 'claude in project p_x is still running', {
        pid: 4242,
        timeout: '10s',
      }),
    )
    expect(message).toContain('did not stop within 10s (process 4242)')
    expect(message).toContain('still running')
    expect(message).toContain('nothing was started in its place')
  })

  it('reads a stop timeout whose details are missing rather than printing holes', () => {
    const message = describeError(new ApiError(409, ErrorCodes.agentStopTimeout, 'still running'))
    expect(message).toContain('did not stop, so it is still running')
    expect(message).not.toContain('undefined')
  })

  // A restart that stopped the agent and could not start a new one has still
  // changed the project. The retired attempt is reported whatever the code was,
  // because the half that failed is not the half that changed anything - and a
  // caller told only "the request failed" would read a project holding an agent
  // it no longer has.
  it('says a failed restart left the project with no agent, whatever the code', () => {
    const message = describeError(
      new ApiError(500, ErrorCodes.agentLaunchFailed, 'could not launch claude', {
        retired: { agentSessionId: 'sess_abc', status: 'CANCELLED' },
      }),
    )
    expect(message).toContain('could not launch claude')
    expect(message).toContain('closed as cancelled')
    expect(message).toContain('no agent in it now')
  })

  it('says nothing about a retired attempt when there was not one', () => {
    const message = describeError(
      new ApiError(500, ErrorCodes.agentLaunchFailed, 'could not launch claude'),
    )
    expect(message).toBe('could not launch claude')
  })
})

describe('small helpers', () => {
  it('capitalises only the first letter', () => {
    expect(capitalise('windows')).toBe('Windows')
    expect(capitalise('')).toBe('')
  })

  it('pluralises on the count', () => {
    expect(pluralise(1, 'folder')).toBe('folder')
    expect(pluralise(0, 'folder')).toBe('folders')
    expect(pluralise(3, 'folder')).toBe('folders')
  })
})
