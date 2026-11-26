package cliworker

import (
	"context"
	"sync"
)

// maxLogBytes bounds what a turn keeps of its text, thinking and tool events for
// re-attaching. Past it those are dropped from the log (still streamed to readers already
// past them), and the others, which carry the session, usage, login and the end, are kept.
const maxLogBytes = 4 << 20

// Turn is one CLI turn: its spec and the log of its events.
type Turn struct {
	spec   TurnSpec
	cancel context.CancelFunc

	mu      sync.Mutex
	events  []Event
	size    int
	full    bool
	done    bool
	changed chan struct{} // closed and replaced on every new event
	ended   chan struct{} // closed when the turn is over
	// sessions are the CLI sessions this turn runs (one, or two when a lost session was
	// replaced), by which the approval socket finds the turn of a hook's question.
	sessions map[string]bool
	// unnamed: the CLI runs a session it hasn't named yet. A new Codex thread gets its ID
	// from Codex, which prints it on stdout, so a hook can ask before the worker has read it.
	unnamed bool
	pending map[string]*pending // permission questions waiting for the owner
}

// startSession records the session a run of the CLI starts with: id, or, when id is empty,
// one the CLI will name (addSession).
func (t *Turn) startSession(id string) {
	t.mu.Lock()
	t.unnamed = id == ""
	t.mu.Unlock()
	t.addSession(id)
}

// addSession records that the turn runs CLI session id.
func (t *Turn) addSession(id string) {
	if id == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sessions == nil {
		t.sessions = map[string]bool{}
	}
	t.sessions[id] = true
	t.unnamed = false
}

// running reports whether the turn is still running, whether it runs session id, and
// whether its CLI runs a session it hasn't named yet.
func (t *Turn) running(session string) (running, hasSession, unnamed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.done, t.sessions[session], t.unnamed
}

// Spec returns what the turn runs.
func (t *Turn) Spec() TurnSpec { return t.spec }

// Over reports whether the turn has ended: its CLI is no longer running.
func (t *Turn) Over() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

func (t *Turn) stop() { t.cancel() }

func (t *Turn) emit(e Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.append(e)
}

func (t *Turn) append(e Event) {
	switch e.Type {
	case EventText, EventThinking, EventTool:
		n := len(e.Text)
		if e.Tool != nil {
			n += len(e.Tool.Input) + len(e.Tool.Output)
		}
		if t.full || t.size+n > maxLogBytes {
			if !t.full {
				t.full = true
				t.push(Event{Type: EventNotice, Text: "this turn's output is too long to show in full: the rest is in the CLI's own session"})
			}
			return
		}
		t.size += n
	}
	t.push(e)
}

func (t *Turn) push(e Event) {
	e.Seq = len(t.events) + 1
	t.events = append(t.events, e)
	close(t.changed)
	t.changed = make(chan struct{})
}

// finish appends the closing EventDone, once.
func (t *Turn) finish(done Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	done.Type = EventDone
	t.push(done)
	t.done = true
	// Its questions end with it, in the same step: each ask wakes on ended and removes its own
	// only later, so until then a finished turn would still list a question nobody can answer.
	clear(t.pending)
	close(t.ended)
	t.cancel() // release the context's resources
}

// Events returns the events after Seq after, whether the turn is over (its EventDone among
// them or earlier), and a channel closed when there is more.
func (t *Turn) Events(after int) (events []Event, done bool, more <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if after < 0 {
		after = 0
	}
	if after < len(t.events) {
		events = append([]Event(nil), t.events[after:]...)
	}
	return events, t.done, t.changed
}

// Wait returns the events after Seq after, waiting until there is at least one, the turn is
// over, or ctx ends.
func (t *Turn) Wait(ctx context.Context, after int) ([]Event, bool, error) {
	for {
		events, done, more := t.Events(after)
		if len(events) > 0 || done {
			return events, done, nil
		}
		select {
		case <-more:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
}
