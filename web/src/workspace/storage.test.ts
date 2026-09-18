import { afterEach, describe, expect, it } from 'vitest'

import { forgetPage, readPage, STORAGE_KEY, writePage } from './storage'

afterEach(() => {
  forgetPage()
})

describe('the remembered page', () => {
  it('is the first one when nothing has been stored', () => {
    expect(readPage()).toBe(0)
  })

  it('survives a write', () => {
    writePage(3)
    expect(readPage()).toBe(3)
    expect(window.localStorage.getItem(STORAGE_KEY)).toBe('3')
  })

  it('answers with the first page rather than throwing on nonsense', () => {
    // A hand-edited value, a value another version wrote, or a browser that
    // answers with something unexpected: a preference is not worth a workspace
    // that refuses to open.
    for (const stored of ['', 'later', '-2', '1e9', '{}', 'NaN']) {
      window.localStorage.setItem(STORAGE_KEY, stored)
      expect(readPage(), `stored ${JSON.stringify(stored)}`).toBe(0)
    }
  })

  it('refuses a page number past the point of being a page', () => {
    window.localStorage.setItem(STORAGE_KEY, '100000')
    expect(readPage()).toBe(0)
  })

  it('rounds a fractional page rather than passing it on', () => {
    writePage(2.7)
    expect(readPage()).toBe(2)
  })

  it('does not throw when storage refuses to answer', () => {
    // A private window, blocked site data, a quota that has been exhausted.
    const original = Object.getOwnPropertyDescriptor(window, 'localStorage')
    Object.defineProperty(window, 'localStorage', {
      configurable: true,
      get() {
        throw new Error('access denied')
      },
    })

    expect(readPage()).toBe(0)
    expect(() => writePage(1)).not.toThrow()
    expect(() => forgetPage()).not.toThrow()

    if (original) Object.defineProperty(window, 'localStorage', original)
  })
})
