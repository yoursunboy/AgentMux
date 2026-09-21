import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { makeProjectCard } from '../test/controller'
import { ProjectGrid } from './ProjectGrid'

describe('ProjectGrid', () => {
  // §11: an installation with nothing registered says so, rather than showing an
  // empty page with no explanation of why it is empty.
  it('says there are no projects rather than showing nothing', () => {
    render(<ProjectGrid cards={[]} columns={3} />)
    expect(screen.getByText('No projects')).toBeInTheDocument()
    // Not a live region: it is static content, and a page that announced it on
    // every render would talk over the things that are worth announcing.
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
  })

  it('renders one card per project', () => {
    const cards = [
      makeProjectCard({ id: 'p_a', name: 'alpha' }),
      makeProjectCard({ id: 'p_b', name: 'bravo' }),
      makeProjectCard({ id: 'p_c', name: 'charlie' }),
    ]
    render(<ProjectGrid cards={cards} columns={3} />)

    expect(screen.getAllByRole('article')).toHaveLength(3)
    expect(screen.getByRole('heading', { name: 'alpha' })).toBeInTheDocument()
  })

  // §9: the server sorted them, and the grid does not sort them again. This
  // asserts the order is the order it was given, which is the whole of the rule.
  it('keeps the order the server sent', () => {
    const cards = [
      makeProjectCard({ id: 'p_z', name: 'needs-you' }),
      makeProjectCard({ id: 'p_a', name: 'idle' }),
    ]
    render(<ProjectGrid cards={cards} columns={3} />)

    const names = screen.getAllByRole('heading', { level: 3 }).map((node) => node.textContent)
    expect(names).toEqual(['needs-you', 'idle'])
  })

  it('hands the column count to the stylesheet rather than writing rules', () => {
    const { container } = render(<ProjectGrid cards={[makeProjectCard()]} columns={2} />)
    const grid = container.querySelector('.project-grid')

    expect(grid).toHaveAttribute('data-columns', '2')
    expect(grid).toHaveStyle({ '--columns': '2' })
  })
})
