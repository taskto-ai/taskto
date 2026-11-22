//go:build unix && !linux

package browser

import "syscall"

// dieWithParent has no equivalent here; Chromium still exits when its DevTools pipe closes.
func dieWithParent(*syscall.SysProcAttr) {}
