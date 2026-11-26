package integrations

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
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
	"taskto.ai/taskto/internal/auth"
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
	// WebhookURLChanged says that the address the webhook signing secret was saved for is not
	// webhook_url any more: the server's public URL changed since. The provider still sends to,
	// and Square signs over, the old address, so the admin registers the new one and saves the
	// signing secret again.
	WebhookURLChanged bool    `json:"webhook_url_changed"`
	Disabled          bool    `json:"disabled"`
	DisabledAt        *string `json:"disabled_at"`
	// Spaces are the spaces whose apps may use the integration, by id: only the ones the caller
	// may manage (their own private space and the workspace's shared ones), never another
	// person's private space. OtherSpaces counts the rest, which the caller can neither see nor
	// change.
	Spaces      []int64 `json:"spaces" apitypes:"nonnull"`
	OtherSpaces int     `json:"other_spaces"`
	// ApprovalSpaces are the ones of Spaces an app's approval allowed (not an admin): they go with
	// the approval, unless an admin keeps them (PUT .../spaces, "keep"). Saving the list never
	// keeps them.
	ApprovalSpaces []int64 `json:"approval_spaces" apitypes:"nonnull"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	LastEventAt    *string `json:"last_event_at"`
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
	// registeredURL is the webhook address the signing secret was saved for, "" for none.
	registeredURL string
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
	i.currencies, i.disabled_at, i.created_at, i.updated_at, i.last_event_at, coalesce(i.webhook_registered_url, '')`

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

// read runs fn in one read-only transaction: a single snapshot, so what it reads together (an
// integration, its secrets and its spaces) is of one state, whatever commits meanwhile.
func (s *Store) read(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // nothing was written
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

// getSnapshot is get in a transaction of its own, for a reader that isn't in one.
func (s *Store) getSnapshot(ctx context.Context, workspace, id int64) (rec record, err error) {
	err = s.read(ctx, func(tx *sql.Tx) (err error) {
		rec, err = s.get(ctx, tx, workspace, id)
		return err
	})
	return rec, err
}

// get reads one integration. An id that isn't positive is "not found": load's 0 means every
// integration, which a lookup by id must never fall into.
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

// write runs fn in a transaction, which takes the write lock when it begins (see package db). The
// admin who is acting is read again first: one disabled after RequireAdmin let the request in
// changes nothing (auth.ErrAccountDisabled).
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
	// publicURL is the server's public URL, which the webhook address a signing secret is saved for
	// is made of.
	publicURL string
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
	_, err = tx.ExecContext(ctx, `INSERT INTO integration_secrets (integration_id, name, sealed, set_at, expires_at, fingerprint)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (integration_id, name) DO UPDATE SET sealed = excluded.sealed, set_at = excluded.set_at,
			expires_at = excluded.expires_at, fingerprint = excluded.fingerprint`,
		ref.id, name, sealed, s.stamp(s.now()), exp, s.fingerprintFor(name, value))
	return err
}

// fingerprintPurpose is what the webhook secrets' fingerprints are made for (serverkey.MAC).
const fingerprintPurpose = "taskto integration webhook secret fingerprint"

// isWebhookSecret says whether name is one of the secrets a delivery is verified with.
func isWebhookSecret(name string) bool {
	return name == SecretWebhookSecret || name == SecretWebhookSecretPrevious
}

// webhookFingerprint is the keyed fingerprint stored beside a webhook secret. The same secret has
// the same fingerprint wherever it is kept, which is what makes reuse a lookup; it is made with
// the server's key, so it can't be computed, or checked against a guess, by anyone who only has
// the database.
func (s *Store) webhookFingerprint(value string) []byte {
	return s.key.MAC(fingerprintPurpose, []byte(value))
}

// fingerprintFor is what is stored in the fingerprint column for a secret called name: nil for
// any secret that isn't a webhook secret.
func (s *Store) fingerprintFor(name, value string) any {
	if !isWebhookSecret(name) {
		return nil
	}
	return s.webhookFingerprint(value)
}

// BackfillFingerprints gives the webhook secrets saved before fingerprints existed theirs, which
// the server does when it starts. A secret that doesn't open (the server key changed) stays
// without one: it verifies nothing either.
//
// It also records every fingerprint found held by more than one integration (see
// markSharedSecrets), which a start does whether or not anything was missing.
func (s *Store) BackfillFingerprints(ctx context.Context) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := s.backfillFingerprints(ctx, tx); err != nil {
			return err
		}
		return s.markSharedSecrets(ctx, tx)
	})
}

