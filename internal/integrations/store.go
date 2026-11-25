package integrations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"

	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/serverkey"
)

var (
	errNotFound = errors.New("integration not found")
	errTaken    = errors.New("an integration with this name exists")
	errTooMany  = errors.New("too many integrations")
	errInUse    = errors.New("the integration is in use")
)

// unreadableError says a sealed secret doesn't open: the server key is not the one that sealed it,
// or the value was tampered with.
type unreadableError struct{ secret string }

func (e unreadableError) Error() string { return "the sealed " + e.secret + " can't be opened" }

const (
	maxIntegrations = 20
	// previousOverlap is how long the webhook secret a person replaced keeps verifying events:
	// the provider may still be signing with it, since it rotates its own secret with an overlap.
	previousOverlap = 24 * time.Hour
)

// Integration is an integration as the API shows it. A secret is never in it: Secrets says,
// for each, "set" or "not set".
type Integration struct {
	ID          int64             `json:"id"`
	Provider    string            `json:"provider"`
	Kind        string            `json:"kind"`
	Name        string            `json:"name"`
	DisplayName string            `json:"display_name"`
	Mode        string            `json:"mode"`
	Settings    map[string]string `json:"settings" apitypes:"nonnull"`
	// MaxAmountMinor is the admin's ceiling for one payment, in the smallest unit of its
	// currency; Currencies are the codes a payment may ask for.
	MaxAmountMinor int64             `json:"max_amount_minor"`
	Currencies     []string          `json:"currencies" apitypes:"nonnull"`
	Secrets        map[string]string `json:"secrets" apitypes:"nonnull"`
	// WebhookURL is the address to register at the provider, null when the server has no public
	// URL (payments are then found by polling).
	WebhookURL *string `json:"webhook_url"`
	Disabled   bool    `json:"disabled"`
	DisabledAt *string `json:"disabled_at"`
	// Spaces are the spaces whose apps may use the integration, by id: only the ones the caller
	// may manage (their own private space and the workspace's shared ones), never another
	// person's private space. OtherSpaces counts the rest, which the caller can neither see nor
	// change.
	Spaces      []int64 `json:"spaces" apitypes:"nonnull"`
	OtherSpaces int     `json:"other_spaces"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	LastEventAt *string `json:"last_event_at"`
}

// The values of Integration.Secrets.
const (
	secretSet    = "set"
	secretNotSet = "not set"
)

// record is a stored integration with what the API doesn't show. Its Spaces are all of them,
// whoever's: the API shows a caller only the ones they manage (Service.view).
type record struct {
	Integration
	publicID string
}

// Store keeps integrations in the database, their secrets sealed with the server key. Every
// method takes the workspace the caller belongs to and touches only its rows: an id of another
// workspace is "not found".
type Store struct {
	db  *sql.DB
	key *serverkey.Key
	now func() time.Time
}

// NewStore returns the store backed by sqlDB, which must be migrated, sealing secrets with key.
func NewStore(sqlDB *sql.DB, key *serverkey.Key) *Store {
	return &Store{db: sqlDB, key: key, now: time.Now}
}

// sealRef is what a sealed secret is bound to: the integration's workspace, row id and public id.
// Each part is checked when a secret is opened, so a sealed value copied to another row (even one
// that took a deleted row's id), another workspace or another name doesn't open.
type sealRef struct {
	workspace, id int64
	publicID      string
}

// ad is the associated data a secret called name is sealed under. None of the parts holds a ":".
func (r sealRef) ad(name string) []byte {
	return []byte("taskto integration:" + strconv.FormatInt(r.workspace, 10) + ":" + r.publicID + ":" +
		strconv.FormatInt(r.id, 10) + ":" + name)
}

func (r record) sealRef(workspace int64) sealRef {
	return sealRef{workspace: workspace, id: r.ID, publicID: r.publicID}
}

// timeFormat matches strftime('%Y-%m-%dT%H:%M:%fZ'), the schema's default.
const timeFormat = "2006-01-02T15:04:05.000Z"

func (s *Store) stamp(t time.Time) string { return t.UTC().Format(timeFormat) }

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const columns = `i.id, i.public_id, i.provider, i.name, i.display_name, i.mode, i.settings, i.max_amount_minor,
	i.currencies, i.disabled_at, i.created_at, i.updated_at, i.last_event_at`

// load reads the workspace's integrations (all of them, or the one with id), oldest first, with
// which secrets are set and which spaces are allowed.
func (s *Store) load(ctx context.Context, q querier, workspace, id int64) ([]record, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+columns+` FROM integrations i
		WHERE i.workspace_id = ? AND (? = 0 OR i.id = ?) ORDER BY i.id`, workspace, id, id)
	if err != nil {
		return nil, err
	}
	recs := []record{}
	for rows.Next() {
		var r record
		var settings, currencies string
		if err := rows.Scan(&r.ID, &r.publicID, &r.Provider, &r.Name, &r.DisplayName, &r.Mode, &settings, &r.MaxAmountMinor,
			&currencies, &r.DisabledAt, &r.CreatedAt, &r.UpdatedAt, &r.LastEventAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(settings), &r.Settings); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(currencies), &r.Currencies); err != nil {
			_ = rows.Close()
			return nil, err
		}
		r.Disabled = r.DisabledAt != nil
		if p := Lookup(r.Provider); p != nil && len(p.Kinds()) > 0 {
			r.Kind = p.Kinds()[0]
		}
		r.Secrets = map[string]string{SecretAPIKey: secretNotSet, SecretWebhookSecret: secretNotSet}
		r.Spaces = []int64{}
		recs = append(recs, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	at := map[int64]*record{}
	for i := range recs {
		at[recs[i].ID] = &recs[i]
	}
	srows, err := q.QueryContext(ctx, `SELECT s.integration_id, s.name FROM integration_secrets s
		JOIN integrations i ON i.id = s.integration_id
		WHERE i.workspace_id = ? AND (? = 0 OR i.id = ?) AND s.name IN ('api_key', 'webhook_secret')`, workspace, id, id)
	if err != nil {
		return nil, err
	}
	for srows.Next() {
		var rid int64
		var name string
		if err := srows.Scan(&rid, &name); err != nil {
			_ = srows.Close()
			return nil, err
		}
		if r := at[rid]; r != nil {
			r.Secrets[name] = secretSet
		}
	}
	if err := errors.Join(srows.Err(), srows.Close()); err != nil {
		return nil, err
	}
	prows, err := q.QueryContext(ctx, `SELECT s.integration_id, s.space_id FROM integration_spaces s
		JOIN integrations i ON i.id = s.integration_id
		WHERE i.workspace_id = ? AND (? = 0 OR i.id = ?) ORDER BY s.space_id`, workspace, id, id)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var rid, space int64
		if err := prows.Scan(&rid, &space); err != nil {
			_ = prows.Close()
			return nil, err
		}
		if r := at[rid]; r != nil {
			r.Spaces = append(r.Spaces, space)
		}
	}
	if err := errors.Join(prows.Err(), prows.Close()); err != nil {
		return nil, err
	}
	return recs, nil
}

func (s *Store) list(ctx context.Context, workspace int64) ([]record, error) {
	return s.load(ctx, s.db, workspace, 0)
}

func (s *Store) get(ctx context.Context, q querier, workspace, id int64) (record, error) {
	recs, err := s.load(ctx, q, workspace, id)
	if err != nil {
		return record{}, err
	}
	if len(recs) == 0 {
		return record{}, errNotFound
	}
	return recs[0], nil
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

// draft is a new integration, validated.
type draft struct {
	provider    Provider
	name        string
	displayName string
	settings    map[string]string
	secrets     map[string]string // clear: only between the request and sealing
	maxAmount   int64
	currencies  []string
	mode        string
}

func newPublicID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// seal stores a secret sealed for its integration and name.
func (s *Store) seal(ctx context.Context, tx *sql.Tx, ref sealRef, name, value string, expires *time.Time) error {
	sealed, err := s.key.Seal([]byte(value), ref.ad(name))
	if err != nil {
		return err
	}
	var exp any
	if expires != nil {
		exp = s.stamp(*expires)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO integration_secrets (integration_id, name, sealed, set_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (integration_id, name) DO UPDATE SET sealed = excluded.sealed, set_at = excluded.set_at, expires_at = excluded.expires_at`,
		ref.id, name, sealed, s.stamp(s.now()), exp)
	return err
}

