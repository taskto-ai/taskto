package cliworker

import "time"

var SignInURLForTest = signInURL

var ReadOnlyForTest = readOnly

var RunApproveForTest = runApprove

func (w *Worker) LayoutForTest() Layout { return w.cfg.Layout }

var ApprovalSocketForTest = approvalSocket

func ToolsReadOnlyForTest(t *Tools, cli CLI, tool string, input []byte) bool {
	return t.readOnly(cli, tool, input)
}

func (w *Worker) HelperLaunchesForTest() int64 { return w.helperLaunches.Load() }

func (w *Worker) TrackGroupForTest(pgid int, user int64, cli CLI, slot Slot) {
	w.trackGroup(pgid, user, cli, slot)
}

func (w *Worker) DropStaleGroupForTest(pgid int, seq uint64) bool {
	w.dropGroups([]groupSeen{{pgid, seq}})
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.groups[pgid]
	return ok
}

func (w *Worker) GroupSeqForTest(pgid int) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.groups[pgid].seq
}

func (w *Worker) SetGroupWaitForTest(d time.Duration) { w.groupWait = d }
