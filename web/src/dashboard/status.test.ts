import { describe, expect, it } from 'vitest'

import {
  absentStyle,
  agentStyle,
  attentionStyle,
  runtimeStyle,
  unavailableStyle,
} from './status'

describe('the status mappings', () => {
  it('reads the runtime vocabulary the project model uses', () => {
    expect(runtimeStyle('running').tone).toBe('positive')
    expect(runtimeStyle('stopped').tone).toBe('neutral')
    expect(runtimeStyle('starting').tone).toBe('attention')
    expect(runtimeStyle('error').tone).toBe('danger')
  })

  it('reads the agent vocabulary the state projection uses', () => {
    expect(agentStyle('RUNNING').tone).toBe('positive')
    expect(agentStyle('WAITING_PERMISSION').tone).toBe('attention')
    expect(agentStyle('WAITING_INPUT').tone).toBe('attention')
    expect(agentStyle('FAILED').tone).toBe('danger')
    expect(agentStyle('STOPPED').tone).toBe('neutral')
  })

  // The one level that means nothing progresses without a person reads stronger
  // than a warning rather than weaker, which is the whole reason the two are
  // different levels.
  it('makes ACTION_REQUIRED read at least as strongly as a warning', () => {
    expect(attentionStyle('ACTION_REQUIRED').tone).toBe('attention')
    expect(attentionStyle('WARNING').tone).toBe('warning')
    expect(attentionStyle('NONE').tone).toBe('neutral')
  })

  it('gives every known value a label and an explanation', () => {
    const known = [
      runtimeStyle('running'),
      agentStyle('RUNNING'),
      attentionStyle('ACTION_REQUIRED'),
    ]
    for (const style of known) {
      expect(style.label).not.toBe('')
      expect(style.detail).not.toBe('')
    }
  })

  // A value this build does not know is shown rather than hidden: a dashboard
  // that blanked an unexpected status would report "fine" about a server it
  // could not read.
  it('shows a value it does not recognise instead of hiding it', () => {
    const style = agentStyle('SUMMONING')
    expect(style.label).toBe('SUMMONING')
    expect(style.tone).toBe('neutral')
    expect(style.detail).toContain('SUMMONING')
  })

  it('says Unknown for a value that is empty', () => {
    expect(agentStyle('').label).toBe('Unknown')
    expect(attentionStyle('   ').label).toBe('Unknown')
  })

  it('distinguishes a section that is unavailable from one that is empty', () => {
    expect(unavailableStyle('agent state projection').label).toBe('unavailable')
    expect(absentStyle('agent').label).toBe('none')
    // They mean different things and a person would do different things next,
    // so the text differs even though both are neutral.
    expect(unavailableStyle('x').detail).not.toBe(absentStyle('x').detail)
  })

  // The two project vocabularies must not be read by the same function: the
  // runtime says "running" and the agent says "RUNNING", and a mapping that
  // accepted either for both would be a mapping that could not tell them apart.
  it('keeps the three vocabularies apart', () => {
    expect(runtimeStyle('RUNNING').label).toBe('RUNNING')
    expect(agentStyle('running').label).toBe('running')
    expect(attentionStyle('running').label).toBe('running')
  })
})
