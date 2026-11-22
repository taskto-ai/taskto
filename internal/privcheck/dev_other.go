//go:build !unix

package privcheck

import "io/fs"

// deviceOf has no device numbers to give here; SetuidFiles is for the Linux worker.
func deviceOf(fs.FileInfo) (uint64, bool) { return 0, false }
