-- AgentMux agent attention (Phase 7.3C-2).
--
-- How much a person needs to care about one attempt, right now.
--
--     agent_events      what happened, append-only, forever
--     agent_states      what the agent is doing
--     agent_attention   whether anybody needs to look
--
-- Three readings of the same log, and they are deliberately three rather than
-- one. An agent that is RUNNING and an agent that is WAITING_PERMISSION are
-- both working; only one of them needs a person. Attention is that difference,
-- and a schema that folded it into the status would make "what is it doing" and
-- "does anybody need to act" the same question, which they are not.
--
-- Like the state, this is derived and can be thrown away and rebuilt from
-- agent_events at any moment. Nothing here is a source of truth.

CREATE TABLE IF NOT EXISTS agent_attention (
    -- The attempt this is the attention of: a row in agent_sessions, and the
    -- same key agent_states uses.
    --
    -- It is deliberately not a foreign key, for the reason 0006 gives: an
    -- attention row is derived from the event log, and the log names a runtime
    -- and a project and never an attempt. The attempt is resolved from the log,
    -- and a rebuild must not depend on the task model still holding the row.
    agent_session_id TEXT NOT NULL PRIMARY KEY,

    -- The project the attempt belongs to.
    --
    -- It is derivable - an attempt names a task, a task names a project - and it
    -- is stored anyway, for the reason agent_events stores its runtime id: the
    -- listing a workspace asks for is "everything in this project that needs
    -- me", and answering it by joining through two tables would make a rule
    -- that changed later change the meaning of rows already written.
    project_id       TEXT NOT NULL,

    -- One of NONE, ACTION_REQUIRED, WARNING, INFO.
    --
    -- These are about the *person*, not about the agent. NONE means there is
    -- nothing to see; ACTION_REQUIRED means something is blocked until somebody
    -- does something; WARNING means it went wrong; INFO means it is worth
    -- knowing. A level is a judgement about urgency, and it is the only
    -- judgement this layer makes.
    level            TEXT NOT NULL,

    -- A short fixed phrase saying why, for example "permission requested".
    --
    -- It is fixed text and never a quotation. The event it was derived from may
    -- carry a prompt or a tool input, and none of that reaches this column -
    -- docs/AGENT_ATTENTION.md §7. It is stored rather than derived from the
    -- level at read time so that a row says what it means on its own, without
    -- the reader having to hold a mapping table.
    reason           TEXT NOT NULL,

    -- When this row was last written.
    updated_at       TEXT NOT NULL
);

-- One project's attention, which is what a workspace asks for.
CREATE INDEX IF NOT EXISTS idx_agent_attention_project
    ON agent_attention (project_id, updated_at DESC, agent_session_id DESC);

-- "Everything that needs somebody", across every project.
CREATE INDEX IF NOT EXISTS idx_agent_attention_level
    ON agent_attention (level, updated_at DESC);
