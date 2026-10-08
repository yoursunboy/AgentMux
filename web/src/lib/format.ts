import { ApiError, ErrorCodes } from '../api/client'
import type { Project, ProjectStatus, ServerInfo } from '../api/types'

/** formatUptime renders a duration in seconds compactly. */
export function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '-'
  if (seconds < 60) return `${Math.floor(seconds)}s`

  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m`

  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ${minutes % 60}m`

  return `${Math.floor(hours / 24)}d ${hours % 24}h`
}

/** formatTimestamp renders an RFC3339 value as a local date and time. */
export function formatTimestamp(value: string | null | undefined): string {
  if (!value) return 'Never'
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) return 'Unknown'
  return parsed.toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

/**
 * formatRelativeTime renders a timestamp as how long ago it was.
 *
 * §7 of the phase brief asks for "10 minutes ago" rather than a date, and it is
 * the right answer for an action: what a person needs to know about something
 * waiting is whether it started waiting now or an hour ago, and a clock time
 * makes them do the subtraction.
 *
 * `now` is a parameter so a test can pin it. A function that read the clock
 * itself could only be tested by mocking one, and the rounding boundaries - the
 * place this kind of function is actually wrong - are exactly what a fixed
 * instant lets a test reach.
 *
 * A value this build cannot read is shown as it arrived rather than as a
 * duration: "Unknown" for an empty one, and the raw string for one that is not a
 * timestamp. A time that came from nowhere must not read as "just now".
 */
export function formatRelativeTime(
  value: string | null | undefined,
  now: Date = new Date(),
): string {
  if (!value) return 'Unknown'

  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) return value

  const seconds = Math.floor((now.getTime() - parsed.getTime()) / 1000)

  // A clock that is slightly behind the server's is ordinary, and "in 3 seconds"
  // about something that has already happened would be a confusing thing to
  // read. Anything within a minute either way is now.
  if (seconds < 60) return 'just now'

  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} ${minutes === 1 ? 'minute' : 'minutes'} ago`

  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} ${hours === 1 ? 'hour' : 'hours'} ago`

  const days = Math.floor(hours / 24)
  if (days < 30) return `${days} ${days === 1 ? 'day' : 'days'} ago`

  const months = Math.floor(days / 30)
  if (months < 12) return `${months} ${months === 1 ? 'month' : 'months'} ago`

  const years = Math.floor(months / 12)
  return `${years} ${years === 1 ? 'year' : 'years'} ago`
}

/**
 * describeHost renders the Global Bar's host section, for example
 * "Windows / WSL" or "Linux".
 */
export function describeHost(info: ServerInfo | null): string {
  if (!info) return 'Unknown host'
  const host = capitalise(info.host)
  if (info.runtimeMode === 'wsl') {
    // Naming the distribution matters when several are installed and the
    // projects live in a particular one.
    return info.distro ? `${host} / WSL (${info.distro})` : `${host} / WSL`
  }
  if (info.runtimeMode === 'native' && info.runtimeOs !== info.host) {
    return `${host} / ${capitalise(info.runtimeOs)}`
  }
  return host
}

/** capitalise turns "windows" into "Windows". */
export function capitalise(value: string): string {
  if (!value) return ''
  return value.charAt(0).toUpperCase() + value.slice(1)
}

/** describeStatus is the label shown next to the state glyph. */
export function describeStatus(status: ProjectStatus): string {
  switch (status) {
    case 'running':
      return 'Running'
    case 'starting':
      return 'Starting'
    case 'stopping':
      return 'Stopping'
    case 'reconnecting':
      return 'Reconnecting'
    case 'error':
      return 'Error'
    case 'orphan':
      return 'Orphaned session'
    default:
      return 'Stopped'
  }
}

/** statusGlyph is the state indicator from the UI spec. */
export function statusGlyph(status: ProjectStatus): string {
  switch (status) {
    case 'running':
      return '●' // filled circle
    case 'reconnecting':
      return '◌' // dotted circle
    case 'starting':
    case 'stopping':
      return '◍' // half-filled: a transition, not a resting point
    case 'error':
      return '▲'
    case 'orphan':
      return '◆'
    default:
      return '○' // hollow circle
  }
}

