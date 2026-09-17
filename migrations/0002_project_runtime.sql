-- AgentMux runtime schema (Phase 2: the persistent session runtime).
--
-- What is stored here, and - more importantly - what is not.
--
-- Stored, because nothing else can answer these questions after a restart:
--
--   * Which session belongs to this project. The name is derivable from the
--     project id, but storing it means a future change to the naming rule
--     cannot make an existing session unreachable.
--
--   * What terminal size the project should come back at. The session itself
--     cannot answer that while it is stopped, and it is a decision the user
--     made rather than something recomputable.
--
--   * Whether the user had the runtime running or stopped. This is intent, not
--     fact: the server checks it against the actual session at startup, so a
--     stale value is corrected rather than believed.
--
-- Not stored, deliberately:
--
--   * Terminal output, in any form, at any granularity. Output belongs to the
--     tmux session, which holds it in its own scrollback. Copying it here
--     would make the database unbounded and would still be a worse terminal
--     than the one it copied.
--
--   * Which client is attached, which controller holds the lease, which
--     WebSocket is open, where a browser has scrolled to. Every one of these
--     is a property of a live connection; a stored copy would outlive the
--     connection that made it true and would be read as fact by the next
--     reader.
--
--   * Liveness as a fact. There is no "is_running" column, because a boolean
--     written once and read much later is a claim about a process that is no
--     longer running. Liveness is asked of the runtime, which is the only
--     thing that knows.

CREATE TABLE IF NOT EXISTS project_runtime (
    project_id     TEXT    NOT NULL PRIMARY KEY,

    -- Which backend owns this session. Recorded so that a future release with
    -- more than one backend can tell whose session this is from the record
    -- rather than guessing from the name.
    backend        TEXT    NOT NULL,

    -- The session name, "amx-{project_id}".
    session_name   TEXT    NOT NULL,

    -- 'RUNNING', 'STOPPED', or 'ERROR'. Transitional states are never written:
    -- a transition recorded on disk would be a claim about a process that has
    -- already moved on.
    state          TEXT    NOT NULL,

    -- The canonical terminal geometry, owned by AgentMux rather than by
    -- whichever client attached last.
    canonical_cols INTEGER NOT NULL,
    canonical_rows INTEGER NOT NULL,

    created_at     TEXT    NOT NULL,
    updated_at     TEXT    NOT NULL,

    -- When the server last confirmed this session existed. Null means it has
    -- never been confirmed since the record was written, which is itself
    -- worth knowing when reading a record that claims to be running.
    last_seen_at   TEXT,

    -- A runtime without its project is meaningless, and archiving a project
    -- is not deleting it, so the cascade only fires on a real delete.
    FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
);

-- One session name belongs to one project. The unique index makes a
-- reconciliation that tries to adopt one session for two projects fail loudly
-- instead of silently picking a winner.
CREATE UNIQUE INDEX IF NOT EXISTS idx_project_runtime_session
    ON project_runtime (session_name);
