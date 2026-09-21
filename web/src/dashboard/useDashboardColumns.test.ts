import { act, renderHook } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { setViewportWidth } from '../test/layout'
import {
  DASHBOARD_BREAKPOINTS,
  dashboardColumnsForWidth,
  useDashboardColumns,
} from './useDashboardColumns'

describe('the console grid width', () => {
  it('answers the layout question without a browser', () => {
    expect(dashboardColumnsForWidth(1440)).toBe(3)
    expect(dashboardColumnsForWidth(1024)).toBe(3)
    expect(dashboardColumnsForWidth(768)).toBe(3)
    expect(dashboardColumnsForWidth(600)).toBe(2)
    expect(dashboardColumnsForWidth(430)).toBe(1)
    expect(dashboardColumnsForWidth(390)).toBe(1)
  })

  it('falls back to the desktop layout for a width that is not a width', () => {
    expect(dashboardColumnsForWidth(0)).toBe(3)
    expect(dashboardColumnsForWidth(Number.NaN)).toBe(3)
  })

  // §10 of the phase brief names four devices. This is that list as a table, so
  // a change to a breakpoint has to be a deliberate change to a device's layout.
  it('gives the devices the brief names the layout it asks for', () => {
    const devices: ReadonlyArray<[string, number, number]> = [
      ['phone, upright', 390, 1],
      ['tablet, upright', 768, 3],
      ['tablet, sideways', 1024, 3],
      ['desktop', 1440, 3],
    ]
    for (const [name, width, want] of devices) {
      expect(dashboardColumnsForWidth(width), name).toBe(want)
    }
  })

  it('reports the column count for the current viewport, and follows it', () => {
    const hook = renderHook(() => useDashboardColumns())

    act(() => setViewportWidth(1440))
    expect(hook.result.current).toBe(3)

    // The polyfill notifies its listeners, which is what a real resize does -
    // so this is the hook reacting rather than the test re-rendering it.
    act(() => setViewportWidth(390))
    expect(hook.result.current).toBe(1)
  })

  it('agrees with the breakpoints it exports', () => {
    // The stylesheet uses the same numbers, and this is the half a test can
    // check: the constant and the function cannot drift apart.
    expect(dashboardColumnsForWidth(DASHBOARD_BREAKPOINTS.three)).toBe(3)
    expect(dashboardColumnsForWidth(DASHBOARD_BREAKPOINTS.three - 1)).toBe(2)
    expect(dashboardColumnsForWidth(DASHBOARD_BREAKPOINTS.two)).toBe(2)
    expect(dashboardColumnsForWidth(DASHBOARD_BREAKPOINTS.two - 1)).toBe(1)
  })
})
