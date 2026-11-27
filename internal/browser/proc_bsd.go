//go:build unix && !linux

package browser

import "syscall"

func dieWithParent(*syscall.SysProcAttr) {}
