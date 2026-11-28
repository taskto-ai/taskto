CREATE TABLE agent_usage_daily (
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace_id INTEGER NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    day TEXT NOT NULL CHECK (length(day) = 10),
    source TEXT NOT NULL CHECK (source IN ('cli', 'agent')),
    engine TEXT NOT NULL CHECK (length(engine) BETWEEN 1 AND 64),
    input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    turns INTEGER NOT NULL DEFAULT 0 CHECK (turns >= 0),
    PRIMARY KEY (user_id, workspace_id, day, source, engine)
) STRICT;

CREATE TABLE agent_usage_turns (
    user_id INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    turn TEXT NOT NULL CHECK (length(turn) BETWEEN 1 AND 128),
    day TEXT NOT NULL CHECK (length(day) = 10),
    PRIMARY KEY (user_id, turn)
) STRICT, WITHOUT ROWID;

ALTER TABLE conversations ADD COLUMN usage_mark INTEGER;
