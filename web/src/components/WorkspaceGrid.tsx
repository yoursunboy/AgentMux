/**
 * The workspace grid: the cells of the current page, laid out.
 *
 * It is a layout and nothing else. It does not fetch, does not know what a
 * runtime is, and does not decide which projects are in the workspace - it is
 * handed the cells and draws them. That is what lets the same page be a 3×2
 * grid of terminals on a desktop, a 2×2 grid on a tablet held sideways, and one
 * terminal at a time on a phone, without any of those being a different
 * feature.
 *
 * # The manager is a cell
 *
 * The Project Manager is the last cell of every page and is not a project: it
 * has no terminal, no runtime and no id, and it is deliberately not a component
 * that pretends otherwise. `docs/WORKSPACE.md` says why it is a panel rather
 * than a sidebar.
 *
 * # Every cell is on its own
 *
 * Each cell is wrapped in a boundary, so a panel that throws takes itself off
 * the page and leaves its neighbours alone. See `PanelBoundary`.
 */
import type { ReactNode } from 'react'

import type { Project } from '../api/types'
import type { WorkspaceColumns, WorkspaceItem } from '../workspace/slots'
import { PanelBoundary } from './PanelBoundary'

interface WorkspaceGridProps {
  items: WorkspaceItem[]
  columns: WorkspaceColumns
  /** One cell, drawn by the caller, which is where the wiring lives. */
  renderProject(project: Project, slot: number): ReactNode
  renderManager(): ReactNode
}

export function WorkspaceGrid({ items, columns, renderProject, renderManager }: WorkspaceGridProps) {
  return (
    <main
      className={`workspace workspace--${columns}`}
      style={{ gridTemplateColumns: `repeat(${columns}, minmax(0, 1fr))` }}
      aria-label="Workspace"
    >
      {items.map((item) => {
        if (item.kind === 'manager') {
          return (
            <PanelBoundary key="manager" label="Project Manager">
              {renderManager()}
            </PanelBoundary>
          )
        }
        // Keyed by project, so a panel that moves keeps its identity and a
        // different project is a different terminal rather than a change to
        // this one.
        return (
          <PanelBoundary key={item.project.id} label={`Project ${item.project.name}`}>
            {renderProject(item.project, item.slot)}
          </PanelBoundary>
        )
      })}
    </main>
  )
}
