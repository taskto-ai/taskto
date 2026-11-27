package integrations

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/serverkey"
)

var (
	errNotFound = errors.New("integration not found")
	errTaken    = errors.New("an integration with this name exists")
	errTooMany  = errors.New("too many integrations")
	errInUse    = errors.New("the integration is in use")
)

type unreadableError struct{ secret string }

func (e unreadableError) Error() string { return "the sealed " + e.secret + " can't be opened" }

const (
	maxIntegrations = 20
	previousOverlap = 24 * time.Hour
)

type Integration struct {
	ID                int64             `json:"id"`
	Provider          string            `json:"provider"`
	Kind              string            `json:"kind"`
	Name              string            `json:"name"`
	DisplayName       string            `json:"display_name"`
	Mode              string            `json:"mode"`
	Settings          map[string]string `json:"settings" apitypes:"nonnull"`
	MaxAmountMinor    int64             `json:"max_amount_minor"`
	Currencies        []string          `json:"currencies" apitypes:"nonnull"`
	Secrets           map[string]string `json:"secrets" apitypes:"nonnull"`
	WebhookURL        *string           `json:"webhook_url"`
	WebhookURLChanged bool              `json:"webhook_url_changed"`
	Disabled          bool              `json:"disabled"`
	DisabledAt        *string           `json:"disabled_at"`
	Spaces            []int64           `json:"spaces" apitypes:"nonnull"`
	OtherSpaces       int               `json:"other_spaces"`
	ApprovalSpaces    []int64           `json:"approval_spaces" apitypes:"nonnull"`
	CreatedAt         string            `json:"created_at"`
	UpdatedAt         string            `json:"updated_at"`
	LastEventAt       *string           `json:"last_event_at"`
}

const (
	secretSet    = "set"
	secretNotSet = "not set"
)

type record struct {
	Integration
	publicID      string
	registeredURL string
}

type Store struct {
	db                  *sql.DB
	key                 *serverkey.Key
	now                 func() time.Time
	modeSwitchUnchecked bool
}

func NewStore(sqlDB *sql.DB, key *serverkey.Key) *Store {
	return &Store{db: sqlDB, key: key, now: time.Now}
}

type sealRef struct {
	workspace, id int64
	publicID      string
}

func (r sealRef) ad(name string) []byte {
	return []byte("taskto integration:" + strconv.FormatInt(r.workspace, 10) + ":" + r.publicID + ":" +
		strconv.FormatInt(r.id, 10) + ":" + name)
}

func (r record) sealRef(workspace int64) sealRef {
	return sealRef{workspace: workspace, id: r.ID, publicID: r.publicID}
}

const timeFormat = "2006-01-02T15:04:05.000Z"

func (s *Store) stamp(t time.Time) string { return t.UTC().Format(timeFormat) }

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const columns = `i.id, i.public_id, i.provider, i.name, i.display_name, i.mode, i.settings, i.max_amount_minor,
	i.currencies, i.disabled_at, i.created_at, i.updated_at, i.last_event_at, coalesce(i.webhook_registered_url, '')`

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
			&currencies, &r.DisabledAt, &r.CreatedAt, &r.UpdatedAt, &r.LastEventAt, &r.registeredURL); err != nil {
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
		r.ApprovalSpaces = []int64{}
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
	prows, err := q.QueryContext(ctx, `SELECT s.integration_id, s.space_id, s.from_approval FROM integration_spaces s
		JOIN integrations i ON i.id = s.integration_id
		WHERE i.workspace_id = ? AND (? = 0 OR i.id = ?) ORDER BY s.space_id`, workspace, id, id)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var rid, space int64
		var fromApproval bool
		if err := prows.Scan(&rid, &space, &fromApproval); err != nil {
			_ = prows.Close()
			return nil, err
		}
		if r := at[rid]; r != nil {
			r.Spaces = append(r.Spaces, space)
			if fromApproval {
				r.ApprovalSpaces = append(r.ApprovalSpaces, space)
			}
		}
	}
	if err := errors.Join(prows.Err(), prows.Close()); err != nil {
		return nil, err
	}
	return recs, nil
}