/**
 * isRuntimeUp reports whether a runtime is in a state where stopping it means
 * something.
 *
 * A reconnecting runtime counts as up: the session is alive and work is running
 * in it, and what is being re-established is AgentMux's view of its output.
 */
export function isRuntimeUp(status: ProjectStatus): boolean {
  return status === 'running' || status === 'reconnecting'
}

/**
 * isRuntimeBusy reports whether a runtime is mid-transition, where neither
 * starting nor stopping it is a meaningful request.
 */
export function isRuntimeBusy(status: ProjectStatus): boolean {
  return status === 'starting' || status === 'stopping'
}

/**
 * shortenPath trims the middle of a long path so the last segments stay
 * visible, which are the ones that identify the project.
 */
export function shortenPath(path: string, maxLength = 48): string {
  if (path.length <= maxLength) return path
  const separator = path.includes('\\') ? '\\' : '/'
  const parts = path.split(separator).filter(Boolean)
  if (parts.length <= 2) return path

  let tail = parts[parts.length - 1]
  for (let i = parts.length - 2; i >= 0; i--) {
    const candidate = `${separator}${parts[i]}${separator}${tail}`
    if (candidate.length + 3 > maxLength) break
    tail = `${parts[i]}${separator}${tail}`
  }
  return `...${separator}${tail}`
}

/**
 * describeError turns a failure into something worth showing a user.
 *
 * The server sends a stable code and a message written for humans; this adds
 * the guidance the message cannot carry on its own, and never falls back to a
 * bare "something went wrong".
 *
 * # The one thing added outside the switch
 *
 * A failed restart carries the attempt it retired, and that is not a detail of
 * the failure - it is a change to the project that happened *before* it. A
 * caller told only that the request failed would read a project that has no
 * agent in it as one that still has the agent it started with, so the note is
 * appended whatever the code was, because the half that failed is not the half
 * that changed anything.
 */
export function describeError(error: unknown): string {
  if (!(error instanceof ApiError)) {
    return error instanceof Error ? error.message : 'An unexpected error occurred.'
  }
  if (error.isNetworkFailure) {
    return 'Could not reach the AgentMux server. Check that it is running, then retry.'
  }
  return `${describeCoded(error)}${describeRetired(error.details)}`
}

/**
 * describeRetired reports the attempt a failed restart closed on its way to
 * failing.
 *
 * An empty string is the ordinary case: every failure that is not a restart
 * carries no `retired`, and neither does a restart that failed before it
 * stopped anything.
 */
function describeRetired(details: Record<string, unknown>): string {
  const retired = details.retired
  if (typeof retired !== 'object' || retired === null) return ''

  const { status } = retired as { status?: unknown }
  const how = typeof status === 'string' && status !== '' ? ` as ${status.toLowerCase()}` : ''
  return (
    ` The attempt that was running was closed${how}, and the new one did not start, so this ` +
    'project has no agent in it now. Start one again when you are ready.'
  )
}

