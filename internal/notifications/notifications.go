// Package notifications is the notification inbox and, for the people who opt in, Web Push.
//
// A notification says that something happened that a person may want to see: a card was assigned
// to them, an effect of an app they used failed and needs them. The inbox is the one place they are
// all found, in the web app, whether or not the browser can push; push is an extra that tells a
// device about one when the app isn't open, and nothing relies on it: a push may arrive late,
// twice or never (the service of the browser's maker carries it, once, with a lifetime), and the
// inbox is what is true. Nothing is pushed to a device until its person turns push on in that
// browser.
//
// What a notification holds is a kind and the IDs of what it is about (Ref), for one person in one
// workspace. It never holds what the record says. What an item says (a card's title, an app's name)
// is worked out whenever it is read or pushed, from what the recipient may read at that moment
// (render.go): a person who lost access to the card since, or whose access to the space ended, is
// shown a generic "something changed" with no names, no IDs and a link to the workspace's home,
// and a push for it says the same. The deep link of an item is an address of the web app, which
// checks access again when it opens.
//
// Push (push.go) is delivered by a worker with bounded tries to the endpoints browsers gave, which
// are reached under the egress policy for public addresses only; a subscription that a push service
// says is gone is removed, one is bound to the session that made it, and leaving a workspace ends
// its notifications. webpush.go has the standards (VAPID and message encryption), made with the
// standard library only. docs/pwa.md and docs/install.md describe it for people.
package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"net/http"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/serverkey"
)

// The kinds of notification. KindChanged is never stored: it is what an item is shown as when its
// recipient can no longer read what it was about.
const (
	KindCardAssigned    = "card_assigned"
	KindAppEffectFailed = "app_effect_failed"
	KindCardDue         = "card_due"
	KindCardOverdue     = "card_overdue"
	KindChanged         = "changed"
)

// Bounds.
const (
	// MaxSubscriptions is how many devices a person may have subscribed.
	MaxSubscriptions = 20
	// MaxListed and DefaultListed bound a page of the inbox.
	MaxListed     = 50
	DefaultListed = 30
	// keepRead and keepAll say how long notifications are kept; perUser caps what a person has in a
	// workspace.
	keepRead = 90 * 24 * time.Hour
	keepAll  = 180 * 24 * time.Hour
	perUser  = 500
)

const timeFormat = "2006-01-02T15:04:05.000Z"

// Ref says what a notification is about: the IDs the kind needs, and no more.
type Ref struct {
	Space  int64 `json:"space,omitempty"`
	Board  int64 `json:"board,omitempty"`
	Card   int64 `json:"card,omitempty"`
	App    int64 `json:"app,omitempty"`
	Effect int64 `json:"effect,omitempty"`
	// Due is the date a card's reminder or overdue notice was made for, a calendar date: the
	// notice says it only while the card is still due then.
	Due string `json:"due,omitempty"`
}

// Notice is a notification to add.
type Notice struct {
	UserID, WorkspaceID int64
	Kind                string
	Ref                 Ref
	// DedupeKey makes adding it again (a retry) add nothing: one per person, kind and key. "" has
	// none.
	DedupeKey string
}

var errUnknownKind = errors.New("notifications: unknown kind")

// Add makes a notification in tx, the transaction of what it is about, and queues a push of it to
// each device its person has subscribed so far. It makes none for someone who isn't a member of
// the workspace, and none twice for one DedupeKey. Producers call it; it checks nothing about what
// the notification is about, which is checked when it is shown.
func Add(ctx context.Context, tx *sql.Tx, n Notice) error {
	switch n.Kind {
	case KindCardAssigned, KindAppEffectFailed, KindCardDue, KindCardOverdue:
	default:
		return errUnknownKind
	}
	ref, err := json.Marshal(n.Ref)
	if err != nil {
		return err
	}
	at := time.Now().UTC().Format(timeFormat)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO notifications (user_id, workspace_id, kind, ref, dedupe_key, created_at)
		SELECT ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM memberships WHERE workspace_id = ? AND user_id = ?)
		ON CONFLICT (user_id, kind, dedupe_key) WHERE dedupe_key <> '' DO NOTHING`,
		n.UserID, n.WorkspaceID, n.Kind, string(ref), n.DedupeKey, at, n.WorkspaceID, n.UserID)
	if err != nil {
		return err
	}
	if made, err := res.RowsAffected(); err != nil || made == 0 {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO push_deliveries (notification_id, subscription_id, next_attempt_at, updated_at)
		SELECT ?, id, ?, ? FROM push_subscriptions WHERE user_id = ?`, id, at, at, n.UserID)
	return err
}

// Options are the service's settings.
type Options struct {
	// Key seals the browsers' auth secrets and the VAPID private key.
	Key *serverkey.Key
	// HTTPClient sends the pushes. nil is the egress client for public addresses only, which is
	// what it must be in a server: an endpoint is an address a person gave.
	HTTPClient *http.Client
	// Egress is where endpoints may be: the policy of registration and of every push. nil is the
	// policy for public addresses only, which is what it must be in a server (an endpoint is an
	// address a person gave); a test that gives its own HTTPClient gives its own policy too.
	Egress *egress.Policy
	// Contact says to the push services who runs the server (the VAPID subject): a mailto: or
	// https: URL. Empty is the project's address.
	Contact string
	// Now is the clock, for tests.
	Now func() time.Time
}

// DefaultContact is the VAPID subject when the operator gives none.
const DefaultContact = "https://taskto.ai"

// Service is the inbox and push.
type Service struct {
	db      *sql.DB
	access  *access.Checker
	key     *serverkey.Key
	vapid   *vapidKey
	client  *http.Client
	contact string
	egress  egress.Policy
	clock   func() time.Time
	// tidyEvery is how often the worker tidies; a field for tests.
	tidyEvery time.Duration
}

// New returns the service over a migrated database, making the server's VAPID key pair if it has
// none yet.
func New(ctx context.Context, sqlDB *sql.DB, spaces *access.Checker, opts Options) (*Service, error) {
	if opts.Key == nil {
		return nil, errors.New("notifications: a server key is required")
	}
	if err := config.ValidatePushContact(opts.Contact); err != nil {
		return nil, fmt.Errorf("notifications: the push contact: %w", err)
	}
	vapid, err := loadVAPID(ctx, sqlDB, opts.Key)
	if err != nil {
		return nil, err
	}
	s := &Service{db: sqlDB, access: spaces, key: opts.Key, vapid: vapid, client: opts.HTTPClient, contact: opts.Contact, clock: opts.Now, tidyEvery: tidyEvery, egress: egress.MustParse("public")}
	if opts.Egress != nil {
		s.egress = *opts.Egress
	}
	if s.client == nil {
		s.client = s.egress.Client(sendTimeout)
	}
	if s.contact == "" {
		s.contact = DefaultContact
	}
	if s.clock == nil {
		s.clock = time.Now
	}
	return s, nil
}

func (s *Service) stamp(t time.Time) string { return t.UTC().Format(timeFormat) }

func (s *Service) now() string { return s.stamp(s.clock()) }

// write runs fn in a transaction.
func (s *Service) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
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