func (s *Store) read(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

func (s *Store) list(ctx context.Context, workspace int64) ([]record, error) {
	var recs []record
	err := s.read(ctx, func(tx *sql.Tx) (err error) {
		recs, err = s.load(ctx, tx, workspace, 0)
		return err
	})
	return recs, err
}

func (s *Store) getSnapshot(ctx context.Context, workspace, id int64) (rec record, err error) {
	err = s.read(ctx, func(tx *sql.Tx) (err error) {
		rec, err = s.get(ctx, tx, workspace, id)
		return err
	})
	return rec, err
}

func (s *Store) get(ctx context.Context, q querier, workspace, id int64) (record, error) {
	if id <= 0 {
		return record{}, errNotFound
	}
	recs, err := s.load(ctx, q, workspace, id)
	if err != nil {
		return record{}, err
	}
	if len(recs) == 0 {
		return record{}, errNotFound
	}
	return recs[0], nil
}

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
	if err := auth.EnsureActorEnabled(ctx, tx); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

type draft struct {
	provider    Provider
	name        string
	displayName string
	settings    map[string]string
	secrets     map[string]string
	maxAmount   int64
	currencies  []string
	mode        string
	publicURL   string
}

func newPublicID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (s *Store) seal(ctx context.Context, tx *sql.Tx, ref sealRef, name, value string, expires *time.Time) error {
	sealed, err := s.key.Seal([]byte(value), ref.ad(name))
	if err != nil {
		return err
	}
	var exp any
	if expires != nil {
		exp = s.stamp(*expires)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO integration_secrets (integration_id, name, sealed, set_at, expires_at, fingerprint)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (integration_id, name) DO UPDATE SET sealed = excluded.sealed, set_at = excluded.set_at,
			expires_at = excluded.expires_at, fingerprint = excluded.fingerprint`,
		ref.id, name, sealed, s.stamp(s.now()), exp, s.fingerprintFor(name, value))
	return err
}

const fingerprintPurpose = "taskto integration webhook secret fingerprint"

func isWebhookSecret(name string) bool {
	return name == SecretWebhookSecret || name == SecretWebhookSecretPrevious
}

func (s *Store) webhookFingerprint(value string) []byte {
	return s.key.MAC(fingerprintPurpose, []byte(value))
}

func (s *Store) fingerprintFor(name, value string) any {
	if !isWebhookSecret(name) {
		return nil
	}
	return s.webhookFingerprint(value)
}

func (s *Store) BackfillFingerprints(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := s.backfillFingerprints(ctx, tx); err != nil {
			return err
		}
		return s.markSharedSecrets(ctx, tx)
	})
}

func (s *Store) backfillFingerprints(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, backfillQuery)
	if err != nil {
		return 0, err
	}
	type pending struct {
		ref    sealRef
		name   string
		sealed []byte
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.ref.id, &p.ref.workspace, &p.ref.publicID, &p.name, &p.sealed); err != nil {
			_ = rows.Close()
			return 0, err
		}
		todo = append(todo, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	filled := 0
	for _, p := range todo {
		plain, err := s.key.Open(p.sealed, p.ref.ad(p.name))
		if err != nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE integration_secrets SET fingerprint = ? WHERE integration_id = ? AND name = ?`,
			s.fingerprintFor(p.name, string(plain)), p.ref.id, p.name); err != nil {
			return 0, err
		}
		filled++
	}
	if filled > 0 {
		if err := s.markSharedSecrets(ctx, tx); err != nil {
			return 0, err
		}
	}
	return filled, nil
}

const backfillQuery = `SELECT i.id, i.workspace_id, i.public_id, s.name, s.sealed
	FROM integration_secrets s INDEXED BY integration_secrets_unfingerprinted
	JOIN integrations i ON i.id = s.integration_id
	WHERE s.fingerprint IS NULL AND s.name IN ('webhook_secret', 'webhook_secret_previous')`

func (s *Store) markSharedSecrets(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO integration_shared_secrets (fingerprint, found_at)
		SELECT fingerprint, ? FROM integration_secrets WHERE fingerprint IS NOT NULL AND (expires_at IS NULL OR expires_at > ?)
		GROUP BY fingerprint HAVING count(DISTINCT integration_id) > 1`, s.stamp(s.now()), s.stamp(s.now()))
	return err
}

func (s *Store) markSharedSecretsOf(ctx context.Context, tx *sql.Tx, workspace, id int64) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO integration_shared_secrets (fingerprint, found_at)
		SELECT DISTINCT a.fingerprint, ? FROM integration_secrets a JOIN integrations i ON i.id = a.integration_id
		WHERE a.integration_id = ? AND i.workspace_id = ? AND a.fingerprint IS NOT NULL AND (a.expires_at IS NULL OR a.expires_at > ?)
			AND EXISTS (SELECT 1 FROM integration_secrets b WHERE b.fingerprint = a.fingerprint AND b.integration_id <> a.integration_id
				AND (b.expires_at IS NULL OR b.expires_at > ?))`,
		s.stamp(s.now()), id, workspace, s.stamp(s.now()), s.stamp(s.now()))
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
		if secret, ok := d.secrets[SecretWebhookSecret]; ok {
			if err := s.checkWebhookSecretFree(ctx, tx, 0, secret); err != nil {
				return err
			}
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
		var registered any
		if _, ok := d.secrets[SecretWebhookSecret]; ok {
			registered = nullString(hookURL(d.publicURL, publicID))
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO integrations (workspace_id, public_id, provider, name, display_name, mode, settings,
			max_amount_minor, currencies, created_by, created_at, updated_at, webhook_registered_url) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			workspace, publicID, d.provider.ID(), d.name, d.displayName, d.mode, string(settings), d.maxAmount, string(currencies),
			user, now, now, registered)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
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

type change struct {
	displayName *string
	settings    map[string]string
	secrets     map[string]string
	maxAmount   *int64
	currencies  []string
	disabled    *bool
	publicURL   string
}

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
		n := old
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
		if n.Mode != old.Mode && old.Mode != "" && !s.modeSwitchUnchecked {
			var payable int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM integration_refs r
				WHERE r.integration_id = ? AND r.account_mode = ? AND `+payableRef, id, old.Mode).Scan(&payable); err != nil {
				return err
			}
			if payable > 0 {
				return apierr.New(http.StatusConflict, apierr.CodeIntegrationModeChangeRefused,
					"payment links made in the integration's current mode may still be paid: the key can't switch modes yet",
					apierr.Params{"count": payable})
			}
		}

		now := s.now()
		ref := old.sealRef(workspace)
		if webhook, ok := c.secrets[SecretWebhookSecret]; ok {
			if err := s.markSharedSecretsOf(ctx, tx, workspace, id); err != nil {
				return err
			}
			if err := s.checkWebhookSecretFree(ctx, tx, id, webhook); err != nil {
				return err
			}
		}
		if webhook, ok := c.secrets[SecretWebhookSecret]; ok && !s.holds(ctx, tx, ref, SecretWebhookSecret, webhook) {
			if err := s.keepPrevious(ctx, tx, ref, now); err != nil {
				return err
			}
			if err := s.seal(ctx, tx, ref, SecretWebhookSecret, webhook, nil); err != nil {
				return err
			}
		}
		if _, ok := c.secrets[SecretWebhookSecret]; ok {
			if _, err := tx.ExecContext(ctx, `UPDATE integrations SET webhook_registered_url = ? WHERE id = ? AND workspace_id = ?`,
				nullString(hookURL(c.publicURL, old.publicID)), id, workspace); err != nil {
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

func (s *Store) checkWebhookSecretFree(ctx context.Context, tx *sql.Tx, except int64, value string) error {
	if _, err := s.backfillFingerprints(ctx, tx); err != nil {
		return err
	}
	taken, err := s.unusable(ctx, tx, except, s.webhookFingerprint(value))
	if err != nil {
		return err
	}
	if taken {
		return errWebhookSecretReused
	}
	return nil
}

func (s *Store) unusable(ctx context.Context, q querier, except int64, fingerprint []byte) (bool, error) {
	var found bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM integration_shared_secrets WHERE fingerprint = ?)`,
		fingerprint).Scan(&found); err != nil || found {
		return found, err
	}
	return s.heldByAnother(ctx, q, except, fingerprint)
}

func (s *Store) heldByAnother(ctx context.Context, q querier, except int64, fingerprint []byte) (bool, error) {
	var held bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM integration_secrets
		WHERE fingerprint = ? AND integration_id <> ? AND name IN (?, ?) AND (expires_at IS NULL OR expires_at > ?))`,
		fingerprint, except, SecretWebhookSecret, SecretWebhookSecretPrevious, s.stamp(s.now())).Scan(&held)
	return held, err
}

func (s *Store) sharedWebhookSecrets(ctx context.Context, q querier, integration int64, creds Credentials) (current, previous bool, err error) {
	check := func(secret Secret) (bool, error) {
		if secret == "" {
			return false, nil
		}
		return s.unusable(ctx, q, integration, s.webhookFingerprint(secret.Reveal()))
	}
	if current, err = check(creds.WebhookSecret); err != nil {
		return false, false, err
	}
	previous, err = check(creds.WebhookSecretPrevious)
	return current, previous, err
}

func (s *Store) holds(ctx context.Context, tx *sql.Tx, ref sealRef, name, value string) bool {
	var sealed []byte
	if err := tx.QueryRowContext(ctx, `SELECT sealed FROM integration_secrets WHERE integration_id = ? AND name = ?`,
		ref.id, name).Scan(&sealed); err != nil {
		return false
	}
	plain, err := s.key.Open(sealed, ref.ad(name))
	return err == nil && subtle.ConstantTimeCompare(plain, []byte(value)) == 1
}

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
	plain, ok := s.opens(sealed, ref, SecretWebhookSecret)
	if !ok {
		return nil
	}
	held, err := s.unusable(ctx, tx, ref.id, s.webhookFingerprint(string(plain)))
	if err != nil {
		return err
	}
	if held {
		_, err = tx.ExecContext(ctx, `DELETE FROM integration_secrets WHERE integration_id = ? AND name = ?`,
			ref.id, SecretWebhookSecretPrevious)
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE integrations SET webhook_previous_registered_url = webhook_registered_url WHERE id = ?`,
		ref.id); err != nil {
		return err
	}
	expires := now.Add(previousOverlap)
	return s.seal(ctx, tx, ref, SecretWebhookSecretPrevious, string(plain), &expires)
}

