package cliworker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

type Permissions string

const (
	Ask      Permissions = "ask"
	AllowAll Permissions = "allow_all"
)

func (p Permissions) Valid() bool { return p == "" || p == Ask || p == AllowAll }

const (
	PermissionAsked   = "asked"
	PermissionAllowed = "allowed"
	PermissionDenied  = "denied"
)

type Permission struct {
	ID     string `json:"id"`
	Tool   string `json:"tool"`
	Input  string `json:"input,omitempty"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type PermissionRequest struct {
	ID      string    `json:"id"`
	Turn    string    `json:"turn"`
	CLI     CLI       `json:"cli"`
	Tool    string    `json:"tool"`
	Input   string    `json:"input,omitempty"`
	AskedAt time.Time `json:"asked_at"`
}

var ErrNoPermission = errors.New("no such permission question is waiting")

type pending struct {
	req    PermissionRequest
	answer chan bool
}

const maxPermissionInput = 2 << 10

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

func (w *Worker) Permissions(a cliowner.Account, conversation int64) []PermissionRequest {
	out := []PermissionRequest{}
	for _, t := range w.turnsOf(a, conversation) {
		t.mu.Lock()
		for _, p := range t.pending {
			out = append(out, p.req)
		}
		t.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AskedAt.Before(out[j].AskedAt) })
	return out
}

func (w *Worker) Answer(a cliowner.Account, conversation int64, id string, allow bool) error {
	for _, t := range w.turnsOf(a, conversation) {
		t.mu.Lock()
		p, ok := t.pending[id]
		t.mu.Unlock()
		if ok {
			select {
			case p.answer <- allow:
			default:
			}
			return nil
		}
	}
	return ErrNoPermission
}

func (w *Worker) turnsOf(a cliowner.Account, conversation int64) []*Turn {
	if w.retired(a.Identity) {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []*Turn
	for _, t := range w.turns {
		if t.acct.id == a.Identity && t.spec.User == a.User && t.spec.Conversation == conversation {
			out = append(out, t)
		}
	}
	return out
}