func (s *Store) create(ctx context.Context, workspace, user int64, d draft) (record, error) {
	var rec record
	err := s.write(ctx, func(tx *sql.Tx) error {
		var count int
		var taken bool
		if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(name = ?), 0) > 0 FROM integrations WHERE workspace_id = ?`,
			d.name, workspace).Scan(&count, &taken); err != nil {
			return err
		}
		if taken {
			return errTaken
		}
		if count >= maxIntegrations {
			return errTooMany
		}
		publicID, err := newPublicID()
		if err != nil {
			return err
		}
		settings, err := json.Marshal(d.settings)
		if err != nil {
			return err
		}
		currencies, err := json.Marshal(d.currencies)
		if err != nil {
			return err
		}
		now := s.stamp(s.now())
		res, err := tx.ExecContext(ctx, `INSERT INTO integrations (workspace_id, public_id, provider, name, display_name, mode, settings,
			max_amount_minor, currencies, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			workspace, publicID, d.provider.ID(), d.name, d.displayName, d.mode, string(settings), d.maxAmount, string(currencies),
			user, now, now)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		// The secrets are sealed for the row's id, which only exists now.
		ref := sealRef{workspace: workspace, id: id, publicID: publicID}
		for name, value := range d.secrets {
			if err := s.seal(ctx, tx, ref, name, value, nil); err != nil {
				return err
			}
		}
		if rec, err = s.get(ctx, tx, workspace, id); err != nil {
			return err
		}
		_, hasWebhook := d.secrets[SecretWebhookSecret]
		return audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: user, Action: audit.IntegrationCreated,
			TargetType: audit.TargetIntegration, Target: id, Details: audit.Details{
				"provider": d.provider.ID(), "kind": rec.Kind, "mode": d.mode, "api_key_set": true, "webhook_secret_set": hasWebhook,
			}})
	})
	return rec, err
}

