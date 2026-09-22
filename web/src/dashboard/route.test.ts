import { describe, expect, it } from 'vitest'

import {
  ACTIONS_PATH,
  actionPath,
  DASHBOARD_PATH,
  isDashboardPath,
  pageFor,
  WORKSPACE_PATH,
} from './route'

describe('the page switch', () => {
  it('names the console', () => {
    expect(isDashboardPath('/dashboard')).toBe(true)
  })

  it('treats a trailing slash as the same address', () => {
    expect(isDashboardPath('/dashboard/')).toBe(true)
  })

  it('sends everything else to the workspace', () => {
    for (const path of ['/', '', '/projects', '/dashboard/extra', '/workspace', '/DASHBOARD']) {
      expect(isDashboardPath(path)).toBe(false)
    }
  })

  it('uses a path the server already serves the entry point for', () => {
    // The single-page fallback is what makes a deep link work at all, so the
    // path has to be one the server would answer with index.html - which every
    // unknown path is.
    expect(DASHBOARD_PATH.startsWith('/')).toBe(true)
  })
})

describe('pageFor', () => {
  it('names the three pages it has', () => {
    expect(pageFor('/')).toEqual({ name: 'workspace', actionId: null })
    expect(pageFor('/dashboard')).toEqual({ name: 'dashboard', actionId: null })
    expect(pageFor('/actions')).toEqual({ name: 'actions', actionId: null })
  })

  it('reads the action out of an action path', () => {
    expect(pageFor('/actions/act_0123456789abcdef')).toEqual({
      name: 'actions',
      actionId: 'act_0123456789abcdef',
    })
  })

  it('treats a trailing slash as the same address', () => {
    expect(pageFor('/actions/')).toEqual({ name: 'actions', actionId: null })
    expect(pageFor('/dashboard//')).toEqual({ name: 'dashboard', actionId: null })
  })

  // The shape check is not a security boundary - the server answers 404 for an
  // id it has never held. What it buys is that a path which cannot be an id
  // opens the queue rather than making a request that could only 404.
  it('opens the queue for an action path it cannot read', () => {
    for (const path of [
      '/actions/nonsense',
      '/actions/act_',
      '/actions/act_NOTHEX',
      '/actions/ACT_0123',
      '/actions/a/b',
      '/actions/act_0123/extra',
    ]) {
      expect(pageFor(path)).toEqual({ name: 'actions', actionId: null })
    }
  })

  // Every path starts with a slash, so a workspace test written as a prefix
  // comparison would swallow every other page. This is the assertion that says
  // the workspace is checked last and by exclusion.
  it('leaves a path it does not know on the workspace', () => {
    expect(pageFor('/nonsense')).toEqual({ name: 'workspace', actionId: null })
    expect(pageFor('/actions-extra')).toEqual({ name: 'workspace', actionId: null })
  })
})

describe('actionPath', () => {
  // The link builder and the path reader have to agree, or a link opens the
  // queue instead of the action it named.
  it('round-trips through pageFor', () => {
    const id = 'act_0123456789abcdef'
    expect(actionPath(id)).toBe(`${ACTIONS_PATH}/${id}`)
    expect(pageFor(actionPath(id)).actionId).toBe(id)
  })

  it('builds a path that is not the queue', () => {
    expect(actionPath('act_1')).not.toBe(ACTIONS_PATH)
  })

  it('is a path the server serves the entry point for, like the other two', () => {
    expect(ACTIONS_PATH.startsWith('/')).toBe(true)
    expect(WORKSPACE_PATH).toBe('/')
  })
})
