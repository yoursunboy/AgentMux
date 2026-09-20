/**
 * Types mirroring the Go JSON contract.
 *
 * These are hand-written on purpose. A generated client would hide the fact
 * that the API is small enough to read, and the shapes here are the ones the
 * server actually sends - see internal/httpapi.
 */

/** A registered development directory. */
export interface Project {
  id: string
  name: string
  /** Authoritative location in host form, for example D:\AI\Projects\App. */
  hostPath: string
  /** The same location as the runtime sees it, for example /mnt/d/AI/Projects/App. */
  runtimePath: string
  /** The collection folder directly containing this project, or "" at a root. */
  collectionPath: string
  /** Derived by the server from the project's terminal runtime. */
  status: ProjectStatus
  pinnedSlot: number | null
  archived: boolean
  createdAt: string
  updatedAt: string
  lastOpenedAt: string | null
}

/**
 * The lifecycle of a project's terminal runtime, as the server reports it.
 *
 * The set is deliberately small. "Waiting for input", "finished", and "asking
 * permission" are states of the program running inside the terminal, not of the
 * terminal, and belong to the Claude Hooks phase.
 *
 * `starting` and `stopping` are transitions; `orphan` means a session exists
 * that no registered project claims, which the server reports and does not
 * touch.
 */
export type ProjectStatus =
  | 'stopped'
  | 'starting'
  | 'running'
  | 'stopping'
  | 'reconnecting'
  | 'error'
  | 'orphan'

/** The state of one project's runtime, from GET /api/projects/{id}/runtime. */
export interface Runtime {
  projectId: string
  /** The backend that owns the session, for example "tmux". */
  backend: string
  /** The session identity: always derived from the project id, never its name. */
  session: string
  state: RuntimeState
  /**
   * Whether the session exists right now, which is not the same as the state: a
   * stopped runtime keeps its session so its scrollback survives.
   */
  sessionAlive: boolean
  /** The canonical geometry. AgentMux's decision, not a client's. */
  cols: number
  rows: number
  /** The highest output sequence number produced so far. */
  sequence: number
  startedAt?: string
  updatedAt: string
  lastSeenAt?: string
  /** Explains a state that needs explaining, for example why a start failed. */
  message?: string
}

/** The runtime states the server produces. */
export type RuntimeState = 'STOPPED' | 'STARTING' | 'RUNNING' | 'STOPPING' | 'ERROR' | 'ORPHAN'

/** A directory that looks like it could be a project. A suggestion only. */
export interface Candidate {
  name: string
  hostPath: string
  runtimePath: string
  collectionPath: string
  projectsRoot: string
  depth: number
  markers: string[]
  score: number
  confidence: 'high' | 'medium' | 'low'
  /** True when the folder name lowered this candidate's rank, not when it was banned. */
  nameDiscounted: boolean
  registered: boolean
  projectId?: string
}

/** What happened at one Projects Root during a scan. */
export interface RootScan {
  path: string
  exists: boolean
  error?: string
}

export interface DiscoveryResult {
  roots: RootScan[]
  candidates: Candidate[]
  warnings: string[]
  scannedDirectories: number
  truncated: boolean
  durationMs: number
}

/** One probed external dependency. */
export interface Dependency {
  name: string
  available: boolean
  path?: string
  required: boolean
  /**
   * The environment the probe actually inspected, for example "linux" or
   * "wsl:Ubuntu-24.04". It is what makes "tmux is missing" actionable: the
   * probe does not always look where the user is typing.
   */
  probedIn?: string
  note?: string
}

/** The state of the AI provider integration. */
export interface ProviderStatus {
  tool: string
  /** False until CC Switch integration lands: the UI must not pretend otherwise. */
  integrated: boolean
  status: string
}

/** The body of GET /api/server. Contains no secrets. */
export interface ServerInfo {
  appName: string
  version: string
  /** The roadmap phase this build implements. */
  phase: string
  status: string
  startedAt: string
  uptimeSeconds: number

  host: string
  hostArch: string
  runtimeMode: string
  runtimeOs: string
  distro?: string
  pathMapper: string

  /**
   * Where the server process itself is running, which on Windows is not where
   * the runtime runs.
   */
  environment: string

  /**
   * Whether this server can run a persistent terminal at all. A client must not
   * offer to start one, or draw one, while this is false.
   */
  runtimeAvailable: boolean
  /** The actionable explanation when it is false. Empty when it is true. */
  runtimeUnavailableReason?: string

  projectsRoot: string
  projectsRoots: string[]
  discoveryDepth: number

  dataDirectory: string
  databasePath: string
  configFile?: string
  webDirectory?: string