// change is an edit of an integration: nil or empty fields stay as they are.
type change struct {
	displayName *string
	settings    map[string]string
	secrets     map[string]string
	maxAmount   *int64
	currencies  []string
	disabled    *bool
}

// update applies c to the workspace's integration id. Everything is validated against the
// stored integration inside the transaction, so a concurrent change can't slip between a check
// and the write.
func (s *Store) update(ctx context.Context, workspace, user, id int64, c change) (record, error) {
	var rec record
	err := s.write(ctx, func(tx *sql.Tx) error {
		old, err := s.get(ctx, tx, workspace, id)
		if err != nil {
			return err
		}
		p := Lookup(old.Provider)
		if p == nil {
			return errUnknownProvider
		}
		n := old // the new state
		changed := map[string]bool{}
		if c.displayName != nil {
			name, err := checkDisplayName(*c.displayName, old.Name)
			if err != nil {
				return err
			}
			n.DisplayName = name
			changed["display_name"] = name != old.DisplayName
		}
		if c.settings != nil {
			if err := checkSettings(p, c.settings); err != nil {
				return err
			}
			n.Settings = c.settings
			changed["settings"] = !reflect.DeepEqual(old.Settings, c.settings)
		}
		for name, value := range c.secrets {
			if _, ok := secretSpec(p, name); !ok {
				return errUnknownSecret
			}
			if err := p.CheckSecret(name, value); err != nil {
				return err
			}
			changed[name] = true
		}
		if c.maxAmount != nil {
			if err := checkCeiling(*c.maxAmount); err != nil {
				return err
			}
			n.MaxAmountMinor = *c.maxAmount
			changed["max_amount"] = *c.maxAmount != old.MaxAmountMinor
		}
		if c.currencies != nil {
			list, ok := normalizeCurrencies(c.currencies)
			if !ok {
				return errBadCurrencies
			}
			n.Currencies = list
			changed["currencies"] = !slices.Equal(old.Currencies, list)
		}
		if mode := p.Mode(n.Settings, c.secrets[SecretAPIKey]); mode != "" {
			n.Mode = mode
		}

		now := s.now()
		ref := old.sealRef(workspace)
		if webhook, ok := c.secrets[SecretWebhookSecret]; ok {
			// The secret that is replaced keeps verifying for a day, so events the provider signed
			// with it before the change took effect there aren't refused.
			if err := s.keepPrevious(ctx, tx, ref, now); err != nil {
				return err
			}
			if err := s.seal(ctx, tx, ref, SecretWebhookSecret, webhook, nil); err != nil {
				return err
			}
		}
		if key, ok := c.secrets[SecretAPIKey]; ok {
			if err := s.seal(ctx, tx, ref, SecretAPIKey, key, nil); err != nil {
				return err
			}
		}
		fields := audit.Changed(changed)
		if len(fields) > 0 {
			settings, err := json.Marshal(n.Settings)
			if err != nil {
				return err
			}
			currencies, err := json.Marshal(n.Currencies)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE integrations SET display_name = ?, mode = ?, settings = ?, max_amount_minor = ?,
				currencies = ?, updated_at = ? WHERE id = ? AND workspace_id = ?`,
				n.DisplayName, n.Mode, string(settings), n.MaxAmountMinor, string(currencies), s.stamp(now), id, workspace); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: user, Action: audit.IntegrationUpdated,
				TargetType: audit.TargetIntegration, Target: id, Details: audit.Details{
					"provider": old.Provider, "mode": n.Mode, "fields": fields,
					"api_key_changed": changed[SecretAPIKey], "webhook_secret_changed": changed[SecretWebhookSecret],
				}}); err != nil {
				return err
			}
		}
		if c.disabled != nil && *c.disabled != old.Disabled {
			action := audit.IntegrationEnabled
			query := `UPDATE integrations SET disabled_at = NULL, disabled_by = NULL, updated_at = ? WHERE id = ? AND workspace_id = ?`
			args := []any{s.stamp(now), id, workspace}
			if *c.disabled {
				action = audit.IntegrationDisabled
				query = `UPDATE integrations SET disabled_at = ?, disabled_by = ?, updated_at = ? WHERE id = ? AND workspace_id = ?`
				args = []any{s.stamp(now), user, s.stamp(now), id, workspace}
			}
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: user, Action: action,
				TargetType: audit.TargetIntegration, Target: id, Details: audit.Details{"provider": old.Provider}}); err != nil {
				return err
			}
		}
		rec, err = s.get(ctx, tx, workspace, id)
		return err
	})
	return rec, err
}

// keepPrevious moves the integration's current webhook secret to the previous one, valid for
// previousOverlap, and drops a previous one that has expired. With no current secret, or one that
// doesn't open (the server key changed), there is nothing to keep.
func (s *Store) keepPrevious(ctx context.Context, tx *sql.Tx, ref sealRef, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM integration_secrets WHERE integration_id = ? AND name = ? AND expires_at <= ?`,
		ref.id, SecretWebhookSecretPrevious, s.stamp(now)); err != nil {
		return err
	}
	var sealed []byte
	err := tx.QueryRowContext(ctx, `SELECT sealed FROM integration_secrets WHERE integration_id = ? AND name = ?`,
		ref.id, SecretWebhookSecret).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	plain, err := s.key.Open(sealed, ref.ad(SecretWebhookSecret))
	if err != nil {
		return nil //nolint:nilerr // a secret that doesn't open can't be kept: the replacement stands alone
	}
	expires := now.Add(previousOverlap)
	return s.seal(ctx, tx, ref, SecretWebhookSecretPrevious, string(plain), &expires)
}

// remove deletes the workspace's integration id with its secrets and spaces. Anything that
// names it with an ON DELETE RESTRICT (an app's approval) makes the delete fail: errInUse.
func (s *Store) remove(ctx context.Context, workspace, id int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var provider, created string
		err := tx.QueryRowContext(ctx, `DELETE FROM integrations WHERE id = ? AND workspace_id = ? RETURNING provider, created_at`,
			id, workspace).Scan(&provider, &created)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return errNotFound
		case isForeignKey(err):
			return errInUse
		case err != nil:
			return err
		}
		kind := ""
		if p := Lookup(provider); p != nil && len(p.Kinds()) > 0 {
			kind = p.Kinds()[0]
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspace, Action: audit.IntegrationRemoved,
			TargetType: audit.TargetIntegration, Target: id, TargetCreated: created,
			Details: audit.Details{"provider": provider, "kind": kind}})
	})
}

// sqliteForeignKey is SQLITE_CONSTRAINT_FOREIGNKEY.
const sqliteForeignKey = 787

func isForeignKey(err error) bool {
	if err == nil {
		return false
	}
	var e *sqlite.Error
	if errors.As(err, &e) && e.Code() == sqliteForeignKey {
		return true
	}
	return strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// setSpaces replaces the spaces the integration may be used in, among the ones the caller may
// manage (manageable): the integration's spaces outside it, another person's private space, are
// kept as they are. The caller has checked that spaces are all manageable; the schema checks
// that they are in the workspace.
func (s *Store) setSpaces(ctx context.Context, workspace, user, id int64, spaces []int64, manageable []int64) (record, error) {
	var rec record
	err := s.write(ctx, func(tx *sql.Tx) error {
		old, err := s.get(ctx, tx, workspace, id)
		if err != nil {
			return err
		}
		var added, removed int64
		for _, sp := range spaces {
			if !slices.Contains(old.Spaces, sp) {
				if _, err := tx.ExecContext(ctx, `INSERT INTO integration_spaces (integration_id, space_id, allowed_by, allowed_at)
					VALUES (?, ?, ?, ?)`, id, sp, user, s.stamp(s.now())); err != nil {
					return err
				}
				added++
			}
		}
		for _, sp := range old.Spaces {
			if slices.Contains(manageable, sp) && !slices.Contains(spaces, sp) {
				if _, err := tx.ExecContext(ctx, `DELETE FROM integration_spaces WHERE integration_id = ? AND space_id = ?`, id, sp); err != nil {
					return err
				}
				removed++
			}
		}
		if added+removed > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE integrations SET updated_at = ? WHERE id = ?`, s.stamp(s.now()), id); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: user, Action: audit.IntegrationSpacesChanged,
				TargetType: audit.TargetIntegration, Target: id,
				Details: audit.Details{"provider": old.Provider, "added": added, "removed": removed}}); err != nil {
				return err
			}
		}
		rec, err = s.get(ctx, tx, workspace, id)
		return err
	})
	return rec, err
}

// usable reports whether the workspace's integration is enabled and allowed in the space.
func (s *Store) usable(ctx context.Context, workspace, integration, space int64) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM integrations i
		JOIN integration_spaces s ON s.integration_id = i.id
		WHERE i.id = ? AND i.workspace_id = ? AND i.disabled_at IS NULL AND s.space_id = ?)`, integration, workspace, space).Scan(&ok)
	return ok, err
}

// credentials opens the integration's secrets. A previous webhook secret that has expired, or
// doesn't open, is left out; the others failing to open is an unreadableError.
func (s *Store) credentials(ctx context.Context, workspace, id int64) (Credentials, error) {
	rec, err := s.get(ctx, s.db, workspace, id)
	if err != nil {
		return Credentials{}, err
	}
	creds := Credentials{Provider: rec.Provider, Mode: rec.Mode, Settings: rec.Settings}
	rows, err := s.db.QueryContext(ctx, `SELECT name, sealed, expires_at FROM integration_secrets WHERE integration_id = ?`, id)
	if err != nil {
		return Credentials{}, err
	}
	defer rows.Close()
	now := s.stamp(s.now())
	for rows.Next() {
		var name string
		var sealed []byte
		var expires *string
		if err := rows.Scan(&name, &sealed, &expires); err != nil {
			return Credentials{}, err
		}
		plain, openErr := s.key.Open(sealed, rec.sealRef(workspace).ad(name))
		if name == SecretWebhookSecretPrevious && (openErr != nil || (expires != nil && *expires <= now)) {
			continue
		}
		if openErr != nil {
			return Credentials{}, unreadableError{secret: name}
		}
		switch name {
		case SecretAPIKey:
			creds.APIKey = Secret(plain)
		case SecretWebhookSecret:
			creds.WebhookSecret = Secret(plain)
		case SecretWebhookSecretPrevious:
			creds.WebhookSecretPrevious = Secret(plain)
		}
	}
	return creds, rows.Err()
}

// SpaceIntegration is an integration as someone who works in a space sees it: enough to build an
// app against, nothing about the account.
type SpaceIntegration struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	// Status is "active", or "disabled" while an admin has switched the integration off.
	Status string `json:"status"`
}

// forSpace lists the integrations allowed in the workspace's space.
func (s *Store) forSpace(ctx context.Context, workspace, space int64) ([]SpaceIntegration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.id, i.name, i.display_name, i.provider, i.disabled_at IS NOT NULL
		FROM integrations i JOIN integration_spaces s ON s.integration_id = i.id
		WHERE i.workspace_id = ? AND s.space_id = ? ORDER BY i.id`, workspace, space)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SpaceIntegration{}
	for rows.Next() {
		var si SpaceIntegration
		var provider string
		var disabled bool
		if err := rows.Scan(&si.ID, &si.Name, &si.DisplayName, &provider, &disabled); err != nil {
			return nil, err
		}
		if p := Lookup(provider); p != nil && len(p.Kinds()) > 0 {
			si.Kind = p.Kinds()[0]
		}
		si.Status = "active"
		if disabled {
			si.Status = "disabled"
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// HookPrefix starts the path of an integration's webhook address: HookPrefix + its public id.
const HookPrefix = "/hooks/"

// hookURL is the address to register at the provider for the public id, "" without a public URL.
func hookURL(publicURL, publicID string) string {
	if publicURL == "" {
		return ""
	}
	return strings.TrimRight(publicURL, "/") + HookPrefix + publicID
}
