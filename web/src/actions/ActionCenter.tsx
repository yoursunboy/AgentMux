import { describeError } from '../lib/format'
import { ActionCard } from './ActionCard'
import { ActionsBar } from './ActionsBar'
import { groupActions } from './actionStyle'
import { useActions } from './useActions'

/**
 * The action centre: everything waiting, across every project.
 *
 * # What the number at the top means
 *
 * The bar shows the server's own counts, and they are counts of what is
 * *pending* - not the length of the list below them. A settled action is still
 * listed, because the queue is a record as well as a to-do list, and it is in
 * neither count, because a thing that was dealt with needs nobody. The two
 * numbers and the three sections are therefore allowed to disagree, and they are
 * meant to: "1 action pending" over two rows is the page saying that one of them
 * still wants you and the other one is history.
 *
 * # Why "needs you" is not simply everything urgent
 *
 * `ACTION_REQUIRED` is the only level that means nothing progresses without a
 * person, and it is the only level a permission request produces. Everything
 * else - a failed attempt, a finished one - is worth reading and blocks nothing,
 * so it is counted separately. Putting them in one list would make the headline
 * only ever grow, because nothing resolves a failure: one action per attempt,
 * for the life of the project.
 *
 * # The three states, and the fourth
 *
 *	no data, loading         Loading actions…
 *	no data, failed          Unable to load actions, with a retry
 *	data                     the queue
 *	data, and a poll failed  the queue, and a note that it is not current
 *
 * The fourth is the one worth having, for the reason the console gives: a queue
 * that blanked itself because one refresh timed out would be unreadable in
 * exactly the conditions somebody needs it.
 */
export interface ActionCenterProps {
  /**
   * How often to re-read, in milliseconds. Omitted means the real interval.
   *
   * It is a parameter because polling is a behaviour worth testing and five
   * seconds is not a length a test should wait. A caller that is not a test has
   * no reason to pass it.
   *
   * It is `number | undefined` rather than merely optional so that a page
   * passing its own prop straight through can pass an absent one: this project
   * compiles with `exactOptionalPropertyTypes`, where an optional property
   * without `undefined` in its type refuses an explicit `undefined`.
   */
  pollMs?: number | undefined
}

export function ActionCenter({ pollMs }: ActionCenterProps = {}) {
  const { data, error, loading, reload } = useActions(pollMs)

  if (data === null) {
    return (
      <div className="action-center__placeholder">
        {loading ? (
          <p className="action-center__message" role="status">
            Loading actions…
          </p>
        ) : (
          <>
            <p className="action-center__message action-center__message--failed" role="alert">
              Unable to load actions
            </p>
            <p className="action-center__error">{describeError(error)}</p>
            <button type="button" className="action-center__retry" onClick={reload}>
              Retry
            </button>
          </>
        )}
      </div>
    )
  }

  const groups = groupActions(data.actions)
  const nothingWaiting = data.needsYou === 0 && data.notices === 0

  return (
    <div className="action-center">
      <h1 className="visually-hidden">AgentMux actions</h1>

      <ActionsBar needsYou={data.needsYou} notices={data.notices} />

      {error !== null && (
        <p className="action-center__stale" role="status">
          Not current - the last refresh failed. {describeError(error)}
          <button type="button" className="action-center__retry" onClick={reload}>
            Retry
          </button>
        </p>
      )}

      <main className="action-center__body">
        {nothingWaiting && (
          <p className="action-center__empty" role="status">
            Nothing is waiting.
          </p>
        )}

        {groups.needsYou.length > 0 && (
          <section className="action-section" aria-label="Needs you">
            <h2 className="action-section__heading">
              Needs you
              <span className="action-section__count">{data.needsYou}</span>
            </h2>
            <div className="action-section__rows">
              {groups.needsYou.map((action) => (
                <ActionCard key={action.id} action={action} />
              ))}
            </div>
          </section>
        )}

        {groups.notices.length > 0 && (
          <section className="action-section" aria-label="Notices">
            <h2 className="action-section__heading">
              Notices
              <span className="action-section__count">{data.notices}</span>
            </h2>
            <div className="action-section__rows">
              {groups.notices.map((action) => (
                <ActionCard key={action.id} action={action} />
              ))}
            </div>
          </section>
        )}

        {groups.settled.length > 0 && (
          // No count on this heading, deliberately. Every other number on this
          // page is something a person can act on; a count of history is not,
          // and a number nobody acts on is a number that only invites the
          // question of why it is there.
          <section className="action-section action-section--settled" aria-label="Settled">
            <h2 className="action-section__heading">Settled</h2>
            <div className="action-section__rows">
              {groups.settled.map((action) => (
                <ActionCard key={action.id} action={action} />
              ))}
            </div>
          </section>
        )}
      </main>
    </div>
  )
}
