/**
 * Builders for the action centre's shapes.
 *
 * They live beside the other fixtures rather than inside `src/actions/`, for the
 * reason `test/controller.ts` does: they describe an API response, and a second
 * test file that wanted one should not have to import another test file.
 *
 * # Why these builders carry fields the server never sends
 *
 * `makeAction` returns an object with `prompt`, `tool_input`, `command`, `token`
 * and `stdout` on it - none of which the server has, and none of which may ever
 * reach the DOM. That is the point. §16 of the phase brief forbids showing what
 * Claude actually asked for, and a rule that is only checked against a clean
 * fixture is a rule that holds until somebody adds a spread. With these fields
 * present, "the card shows only the fields it names" becomes an assertion that
 * fails the moment `{...action}` appears in a component.
 *
 * The defaults describe the case that matters most: one permission request,
 * pending, needing somebody.
 */
import type { ActionItem, ActionQueue } from '../api/types'

/**
 * One action, as the queue would send it - plus the fields it never would.
 *
 * The extra keys are returned as part of the object deliberately. They are
 * typed away by the cast at the end so a caller still sees an `ActionItem`,
 * which is what a component is handed.
 */
export function makeAction(overrides: Partial<ActionItem> = {}): ActionItem {
  const action = {
    id: 'act_0123456789abcdef0123',
    projectId: 'p_0123456789abcdef0123',
    projectName: 'AgentMux',
    agentSessionId: 'sess_0123456789abcdef0123',
    type: 'PERMISSION_REQUEST',
    level: 'ACTION_REQUIRED',
    status: 'PENDING',
    reason: 'permission requested',
    createdAt: '2026-09-21T09:00:12Z',
    resolvedAt: null,

    // Everything below this line is a field the server does not have and this
    // client must never draw. See the file comment.
    prompt: 'deploy the scratch build',
    tool_input: { command: 'deploy.sh --token sk-test-0000' },
    command: 'deploy.sh --token sk-test-0000',
    token: 'sk-test-0000',
    transcript: 'assistant: I will run the deploy script',
    stdout: 'Enter passphrase for key /home/dev/.ssh/id_ed25519',

    ...overrides,
  }
  return action as ActionItem
}

/** An action already dealt with, which is what most of them become. */
export function makeResolvedAction(overrides: Partial<ActionItem> = {}): ActionItem {
  return makeAction({
    status: 'RESOLVED',
    level: 'NONE',
    reason: 'agent started',
    resolvedAt: '2026-09-21T09:05:00Z',
    ...overrides,
  })
}

/**
 * A whole queue response.
 *
 * The counts are folded the way the server folds them - pending only, split on
 * `ACTION_REQUIRED` - rather than being passed in. A test that had to say the
 * counts too would be a test that could disagree with the list it just built,
 * and the one thing a queue must never do is add up wrong.
 */
export function makeActionQueue(actions: ActionItem[] = []): ActionQueue {
  const pending = actions.filter((action) => action.status === 'PENDING')
  return {
    actions,
    count: actions.length,
    needsYou: pending.filter((action) => action.level === 'ACTION_REQUIRED').length,
    notices: pending.filter((action) => action.level !== 'ACTION_REQUIRED').length,
  }
}
