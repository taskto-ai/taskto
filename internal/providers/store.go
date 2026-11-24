package providers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
			TargetType: audit.TargetProvider, Target: id, Details: audit.Details{
				"name": d.name, "kind": d.kind, "scope": string(o.scope()), "base_host": audit.Host(d.baseURL), "api_key_set": d.apiKey != nil,
			}})
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
			TargetType: audit.TargetProvider, Target: id, Details: audit.Details{
				"name": old.Name, "kind": old.Kind, "scope": string(o.scope()), "fields": fields,
				"base_host": audit.Host(d.baseURL), "api_key_changed": keyChanged,
			}})
	})
	return rec, err
}

// remove deletes o's account id.
func (s *Store) remove(ctx context.Context, o owner, id int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var name, kind string
		err := tx.QueryRowContext(ctx, `DELETE FROM provider_accounts WHERE id = ? AND `+ownerCond+` RETURNING name, kind`,
			append([]any{id}, o.args()...)...).Scan(&name, &kind)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: o.workspace, Action: audit.ProviderAccountRemoved,
			TargetType: audit.TargetProvider, Target: id, Details: audit.Details{"name": name, "kind": kind, "scope": string(o.scope())}})
	})
}

func nullJSON(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}