// backfillFingerprints fills in the fingerprints that are missing, and says how many it did. The
// query is bounded by the partial index on exactly those rows (migration 0053), so with nothing
// missing, which is every create and rotation after the first start, it reads no row of the table.
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
		// Secrets that were just given their fingerprint may be ones two integrations hold.
		if err := s.markSharedSecrets(ctx, tx); err != nil {
			return 0, err
		}
	}
	return filled, nil
}

// backfillQuery finds the webhook secrets without a fingerprint. Its names are literals, the very
// ones of the partial index integration_secrets_unfingerprinted, because SQLite only uses a
// partial index for a query whose condition it can see implies the index's; a test checks the plan.
const backfillQuery = `SELECT i.id, i.workspace_id, i.public_id, s.name, s.sealed
	FROM integration_secrets s INDEXED BY integration_secrets_unfingerprinted
	JOIN integrations i ON i.id = s.integration_id
	WHERE s.fingerprint IS NULL AND s.name IN ('webhook_secret', 'webhook_secret_previous')`

// markSharedSecrets records, as compromised, every webhook secret that more than one integration
// holds now (an expired previous secret holds nothing): it is known to each of their workspaces.
// Once recorded it
// verifies for no integration, whoever rotates away from it first, and no integration may be given
// it again; each that holds it refuses deliveries until its provider gives it a fresh secret.
func (s *Store) markSharedSecrets(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO integration_shared_secrets (fingerprint, found_at)
		SELECT fingerprint, ? FROM integration_secrets WHERE fingerprint IS NOT NULL AND (expires_at IS NULL OR expires_at > ?)
		GROUP BY fingerprint HAVING count(DISTINCT integration_id) > 1`, s.stamp(s.now()), s.stamp(s.now()))
	return err
}

// markSharedSecretsOf is markSharedSecrets for the secrets of one integration of the workspace,
// run before a change or a removal takes a copy away, so the other holder is not left as the only
// one. A write is the only place it can be recorded: a delivery or a check only reads.
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
	// publicURL is the server's public URL: a webhook secret in secrets is saved for the address
	// made of it.
	publicURL string
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
			// Whatever copy of a shared secret this change takes away, the other holder must not be left
			// as the only one: it is recorded first. Then the new secret must be one that was never
			// shared, even if it is the one this integration has now.
			if err := s.markSharedSecretsOf(ctx, tx, workspace, id); err != nil {
				return err
			}
			if err := s.checkWebhookSecretFree(ctx, tx, id, webhook); err != nil {
				return err
			}
		}
		if webhook, ok := c.secrets[SecretWebhookSecret]; ok && !s.holds(ctx, tx, ref, SecretWebhookSecret, webhook) {
			// The secret that is replaced keeps verifying for a day, so events the provider signed
			// with it before the change took effect there aren't refused. Sending the secret that
			// is already current (a retry whose answer was lost) leaves both slots as they are:
			// copying it into the previous slot would drop the one it replaced.
			if err := s.keepPrevious(ctx, tx, ref, now); err != nil {
				return err
			}
			if err := s.seal(ctx, tx, ref, SecretWebhookSecret, webhook, nil); err != nil {
				return err
			}
		}
		if _, ok := c.secrets[SecretWebhookSecret]; ok {
			// Saving the signing secret, the same one included, says the provider sends to the address
			// shown now: an admin who registered the new address after the public URL changed saves the
			// secret again, which is what clears the warning.
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

// checkWebhookSecretFree refuses value as the webhook secret of integration except (0 for one not
// made yet) when another integration, in any workspace, verifies deliveries with it: as its
// current secret or as a previous one that hasn't expired. A signature has to open exactly one
// integration, so one secret can't serve two. It is a lookup of the secret's fingerprint (after
// giving any secret saved before fingerprints their own), and the answer names nothing of the
// integration that holds it.
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

// unusable says whether a webhook secret with the fingerprint can verify no delivery for integration
// except: one that was found held by more than one integration, ever (integration_shared_secrets),
// or that another holds now.
func (s *Store) unusable(ctx context.Context, q querier, except int64, fingerprint []byte) (bool, error) {
	var found bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM integration_shared_secrets WHERE fingerprint = ?)`,
		fingerprint).Scan(&found); err != nil || found {
		return found, err
	}
	return s.heldByAnother(ctx, q, except, fingerprint)
}

