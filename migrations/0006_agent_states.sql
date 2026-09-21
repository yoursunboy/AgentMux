-- AgentMux agent state projection (Phase 7.3C-1).
--
-- A projection of the event log: what is true about an attempt now, computed
-- from what is recorded about what happened.
--
--     agent_events      what happened, append-only, forever
--     agent_states      what that adds up to, one row per attempt, overwritten
--
-- Neither replaces the other and neither is derived at read time. The events
-- are the history and are never rewritten; this table is the current answer and
-- can be thrown away and rebuilt from them at any moment - see
-- docs/AGENT_STATE.md §4.
--
-- Why it is a table rather than a computation. Reading a state by replaying a
-- project's whole event log would make every read cost the whole history, and
-- the cost would grow with the age of the installation rather than with what is
-- being asked. The projection pays it once, at write time, where it is one row
-- per event.
--
-- Why it is not a column on agent_sessions. The attempt is the task model's
-- record of an attempt; this is what the agent reported about it. They have
-- different sources, different lifetimes and different owners, and a schema
-- that merged them would make the task model depend on the event vocabulary.
-- docs/AGENT_STATE.md §1 draws the line.

CREATE TABLE IF NOT EXISTS agent_states (
    -- The attempt this is the state of: a row in agent_sessions.
    --
    -- It is the key, and it is deliberately NOT a foreign key. The state is a
    -- projection of the event log, and an event names a runtime and a project
    -- but never an attempt; the attempt is resolved from the log itself, by way
    -- of the session events that carry both. A foreign key would make the
    -- projection fail on an event whose attempt the task model has since
    -- forgotten, which is exactly the case a projection has to survive - and
    -- it would make the task tables a precondition of rebuilding from events,
    -- which is the one thing a rebuild must not require.
    agent_session_id TEXT NOT NULL PRIMARY KEY,

    -- The project the attempt belongs to. Always present, and the scope every
    -- listing starts from.
    project_id       TEXT NOT NULL,

    -- The runtime the attempt ran in, or NULL.
    --
    -- NULL until the attempt is bound to a runtime, which is a real state and
    -- not a placeholder: an attempt is created before its runtime is, and the
    -- window between the two is the same window agent_sessions.runtime_id
    -- describes. It is also how an agent event - which names a runtime and no
    -- attempt - finds the row it belongs to.
    runtime_id       TEXT,

    -- One of CREATED, RUNNING, WAITING_INPUT, WAITING_PERMISSION, COMPLETED,
    -- FAILED, STOPPED.
    --
    -- These are the agent's own states and they are deliberately not the
    -- runtime's or the task's. Three layers, three vocabularies: a runtime is
    -- RUNNING whenever its terminal exists, a task is COMPLETED when a person
    -- says so, and an agent is WAITING_PERMISSION when it asked for something.
    -- A single status column shared between them would be the end of all three.
    status           TEXT NOT NULL,

    -- The type of the last event that was projected into this row.
    --
    -- It is what makes the state explainable: a row that says RUNNING can say
    -- which fact last put it there. It is a type and never a payload - the
    -- payload is where a prompt or a tool input would be, and docs/AGENT_STATE.md
    -- §7 is the rule.
    last_event       TEXT NOT NULL,

    -- When that event happened, as the event recorded it.
    --
    -- The projection is guarded on this: an event older than the one already
    -- applied is ignored, which is what makes replay idempotent and what makes
    -- two events arriving at once resolve to the same answer in either order.
    last_event_at    TEXT NOT NULL,

    -- When this row was last written, by this server's clock.
    updated_at       TEXT NOT NULL
);

-- One project's agent states, which is what a workspace panel asks for.
CREATE INDEX IF NOT EXISTS idx_agent_states_project
    ON agent_states (project_id, updated_at DESC, agent_session_id DESC);

-- "What is the agent in this runtime doing", which is also the lookup an agent
-- event uses to find its attempt - the event names a runtime and nothing else.
CREATE INDEX IF NOT EXISTS idx_agent_states_runtime
    ON agent_states (runtime_id, last_event_at DESC);

-- "Everything waiting on a permission", across every project.
CREATE INDEX IF NOT EXISTS idx_agent_states_status
    ON agent_states (status, updated_at DESC);
