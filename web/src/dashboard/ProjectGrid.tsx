import type { CSSProperties } from 'react'

import type { ProjectCard as ProjectCardData } from '../api/types'
import { ProjectPanel } from './ProjectPanel'
import type { DashboardColumns } from './useDashboardColumns'

/**
 * The projects, as a grid of cards.
 *
 * # The column count is a variable, not three rules
 *
 * The number comes from `useDashboardColumns` and is handed to the stylesheet as
 * a custom property, so `grid-template-columns` is written once. The alternative
 * - three media queries, one per tier - would put the same numbers in two files
 * and make a layout test impossible: jsdom does not evaluate media queries, so
 * "three columns on a desktop" would be a claim nothing checked.
 *
 * The rows are the grid's business. There is no fixed height anywhere, because
 * a card whose text is longer than its neighbour's must not clip - and because
 * §10 of the phase brief asks for exactly that.
 *
 * # No pagination, and why
 *
 * The workspace paginates because a page holds a fixed number of terminal
 * panels and the manager, and a grid without pages would show a terminal the
 * user cannot reach. A card is not a terminal. There is nothing to reach into,
 * so the grid grows downwards and the page scrolls, which is what §11 asks for.
 */
export interface ProjectGridProps {
  cards: ProjectCardData[]
  columns: DashboardColumns
}

export function ProjectGrid({ cards, columns }: ProjectGridProps) {
  if (cards.length === 0) {
    return (
      // Not a live region. The message is static content, and a page that
      // announced it on every render would be a page that talks over itself -
      // the server bar and the stale strip are the two things worth announcing.
      <p className="project-grid__empty">No projects</p>
    )
  }

  return (
    <div
      className="project-grid"
      data-columns={columns}
      style={{ '--columns': columns } as CSSProperties}
    >
      {cards.map((card) => (
        <ProjectPanel key={card.id} card={card} />
      ))}
    </div>
  )
}
