/**
 * Builders for the controller dashboard's shapes.
 *
 * They live beside the other fixtures rather than inside the dashboard's tests
 * because they describe an API response, and a second test file that wanted one
 * should not have to import another test file to get it.
 *
 * The defaults describe a console with one project whose runtime is up and whose
 * agent is working - the ordinary case - so a test that is about a failure says
 * only what makes it one.
 */
import type {
  ControllerServer,
  Dashboard,
  ModelBinding,
  ProjectCard,
  QueueSummary,
} from '../api/types'

/** A server that is up and can host a terminal. */
export function makeControllerServer(overrides: Partial<ControllerServer> = {}): ControllerServer {
  return {
    status: 'online',
    runtimeAvailable: true,
    runtimeUnavailableReason: '',
    version: '0.6.5',
    uptimeSeconds: 4211,
    ...overrides,
  }
}

/** One project card, with every section answering. */
export function makeProjectCard(overrides: Partial<ProjectCard> = {}): ProjectCard {
  return {
    id: 'p_0123456789abcdef0123',
    name: 'AgentMux',
    runtime: { status: 'running' },
    agent: {
      available: true,
      sessionId: 'sess_0123456789abcdef0123',
      status: 'RUNNING',
      lastEvent: 'agent.started',
      updatedAt: '2026-09-21T09:00:12Z',
    },
    attention: { available: true, level: 'NONE', reason: 'agent started' },
    actions: { available: true, pending: 0 },
    updatedAt: '2026-09-21T09:00:12Z',
    ...overrides,
  }
}

/** A card for a project nothing has ever run in. */
export function makeQuietProjectCard(overrides: Partial<ProjectCard> = {}): ProjectCard {
  return makeProjectCard({
    runtime: { status: 'stopped' },
    agent: null,
    attention: null,
    actions: { available: true, pending: 0 },
    ...overrides,
  })
}

/** How much is waiting, for the console's bar. */
export function makeQueueSummary(overrides: Partial<QueueSummary> = {}): QueueSummary {
  return { needsYou: 0, notices: 0, ...overrides }
}

/**
 * A whole dashboard response.
 *
 * The default queue is empty, which is the ordinary case and the one a test that
 * is about something else should not have to say. A test about the bar passes
 * its own.
 */
export function makeDashboard(overrides: Partial<Dashboard> = {}): Dashboard {
  return {
    server: makeControllerServer(),
    projects: [makeProjectCard()],
    count: 1,
    queue: makeQueueSummary(),
    ...overrides,
  }
}

/** A model binding, for the console's provider section. */
export function makeModelBinding(overrides: Partial<ModelBinding> = {}): ModelBinding {
  return { tool: 'claude', model: 'deepseek flash', ...overrides }
}