  /**
   * Whether this build contains the runtime at all. It is a statement about the
   * software, while `runtimeAvailable` is a statement about the machine and
   * `features.terminal` combines them with what is installed. All three have to
   * hold before a terminal may be offered.
   */
  terminalRuntimeImplemented: boolean

  /**
   * Why a terminal cannot be offered here, in one field, empty when it can. It
   * is the same sentence that appears in `warnings`, promoted so a client does
   * not have to find it in a list.
   */
  terminalBlocker?: string

  dependencies: Dependency[]
  provider: ProviderStatus
  features: Record<string, boolean>
  warnings: string[]
}

/** What to create. */
export interface CreateProjectInput {
  name: string
  /** Empty means the Projects Root itself. */
  collectionPath?: string
  /** Selects a root when collectionPath is empty. */
  projectsRoot?: string
  initGit?: boolean
}

/** An existing directory to adopt. */
export interface RegisterProjectInput {
  hostPath: string
  /** Overrides the display name. Empty means the directory's own name. */
  name?: string
}

/**
 * What somebody wants an agent to do, from POST /api/projects/{id}/tasks.
 *
 * A task is not a terminal, not a runtime and not a process. It is the thing
 * those exist to serve, and it outlives every one of them: "is the terminal up"
 * stops meaning anything when the process exits, while "was this finished" stays
 * true afterwards. See docs/TASK_MODEL.md.
 *
 * There is no `description`, `priority` or `tags`. A field belongs here when
 * something reads it, and nothing does.
 */
export interface Task {
  /** "task_" followed by 96 random bits in hex. */
  id: string
  projectId: string
  /** What the person wrote. Free text, never markup, never a command. */
  title: string
  status: TaskStatus
  /** Server-produced, UTC. */
  createdAt: string
  updatedAt: string
  /**
   * When the task entered `COMPLETED`, or null.
   *
   * It is present and null rather than omitted, so a client rendering
   * "completed at" can tell "not completed" from "this server does not say".
   */
  completedAt: string | null
}

/**
 * A task's lifecycle.
 *
 * `WAITING` is the state a task has and a session deliberately does not: being
 * blocked on something outside the work is a real condition for a piece of
 * work, and it is not clear what an attempt would be waiting for.
 *
 * The three terminal statuses are final. `COMPLETED` cannot return to
 * `RUNNING` - a fact that was reported as finished and then reported as running
 * again would make the first report false.
 */
export type TaskStatus = 'CREATED' | 'RUNNING' | 'WAITING' | 'COMPLETED' | 'FAILED' | 'CANCELLED'

/**
 * One attempt at a task, from POST /api/tasks/{id}/sessions.
 *
 * A task and a process have different lifetimes, and this is the level where
 * that difference is recorded: restarting the terminal is a second session
 * rather than an overwrite of the first, and the task is what says both were
 * attempts at the same goal.
 */
export interface AgentSession {
  /** "sess_" followed by 96 random bits in hex. */
  id: string
  taskId: string
  /**
   * The runtime this attempt ran in, or absent.
   *
   * It is omitted rather than null when there is none, because the window
   * between creating an attempt and attaching a runtime is a real state: a
   * client that sees no `runtimeId` knows the attempt has not been given one
   * yet. It is set once and never rewritten.
   */
  runtimeId?: string
  status: AgentSessionStatus
  /** Set when the attempt entered `RUNNING`, null before that. */
  startedAt: string | null
  /** Set when the attempt stopped running, null while it runs. */
  endedAt: string | null
  createdAt: string
}

/** An attempt's lifecycle. There is no `WAITING`; see TaskStatus. */
export type AgentSessionStatus = 'CREATED' | 'RUNNING' | 'COMPLETED' | 'FAILED' | 'CANCELLED'

/** The body of POST /api/projects/{id}/tasks. The project is in the path. */
export interface CreateTaskInput {
  title: string
}

/**
 * The body of PATCH /api/tasks/{id}.
 *
 * Both fields are optional and at least one is required. A field that is absent
 * is left alone; sending a field as null is refused, because there is no such
 * thing as a task with no title or an attempt with no status, and accepting the
 * request would silently mean something other than what it says.
 */
export interface UpdateTaskInput {
  title?: string
  status?: TaskStatus
}

/** The body of PATCH /api/sessions/{id}. */
export interface UpdateSessionInput {
  status?: AgentSessionStatus
  /**
   * The runtime to bind this attempt to. It is set at most once - a session
   * that already has one is refused rather than rebound - and the runtime need
   * not exist: a runtime can be destroyed while the attempt that used it
   * remains.
   */
  runtimeId?: string
}
