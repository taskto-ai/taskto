package boards

import (
	"database/sql"
	"errors"
	"slices"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/schedule"
)

type Column string

const (
	Backlog  Column = "backlog"
	Todo     Column = "todo"
	Doing    Column = "doing"
	Review   Column = "review"
	Done     Column = "done"
	Archived Column = "archived"
)

var columns = []Column{Backlog, Todo, Doing, Review, Done, Archived}

func (c Column) valid() bool { return slices.Contains(columns, c) }

func (c Column) onBoard() bool { return c.valid() && c != Archived }

type Kind string

const (
	Personal Kind = "personal"
	Team     Kind = "team"
)

type Actor struct {
	UserID int64
	Agent  bool
	Via    string
}

type Author struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Agent bool   `json:"agent"`
	Via   string `json:"via,omitempty"`
}

type Board struct {
	ID         int64   `json:"id"`
	SpaceID    int64   `json:"space_id"`
	Kind       Kind    `json:"kind"`
	Name       string  `json:"name"`
	ArchivedAt *string `json:"archived_at"`
	Version    int64   `json:"version"`
	CreatedAt  string  `json:"created_at"`
	CreatedBy  *Author `json:"created_by"`
	UpdatedAt  string  `json:"updated_at"`
	UpdatedBy  *Author `json:"updated_by"`
}

type AssigneeKind string

const (
	AssignUser    AssigneeKind = "user"
	AssignAgent   AssigneeKind = "agent"
	AssignDeleted AssigneeKind = "deleted"
)

type Assignee struct {
	Kind AssigneeKind `json:"kind"`
	ID   int64        `json:"id,omitempty"`
	Name string       `json:"name,omitempty"`
}

type Card struct {
	ID            int64     `json:"id"`
	BoardID       int64     `json:"board_id"`
	Title         string    `json:"title"`
	Body          string    `json:"body"`
	Column        Column    `json:"column"`
	Rank          string    `json:"rank"`
	ArchivedFrom  *Column   `json:"archived_from"`
	Assignee      *Assignee `json:"assignee"`
	Blocked       bool      `json:"blocked"`
	BlockedReason *string   `json:"blocked_reason"`
	BlockedBy     []int64   `json:"blocked_by" apitypes:"nonnull"`
	Docs          []string  `json:"docs" apitypes:"nonnull"`
	Conversations []string  `json:"conversations" apitypes:"nonnull"`
	Due           *string   `json:"due"`
	Labels        []string  `json:"labels" apitypes:"nonnull"`
	Version       int64     `json:"version"`
	CreatedAt     string    `json:"created_at"`
	CreatedBy     *Author   `json:"created_by"`
	UpdatedAt     string    `json:"updated_at"`
	UpdatedBy     *Author   `json:"updated_by"`
}

type Comment struct {
	ID        int64             `json:"id"`
	CardID    int64             `json:"card_id"`
	Author    *Author           `json:"author"`
	Body      string            `json:"body"`
	CreatedAt string            `json:"created_at"`
	Mentions  []MentionedPerson `json:"mentions" apitypes:"nonnull"`
}

type MentionedPerson struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Thread struct {
	Card
	Comments []Comment `json:"comments" apitypes:"nonnull"`
}

var (
	ErrNotFound      = errors.New("not found")
	ErrReadOnly      = errors.New("you can only read this space")
	ErrBoardArchived = errors.New("this board is archived: it is read-only")
	ErrStale         = errors.New("it changed since you read it: reload and try again")
	ErrInvalid       = errors.New("invalid request")
)

type invalidError struct {
	msg    string
	code   apierr.Code
	params apierr.Params
}

func (e *invalidError) Error() string        { return e.msg }
func (e *invalidError) Is(target error) bool { return target == ErrInvalid }

func invalid(msg string) error { return &invalidError{msg: msg, code: apierr.CodeBadRequest} }

func invalidAs(code apierr.Code, params apierr.Params, msg string) error {
	return &invalidError{msg: msg, code: code, params: params}
}

type Service struct {
	db     *sql.DB
	access *access.Checker
	sched  *schedule.Service
}

func New(sqlDB *sql.DB, c *access.Checker) *Service {
	return &Service{db: sqlDB, access: c}
}
