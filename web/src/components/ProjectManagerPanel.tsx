/**
 * ProjectManagerPanel is the workspace's last cell: what is open, what exists,
 * and the three ways to get more.
 *
 * # It is a panel, not a sidebar
 *
 * The UI spec has no permanent sidebar, no explorer and no second navigation
 * bar, so the workspace's own controls live in a cell of the workspace itself.
 * The cell is fixed - it is the last one on every page - which is what makes it
 * findable without being chrome.
 *
 * # Two lists, and why they are different
 *
 * "In the workspace" is the ordered set of projects being watched, which is the
 * same fact as "these projects have a reserved slot". "Registered" is
 * everything else AgentMux knows about: a workspace with fifty registered
 * projects and eight panels is the ordinary case, not a limitation, and the
 * distinction is the reason this panel has two lists rather than one with
 * checkmarks.
 *
 * Opening a project reserves it a slot; removing it gives the slot up. Neither
 * touches the runtime - removing a project from the grid leaves its terminal
 * running, and that is stated in the panel rather than left to be discovered.
 */
import type { Candidate, DiscoveryResult, Project } from '../api/types'
import { describeError, displaySlot, pluralise, shortenPath } from '../lib/format'
import { members } from '../workspace/slots'

interface ProjectManagerPanelProps {
  projects: Project[]
  projectsLoading: boolean
  projectsError: Error | null

  discovery: DiscoveryResult | null
  discoveryLoading: boolean
  discoveryError: Error | null

  busy: boolean
  onOpenNew: () => void
  onOpenRegister: () => void
  onDiscover: () => void
  onRegisterCandidate: (candidate: Candidate) => void
  onRefresh: () => void
  onOpenInWorkspace: (project: Project) => void
  onRemoveFromWorkspace: (project: Project) => void
  onMove: (project: Project, direction: 'left' | 'right') => void
  onFocus: (project: Project) => void
}

