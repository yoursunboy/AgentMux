-- AgentMux initial schema (Phase 1: project model and server foundation).
--
-- Scope rules this schema encodes:
--
--   * SQLite holds metadata only. Terminal output is never stored here; a
--     growing output log is not a metadata concern and would make the
--     database unbounded.
--   * AgentMux runtime state is never written into a managed project
--     repository. Everything AgentMux knows lives in its own data directory.
--   * There is deliberately no project_runtime table yet. Runtime columns
--     (canonical PTY size, output sequence, controller lease) have no
--     meaning until the SessionBackend exists in Phase 2, and adding them
--     now would only invite code to read and write placeholder values.

CREATE TABLE IF NOT EXISTS projects (
    -- Stable identity. Display names may change; this must not, because the
    -- tmux session name is derived from it as "amx-{id}".
    id              TEXT    NOT NULL PRIMARY KEY,

    -- User-facing name. Renaming a project never changes its identity.
    name            TEXT    NOT NULL,

    -- The authoritative project location, in host form.
    host_path       TEXT    NOT NULL,

    -- The same location as the runtime sees it. Derived through the
    -- HostAdapter at registration time and stored, so that a later change to
    -- the mapping configuration cannot silently point a project elsewhere.
    runtime_path    TEXT    NOT NULL,

    -- The collection/group folder directly containing the project, or '' when
    -- the project is a direct child of its Projects Root. Collections are an
    -- organisational layer only: AgentMux never runs a session against one.
    collection_path TEXT    NOT NULL DEFAULT '',

    -- Reserved display slot. NULL means the project is not pinned.
    pinned_slot     INTEGER,

    archived        INTEGER NOT NULL DEFAULT 0,

    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL,
    last_opened_at  TEXT
);

-- Host path identifies a project. NOCASE makes re-registering the same
-- project under different letter casing resolve to the existing record
-- instead of creating a duplicate, which is what a Windows user expects.
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_host_path
    ON projects (host_path COLLATE NOCASE);

CREATE INDEX IF NOT EXISTS idx_projects_archived
    ON projects (archived);

-- Small key/value store for server-level preferences. Kept intentionally
-- narrow in Phase 1: an install identity, and room for settings that belong
-- to the installation rather than to a project.
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT NOT NULL PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
