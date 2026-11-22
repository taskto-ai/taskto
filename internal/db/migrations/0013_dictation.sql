-- Dictation: each workspace's speech-to-text settings (internal/dictation, docs/dictation.md).
-- A workspace without a row has the defaults: engine auto, language detected, no API key.

CREATE TABLE dictation_settings (
    workspace_id INTEGER PRIMARY KEY REFERENCES workspaces (id) ON DELETE CASCADE,
    -- auto, api, local, or an add-on's ID (such as desertant-voz).
    engine       TEXT NOT NULL DEFAULT 'auto',
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
