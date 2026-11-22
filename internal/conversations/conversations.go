// Package conversations is the API for conversations with the agent: who owns each one, and
// passing a caller's requests on to the agent runtime (internal/agentrt), whose sidecar keeps
// the content and knows nothing about users.
//
// A conversation belongs to one user in one workspace and is private to them: every route
// checks the caller is its owner and a current member of the workspace, and answers 404
// otherwise. A model choice passes providers.Usable first. The routes are in docs/api.md.
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

// Runtime is the agent runtime as conversations use it: an *agentrt.Client (see Client), or a
// fake in tests.
type Runtime interface {
	CreateConversationOn(ctx context.Context, model agentrt.ModelRef) (int64, error)
	Submit(ctx context.Context, conversation int64, in agentrt.Input) (int64, error)
	Messages(ctx context.Context, conversation int64) ([]agentrt.Message, error)
	Model(ctx context.Context, conversation int64) (agentrt.ModelRef, error)
	SetModel(ctx context.Context, conversation int64, model agentrt.ModelRef) error
	Abort(ctx context.Context, conversation int64) error
	Entries(ctx context.Context, conversation, before int64, limit int) (json.RawMessage, error)
	Watch(ctx context.Context, conversation int64) (Watch, error)
}

// Watch is an open watch stream of the agent runtime (*agentrt.Watch).
type Watch interface {
	Next() (agentrt.Event, error)
	Close() error
}

// Client adapts an agentrt.Client to Runtime.
func Client(c *agentrt.Client) Runtime { return clientRuntime{c} }

type clientRuntime struct{ *agentrt.Client }

func (c clientRuntime) Watch(ctx context.Context, conversation int64) (Watch, error) {
	w, err := c.Client.Watch(ctx, conversation)
	if err != nil {
		return nil, err // not a nil *agentrt.Watch in a non-nil interface
	}
	return w, nil
}

// Agent returns the agent runtime once it answers, waiting while it starts or restarts until
// ctx ends.
type Agent func(ctx context.Context) (Runtime, error)

// Service serves the conversation API.
type Service struct {
	db    *sql.DB
	agent Agent // nil when the agent runtime is off
	// capability makes a conversation's capability for the agent's tools; nil: none (see
	// SetCapabilities).
	capability func(conversation, agentConversation int64) string
	// questions are the CLI workers' permission questions; nil: none (see SetCLIQuestions).
	questions CLIQuestions
	// language is the caller's language by default (auth.Service.Language, set by Routes),
	// for the agent's replies (docs/i18n.md#the-agent-and-dictation).
	language func(r *http.Request, browser string) string

	// closing ends every event stream once the server shuts down (see EndStreams).
	closing    context.Context
	endStreams context.CancelFunc

	// streams counts each user's open event streams, against maxStreams.
	mu      sync.Mutex
	streams map[int64]int

	// Tunables; tests shorten them.
	readyWait    time.Duration // how long a request waits for a starting agent runtime
	heartbeat    time.Duration // between comments on an idle event stream
	writeTimeout time.Duration // for the client to take one event before it is dropped
	maxStreams   int           // open event streams per user, across their conversations
}

// New returns the API over the database sqlDB, which must be migrated. agent is nil when the
// agent runtime is off: the routes that need it then answer 503.
func New(sqlDB *sql.DB, agent Agent) *Service {
	closing, end := context.WithCancel(context.Background())
	return &Service{
		db: sqlDB, agent: agent, closing: closing, endStreams: end, streams: map[int64]int{},
		readyWait: 5 * time.Second, heartbeat: 15 * time.Second, writeTimeout: time.Minute,
		// Enough for a handful of tabs; each stream pins a sidecar watch.
		maxStreams: 16,
	}
}

// openStream counts a new event stream for user, or reports false if they have
// maxStreams open already. Call the returned func when the stream ends.
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

// EndStreams ends every open event stream, telling each client, so they don't hold up a
// server that is shutting down. Register it with http.Server.RegisterOnShutdown.
func (s *Service) EndStreams() { s.endStreams() }

// Conversation is a conversation as the API shows it.
type Conversation struct {
	ID          int64   `json:"id"`
	WorkspaceID int64   `json:"workspace_id"`
	Title       string  `json:"title"`
	ArchivedAt  *string `json:"archived_at"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
	// CLIPermissions is "ask" (a CLI asks its owner before it acts) or "allow_all"
	// (docs/agent-safety.md).
	CLIPermissions string `json:"cli_permissions"`

	agentID int64 // the agent runtime's ID, never shown
}

var errNotFound = errors.New("conversation not found")

const columns = `id, workspace_id, agent_id, title, archived_at, created_at, updated_at, cli_permissions`

func scan(row interface{ Scan(...any) error }) (Conversation, error) {
	var c Conversation
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.agentID, &c.Title, &c.ArchivedAt, &c.CreatedAt, &c.UpdatedAt, &c.CLIPermissions)
	return c, err
}

// find returns conversation id if user owns it in workspace and is a member there now, or
// errNotFound. Membership is checked here too, not only by the middleware, so a long event
// stream can ask again.
func (s *Service) find(ctx context.Context, workspace, user, id int64) (Conversation, error) {
	c, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM conversations c
		WHERE id = ? AND workspace_id = ? AND owner_id = ?
		AND EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = c.workspace_id AND m.user_id = c.owner_id)`,
		id, workspace, user))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, errNotFound
	}
	return c, err
}

// list returns user's conversations in workspace, the most recently updated first: the
// archived ones or the others.
func (s *Service) list(ctx context.Context, workspace, user int64, archived bool) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM conversations
		WHERE workspace_id = ? AND owner_id = ? AND (archived_at IS NOT NULL) = ?
		ORDER BY updated_at DESC, id DESC`, workspace, user, archived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Conversation{}
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) insert(ctx context.Context, workspace, user, agentID int64, title string) (Conversation, error) {
	return scan(s.db.QueryRowContext(ctx, `INSERT INTO conversations (workspace_id, owner_id, agent_id, title)
		VALUES (?, ?, ?, ?) RETURNING `+columns, workspace, user, agentID, title))
}

// update renames, archives or unarchives conversation id, and marks it updated. A nil
// argument leaves that as it is; archiving an archived conversation keeps its archived_at.
// The statement repeats find's conditions (owner, workspace, still a member), so access
// lost after the lookup can't slip a change through: it is ErrNotFound then.
func (s *Service) update(ctx context.Context, workspace, user, id int64, title *string, archived *bool, cliPermissions *string) (Conversation, error) {
	c, err := scan(s.db.QueryRowContext(ctx, `UPDATE conversations SET
		title = coalesce(?1, title),
		archived_at = CASE WHEN ?2 IS NULL THEN archived_at WHEN ?2 THEN coalesce(archived_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')) END,
		cli_permissions = coalesce(?6, cli_permissions),
		updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?3 AND workspace_id = ?4 AND owner_id = ?5
		AND EXISTS (SELECT 1 FROM memberships m WHERE m.workspace_id = ?4 AND m.user_id = ?5)
		RETURNING `+columns, title, archived, id, workspace, user, cliPermissions))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, errNotFound
	}
	return c, err
}

// touch marks conversation id updated, after a message.
func (s *Service) touch(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, id)
	return err
}
