/**
 * The Prompt Bar.
 *
 * What matters about it is that it is not a second channel: what it sends is the
 * bytes a keystroke would send, in the same message, to the same terminal. The
 * tests below are mostly about the two things it does that a keystroke cannot -
 * reading the text before it is sent, and appending exactly one carriage return.
 */
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { MAX_PROMPT_BYTES, PromptBar } from './PromptBar'
import { makeSession } from '../test/terminal'

function renderBar(options: { disabled?: boolean; text?: string } = {}) {
  const double = makeSession()
  const view = render(<PromptBar session={double.session} disabled={options.disabled ?? false} />)
  return { ...double, ...view }
}

const field = () => screen.getByLabelText('Message Claude')
const sendButton = () => screen.getByRole('button', { name: 'Send' })

describe('PromptBar', () => {
  it('sends the text and one carriage return', async () => {
    // Two calls, not one: the text and the Enter that submits it. They are
    // separate messages only because a prompt can be longer than one message may
    // carry, and the terminal receives a byte stream either way.
    const { session } = renderBar()

    await userEvent.type(field(), 'explain this file')
    await userEvent.click(sendButton())

    expect(session.input).toHaveBeenNthCalledWith(1, 'explain this file')
    expect(session.input).toHaveBeenNthCalledWith(2, '\r')
    expect(session.input).toHaveBeenCalledTimes(2)
  })

  it('submits on Enter rather than inserting a newline', async () => {
    const { session } = renderBar()

    await userEvent.type(field(), 'hello{Enter}')

    expect(session.input).toHaveBeenNthCalledWith(1, 'hello')
    expect(session.input).toHaveBeenNthCalledWith(2, '\r')
  })

  it('keeps Shift+Enter as a newline inside the prompt', async () => {
    // Which is what makes a multi-line prompt possible at all, and the reason
    // the text is read before it is sent.
    const { session } = renderBar()

    await userEvent.type(field(), 'first{Shift>}{Enter}{/Shift}second')
    expect(session.input).not.toHaveBeenCalled()

    await userEvent.click(sendButton())

    expect(session.input).toHaveBeenNthCalledWith(1, 'first\nsecond')
    expect(session.input).toHaveBeenNthCalledWith(2, '\r')
  })

  it('clears the field after sending, and keeps the focus there', async () => {
    // Focus staying is what makes one prompt followable by another without
    // clicking back into the field.
    renderBar()

    await userEvent.type(field(), 'first{Enter}')

    expect(field()).toHaveValue('')
    expect(field()).toHaveFocus()
  })

  it('refuses to send whitespace', async () => {
    const { session } = renderBar()

    await userEvent.type(field(), '   {Enter}')

    expect(session.input).not.toHaveBeenCalled()
  })

  it('trims the trailing newline a paste can bring with it', async () => {
    // Otherwise the newline would submit, and the carriage return this component
    // appends would submit an empty line after it.
    const { session } = renderBar()

    await userEvent.type(field(), 'done')
    await userEvent.click(sendButton())

    expect(session.input).toHaveBeenNthCalledWith(1, 'done')
    expect(session.input).toHaveBeenNthCalledWith(2, '\r')
  })

  it('is disabled until there is a connected terminal to write to', () => {
    // Accepting text that would go nowhere is worse than refusing it: the person
    // would watch it disappear.
    renderBar({ disabled: true })

    expect(field()).toBeDisabled()
    expect(sendButton()).toBeDisabled()
  })

  it('says so rather than silently refusing when the text is too long', async () => {
    // A prompt that reaches the limit is a paste into the wrong field, and
    // saying so while the mistake is on screen is the whole point of the limit.
    renderBar()

    const oversized = 'x'.repeat(MAX_PROMPT_BYTES + 1)
    // Typing 8 KiB one character at a time is slow; the field takes a paste.
    await userEvent.click(field())
    await userEvent.paste(oversized)

    expect(screen.getByText(new RegExp(`the limit is ${MAX_PROMPT_BYTES}`))).toBeInTheDocument()
    expect(sendButton()).toBeDisabled()
  })

  it('counts bytes rather than characters, because the limit is bytes', async () => {
    // A multi-byte character counts for more than one, and a limit that counted
    // characters would let a paste of them through and be refused on the wire.
    renderBar()

    // Half as many characters, because each is two bytes: this is exactly the
    // limit.
    await userEvent.click(field())
    await userEvent.paste('é'.repeat(MAX_PROMPT_BYTES / 2))
    expect(sendButton()).toBeEnabled()

    // One more character is one byte past it, which the same number of ASCII
    // characters would not be.
    await userEvent.paste('é')
    expect(sendButton()).toBeDisabled()
  })

  it('sends nothing when the button is clicked with an empty field', async () => {
    const { session } = renderBar()

    expect(sendButton()).toBeDisabled()
    await userEvent.click(sendButton())

    expect(session.input).not.toHaveBeenCalled()
  })

  it('does not submit an empty prompt on Enter', async () => {
    const { session } = renderBar()

    await userEvent.type(field(), '{Enter}')

    expect(session.input).not.toHaveBeenCalled()
  })

  it('does not send a paste it has already refused for being too long', async () => {
    // The button is disabled, but Enter is still a key. The guard has to be
    // where the sending happens, not only where the button is drawn.
    const { session } = renderBar()

    await userEvent.click(field())
    await userEvent.paste('x'.repeat(MAX_PROMPT_BYTES + 1))
    await userEvent.keyboard('{Enter}')

    expect(session.input).not.toHaveBeenCalled()
  })

  it('names the control for anyone not looking at the placeholder', () => {
    renderBar()

    // The placeholder changes with the connection, so the accessible name must
    // not: a label that changed would be a control whose name depends on state.
    expect(screen.getByLabelText('Message Claude')).toBeInTheDocument()
  })
})
