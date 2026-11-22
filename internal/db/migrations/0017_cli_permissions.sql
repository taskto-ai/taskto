-- Permission prompts in conversations on a CLI worker (docs/agent-safety.md): "ask", the
-- default, has the CLI ask the conversation's owner before it runs a command, edits a file or
-- fetches a page; "allow_all", which only the owner sets, for this conversation alone, asks
-- nothing.
ALTER TABLE conversations ADD COLUMN cli_permissions TEXT NOT NULL DEFAULT 'ask'
    CHECK (cli_permissions IN ('ask', 'allow_all'));
