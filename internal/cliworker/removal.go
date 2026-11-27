package cliworker

import (
	"context"
	"errors"
	"slices"
	"time"
)

var ErrRemoving = errors.New("this account's CLI files are being removed: try again in a moment")

var ErrBusy = errors.New("a process of this account is still running: its files are kept, try again")

type scope struct {
	user int64
	all  bool
	cli  CLI
	slot Slot
}

func (s scope) covers(user int64, cli CLI, slot Slot) bool {
	return s.user == user && (s.all || (s.cli == cli && s.slot == slot))
}

func (s scope) overlaps(o scope) bool {
	return s.user == o.user && (s.all || o.all || (s.cli == o.cli && s.slot == o.slot))
}

type gate struct {
	scope
	done chan struct{}
}

type helperRun struct {
	scope
	cancel context.CancelFunc
	done   chan struct{}
}

type groupRec struct {
	scope
	seq uint64
}

type groupSeen struct {
	pgid int
	seq  uint64
}

func (w *Worker) dropGroups(seen []groupSeen) {
	if len(seen) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, g := range seen {
		if rec, ok := w.groups[g.pgid]; ok && rec.seq == g.seq {
			delete(w.groups, g.pgid)
		}
	}
}

func (w *Worker) gated(user int64, cli CLI, slot Slot) bool {
	for _, g := range w.gates {
		if g.covers(user, cli, slot) {
			return true
		}
	}
	return false
}

func (w *Worker) admit(ctx context.Context, user int64, cli CLI, slot Slot) (context.Context, func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.isForgotten(user) {
		return nil, nil, ErrForgotten
	}
	if w.gated(user, cli, slot) {
		return nil, nil, ErrRemoving
	}
	ctx, cancel := context.WithCancel(ctx)
	h := &helperRun{scope: scope{user: user, cli: cli, slot: slot}, cancel: cancel, done: make(chan struct{})}
	w.helpers[h] = struct{}{}
	return ctx, func() {
		w.mu.Lock()
		delete(w.helpers, h)
		w.mu.Unlock()
		close(h.done)
		cancel()
	}, nil
}

const maxGroups = 256

func (w *Worker) trackGroup(pgid int, user int64, cli CLI, slot Slot) {
	if pgid <= 1 || w.groupWait < 0 {
		return
	}
	w.mu.Lock()
	w.groupSeq++
	w.groups[pgid] = groupRec{scope: scope{user: user, cli: cli, slot: slot}, seq: w.groupSeq}
	var old []groupSeen
	if len(w.groups) > maxGroups {
		for p, rec := range w.groups {
			if p != pgid {
				old = append(old, groupSeen{p, rec.seq})
			}
		}
	}
	w.mu.Unlock()
	var over []groupSeen
	for _, g := range old {
		if !groupLive(g.pgid) {
			over = append(over, g)
		}
	}
	w.dropGroups(over)
}

func (w *Worker) beginGate(ctx context.Context, sc scope) (end func(), err error) {
	for {
		w.mu.Lock()
		var other *gate
		for _, o := range w.gates {
			if sc.overlaps(o.scope) {
				other = o
				break
			}
		}
		if other == nil {
			g := &gate{scope: sc, done: make(chan struct{})}
			w.gates = append(w.gates, g)
			w.mu.Unlock()
			return func() {
				w.mu.Lock()
				if i := slices.Index(w.gates, g); i >= 0 {
					w.gates = slices.Delete(w.gates, i, i+1)
					close(g.done)
				}
				w.mu.Unlock()
			}, nil
		}
		w.mu.Unlock()
		select {
		case <-other.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (w *Worker) quiesce(ctx context.Context, sc scope) error {
	w.mu.Lock()
	var turns []*Turn
	for _, t := range w.turns {
		if sc.covers(t.spec.User, t.spec.CLI, t.spec.Slot) && !t.Over() {
			turns = append(turns, t)
		}
	}
	var helpers []*helperRun
	for h := range w.helpers {
		if sc.covers(h.user, h.cli, h.slot) {
			helpers = append(helpers, h)
		}
	}
	var logins []*Login
	for l := range w.loginRuns {
		if sc.covers(l.user, l.cli, l.slot) {
			logins = append(logins, l)
		}
	}
	w.mu.Unlock()

	for _, h := range helpers {
		h.cancel()
	}
	for _, t := range turns {
		t.stop()
	}
	for _, l := range logins {
		l.stop()
	}
	wait := time.NewTimer(w.cfg.StopGrace + 10*time.Second)
	defer wait.Stop()
	pending := make([]<-chan struct{}, 0, len(helpers)+len(turns)+len(logins))
	for _, h := range helpers {
		pending = append(pending, h.done)
	}
	for _, t := range turns {
		pending = append(pending, t.ended)
	}
	for _, l := range logins {
		pending = append(pending, l.done)
	}
	for _, c := range pending {
		select {
		case <-c:
		case <-wait.C:
			return ErrBusy
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return w.waitGroups(ctx, sc)
}

const defaultGroupWait = 3 * time.Second

func (w *Worker) waitGroups(ctx context.Context, sc scope) error {
	deadline := time.NewTimer(w.groupWait)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		w.mu.Lock()
		var seen []groupSeen
		for p, g := range w.groups {
			if sc.covers(g.user, g.cli, g.slot) {
				seen = append(seen, groupSeen{p, g.seq})
			}
		}
		w.mu.Unlock()
		var live, over []groupSeen
		for _, g := range seen {
			if groupLive(g.pgid) {
				live = append(live, g)
			} else {
				over = append(over, g)
			}
		}
		w.dropGroups(over)
		if len(live) == 0 {
			return nil
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return ErrBusy
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
