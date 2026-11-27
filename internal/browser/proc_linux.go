//go:build linux

package browser

import "syscall"

func dieWithParent(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }
