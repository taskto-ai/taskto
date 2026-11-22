package cliworker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"time"
)

// Permissions inside a turn (docs/agent-safety.md): by default a CLI asks before it runs a
// command, edits a file or fetches a page, and the question goes to the conversation's owner.
// Each CLI runs our hook program (`taskto cli-approve`, RunApprove) before a tool runs; it
// asks the worker on its approval socket (approve.go), and the worker answers at once (an
// AllowAll turn, or a call the read-only policy allows) or asks the owner: a
// EventPermission in the turn, and a PermissionRequest that Permissions lists and Answer
// answers.

// Permissions says whether a turn's CLI asks before it acts.
type Permissions string

// The permission modes of a turn.
const (
	// Ask (the default, also the zero value): the CLI asks, and the owner answers.
	Ask Permissions = "ask"
	// AllowAll: nothing asks. The owner turns this on for one conversation.
	AllowAll Permissions = "allow_all"
)

// Valid reports whether p is a known mode ("" is Ask).
func (p Permissions) Valid() bool { return p == "" || p == Ask || p == AllowAll }

// The states of a permission question, in EventPermission's Permission.Status.
const (
	PermissionAsked   = "asked"
	PermissionAllowed = "allowed"
	PermissionDenied  = "denied"
)

// Permission is a question to the owner, in an EventPermission: once when asked, and once
// more when it is answered (or denied for want of an answer).
type Permission struct {
	ID string `json:"id"`
	// Tool is the CLI's name for the tool (Bash, Edit, WebFetch, …).
	Tool string `json:"tool"`
	// Input is what the tool would do, shortened: a command, or the tool's input as JSON.
	Input  string `json:"input,omitempty"`
	Status string `json:"status"`
	// Reason says why it was denied, when it wasn't the owner.
	Reason string `json:"reason,omitempty"`
}

// PermissionRequest is a question waiting for its answer.
type PermissionRequest struct {
	ID      string    `json:"id"`
	Turn    string    `json:"turn"`
	CLI     CLI       `json:"cli"`
	Tool    string    `json:"tool"`
	Input   string    `json:"input,omitempty"`
	AskedAt time.Time `json:"asked_at"`
}

// ErrNoPermission is Answer's error for a question that isn't waiting (any more), or isn't
// in that user's conversation.
var ErrNoPermission = errors.New("no such permission question is waiting")

// pending is a question waiting in a turn.
type pending struct {
	req    PermissionRequest
	answer chan bool // buffered: the first answer wins
}

// maxPermissionInput bounds a question's input, for the UI.
const maxPermissionInput = 2 << 10

// ask asks t's owner whether the CLI may use tool, and waits for the answer, the end of the
// turn or AskTimeout, whichever comes first.
func (w *Worker) ask(t *Turn, tool, input string) (bool, string) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	p := &pending{
		req: PermissionRequest{
			ID: hex.EncodeToString(b), Turn: t.spec.ID, CLI: t.spec.CLI,
			Tool: cut(tool, 200), Input: cut(input, maxPermissionInput), AskedAt: time.Now().UTC(),
		},
		answer: make(chan bool, 1),
	}
	if !t.addPending(p) {
		return false, "the turn has ended"
	}
	timer := time.NewTimer(w.cfg.AskTimeout)
	defer timer.Stop()
	var allow bool
	var reason string
	select {
	case allow = <-p.answer:
		if !allow {
			reason = "the user denied it"
		}
	case <-t.ended:
		reason = "the turn has ended"
	case <-timer.C:
		reason = "nobody answered in " + w.cfg.AskTimeout.String()
	}
	t.removePending(p, allow, reason)
	return allow, reason
}

// addPending records p and announces it in the turn, unless the turn is over.
func (t *Turn) addPending(p *pending) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return false
	}
	if t.pending == nil {
		t.pending = map[string]*pending{}
	}
	t.pending[p.req.ID] = p
	t.push(Event{Type: EventPermission, Permission: &Permission{ID: p.req.ID, Tool: p.req.Tool, Input: p.req.Input, Status: PermissionAsked}})
	return true
}

// removePending forgets p and records how it was answered.
func (t *Turn) removePending(p *pending, allowed bool, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pending, p.req.ID)
	if t.done {
		return
	}
	status := PermissionDenied
	if allowed {
		status = PermissionAllowed
	}
	t.push(Event{Type: EventPermission, Permission: &Permission{ID: p.req.ID, Tool: p.req.Tool, Status: status, Reason: reason}})
}

// Permissions returns the questions waiting in user's conversation, oldest first.
func (w *Worker) Permissions(user, conversation int64) []PermissionRequest {
	out := []PermissionRequest{}
	for _, t := range w.turnsOf(user, conversation) {
		t.mu.Lock()
		for _, p := range t.pending {
			out = append(out, p.req)
		}
		t.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AskedAt.Before(out[j].AskedAt) })
	return out
}

// Answer answers question id in user's conversation: the CLI goes on with the tool, or is
// told the user denied it. A question that isn't waiting there is ErrNoPermission.
func (w *Worker) Answer(user, conversation int64, id string, allow bool) error {
	for _, t := range w.turnsOf(user, conversation) {
		t.mu.Lock()
		p, ok := t.pending[id]
		t.mu.Unlock()
		if ok {
			select {
			case p.answer <- allow:
			default: // answered already
			}
			return nil
		}
	}
	return ErrNoPermission
}

// turnsOf returns user's turns in conversation.
func (w *Worker) turnsOf(user, conversation int64) []*Turn {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []*Turn
	for _, t := range w.turns {
		if t.spec.User == user && t.spec.Conversation == conversation {
			out = append(out, t)
		}
	}
	return out
}
