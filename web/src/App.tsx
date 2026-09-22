import { useCallback, useEffect, useState } from 'react'

import {
  ApiError,
  ErrorCodes,
  createProject,
  destroyRuntime,
  discoverProjects,
  registerProject,
  setPinnedSlot,
  startAgent,
  startRuntime,
  stopRuntime,
} from './api/client'
import type { Candidate, DiscoveryResult, Project } from './api/types'
import { ErrorBanner } from './components/ErrorBanner'
import { DashboardPage } from './dashboard/DashboardPage'
import { isDashboardPath } from './dashboard/route'
import { GlobalBar } from './components/GlobalBar'
import { NewProjectDialog } from './components/NewProjectDialog'
import { ProjectDetailsDialog } from './components/ProjectDetailsDialog'
import { ProjectManagerPanel } from './components/ProjectManagerPanel'
import { ProjectPanel, type PanelActions, type PanelPosition } from './components/ProjectPanel'
import { RegisterProjectDialog } from './components/RegisterProjectDialog'
import { WorkspaceGrid } from './components/WorkspaceGrid'
import { WorkspacePager } from './components/WorkspacePager'
import { useProjects, useServerInfo } from './hooks/useProjects'
import { describeError } from './lib/format'
import { createTerminalClient } from './terminal/client'
import { TerminalProvider, useConnectionStatus, useTerminalClient } from './terminal/useTerminal'
import {
  firstFreeSlot,
  members,
  moveToSlot,
  neighbourSwap,
  type SlotChange,
} from './workspace/slots'
import { useFullscreen } from './workspace/useFullscreen'
import { useWorkspace } from './workspace/useWorkspace'

type Dialog = 'none' | 'new' | 'register'

/**
 * App decides which page this is.
 *
 * There are two, and the choice is a string comparison rather than a router: the
 * console at `/dashboard`, and the workspace at everything else. The server
 * already serves the single-page entry point for any unknown path, so a deep
 * link works without a history abstraction or a route-matching language - see
 * `dashboard/route.ts` for why that is the whole of the mechanism.
 *
 * The workspace is the default, and it has been since Phase 5. A deep link that
 * this build does not recognise lands there, as it always has.
 */
export function App() {
  // One terminal client for the page, whichever page it is. Creating one opens
  // nothing: the socket is opened by the first subscribe and closed shortly
  // after the last release, so a page with no terminal in it costs nothing by
  // holding one.
  //
  // It is here rather than inside either page because both pages have terminals
  // in them now, and a client created per page would be a second connection to
  // the same server from the same tab. The console's viewers and the workspace's
  // panel are the same browser, and the server should see it that way.
  const [terminalClient] = useState(() => createTerminalClient())
  useEffect(() => () => terminalClient.close(), [terminalClient])

  return (
    <TerminalProvider client={terminalClient}>
      {isDashboardPath(window.location.pathname) ? <DashboardPage /> : <WorkspaceApp />}
    </TerminalProvider>
  )
}

/**
 * WorkspaceApp is the workspace: a global bar, a grid of project panels, and the
 * Project Manager in the last cell of every page.
 *
 * It is one page of up to five terminals plus the manager, and pages of them
 * when there are more projects open than fit; a route for "which panel is
 * focused" would be a router introduced to express a click.
 *
 * Everything the panels can do is decided here. A panel is handed callbacks and
 * knows nothing about the API, the workspace, or which display mode it is in -
 * which is what makes the grid, focus and full screen three boxes around one
 * component rather than three features.
 */
