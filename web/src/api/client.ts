/**
 * The single place the browser talks to the AgentMux server.
 *
 * Every request goes through `request`, so there is no scattered fetch() to
 * keep in sync and every failure arrives as an ApiError carrying the server's
 * own error code. Components switch on that code rather than on a status
 * number or a message string.
 */
import type {
  AgentSession,
  CreateProjectInput,
  CreateTaskInput,
  DiscoveryResult,
  Project,
  RegisterProjectInput,
  Runtime,
  ServerInfo,
  Task,
  TaskStatus,
  UpdateSessionInput,
  UpdateTaskInput,
} from './types'

/** The API is served from the same origin as this bundle. */
const BASE_URL = '/api'

/** A failing request, carrying the server's stable error code. */
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly details: Record<string, unknown>

  constructor(status: number, code: string, message: string, details: Record<string, unknown> = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.details = details
  }

  /**
   * True when the server could not be reached at all, as opposed to answering
   * with a failure. The UI shows a different message for each case.
   */
  get isNetworkFailure(): boolean {
    return this.status === 0
  }
}

/** Error codes the server produces, gathered in one place for the UI to name. */
export const ErrorCodes = {
  invalidInput: 'invalid_input',
  invalidName: 'invalid_project_name',
  pathNotFound: 'path_not_found',
  notADirectory: 'path_not_a_directory',
  pathNotAccessible: 'path_not_accessible',
  outsideProjectsRoot: 'path_outside_projects_root',
  pathIsProjectsRoot: 'path_is_projects_root',
  alreadyRegistered: 'project_already_registered',
  targetExists: 'path_already_exists',
  gitUnavailable: 'git_unavailable',
  gitInitFailed: 'git_init_failed',
  runtimePathMappingFailed: 'runtime_path_mapping_failed',
  notFound: 'project_not_found',
  storageFailure: 'storage_failure',
  notFoundHttp: 'not_found',
  internal: 'internal_error',
  invalidRequest: 'invalid_request',

  // Runtime codes. They are separate from the project codes because they mean
  // something different to a user: a project failure is about their folder, a
  // runtime failure is about the terminal inside it.
  runtimeUnavailable: 'runtime_unavailable',
  runtimeBackendUnavailable: 'runtime_backend_unavailable',
  runtimeNotFound: 'runtime_not_found',
  runtimeAlreadyRunning: 'runtime_already_running',
  runtimeNotRunning: 'runtime_not_running',
  runtimeStartFailed: 'runtime_start_failed',
  runtimeStopFailed: 'runtime_stop_failed',
  runtimeDestroyFailed: 'runtime_destroy_failed',
  runtimeInputFailed: 'runtime_input_failed',
  runtimeResizeFailed: 'runtime_resize_failed',
  runtimeBackendFailure: 'runtime_backend_failure',
  invalidTerminalSize: 'invalid_terminal_size',
} as const

