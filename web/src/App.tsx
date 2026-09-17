import { useCallback, useState } from 'react'

import { ApiError, ErrorCodes, createProject, discoverProjects, registerProject } from './api/client'
import type { Candidate, DiscoveryResult, Project } from './api/types'
import { ErrorBanner } from './components/ErrorBanner'
import { GlobalBar } from './components/GlobalBar'
import { NewProjectDialog } from './components/NewProjectDialog'
import { ProjectManagerPanel } from './components/ProjectManagerPanel'
import { ProjectPanel } from './components/ProjectPanel'
import { RegisterProjectDialog } from './components/RegisterProjectDialog'
import { useProjects, useServerInfo } from './hooks/useProjects'
import { describeError } from './lib/format'

type Dialog = 'none' | 'new' | 'register'

/**
 * App is the whole Phase 1 UI: a global bar, project panels, and the project
 * manager.
 *
 * There is no router and no dashboard. The UI spec describes one workspace,
 * and inventing pages for a product with three screens would be furniture, not
 * function.
 */
export function App() {
  const server = useServerInfo()
  const projects = useProjects()
  const reloadServer = server.reload

  const [dialog, setDialog] = useState<Dialog>('none')
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState<{ dialog: Dialog; error: Error } | null>(null)
  const [pageError, setPageError] = useState<string | null>(null)

  const [discovery, setDiscovery] = useState<DiscoveryResult | null>(null)
  const [discoveryLoading, setDiscoveryLoading] = useState(false)
  const [discoveryError, setDiscoveryError] = useState<Error | null>(null)

  const scan = useCallback(async () => {
    setDiscoveryLoading(true)
    setDiscoveryError(null)
    try {
      setDiscovery(await discoverProjects())
    } catch (error) {
      setDiscoveryError(error instanceof Error ? error : new Error(String(error)))
    } finally {
      setDiscoveryLoading(false)
    }
  }, [])

  const list = projects.data ?? []
  const selected = list.find((project) => project.id === selectedId) ?? list[0] ?? null
  const roots = server.data?.projectsRoots ?? []
  const refreshProjects = projects.refresh

  /** reportFailure keeps the message, not just the fact, of a failure. */
  const reportFailure = useCallback((scope: Dialog, error: unknown) => {
    const failure = error instanceof Error ? error : new Error(String(error))
    if (scope === 'none') {
      setPageError(describeError(failure))
      return
    }
    setActionError({ dialog: scope, error: failure })
  }, [])

  /** afterWrite refreshes the list and the discovery marks. */
  const afterWrite = useCallback(
    async (created: Project) => {
      setSelectedId(created.id)
      setActionError(null)
      setDialog('none')
      refreshProjects()
      if (discovery) await scan()
    },
    [discovery, refreshProjects, scan],
  )

  const onRegister = useCallback(
    async (input: { hostPath: string; name?: string }) => {
      setBusy(true)
      try {
        await afterWrite(await registerProject(input))
      } catch (error) {
        // A duplicate is worth refreshing for: the user's intent - "I want
        // this folder" - is already satisfied, so the list should show it.
        if (error instanceof ApiError && error.code === ErrorCodes.alreadyRegistered) {
          refreshProjects()
        }
        reportFailure('register', error)
      } finally {
        setBusy(false)
      }
    },
    [afterWrite, refreshProjects, reportFailure],
  )

  const onRegisterCandidate = useCallback(
    (candidate: Candidate) => {
      void onRegister({ hostPath: candidate.hostPath })
    },
    [onRegister],
  )

  const onCreate = useCallback(
    async (input: { name: string; collectionPath?: string; initGit: boolean }) => {
      setBusy(true)
      try {
        await afterWrite(await createProject(input))
      } catch (error) {
        reportFailure('new', error)
      } finally {
        setBusy(false)
      }
    },
    [afterWrite, reportFailure],
  )

  const retryEverything = useCallback(() => {
    setPageError(null)
    reloadServer()
    refreshProjects()
  }, [reloadServer, refreshProjects])

  return (
    <div className="app">
      <GlobalBar
        info={server.data}
        loading={server.loading}
        error={server.error ? describeError(server.error) : null}
        onRetry={retryEverything}
      />

      {server.error && (
        <ErrorBanner
          message={describeError(server.error)}
          {...(server.error instanceof ApiError && server.error.status > 0
            ? { detail: `${server.error.code} (HTTP ${server.error.status})` }
            : {})}
          onRetry={retryEverything}
        />
      )}

      {pageError && <ErrorBanner message={pageError} onDismiss={() => setPageError(null)} />}

      {server.data && server.data.warnings.length > 0 && (
        <div className="warnings">
          {server.data.warnings.map((warning) => (
            <span className="warnings__item" key={warning}>
              {warning}
            </span>
          ))}
        </div>
      )}

      <main className="workspace">
        {selected ? (
          <ProjectPanel project={selected} />
        ) : (
          <section className="panel panel--empty" aria-label="No project selected">
            <div className="panel__body">
              <p className="notice">
                {projects.loading
                  ? 'Loading…'
                  : 'No project is open. Create or register one to get started.'}
              </p>
            </div>
          </section>
        )}

        <ProjectManagerPanel
          projects={list}
          projectsLoading={projects.loading}
          projectsError={projects.error}
          discovery={discovery}
          discoveryLoading={discoveryLoading}
          discoveryError={discoveryError}
          busy={busy}
          onOpenNew={() => {
            setActionError(null)
            setDialog('new')
          }}
          onOpenRegister={() => {
            setActionError(null)
            setDialog('register')
          }}
          onDiscover={() => void scan()}
          onRegisterCandidate={onRegisterCandidate}
          onRefresh={refreshProjects}
          selectedId={selected?.id ?? null}
          onSelect={(project) => setSelectedId(project.id)}
        />
      </main>

      {dialog === 'new' && (
        <NewProjectDialog
          roots={roots}
          busy={busy}
          error={actionError?.dialog === 'new' ? actionError.error : null}
          onCancel={() => setDialog('none')}
          onSubmit={(input) => void onCreate(input)}
        />
      )}

      {dialog === 'register' && (
        <RegisterProjectDialog
          discovery={discovery}
          discoveryLoading={discoveryLoading}
          discoveryError={discoveryError}
          busy={busy}
          error={actionError?.dialog === 'register' ? actionError.error : null}
          onScan={() => void scan()}
          onCancel={() => setDialog('none')}
          onSubmit={(input) => void onRegister(input)}
          onRegisterCandidate={onRegisterCandidate}
        />
      )}
    </div>
  )
}