export function ProjectManagerPanel(props: ProjectManagerPanelProps) {
  const {
    projects,
    projectsLoading,
    projectsError,
    discovery,
    discoveryLoading,
    discoveryError,
    busy,
    onOpenNew,
    onOpenRegister,
    onDiscover,
    onRegisterCandidate,
    onRefresh,
    onOpenInWorkspace,
    onRemoveFromWorkspace,
    onMove,
    onFocus,
  } = props

  const open = members(projects)
  const closed = projects.filter((project) => project.pinnedSlot === null)
  const unregistered = discovery?.candidates.filter((c) => !c.registered) ?? []

  return (
    <section className="panel panel--manager" aria-label="Projects">
      <header className="panel__header">
        <h2 className="panel__name">PROJECTS</h2>
        <div className="panel__header-actions">
          <button type="button" className="button button--small" onClick={onOpenNew} disabled={busy}>
            + New Project
          </button>
          <button
            type="button"
            className="button button--small button--quiet"
            onClick={onOpenRegister}
            disabled={busy}
          >
            Open / Register
          </button>
        </div>
      </header>

      <div className="panel__body panel__body--scroll">
        {projectsError && <p className="notice notice--error">{describeError(projectsError)}</p>}

        {projectsLoading && projects.length === 0 && <p className="notice">Loading projects…</p>}

        {!projectsLoading && !projectsError && projects.length === 0 && (
          <p className="notice">
            No projects registered yet. Use <strong>+ New Project</strong> to create one, or{' '}
            <strong>Open / Register</strong> to adopt a folder you already have.
          </p>
        )}

        {projects.length > 0 && (
          <section className="manager__section" aria-label="In the workspace">
            <div className="manager__section-header">
              <h3>In the workspace</h3>
              <span className="manager__count">{open.length}</span>
            </div>

            {open.length === 0 ? (
              <p className="notice notice--quiet">
                Nothing is open. A project you open here gets a panel, and its terminal keeps
                running whether or not the panel is showing.
              </p>
            ) : (
              <ul className="project-list">
                {open.map((project, index) => (
                  <li key={project.id} className="project-list__row" data-project-id={project.id}>
                    <span className="project-list__slot" aria-label={`Slot ${index + 1}`}>
                      {displaySlot(project.pinnedSlot)}
                    </span>
                    <button
                      type="button"
                      className="project-list__item"
                      onClick={() => onFocus(project)}
                      aria-label={`Open ${project.name} on its own`}
                      title={`${project.hostPath} — open this project on its own`}
                    >
                      <span className="project-list__name">{project.name}</span>
                      <span className="project-list__path">{shortenPath(project.runtimePath, 28)}</span>
                    </button>
                    <span className="project-list__actions">
                      <button
                        type="button"
                        className="button button--small"
                        aria-label={`Move ${project.name} left`}
                        disabled={busy || index === 0}
                        onClick={() => onMove(project, 'left')}
                      >
                        <span aria-hidden="true">‹</span>
                      </button>
                      <button
                        type="button"
                        className="button button--small"
                        aria-label={`Move ${project.name} right`}
                        disabled={busy || index === open.length - 1}
                        onClick={() => onMove(project, 'right')}
                      >
                        <span aria-hidden="true">›</span>
                      </button>
                      <button
                        type="button"
                        className="button button--small button--quiet"
                        aria-label={`Remove ${project.name} from the workspace`}
                        title="Take the panel off the grid. The runtime keeps running."
                        disabled={busy}
                        onClick={() => onRemoveFromWorkspace(project)}
                      >
                        <span aria-hidden="true">×</span>
                      </button>
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </section>
        )}

        {closed.length > 0 && (
          <section className="manager__section" aria-label="Registered">
            <div className="manager__section-header">
              <h3>Registered</h3>
              <span className="manager__count">{closed.length}</span>
            </div>
            <ul className="project-list">
              {closed.map((project) => (
                <li key={project.id} className="project-list__row" data-project-id={project.id}>
                  <button
                    type="button"
                    className="project-list__item"
                    onClick={() => onOpenInWorkspace(project)}
                    aria-label={`Open ${project.name} in the workspace`}
                    title={`${project.hostPath} — open a panel for this project`}
                    disabled={busy}
                  >
                    <span className="project-list__name">{project.name}</span>
                    <span className="project-list__path">
                      {shortenPath(project.runtimePath, 28)}
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          </section>
        )}

        <section className="manager__section" aria-label="Discover">
          <div className="manager__section-header">
            <h3>Discover</h3>
            <button
              type="button"
              className="button button--small button--quiet"
              onClick={onDiscover}
              disabled={busy || discoveryLoading}
            >
              {discoveryLoading ? 'Scanning…' : 'Scan'}
            </button>
          </div>

          {discoveryError && <p className="notice notice--error">{describeError(discoveryError)}</p>}

          {discovery && (
            <>
              {discovery.warnings.map((warning) => (
                <p className="notice notice--warning" key={warning}>
                  {warning}
                </p>
              ))}

              {discovery.roots.map((root) => (
                <p className="notice notice--quiet" key={root.path}>
                  Root <code>{shortenPath(root.path, 32)}</code>
                  {root.exists ? '' : ` — ${root.error ?? 'not available'}`}
                </p>
              ))}

              {unregistered.length === 0 ? (
                <p className="notice">
                  Nothing new found in {discovery.scannedDirectories}{' '}
                  {pluralise(discovery.scannedDirectories, 'folder')}
                  {discovery.truncated ? ' (the scan stopped early)' : ''}.
                </p>
              ) : (
                <ul className="candidate-list">
                  {unregistered.map((candidate) => (
                    <li className="candidate" key={candidate.hostPath}>
                      <div className="candidate__text">
                        <span className="candidate__name" title={candidate.hostPath}>
                          {candidate.name}
                        </span>
                        <span className="candidate__path">{shortenPath(candidate.hostPath, 32)}</span>
                        <span className="candidate__markers">
                          {candidate.confidence} confidence &middot; {candidate.markers.join(', ')}
                          {candidate.nameDiscounted ? ' · folder name ranks it lower' : ''}
                        </span>
                      </div>
                      <button
                        type="button"
                        className="button button--small"
                        // Named per candidate: the panel already has a
                        // Register button, and a screen reader should not hear
                        // two identical ones.
                        aria-label={`Register ${candidate.name}`}
                        disabled={busy}
                        onClick={() => onRegisterCandidate(candidate)}
                      >
                        Register
                      </button>
                    </li>
                  ))}
                </ul>
              )}
            </>
          )}

          {!discovery && !discoveryLoading && !discoveryError && (
            <p className="notice notice--quiet">
              Scan a Projects Root for folders that look like projects. Scanning only suggests;
              nothing is registered until you say so.
            </p>
          )}
        </section>
      </div>

      <footer className="panel__footer panel__footer--manager">
        <button type="button" className="button button--small button--quiet" onClick={onRefresh} disabled={busy}>
          Refresh
        </button>
      </footer>
    </section>
  )
}
