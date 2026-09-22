/**
 * Rendering helpers for the console's tests.
 *
 * The console's cards contain terminals, so a component test has to answer two
 * questions the workspace's tests answer once each: which terminal client the
 * page is using, and what xterm does.
 *
 * The client is published through the provider here, with a double from
 * `./terminal` - the same one the workspace's tests use, so both pages are
 * tested against one idea of what a client is.
 *
 * xterm is *not* mocked here. `vi.mock` is hoisted per test file, so a module
 * that called it on another file's behalf would be depending on import order;
 * a test file that renders a running card states its own mocks at the top,
 * which is three lines and is what `components/TerminalView.test.tsx` does.
 */
import { render, type RenderResult } from '@testing-library/react'
import type { ReactNode } from 'react'

import type { TerminalClient } from '../terminal/client'
import { TerminalProvider } from '../terminal/useTerminal'
import { makeClient, type ClientDouble } from './terminal'

/** What a dashboard test gets back: the render, plus the client it was given. */
export interface DashboardRender extends RenderResult {
  /** The terminal client the page is using, and what it was asked for. */
  terminal: ClientDouble
}

/**
 * renderWithTerminal renders inside a terminal provider.
 *
 * `overrides` replaces parts of the client - a status, most often - so a test
 * can put the page into a connection state without a socket.
 */
export function renderWithTerminal(
  ui: ReactNode,
  overrides: Partial<TerminalClient> = {},
): DashboardRender {
  const terminal = makeClient(overrides)
  return { ...render(<TerminalProvider client={terminal.client}>{ui}</TerminalProvider>), terminal }
}
