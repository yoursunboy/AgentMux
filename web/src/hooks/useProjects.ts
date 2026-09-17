import { useCallback, useMemo } from 'react'

import { fetchProjects, fetchServerInfo } from '../api/client'
import type { Project, ServerInfo } from '../api/types'
import { useAsyncResource, type AsyncState } from './useAsyncResource'

/**
 * useServerInfo reads GET /api/server.
 *
 * The server's own capability flags drive the UI: a control whose feature flag
 * is false is disabled and says so, rather than being hidden or faked.
 */
export function useServerInfo(): AsyncState<ServerInfo> {
  return useAsyncResource<ServerInfo>(useCallback((signal) => fetchServerInfo(signal), []))
}

/** The project list, plus a way to refresh it after a write. */
export interface ProjectsState extends AsyncState<Project[]> {
  refresh: () => void
}

/** useProjects reads GET /api/projects. */
export function useProjects(): ProjectsState {
  const state = useAsyncResource<Project[]>(
    useCallback((signal) => fetchProjects({ includeArchived: false }, signal), []),
  )
  const { data, error, loading, reload } = state
  // A stable object, so a caller may put it in a dependency list without the
  // list changing on every render.
  return useMemo(() => ({ data, error, loading, reload, refresh: reload }), [data, error, loading, reload])
}
