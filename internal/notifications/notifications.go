package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"net/http"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/mail"
	"taskto.ai/taskto/internal/schedule"
	"taskto.ai/taskto/internal/serverkey"
)

const (
	KindCardAssigned       = "card_assigned"
	KindCardComment        = "card_comment"
	KindCardMention        = "card_mention"
	KindAppEffectFailed    = "app_effect_failed"
	KindCardDue            = "card_due"
	KindCardOverdue        = "card_overdue"
	KindIntegrationEvent   = "integration_event"
	KindConversationShared = "conversation_shared"
	KindChanged            = "changed"
)

const (
	MaxSubscriptions = 20
	MaxListed        = 50
	DefaultListed    = 30
	keepRead         = 90 * 24 * time.Hour
	keepAll          = 180 * 24 * time.Hour
	perUser          = 500
)

const timeFormat = "2006-01-02T15:04:05.000Z"

type Ref struct {
	Space        int64  `json:"space,omitempty"`
	Board        int64  `json:"board,omitempty"`
	Card         int64  `json:"card,omitempty"`
	App          int64  `json:"app,omitempty"`
	Effect       int64  `json:"effect,omitempty"`
	Integration  int64  `json:"integration,omitempty"`
	Event        int64  `json:"event,omitempty"`
	Conversation int64  `json:"conversation,omitempty"`
	Due          string `json:"due,omitempty"`
}

type Notice struct {
	UserID, WorkspaceID int64
	Kind                string
	Ref                 Ref
	DedupeKey           string
}

var errUnknownKind = errors.New("notifications: unknown kind")

func Add(ctx context.Context, tx *sql.Tx, n Notice) error {
	switch n.Kind {
	case KindCardAssigned, KindCardComment, KindCardMention, KindAppEffectFailed, KindCardDue, KindCardOverdue, KindIntegrationEvent, KindConversationShared:
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
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO push_deliveries (notification_id, subscription_id, next_attempt_at, updated_at)
		SELECT ?, id, ?, ? FROM push_subscriptions WHERE user_id = ?`, id, at, at, n.UserID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO email_deliveries (notification_id, next_attempt_at, updated_at)
		SELECT ?, ?, ? WHERE EXISTS (
			SELECT 1 FROM notification_email_prefs
			WHERE user_id = ? AND workspace_id = ? AND kind = ? AND mode = 'instant')
		AND EXISTS (
			SELECT 1 FROM notification_email_verification v JOIN users u ON u.id = v.user_id
			WHERE v.user_id = ? AND v.verified_at IS NOT NULL AND v.address = u.email)`,
		id, at, at, n.UserID, n.WorkspaceID, n.Kind, n.UserID)
	return err
}

type Options struct {
	Key        *serverkey.Key
	HTTPClient *http.Client
	Egress     *egress.Policy
	Contact    string
	Mail       *mail.Service
	PublicURL  string
	Now        func() time.Time
}

type ConversationReader interface {
	Readable(ctx context.Context, workspace, user, conversation int64) (title, owner string, ok bool, err error)
}

func (s *Service) SetConversations(c ConversationReader) { s.conversations = c }

const DefaultContact = "https://taskto.ai"

type Service struct {
	db            *sql.DB
	access        *access.Checker
	key           *serverkey.Key
	vapid         *vapidKey
	client        *http.Client
	contact       string
	egress        egress.Policy
	clock         func() time.Time
	mail          *mail.Service
	publicURL     string
	sched         *schedule.Service
	conversations ConversationReader
	tidyEvery     time.Duration
	afterReserve  func()
	betweenReads  func(*sql.Tx)
}

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
	s := &Service{db: sqlDB, access: spaces, key: opts.Key, vapid: vapid, client: opts.HTTPClient, contact: opts.Contact, clock: opts.Now, tidyEvery: tidyEvery, egress: egress.MustParse("public"), mail: opts.Mail, publicURL: strings.TrimRight(opts.PublicURL, "/")}
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
