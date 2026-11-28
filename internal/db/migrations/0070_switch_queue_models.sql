-- A model provider's model as an entry of the switching queue (docs/account-switching.md): when
-- every CLI account of the queue is limited, the conversation goes on there. An entry is exactly one
-- of an own account, a shared account or a model.
CREATE TABLE cli_switch_queue_new (
    user_id           INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    position          INTEGER NOT NULL,
    account_id        INTEGER REFERENCES cli_accounts (id) ON DELETE CASCADE,
    shared_account_id INTEGER REFERENCES cli_shared_accounts (id) ON DELETE CASCADE,
    provider          TEXT,
    model_id          TEXT,
    PRIMARY KEY (user_id, position),
    CHECK ((provider IS NULL) = (model_id IS NULL)),
    CHECK ((account_id IS NOT NULL) + (shared_account_id IS NOT NULL) + (provider IS NOT NULL) = 1)
) STRICT;

INSERT INTO cli_switch_queue_new (user_id, position, account_id, shared_account_id)
    SELECT user_id, position, account_id, shared_account_id FROM cli_switch_queue;

DROP TABLE cli_switch_queue;
ALTER TABLE cli_switch_queue_new RENAME TO cli_switch_queue;

CREATE UNIQUE INDEX cli_switch_queue_account ON cli_switch_queue (user_id, account_id);
CREATE UNIQUE INDEX cli_switch_queue_shared ON cli_switch_queue (user_id, shared_account_id);
CREATE UNIQUE INDEX cli_switch_queue_model ON cli_switch_queue (user_id, provider, model_id);
