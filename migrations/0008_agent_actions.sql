-- AgentMux agent action queue (Phase 7.3C-2).
--
-- What a person can do about an attempt, recorded as a pending item.
--
--     agent_attention   whether anybody needs to look
--     agent_actions     what they might do about it
--
-- An attention row is a single level, overwritten by the next thing that
-- happens. An action is a row that stays: it is raised once, and it is there
-- until something resolves it. That is the difference between the two, and it
-- is why they are two tables rather than one row with a list in it.
--
-- # What an action is not
--
-- It is not a request to AgentMux and it is not a command. Raising a
-- PERMISSION_REQUEST action records that Claude asked for something; it does
-- **not** answer it, does not influence it, and does not carry a decision
-- anywhere. Nothing in this build can allow or deny anything, and this table is
-- where that would start - which is why it does not.
--
-- Like the attention it sits beside, this is derived and can be thrown away and
-- rebuilt from agent_events at any moment.

CREATE TABLE IF NOT EXISTS agent_actions (
    -- The action's identity.
    --
    -- It is derived from the event that raised it, so raising the same action
    -- twice is the same row: a rebuild, or an event delivered twice, cannot
    -- produce two. That is what makes the queue idempotent without a uniqueness
    -- rule on anything else - and it is why nothing here needs a timestamp
    -- comparison the way agent_states does.
    id               TEXT NOT NULL PRIMARY KEY,

    -- The attempt this is about.
    agent_session_id TEXT NOT NULL,

    -- The project the attempt belongs to, stored for the reason agent_attention
    -- stores it: the listing is per project.
    project_id       TEXT NOT NULL,

    -- One of PERMISSION_REQUEST, VIEW_FAILURE, VIEW_COMPLETION.
    --
    -- Every one of them is something a person *looks at*. None of them is
    -- something AgentMux does: there is no ALLOW, no DENY and no EXECUTE, and
    -- their absence is the boundary this phase stops at.
    type             TEXT NOT NULL,

    -- One of PENDING, RESOLVED, EXPIRED.
    status           TEXT NOT NULL,

    -- A short fixed phrase, for example "permission requested".
    --
    -- Fixed text, never a quotation from a payload. See agent_attention.reason.
    reason           TEXT NOT NULL,

    created_at       TEXT NOT NULL,

    -- When the action stopped being pending, or NULL.
    --
    -- NULL is the ordinary value and it is not a placeholder: an action that is
    -- still waiting is exactly the row a client is looking for, and a sentinel
    -- timestamp would make "still pending" something a reader had to know how
    -- to recognise.
    resolved_at      TEXT
);

-- One project's queue, pending first and then newest first.
CREATE INDEX IF NOT EXISTS idx_agent_actions_project
    ON agent_actions (project_id, status, created_at DESC, id DESC);

-- One attempt's actions, which is how the projection resolves them.
CREATE INDEX IF NOT EXISTS idx_agent_actions_session
    ON agent_actions (agent_session_id, status, created_at DESC);
