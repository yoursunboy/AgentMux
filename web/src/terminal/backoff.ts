/**
 * How long the terminal client waits before trying again.
 *
 * The socket is re-established by the browser, and a browser that retries in a
 * tight loop against a server that is restarting turns a restart into a
 * stampede: every open tab hammering the port at once, which is exactly when
 * the server most needs to be left alone. The delays grow, and they are jittered
 * so that several tabs that were opened together do not come back in step.
 *
 * The two numbers here are the whole policy, and they are a policy rather than
 * an implementation detail because the two halves of it are chosen differently:
 *
 *   - Once a connection has been established at least once, the client retries
 *     forever. A server restart is a normal event - it is how the server is
 *     upgraded - and a browser tab left open across one is expected to come
 *     back on its own. There is no attempt limit on this path.
 *   - If the very first connection never succeeds, the client gives up after
 *     MAX_INITIAL_ATTEMPTS and says so. Something is wrong that retrying will
 *     not fix: the server is not running, is on another port, or refused this
 *     page. A tab that retried forever would be a tab that never tells anyone.
 */

/** Delay before each attempt, capped by the last entry. */
export const RECONNECT_DELAYS_MS = [250, 500, 1000, 2000, 4000, 8000, 10000] as const

/** How many attempts a client makes before it has ever connected. */
export const MAX_INITIAL_ATTEMPTS = 5

/**
 * reconnectDelay is how long to wait before attempt number `attempt`, counting
 * from zero.
 *
 * `jitter` is a seam for tests, so that a delay is a number a test can predict
 * rather than one it has to accept a range for. It returns a value in [0, 1).
 */
export function reconnectDelay(attempt: number, jitter: () => number = Math.random): number {
  const base = RECONNECT_DELAYS_MS[Math.min(attempt, RECONNECT_DELAYS_MS.length - 1)]!
  // Never less than half the nominal delay: a shorter floor would let a
  // hundred tabs retry as fast as the first one did.
  return Math.round(base * (0.5 + jitter() * 0.5))
}
