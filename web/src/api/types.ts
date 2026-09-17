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
  /** Derived by the server. Phase 1 always reports "stopped". */
  status: ProjectStatus
  pinnedSlot: number | null
  archived: boolean
  createdAt: string
  updatedAt: string
  lastOpenedAt: string | null
}

/**
 * Phase 1 has no session runtime, so "running" and "reconnecting" are part of
 * the contract but are never produced yet.
 */
export type ProjectStatus = 'stopped' | 'running' | 'reconnecting'

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
  installId?: string

  host: string
  hostArch: string
  runtimeMode: string
  runtimeOs: string
  distro?: string
  pathMapper: string

  projectsRoot: string
  projectsRoots: string[]
  discoveryDepth: number

  dataDirectory: string
  databasePath: string
  configFile?: string
  webDirectory?: string

  /** False until Phase 2. A client must not render a terminal it cannot fill. */
  terminalRuntimeImplemented: boolean

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
