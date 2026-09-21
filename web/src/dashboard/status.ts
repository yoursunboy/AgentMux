/**
 * How a status reads, and nothing about what colour that is.
 *
 * # Why there is no colour in this file
 *
 * §8 of the phase brief asks for exactly this: a status mapping rather than
 * `if (status === 'RUNNING') green`. The reason is that the two questions are
 * different. "Is this good, urgent, or broken" is a fact about the status, and
 * it belongs in one place that every panel asks. "What colour is urgent" is a
 * fact about the theme, and the stylesheet already owns it as a custom
 * property.
 *
 * A component that decided both would be a component that had to change when
 * the palette did, and there would be as many places to change as there are
 * panels. Here, a status becomes a *tone*, the tone becomes a class, and the
 * class becomes a colour — and only the last of those three is styling.
 *
 * # What happens to a value this build does not know
 *
 * It gets the neutral tone and its own text. A server that grows a status, or
 * an older frontend talking to a newer one, shows the word rather than an
 * empty box: hiding a value nobody expected is how a dashboard reports "fine"
 * when something has gone wrong.
 */

/** How a status reads. The stylesheet turns each of these into a colour. */
export type Tone = 'neutral' | 'info' | 'positive' | 'attention' | 'warning' | 'danger'

/** A status, ready to render. */
export interface StatusStyle {
  /** The tone, which the stylesheet maps to a colour token. */
  tone: Tone

  /** What to show. Almost always the status itself, spelled as the server spelled it. */
  label: string

  /** One line saying what it means, for a tooltip. */
  detail: string
}

/** known builds a style for a value this file has a meaning for. */
function known(tone: Tone, label: string, detail: string): StatusStyle {
  return { tone, label, detail }
}

/**
 * unknown builds a style for a value this file does not recognise.
 *
 * The raw value is shown rather than replaced. It is the difference between
 * "this server is running something this build does not know about" and "there
 * is nothing here", and only one of those is true.
 */
function unknown(value: string, what: string): StatusStyle {
  const label = value.trim()
  return {
    tone: 'neutral',
    label: label === '' ? 'Unknown' : label,
    detail:
      label === ''
        ? `This project reports no ${what}.`
        : `${label} is not a ${what} this build knows about.`,
  }
}

/**
 * runtimeStyle reads the project model's vocabulary, which is lowercase.
 *
 * It is the same set `GET /api/projects` returns in a project's `status`, and
 * it is passed through rather than translated - see `docs/CONTROLLER_API.md` §2.
 */
export function runtimeStyle(status: string): StatusStyle {
  switch (status) {
    case 'running':
      return known('positive', 'running', 'This project has a terminal running.')
    case 'starting':
      return known('attention', 'starting', 'The terminal is coming up.')
    case 'stopping':
      return known('attention', 'stopping', 'The terminal is shutting down.')
    case 'reconnecting':
      return known('attention', 'reconnecting', 'The terminal is alive and its output is being re-established.')
    case 'stopped':
      return known('neutral', 'stopped', 'No terminal is running for this project.')
    case 'error':
      return known('danger', 'error', 'The terminal could not be started, or it failed.')
    case 'orphan':
      return known('danger', 'orphan', 'A terminal exists for this project but nothing is registered for it.')
    default:
      return unknown(status, 'runtime status')
  }
}

/**
 * agentStyle reads the agent state projection's vocabulary, which is uppercase.
 *
 * It is the same set `GET /api/sessions/{id}/state` returns, and it is the
 * agent's own - a runtime being RUNNING says nothing about what the agent is
 * doing, which is the whole reason the two are separate.
 */
export function agentStyle(status: string): StatusStyle {
  switch (status) {
    case 'CREATED':
      return known('neutral', 'created', 'The attempt exists and nothing has started it.')
    case 'RUNNING':
      return known('positive', 'running', 'The agent is working.')
    case 'WAITING_INPUT':
      return known('attention', 'waiting for input', 'The agent is waiting for a person to say something.')
    case 'WAITING_PERMISSION':
      return known('attention', 'waiting for permission', 'The agent asked to use a tool and is blocked on the answer.')
    case 'COMPLETED':
      return known('positive', 'completed', 'The last turn finished and reported success.')
    case 'FAILED':
      return known('danger', 'failed', 'The last turn finished and reported failure.')
    case 'STOPPED':
      return known('neutral', 'stopped', 'The session ended and nothing is running.')
    default:
      return unknown(status, 'agent status')
  }
}

/**
 * attentionStyle reads the attention projection's vocabulary.
 *
 * `ACTION_REQUIRED` is the only level that means nothing progresses without a
 * person, which is why it reads stronger than a warning rather than weaker.
 */
export function attentionStyle(level: string): StatusStyle {
  switch (level) {
    case 'NONE':
      return known('neutral', 'nothing needed', 'There is nothing to see here.')
    case 'ACTION_REQUIRED':
      return known('attention', 'needs you', 'Something is blocked until a person acts.')
    case 'WARNING':
      return known('warning', 'warning', 'Something went wrong. It can be read later.')
    case 'INFO':
      return known('info', 'info', 'Something worth knowing happened.')
    default:
      return unknown(level, 'attention level')
  }
}

/**
 * unavailableStyle reads a section the server could not answer at all.
 *
 * It is not the same as a status of "nothing is running", and the console says
 * so: the difference between a project nobody has started and a server that
 * cannot say is the difference between two things a person would do next.
 */
export function unavailableStyle(what: string): StatusStyle {
  return {
    tone: 'neutral',
    label: 'unavailable',
    detail: `This server was started without the ${what}, so it cannot say.`,
  }
}

/**
 * absentStyle reads a section that has nothing to report.
 *
 * It is the ordinary case and not a fault: a project no agent has ever run in
 * has no agent, which is a fact rather than a gap.
 */
export function absentStyle(what: string): StatusStyle {
  return {
    tone: 'neutral',
    label: 'none',
    detail: `Nothing has reported a ${what} for this project.`,
  }
}
