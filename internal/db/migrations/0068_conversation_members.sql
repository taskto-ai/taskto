-- Sharing a conversation (docs/api.md#sharing-a-conversation): its owner lets chosen members of
-- the workspace read it. A viewer sees the messages and the agent's work as they stream and
-- changes nothing; a participant is the role a later step lets post. The owner is never a row here.
--
-- A row only counts while its user is a member of the workspace and not disabled, and while the
-- owner still is (internal/conversations, Service.reach). Leaving the workspace removes the
-- person's rows for its conversations, so rejoining never brings a share back.
CREATE TABLE conversation_members (
    conversation_id INTEGER NOT NULL REFERENCES conversations (id) ON DELETE CASCADE,
    user_id         INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role            TEXT NOT NULL CHECK (role IN ('viewer', 'participant')),
    added_by        INTEGER REFERENCES users (id) ON DELETE SET NULL,
    added_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (conversation_id, user_id)
) STRICT;

CREATE INDEX conversation_members_by_user ON conversation_members (user_id);

CREATE TRIGGER conversation_members_not_owner
BEFORE INSERT ON conversation_members
WHEN NEW.user_id = (SELECT owner_id FROM conversations WHERE id = NEW.conversation_id)
BEGIN
    SELECT RAISE(ABORT, 'the owner of a conversation is not one of its members');
END;

CREATE TRIGGER conversation_members_leave_workspace
AFTER DELETE ON memberships
BEGIN
    DELETE FROM conversation_members
    WHERE user_id = OLD.user_id
      AND conversation_id IN (SELECT id FROM conversations WHERE workspace_id = OLD.workspace_id);
END;
