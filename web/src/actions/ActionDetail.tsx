import { describeError, formatRelativeTime } from '../lib/format'
import { ACTIONS_PATH, WORKSPACE_PATH } from '../dashboard/route'
import { StatusBadge } from '../dashboard/StatusBadge'
import { actionStyle, actionTypeLabel } from './actionStyle'
import { useAction } from './useActions'

/**
 * One action, in full.
 *
 * # Why it is a page and not a panel
 *
 * `/actions/{id}` is an address. A panel would put the queue and its selection
 * in one component's state, and the console gave up that kind of coupling when it
 * chose a page load over a client-side swap - `dashboard/route.ts` is the long
 * form. A dialog would be a modal for a decision, and reading an action is not a
 * decision: the one decision there is gets refused by design, below.
 *
 * # Why there is no button on it
 *
 * There is no Allow, no Deny, no Acknowledge and no Resolve. §6 of the phase
 * brief asks for the permission request to be *visible*, and the endpoint that
 * would answer one does not exist - `internal/httpapi/attention.go` says why in
 * its own words, and `TestAnActionCannotBeAnswered` is the assertion that every
 * method against every path refuses.
 *
 * The consequence is stated on the page rather than left for a person to work
 * out. An action centre with no answer on it and no explanation would read as a
 * page that was broken, and the honest reading is the opposite: this is an
 * observer of a session, and the session is where the answer is given.
 */
export interface ActionDetailProps {
  /** The action to read. */
  id: string

  /**
   * How often to re-read, in milliseconds. Omitted means the real interval.
   *
   * It is a parameter because polling is a behaviour worth testing and five
   * seconds is not a length a test should wait. `number | undefined` for the
   * reason `ActionCenterProps` gives.
   */
  pollMs?: number | undefined
}

export function ActionDetail({ id, pollMs }: ActionDetailProps) {
  const { data, error, loading, reload } = useAction(id, pollMs)

  if (data === null) {
    return (
      <div className="action-detail__placeholder">
        {loading ? (
          <p className="action-detail__message" role="status">
            Loading action…
          </p>
        ) : (
          <>
            <p className="action-detail__message action-detail__message--failed" role="alert">
              Unable to load this action
            </p>
            <p className="action-detail__error">{describeError(error)}</p>
            <div className="action-detail__controls">
              <button type="button" className="action-detail__retry" onClick={reload}>
                Retry
              </button>
              <a className="action-detail__link" href={ACTIONS_PATH}>
                Back to all actions
              </a>
            </div>
          </>
        )}
      </div>
    )
  }

  const style = actionStyle(data.level, data.status)

  return (
    <article className="action-detail">
      <p className="action-detail__back">
        <a className="action-detail__link" href={ACTIONS_PATH}>
          {'←'} All actions
        </a>
      </p>

      {error !== null && (
        <p className="action-detail__stale" role="status">
          Not current - the last refresh failed. {describeError(error)}
        </p>
      )}

      <h1 className="action-detail__type">{actionTypeLabel(data.type)}</h1>

      {/* Seven labelled rows and nothing else. The labels are exactly these, in
          this order, and a test asserts the set - which is §16's rule made
          checkable rather than remembered. */}
      <dl className="action-detail__fields">
        <dt className="action-detail__term">Project</dt>
        <dd className="action-detail__value" title={data.projectId}>
          {data.projectName.trim() === '' ? data.projectId : data.projectName}
        </dd>

        <dt className="action-detail__term">Session</dt>
        <dd className="action-detail__value">{data.agentSessionId}</dd>

        <dt className="action-detail__term">Type</dt>
        <dd className="action-detail__value">{data.type}</dd>

        <dt className="action-detail__term">Status</dt>
        <dd className="action-detail__value">
          <StatusBadge status={style} />
        </dd>

        <dt className="action-detail__term">Reason</dt>
        <dd className="action-detail__value">{data.reason}</dd>

        <dt className="action-detail__term">Created</dt>
        <dd className="action-detail__value">{formatRelativeTime(data.createdAt)}</dd>

        <dt className="action-detail__term">Resolved</dt>
        <dd className="action-detail__value">
          {data.resolvedAt === null ? '-' : formatRelativeTime(data.resolvedAt)}
        </dd>
      </dl>

      <p className="action-detail__note">
        AgentMux shows you what is waiting; it does not answer it for you. The answer is given at
        the terminal.
      </p>

      {/* §7 of the phase brief asks for this link and it lands on the workspace
          root rather than on the project, because the workspace has no
          URL-addressed project: it restores its layout from localStorage, and
          nothing in a path or a query selects one. A link that carried an id
          nothing read would be a link that appeared to work. */}
      <a className="action-detail__project-link" href={WORKSPACE_PATH}>
        Open project
      </a>
    </article>
  )
}