function WorkspaceApp() {
  const terminalClient = useTerminalClient()
  const server = useServerInfo()
  const projects = useProjects()
  const reloadServer = server.reload

  // One terminal client for the page. It opens its socket when the first
  // terminal is watched and closes it when the last one is released, so a page
  // with nothing on screen holds no connection. Every panel subscribes through
  // this, which is what keeps a workspace of five terminals to one WebSocket.
  const connection = useConnectionStatus(terminalClient)

  const list = projects.data ?? []
  const workspace = useWorkspace(list)
  const fullscreen = useFullscreen()

  const [dialog, setDialog] = useState<Dialog>('none')
  const [detailsId, setDetailsId] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState<{ dialog: Dialog; error: Error } | null>(null)
  const [pageError, setPageError] = useState<string | null>(null)

  const [discovery, setDiscovery] = useState<DiscoveryResult | null>(null)
  const [discoveryLoading, setDiscoveryLoading] = useState(false)
  const [discoveryError, setDiscoveryError] = useState<Error | null>(null)

  const roots = server.data?.projectsRoots ?? []
  const refreshProjects = projects.refresh
  const { focus, unfocus } = workspace

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

  /** reportFailure keeps the message, not just the fact, of a failure. */
  const reportFailure = useCallback((scope: Dialog, error: unknown) => {
    const failure = error instanceof Error ? error : new Error(String(error))
    if (scope === 'none') {
      setPageError(describeError(failure))
      return
    }
    setActionError({ dialog: scope, error: failure })
  }, [])

  /** afterWrite folds a created or registered project back into the page. */
  const afterWrite = useCallback(
    async (created: Project) => {
      setActionError(null)
      setDialog('none')
      // A project that was just created or registered joins the workspace: a
      // project that appeared nowhere would look like the action failed. The
      // pin is a second write, so a failure here is reported as what it is -
      // the project exists, and it is the panel that did not open.
      try {
        await setPinnedSlot(created.id, firstFreeSlot(list))
      } catch (error) {
        setPageError(
          `${created.name} was created, but it could not be opened in the workspace. ${describeError(error)}`,
        )
      } finally {
        refreshProjects()
      }
      if (discovery) await scan()
    },
    [discovery, list, refreshProjects, scan],
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

  /**
   * runRuntimeAction drives one request about a runtime and folds the outcome
   * back into the page.
   *
   * The project list is refreshed afterwards rather than the panel patching its
   * own copy, because the runtime state is the server's to report: a session can
   * be started from a previous tab, outlive this page entirely, or be stopped by
   * a restart, and a panel that kept its own answer would drift from all three.
   */
  const runRuntimeAction = useCallback(
    async (projectId: string, action: 'start' | 'stop' | 'destroy' | 'agent') => {
      setBusy(true)
      setPageError(null)
      try {
        if (action === 'start') await startRuntime(projectId)
        else if (action === 'stop') await stopRuntime(projectId)
        else if (action === 'destroy') await destroyRuntime(projectId)
        else await startAgent(projectId)
      } catch (error) {
        reportFailure('none', error)
      } finally {
        setBusy(false)
        refreshProjects()
        // The server's own view of whether a terminal can run here can change
        // while the page is open - tmux installed, the server restarted - and a
        // failed start is exactly when it is worth asking again.
        reloadServer()
      }
    },
    [refreshProjects, reloadServer, reportFailure],
  )

  /**
   * applySlots writes reservation changes and refreshes.
   *
   * Writes are sequential rather than parallel on purpose: a swap is two
   * reservations that have to end up exchanged, and the second one needs the
   * list the first one produced to be sure it is the same two projects. Two
   * requests is not a performance question, and a half-applied swap is a
   * workspace with two projects in one slot.
   */
  const applySlots = useCallback(
    async (changes: SlotChange[]) => {
      if (changes.length === 0) return
      setBusy(true)
      setPageError(null)
      try {
        for (const change of changes) {
          await setPinnedSlot(change.projectId, change.slot)
        }
      } catch (error) {
        reportFailure('none', error)
      } finally {
        setBusy(false)
        refreshProjects()
      }
    },
    [refreshProjects, reportFailure],
  )

  const openInWorkspace = useCallback(
    (project: Project) => void applySlots([{ projectId: project.id, slot: firstFreeSlot(list) }]),
    [applySlots, list],
  )

  // Removing a project takes its panel off the grid and nothing else. The
  // runtime, the session and whatever is running in it are untouched: this
  // writes one column of one row.
  const removeFromWorkspace = useCallback(
    (project: Project) => void applySlots([{ projectId: project.id, slot: null }]),
    [applySlots],
  )

  const move = useCallback(
    (project: Project, direction: 'left' | 'right') => {
      void applySlots(neighbourSwap(list, project.id, direction) ?? [])
    },
    [applySlots, list],
  )

  const pin = useCallback(
    (project: Project, slot: number) => {
      void applySlots(moveToSlot(list, project.id, slot))
    },
    [applySlots, list],
  )

  const details = list.find((project) => project.id === detailsId) ?? null

  /** The actions one panel is given. Rebuilt when a dependency moves, not per render. */
  const panelActions = useCallback(
    (project: Project): PanelActions => ({
      startRuntime: () => void runRuntimeAction(project.id, 'start'),
      stopRuntime: () => void runRuntimeAction(project.id, 'stop'),
      startAgent: () => void runRuntimeAction(project.id, 'agent'),
      destroyRuntime: () => void runRuntimeAction(project.id, 'destroy'),
      focus: () => focus(project.id),
      leaveFocus: unfocus,
      toggleFullscreen: fullscreen.toggle,
      move: (direction) => move(project, direction),
      pin: (slot) => pin(project, slot),
      remove: () => {
        if (workspace.focused?.id === project.id) unfocus()
        removeFromWorkspace(project)
      },
      details: () => setDetailsId(project.id),
    }),
    [
      focus,
      fullscreen.toggle,
      move,
      pin,
      removeFromWorkspace,
      runRuntimeAction,
      unfocus,
      workspace.focused?.id,
      list,
    ],
  )

  return (
    <div className="app">
      <GlobalBar
        info={server.data}
        loading={server.loading}
        error={server.error ? describeError(server.error) : null}
        connection={connection}
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

      <WorkspacePager
        page={workspace.page}
        pages={workspace.pages}
        onPrevious={workspace.previousPage}
        onNext={workspace.nextPage}
      />

      {workspace.focused ? (
        <main className="workspace workspace--focus" aria-label="Focused project">
          <ProjectPanel
            key={workspace.focused.id}
            project={workspace.focused}
            terminalAvailable={server.data?.features.terminal === true}
            terminalBlocker={server.data?.terminalBlocker ?? ''}
            busy={busy}
            position={workspacePosition(list, workspace.focused.id)}
            actions={panelActions(workspace.focused)}
            mode={fullscreen.active ? 'fullscreen' : 'focus'}
            canFullscreen={fullscreen.supported}
          />
        </main>
      ) : (
        <WorkspaceGrid
          items={workspace.items}
          columns={workspace.columns}
          renderProject={(project) => (
            <ProjectPanel
              project={project}
              terminalAvailable={server.data?.features.terminal === true}
              terminalBlocker={server.data?.terminalBlocker ?? ''}
              busy={busy}
              position={workspacePosition(list, project.id)}
              actions={panelActions(project)}
              mode="grid"
              canFullscreen={fullscreen.supported}
            />
          )}
          renderManager={() => (
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
              onOpenInWorkspace={openInWorkspace}
              onRemoveFromWorkspace={removeFromWorkspace}
              onMove={move}
              onFocus={(project) => focus(project.id)}
            />
          )}
        />
      )}

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

      {details && <ProjectDetailsDialog project={details} onClose={() => setDetailsId(null)} />}
    </div>
  )
}

/**
 * workspacePosition is where a panel sits, for the labels and the move buttons.
 *
 * It is derived from the rendered order rather than read from the stored slot,
 * because those are two different numbers the moment a reservation collides
 * with another or leaves a gap: what a person can see is the position, and the
 * menu should say what it is looking at.
 */
function workspacePosition(
  projects: readonly Project[],
  projectId: string,
): PanelPosition {
  const open = members(projects)
  const index = open.findIndex((project) => project.id === projectId)
  if (index === -1) {
    return { slot: 0, total: open.length, canMoveLeft: false, canMoveRight: false }
  }
  return {
    slot: index,
    total: open.length,
    canMoveLeft: index > 0,
    canMoveRight: index < open.length - 1,
  }
}
