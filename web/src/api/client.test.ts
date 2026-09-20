import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  ApiError,
  createProject,
  createSession,
  createTask,
  discoverProjects,
  fetchProject,
  fetchProjects,
  fetchServerInfo,
  fetchSession,
  fetchSessions,
  fetchTasks,
  registerProject,
  updateSession,
  updateTask,
} from './client'
import { makeDiscovery, makeProject, makeServerInfo, makeSession, makeTask } from '../test/fixtures'

/** jsonResponse builds the Response a fetch mock returns. */
function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function errorResponse(status: number, code: string, message: string, details?: unknown): Response {
  return jsonResponse({ error: { code, message, ...(details ? { details } : {}) } }, status)
}

function mockFetch(response: Response | Error) {
  const spy = vi.fn((_input: RequestInfo | URL, _init?: RequestInit) =>
    response instanceof Error ? Promise.reject(response) : Promise.resolve(response.clone()),
  )
  vi.stubGlobal('fetch', spy)
  return spy
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('request', () => {
  it('asks the API under /api, so the browser sees one origin', async () => {
    const spy = mockFetch(jsonResponse(makeServerInfo()))
    await fetchServerInfo()
    expect(spy).toHaveBeenCalledWith('/api/server', expect.anything())
  })

  it('turns an unreachable server into a typed failure rather than a rejection', async () => {
    mockFetch(new TypeError('Failed to fetch'))

    const error = await fetchServerInfo().catch((e: unknown) => e)
    expect(error).toBeInstanceOf(ApiError)
    expect((error as ApiError).isNetworkFailure).toBe(true)
    expect((error as ApiError).status).toBe(0)
  })

  it('carries the server error code and details through', async () => {
    mockFetch(errorResponse(409, 'project_already_registered', 'already registered', { a: 1 }))

    const error = (await registerProject({ hostPath: 'x' }).catch((e: unknown) => e)) as ApiError
    expect(error).toBeInstanceOf(ApiError)
    expect(error.status).toBe(409)
    expect(error.code).toBe('project_already_registered')
    expect(error.message).toBe('already registered')
    expect(error.details).toEqual({ a: 1 })
  })

  it('reports a non-JSON body as such instead of throwing a parse error', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('<html>proxy error</html>', { status: 502 }))),
    )

    const error = (await fetchServerInfo().catch((e: unknown) => e)) as ApiError
    expect(error.code).toBe('invalid_response')
    expect(error.status).toBe(502)
  })

  it('does not mistake an empty body for success', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(new Response('', { status: 200 }))))

    const error = (await fetchServerInfo().catch((e: unknown) => e)) as ApiError
    expect(error.code).toBe('empty_response')
  })
})

describe('fetchProjects', () => {
  it('asks for the archived ones only when told to', async () => {
    const spy = mockFetch(jsonResponse({ projects: [], count: 0 }))

    await fetchProjects()
    expect(spy).toHaveBeenLastCalledWith('/api/projects', expect.anything())

    await fetchProjects({ includeArchived: true })
    expect(spy).toHaveBeenLastCalledWith('/api/projects?includeArchived=true', expect.anything())
  })

  it('treats a null list as empty so the UI cannot crash on it', async () => {
    mockFetch(jsonResponse({ projects: null, count: 0 }))
    expect(await fetchProjects()).toEqual([])
  })

  it('returns the projects it was given', async () => {
    const project = makeProject()
    mockFetch(jsonResponse({ projects: [project], count: 1 }))
    expect(await fetchProjects()).toEqual([project])
  })
})

describe('discoverProjects', () => {
  it('passes the depth only when one is asked for', async () => {
    const spy = mockFetch(jsonResponse(makeDiscovery()))

    await discoverProjects()
    expect(spy).toHaveBeenLastCalledWith('/api/projects/discover', expect.anything())

    await discoverProjects({ depth: 2 })
    expect(spy).toHaveBeenLastCalledWith('/api/projects/discover?depth=2', expect.anything())
  })
})

describe('writes', () => {
  it('posts registration as JSON and unwraps the project', async () => {
    const project = makeProject()
    const spy = mockFetch(jsonResponse({ project }, 201))

    const result = await registerProject({ hostPath: project.hostPath })
    expect(result).toEqual(project)

    const [url, init] = spy.mock.calls[0]!
    expect(String(url)).toBe('/api/projects/register')
    expect(init?.method).toBe('POST')
    expect(JSON.parse(String(init?.body))).toEqual({ hostPath: project.hostPath })
    expect((init?.headers as Record<string, string>)['Content-Type']).toBe('application/json')
  })

  it('posts creation as JSON without optional fields that were not given', async () => {
    const spy = mockFetch(jsonResponse({ project: makeProject() }, 201))

    await createProject({ name: 'App', initGit: true })

    const init = spy.mock.calls[0]?.[1]
    expect(JSON.parse(String(init?.body))).toEqual({ name: 'App', initGit: true })
  })

  it('escapes an identifier so it cannot alter the request path', async () => {
    const spy = mockFetch(jsonResponse({ project: makeProject() }))

    await fetchProject('a/b c')
    expect(String(spy.mock.calls[0]?.[0])).toBe('/api/projects/a%2Fb%20c')
  })
})

