-- AgentMux agent session schema (Phase 7.2).
--
-- A session is one attempt at a task.
--
--     Task: Fix websocket bug
--       AgentSession #1 -> Runtime #1     the first attempt
--       AgentSession #2 -> Runtime #2     the second attempt
--
-- One task, two sessions: the first attempt went sideways and the second is a
-- different attempt at the same goal. Neither replaces the other, and the task
-- is what says both were for.
--
-- Why this level exists at all: a task and a process have different lifetimes.
-- If the task held the runtime id directly, restarting the terminal would either
-- overwrite the first attempt's history or force a new task for what is plainly
-- the same work. The session is where "how many attempts" lives, and it is the
-- level at which each attempt's own facts are recorded.
--
-- docs/TASK_MODEL.md §3 and §4 are the long form.

CREATE TABLE IF NOT EXISTS agent_sessions (
    -- The session's identity: "sess_" followed by 96 random bits in hex.
    id         TEXT NOT NULL PRIMARY KEY,

    -- The task this is an attempt at. Always present.
    task_id    TEXT NOT NULL,

    -- The runtime this attempt runs in, or NULL.
    --
    -- NULL is a first-class value and not a placeholder. A session is created
    -- before its runtime is - creating a session and starting a process are two
    -- different acts - and the window between the two calls is a real state
    -- that this column names rather than hides. Forcing them into one
    -- transaction would hold a write lock open across a process launch and
    -- would make the task layer a dependency of the runtime layer: a tmux that
    -- refused to start would roll back the record that somebody wanted this
    -- work done at all.
    --
    -- It is deliberately NOT a foreign key, for the same reason
    -- agent_events.runtime_id is not one. The project_runtime row is deleted
    -- when a runtime is destroyed, and "this attempt happened, and this is the
    -- runtime it ran in" has to outlive it. A cascade there would delete the
    -- record of the attempt along with the process it used.
    --
    -- In this build the value is the runtime's session name, "amx-{project_id}",
    -- which means every attempt in one project records the same runtime id even
    -- across restarts - there is one runtime per project. The column is
    -- per-session anyway, so a future backend that hosts more than one runtime
    -- per project needs no schema change.
    runtime_id TEXT,

    -- 'CREATED', 'RUNNING', 'COMPLETED', 'FAILED' or 'CANCELLED'.
    --
    -- There is no WAITING, and its absence is a decision: it is not clear what
    -- a session would be waiting for. A session whose runtime has stopped is
    -- not waiting, it is over; a session whose runtime is up and unused is idle,
    -- and idle is not a lifecycle state. WAITING belongs to the task, where
    -- being blocked on something outside the work is a real and long-lived
    -- condition.
    status     TEXT NOT NULL,

    -- When the attempt began, or NULL.
    --
    -- Set when the session enters RUNNING. It stays NULL for a session that
    -- failed before it ever ran, which is the honest reading of a runtime that
    -- never came up.
    started_at TEXT,

    -- When the attempt stopped being RUNNING, or NULL.
    ended_at   TEXT,

    created_at TEXT NOT NULL,

    -- A session without its task is not an attempt at anything, so the cascade
    -- is right here in a way it is not for the runtime above.
    --
    -- As with tasks, nothing in this build deletes a task and there is no
    -- endpoint that could. The constraint is declared so that the day something
    -- does, the database decides what happens to the attempts rather than
    -- leaving them behind. It does not touch agent_events.
    FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE CASCADE
);

-- One task's attempts, newest first. The query every consumer starts from.
CREATE INDEX IF NOT EXISTS idx_agent_sessions_task
    ON agent_sessions (task_id, created_at DESC, id DESC);

-- "Which attempts ran in this runtime" links the two levels, and is how a
-- session's events are reached: a session names its runtime, and the runtime's
-- timeline is one of the two endpoints Phase 7.1 built.
CREATE INDEX IF NOT EXISTS idx_agent_sessions_runtime
    ON agent_sessions (runtime_id, created_at DESC);

-- "What is running right now" across every task.
CREATE INDEX IF NOT EXISTS idx_agent_sessions_status
    ON agent_sessions (status, created_at DESC);

-- Ordering across tasks, which the indexes above do not provide on their own.
CREATE INDEX IF NOT EXISTS idx_agent_sessions_created
    ON agent_sessions (created_at DESC, id DESC);
