import { describe, expect, it } from 'vitest'

import { makeAction, makeResolvedAction } from '../test/actions'
import {
  actionGroup,
  actionStyle,
  actionTypeLabel,
  groupActions,
  pendingSentence,
} from './actionStyle'

describe('actionGroup', () => {
  // §9's three states, and the one rule that decides between them: a settled
  // action is in neither live list, whatever it used to need.
  it('puts a pending ACTION_REQUIRED under what needs somebody', () => {
    expect(actionGroup('ACTION_REQUIRED', 'PENDING')).toBe('needs-you')
  })

  it('puts everything else pending under notices', () => {
    for (const level of ['WARNING', 'INFO', 'NONE', 'SOMETHING_NEW', '']) {
      expect(actionGroup(level, 'PENDING')).toBe('notices')
    }
  })

  // The one that would be easy to get wrong: a permission request that was
  // answered still carries ACTION_REQUIRED, and showing it under "Needs you"
  // would send somebody to look at something already done.
  it('takes a settled permission request out of what needs somebody', () => {
    expect(actionGroup('ACTION_REQUIRED', 'RESOLVED')).toBe('settled')
    expect(actionGroup('ACTION_REQUIRED', 'EXPIRED')).toBe('settled')
    expect(actionGroup('ACTION_REQUIRED', 'SOMETHING_NEW')).toBe('settled')
  })
})

describe('actionStyle', () => {
  // The pending case composes the console's existing attention mapping rather
  // than restating it, so a WARNING action is the same colour on both pages.
  it('borrows the attention tone while an action is pending', () => {
    expect(actionStyle('ACTION_REQUIRED', 'PENDING').tone).toBe('attention')
    expect(actionStyle('WARNING', 'PENDING').tone).toBe('warning')
    expect(actionStyle('INFO', 'PENDING').tone).toBe('info')
  })

  it('goes quiet once an action is settled', () => {
    for (const status of ['RESOLVED', 'EXPIRED']) {
      expect(actionStyle('ACTION_REQUIRED', status).tone).toBe('neutral')
    }
  })

  // §9's rule as a value: PENDING, RESOLVED and EXPIRED are the three this build
  // knows, and APPROVED and DENIED are not among them.
  it('spells the three statuses it knows', () => {
    expect(actionStyle('WARNING', 'RESOLVED').label).toBe('resolved')
    expect(actionStyle('WARNING', 'EXPIRED').label).toBe('expired')
  })

  it('shows a status it does not know rather than blanking it', () => {
    const style = actionStyle('WARNING', 'APPROVED')
    expect(style.label).toBe('APPROVED')
    expect(style.tone).toBe('neutral')
    expect(style.detail).toContain('APPROVED')
  })
})

describe('actionTypeLabel', () => {
  // §16: the sentence is this build's, not the server's. A label that quoted the
  // request would be the leak, so the three types are three fixed phrases.
  it('writes a fixed sentence for each type', () => {
    expect(actionTypeLabel('PERMISSION_REQUEST')).toBe('Claude is waiting for permission')
    expect(actionTypeLabel('VIEW_FAILURE')).toBe('Something failed')
    expect(actionTypeLabel('VIEW_COMPLETION')).toBe('Something finished')
  })

  it('shows a type it does not know rather than pretending it is fine', () => {
    expect(actionTypeLabel('SOMETHING_NEW')).toBe('SOMETHING_NEW')
    expect(actionTypeLabel('')).toBe('Unknown action')
  })
})

describe('pendingSentence', () => {
  it('agrees with its own count', () => {
    expect(pendingSentence(0)).toBe('0 actions pending')
    expect(pendingSentence(1)).toBe('1 action pending')
    expect(pendingSentence(3)).toBe('3 actions pending')
  })
})

describe('groupActions', () => {
  // The property the whole page rests on: the server decided the order, and a
  // section is a filter over it rather than a second sort.
  it('keeps the order the server sent, in every list', () => {
    const queue = [
      makeAction({ id: 'act_aaaa', type: 'VIEW_FAILURE', level: 'WARNING', reason: 'agent failed' }),
      makeAction({ id: 'act_bbbb', projectName: 'studio' }),
      makeAction({ id: 'act_cccc', type: 'VIEW_COMPLETION', level: 'INFO', reason: 'agent completed' }),
      makeResolvedAction({ id: 'act_dddd' }),
      makeAction({ id: 'act_eeee', projectName: 'alpha' }),
    ]

    const groups = groupActions(queue)

    expect(groups.needsYou.map((action) => action.id)).toEqual(['act_bbbb', 'act_eeee'])
    expect(groups.notices.map((action) => action.id)).toEqual(['act_aaaa', 'act_cccc'])
    expect(groups.settled.map((action) => action.id)).toEqual(['act_dddd'])
  })

  it('answers with three lists even when there is nothing in them', () => {
    expect(groupActions([])).toEqual({ needsYou: [], notices: [], settled: [] })
  })
})
