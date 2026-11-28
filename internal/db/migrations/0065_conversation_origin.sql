ALTER TABLE conversations ADD COLUMN origin TEXT;
CREATE UNIQUE INDEX conversations_by_origin ON conversations (origin);
ALTER TABLE conversations ADD COLUMN origin_kind TEXT
    CHECK (origin_kind IS NULL OR origin_kind IN ('app', 'routine'));
ALTER TABLE conversations ADD COLUMN origin_space_id INTEGER;
ALTER TABLE conversations ADD COLUMN origin_app_id INTEGER;
ALTER TABLE conversations ADD COLUMN origin_routine_id INTEGER;
ALTER TABLE conversations ADD COLUMN origin_submission INTEGER;

CREATE TRIGGER conversations_origin_consistent
BEFORE INSERT ON conversations
WHEN (NEW.origin IS NULL) != (NEW.origin_kind IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'a conversation origin needs a kind');
END;

DROP TRIGGER conversations_fixed_identity;

CREATE TRIGGER conversations_fixed_identity
BEFORE UPDATE OF workspace_id, owner_id, agent_id, origin, origin_kind, origin_space_id, origin_app_id, origin_routine_id ON conversations
BEGIN
    SELECT RAISE(ABORT, 'a conversation''s workspace, owner, agent conversation and origin cannot change');
END;
