/**
 * The single place the browser talks to the AgentMux server.
 *
 * Every request goes through `request`, so there is no scattered fetch() to
 * keep in sync and every failure arrives as an ApiError carrying the server's
 * own error code. Components switch on that code rather than on a status
 * number or a message string.
 */
import type {
  Candidate,
  CreateProjectInput,
  DiscoveryResult,
  Project,
  RegisterProjectInput,
  ServerInfo,
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

/** Re-exported so callers do not import from two modules for one concept. */
export type { Candidate, DiscoveryResult, Project, ServerInfo }
