-- Card links to conversations (card_links, migration 0005), corrected. A conversation link's
-- target is the conversation API's ID (conversations.id, in decimal), never the agent
-- runtime's (conversations.agent_id), which nothing outside the server ever sees. 0005's
-- comment says "by the agent runtime's ID": that was wrong, and a merged migration is never
-- edited, so this one says it instead.
--
-- Conversations are private to their owner, so a link is checked when it is added
-- (internal/boards): it must name a conversation of the user adding it, or of the user the
-- agent works for, in the board's workspace. Anyone who may edit the card may keep or remove
-- it. Following a link is the conversation API, which opens a conversation only for its
-- owner. A link still isn't a foreign key: it stays when its conversation goes.

-- Links that aren't an ID can't be followed; the API refuses them now, and they would make
-- every later edit of the card's links fail.
DELETE FROM card_links
WHERE kind = 'conversation'
  AND NOT (target GLOB '[1-9]*' AND target NOT GLOB '*[^0-9]*' AND length(target) <= 19);

-- One spelling per conversation: decimal, without a sign or leading zeros.
CREATE TRIGGER card_links_conversation_id
BEFORE INSERT ON card_links
WHEN NEW.kind = 'conversation'
  AND NOT (NEW.target GLOB '[1-9]*' AND NEW.target NOT GLOB '*[^0-9]*' AND length(NEW.target) <= 19)
BEGIN
    SELECT RAISE(ABORT, 'a conversation link is a conversation''s ID');
END;