// heldByAnother says whether an integration other than except has a webhook secret, current or
// previous and not expired, with the fingerprint.
func (s *Store) heldByAnother(ctx context.Context, q querier, except int64, fingerprint []byte) (bool, error) {
	var held bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM integration_secrets
		WHERE fingerprint = ? AND integration_id <> ? AND name IN (?, ?) AND (expires_at IS NULL OR expires_at > ?))`,
		fingerprint, except, SecretWebhookSecret, SecretWebhookSecretPrevious, s.stamp(s.now())).Scan(&held)
	return held, err
}

// sharedWebhookSecrets says which of creds' webhook secrets another integration holds too (one an
// old install can have, from before a secret was refused for it). Such a secret opens more than
// one integration, so it is used for none.
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

// holds says whether the integration's stored secret called name is, once opened, value. A
// secret that is missing or doesn't open is not.
func (s *Store) holds(ctx context.Context, tx *sql.Tx, ref sealRef, name, value string) bool {
	var sealed []byte
	if err := tx.QueryRowContext(ctx, `SELECT sealed FROM integration_secrets WHERE integration_id = ? AND name = ?`,
		ref.id, name).Scan(&sealed); err != nil {
		return false
	}
	plain, err := s.key.Open(sealed, ref.ad(name))
	return err == nil && subtle.ConstantTimeCompare(plain, []byte(value)) == 1
}

// keepPrevious moves the integration's current webhook secret to the previous one, valid for
// previousOverlap, and drops a previous one that has expired. With no current secret, or one that
// doesn't open (the server key changed) or that another integration holds too, there is nothing
// to keep.
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
	// A secret another integration holds too (one an old install can have) verified nothing here, and
	// kept as the previous one it would keep the other's from verifying for the overlap: it is let go.
	held, err := s.unusable(ctx, tx, ref.id, s.webhookFingerprint(string(plain)))
	if err != nil {
		return err
	}
	if held {
		_, err = tx.ExecContext(ctx, `DELETE FROM integration_secrets WHERE integration_id = ? AND name = ?`,
			ref.id, SecretWebhookSecretPrevious)
		return err
	}
	expires := now.Add(previousOverlap)
	return s.seal(ctx, tx, ref, SecretWebhookSecretPrevious, string(plain), &expires)
}

// remove deletes the workspace's integration id with its secrets and spaces. Anything that
// names it with an ON DELETE RESTRICT (an app's approval) makes the delete fail: errInUse.
func (s *Store) remove(ctx context.Context, workspace, id int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		// Its secrets go with it: one another integration holds too is recorded as shared first.
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
//
// A space an app's approval allowed stays the approval's when the list names it: only a space in
// keep (a subset of spaces, the admin's explicit word) becomes the admin's own, which no approval
// takes away, and each such change is an audit event of its own.
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
					continue // already allowed, and left as it is: an approval's grant stays its own
				}
				// Kept by the admin who named it: it is theirs now, and no approval takes it away.
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
func (s *Store) credentials(ctx context.Context, workspace, id int64) (creds Credentials, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		creds, err = s.openCredentials(ctx, tx, workspace, id)
		return err
	})
	return creds, err
}

// hookCredentials is credentials with the revision of the webhook secrets it read, from the same
// snapshot, for the webhook route's guard, and without a webhook secret that more than one
// integration holds.
func (s *Store) hookCredentials(ctx context.Context, workspace, id int64) (creds Credentials, revision string, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		if creds, err = s.openCredentials(ctx, tx, workspace, id); err != nil {
			return err
		}
		// A webhook secret that another integration holds too is left out, as if it were not set:
		// the delivery can't be told to be for this integration, so it is refused.
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

// ForSpace lists the integrations allowed in the workspace's space, as someone who works there
// sees them. The caller has checked that they may read the space.
func (s *Store) ForSpace(ctx context.Context, workspace, space int64) ([]SpaceIntegration, error) {
	return s.forSpace(ctx, workspace, space)
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
