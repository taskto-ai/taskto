package cliworker

import (
	"fmt"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

// SignInURLForTest exposes signInURL to the package's external tests.
var SignInURLForTest = signInURL

// ReadOnlyForTest exposes the read-only policy.
var ReadOnlyForTest = readOnly

// RunApproveForTest exposes the hook program with its own stdin and outputs.
var RunApproveForTest = runApprove

// LayoutForTest returns the worker's layout.
func (w *Worker) LayoutForTest() Layout { return w.cfg.Layout }

// ApprovalSocketForTest exposes where the approval socket goes.
var ApprovalSocketForTest = approvalSocket

// ToolsReadOnlyForTest exposes the read-only policy for taskto's tools.
func ToolsReadOnlyForTest(t *Tools, cli CLI, tool string, input []byte) bool {
	return t.readOnly(cli, tool, input)
}

// HelperLaunchesForTest is how many helpers the worker has been asked to start.
func (w *Worker) HelperLaunchesForTest() int64 { return w.helperLaunches.Load() }

// TrackGroupForTest lists process group pgid as one the worker started for the CLI account of
// cli in slot of the test account of user (AccountForTest).
func (w *Worker) TrackGroupForTest(pgid int, user int64, cli CLI, slot Slot) {
	w.trackGroup(pgid, IdentityForTest(user), cli, slot)
}

// DropStaleGroupForTest is what a removal's wait does when it saw the record of group pgid, with
// the registration number seq, hold no live process: it lets the record go, unless the system gave
// the group ID again and the record is a newer registration's. It reports whether the record is
// still listed.
func (w *Worker) DropStaleGroupForTest(pgid int, seq uint64) bool {
	w.dropGroups([]groupSeen{{pgid, seq}})
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.groups[pgid]
	return ok
}

// GroupSeqForTest is the registration number of the record listed for group pgid, 0 for none.
func (w *Worker) GroupSeqForTest(pgid int) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.groups[pgid].seq
}

// SetGroupWaitForTest sets how long a removal waits for process groups to hold no live process;
// a negative time stops the worker from listing groups at all, so that a test of what a removal
// waits for otherwise (a turn's end, a sign-in's) is not answered by the groups' wait.
func (w *Worker) SetGroupWaitForTest(d time.Duration) { w.groupWait = d }

// TestBoot is the server boot the tests' requests carry.
const TestBoot = "00000000000000000000000000000b00"

// IdentityForTest is the identity the tests give user's account.
func IdentityForTest(user int64) string { return fmt.Sprintf("%032x", user) }

// AccountForTest is the account the tests give user.
func AccountForTest(user int64) cliowner.Account {
	return cliowner.Account{User: user, Identity: IdentityForTest(user), Boot: TestBoot}
}

// HomeForTest is a's home number: its record's, retired or not, or a new one.
func (w *Worker) HomeForTest(a cliowner.Account) int64 {
	if r, ok, err := w.find(a.Identity); err == nil && ok {
		return r.Home
	}
	acct, err := w.resolve(a, true)
	if err != nil {
		panic(err)
	}
	return acct.home
}

// AccountFileForTest is the path of identity's record.
func (w *Worker) AccountFileForTest(identity string) string { return w.accountFile(identity) }

// CounterFileForTest is the path of the home counter.
func (w *Worker) CounterFileForTest() string { return w.counterFile() }

// SetAllocateHookForTest has f run once a home number is taken, before it is recorded; an error
// from it stops the allocation there, as a crash would.
func (w *Worker) SetAllocateHookForTest(f func() error) { w.allocateHookForTest = f }

// FirstHomeForTest is the first home number.
const FirstHomeForTest = firstHome

// LegacyDirForTest is the directory a worker from before homes made for user.
func (l Layout) LegacyDirForTest(user int64) string { return l.legacy(user) }

// GroupsForTest is how many process groups the worker lists.
func (w *Worker) GroupsForTest() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.groups)
}

// MaxUIDForTest is the uid every user's uid stays below.
const MaxUIDForTest = maxUID

// SetDialHookForTest has f run in a live view's dial between its admission and its helper.
func (w *Worker) SetDialHookForTest(f func()) { w.dialHookForTest = f }

// HelpersBuiltForTest is how many helpers passed every check to be built.
func (w *Worker) HelpersBuiltForTest() int64 { return w.helpersBuilt.Load() }

// GatedForTest reports whether a removal of all of the test account of user's CLI accounts has
// closed its gate.
func (w *Worker) GatedForTest(user int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gated(IdentityForTest(user), "", "")
}

// SetAdoptHookForTest has f run once a directory from before homes is moved to its account's home,
// before the account's record says so; an error from it stops there, as a crash would.
func (w *Worker) SetAdoptHookForTest(f func() error) { w.adoptHookForTest = f }

// TurnForTest is a client's handle on the turn spec names, as if the server had it: for a test of
// what the worker answers a handle of another account's.
func (c *Remote) TurnForTest(spec TurnSpec) *RemoteTurn { return &RemoteTurn{c: c, spec: spec} }

// SetHandOffHookForTest has f run in a live view's dial once its helper is connected, before the
// connection's watch for a removal is set.
func (w *Worker) SetHandOffHookForTest(f func()) { w.handOffHookForTest = f }

// CrashForTest lets the worker's volume go as a crashed worker's process would: its lock is
// released, and nothing else is stopped.
func (w *Worker) CrashForTest() { w.closeVolume() }

// RetireOnlyForTest records identity as retired and does nothing else: as a removal that failed
// before it dropped what the worker holds of the account.
func (w *Worker) RetireOnlyForTest(identity string) error { return w.retire(identity, 0) }

// MaxHomeForTest is the first home number there is none above.
const MaxHomeForTest = maxHome

// SetProvisionHookForTest has f run as an isolated home is made, once it is owned and before it is
// renamed into place; an error from it stops there, as a crash would.
func (w *Worker) SetProvisionHookForTest(f func() error) { w.provisionHookForTest = f }
