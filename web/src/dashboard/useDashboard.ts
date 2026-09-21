/**
 * The console's data, and how often it is re-read.
 *
 * # Why polling and not a socket
 *
 * The terminal has a WebSocket because it has to: bytes arrive when they
 * arrive, and a frame that is a second late is a frame the user watched arrive
 * late. A dashboard is not that. It shows what needs a person, which changes
 * when an agent does something - a handful of times a minute at the very most -
 * and a status that is five seconds old has never misled anybody.
 *
 * §13 of the phase brief asks for exactly this, and the reason it is the right
 * answer rather than the lazy one is that a socket would cost a connection,
 * a reconnect policy, a server-side subscription registry and a lifecycle to
 * get wrong, in exchange for latency nobody can perceive at this cadence. The
 * real-time terminal is a later phase and has its own reasons.
 *
 * # What happens when a poll fails
 *
 * The data already on screen is kept. A dashboard that blanks every time one
 * request times out in a lift is a dashboard nobody can read; one that keeps
 * showing the last good answer and says so is one that can be trusted. The
 * error is reported - `error` is set either way - and the page decides how
 * loudly, according to whether there is anything to show behind it.
 */
import { useCallback, useEffect, useState } from 'react'

import { fetchDashboard } from '../api/client'
import type { Dashboard } from '../api/types'
import { useAsyncResource, type AsyncState } from '../hooks/useAsyncResource'

/** How often the console re-reads the dashboard. */
export const DASHBOARD_POLL_MS = 5000

/**
 * useDashboard reads GET /api/controller, and reads it again every few seconds.
 *
 * The polling is expressed as the read's restart key rather than as a second
 * fetch path, so there is one place a dashboard request is made and one place
 * its result is applied. A slow response cannot overwrite a newer one, because
 * the shared hook already drops results from an effect that is no longer
 * current - which matters more here than anywhere, since the reads overlap by
 * design.
 *
 * A poll interval of zero or less turns polling off, which is what a test wants
 * when it is asserting what one read produces. `undefined` takes the real
 * interval; a number is a caller that has chosen one.
 */
export function useDashboard(pollMs: number = DASHBOARD_POLL_MS): AsyncState<Dashboard> {
  const [tick, setTick] = useState(0)
  const state = useAsyncResource<Dashboard>(
    useCallback((signal: AbortSignal) => fetchDashboard(signal), []),
    String(tick),
  )

  useEffect(() => {
    if (pollMs <= 0) return
    const timer = setInterval(() => setTick((n) => n + 1), pollMs)
    return () => clearInterval(timer)
  }, [pollMs])

  return state
}
