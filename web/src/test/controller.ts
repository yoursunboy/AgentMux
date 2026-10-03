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
  PermissionMode,
  ProjectCard,
  ProjectSettings,
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
    // The mode a project nobody has configured is on - the server's default,
    // which is what a project has before anybody opens the menu. It is not a
    // stand-in for "unknown": a test that wants the server unable to answer
    // says `available: false`.
    settings: { available: true, permissionMode: 'bypassPermissions' },
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
 * The three modes this build offers, in the order the menu lists them.
 *
 * It is written here as well as in `PermissionMenu` on purpose: a test that
 * walked the menu by asking the component what it contains would pass whatever
 * the component happened to render, including nothing. The list is the claim
 * being checked.
 */
export const PERMISSION_MODES: readonly PermissionMode[] = [
  'manual',
  'acceptEdits',
  'bypassPermissions',
]

/** One project's launch settings, from GET /api/projects/{id}/settings. */
export function makeProjectSettings(overrides: Partial<ProjectSettings> = {}): ProjectSettings {
  return { permissionMode: 'bypassPermissions', ...overrides }
}

/**
 * The `settings` section of a card.
 *
 * `available: true` with the default mode is the state a project is in before
 * anybody has opened the menu, which is what most tests want. A test about the
 * server being unable to answer passes `available: false`, and should then
 * expect no mode rather than a defaulted one - the default is a mode the server
 * reports, not one a client fills in.
 */
export function makeCardSettings(
  overrides: Partial<ProjectCard['settings']> = {},
): ProjectCard['settings'] {
  return { available: true, permissionMode: 'bypassPermissions', ...overrides }
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
