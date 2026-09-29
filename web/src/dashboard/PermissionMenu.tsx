import { useCallback, useEffect, useId, useRef, useState } from 'react'

import { setPermissionMode } from '../api/client'
import type { PermissionMode } from '../api/types'
import { describeError } from '../lib/format'

/**
 * How much Claude asks before it acts, as a menu on the project card.
 *
 * # What this is, and what the button beside it is
 *
 * The card has two controls that both mention permission, and they are not two
 * spellings of one thing:
 *
 *	Mode        sends Shift+Tab to the running agent, now
 *	Permission  chooses the mode the *next* launch starts in
 *
 * `ModeSwitch` is the keyboard moved to a screen that has none. This is a
 * setting. The difference is the whole reason this component exists separately:
 * a mode sent as a keystroke changes what the agent on screen is doing, and a
 * mode stored here changes nothing at all until that agent is restarted.
 *
 * # Why it does not restart anything
 *
 * §8 of the phase brief, and the reason is a person's work. A running agent may
 * be in the middle of something; killing its session because somebody changed a
 * preference two rows up would be the console deciding that the preference
 * mattered more than the work. So the mode is stored, the agent is left alone,
 * and the card says in as many words that a restart is what makes it take
 * effect.
 *
 * # Why it does not draw a mode it was not told
 *
 * The mode shown is the server's, from the card's `settings` section, and it is
 * what the *next* launch will get. When the server cannot answer, the section
 * is unavailable and this renders a disabled control with the reason as its
 * tooltip - rather than defaulting to `manual`, which would be this component
 * inventing the one fact it is here to report.
 *
 * # Why the menu marks a choice rather than toggling one
 *
 * The three modes are a choice out of three, so the items are radios: one is
 * always chosen, and the menu opens showing which. A checkbox would say a mode
 * can be off, and there is no off - `manual` is what off would mean, and
 * `manual` is a value.
 */
export interface PermissionMenuProps {
  /** The project whose next launch this configures. */
  projectId: string
  /** What the server says the project is configured with, or "" when it cannot say. */
  mode: string
  /** False when the server has no settings store, or the read failed. */
  available: boolean
  /**
   * Called after the server has accepted a change, so the page can re-read
   * rather than patch its own copy.
   *
   * The console's rule after a write is to ask the server what is true - see
   * `DashboardPage` - and a card that updated itself from the response would be
   * the one place in this client where a local copy of server state lives.
   */
  onChanged?: (() => void) | undefined
}

/**
 * The three modes, in the order the menu lists them.
 *
 * It is written here rather than derived from the server's response, because
 * the response carries the mode in force and not the vocabulary. The labels are
 * the values: `acceptEdits` is spelled the way the CLI spells it, and a
 * friendlier name would be a second vocabulary that could drift from the one
 * `--permission-mode` takes.
 */
const MODES: readonly PermissionMode[] = ['manual', 'acceptEdits', 'bypassPermissions']

/**
 * What each mode means, in the few words a menu item has room for.
 *
 * They are the reason somebody opens this menu, and the reason is not the
 * spelling - `bypassPermissions` says what it does to a person who already
 * knows the CLI, and nothing to anybody else.
 */
const MEANING: Record<PermissionMode, string> = {
  manual: 'ask before every action',
  acceptEdits: 'ask, except about edits',
  bypassPermissions: 'do not ask',
}