describe('tasks and agent sessions', () => {
  it('posts a task to its project and unwraps it', async () => {
    const task = makeTask()
    const spy = mockFetch(jsonResponse({ task }, 201))

    const result = await createTask(task.projectId, { title: task.title })
    expect(result).toEqual(task)

    const [url, init] = spy.mock.calls[0]!
    expect(String(url)).toBe(`/api/projects/${task.projectId}/tasks`)
    expect(init?.method).toBe('POST')
    // The project is in the path and deliberately not in the body: two places to
    // say which project a task belongs to is one place too many.
    expect(JSON.parse(String(init?.body))).toEqual({ title: task.title })
  })

  it('escapes the project identifier in a task path', async () => {
    const spy = mockFetch(jsonResponse({ tasks: [], count: 0 }))

    await fetchTasks('a/b c')
    expect(String(spy.mock.calls[0]?.[0])).toBe('/api/projects/a%2Fb%20c/tasks')
  })

  it('omits query parameters that were not asked for', async () => {
    const spy = mockFetch(jsonResponse({ tasks: [], count: 0 }))

    await fetchTasks('p_1')
    expect(String(spy.mock.calls[0]?.[0])).toBe('/api/projects/p_1/tasks')
  })

  it('sends the status filter and the limit when they were given', async () => {
    const spy = mockFetch(jsonResponse({ tasks: [], count: 0 }))

    await fetchTasks('p_1', { status: 'RUNNING', limit: 5 })
    expect(String(spy.mock.calls[0]?.[0])).toBe('/api/projects/p_1/tasks?status=RUNNING&limit=5')
  })

  it('patches only the fields it was given', async () => {
    const spy = mockFetch(jsonResponse({ task: makeTask({ status: 'RUNNING' }) }))

    await updateTask('task_1', { status: 'RUNNING' })

    const [url, init] = spy.mock.calls[0]!
    expect(String(url)).toBe('/api/tasks/task_1')
    expect(init?.method).toBe('PATCH')
    expect(JSON.parse(String(init?.body))).toEqual({ status: 'RUNNING' })
  })

  it('surfaces a refused transition with the statuses that were available', async () => {
    mockFetch(
      errorResponse(409, 'invalid_status_transition', 'a task cannot move from COMPLETED to RUNNING', {
        from: 'COMPLETED',
        to: 'RUNNING',
        allowed: [],
      }),
    )

    await expect(updateTask('task_1', { status: 'RUNNING' })).rejects.toMatchObject({
      status: 409,
      code: 'invalid_status_transition',
      details: { from: 'COMPLETED', to: 'RUNNING' },
    })
  })

  it('creates an attempt with no body, because an attempt needs nothing else to exist', async () => {
    const session = makeSession()
    const spy = mockFetch(jsonResponse({ session }, 201))

    const result = await createSession(session.taskId)

    expect(result).toEqual(session)
    const [url, init] = spy.mock.calls[0]!
    expect(String(url)).toBe(`/api/tasks/${session.taskId}/sessions`)
    expect(init?.method).toBe('POST')
    // No runtimeId: creating an attempt and starting a process are two acts, and
    // the server refuses a body that names a runtime here.
    expect(init?.body).toBeUndefined()
  })

  it('binds a runtime to an attempt through the patch', async () => {
    const runtimeId = 'amx-p_0123456789abcdef0123'
    const spy = mockFetch(jsonResponse({ session: makeSession({ runtimeId }) }))

    const result = await updateSession('sess_1', { runtimeId })

    expect(result.runtimeId).toBe(runtimeId)
    const init = spy.mock.calls[0]?.[1]
    expect(JSON.parse(String(init?.body))).toEqual({ runtimeId })
  })

  it('escapes a session identifier in the path', async () => {
    const spy = mockFetch(jsonResponse({ session: makeSession() }))

    await fetchSession('a/b')
    expect(String(spy.mock.calls[0]?.[0])).toBe('/api/sessions/a%2Fb')
  })

  it('lists a task’s attempts without a query when none was asked for', async () => {
    const spy = mockFetch(jsonResponse({ sessions: [], count: 0 }))

    await fetchSessions('task_1')
    expect(String(spy.mock.calls[0]?.[0])).toBe('/api/tasks/task_1/sessions')
  })
})
