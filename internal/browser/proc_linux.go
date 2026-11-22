//go:build linux

package browser

import "syscall"

// dieWithParent asks the kernel to kill the process if the thread that started it exits: a
// browser never outlives the server (local mode) or the worker (container mode, whose
// start-up sweep is the guarantee).
func dieWithParent(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }
