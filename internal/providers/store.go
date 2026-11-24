package providers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/serverkey"
)

var (
	errNotFound = errors.New("provider account not found")
	errTaken    = errors.New("an account with this name exists")
	errTooMany  = errors.New("too many provider accounts")
)

// owner is whose accounts these are: a workspace's or a user's. Exactly one ID is set.
type owner struct {
	workspace int64
	user      int64
}

func (o owner) scope() Scope {
	if o.workspace != 0 {
		return ScopeWorkspace
	}
	return ScopeUser
}

func (o owner) id() int64 { return o.workspace + o.user }

// ownerCond selects an owner's rows, with args: workspace_id IS NULL for a user's, and
// user_id IS NULL for a workspace's.
const ownerCond = `workspace_id IS ? AND user_id IS ?`

func (o owner) args() []any {
	var ws, user any
	if o.workspace != 0 {
		ws = o.workspace
	} else {
		user = o.user
	}
	return []any{ws, user}
}

// record is a stored account, with its sealed key.
type record struct {
	Account
	owner  owner
	sealed []byte
}

// Store keeps provider accounts in the database, their keys sealed with the server key.
type Store struct {
	db  *sql.DB
	key *serverkey.Key
	now func() time.Time
}

// NewStore returns the store backed by sqlDB, which must be migrated, sealing keys with key.
func NewStore(sqlDB *sql.DB, key *serverkey.Key) *Store {
	return &Store{db: sqlDB, key: key, now: time.Now}
}

// keyAD binds a sealed key to its row, so a sealed value copied to another account doesn't open.
func keyAD(id int64) []byte {
	return []byte("taskto provider_accounts " + strconv.FormatInt(id, 10) + " api_key")
}

// apiKey returns the account's key in clear, or "" if it has none.
func (s *Store) apiKey(rec record) (string, error) {
	if rec.sealed == nil {
		return "", nil
	}
	plain, err := s.key.Open(rec.sealed, keyAD(rec.ID))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// timeFormat matches strftime('%Y-%m-%dT%H:%M:%fZ'), the schema's default.
const timeFormat = "2006-01-02T15:04:05.000Z"

const columns = `id, workspace_id, user_id, name, display_name, kind, base_url, api_key, compat, models, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scan(row scanner) (record, error) {
	var rec record
	var ws, user sql.NullInt64
	var compat sql.NullString
	var models string
	if err := row.Scan(&rec.ID, &ws, &user, &rec.Name, &rec.DisplayName, &rec.Kind, &rec.BaseURL, &rec.sealed,
		&compat, &models, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return record{}, err
	}
	rec.owner = owner{workspace: ws.Int64, user: user.Int64}
	rec.Scope = rec.owner.scope()
	rec.Provider = providerID(rec.Scope, rec.owner.id(), rec.Name)
	rec.APIKey = keyNotSet
	if rec.sealed != nil {
		rec.APIKey = keySet
	}
	if compat.Valid {
		rec.Compat = json.RawMessage(compat.String)
	}
	if err := json.Unmarshal([]byte(models), &rec.Models); err != nil {
		return record{}, err
	}
	return rec, nil
}

func collect(rows *sql.Rows) ([]record, error) {
	defer rows.Close()
	recs := []record{}
	for rows.Next() {
		rec, err := scan(rows)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
	}
	return recs, rows.Err()
}

// list returns the owner's accounts, oldest first.
func (s *Store) list(ctx context.Context, o owner) ([]record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM provider_accounts WHERE `+ownerCond+` ORDER BY id`, o.args()...)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

// all returns every account: workspaces' first, then users', each oldest first.
func (s *Store) all(ctx context.Context) ([]record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM provider_accounts ORDER BY user_id IS NOT NULL, id`)
	if err != nil {
		return nil, err
	}
	return collect(rows)
}

// write runs fn in a transaction, which takes the write lock when it begins (see package db).
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); !errors.Is(rbErr, sql.ErrTxDone) {
				err = errors.Join(err, rbErr)
			}
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func get(ctx context.Context, tx *sql.Tx, o owner, id int64) (record, error) {
	rec, err := scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM provider_accounts WHERE id = ? AND `+ownerCond,
		append([]any{id}, o.args()...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return record{}, errNotFound
	}
	return rec, err
}

