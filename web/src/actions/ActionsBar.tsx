import { DASHBOARD_PATH, WORKSPACE_PATH } from '../dashboard/route'

/**
 * The action centre's own top bar.
 *
 * # Why it is not the console's bar
 *
 * It is a different page with a different question. The console's bar reports
 * the server's identity, whether a terminal can run here and which model is
 * bound - none of which is what this page is about - and it reads that from the
 * dashboard response. Reusing it would mean this page making a `GET
 * /api/controller` request to draw a header, which is the second request
 * `DashboardPage`'s own doc forbids, for a page that already knows how much is
 * waiting because it just listed it.
 *
 * So the two counts here come from the queue response itself, and the classes
 * are this bar's own. A shared bar would have to be told which page it was on,
 * which is the thing the two pages exist to avoid.
 */
export interface ActionsBarProps {
  /** Pending actions that nothing progresses without. */
  needsYou: number
  /** Pending actions that are worth reading and stop nothing. */
  notices: number
}

export function ActionsBar({ needsYou, notices }: ActionsBarProps) {
  return (
    <header className="actions-bar" role="banner">
      <span className="actions-bar__title">Actions</span>

      <span className="actions-bar__divider" aria-hidden="true">
        |
      </span>

      <span className="actions-bar__item">
        Needs you:{' '}
        <span
          className={
            needsYou > 0
              ? 'actions-bar__number actions-bar__number--needs-you'
              : 'actions-bar__number'
          }
        >
          {needsYou}
        </span>
      </span>

      <span className="actions-bar__divider" aria-hidden="true">
        |
      </span>

      <span className="actions-bar__item">
        Notices: <span className="actions-bar__number">{notices}</span>
      </span>

      <div className="actions-bar__spacer" />

      <a className="actions-bar__link" href={DASHBOARD_PATH}>
        Dashboard
      </a>

      <a className="actions-bar__link" href={WORKSPACE_PATH}>
        Workspace
      </a>
    </header>
  )
}
