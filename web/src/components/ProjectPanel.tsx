import type { Project } from '../api/types'
import { describeProjectLocation, describeStatus, formatTimestamp, statusGlyph } from '../lib/format'

interface ProjectPanelProps {
  project: Project
}

/**
 * ProjectPanel is the chrome around a project.
 *
 * There is no terminal here because there is no terminal runtime yet: Phase 2
 * adds tmux and Phase 4 adds xterm.js over a WebSocket. Until then the body
 * states that plainly and shows what the server actually knows about the
 * project, which is more useful to a user than a black rectangle that never
 * prints anything.
 */
export function ProjectPanel({ project }: ProjectPanelProps) {
  return (
    <section className="panel" aria-label={`Project ${project.name}`}>
      <header className="panel__header">
        <div className="panel__identity">
          <h2 className="panel__name" title={project.hostPath}>
            {project.name}
          </h2>
          <span className={`panel__status panel__status--${project.status}`}>
            <span aria-hidden="true">{statusGlyph(project.status)}</span> {describeStatus(project.status)}
          </span>
        </div>
      </header>

      <div className="panel__body">
        <div className="panel__placeholder">
          <p className="panel__placeholder-title">Terminal not available yet</p>
          <p className="panel__placeholder-text">
            This build is <strong>Phase 1</strong> and has no session runtime. Persistent terminals
            arrive with tmux in Phase 2, and the terminal view in Phase 4. Until then this panel
            shows the project as the server has it recorded.
          </p>
        </div>

        <dl className="metadata">
          <div className="metadata__row">
            <dt>Location</dt>
            <dd title={project.hostPath}>{describeProjectLocation(project)}</dd>
          </div>
          <div className="metadata__row">
            <dt>Host path</dt>
            <dd>
              <code>{project.hostPath}</code>
            </dd>
          </div>
          <div className="metadata__row">
            <dt>Runtime path</dt>
            <dd>
              <code>{project.runtimePath}</code>
            </dd>
          </div>
          <div className="metadata__row">
            <dt>Session name</dt>
            <dd>
              <code>amx-{project.id}</code>
            </dd>
          </div>
          <div className="metadata__row">
            <dt>Registered</dt>
            <dd>{formatTimestamp(project.createdAt)}</dd>
          </div>
          <div className="metadata__row">
            <dt>Last opened</dt>
            <dd>{formatTimestamp(project.lastOpenedAt)}</dd>
          </div>
        </dl>
      </div>

      <footer className="panel__footer">
        <input
          className="panel__prompt"
          type="text"
          placeholder="Message Claude… (available in Phase 3)"
          disabled
          aria-label="Message Claude"
        />
        <button type="button" className="button" disabled title="Available in Phase 3">
          Send
        </button>
      </footer>
    </section>
  )
}
