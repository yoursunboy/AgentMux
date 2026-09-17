import type { Candidate, DiscoveryResult, Project } from '../api/types'
import { describeError, pluralise, shortenPath } from '../lib/format'

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
  selectedId: string | null
  onSelect: (project: Project) => void
}

/**
 * ProjectManagerPanel is the fourth grid cell: a compact list of what the user
 * has, plus the three ways to get more.
 *
 * It is a panel rather than a page. The UI spec has no permanent sidebar and
 * no second navigation bar, so everything here is a short list and a few
 * buttons.
 */
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
    selectedId,
    onSelect,
  } = props

  const unregistered = discovery?.candidates.filter((c) => !c.registered) ?? []

  return (
    <section className="panel panel--manager" aria-label="Projects">
      <header className="panel__header">
        <h2 className="panel__name">PROJECTS</h2>
        <div className="panel__header-actions">
          <button type="button" className="button" onClick={onOpenNew} disabled={busy}>
            + New
          </button>
          <button type="button" className="button button--quiet" onClick={onOpenRegister} disabled={busy}>
            Register
          </button>
        </div>
      </header>

      <div className="panel__body panel__body--scroll">
        {projectsError && <p className="notice notice--error">{describeError(projectsError)}</p>}

        {projectsLoading && projects.length === 0 && <p className="notice">Loading projects…</p>}

        {!projectsLoading && !projectsError && projects.length === 0 && (
          <p className="notice">
            No projects registered yet. Use <strong>New</strong> to create one, or{' '}
            <strong>Register</strong> to adopt a folder you already have.
          </p>
        )}

        {projects.length > 0 && (
          <ul className="project-list">
            {projects.map((project) => (
              <li key={project.id}>
                <button
                  type="button"
                  className={
                    project.id === selectedId ? 'project-list__item is-selected' : 'project-list__item'
                  }
                  onClick={() => onSelect(project)}
                  title={project.hostPath}
                >
                  <span className="project-list__name">{project.name}</span>
                  <span className="project-list__path">{shortenPath(project.runtimePath, 34)}</span>
                </button>
              </li>
            ))}
          </ul>
        )}

        <div className="manager__section">
          <div className="manager__section-header">
            <h3>Discover</h3>
            <button type="button" className="button button--quiet" onClick={onDiscover} disabled={busy || discoveryLoading}>
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
                        className="button"
                        // Named per candidate: the panel already has a Register
                        // button, and a screen reader should not hear two
                        // identical ones.
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
              Scan a Projects Root for folders that look like projects. Scanning only suggests; nothing
              is registered until you say so.
            </p>
          )}
        </div>
      </div>

      <footer className="panel__footer panel__footer--manager">
        <button type="button" className="button button--quiet" onClick={onRefresh} disabled={busy}>
          Refresh
        </button>
      </footer>
    </section>
  )
}
