package cliworker

import "context"

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

// BeginRemovalForTest starts a removal of user's files (every account, or cli's account in
// slot) as Forget and Logout do, and returns the function that ends it.
func (w *Worker) BeginRemovalForTest(ctx context.Context, user int64, all bool, cli CLI, slot Slot) (func(), error) {
	return w.beginRemoval(ctx, &removal{user: user, all: all, cli: cli, slot: slot})
}
