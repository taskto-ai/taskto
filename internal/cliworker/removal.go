package cliworker

import (
	"context"
	"errors"
	"slices"
	"time"
)

// A removal is the worker taking away files that turns, logins and helpers use: a deleted
// user's whole directory (Forget) or one account's config directory (Logout). A CLI running
// on them would go on with the login in its memory, write its sessions back into a directory
// that was just removed, and keep acting for a person who signed out or is gone; a helper
// would make the directories again. So a removal is a gate: from its start nothing new touches
// what it covers (a turn, a sign-in, a status check or any other helper is refused with
// ErrRemoving); what already does is stopped and waited for (the helpers, the turns, the
// sign-ins, and every process group the account started, finished turns' included); and only
// then are the files removed. Removals of the same account run one after the other.
type removal struct {
	user int64
	// all covers every account of user; else the one account, cli and slot.
	all  bool
	cli  CLI
	slot Slot
	done chan struct{} // closed when the removal has ended
}

func (r *removal) covers(user int64, cli CLI, slot Slot) bool {
	return r.user == user && (r.all || (r.cli == cli && r.slot == slot))
}

// overlaps reports whether r and o cover an account in common.
func (r *removal) overlaps(o *removal) bool {
	return r.user == o.user && (r.all || o.all || (r.cli == o.cli && r.slot == o.slot))
}

// ErrBusy is a removal's answer when a process of the account is still running after the wait:
// its files are kept, and the removal can be tried again.
var ErrBusy = errors.New("a process of this account is still running: its files are kept, try again")

// helperRun is a helper (a status check, a read of the account's files) that is running.
type helperRun struct {
	user   int64
	cli    CLI
	slot   Slot
	cancel context.CancelFunc
	done   chan struct{}
}

// groupRec is a process group a turn, a sign-in or a helper started, and whose account.
type groupRec struct {
	user int64
	cli  CLI
	slot Slot
}

// removing reports whether a removal covers the account of user's cli in slot. The caller
// holds w.mu.
func (w *Worker) removing(user int64, cli CLI, slot Slot) bool {
	for _, r := range w.removals {
		if r.covers(user, cli, slot) {
			return true
		}
	}
	return false
}

// admit lets a helper that works on the account of user's cli in slot start, or refuses it
// (ErrRemoving) while a removal covers the account. A removal that begins while it runs cancels
// the returned context and waits until release is called. For the user's own files (no CLI),
// only a removal of everything of theirs covers it.
func (w *Worker) admit(ctx context.Context, user int64, cli CLI, slot Slot) (context.Context, func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.removing(user, cli, slot) {
		return nil, nil, ErrRemoving
	}
	ctx, cancel := context.WithCancel(ctx)
	h := &helperRun{user: user, cli: cli, slot: slot, cancel: cancel, done: make(chan struct{})}
	w.helpers[h] = struct{}{}
	return ctx, func() {
		w.mu.Lock()
		delete(w.helpers, h)
		w.mu.Unlock()
		close(h.done)
		cancel()
	}, nil
}

// trackGroup records a process group started for the account of user's cli in slot, until a
// removal sees it empty. A group is never forgotten because its turn is over: what a turn left
// behind may still run.
func (w *Worker) trackGroup(pgid int, user int64, cli CLI, slot Slot) {
	if pgid <= 1 {
		return
	}
	w.mu.Lock()
	w.groups[pgid] = groupRec{user, cli, slot}
	var old []int
	for p := range w.groups {
		if p != pgid {
			old = append(old, p)
		}
	}
	w.mu.Unlock()
	// The groups that are over are let go here, so the list stays as long as what runs. A group
	// ID is not reused while the group has a member; one let go and reused later is waited
	// for by mistake at worst, which a removal's retry settles.
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

// beginRemoval starts r, once the removals it overlaps have ended: no turn, sign-in or helper
// starts on what it covers until the returned end is called, and what runs on it is stopped and
// waited for. When something doesn't end in time (ErrBusy), or ctx ends, it ends r itself and
// returns an error: the files must stay where a CLI may still be using them.
func (w *Worker) beginRemoval(ctx context.Context, r *removal) (end func(), err error) {
	r.done = make(chan struct{})
	for {
		w.mu.Lock()
		var other *removal
		for _, o := range w.removals {
			if r.overlaps(o) {
				other = o
				break
			}
		}
		if other == nil {
			break
		}
		w.mu.Unlock()
		select {
		case <-other.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	w.removals = append(w.removals, r)
	var turns []*Turn
	for _, t := range w.turns {
		if r.covers(t.spec.User, t.spec.CLI, t.spec.Slot) && !t.Over() {
			turns = append(turns, t)
		}
	}
	var helpers []*helperRun
	for h := range w.helpers {
		if r.covers(h.user, h.cli, h.slot) {
			helpers = append(helpers, h)
		}
	}
	// Every sign-in not over: one that was cancelled and replaced is still a process until it
	// exits, and is kept here until then.
	var logins []*Login
	for l := range w.loginRuns {
		if r.covers(l.user, l.cli, l.slot) {
			logins = append(logins, l)
		}
	}
	w.mu.Unlock()
	end = func() {
		w.mu.Lock()
		if i := slices.Index(w.removals, r); i >= 0 {
			w.removals = slices.Delete(w.removals, i, i+1)
			close(r.done)
		}
		w.mu.Unlock()
	}
	for _, h := range helpers {
		h.cancel()
	}
	for _, t := range turns {
		t.stop()
	}
	for _, l := range logins {
		l.markCancelled()
		l.cancel()
	}
	// The helpers are killed at once; a turn gets SIGTERM, the grace time, SIGKILL, then the
	// pipes' own wait (runCLI); a sign-in the same.
	wait := time.NewTimer(w.cfg.StopGrace + 10*time.Second)
	defer wait.Stop()
	fail := func(err error) (func(), error) {
		end()
		return nil, err
	}
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
			return fail(ErrBusy)
		case <-ctx.Done():
			return fail(ctx.Err())
		}
	}
	if err := w.waitGroups(ctx, r); err != nil {
		return fail(err)
	}
	return end, nil
}

// defaultGroupWait bounds the wait for the process groups of an account to hold no live process.
const defaultGroupWait = 3 * time.Second

// waitGroups waits until every process group started for what r covers holds no live process: a
// group that is empty, or has only zombies, which hold nothing and write nothing. That
// includes the groups of turns, sign-ins and helpers that are over: a turn's end kills its
// group, but a process of it may be finishing a system call, and could write a file after the
// removal. When one still has a live process after the wait, it is ErrBusy and the groups stay
// listed, so a retry waits for them again.
func (w *Worker) waitGroups(ctx context.Context, r *removal) error {
	deadline := time.NewTimer(w.groupWait)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		w.mu.Lock()
		var pgids []int
		for p, g := range w.groups {
			if r.covers(g.user, g.cli, g.slot) {
				pgids = append(pgids, p)
			}
		}
		w.mu.Unlock()
		var live []int
		for _, p := range pgids {
			if groupLive(p) {
				live = append(live, p)
			}
		}
		w.mu.Lock()
		for _, p := range pgids {
			if !slices.Contains(live, p) {
				delete(w.groups, p)
			}
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
