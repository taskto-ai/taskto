package cliworker

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// A removal is the worker taking away files that turns and logins use: a deleted user's whole
// directory (Forget) or one account's config directory (Logout). A CLI running on them would
// go on with the login in its memory, write its sessions back into a directory that was just
// removed, and keep acting for a person who signed out or is gone. So a removal first refuses
// new turns and logins on what it removes, then stops the turns already running on it and
// waits for their process groups to end, and only then removes the files.
type removal struct {
	user int64
	// all covers every account of user; else the one account, cli and slot.
	all  bool
	cli  CLI
	slot Slot
}

func (r *removal) covers(user int64, cli CLI, slot Slot) bool {
	return r.user == user && (r.all || (r.cli == cli && r.slot == slot))
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

// beginRemoval starts r: no turn or login starts on what it covers until the returned end is
// called, and the turns running on it are stopped and waited for. When they don't end in time,
// or ctx ends, it ends r itself and returns an error: the files must stay where a CLI may
// still be using them.
func (w *Worker) beginRemoval(ctx context.Context, r *removal) (end func(), err error) {
	w.mu.Lock()
	w.removals = append(w.removals, r)
	var turns []*Turn
	for _, t := range w.turns {
		if r.covers(t.spec.User, t.spec.CLI, t.spec.Slot) {
			turns = append(turns, t)
		}
	}
	w.mu.Unlock()
	end = func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if i := slices.Index(w.removals, r); i >= 0 {
			w.removals = slices.Delete(w.removals, i, i+1)
		}
	}
	for _, t := range turns {
		t.stop()
	}
	// SIGTERM, the grace time, SIGKILL, then the pipes' own wait (runCLI).
	wait := time.NewTimer(w.cfg.StopGrace + 10*time.Second)
	defer wait.Stop()
	for _, t := range turns {
		select {
		case <-t.ended:
		case <-wait.C:
			end()
			return nil, fmt.Errorf("a turn on the account didn't stop: its files are kept")
		case <-ctx.Done():
			end()
			return nil, ctx.Err()
		}
	}
	// The turn is over when its CLI is reaped and its group has been sent SIGKILL: a process of
	// the group may still be finishing a system call, and could write a file after the removal.
	// Each group is polled until it is gone. One that outlives groupWait is let go with a
	// warning: it can be only zombies, which no one reaps in a container without an init
	// process, and which write nothing.
	for _, t := range turns {
		if err := waitGroupGone(ctx, t.group()); err != nil {
			end()
			return nil, err
		}
	}
	return end, nil
}

// groupWait bounds the wait for a stopped turn's process group to be empty.
const groupWait = 3 * time.Second

// waitGroupGone waits until no process of group pgid exists, for groupWait at most, or ctx.
func waitGroupGone(ctx context.Context, pgid int) error {
	deadline := time.NewTimer(groupWait)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for groupAlive(pgid) {
		select {
		case <-tick.C:
		case <-deadline.C:
			slog.Warn("cliworker: a stopped turn's process group is still listed; removing its files anyway", "pgid", pgid)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