// create adds an account for o, created by user. d must be validated.
func (s *Store) create(ctx context.Context, o owner, d draft, user int64) (record, error) {
	var rec record
	err := s.write(ctx, func(tx *sql.Tx) error {
		var count int
		var taken bool
		if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(name = ?), 0) > 0 FROM provider_accounts WHERE `+ownerCond,
			append([]any{d.name}, o.args()...)...).Scan(&count, &taken); err != nil {
			return err
		}
		if taken {
			return errTaken
		}
		if count >= maxAccounts {
			return errTooMany
		}
		models, err := json.Marshal(d.models)
		if err != nil {
			return err
		}
		owners := o.args()
		res, err := tx.ExecContext(ctx, `INSERT INTO provider_accounts
			(workspace_id, user_id, name, display_name, kind, base_url, compat, models, created_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			owners[0], owners[1], d.name, d.displayName, d.kind, d.baseURL, nullJSON(d.compat), string(models), user)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		// The key is sealed for the row's ID, which only exists now.
		if d.apiKey != nil {
			sealed, err := s.key.Seal([]byte(*d.apiKey), keyAD(id))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE provider_accounts SET api_key = ? WHERE id = ?`, sealed, id); err != nil {
				return err
			}
		}
		rec, err = get(ctx, tx, o, id)
		if err != nil {
			return err
		}
		// A personal account belongs to no workspace: its event has none either. The key is only
		// said to be set; the address is kept as its host, which a URL's credentials never are.
		return audit.Record(ctx, tx, audit.Event{Workspace: o.workspace, Actor: user, Action: audit.ProviderAccountCreated,
			TargetType: audit.TargetProvider, Target: id, Details: details(d.kind, o, d.baseURL, audit.Details{"api_key_set": d.apiKey != nil})})
	})
	return rec, err
}

// update changes o's account id: change gets the stored settings (without the key) and
// edits them; the result is validated and saved.
func (s *Store) update(ctx context.Context, o owner, id int64, change func(*draft)) (record, error) {
	var rec record
	err := s.write(ctx, func(tx *sql.Tx) error {
		old, err := get(ctx, tx, o, id)
		if err != nil {
			return err
		}
		d := draft{
			name: old.Name, displayName: old.DisplayName, kind: old.Kind, baseURL: old.BaseURL,
			keepKey: old.sealed != nil, compat: old.Compat, models: old.Models,
		}
		change(&d)
		if err := d.validate(d.apiKey != nil || d.keepKey); err != nil {
			return err
		}
		sealed := old.sealed
		switch {
		case d.apiKey != nil:
			if sealed, err = s.key.Seal([]byte(*d.apiKey), keyAD(id)); err != nil {
				return err
			}
		case !d.keepKey:
			sealed = nil
		}
		models, err := json.Marshal(d.models)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE provider_accounts
			SET display_name = ?, base_url = ?, api_key = ?, compat = ?, models = ?, updated_at = ?
			WHERE id = ?`,
			d.displayName, d.baseURL, sealed, nullJSON(d.compat), string(models), s.now().UTC().Format(timeFormat), id); err != nil {
			return err
		}
		rec, err = get(ctx, tx, o, id)
		if err != nil {
			return err
		}
		oldModels, err := json.Marshal(old.Models)
		if err != nil {
			return err
		}
		keyChanged := d.apiKey != nil || (!d.keepKey && old.sealed != nil)
		fields := audit.Changed(map[string]bool{
			"display_name": old.DisplayName != d.displayName,
			"base_url":     old.BaseURL != d.baseURL,
			"api_key":      keyChanged,
			"compat":       !bytes.Equal(old.Compat, d.compat),
			"models":       string(oldModels) != string(models),
		})
		return audit.Record(ctx, tx, audit.Event{Workspace: o.workspace, Action: audit.ProviderAccountUpdated,
			TargetType: audit.TargetProvider, Target: id, Details: details(old.Kind, o, d.baseURL, audit.Details{"fields": fields, "api_key_changed": keyChanged})})
	})
	return rec, err
}

// details are the details of an account's event: what kind of account and where it is, which
// never holds a name a person typed. The address is its host alone, left out when it has none.
func details(kind string, o owner, baseURL string, more audit.Details) audit.Details {
	d := audit.Details{"kind": kind, "scope": string(o.scope())}
	if h := audit.Host(baseURL); h != "" {
		d["base_host"] = h
	}
	maps.Copy(d, more)
	return d
}

// remove deletes o's account id. A workspace's embeddings setting and its spaces' consents that
// name the account go with it (ON DELETE CASCADE), and are recorded in the same transaction.
func (s *Store) remove(ctx context.Context, o owner, id int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		settings, err := cascadedPairs(ctx, tx, `SELECT workspace_id, 0 FROM search_settings WHERE account_id = ?`, id)
		if err != nil {
			return err
		}
		consents, err := cascadedPairs(ctx, tx, `SELECT c.space_id, sp.workspace_id FROM search_embedding_consent c
			JOIN spaces sp ON sp.id = c.space_id WHERE c.account_id = ?`, id)
		if err != nil {
			return err
		}
		var kind, created string
		err = tx.QueryRowContext(ctx, `DELETE FROM provider_accounts WHERE id = ? AND `+ownerCond+` RETURNING kind, created_at`,
			append([]any{id}, o.args()...)...).Scan(&kind, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{Workspace: o.workspace, Action: audit.ProviderAccountRemoved,
			TargetType: audit.TargetProvider, Target: id, TargetCreated: created,
			Details: audit.Details{"kind": kind, "scope": string(o.scope())}}); err != nil {
			return err
		}
		for _, st := range settings {
			if err := audit.Record(ctx, tx, audit.Event{Workspace: st[0], Action: audit.SearchEmbeddingsChanged,
				TargetType: audit.TargetWorkspace, Target: st[0],
				Details: audit.Details{"enabled": false, "account": id, "cascade": true}}); err != nil {
				return err
			}
		}
		for _, c := range consents {
			if err := audit.Record(ctx, tx, audit.Event{Workspace: c[1], Action: audit.SearchConsentChanged,
				TargetType: audit.TargetSpace, Target: c[0], Details: audit.Details{"enabled": false, "cascade": true}}); err != nil {
				return err
			}
		}
		return nil
	})
}

// cascadedPairs runs a query of two ids, read before the account's delete takes its rows.
func cascadedPairs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([][2]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all [][2]int64
	for rows.Next() {
		var p [2]int64
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			return nil, err
		}
		all = append(all, p)
	}
	return all, rows.Err()
}

func nullJSON(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}