func (s *Store) remove(ctx context.Context, workspace, id int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := s.markSharedSecretsOf(ctx, tx, workspace, id); err != nil {
			return err
		}
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

func (s *Store) setSpaces(ctx context.Context, workspace, user, id int64, spaces, keep []int64, manageable []int64) (record, error) {
	var rec record
	err := s.write(ctx, func(tx *sql.Tx) error {
		old, err := s.get(ctx, tx, workspace, id)
		if err != nil {
			return err
		}
		var added, removed, kept int64
		for _, sp := range spaces {
			if slices.Contains(old.Spaces, sp) {
				if !slices.Contains(keep, sp) {
					continue
				}
				res, err := tx.ExecContext(ctx, `UPDATE integration_spaces SET from_approval = 0, allowed_by = ?, allowed_at = ?
					WHERE integration_id = ? AND space_id = ? AND from_approval = 1`, user, s.stamp(s.now()), id, sp)
				if err != nil {
					return err
				}
				n, err := res.RowsAffected()
				if err != nil {
					return err
				}
				kept += n
			} else {
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
		if added+removed+kept > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE integrations SET updated_at = ? WHERE id = ?`, s.stamp(s.now()), id); err != nil {
				return err
			}
		}
		if added+removed > 0 {
			if err := audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: user, Action: audit.IntegrationSpacesChanged,
				TargetType: audit.TargetIntegration, Target: id,
				Details: audit.Details{"provider": old.Provider, "added": added, "removed": removed}}); err != nil {
				return err
			}
		}
		if kept > 0 {
			if err := audit.Record(ctx, tx, audit.Event{Workspace: workspace, Actor: user, Action: audit.IntegrationSpacesKept,
				TargetType: audit.TargetIntegration, Target: id,
				Details: audit.Details{"provider": old.Provider, "kept": kept}}); err != nil {
				return err
			}
		}
		rec, err = s.get(ctx, tx, workspace, id)
		return err
	})
	return rec, err
}

func (s *Store) usable(ctx context.Context, workspace, integration, space int64) (bool, error) {
	return s.usableIn(ctx, s.db, workspace, integration, space)
}

func (s *Store) usableIn(ctx context.Context, q querier, workspace, integration, space int64) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM integrations i
		JOIN integration_spaces s ON s.integration_id = i.id
		WHERE i.id = ? AND i.workspace_id = ? AND i.disabled_at IS NULL AND s.space_id = ?)`, integration, workspace, space).Scan(&ok)
	return ok, err
}

func (s *Store) credentials(ctx context.Context, workspace, id int64) (creds Credentials, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		creds, err = s.openCredentials(ctx, tx, workspace, id)
		return err
	})
	return creds, err
}

func (s *Store) hookCredentials(ctx context.Context, workspace, id int64) (creds Credentials, revision string, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		if creds, err = s.openCredentials(ctx, tx, workspace, id); err != nil {
			return err
		}
		current, previous, err := s.sharedWebhookSecrets(ctx, tx, id, creds)
		if err != nil {
			return err
		}
		if current {
			creds.WebhookSecret = ""
		}
		if previous {
			creds.WebhookSecretPrevious = ""
		}
		revision, err = s.credentialRevision(ctx, tx, id)
		return err
	})
	return creds, revision, err
}

func (s *Store) openCredentials(ctx context.Context, tx *sql.Tx, workspace, id int64) (Credentials, error) {
	rec, err := s.get(ctx, tx, workspace, id)
	if err != nil {
		return Credentials{}, err
	}
	creds := Credentials{Provider: rec.Provider, Mode: rec.Mode, Settings: rec.Settings}
	rows, err := tx.QueryContext(ctx, `SELECT name, sealed, expires_at FROM integration_secrets WHERE integration_id = ?`, id)
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

type SpaceIntegration struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
}

func (s *Store) ForSpace(ctx context.Context, workspace, space int64) ([]SpaceIntegration, error) {
	return s.forSpace(ctx, workspace, space)
}

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

const HookPrefix = "/hooks/"

func hookURL(publicURL, publicID string) string {
	if publicURL == "" {
		return ""
	}
	return strings.TrimRight(publicURL, "/") + HookPrefix + publicID
}

func (s *Store) opens(sealed []byte, ref sealRef, name string) ([]byte, bool) {
	plain, err := s.key.Open(sealed, ref.ad(name))
	return plain, err == nil
}
