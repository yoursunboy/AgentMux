-- AgentMux per-project launch settings (Phase 7.5.1).
--
-- One row per project, holding what a project's *next* agent launch should be
-- told. A project with no row is a project nobody has configured, and it
-- launches under the default rather than being an error.
--
-- # Why a table rather than a column on `projects`
--
-- `projects` is the registry: which directories AgentMux manages, and where
-- each one sits in the workspace. Every listing in this build reads all of it,
-- and the one mutable column it carries - `pinned_slot` - is mutable precisely
-- because every listing wants it.
--
-- A setting is the opposite shape. Two readers want it: the launch path, which
-- reads one project at a time, and the console, which reads all of them but
-- joins none of them. Putting it on `projects` would add a column to the row
-- every query already reads in order to answer a question most of them are not
-- asking, and it would make the next setting a migration on the busiest table
-- in the schema.
--
-- # Why the mode is stored at all
--
-- Claude cycles its permission mode on Shift+Tab, inside its own TUI, and the
-- only thing that knows which mode resulted is the process drawing that TUI.
-- No hook fires on a Shift+Tab, so nothing AgentMux can observe reports it.
-- web/src/dashboard/ModeSwitch.tsx says the same thing from the other end: a
-- control that asked to be told the mode in force would be drawing a guess.
--
-- So this table does not record the mode in force. It records the mode a
-- *launch* is configured to start in, which is a fact AgentMux owns because it
-- is AgentMux that types the command line. That is the whole of the difference
-- between this and a runtime switch, and it is why there is no column here for
-- "the mode now".
--
-- # What this table deliberately is not
--
-- It is not a record of what any launch was told. A mode changed while an agent
-- is running has not reached that agent - the process keeps the mode it started
-- with - and nothing here claims otherwise. The console says so in as many
-- words rather than leaving a person to discover it.
--
-- It is not per session and not per attempt. A project is the unit a person
-- configures, and a mode that had to be chosen again for every attempt would be
-- a decision nobody wants to make twice.

CREATE TABLE IF NOT EXISTS project_settings (
    -- The project this configures, and the whole of its identity.
    --
    -- It is the primary key because there is exactly one settings row per
    -- project, and a surrogate id would let a second row exist without anything
    -- noticing. Every read names this column, which is why nothing below adds
    -- an index: the primary key already is one.
    --
    -- It is also a foreign key, declared at the foot of the table where the
    -- argument for it is.
    project_id      TEXT NOT NULL PRIMARY KEY,

    -- The value of `--permission-mode` for this project's next launch.
    --
    -- One of `manual`, `acceptEdits`, `bypassPermissions`.
    --
    -- It is TEXT and not a CHECK constraint, because the vocabulary lives in
    -- internal/claude - it is the CLI's own set of values, and the CLI is where
    -- it is documented - and a constraint here would be a second copy of it
    -- that could drift. The vocabulary is enforced where the row is written,
    -- and a value that is not one of the three is refused there rather than
    -- stored and discovered here. 0009_usage_events.sql takes the same position
    -- for its own vocabulary.
    permission_mode TEXT NOT NULL,

    -- When the row was first written, RFC 3339 UTC with nanoseconds.
    created_at      TEXT NOT NULL,

    -- When the row was last written.
    --
    -- It is the write time and not the change time: a save that sets the mode a
    -- project already has still moves it. Making it mean "when did this last
    -- differ" would cost a read before every write to decide whether to write,
    -- and no reader acts on the difference.
    updated_at      TEXT NOT NULL,

    -- A setting is not a thing that survives its project.
    --
    -- Every other table that belongs to a project - project_runtime,
    -- agent_events, tasks - declares this same constraint, and for the same
    -- reason: settings for a project that does not exist are unreachable
    -- through every endpoint this API has, so a row left behind would be a row
    -- nothing could read. Nothing in this build deletes a project and there is
    -- no endpoint that could; the constraint is here so that the day something
    -- does, this decides the question rather than leaving rows behind.
    FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
);
