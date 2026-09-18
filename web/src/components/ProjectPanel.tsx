/**
 * One project in the workspace: a header, its terminal, and its prompt bar.
 *
 * # The header is three things
 *
 * A name, a runtime state, and a way in. Everything else - where the project
 * lives, what its session is called, when it was registered, whether a Claude
 * process is running inside it - is one menu item away, because a grid panel is
 * a terminal and every line of chrome is a line of terminal nobody can see.
 * `docs/UI_SPEC.md` §6 draws the same header.
 *
 * # What the panel does not decide
 *
 * It does not know what the workspace is. Moving, pinning and removing are
 * callbacks, and the runtime actions are callbacks, so this component can be
 * drawn in a grid, on its own, or full screen without knowing which it is in.
 * That is also what makes the three display modes safe: they are the same
 * component in a different box, and none of them can reach the runtime except
 * through the two buttons that say so.
 */
import { useState } from 'react'

import type { Project } from '../api/types'
import {
  describeStatus,
  isRuntimeBusy,
  isRuntimeUp,
  statusGlyph,
} from '../lib/format'
import { useTerminalSession } from '../terminal/useTerminal'
import { ConfirmDialog } from './ConfirmDialog'
import { PanelMenu, type MenuItem } from './PanelMenu'
import { ProjectTerminal } from './ProjectTerminal'
import { PromptBar } from './PromptBar'

/** Where this panel sits in the workspace, and which way it can move. */
export interface PanelPosition {
  /** Zero-based workspace slot. */
  slot: number
  /** How many projects are in the workspace. */
  total: number
  canMoveLeft: boolean
  canMoveRight: boolean
}

/** Everything the panel can ask for. None of it is implemented here. */
export interface PanelActions {
  startRuntime(): void
  stopRuntime(): void
  startAgent(): void
  destroyRuntime(): void
  focus(): void
  /** Leave focus or full screen and go back to the grid. */
  leaveFocus(): void
  toggleFullscreen(): void
  move(direction: 'left' | 'right'): void
  pin(slot: number): void
  remove(): void
  details(): void
}

interface ProjectPanelProps {
  project: Project
  /**
   * Whether this server can run a terminal at all: the build has the runtime,
   * the server is on the right side of the WSL boundary, and tmux is installed
   * where sessions run. False means the controls are not offered at all.
   */
  terminalAvailable: boolean
  /** Why not, when it cannot. Empty when it can. */
  terminalBlocker: string
  /** True while a runtime request is in flight for any project. */
  busy: boolean
  position: PanelPosition
  actions: PanelActions
  /** Grid, focus, or full screen. It changes the font and the chrome, nothing else. */
  mode: 'grid' | 'focus' | 'fullscreen'
  /** False when this browser cannot go full screen, so the item is not offered. */
  canFullscreen: boolean
}

/** How large the terminal's text is in each display mode. */
const FONT_SIZE: Record<ProjectPanelProps['mode'], number> = {
  grid: 11,
  focus: 12.5,
  fullscreen: 13,
}

