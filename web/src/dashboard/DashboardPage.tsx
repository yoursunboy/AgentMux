import { describeError } from '../lib/format'
import { ProjectGrid } from './ProjectGrid'
import { ServerBar } from './ServerBar'
import { useDashboard } from './useDashboard'
import { useDashboardColumns } from './useDashboardColumns'
import { WORKSPACE_PATH } from './route'

/**
 * The console: one page, one request, and the projects that need somebody at
 * the top.
 *
 * # One request, and no joining
 *
 * It reads `GET /api/controller` and nothing else. Every runtime, agent,
 * attention and action value on the page arrives inside that one response,
 * already joined and already sorted. Calling the four per-project endpoints and
 * combining them here would be this page reimplementing the read model the
 * server already exposes - and §3 of the phase brief is explicit that it must
 * not.
 *
 * # The three states, and the fourth
 *
 *	no data, loading         Loading controller…
 *	no data, failed          Unable to load dashboard, with a retry
 *	data                     the console
 *	data, and a poll failed  the console, and a note that it is not current
 *
 * The fourth is the one worth having. A dashboard that blanked itself because
 * one refresh timed out would be unreadable in exactly the conditions somebody
 * needs it - and the data it was already showing was true thirty seconds ago
 * and is very likely true now.
 */
export interface DashboardPageProps {
  /**
   * How often to re-read, in milliseconds. Omitted means the real interval.
   *
   * It is a parameter because the polling is a behaviour worth testing and five
   * seconds is not a length a test should wait. A caller that is not a test has
   * no reason to pass it.
   */
  pollMs?: number
}

export function DashboardPage({ pollMs }: DashboardPageProps = {}) {
  const { data, error, loading, reload } = useDashboard(pollMs)
  const columns = useDashboardColumns()

  return (
    <div className="dashboard">
      <h1 className="visually-hidden">AgentMux console</h1>

      {data === null ? (
        <div className="dashboard__placeholder">
          {loading ? (
            <p className="dashboard__message" role="status">
              Loading controller…
            </p>
          ) : (
            <>
              <p className="dashboard__message dashboard__message--failed" role="alert">
                Unable to load dashboard
              </p>
              <p className="dashboard__detail">{describeError(error)}</p>
              <div className="dashboard__actions">
                <button type="button" className="dashboard__retry" onClick={reload}>
                  Retry
                </button>
                <a className="dashboard__link" href={WORKSPACE_PATH}>
                  Back to the workspace
                </a>
              </div>
            </>
          )}
        </div>
      ) : (
        <>
          <ServerBar server={data.server} queue={data.queue} />

          {error !== null && (
            <p className="dashboard__stale" role="status">
              Not current - the last refresh failed. {describeError(error)}
              <button type="button" className="dashboard__retry" onClick={reload}>
                Retry
              </button>
            </p>
          )}

          <main className="dashboard__body">
            <ProjectGrid cards={data.projects} columns={columns} />
          </main>
        </>
      )}
    </div>
  )
}
