import { describe, expect, it } from 'vitest'

import globalCss from '../styles/global.css?raw'
import { GRID_BREAKPOINTS, columnsForWidth } from './useLayout'

describe('grid columns', () => {
  it('is three on a desktop', () => {
    expect(columnsForWidth(1920)).toBe(3)
    expect(columnsForWidth(1440)).toBe(3)
    expect(columnsForWidth(GRID_BREAKPOINTS.three)).toBe(3)
  })

  it('is two on a tablet held sideways', () => {
    // 1180 and 1024 are the sizes a landscape iPad reports. Three columns there
    // would leave each terminal under sixty columns, which is why they get two.
    expect(columnsForWidth(1180)).toBe(2)
    expect(columnsForWidth(1024)).toBe(2)
    expect(columnsForWidth(GRID_BREAKPOINTS.two)).toBe(2)
  })

  it('is one on a phone, and on a tablet held upright', () => {
    expect(columnsForWidth(820)).toBe(1)
    expect(columnsForWidth(390)).toBe(1)
    expect(columnsForWidth(GRID_BREAKPOINTS.two - 1)).toBe(1)
  })

  it('falls back to the desktop layout rather than to a guess', () => {
    // No width at all is not a narrow screen; it is a question that could not be
    // answered, and guessing narrow would make a page hold fewer projects than
    // the grid has room for.
    expect(columnsForWidth(Number.NaN)).toBe(3)
    expect(columnsForWidth(0)).toBe(3)
    expect(columnsForWidth(-100)).toBe(3)
  })
})

describe('the stylesheet and the hook', () => {
  it('does not set the grid’s columns in two places', () => {
    // The column count is one decision with two consumers - the CSS that lays
    // the grid out and the page model that decides how many projects fit - and
    // the way they stay in agreement is by there being one of them. The
    // component sets grid-template-columns inline; the stylesheet must not.
    const workspaceRules = globalCss
      .split('}')
      .filter((rule) => /\.workspace[^-_]/.test(rule.split('{')[0] ?? ''))

    for (const rule of workspaceRules) {
      expect(rule, `a .workspace rule sets columns: ${rule.slice(0, 120)}`).not.toContain(
        'grid-template-columns',
      )
    }
  })
})
