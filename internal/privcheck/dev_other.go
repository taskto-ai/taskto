//go:build !unix

package privcheck

import "io/fs"

func deviceOf(fs.FileInfo) (uint64, bool) { return 0, false }
