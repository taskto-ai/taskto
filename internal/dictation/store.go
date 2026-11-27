package dictation

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"taskto.ai/taskto/internal/audit"
)

const (
	EngineAPI   = "api"
	EngineLocal = "local"
)

const (
	defaultBaseURL = "https://api.openai.com/v1"
	defaultModel   = "whisper-1"
)

type settings struct {
	engine, language string
	baseURL, model   string
	sealed           []byte
	notice           string
	updatedAt        string
}

func defaults() settings {
	return settings{engine: EngineLocal, baseURL: defaultBaseURL, model: defaultModel}
}

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

func (s *Service) apiKey(workspaceID int64, st settings) (string, error) {
	if st.sealed == nil {
		return "", nil
	}
	plain, err := s.key.Open(st.sealed, keyAD(workspaceID))
	return string(plain), err
}

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
	defer func() { _ = tx.Rollback() }()
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
	details := audit.Details{"engine": st.engine, "api_key_changed": newKey != nil, "api_key_set": sealed != nil, "notice_confirmed": noticeBy}
	if st.language != "" {
		details["language"] = st.language
	}
	if h := audit.Host(st.baseURL); h != "" {
		details["base_host"] = h
	}
	if err := audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Actor: userID, Action: audit.DictationSettingsChanged,
		TargetType: audit.TargetWorkspace, Target: workspaceID, Details: details}); err != nil {
		return err
	}
	return tx.Commit()
}
