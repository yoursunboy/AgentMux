/**
 * The reconnect policy, which is two decisions rather than a delay table.
 */
import { describe, expect, it } from 'vitest'

import { MAX_INITIAL_ATTEMPTS, RECONNECT_DELAYS_MS, reconnectDelay } from './backoff'

/** A jitter source that always returns the same value. */
function fixed(value: number): () => number {
  return () => value
}

describe('reconnectDelay', () => {
  it('grows with each attempt', () => {
    const delays = RECONNECT_DELAYS_MS.map((_, index) => reconnectDelay(index, fixed(1)))

    for (let i = 1; i < delays.length; i++) {
      expect(delays[i]!).toBeGreaterThan(delays[i - 1]!)
    }
  })

  it('never waits less than half the nominal delay', () => {
    // The floor is the point of the jitter being multiplicative rather than
    // additive: a hundred tabs that opened together must not all come back as
    // fast as the first one did.
    for (let attempt = 0; attempt < RECONNECT_DELAYS_MS.length; attempt++) {
      expect(reconnectDelay(attempt, fixed(0))).toBe(Math.round(RECONNECT_DELAYS_MS[attempt]! * 0.5))
      expect(reconnectDelay(attempt, fixed(1))).toBe(RECONNECT_DELAYS_MS[attempt])
    }
  })

  it('keeps the whole range inside the nominal delay', () => {
    for (let attempt = 0; attempt < RECONNECT_DELAYS_MS.length; attempt++) {
      const delay = reconnectDelay(attempt, fixed(0.37))
      const nominal = RECONNECT_DELAYS_MS[attempt]!

      expect(delay).toBeGreaterThanOrEqual(nominal / 2)
      expect(delay).toBeLessThanOrEqual(nominal)
    }
  })

  it('holds at the last delay rather than growing without bound', () => {
    const last = RECONNECT_DELAYS_MS.at(-1)!

    for (const attempt of [RECONNECT_DELAYS_MS.length, 50, 10_000]) {
      expect(reconnectDelay(attempt, fixed(1))).toBe(last)
    }
  })

  it('never returns a fraction of a millisecond', () => {
    // setTimeout tolerates a fraction, but a delay that is not a whole number is
    // a number a test cannot assert on and a log line that reads oddly.
    for (let attempt = 0; attempt < 8; attempt++) {
      for (const value of [0, 0.1, 0.333, 0.5, 0.999]) {
        expect(Number.isInteger(reconnectDelay(attempt, fixed(value)))).toBe(true)
      }
    }
  })

  it('starts the very first retry promptly', () => {
    // A socket that never opened is usually a server that is starting up, and
    // the first retry is the one a person is waiting for.
    expect(reconnectDelay(0, fixed(1))).toBe(250)
  })
})

describe('MAX_INITIAL_ATTEMPTS', () => {
  it('is a small number, because it exists to give up rather than to keep trying', () => {
    expect(MAX_INITIAL_ATTEMPTS).toBeGreaterThan(1)
    expect(MAX_INITIAL_ATTEMPTS).toBeLessThanOrEqual(10)
  })

  it('is no longer than the delay table, so every attempt has its own delay', () => {
    expect(MAX_INITIAL_ATTEMPTS).toBeLessThanOrEqual(RECONNECT_DELAYS_MS.length)
  })
})
