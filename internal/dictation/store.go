package dictation

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"taskto.ai/taskto/internal/audit"
)

// The values of Settings.Engine besides add-on IDs. Local is the default: the hosted API is
// used only when an admin picks it, never because a key is set (docs/dictation.md).
const (
	EngineAPI   = "api"
	EngineLocal = "local"
)

// The API engine's defaults: OpenAI's endpoint and its Whisper model.
const (
	defaultBaseURL = "https://api.openai.com/v1"
	defaultModel   = "whisper-1"
)

// settings is a workspace's row, with the API key still sealed.
type settings struct {
	engine, language string
	baseURL, model   string
	sealed           []byte
	notice           string // the add-on whose notice an admin confirmed, or ""
	updatedAt        string
}

func defaults() settings {
	return settings{engine: EngineLocal, baseURL: defaultBaseURL, model: defaultModel}
}

// keyAD binds a sealed key to its workspace's row, so a sealed value copied elsewhere doesn't open.
func keyAD(workspaceID int64) []byte {
	return []byte("taskto dictation_settings " + strconv.FormatInt(workspaceID, 10) + " api_key")
}

func (s *Service) load(ctx context.Context, workspaceID int64) (settings, error) {
	st := defaults()
	var notice sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT engine, language, api_base_url, api_model, api_key, addon_notice, updated_at
		FROM dictation_settings WHERE workspace_id = ?`, workspaceID).
		Scan(&st.engine, &st.language, &st.baseURL, &st.model, &st.sealed, &notice, &st.updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return defaults(), nil
	}
	st.notice = notice.String
	return st, err
}

// apiKey returns the workspace's key in clear, or "" without one.
func (s *Service) apiKey(workspaceID int64, st settings) (string, error) {
	if st.sealed == nil {
		return "", nil
	}
	plain, err := s.key.Open(st.sealed, keyAD(workspaceID))
	return string(plain), err
}

// save writes st for workspaceID. newKey, if not nil, replaces the stored key ("" removes
// it); the clear key only lives between here and Seal. noticeBy is set when st.notice is
// newly confirmed.
func (s *Service) save(ctx context.Context, workspaceID, userID int64, st settings, newKey *string, noticeBy bool) error {
	sealed := st.sealed
	if newKey != nil {
		sealed = nil
		if *newKey != "" {
			var err error
			if sealed, err = s.key.Seal([]byte(*newKey), keyAD(workspaceID)); err != nil {
				return err
			}
		}
	}
	var notice any
	if st.notice != "" {
		notice = st.notice
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // after a commit, a no-op
	if _, err := tx.ExecContext(ctx, `INSERT INTO dictation_settings
			(workspace_id, engine, language, api_base_url, api_model, api_key, addon_notice, notice_by, notice_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, CASE WHEN ? THEN ? END, CASE WHEN ? THEN strftime('%Y-%m-%dT%H:%M:%fZ', 'now') END, ?)
		ON CONFLICT (workspace_id) DO UPDATE SET
			engine = excluded.engine, language = excluded.language,
			api_base_url = excluded.api_base_url, api_model = excluded.api_model, api_key = excluded.api_key,
			addon_notice = excluded.addon_notice,
			notice_by = CASE WHEN ? THEN excluded.notice_by WHEN excluded.addon_notice IS NULL THEN NULL ELSE notice_by END,
			notice_at = CASE WHEN ? THEN excluded.notice_at WHEN excluded.addon_notice IS NULL THEN NULL ELSE notice_at END,
			updated_by = excluded.updated_by,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
		workspaceID, st.engine, st.language, st.baseURL, st.model, sealed, notice,
		noticeBy, userID, noticeBy, userID, noticeBy, noticeBy); err != nil {
		return err
	}
	// The key is only said to have changed, and to be set; the address is kept as its host.
	if err := audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Actor: userID, Action: audit.DictationSettingsChanged,
		TargetType: audit.TargetWorkspace, Target: workspaceID, Details: audit.Details{
			"engine": st.engine, "language": st.language, "api_model": st.model, "base_host": audit.Host(st.baseURL),
			"api_key_changed": newKey != nil, "api_key_set": sealed != nil, "notice_confirmed": noticeBy,
		}}); err != nil {
		return err
	}
	return tx.Commit()
}
