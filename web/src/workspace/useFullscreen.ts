/**
 * Full screen, as a layout mode rather than a second kind of terminal.
 *
 * It is the browser's own full screen on the document, and it is the last of
 * the three display modes: grid, focus, full screen. All three draw the same
 * component around the same subscription, so moving between them cannot restart
 * a runtime, re-create a session, or lose what a program is doing - which is
 * the property `docs/UI_SPEC.md` §7 states as "changing modes never restarts
 * the session".
 *
 * It is a hook rather than a call at the point of use because the browser can
 * leave full screen without asking - the Escape key, a system gesture, another
 * window taking over - and a UI that believed it was still full screen would
 * draw a mode the browser is not in.
 */
import { useCallback, useEffect, useState } from 'react'

export interface Fullscreen {
  /** Whether this browser can do it at all. */
  supported: boolean
  /** Whether it is doing it right now. */
  active: boolean
  /** toggle enters or leaves full screen. */
  toggle(): void
}

export function useFullscreen(): Fullscreen {
  const supported =
    typeof document !== 'undefined' && typeof document.documentElement?.requestFullscreen === 'function'
  const [active, setActive] = useState<boolean>(
    () => typeof document !== 'undefined' && document.fullscreenElement !== null,
  )

  useEffect(() => {
    if (!supported) return
    const onChange = (): void => setActive(document.fullscreenElement !== null)
    document.addEventListener('fullscreenchange', onChange)
    return () => document.removeEventListener('fullscreenchange', onChange)
  }, [supported])

  const toggle = useCallback(() => {
    if (!supported) return
    // Both calls return a promise that rejects when the browser refuses - a
    // gesture it did not accept, a permission policy, an embedded frame. There
    // is nothing to repair and nothing to report: the mode simply does not
    // change, and `fullscreenchange` is what decides what is drawn.
    if (document.fullscreenElement === null) {
      void document.documentElement.requestFullscreen().catch(() => {})
    } else {
      void document.exitFullscreen().catch(() => {})
    }
  }, [supported])

  return { supported, active, toggle }
}