/** The JSON envelope a failing response carries. */
interface ErrorEnvelope {
  error?: {
    code?: string
    message?: string
    details?: Record<string, unknown>
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  // A JSON content type only makes sense when there is a body. It is added by
  // conditional spread because `exactOptionalPropertyTypes` rejects an explicit
  // `headers: undefined`.
  const jsonHeaders = init?.body ? { 'Content-Type': 'application/json' } : null

  let response: Response
  try {
    response = await fetch(`${BASE_URL}${path}`, {
      ...init,
      ...(jsonHeaders ? { headers: jsonHeaders } : {}),
    })
  } catch (cause) {
    // A stopped server, a dropped connection, or a blocked request all land
    // here. The user needs to know the server is unreachable, not that a
    // promise rejected.
    throw new ApiError(0, 'network_unreachable', 'Could not reach the AgentMux server.', {
      cause: String(cause),
    })
  }

  const text = await response.text()
  let payload: unknown = undefined
  if (text.trim() !== '') {
    try {
      payload = JSON.parse(text)
    } catch {
      // A non-JSON body means something other than this API answered, for
      // example a proxy error page.
      throw new ApiError(
        response.status,
        'invalid_response',
        `The server returned a response that is not JSON (status ${response.status}).`,
      )
    }
  }

  if (!response.ok) {
    const envelope = (payload ?? {}) as ErrorEnvelope
    throw new ApiError(
      response.status,
      envelope.error?.code ?? 'unknown_error',
      envelope.error?.message ?? `The request failed with status ${response.status}.`,
      envelope.error?.details ?? {},
    )
  }

  if (payload === undefined) {
    // A 204 with no body is only expected for a preflight, which the browser
    // issues on its own. Reaching here means the contract changed.
    throw new ApiError(response.status, 'empty_response', 'The server returned an empty response.')
  }
  return payload as T
}

/** Server identity, health, capability flags, and configuration summary. */
export async function fetchServerInfo(signal?: AbortSignal): Promise<ServerInfo> {
  return request<ServerInfo>('/server', { signal: signal ?? null })
}

/** Registered projects. Archived projects are hidden unless asked for. */
export async function fetchProjects(
  options: { includeArchived?: boolean } = {},
  signal?: AbortSignal,
): Promise<Project[]> {
  const query = options.includeArchived ? '?includeArchived=true' : ''
  const body = await request<{ projects: Project[] | null; count: number }>(`/projects${query}`, {
    signal: signal ?? null,
  })
  // The server sends [] rather than null, but a null-safe read here costs
  // nothing and keeps a bad response from crashing the list.
  return body.projects ?? []
}

export async function fetchProject(id: string, signal?: AbortSignal): Promise<Project> {
  const body = await request<{ project: Project }>(`/projects/${encodeURIComponent(id)}`, {
    signal: signal ?? null,
  })
  return body.project
}

/** Scan the Projects Roots for project candidates. Suggests; never registers. */
export async function discoverProjects(
  options: { depth?: number } = {},
  signal?: AbortSignal,
): Promise<DiscoveryResult> {
  const query = options.depth ? `?depth=${options.depth}` : ''
  return request<DiscoveryResult>(`/projects/discover${query}`, { signal: signal ?? null })
}

/** Register an existing directory. */
export async function registerProject(
  input: RegisterProjectInput,
  signal?: AbortSignal,
): Promise<Project> {
  const body = await request<{ project: Project }>('/projects/register', {
    method: 'POST',
    body: JSON.stringify(input),
    signal: signal ?? null,
  })
  return body.project
}

/** Create a new project directory. */
export async function createProject(
  input: CreateProjectInput,
  signal?: AbortSignal,
): Promise<Project> {
  const body = await request<{ project: Project }>('/projects', {
    method: 'POST',
    body: JSON.stringify(input),
    signal: signal ?? null,
  })
  return body.project
}

/**
 * A project's terminal runtime.
 *
 * A project that has never had one is reported as stopped rather than as a
 * missing resource, so a caller renders one shape instead of two.
 */
export async function fetchRuntime(projectId: string, signal?: AbortSignal): Promise<Runtime> {
  const body = await request<{ runtime: Runtime }>(
    `/projects/${encodeURIComponent(projectId)}/runtime`,
    { signal: signal ?? null },
  )
  return body.runtime
}

/**
 * Start a project's runtime. It is idempotent: starting a running runtime
 * returns it unchanged rather than failing.
 */
export async function startRuntime(projectId: string, signal?: AbortSignal): Promise<Runtime> {
  const body = await request<{ runtime: Runtime }>(
    `/projects/${encodeURIComponent(projectId)}/runtime/start`,
    { method: 'POST', signal: signal ?? null },
  )
  return body.runtime
}

/**
 * Stop whatever is running in a project's runtime.
 *
 * The terminal survives, with its scrollback; only the work in it ends. Removing
 * the session is `destroyRuntime`, which is a different and irreversible thing.
 */
export async function stopRuntime(projectId: string, signal?: AbortSignal): Promise<Runtime> {
  const body = await request<{ runtime: Runtime }>(
    `/projects/${encodeURIComponent(projectId)}/runtime/stop`,
    { method: 'POST', signal: signal ?? null },
  )
  return body.runtime
}

/**
 * Remove a project's terminal session and everything in it. Destructive: the
 * session, its scrollback, and its record are all gone afterwards.
 *
 * Nothing in Phase 2 calls this from the UI. It is here because the endpoint
 * exists and a client that cannot express it would be a client that has to be
 * changed later.
 */
export async function destroyRuntime(projectId: string, signal?: AbortSignal): Promise<Runtime> {
  const body = await request<{ runtime: Runtime }>(
    `/projects/${encodeURIComponent(projectId)}/runtime`,
    { method: 'DELETE', signal: signal ?? null },
  )
  return body.runtime
}

/**
 * Reserve a project's workspace slot, or clear the reservation with null.
 *
 * A project is in the workspace exactly when it has a slot, which is why this
 * one call is both "add to the workspace" and "move within it", and why
 * clearing it is what "remove from the workspace" means. It is a property of
 * the project rather than of a browser, so it is the same on every device, and
 * it survives a reload.
 *
 * It does not touch the runtime. Removing a project from the workspace leaves
 * its terminal running - `docs/WORKSPACE.md` says why that is the important
 * half of the rule.
 */
export async function setPinnedSlot(
  projectId: string,
  slot: number | null,
  signal?: AbortSignal,
): Promise<Project> {
  const body = await request<{ project: Project }>(`/projects/${encodeURIComponent(projectId)}`, {
    method: 'PATCH',
    body: JSON.stringify({ pinnedSlot: slot }),
    signal: signal ?? null,
  })
  return body.project
}

/**
 * Start the coding agent in a project's runtime.
 *
 * It is idempotent in the way that matters here: an agent that is already
 * running is adopted rather than launched a second time, so this is the safe
 * way to say "make sure Claude is running in this project" without first
 * finding out whether it is. That is exactly what a grid panel wants, since a
 * panel does not poll the agent's state - the terminal shows it.
 */
export async function startAgent(projectId: string, signal?: AbortSignal): Promise<void> {
  await request<unknown>(`/projects/${encodeURIComponent(projectId)}/runtime/agent/start`, {
    method: 'POST',
    signal: signal ?? null,
  })
}

/** Re-exported so callers do not import from two modules for one concept. */
export type {
  AgentSession,
  AgentSessionStatus,
  Candidate,
  DiscoveryResult,
  Project,
  Runtime,
  ServerInfo,
  Task,
  TaskStatus,
} from './types'

// ---------------------------------------------------------------------------
// Tasks and agent sessions
// ---------------------------------------------------------------------------

/**
 * Record a piece of work somebody wants an agent to do.
 *
 * It starts nothing. Creating a task does not start a runtime, launch an agent
 * or send a prompt: it records that the work is wanted, and that fact does not
 * depend on any process being alive. The two halves of AgentMux are joined by a
 * later phase.
 *
 * The project is in the path and deliberately not in the body - a body that also
 * names one is refused rather than resolved by a rule nobody wrote down.
 */
export async function createTask(
  projectId: string,
  input: CreateTaskInput,
  signal?: AbortSignal,
): Promise<Task> {
  const body = await request<{ task: Task }>(
    `/projects/${encodeURIComponent(projectId)}/tasks`,
    { method: 'POST', body: JSON.stringify(input), signal: signal ?? null },
  )
  return body.task
}

/**
 * List a project's tasks, newest first.
 *
 * An unknown project is a 404 rather than an empty list, because "this project
 * does not exist" and "this project has no tasks" are different answers and an
 * empty list cannot tell them apart.
 */
export async function fetchTasks(
  projectId: string,
  options: { status?: TaskStatus; limit?: number } = {},
  signal?: AbortSignal,
): Promise<Task[]> {
  const query = new URLSearchParams()
  if (options.status) query.set('status', options.status)
  if (options.limit !== undefined) query.set('limit', String(options.limit))
  const suffix = query.toString() === '' ? '' : `?${query}`

  const body = await request<{ tasks: Task[]; count: number }>(
    `/projects/${encodeURIComponent(projectId)}/tasks${suffix}`,
    { signal: signal ?? null },
  )
  return body.tasks
}

/** One task by its own id. */
export async function fetchTask(id: string, signal?: AbortSignal): Promise<Task> {
  const body = await request<{ task: Task }>(`/tasks/${encodeURIComponent(id)}`, {
    signal: signal ?? null,
  })
  return body.task
}

/**
 * Change a task's title, its status, or both.
 *
 * This is the only call that moves a task, and it cannot bypass the lifecycle:
 * the server validates the transition against the status it reads, so a
 * `COMPLETED` task cannot be moved back to `RUNNING` by any request it accepts.
 * A refused transition arrives as an `ApiError` with code
 * `invalid_status_transition` and the statuses that were available.
 *
 * There is no delete. A cancelled task is a record of work somebody decided not
 * to do, which is worth keeping.
 */
export async function updateTask(
  id: string,
  input: UpdateTaskInput,
  signal?: AbortSignal,
): Promise<Task> {
  const body = await request<{ task: Task }>(`/tasks/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: JSON.stringify(input),
    signal: signal ?? null,
  })
  return body.task
}

/**
 * Start a new attempt at a task.
 *
 * The attempt begins with no runtime, and `runtimeId` is deliberately not
 * accepted here: creating an attempt and starting a process are two acts, and
 * the window between them is a real state. Attach one afterwards with
 * `updateSession`.
 *
 * A second attempt is a second session. Neither replaces the other - that is
 * what this level is for.
 */
export async function createSession(taskId: string, signal?: AbortSignal): Promise<AgentSession> {
  const body = await request<{ session: AgentSession }>(
    `/tasks/${encodeURIComponent(taskId)}/sessions`,
    { method: 'POST', signal: signal ?? null },
  )
  return body.session
}

/** List one task's attempts, newest first. */
export async function fetchSessions(
  taskId: string,
  options: { limit?: number } = {},
  signal?: AbortSignal,
): Promise<AgentSession[]> {
  const query = new URLSearchParams()
  if (options.limit !== undefined) query.set('limit', String(options.limit))
  const suffix = query.toString() === '' ? '' : `?${query}`

  const body = await request<{ sessions: AgentSession[]; count: number }>(
    `/tasks/${encodeURIComponent(taskId)}/sessions${suffix}`,
    { signal: signal ?? null },
  )
  return body.sessions
}

/** One attempt by its own id. */
export async function fetchSession(id: string, signal?: AbortSignal): Promise<AgentSession> {
  const body = await request<{ session: AgentSession }>(`/sessions/${encodeURIComponent(id)}`, {
    signal: signal ?? null,
  })
  return body.session
}

/**
 * Change an attempt's status, bind it to a runtime, or both.
 *
 * The status is applied first, so a caller told it lost a race is told so
 * before a runtime has been attached on its behalf - nothing about a refused
 * request is left behind.
 *
 * The runtime is bound once. A session that already has one is refused with
 * `status_conflict` rather than rebound, because the field answers "which
 * runtime did this attempt run in" and a field that can be rewritten stops
 * answering it.
 */
export async function updateSession(
  id: string,
  input: UpdateSessionInput,
  signal?: AbortSignal,
): Promise<AgentSession> {
  const body = await request<{ session: AgentSession }>(`/sessions/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: JSON.stringify(input),
    signal: signal ?? null,
  })
  return body.session
}