export function ProjectPanel({
  project,
  terminalAvailable,
  terminalBlocker,
  busy,
  position,
  actions,
  mode,
  canFullscreen,
}: ProjectPanelProps) {
  const [confirmDestroy, setConfirmDestroy] = useState(false)

  const up = isRuntimeUp(project.status)
  const transitioning = isRuntimeBusy(project.status)
  const controlsDisabled = busy || transitioning || !terminalAvailable
  const watching = terminalAvailable && up
  const session = useTerminalSession(project.id, watching)
  const connected = session.status.state === 'open'
  const focused = mode !== 'grid'

  const menu: MenuItem[] = [
    {
      key: 'focus',
      label: focused ? 'Back to the workspace' : 'Focus',
      onSelect: () => (focused ? actions.leaveFocus() : actions.focus()),
    },
    ...(focused && canFullscreen
      ? [
          {
            key: 'fullscreen',
            label: mode === 'fullscreen' ? 'Leave full screen' : 'Full screen',
            onSelect: actions.toggleFullscreen,
          },
        ]
      : []),
    {
      key: 'redraw',
      label: 'Redraw terminal',
      onSelect: session.askAgain,
      disabled: !watching,
      title: 'Ask the server for this terminal again from a fresh screen',
      separated: true,
    },
    {
      key: 'pin',
      label: `Pin to slot ${position.slot + 1}`,
      onSelect: () => actions.pin(position.slot),
    },
    {
      key: 'left',
      label: 'Move left',
      onSelect: () => actions.move('left'),
      disabled: !position.canMoveLeft,
      title: position.canMoveLeft ? '' : 'This project is already first',
    },
    {
      key: 'right',
      label: 'Move right',
      onSelect: () => actions.move('right'),
      disabled: !position.canMoveRight,
      title: position.canMoveRight ? '' : 'This project is already last',
    },
    {
      key: 'start-agent',
      label: 'Start Claude here',
      onSelect: actions.startAgent,
      disabled: controlsDisabled || !up,
      title: up
        ? 'Start Claude Code in this terminal. Starts it only if it is not already running.'
        : 'Start the runtime first: an agent with no terminal is a process nobody can see',
      separated: true,
    },
    {
      key: 'stop',
      label: 'Stop runtime',
      onSelect: actions.stopRuntime,
      disabled: controlsDisabled || !up,
      title: 'End what is running, keeping the terminal and its scrollback',
    },
    {
      key: 'destroy',
      label: 'Destroy runtime…',
      onSelect: () => setConfirmDestroy(true),
      disabled: controlsDisabled || !up,
      tone: 'danger',
      title: 'Remove the session and its scrollback. This cannot be undone',
    },
    {
      key: 'details',
      label: 'Project details',
      onSelect: actions.details,
      separated: true,
    },
    {
      key: 'remove',
      label: 'Remove from workspace',
      onSelect: actions.remove,
      title: 'Take this project off the grid. Its runtime keeps running',
    },
  ]

  return (
    <section
      className={`panel panel--project panel--${mode}`}
      aria-label={`Project ${project.name}`}
      data-project-id={project.id}
    >
      <header className="panel__header">
        {focused && (
          <button
            type="button"
            className="button button--small panel__back"
            onClick={actions.leaveFocus}
            aria-label="Back to the workspace"
            title="Back to the workspace. The terminal is untouched."
          >
            <span aria-hidden="true">←</span> Workspace
          </button>
        )}

        <div className="panel__identity">
          <h2 className="panel__name" title={`${project.name} — ${project.hostPath}`}>
            {project.name}
          </h2>
          <span
            className={`panel__status panel__status--${project.status}`}
            title={describeStatus(project.status)}
          >
            <span aria-hidden="true">{statusGlyph(project.status)}</span>{' '}
            {describeStatus(project.status)}
          </span>
        </div>

        <div className="panel__header-actions">
          {focused
            ? canFullscreen && (
                <button
                  type="button"
                  className="button button--small"
                  onClick={actions.toggleFullscreen}
                  aria-label={mode === 'fullscreen' ? 'Leave full screen' : 'Full screen'}
                  title={mode === 'fullscreen' ? 'Leave full screen' : 'Full screen'}
                >
                  <span aria-hidden="true">⛶</span>
                </button>
              )
            : (
                <button
                  type="button"
                  className="button button--small panel__focus"
                  onClick={actions.focus}
                  aria-label={`Focus ${project.name}`}
                  title="Show this project on its own"
                >
                  <span aria-hidden="true">⛶</span>
                </button>
              )}
          <PanelMenu label={`Actions for ${project.name}`} items={menu} />
        </div>
      </header>

      <div className={`panel__body${watching ? ' panel__body--terminal' : ''}`}>
        {watching ? (
          <ProjectTerminal session={session} fontSize={FONT_SIZE[mode]} showRedraw={mode !== 'grid'} />
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
                <p className="panel__placeholder-title">Runtime stopped</p>
                <p className="panel__placeholder-text">
                  The session behind this panel outlives the page: it keeps running while you
                  reload, close the browser, or restart AgentMux.
                </p>
                <div className="panel__placeholder-actions">
                  <button
                    type="button"
                    className="button button--primary"
                    onClick={actions.startRuntime}
                    disabled={controlsDisabled}
                    title={
                      terminalAvailable
                        ? 'Start this project’s terminal session'
                        : 'This server cannot host a terminal runtime'
                    }
                  >
                    Start
                  </button>
                </div>
              </>
            )}
          </div>
        )}

        {/* In the grid the terminal is the panel, so nothing else is drawn. The
            details a grid header must not carry are one menu item away. */}
      </div>

      <footer className="panel__footer">
        {/* The Prompt Bar writes to the terminal through the same messages a
            keystroke does; it is disabled until there is a live one to write to
            rather than accepting text that would go nowhere.

            Both halves are needed. The connection is the page's, not this
            panel's - one socket serves every project - so an open connection
            says nothing about whether *this* project has a terminal. */}
        <PromptBar
          session={session}
          disabled={!watching || !connected}
          {...(mode === 'grid' ? { compact: true } : {})}
        />
      </footer>

      {confirmDestroy && (
        <DestroyDialog
          project={project}
          onCancel={() => setConfirmDestroy(false)}
          onConfirm={() => {
            setConfirmDestroy(false)
            actions.destroyRuntime()
          }}
        />
      )}
    </section>
  )
}

/** DestroyDialog is the confirmation, kept out of the menu so the menu can close. */
function DestroyDialog({
  project,
  onCancel,
  onConfirm,
}: {
  project: Project
  onCancel: () => void
  onConfirm: () => void
}) {
  return (
    <ConfirmDialog
      title={`Destroy ${project.name}'s runtime?`}
      body={
        <>
          <p>
            The session <code>amx-{project.id}</code> is removed, and everything printed in it goes
            with it. Work that was running in it stops.
          </p>
          <p>
            Use <strong>Stop runtime</strong> instead if you want the terminal and its scrollback to
            still be there when you come back.
          </p>
        </>
      }
      confirmLabel="Destroy runtime"
      onConfirm={onConfirm}
      onCancel={onCancel}
    />
  )
}
