import type { Candidate, DiscoveryResult, Project, ServerInfo } from '../api/types'

/**
 * Builders for the API shapes, so a test states only what it is about.
 *
 * Every builder takes a partial override; the defaults describe an ordinary
 * healthy server so a test that only cares about one field stays readable.
 */

export function makeProject(overrides: Partial<Project> = {}): Project {
  return {
    id: 'p_0123456789abcdef0123',
    name: 'AgentMux',
    hostPath: 'D:\\AI\\Projects\\2026 AgentMux\\AgentMux',
    runtimePath: '/mnt/d/AI/Projects/2026 AgentMux/AgentMux',
    collectionPath: 'D:\\AI\\Projects\\2026 AgentMux',
    status: 'stopped',
    pinnedSlot: null,
    archived: false,
    createdAt: '2026-09-17T12:00:00Z',
    updatedAt: '2026-09-17T12:00:00Z',
    lastOpenedAt: null,
    ...overrides,
  }
}

export function makeCandidate(overrides: Partial<Candidate> = {}): Candidate {
  return {
    name: 'AgentMux',
    hostPath: 'D:\\AI\\Projects\\2026 AgentMux\\AgentMux',
    runtimePath: '/mnt/d/AI/Projects/2026 AgentMux/AgentMux',
    collectionPath: 'D:\\AI\\Projects\\2026 AgentMux',
    projectsRoot: 'D:\\AI\\Projects',
    depth: 2,
    markers: ['CLAUDE.md', 'go.mod'],
    score: 5,
    confidence: 'high',
    nameDiscounted: false,
    registered: false,
    ...overrides,
  }
}

export function makeDiscovery(overrides: Partial<DiscoveryResult> = {}): DiscoveryResult {
  return {
    roots: [{ path: 'D:\\AI\\Projects', exists: true }],
    candidates: [makeCandidate()],
    warnings: [],
    scannedDirectories: 12,
    truncated: false,
    durationMs: 8,
    ...overrides,
  }
}

export function makeServerInfo(overrides: Partial<ServerInfo> = {}): ServerInfo {
  return {
    appName: 'AgentMux',
    version: '0.1.0',
    phase: 'Phase 5 - Multi-project workspace',
    status: 'online',
    startedAt: '2026-09-17T12:00:00Z',
    uptimeSeconds: 754,

    host: 'windows',
    hostArch: 'amd64',
    runtimeMode: 'wsl',
    runtimeOs: 'linux',
    distro: 'Ubuntu-24.04',
    pathMapper: 'wsl',

    // The common case on Windows: the server is on the host, so it cannot host
    // a terminal. A test that wants the other case overrides these three.
    environment: 'windows',
    runtimeAvailable: false,
    runtimeUnavailableReason: 'tmux runtime requires AgentMux Server to run inside WSL.',

    projectsRoot: 'D:\\AI\\Projects',
    projectsRoots: ['D:\\AI\\Projects'],
    discoveryDepth: 3,

    dataDirectory: 'C:\\Users\\yours\\AppData\\Local\\AgentMux',
    databasePath: 'C:\\Users\\yours\\AppData\\Local\\AgentMux\\agentmux.db',

    terminalRuntimeImplemented: true,
    terminalBlocker: 'tmux runtime requires AgentMux Server to run inside WSL.',

    dependencies: [{ name: 'git', available: true, required: false, probedIn: 'linux' }],
    provider: { tool: 'claude', integrated: false, status: 'not_integrated' },
    features: {
      projectRegistration: true,
      projectCreation: true,
      projectDiscovery: true,
      gitInit: true,
      terminal: false,
      providerSwitch: false,
    },
    warnings: [],
    ...overrides,
  }
}

/**
 * A server that can host a terminal: tmux is installed where sessions run and
 * the runtime is available. Tests that exercise the runtime controls start from
 * this.
 */
export function makeTerminalReadyServerInfo(overrides: Partial<ServerInfo> = {}): ServerInfo {
  return makeServerInfo({
    environment: 'wsl',
    runtimeAvailable: true,
    runtimeUnavailableReason: '',
    terminalBlocker: '',
    features: {
      projectRegistration: true,
      projectCreation: true,
      projectDiscovery: true,
      gitInit: true,
      terminal: true,
      providerSwitch: false,
    },
    ...overrides,
  })
}
