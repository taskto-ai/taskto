package cliworker

import "time"

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

// SetGroupWaitForTest sets how long a removal waits for process groups to hold no live process;
// a negative time stops the worker from listing groups at all, so that a test of what a removal
// waits for otherwise (a turn's end, a sign-in's) is not answered by the groups' wait.
func (w *Worker) SetGroupWaitForTest(d time.Duration) { w.groupWait = d }
