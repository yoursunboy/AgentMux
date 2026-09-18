import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { WorkspacePager } from './WorkspacePager'

function renderPager(page = 0, pages = 2) {
  const onPrevious = vi.fn()
  const onNext = vi.fn()
  const view = render(
    <WorkspacePager page={page} pages={pages} onPrevious={onPrevious} onNext={onNext} />,
  )
  return { ...view, onPrevious, onNext }
}

describe('WorkspacePager', () => {
  it('is not there at all when there is one page', () => {
    // A workspace that fits on one screen should pay nothing for a control it
    // cannot use.
    const { container } = renderPager(0, 1)

    expect(container).toBeEmptyDOMElement()
    expect(screen.queryByRole('group', { name: 'Workspace pages' })).not.toBeInTheDocument()
  })

  it('counts the pages the way a person counts', () => {
    renderPager(0, 3)
    expect(screen.getByText('1 / 3')).toBeInTheDocument()

    renderPager(2, 3)
    expect(screen.getByText('3 / 3')).toBeInTheDocument()
  })

  it('goes forward and back', async () => {
    const { onNext, onPrevious } = renderPager(1, 3)

    await userEvent.click(screen.getByRole('button', { name: 'Next page' }))
    expect(onNext).toHaveBeenCalledOnce()

    await userEvent.click(screen.getByRole('button', { name: 'Previous page' }))
    expect(onPrevious).toHaveBeenCalledOnce()
  })

  it('disables the direction with no page beyond it', () => {
    const { unmount } = renderPager(0, 2)
    expect(screen.getByRole('button', { name: 'Previous page' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Next page' })).toBeEnabled()
    unmount()

    renderPager(1, 2)
    expect(screen.getByRole('button', { name: 'Previous page' })).toBeEnabled()
    expect(screen.getByRole('button', { name: 'Next page' })).toBeDisabled()
  })

  it('changes page on a sideways swipe, which is the gesture a tablet has', () => {
    // fireEvent rather than userEvent: a swipe is a pair of touch events, and
    // what is under test is the distance the finger travelled.
    const { onNext } = renderPager(0, 2)
    const strip = screen.getByRole('group', { name: 'Workspace pages' })

    fireTouch(strip, 'touchstart', 300)
    fireTouch(strip, 'touchend', 200)

    expect(onNext).toHaveBeenCalledOnce()
  })

  it('ignores a drag too short to be a swipe', () => {
    const { onNext, onPrevious } = renderPager(1, 3)
    const strip = screen.getByRole('group', { name: 'Workspace pages' })

    fireTouch(strip, 'touchstart', 300)
    fireTouch(strip, 'touchend', 280)

    expect(onNext).not.toHaveBeenCalled()
    expect(onPrevious).not.toHaveBeenCalled()
  })

  it('swipes backwards as well as forwards', () => {
    const { onPrevious } = renderPager(1, 3)
    const strip = screen.getByRole('group', { name: 'Workspace pages' })

    fireTouch(strip, 'touchstart', 200)
    fireTouch(strip, 'touchend', 320)

    expect(onPrevious).toHaveBeenCalledOnce()
  })
})

/** fireTouch dispatches one end of a swipe at a horizontal position. */
function fireTouch(element: HTMLElement, type: 'touchstart' | 'touchend', clientX: number): void {
  const touch = { clientX } as Touch
  const event = new Event(type, { bubbles: true }) as TouchEvent
  Object.defineProperty(event, type === 'touchstart' ? 'touches' : 'changedTouches', {
    value: [touch],
  })
  element.dispatchEvent(event)
}
