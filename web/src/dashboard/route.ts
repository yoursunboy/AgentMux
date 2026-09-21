/**
 * Which page this is.
 *
 * # Why a path and not a router
 *
 * The application has no router and does not need one for two pages. The server
 * already serves the single-page entry point for any unknown path - which is
 * what makes a deep link work at all - so `App` reading `location.pathname` is
 * the whole of the mechanism.
 *
 * A router library would be a dependency, a history abstraction and a
 * route-matching language, to answer a question a string comparison answers.
 * The workspace having no router is a decision the code has carried since Phase
 * 5; this does not change it, it adds one more value the comparison can take.
 *
 * # Why a full navigation and not a client-side swap
 *
 * The console has no terminal in it, so leaving the workspace to open it costs
 * a socket that will be reopened on the way back. That is a page load, which is
 * what a link does, and it means the two pages share no state to get out of
 * step - a workspace with a half-open terminal and a dashboard that had to be
 * told not to poll would both be consequences of keeping them in one document.
 */

/** The path the console is served at. */
export const DASHBOARD_PATH = '/dashboard'

/** The path the workspace is served at. */
export const WORKSPACE_PATH = '/'

/**
 * isDashboardPath reports whether a location path names the console.
 *
 * A trailing slash is the same address, so `/dashboard/` is the console too.
 * Anything else - including a path this build does not know - is the workspace,
 * which is what the server serves at `/` and what a deep link into the
 * application has always landed on.
 */
export function isDashboardPath(pathname: string): boolean {
  const trimmed = pathname.replace(/\/+$/, '')
  return trimmed === DASHBOARD_PATH
}
