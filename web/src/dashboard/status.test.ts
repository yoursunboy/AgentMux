import { describe, expect, it } from 'vitest'

import {
  absentStyle,
  agentIsRunning,
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

  // Which agent states have a process behind them, which is the question the
  // lifecycle buttons are drawn from. It is the agent's vocabulary and not the
  // runtime's: "running" is a terminal that is up, "RUNNING" is Claude working
  // in one, and the two are different layers saying different true things.
  it('says which agent states have a process to stop', () => {
    expect(agentIsRunning('RUNNING')).toBe(true)
    expect(agentIsRunning('WAITING_INPUT')).toBe(true)
    expect(agentIsRunning('WAITING_PERMISSION')).toBe(true)

    // CREATED is an attempt that was recorded and never launched; the rest are
    // attempts that are over.
    expect(agentIsRunning('CREATED')).toBe(false)
    expect(agentIsRunning('COMPLETED')).toBe(false)
    expect(agentIsRunning('FAILED')).toBe(false)
    expect(agentIsRunning('STOPPED')).toBe(false)

    // A project nothing has ever run in, and a status from a newer build. Both
    // read as "not running", which draws Start - and Start is the safe half of
    // being wrong, because the server adopts a process that is already there
    // rather than launching a second one beside it.
    expect(agentIsRunning(undefined)).toBe(false)
    expect(agentIsRunning('SUMMONING')).toBe(false)
  })
})
