/**
 * The action centre's data, and how often it is re-read.
 *
 * # Why polling and not a socket
 *
 * The same reason the console polls: an action changes when an agent does
 * something, which is a handful of times a minute at the very most. §12 of the
 * phase brief asks for polling, and a socket here would cost a connection, a
 * reconnect policy and a server-side subscription registry in exchange for
 * latency nobody can perceive at this cadence. The terminal is the thing that
 * needs real time, and it already has it.
 *
 * # What happens when a poll fails
 *
 * The data already on screen is kept, exactly as the console keeps it. A queue
 * that blanked because one refresh timed out would be unreadable in precisely
 * the conditions somebody is looking at it.
 */
import { useCallback, useEffect, useState } from 'react'

import { fetchAction, fetchActions } from '../api/client'
import type { ActionItem, ActionQueue } from '../api/types'
import { useAsyncResource, type AsyncState } from '../hooks/useAsyncResource'

/** How often the action centre re-reads the queue. */
export const ACTIONS_POLL_MS = 5000

/**
 * useActions reads GET /api/actions, and reads it again every few seconds.
 *
 * The polling is the read's restart key rather than a second fetch path, so
 * there is one place a queue request is made and one place its result is
 * applied - and the shared hook already drops a slow response that would
 * otherwise overwrite a newer one.
 *
 * A poll interval of zero or less turns polling off, which is what a test wants
 * when it is asserting what one read produces.
 */
export function useActions(pollMs: number = ACTIONS_POLL_MS): AsyncState<ActionQueue> {
  const [tick, setTick] = useState(0)
  const state = useAsyncResource<ActionQueue>(
    useCallback((signal: AbortSignal) => fetchActions(signal), []),
    String(tick),
  )

  useEffect(() => {
    if (pollMs <= 0) return
    const timer = setInterval(() => setTick((n) => n + 1), pollMs)
    return () => clearInterval(timer)
  }, [pollMs])

  return state
}

/**
 * useAction reads GET /api/actions/{id}.
 *
 * The restart key carries the id as well as the tick, and it has to. The shared
 * hook holds the loader in a ref, so only the key restarts the read - and the
 * loader closes over `id`. A key of `String(tick)` alone would poll the *first*
 * id this component was rendered with, for as long as it stayed mounted, and a
 * page that navigated from one action to another without a reload would show the
 * wrong one. Today the id only changes via a page load; this must not depend on
 * that staying true.
 */
export function useAction(id: string, pollMs: number = ACTIONS_POLL_MS): AsyncState<ActionItem> {
  const [tick, setTick] = useState(0)
  const state = useAsyncResource<ActionItem>(
    useCallback((signal: AbortSignal) => fetchAction(id, signal), [id]),
    `${id}:${tick}`,
  )

  useEffect(() => {
    if (pollMs <= 0) return
    const timer = setInterval(() => setTick((n) => n + 1), pollMs)
    return () => clearInterval(timer)
  }, [pollMs])

  return state
}