export function PermissionMenu({
  projectId,
  mode,
  available,
  onChanged,
}: PermissionMenuProps) {
  const [open, setOpen] = useState(false)
  const [saved, setSaved] = useState(false)
  const [failure, setFailure] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const rootRef = useRef<HTMLDivElement | null>(null)
  const buttonRef = useRef<HTMLButtonElement | null>(null)
  const menuId = useId()

  // A menu that stays open when the page moves is a menu that is now over
  // something else, so everything that could change what is underneath closes
  // it: a click anywhere else, the Escape key, or the card going away. It is
  // the same three rules `components/PanelMenu` follows.
  useEffect(() => {
    if (!open) return

    const onPointerDown = (event: MouseEvent | TouchEvent): void => {
      if (rootRef.current?.contains(event.target as Node)) return
      setOpen(false)
    }
    const onKeyDown = (event: KeyboardEvent): void => {
      if (event.key !== 'Escape') return
      setOpen(false)
      // Focus goes back to the button that opened it, so tabbing continues from
      // where the person was rather than from the top of the page.
      buttonRef.current?.focus()
    }

    document.addEventListener('mousedown', onPointerDown)
    document.addEventListener('touchstart', onPointerDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onPointerDown)
      document.removeEventListener('touchstart', onPointerDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [open])

  // A card that unmounts mid-save - because a poll dropped the project, or the
  // page navigated - must not set state on a component that is gone.
  const live = useRef(true)
  useEffect(() => {
    live.current = true
    return () => {
      live.current = false
    }
  }, [])

  const choose = useCallback(
    async (next: PermissionMode) => {
      setOpen(false)
      if (next === mode) return

      setBusy(true)
      setFailure(null)
      setSaved(false)
      try {
        await setPermissionMode(projectId, next)
        if (!live.current) return
        // The message is the point, not a flourish: the write changed a
        // preference and nothing else, and a person who expected the agent to
        // stop asking would otherwise conclude the button does not work.
        setSaved(true)
        onChanged?.()
      } catch (cause) {
        if (!live.current) return
        setFailure(describeError(cause))
      } finally {
        if (live.current) setBusy(false)
      }
    },
    [projectId, mode, onChanged],
  )

  const label = available ? 'Permission' : 'Permission (unavailable)'

  return (
    <div className="permission-menu" ref={rootRef}>
      <button
        type="button"
        ref={buttonRef}
        className="button button--small permission-menu__button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        disabled={!available || busy}
        title={
          available
            ? `Claude permission mode for the next launch: ${mode}`
            : 'The server cannot report this project’s settings.'
        }
        onClick={() => {
          // Opening the menu puts away whatever the last use of it said. The
          // message describes one save, and leaving it up over a menu somebody
          // has reopened would read as a statement about the mode now shown.
          setSaved(false)
          setFailure(null)
          setOpen((previous) => !previous)
        }}
      >
        {busy ? 'Saving…' : label}
      </button>

      {open && (
        <div className="permission-menu__list" role="menu" id={menuId} aria-label="Claude Permission Mode">
          <p className="permission-menu__heading" aria-hidden="true">
            Claude Permission Mode
          </p>
          {MODES.map((candidate) => {
            const chosen = candidate === mode
            return (
              <button
                key={candidate}
                type="button"
                role="menuitemradio"
                aria-checked={chosen}
                className={
                  chosen
                    ? 'permission-menu__item permission-menu__item--chosen'
                    : 'permission-menu__item'
                }
                onClick={() => void choose(candidate)}
              >
                {/* The mark is decoration; `aria-checked` is what says which one
                    is chosen, so a screen reader is told once rather than twice. */}
                <span className="permission-menu__mark" aria-hidden="true">
                  {chosen ? '●' : '○'}
                </span>
                <span className="permission-menu__name">{candidate}</span>
                <span className="permission-menu__meaning">{MEANING[candidate]}</span>
              </button>
            )
          })}
        </div>
      )}

      {/* One live region for both outcomes, so a person using a screen reader is
          told what happened rather than being left with a menu that closed. */}
      <p className="permission-menu__status" role="status">
        {failure !== null ? (
          <span className="permission-menu__status--failed">{failure}</span>
        ) : saved ? (
          // §8: an existing Claude session is not terminated to apply a
          // preference, so the card says what would apply it instead.
          <span className="permission-menu__status--saved">
            Permission mode updated. Restart Agent to apply.
          </span>
        ) : null}
      </p>
    </div>
  )
}
