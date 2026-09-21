import { describe, expect, it } from 'vitest'

import { DASHBOARD_PATH, isDashboardPath } from './route'

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
