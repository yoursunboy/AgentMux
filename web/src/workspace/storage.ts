/**
 * The one thing the workspace keeps in the browser: which page was open.
 *
 * # What is not here, and why
 *
 * Not the terminal's contents, not the Prompt Bar's text, not anything typed or
 * printed - `docs/TERMINAL.md` §13 and §14 say why, and Phase 4 was built to
 * keep that true.
 *
 * Not which projects are in the workspace either, which is the less obvious
 * answer. Membership is "this project has a reserved slot", and that lives in
 * the server's database, so a workspace survives a reload, a second browser and
 * a server restart with no second copy to disagree with the first. A list here
 * would be a cache of a server fact, and the first time it went stale the
 * workspace would either lose a project or invent one.
 *
 * What is left is genuinely a property of this browser: which page somebody was
 * looking at. That is a preference, it is worth nothing to anybody else, and it
 * is deliberately the only thing written here.
 */

/** The storage key, namespaced so it cannot collide with anything else. */
export const STORAGE_KEY = 'agentmux.workspace.page.v1'

/** The largest page number worth remembering.
 *
 * A workspace with more than this many pages is not a thing; the bound is here
 * so that a corrupted or hand-edited value cannot be turned into a page index
 * the UI has to reason about.
 */
const MAX_PAGE = 999

/**
 * readPage returns the remembered page, or 0.
 *
 * Storage can throw rather than return - a private window, blocked site data,
 * a quota that has been exhausted - and it can hold anything at all, including
 * values another version of the app wrote. Every one of those is answered the
 * same way, with the first page, because the alternative is a workspace that
 * refuses to open over a preference.
 */
export function readPage(): number {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY)
    if (raw === null) return 0
    // A canonical non-negative integer or nothing. Number.parseInt would take
    // "3 pages" and "1e9" and call them 3 and 1, which is a stored value being
    // interpreted rather than read.
    if (!/^\d{1,4}$/.test(raw)) return 0
    const page = Number(raw)
    if (page > MAX_PAGE) return 0
    return page
  } catch {
    return 0
  }
}

/** writePage remembers the page. Failure is silent, and it is not a failure. */
export function writePage(page: number): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, String(Math.max(0, Math.trunc(page))))
  } catch {
    // A browser that will not store a page number is a browser that opens on
    // the first page next time. Nothing about the workspace depends on this.
  }
}

/** forgetPage clears the preference. It exists for tests and for a reset. */
export function forgetPage(): void {
  try {
    window.localStorage.removeItem(STORAGE_KEY)
  } catch {
    // Nothing to do: there is no stored value either way.
  }
}