/** describeCoded is the per-code guidance, for a failure that reached the server. */
function describeCoded(error: ApiError): string {
  switch (error.code) {
    case ErrorCodes.alreadyRegistered: {
      const path = error.details.hostPath
      return typeof path === 'string'
        ? `${path} is already registered.`
        : 'That folder is already registered.'
    }
    case ErrorCodes.targetExists:
      return 'A folder with that name already exists. Choose another name, or register the existing folder instead.'
    case ErrorCodes.outsideProjectsRoot:
      return `${error.message} Add it as a Projects Root on the server, then try again.`
    case ErrorCodes.pathIsProjectsRoot:
      return `${error.message} Register a folder inside it instead.`
    case ErrorCodes.pathNotFound:
      return `${error.message} Check the path spelling, and that the drive is available.`
    case ErrorCodes.notADirectory:
      return `${error.message} Choose a folder, not a file.`
    case ErrorCodes.pathNotAccessible:
      return `${error.message} Check the folder's permissions.`
    case ErrorCodes.gitUnavailable:
      return 'Git is not available on the server, so the repository was not created. Install Git, then create the project again.'
    case ErrorCodes.gitInitFailed: {
      // The server reports whether it rolled the new folder back, because only
      // it knows whether this request created the folder or found it there.
      const tail =
        error.details.rolledBack === true
          ? ' The folder AgentMux created for it was removed again, so nothing was left behind.'
          : ' The folder was left exactly as it was.'
      return `${error.message}${tail}`
    }
    case ErrorCodes.runtimePathMappingFailed:
      return `${error.message} Check the runtime mode and WSL mount configuration on the server.`
    // The runtime refusals carry the server's own explanation, which is the one
    // that says whether to start the server inside WSL or to install tmux - the
    // two have different fixes and guessing between them wastes the user's time.
    case ErrorCodes.runtimeUnavailable:
    case ErrorCodes.runtimeBackendUnavailable:
      return error.message
    case ErrorCodes.runtimeNotRunning:
      return `${error.message} Start the runtime, then try again.`
    case ErrorCodes.runtimeAlreadyRunning:
      return `${error.message} It is already running.`
    case ErrorCodes.runtimeStartFailed:
      return `${error.message} Check the server log, and that the project's folder still exists.`
    case ErrorCodes.runtimeStopFailed:
    case ErrorCodes.runtimeDestroyFailed:
      return `${error.message} The session may still be running; check the server log.`
    // The agent lifecycle's refusals. Each carries the server's own sentence,
    // which is the actionable half - "the shell in this terminal is not in the
    // project's directory" says what to do in a way no generic wording can.
    case ErrorCodes.agentUnavailable:
    case ErrorCodes.agentLaunchFailed:
    case ErrorCodes.agentStopFailed:
    case ErrorCodes.agentTerminalBusy:
    case ErrorCodes.agentWrongDirectory:
      return error.message
    case ErrorCodes.agentStopTimeout: {
      // Read rather than reported, because it is not a failure of the request:
      // the interrupt was delivered, the agent declined it, and the state the
      // card shows next has to be "still running". The pid and the grace are
      // the server's own details, and they are the two facts that make the
      // sentence checkable rather than reassuring.
      const pid = error.details.pid
      const grace = error.details.timeout
      const which = typeof pid === 'number' ? ` (process ${pid})` : ''
      const within = typeof grace === 'string' ? ` within ${grace}` : ''
      return (
        `Claude did not stop${within}${which}, so it is still running and nothing was started ` +
        'in its place. It may be handling the interrupt itself, or waiting on something of ' +
        'its own; stop it again once it is idle.'
      )
    }
    case ErrorCodes.storageFailure:
      return 'The AgentMux metadata store could not complete the request. Check the server log.'
    case ErrorCodes.internal:
    case ErrorCodes.notFoundHttp:
      return `${error.message} Check the server log for details.`
    default:
      return error.message
  }
}

/**
 * describeProjectLocation is the subtitle under a project name: the collection
 * it sits in, or a note that it sits directly under a root.
 */
export function describeProjectLocation(project: Project): string {
  if (!project.collectionPath) return 'Directly under a Projects Root'
  return `Collection: ${shortenPath(project.collectionPath, 40)}`
}

/** pluralise picks between a singular and a plural noun. */
export function pluralise(count: number, singular: string, plural = `${singular}s`): string {
  return count === 1 ? singular : plural
}

/**
 * SessionPrefix is the namespace every AgentMux runtime session name carries.
 *
 * It mirrors `project.SessionPrefix` in the Go model, and it is spelled once
 * here for the same reason it is spelled once there: two spellings of a session
 * name are two different sessions, and the failure that produces - a project
 * whose terminal is running but unreachable - is invisible until it matters.
 */
export const SessionPrefix = 'amx-'

/**
 * sessionNameFor is the runtime session name for a project.
 *
 * Derived from the stable id and never from the display name, so renaming a
 * project cannot orphan a running session.
 */
export function sessionNameFor(projectId: string): string {
  return `${SessionPrefix}${projectId}`
}

/**
 * displaySlot renders a zero-based workspace slot the way a person counts.
 *
 * The stored slot is an index because that is what a page and a position are
 * computed from; "slot 1" on screen is the first one, which is index 0.
 */
export function displaySlot(slot: number | null): string {
  return slot === null ? 'Not in the workspace' : String(slot + 1)
}
