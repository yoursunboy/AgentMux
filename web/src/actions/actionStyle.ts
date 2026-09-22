/**
 * How an action reads, and nothing about what colour that is.
 *
 * It is `dashboard/status.ts`'s pattern applied to the action vocabulary: a type
 * becomes a tone, the tone becomes a class, and the class becomes a colour - and
 * only the last of those is styling, which is why none of it is here.
 *
 * # Why the level is read and the type is not
 *
 * "Which of these two lists does this row belong in" is answered from the action's
 * *level*, not from its type. The three types correspond exactly to three levels,
 * so a second place in the client that knew `PERMISSION_REQUEST` means
 * `ACTION_REQUIRED` would be a second place the two could drift apart - and the
 * whole reason the server derives one from the other is that drifting is the
 * failure mode. The type is used here for one thing only: the sentence a card
 * shows.
 */
import type { ActionItem } from '../api/types'
import type { StatusStyle } from '../dashboard/status'
import { attentionStyle } from '../dashboard/status'

/**
 * Which list an action belongs in.
 *
 *	needs-you   nothing progresses until somebody acts
 *	notices     worth reading, and nothing is blocked by it
 *	settled     already dealt with, or past the point of being dealt with
 *
 * The three are a product decision rather than a reading of the data, which is
 * why they are here and not on the server: the queue sends one ordered list, and
 * this is what a page makes of it.
 */
export type ActionGroup = 'needs-you' | 'notices' | 'settled'

/**
 * actionGroup places one action.
 *
 * A settled action is in neither of the two live lists, whatever its level. A
 * permission request that was answered does not need anybody, and leaving it
 * under "Needs you" would be the page telling a person to go and look at
 * something already done.
 *
 * Everything that is not `ACTION_REQUIRED` is a notice, including a level this
 * build does not know. That is the direction to fail in: a row from a newer
 * server is something to read, and it is certainly not a claim that work has
 * stopped.
 */
export function actionGroup(level: string, status: string): ActionGroup {
  if (status !== 'PENDING') return 'settled'
  return level === 'ACTION_REQUIRED' ? 'needs-you' : 'notices'
}

/**
 * actionStyle reads the action's level, and its status once it is no longer
 * pending.
 *
 * The pending case composes `attentionStyle` rather than restating it, so a
 * `WARNING` action is the same colour on this page as it is on a project card.
 * A settled action is neutral whatever it was: a badge that kept shouting after
 * the thing was dealt with would make the two lists impossible to tell apart at
 * a glance, which is the only reason the badge is coloured at all.
 */
export function actionStyle(level: string, status: string): StatusStyle {
  if (status === 'PENDING') return attentionStyle(level)

  switch (status) {
    case 'RESOLVED':
      return {
        tone: 'neutral',
        label: 'resolved',
        detail: 'This was dealt with. It is kept because the queue is a record as well as a to-do list.',
      }
    case 'EXPIRED':
      return {
        tone: 'neutral',
        label: 'expired',
        detail: 'Nothing was waiting on this any more by the time anybody looked.',
      }
    default:
      return {
        tone: 'neutral',
        label: status.trim() === '' ? 'Unknown' : status.trim(),
        detail: `${status} is not an action status this build knows about.`,
      }
  }
}

/**
 * actionTypeLabel is the sentence a card leads with.
 *
 * It is written by this build from the type and says nothing about what was
 * asked. §16 of the phase brief is the rule and this is where it is easiest to
 * break: the type is one enumeration, the request behind it is a tool name, a
 * path and a command line, and only the first of those is ever shown.
 */
export function actionTypeLabel(type: string): string {
  switch (type) {
    case 'PERMISSION_REQUEST':
      return 'Claude is waiting for permission'
    case 'VIEW_FAILURE':
      return 'Something failed'
    case 'VIEW_COMPLETION':
      return 'Something finished'
    default:
      return type.trim() === '' ? 'Unknown action' : type.trim()
  }
}

/**
 * pendingSentence writes the count the way both the card and the queue say it.
 *
 * It is one function rather than a template in each place because the card's
 * sentence and the queue's heading are the same claim about the same number, and
 * two spellings of it is how a console comes to disagree with itself.
 */
export function pendingSentence(count: number): string {
  return `${count} ${count === 1 ? 'action' : 'actions'} pending`
}

/** The queue, split into the three lists a page shows. */
export interface ActionGroups {
  needsYou: ActionItem[]
  notices: ActionItem[]
  settled: ActionItem[]
}

/**
 * groupActions splits the queue without reordering it.
 *
 * Each list is a stable filter of the array the server sent, in the order the
 * server sent it, so a section cannot present a different order from the queue
 * it came from. The server's ordering is the answer to "which of these is at the
 * top", and sorting here would be a second answer to it.
 *
 * The empty arrays are `[]` rather than null, so a caller renders one shape.
 */
export function groupActions(actions: readonly ActionItem[]): ActionGroups {
  const groups: ActionGroups = { needsYou: [], notices: [], settled: [] }
  for (const action of actions) {
    switch (actionGroup(action.level, action.status)) {
      case 'needs-you':
        groups.needsYou.push(action)
        break
      case 'notices':
        groups.notices.push(action)
        break
      default:
        groups.settled.push(action)
    }
  }
  return groups
}
