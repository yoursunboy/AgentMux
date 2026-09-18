/**
 * The pager: which page of the workspace is showing, and how to get to another.
 *
 * It is a thin strip that exists only when there is more than one page, so a
 * workspace that fits on one screen pays nothing for it. `docs/UI_SPEC.md` asks
 * for a minimal control at the top, and that is what this is: two buttons and a
 * count, no animation and no carousel.
 *
 * # Why the swipe is only here
 *
 * A tablet or a phone can change page by swiping the strip sideways. That
 * gesture is deliberately not available on the terminal itself: a horizontal
 * drag inside a terminal is how a person selects text, how a long line is read,
 * and - in a full-screen program - how a mouse-reporting application is driven.
 * Taking it for navigation would break all three, and the strip is a place
 * where nothing else is happening.
 */
import { useRef, type TouchEvent } from 'react'

/** How far a finger has to travel sideways to count as a swipe, in pixels. */
const SWIPE_THRESHOLD_PX = 48

interface WorkspacePagerProps {
  page: number
  pages: number
  onPrevious: () => void
  onNext: () => void
}

export function WorkspacePager({ page, pages, onPrevious, onNext }: WorkspacePagerProps) {
  const startX = useRef<number | null>(null)

  if (pages <= 1) return null

  const onTouchStart = (event: TouchEvent<HTMLDivElement>): void => {
    startX.current = event.touches[0]?.clientX ?? null
  }
  const onTouchEnd = (event: TouchEvent<HTMLDivElement>): void => {
    const from = startX.current
    startX.current = null
    if (from === null) return
    const to = event.changedTouches[0]?.clientX
    if (to === undefined) return
    const distance = to - from
    if (Math.abs(distance) < SWIPE_THRESHOLD_PX) return
    if (distance < 0) onNext()
    else onPrevious()
  }

  return (
    <div
      className="pager"
      role="group"
      aria-label="Workspace pages"
      onTouchStart={onTouchStart}
      onTouchEnd={onTouchEnd}
    >
      <button
        type="button"
        className="button button--small"
        onClick={onPrevious}
        disabled={page <= 0}
        aria-label="Previous page"
      >
        <span aria-hidden="true">‹</span>
      </button>
      <span className="pager__count" role="status" aria-live="polite">
        {page + 1} / {pages}
      </span>
      <button
        type="button"
        className="button button--small"
        onClick={onNext}
        disabled={page >= pages - 1}
        aria-label="Next page"
      >
        <span aria-hidden="true">›</span>
      </button>
    </div>
  )
}
