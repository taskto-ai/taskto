package cliworker

import (
	"context"
	"sync"
)

const maxLogBytes = 4 << 20

type Turn struct {
	spec   TurnSpec
	acct   account
	cancel context.CancelFunc

	mu       sync.Mutex
	events   []Event
	size     int
	full     bool
	done     bool
	changed  chan struct{}
	ended    chan struct{}
	sessions map[string]bool
	unnamed  bool
	pending  map[string]*pending
}

func (t *Turn) startSession(id string) {
	t.mu.Lock()
	t.unnamed = id == ""
	t.mu.Unlock()
	t.addSession(id)
}

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

func (t *Turn) running(session string) (running, hasSession, unnamed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.done, t.sessions[session], t.unnamed
}

func (t *Turn) Spec() TurnSpec { return t.spec }

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

func (t *Turn) finish(done Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	done.Type = EventDone
	t.push(done)
	t.done = true
	clear(t.pending)
	close(t.ended)
	t.cancel()
}

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
