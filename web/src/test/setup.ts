import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach, beforeAll, vi } from 'vitest'

import { installMatchMedia, resetViewport } from './layout'

// jsdom has no viewport to ask about, so the workspace's breakpoints are
// answered by a polyfill rather than by whatever jsdom defaults to.
beforeAll(() => {
  installMatchMedia()
})

// Each test starts with an empty document, so one test's DOM cannot leak into
// the next - and with the default viewport, so one test's screen size cannot
// either.
afterEach(() => {
  cleanup()
  resetViewport()
  vi.restoreAllMocks()
})
