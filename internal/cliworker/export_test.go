package cliworker

import (
	"fmt"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

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
	w.trackGroup(pgid, IdentityForTest(user), cli, slot)
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

const TestBoot = "00000000000000000000000000000b00"

const TestInstallation = "00000000000000000000000000000c00"

func IdentityForTest(user int64) string { return fmt.Sprintf("%032x", user) }

func AccountForTest(user int64) cliowner.Account {
	return cliowner.Account{User: user, Identity: IdentityForTest(user), Boot: TestBoot, Installation: TestInstallation}
}

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

func (w *Worker) AccountFileForTest(identity string) string { return w.accountFile(identity) }

func (w *Worker) CounterFileForTest() string { return w.counterFile() }

func (w *Worker) SetAllocateHookForTest(f func() error) { w.allocateHookForTest = f }

const FirstHomeForTest = firstHome

func (l Layout) LegacyDirForTest(user int64) string { return l.legacy(user) }

func (w *Worker) GroupsForTest() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.groups)
}

const MaxUIDForTest = maxUID

func (w *Worker) SetDialHookForTest(f func()) { w.dialHookForTest = f }

func (w *Worker) HelpersBuiltForTest() int64 { return w.helpersBuilt.Load() }

func (w *Worker) GatedForTest(user int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.gated(IdentityForTest(user), "", "")
}

func (w *Worker) SetAdoptHookForTest(f func() error) { w.adoptHookForTest = f }

func (c *Remote) TurnForTest(spec TurnSpec) *RemoteTurn { return &RemoteTurn{c: c, spec: spec} }

func (w *Worker) SetHandOffHookForTest(f func()) { w.handOffHookForTest = f }

func (w *Worker) CrashForTest() { w.closeVolume() }

func (w *Worker) RetireOnlyForTest(identity string) error { return w.retire(identity, 0) }

const MaxHomeForTest = maxHome

func (w *Worker) SetProvisionHookForTest(f func() error) { w.provisionHookForTest = f }

func (w *Worker) HoldCensusForTest() (release func()) {
	w.censusMu.Lock()
	return w.censusMu.Unlock
}

func (w *Worker) ReservedForTest(boot string) (int64, bool) {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	r, ok := w.reserved[boot]
	return r.seq, ok
}

func (w *Worker) LatestBootForTest() string { return w.latestBoot() }

func (w *Worker) SetArrivedHookForTest(f func(boot string)) { w.arrivedHookForTest = f }

func (w *Worker) SetRetiredHookForTest(f func(identity string)) { w.retiredHookForTest = f }

func (w *Worker) SetDetachHookForTest(f func() error) { w.detachHookForTest = f }
