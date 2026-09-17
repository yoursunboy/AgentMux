import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach, vi } from 'vitest'

// Each test starts with an empty document, so one test's DOM cannot leak into
// the next.
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})
