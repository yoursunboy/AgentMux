/**
 * The "more" menu a panel header opens.
 *
 * # Why a menu and not more buttons
 *
 * A grid panel is a terminal with a header, and the header earns its height by
 * staying small. The actions that are not Focus are the ones a person needs
 * occasionally - stop the runtime, move the panel, look at where it lives - and
 * putting them all in the header would take the space the terminal needs. The
 * menu is where "occasionally" belongs.
 *
 * # What it deliberately is not
 *
 * Nothing here is destructive without saying so, and nothing here is destructive
 * without asking. Destroying a runtime - which takes the session and its
 * scrollback with it - is a different act from stopping one, and it is not a
 * menu item that can be hit by accident: it asks first. See
 * `docs/WORKSPACE.md` on why removing a panel from the grid is neither of
 * those.
 */
import { useEffect, useId, useRef, useState, type ReactNode } from 'react'

export interface MenuItem {
  /** A stable key for the list. */
  key: string
  label: string
  onSelect: () => void
  disabled?: boolean
  /** Why it is disabled, shown as the item's tooltip. */
  title?: string
  /** `danger` marks an action that cannot be undone. */
  tone?: 'default' | 'danger'
  /** True to draw a separator above this item. */
  separated?: boolean
}

interface PanelMenuProps {
  /** The accessible name of the button that opens the menu. */
  label: string
  items: MenuItem[]
  disabled?: boolean
}

export function PanelMenu({ label, items, disabled = false }: PanelMenuProps) {
  const [open, setOpen] = useState(false)
  const rootRef = useRef<HTMLDivElement | null>(null)
  const buttonRef = useRef<HTMLButtonElement | null>(null)
  const menuId = useId()

  // A menu that stays open when the page moves is a menu that is now over
  // something else, so everything that could change what is underneath closes
  // it: a click anywhere else, the Escape key, or the panel going away.
  useEffect(() => {
    if (!open) return

    const onPointerDown = (event: MouseEvent | TouchEvent): void => {
      if (rootRef.current?.contains(event.target as Node)) return
      setOpen(false)
    }
    const onKeyDown = (event: KeyboardEvent): void => {
      if (event.key !== 'Escape') return
      setOpen(false)
      // Focus goes back to the button that opened it, so tabbing continues
      // from where the person was rather than from the top of the page.
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

  return (
    <div className="panel-menu" ref={rootRef}>
      <button
        type="button"
        ref={buttonRef}
        className="button button--small panel-menu__button"
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        disabled={disabled}
        onClick={() => setOpen((previous) => !previous)}
      >
        <span aria-hidden="true">⋯</span>
      </button>

      {open && (
        <div className="panel-menu__list" role="menu" id={menuId} aria-label={label}>
          {items.map((item) => (
            <MenuButton key={item.key} item={item} onDone={() => setOpen(false)} />
          ))}
        </div>
      )}
    </div>
  )
}

function MenuButton({ item, onDone }: { item: MenuItem; onDone: () => void }): ReactNode {
  return (
    <>
      {item.separated && <div className="panel-menu__separator" role="separator" />}
      <button
        type="button"
        role="menuitem"
        className={
          item.tone === 'danger'
            ? 'panel-menu__item panel-menu__item--danger'
            : 'panel-menu__item'
        }
        disabled={item.disabled ?? false}
        title={item.title ?? ''}
        onClick={() => {
          onDone()
          item.onSelect()
        }}
      >
        {item.label}
      </button>
    </>
  )
}
