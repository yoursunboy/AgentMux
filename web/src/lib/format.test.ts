import { describe, expect, it } from 'vitest'

import { ApiError, ErrorCodes } from '../api/client'
import {
  capitalise,
  describeError,
  describeHost,
  describeProjectLocation,
  describeStatus,
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
