import type { Project } from '../api/types'
import {
  describeProjectLocation,
  describeStatus,
  formatTimestamp,
  isRuntimeBusy,
  isRuntimeUp,
  statusGlyph,
} from '../lib/format'
import { useTerminalSession } from '../terminal/useTerminal'
import { ProjectTerminal } from './ProjectTerminal'
import { PromptBar } from './PromptBar'

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
 * ProjectPanel is the chrome around a project and, when there is one, its
 * terminal.
 *
 * The terminal is only subscribed to while the runtime is up, and the panel
 * says which of the two reasons it is not showing one rather than drawing an
 * empty screen: a server that cannot host a terminal and a project whose
 * terminal is stopped are different facts with different fixes, and a black
 * rectangle would state neither.
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
  const watching = terminalAvailable && up
  const session = useTerminalSession(project.id, watching)
  const connected = session.status.state === 'open'

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

      <div className={`panel__body${watching ? ' panel__body--terminal' : ''}`}>
        {watching ? (
          <ProjectTerminal session={session} />
        ) : (
          <div className="panel__placeholder">
            {!terminalAvailable ? (
              <>
                <p className="panel__placeholder-title">Terminal runtime unavailable</p>
                <p className="panel__placeholder-text">
                  {terminalBlocker || 'This server cannot host a terminal runtime.'}
                </p>
              </>
            ) : (
              <>
                <p className="panel__placeholder-title">This project’s terminal is stopped</p>
                <p className="panel__placeholder-text">
                  Start the runtime to see it here. The session behind it outlives this page: it
                  keeps running while you reload, close the browser, or restart AgentMux, and its
                  scrollback is still there when you come back.
                </p>
              </>
            )}
          </div>
        )}

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
        {/* The Prompt Bar writes to the terminal through the same messages a
            keystroke does; it is disabled until there is a live one to write
            to, rather than accepting text that would go nowhere.

            Both halves are needed. The connection is the page's, not this
            panel's - one socket serves every project - so an open connection
            says nothing about whether *this* project has a terminal. Without
            the second half, a panel whose runtime is stopped would offer an
            enabled field while a socket was open for the project next to it,
            and swallow whatever was typed into it. */}
        <PromptBar session={session} disabled={!watching || !connected} />
      </footer>
    </section>
  )
}
