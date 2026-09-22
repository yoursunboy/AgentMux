import { ActionCenter } from './ActionCenter'
import { ActionDetail } from './ActionDetail'

/**
 * The action centre, which is either the queue or one of its rows.
 *
 * # Why the choice is here
 *
 * `App` reads the path and hands this page the id, and this component decides
 * what that means. The alternative - `App` choosing between `ActionCenter` and
 * `ActionDetail` itself - would put two more components into the one place that
 * exists to answer "which page is this", and that question already has a file.
 */
export interface ActionsPageProps {
  /** The action to read, or null for the queue. */
  actionId: string | null

  /**
   * How often to re-read, in milliseconds. Omitted means the real interval.
   *
   * It is passed through to whichever half is rendered, so a test can assert
   * what one read produces without waiting five seconds.
   */
  pollMs?: number | undefined
}

export function ActionsPage({ actionId, pollMs }: ActionsPageProps) {
  if (actionId === null) {
    return <ActionCenter pollMs={pollMs} />
  }
  return <ActionDetail id={actionId} pollMs={pollMs} />
}
