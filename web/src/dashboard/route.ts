/**
 * Which page this is.
 *
 * # Why a path and not a router
 *
 * The application has no router and does not need one for three pages. The
 * server already serves the single-page entry point for any unknown path - which
 * is what makes a deep link work at all - so `App` reading `location.pathname` is
 * the whole of the mechanism.
 *
 * A router library would be a dependency, a history abstraction and a
 * route-matching language, to answer a question a string comparison answers.
 * The workspace having no router is a decision the code has carried since Phase
 * 5; this does not change it, it adds one more value the comparison can take.
 *
 * # Why a full navigation and not a client-side swap
 *
 * A link is what a page load is, and it means the pages share no state to get out
 * of step - a workspace with a half-open terminal and a dashboard that had to be
 * told not to poll would both be consequences of keeping them in one document.
 * The terminal client is above all of them rather than in one of them, so the
 * cost of walking between pages is a page load and not a socket: the first
 * subscribe on the new page reuses the client the old page was already using.
 *
 * The action centre is a third page for the same reason, and it is a page rather
 * than a panel on the console because the console's own bar already has to be
 * counted and read from one response: a selection held in the console's state
 * would be a second thing that page load exists to avoid.
 */

/** The path the console is served at. */
export const DASHBOARD_PATH = '/dashboard'

/** The path the workspace is served at. */
export const WORKSPACE_PATH = '/'

/** The path the action centre is served at. */
export const ACTIONS_PATH = '/actions'

/** Which page a path names. */
export type PageName = 'workspace' | 'dashboard' | 'actions'

/** A page, and what it is about. */
export interface Page {
  name: PageName

  /**
   * The action the page is about, or null when it is about no particular one.
   *
   * Only the action centre ever sets it, and only when the path named an action:
   * `/actions` is the queue and `/actions/act_...` is one of its rows.
   */
  actionId: string | null
}

/**
 * pageFor reads a location path.
 *
 * It is one prefix comparison and one slice, which is what the console chose
 * over a router - see the file comment. Anything this build does not recognise
 * is the workspace, which is what the server serves at `/` and what a deep link
 * into the application has always landed on.
 *
 * A trailing slash is the same address, so `/dashboard/` is the console too. The
 * action centre is checked before the workspace for a reason that is not
 * obvious: every path starts with `/`, so a workspace test that matched a prefix
 * would swallow every other page.
 */
export function pageFor(pathname: string): Page {
  const trimmed = pathname.replace(/\/+$/, '')

  if (trimmed === DASHBOARD_PATH) {
    return { name: 'dashboard', actionId: null }
  }

  if (trimmed === ACTIONS_PATH) {
    return { name: 'actions', actionId: null }
  }

  if (trimmed.startsWith(`${ACTIONS_PATH}/`)) {
    return { name: 'actions', actionId: actionIdOf(trimmed.slice(ACTIONS_PATH.length + 1)) }
  }

  return { name: 'workspace', actionId: null }
}

/**
 * actionIdOf reads the id out of an action-centre path.
 *
 * The shape check is not a security boundary and is not written as one: the
 * server answers 404 for an id it has never held, and a client that skipped the
 * request entirely could not tell that apart from a failure. What it buys is that
 * `/actions/a/b` opens the queue rather than asking the server about a path that
 * cannot be an id - one segment is the whole grammar, and a request that can only
 * 404 is a request not worth making.
 */
function actionIdOf(rest: string): string | null {
  if (rest === '' || rest.includes('/')) return null
  return /^act_[0-9a-f]+$/.test(rest) ? rest : null
}

/**
 * isDashboardPath reports whether a location path names the console.
 *
 * It is kept as a name because `AppRouting` and its test have always asked the
 * question this way. It is expressed in terms of `pageFor` rather than beside it,
 * so there is one comparison in this file and not two.
 */
export function isDashboardPath(pathname: string): boolean {
  return pageFor(pathname).name === 'dashboard'
}

/**
 * actionPath is where one action is read.
 *
 * It is here rather than in the action centre's own files because it is the
 * inverse of `pageFor`'s action branch, and the two have to agree: a link built
 * one way and read the other would be a link that opened the queue.
 */
export function actionPath(id: string): string {
  return `${ACTIONS_PATH}/${id}`
}
