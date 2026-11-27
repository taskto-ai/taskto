//go:build unix

package privcheck

import (
	"io/fs"
	"syscall"
)

func deviceOf(info fs.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return deviceNumber(st.Dev), true
}

func deviceNumber[T ~int32 | ~uint32 | ~int64 | ~uint64](dev T) uint64 { return uint64(dev) }
