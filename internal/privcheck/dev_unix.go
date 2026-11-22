//go:build unix

package privcheck

import (
	"io/fs"
	"syscall"
)

// deviceOf returns the device of the file system info's file is on.
func deviceOf(info fs.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true //nolint:gosec,unconvert,nolintlint // G115: never negative; int32 on darwin, uint64 on linux
}
