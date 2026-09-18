/**
 * Where a project actually lives, and under what identity.
 *
 * A grid panel's header does not carry a host path, a runtime path, a session
 * name, or a registration date, because those are things a person needs once -
 * when something is wrong, or when they are about to run a command by hand -
 * and the terminal needs the height every second. This dialog is where "once"
 * goes: reachable from every panel's menu, honest about everything the panel
 * deliberately does not say.
 *
 * The session name is here rather than hidden, and it is the reason a person
 * opens this. It is what attaching a terminal by hand needs, and it is derived
 * from the project's id rather than its name so that renaming a project cannot
 * orphan a running session.
 */
import { useEffect } from 'react'

import type { Project } from '../api/types'
import { describeProjectLocation, displaySlot, formatTimestamp, sessionNameFor } from '../lib/format'

interface ProjectDetailsDialogProps {
  project: Project
  onClose: () => void
}

export function ProjectDetailsDialog({ project, onClose }: ProjectDetailsDialogProps) {
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent): void => {
      if (event.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [onClose])

  return (
    <div
      className="dialog-backdrop"
      onMouseDown={(event) => event.target === event.currentTarget && onClose()}
    >
      <div
        className="dialog dialog--wide"
        role="dialog"
        aria-modal="true"
        aria-label={`${project.name} details`}
      >
        <h2 className="dialog__title">{project.name}</h2>

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
              <code>{sessionNameFor(project.id)}</code>
            </dd>
          </div>
          <div className="metadata__row">
            <dt>Workspace slot</dt>
            <dd>{displaySlot(project.pinnedSlot)}</dd>
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

        <p className="field__hint">
          The tmux socket is <code>&lt;data-dir&gt;/tmux/{project.id}.sock</code>. Each project has
          its own tmux server, so attaching to one cannot disturb another.
        </p>

        <div className="dialog__actions">
          <button type="button" className="button" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
    </div>
  )
}
