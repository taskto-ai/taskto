package cliworker

import (
	"context"
	"errors"
	"slices"
	"time"
)

// Removing files that turns, sign-ins and helpers use (a deleted user's whole directory, Forget,
// or one disconnected account's config directory, Logout) must not race them. A CLI that goes on
// running keeps the login in its memory and writes its session back into a directory that was
// just removed, a helper or a new turn makes the directory again, and a person who signed out or
// is gone is still acted for. So a removal has three steps:
//
//  1. nothing new starts on what it covers: for Forget the user's durable mark (forget.go), for
//     Logout a gate that lasts as long as the removal (ErrRemoving);
//  2. what runs on it is stopped and waited for (quiesce): the helper requests in flight are
//     cancelled, the turns (queued ones included) and sign-ins (every execution, also a cancelled
//     one a new sign-in replaced) are stopped, and the process groups they started hold no live
//     process;
//  3. only then are the files removed.

// ErrRemoving is the answer of Start, StartLogin and the helper requests while the files of the
// account they would use are being removed (a disconnect): it is over in a moment.
var ErrRemoving = errors.New("this account's CLI files are being removed: try again in a moment")

// ErrBusy is a removal's answer when something of the account is still running after it was
// stopped and waited for: the files are kept, and the removal can be tried again.
var ErrBusy = errors.New("a process of this account is still running: its files are kept, try again")

// scope is what a removal covers: all of a user's accounts, or the one account of cli in slot.
type scope struct {
	user int64
	all  bool
	cli  CLI
	slot Slot
}

func (s scope) covers(user int64, cli CLI, slot Slot) bool {
	return s.user == user && (s.all || (s.cli == cli && s.slot == slot))
}

// overlaps reports whether s and o cover an account in common.
func (s scope) overlaps(o scope) bool {
	return s.user == o.user && (s.all || o.all || (s.cli == o.cli && s.slot == o.slot))
}

// gate is an account whose files are being removed by a Logout.
type gate struct {
	scope
	done chan struct{} // closed when the removal has ended
}

// helperRun is a helper request (a status check, a read of the account's files) that is running.
type helperRun struct {
	scope
	cancel context.CancelFunc
	done   chan struct{}
}

// groupRec is a process group started for an account.
type groupRec struct{ scope }

// gated reports whether a removal's gate covers the account of user's cli in slot. The caller
// holds w.mu.
func (w *Worker) gated(user int64, cli CLI, slot Slot) bool {
	for _, g := range w.gates {
		if g.covers(user, cli, slot) {
			return true
		}
	}
	return false
}

// admit lets a helper request for the account of user's cli in slot start, or refuses it: for a
// user that was forgotten (ErrForgotten) or an account that is being removed (ErrRemoving). A
// removal that begins while it runs cancels the returned context and waits until release is
// called. A request that names no CLI (the user's own files) is covered by a removal of all of
// the user's accounts only.
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

// maxGroups is how many process groups are listed before the ones that hold no live process
// are let go.
const maxGroups = 256

// trackGroup lists process group pgid, started for the account of user's cli in slot, until a
// removal sees it hold no live process. A group is not forgotten because its turn is over: what
// a turn left behind may still be running when it is killed.
func (w *Worker) trackGroup(pgid int, user int64, cli CLI, slot Slot) {
	if pgid <= 1 || w.groupWait < 0 {
		return
	}
	w.mu.Lock()
	w.groups[pgid] = groupRec{scope{user: user, cli: cli, slot: slot}}
	var old []int
	if len(w.groups) > maxGroups {
		for p := range w.groups {
			if p != pgid {
				old = append(old, p)
			}
		}
	}
	w.mu.Unlock()
	var over []int
	for _, p := range old {
		if !groupLive(p) {
			over = append(over, p)
		}
	}
	if len(over) > 0 {
		w.mu.Lock()
		for _, p := range over {
			delete(w.groups, p)
		}
		w.mu.Unlock()
	}
}

// beginGate closes the account of sc (one account) to new turns, sign-ins and helper requests,
// once a removal of an account in common has ended, and returns the function that opens it again.
func (w *Worker) beginGate(ctx context.Context, sc scope) (end func(), err error) {
	for {
		w.mu.Lock()
		if w.isForgotten(sc.user) {
			w.mu.Unlock()
			return nil, ErrForgotten
		}
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

// quiesce stops what runs on what sc covers and waits until nothing does: the caller has made
// sure that nothing new starts (the user's mark, or a gate). It is ErrBusy when something
// doesn't end in time; nothing has been removed then, and the caller keeps the files.
func (w *Worker) quiesce(ctx context.Context, sc scope) error {
	// Everything is read under the lock a start checks the gate or the mark under: a turn or a
	// sign-in is either refused or among these.
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
	// Every execution, not only the current login of each account: a cancelled one that a new
	// sign-in replaced is a process until it exits.
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
	// A helper is killed at once; a turn or a sign-in gets SIGTERM, the grace time, SIGKILL.
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

// defaultGroupWait is how long a removal waits for the process groups of an account to hold no
// live process.
const defaultGroupWait = 3 * time.Second

// waitGroups waits until every process group started for what sc covers holds no live process: a
// group that is gone, or holds only zombies (killed, waiting to be reaped: they hold no file and
// write nothing). That includes the groups of turns and sign-ins that are over: their end kills
// the group, but a process of it may be finishing a system call and could write a file after the
// removal. A group that still has one after the wait is ErrBusy, and stays listed, so a retry
// waits for it again.
func (w *Worker) waitGroups(ctx context.Context, sc scope) error {
	deadline := time.NewTimer(w.groupWait)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		w.mu.Lock()
		var pgids []int
		for p, g := range w.groups {
			if sc.covers(g.user, g.cli, g.slot) {
				pgids = append(pgids, p)
			}
		}
		w.mu.Unlock()
		var live, over []int
		for _, p := range pgids {
			if groupLive(p) {
				live = append(live, p)
			} else {
				over = append(over, p)
			}
		}
		w.mu.Lock()
		for _, p := range over {
			delete(w.groups, p)
		}
		w.mu.Unlock()
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
