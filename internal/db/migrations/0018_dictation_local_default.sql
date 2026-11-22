-- Dictation is local first (docs/dictation.md): the default engine is the server's local
-- model, and the hosted API is used only when an admin picks it. The old default, auto, used
-- the API as soon as a key was set; it is gone, and its rows become local. A workspace that
-- wants the API picks it in Settings, so audio never leaves the server because of a key alone.
--
-- SQLite can't change a column's default in place, so the table is rebuilt. Nothing refers to
-- it, so dropping it deletes nothing else.
CREATE TABLE dictation_settings_new (
    workspace_id INTEGER PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    -- api, local, or an add-on's ID (such as desertant-voz).
    engine       TEXT NOT NULL DEFAULT 'local',
    -- An ISO 639-1 code, or '' for the engine to detect it.
    language     TEXT NOT NULL DEFAULT '',
    -- An OpenAI-compatible endpoint: POST {api_base_url}/audio/transcriptions.
    api_base_url TEXT NOT NULL DEFAULT 'https://api.openai.com/v1',
    api_model    TEXT NOT NULL DEFAULT 'whisper-1',
    -- Sealed with the server key (internal/serverkey), its associated data naming the
    -- workspace and the column. NULL: no key.
    api_key      BLOB,
    -- The add-on whose notice (licence check, installs) an admin confirmed, by whom and
    -- when; choosing an add-on as the engine needs it. NULL: none.
    addon_notice TEXT,
    notice_by    INTEGER REFERENCES users (id) ON DELETE SET NULL,
    notice_at    TEXT,
    updated_by   INTEGER REFERENCES users (id) ON DELETE SET NULL,
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

INSERT INTO dictation_settings_new (workspace_id, engine, language, api_base_url, api_model, api_key,
        addon_notice, notice_by, notice_at, updated_by, updated_at)
    SELECT workspace_id, CASE engine WHEN 'auto' THEN 'local' ELSE engine END, language, api_base_url,
        api_model, api_key, addon_notice, notice_by, notice_at, updated_by, updated_at
    FROM dictation_settings;
DROP TABLE dictation_settings;
ALTER TABLE dictation_settings_new RENAME TO dictation_settings;
