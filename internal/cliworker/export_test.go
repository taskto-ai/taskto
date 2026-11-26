package cliworker

import (
	"fmt"
	"time"
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

// TrackGroupForTest lists process group pgid as one the worker started for the account of
// user's cli in slot.
func (w *Worker) TrackGroupForTest(pgid int, user int64, cli CLI, slot Slot) {
	w.trackGroup(pgid, user, cli, slot)
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

// TestBoot is the server boot the tests introduce users with.
const TestBoot = "00000000000000000000000000000b00"

// IdentityForTest is the identity the tests give user's account.
func IdentityForTest(user int64) string { return fmt.Sprintf("%032x", user) }

// OwnerFileForTest is the path of user's owner record.
func (w *Worker) OwnerFileForTest(user int64) string { return w.ownerFile(user) }

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

// GatedForTest reports whether a removal of all of user's accounts has closed their gate.
func (w *Worker) GatedForTest(user int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gated(user, "", "")
}
