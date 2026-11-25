// Package boards is the to-do side of TaskTo.AI: boards of cards in columns, each card
// with an assignee (a person or the agent), a blocked state, a due date, labels, links to
// docs and conversations, and a comment thread.
//
// A board lives in a space, and access to it is access to the space, as package access
// answers it:
//
//   - every private space has one personal board, created with the space (migration
//     0005); its owner is the only one who reads or writes it;
//   - a shared space has any number of team boards: its editors create, change and
//     archive them and their cards, its viewers read them;
//   - a board or card the caller can't read answers ErrNotFound, like a space; boards of
//     an archived space, and archived boards, are read-only.
//
// Changes are optimistic: boards and cards carry a version, bumped by every change, and
// an update names the version it was made against. If it isn't the current one the
// update fails with ErrStale and changes nothing.
//
// HTTP handlers (http.go) and the agent's tools (tools.go) share the code below; the
// tools take an Actor and check its user's access to the space themselves.
package boards

import (
	"database/sql"
	"errors"
	"slices"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/schedule"
)

// Column is where a card is on its board.
type Column string

// The columns, in board order. Archived cards are off the board; archiving a card
// remembers its column, so restoring it puts it back there.
const (
	Backlog  Column = "backlog"
	Todo     Column = "todo"
	Doing    Column = "doing"
	Review   Column = "review"
	Done     Column = "done"
	Archived Column = "archived"
)

// columns lists every column, in board order.
var columns = []Column{Backlog, Todo, Doing, Review, Done, Archived}

func (c Column) valid() bool { return slices.Contains(columns, c) }

// onBoard reports whether c is a column cards can be created and moved in: any but
// Archived.
func (c Column) onBoard() bool { return c.valid() && c != Archived }

// Kind is a board's kind: a private space has its one personal board, a shared space
// any number of team boards.
type Kind string

// The kinds of board.
const (
	Personal Kind = "personal"
	Team     Kind = "team"
)

// Actor is who makes a change: a user, or the agent working for one. The agent has
// exactly its user's access, and changes it makes are marked as the agent's. An agent of
// the user's own reaching the server with an access token is one too: Via is then the
// token's name.
type Actor struct {
	UserID int64
	Agent  bool
	Via    string
}

// Author is who made something: a user, through the agent if Agent is set, and with the
// access token named Via if one was used. It is nil in replies if the user's account no
// longer exists.
type Author struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Agent bool   `json:"agent"`
	Via   string `json:"via,omitempty"`
}

// Board is a board of cards.
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

// AssigneeKind says whether a card is assigned to a person or to the agent.
type AssigneeKind string

// The kinds of assignee.
const (
	AssignUser  AssigneeKind = "user"
	AssignAgent AssigneeKind = "agent"
)

// Assignee is who a card is assigned to. In requests, ID names the user for AssignUser
// and Name is ignored.
type Assignee struct {
	Kind AssigneeKind `json:"kind"`
	ID   int64        `json:"id,omitempty"`
	Name string       `json:"name,omitempty"`
}

// Card is a card, without its thread.
type Card struct {
	ID      int64  `json:"id"`
	BoardID int64  `json:"board_id"`
	Title   string `json:"title"`
	// Body is Markdown.
	Body   string `json:"body"`
	Column Column `json:"column"`
	// Rank orders the cards of a column: compare ranks as byte strings. Clients treat it
	// as opaque, and move cards by position.
	Rank string `json:"rank"`
	// ArchivedFrom is an archived card's column before it was archived.
	ArchivedFrom *Column   `json:"archived_from"`
	Assignee     *Assignee `json:"assignee"`
	// Blocked is set when the card has a blocked reason or cards it waits for.
	Blocked       bool    `json:"blocked"`
	BlockedReason *string `json:"blocked_reason"`
	BlockedBy     []int64 `json:"blocked_by" apitypes:"nonnull"`
	// Docs are knowledge-base paths in the board's space; Conversations are conversation
	// IDs. Neither is checked to exist.
	Docs          []string `json:"docs" apitypes:"nonnull"`
	Conversations []string `json:"conversations" apitypes:"nonnull"`
	// Due is a calendar date, YYYY-MM-DD, with no time or time zone; nil for none.
	Due *string `json:"due"`
	// Labels are the card's free-text labels, normalized (NFC, case folded), sorted; never nil.
	Labels    []string `json:"labels" apitypes:"nonnull"`
	Version   int64    `json:"version"`
	CreatedAt string   `json:"created_at"`
	CreatedBy *Author  `json:"created_by"`
	UpdatedAt string   `json:"updated_at"`
	UpdatedBy *Author  `json:"updated_by"`
}

// Comment is a message in a card's thread.
type Comment struct {
	ID        int64   `json:"id"`
	CardID    int64   `json:"card_id"`
	Author    *Author `json:"author"`
	Body      string  `json:"body"`
	CreatedAt string  `json:"created_at"`
	// Mentions are the people the body mentions (`@[label](user:ID)`) whom the reader may see in
	// the space, by ID, with the name the server has for them: the name to show, since the label
	// in the text is whatever its writer typed. Never nil.
	Mentions []MentionedPerson `json:"mentions" apitypes:"nonnull"`
}

// MentionedPerson is a person a comment mentions, as the server names them.
type MentionedPerson struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Thread is a card with its comments, oldest first.
type Thread struct {
	Card
	Comments []Comment `json:"comments" apitypes:"nonnull"`
}

var (
	// ErrNotFound is the answer for a space, board or card that doesn't exist or that
	// the user can't read: callers must not tell the two apart.
	ErrNotFound = errors.New("not found")
	// ErrReadOnly is for a write to a space the user may only read.
	ErrReadOnly = errors.New("you can only read this space")
	// ErrBoardArchived is for a change to an archived board or its cards.
	ErrBoardArchived = errors.New("this board is archived: it is read-only")
	// ErrStale is for an update made against a version that is no longer the current
	// one: someone else changed the board or card in between.
	ErrStale = errors.New("it changed since you read it: reload and try again")
	// ErrInvalid matches every validation error (errors.Is); their messages say what is
	// wrong.
	ErrInvalid = errors.New("invalid request")
)

// invalidError is a validation error with a message for the caller. The API answers it with
// its code and params, for the web app to say it in the user's language; the agent's tools
// give the model the message. Errors only a broken client causes have apierr.CodeBadRequest.
type invalidError struct {
	msg    string
	code   apierr.Code
	params apierr.Params
}

func (e *invalidError) Error() string        { return e.msg }
func (e *invalidError) Is(target error) bool { return target == ErrInvalid }

// invalid is a validation error only a broken client causes.
func invalid(msg string) error { return &invalidError{msg: msg, code: apierr.CodeBadRequest} }

// invalidAs is a validation error people can cause, with its code and the params of its
// translated message.
func invalidAs(code apierr.Code, params apierr.Params, msg string) error {
	return &invalidError{msg: msg, code: code, params: params}
}

// Service stores boards, and checks access to them through package access.
type Service struct {
	db     *sql.DB
	access *access.Checker
	// sched schedules the due-date reminders (reminders.go); nil makes none.
	sched *schedule.Service
}

// New returns the boards service over sqlDB, which must be migrated.
func New(sqlDB *sql.DB, c *access.Checker) *Service {
	return &Service{db: sqlDB, access: c}
}
