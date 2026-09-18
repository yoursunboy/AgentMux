/**
 * The Prompt Bar: a place to type a message rather than a keystroke at a time.
 *
 * # Why it is not a second channel
 *
 * It sends the same thing a typed keystroke sends, in the same message, to the
 * same terminal. There is no prompt API, no queue, and nothing that could
 * arrive by a different route from the characters around it: what leaves this
 * component is bytes, exactly as if they had been typed. That is deliberate. A
 * "prompt" that travelled separately would be a second way to write to a
 * terminal, and the second way is always the one that turns out not to have the
 * same ordering, the same subscription check, or the same limit as the first.
 *
 * What it does add is that the text is read before it is sent, which is the one
 * thing a terminal cannot do. A newline inside a prompt stays a newline for the
 * program to interpret rather than submitting anything early, because the Enter
 * that submits is the carriage return this component appends - and it is
 * appended once, at the end.
 */
import { useCallback, useMemo, useRef, useState, type ChangeEvent, type KeyboardEvent } from 'react'

import type { TerminalSession } from '../terminal/useTerminal'

/**
 * The largest prompt this will send.
 *
 * Smaller than the protocol's own limit by a long way, and that is the point: a
 * person writing a message to a program is not writing eight kilobytes, so a
 * prompt that reaches this size is a mistake - a paste into the wrong field -
 * and refusing it here says so while the mistake is on screen rather than after
 * it has been typed at a terminal. The server's limit exists to bound what a
 * browser can make the server hold, not to be a target.
 */
export const MAX_PROMPT_BYTES = 8 << 10

const encoder = new TextEncoder()

interface PromptBarProps {
  session: TerminalSession
  /** Whether there is a connected terminal to send to. */
  disabled: boolean
  /**
   * Compact is the grid's shape: one line, no hint row, a send button that
   * fits beside the field.
   *
   * A grid panel is about three hundred pixels tall, and a Prompt Bar that
   * takes three rows of it is a Prompt Bar that costs the terminal more than it
   * gives the person typing. Nothing is lost - the size limit is still enforced
   * and still announced - it is only the layout that is tighter.
   */
  compact?: boolean
}

export function PromptBar({ session, disabled, compact = false }: PromptBarProps) {
  const [text, setText] = useState('')
  const areaRef = useRef<HTMLTextAreaElement | null>(null)

  const bytes = useMemo(() => encoder.encode(text).length, [text])
  const tooLong = bytes > MAX_PROMPT_BYTES
  const canSend = !disabled && !tooLong && text.trim() !== ''

  const send = useCallback(() => {
    const trimmed = text.replace(/\s+$/, '')
    if (disabled || trimmed === '' || encoder.encode(trimmed).length > MAX_PROMPT_BYTES) return
    // The text, then the carriage return that submits it. They are separate
    // messages only because a prompt can be longer than one message may carry;
    // the terminal receives a byte stream and cannot tell the difference.
    session.input(trimmed)
    session.input('\r')
    setText('')
    // Focus stays here. Moving it to the terminal would mean a prompt could not
    // be followed by another prompt without clicking back, and following one
    // prompt with another is what this field is for.
    areaRef.current?.focus()
  }, [disabled, session, text])

  const onChange = useCallback((event: ChangeEvent<HTMLTextAreaElement>) => {
    setText(event.target.value)
  }, [])

  const onKeyDown = useCallback(
    (event: KeyboardEvent<HTMLTextAreaElement>) => {
      if (event.key !== 'Enter') return
      // Shift+Enter is a new line inside the prompt, which is how every message
      // field works and is what makes a multi-line prompt possible at all.
      if (event.shiftKey) return
      event.preventDefault()
      send()
    },
    [send],
  )

  return (
    <div className={compact ? 'prompt-bar prompt-bar--compact' : 'prompt-bar'}>
      <textarea
        ref={areaRef}
        className="prompt-bar__input"
        value={text}
        onChange={onChange}
        onKeyDown={onKeyDown}
        rows={1}
        placeholder={
          disabled ? 'Message Claude… (start the runtime first)' : 'Message Claude… ⏎ sends'
        }
        disabled={disabled}
        aria-label="Message Claude"
        spellCheck={false}
      />
      <div className="prompt-bar__actions">
        {/* Announced rather than silently disabling the button, so that a
            paste into the wrong field explains itself. In the compact layout
            the hint replaces the button rather than sitting beside it: a
            refusal is the more important thing on screen. */}
        {compact && tooLong ? (
          <span className="prompt-bar__hint" aria-live="polite">
            Too long: {bytes} bytes, the limit is {MAX_PROMPT_BYTES}.
          </span>
        ) : (
          <>
            <span className="prompt-bar__hint" aria-live="polite">
              {tooLong ? `Too long: ${bytes} bytes, the limit is ${MAX_PROMPT_BYTES}.` : ''}
            </span>
            <button type="button" className="button" onClick={send} disabled={!canSend}>
              Send
            </button>
          </>
        )}
      </div>
    </div>
  )
}
