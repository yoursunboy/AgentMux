import type { Project } from '../api/types'
import {
  describeProjectLocation,
  describeStatus,
  formatTimestamp,
  isRuntimeBusy,
  isRuntimeUp,
  statusGlyph,
} from '../lib/format'

interface ProjectPanelProps {
  project: Project
  /**
   * Whether this server can run a terminal at all: the build has the runtime,
   * the server is on the right side of the WSL boundary, and tmux is installed
   * where sessions run. False means the controls are not offered at all.
   */
  terminalAvailable: boolean
  /**
   * Why not, when it cannot. Empty when it can. This is the server's own
   * wording, because only the server knows which of the three is missing.
   */
  terminalBlocker: string
  /** True while a runtime request is in flight for any project. */
  busy: boolean
  onStartRuntime: () => void
  onStopRuntime: () => void
}

/**
 * ProjectPanel is the chrome around a project.
 *
 * There is still no terminal here, and it does not pretend otherwise: the
 * runtime is real and persistent, but nothing streams its output to a browser
 * until the terminal view lands in Phase 4. What this panel offers is what the
 * server can actually do today - start and stop the session - and a plain
 * statement of what is not here yet.
 */
export function ProjectPanel({
  project,
  terminalAvailable,
  terminalBlocker,
  busy,
  onStartRuntime,
  onStopRuntime,
}: ProjectPanelProps) {
  const up = isRuntimeUp(project.status)
  const transitioning = isRuntimeBusy(project.status)
  const controlsDisabled = busy || transitioning || !terminalAvailable

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

        <div className="panel__runtime">
          <span className="panel__runtime-label">Runtime</span>
          <button
            type="button"
            className="button"
            onClick={onStartRuntime}
            disabled={controlsDisabled || up}
            title={
              terminalAvailable
                ? 'Start this project’s terminal session'
                : 'This server cannot host a terminal runtime'
            }
          >
            Start runtime
          </button>
          <button
            type="button"
            className="button"
            onClick={onStopRuntime}
            disabled={controlsDisabled || !up}
            title="End what is running, keeping the terminal and its scrollback"
          >
            Stop runtime
          </button>
        </div>
      </header>

      <div className="panel__body">
        <div className="panel__placeholder">
          {terminalAvailable ? (
            <>
              <p className="panel__placeholder-title">Terminal UI coming in Phase 4</p>
              <p className="panel__placeholder-text">
                The session behind this button is real and outlives the server: it keeps running
                while you reload the page, close the browser, or restart AgentMux. This build is
                <strong> Phase 2</strong>, so starting it is all the UI can do — streaming the
                terminal into this panel arrives in Phase 4, and running Claude Code in it in
                Phase 3.
              </p>
            </>
          ) : (
            <>
              <p className="panel__placeholder-title">Terminal runtime unavailable</p>
              <p className="panel__placeholder-text">
                {terminalBlocker || 'This server cannot host a terminal runtime.'}
              </p>
            </>
          )}
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
