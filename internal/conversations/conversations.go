package conversations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"taskto.ai/taskto/internal/agentrt"
)

type Runtime interface {
	CreateConversationOn(ctx context.Context, model agentrt.ModelRef) (int64, error)
	Submit(ctx context.Context, conversation int64, in agentrt.Input) (int64, error)
	Note(ctx context.Context, conversation int64, n agentrt.NoteInput) (int64, error)
	Wait(ctx context.Context, submission int64) (agentrt.Settled, error)
	Messages(ctx context.Context, conversation int64) ([]agentrt.Message, error)
	Model(ctx context.Context, conversation int64) (agentrt.ModelRef, error)
	SetModel(ctx context.Context, conversation int64, model agentrt.ModelRef) error
	Abort(ctx context.Context, conversation int64) error
	Entries(ctx context.Context, conversation, before int64, limit int) (json.RawMessage, error)
	Watch(ctx context.Context, conversation int64) (Watch, error)
}

type Watch interface {
	Next() (agentrt.Event, error)
	Close() error
}

func Client(c *agentrt.Client) Runtime { return clientRuntime{c} }

type clientRuntime struct{ *agentrt.Client }

func (c clientRuntime) Watch(ctx context.Context, conversation int64) (Watch, error) {
	w, err := c.Client.Watch(ctx, conversation)
	if err != nil {
		return nil, err
	}
	return w, nil
}

type Agent func(ctx context.Context) (Runtime, error)

type Service struct {
	db           *sql.DB
	agent        Agent
	capability   func(conversation, agentConversation int64) string
	questions    CLIQuestions
	language     func(r *http.Request, browser string) string
	watchSession func(r *http.Request, every time.Duration) (context.Context, context.CancelFunc)

	closing    context.Context
	endStreams context.CancelFunc

	mu      sync.Mutex
	streams map[int64]int

	readyWait     time.Duration
	exportTimeout time.Duration
	heartbeat     time.Duration
	writeTimeout  time.Duration
	maxStreams    int

	heartbeatTicks func() (<-chan time.Time, func())
}

func New(sqlDB *sql.DB, agent Agent) *Service {
	closing, end := context.WithCancel(context.Background())
	return &Service{
		db: sqlDB, agent: agent, closing: closing, endStreams: end, streams: map[int64]int{},
		readyWait: 5 * time.Second, exportTimeout: 30 * time.Second, heartbeat: 15 * time.Second, writeTimeout: time.Minute,
		maxStreams: 16,
	}
}

func (s *Service) openStream(user int64) (func(), bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams[user] >= s.maxStreams {
		return nil, false
	}
	s.streams[user]++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.streams[user]--; s.streams[user] <= 0 {
			delete(s.streams, user)
		}
	}, true
}

func (s *Service) EndStreams() { s.endStreams() }

const (
	RoleOwner       = "owner"
	RoleViewer      = "viewer"
	RoleParticipant = "participant"
)

type Person struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Conversation struct {
	ID             int64   `json:"id"`
	WorkspaceID    int64   `json:"workspace_id"`
	Title          string  `json:"title"`
	ArchivedAt     *string `json:"archived_at"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	CLIPermissions string  `json:"cli_permissions"`
	Role           string  `json:"role"`
	Owner          Person  `json:"owner"`

	agentID int64
}

var errNotFound = errors.New("conversation not found")

const reachColumns = `c.id, c.workspace_id, c.agent_id, c.title, c.archived_at, c.created_at, c.updated_at, c.cli_permissions,
	CASE WHEN c.owner_id = ?1 THEN 'owner' ELSE m.role END, c.owner_id, o.name`

const reachFrom = `FROM conversations c
	JOIN users o ON o.id = c.owner_id AND o.disabled_at IS NULL
	LEFT JOIN conversation_members m ON m.conversation_id = c.id AND m.user_id = ?1`

const reachWhere = `c.workspace_id = ?2
	AND EXISTS (SELECT 1 FROM memberships om WHERE om.workspace_id = c.workspace_id AND om.user_id = c.owner_id)
	AND (c.owner_id = ?1 OR (m.user_id IS NOT NULL
		AND EXISTS (SELECT 1 FROM memberships um WHERE um.workspace_id = c.workspace_id AND um.user_id = ?1)
		AND EXISTS (SELECT 1 FROM users cu WHERE cu.id = ?1 AND cu.disabled_at IS NULL)))`

func scanReach(row interface{ Scan(...any) error }) (Conversation, error) {
	var c Conversation
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.agentID, &c.Title, &c.ArchivedAt, &c.CreatedAt, &c.UpdatedAt, &c.CLIPermissions,
		&c.Role, &c.Owner.ID, &c.Owner.Name)
	return c, err
}

func (s *Service) reach(ctx context.Context, workspace, user, id int64) (Conversation, error) {
	c, err := scanReach(s.db.QueryRowContext(ctx, `SELECT `+reachColumns+` `+reachFrom+`
		WHERE c.id = ?3 AND `+reachWhere, user, workspace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, errNotFound
	}
	return c, err
}

func (s *Service) list(ctx context.Context, workspace, user int64, archived, shared bool) ([]Conversation, error) {
	who := `c.owner_id = ?1`
	if shared {
		who = `c.owner_id <> ?1`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+reachColumns+` `+reachFrom+`
		WHERE `+reachWhere+` AND `+who+` AND (c.archived_at IS NOT NULL) = ?3
		ORDER BY c.updated_at DESC, c.id DESC`, user, workspace, archived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		c, err := scanReach(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) insert(ctx context.Context, workspace, user, agentID int64, title string) (Conversation, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO conversations (workspace_id, owner_id, agent_id, title, usage_mark)
		VALUES (?, ?, ?, ?, 0) RETURNING id`, workspace, user, agentID, title).Scan(&id); err != nil {
		return Conversation{}, err
	}
	return s.reach(ctx, workspace, user, id)
}

func (s *Service) update(ctx context.Context, workspace, user, id int64, title *string, archived *bool, cliPermissions *string) (Conversation, error) {
	err := s.db.QueryRowContext(ctx, `UPDATE conversations SET
		title = coalesce(?1, title),
		archived_at = CASE WHEN ?2 IS NULL THEN archived_at WHEN ?2 THEN coalesce(archived_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')) END,
		cli_permissions = coalesce(?6, cli_permissions),
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?3 AND workspace_id = ?4 AND owner_id = ?5
		AND EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = ?4 AND m.user_id = ?5)
		AND EXISTS (SELECT 1 FROM users u WHERE u.id = ?5 AND u.disabled_at IS NULL)
		RETURNING id`, title, archived, id, workspace, user, cliPermissions).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, errNotFound
	}
	if err != nil {
		return Conversation{}, err
	}
	return s.reach(ctx, workspace, user, id)
}

func (s *Service) touch(ctx context.Context, id, agentID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ? AND agent_id = ?`, id, agentID)
	return err
}
