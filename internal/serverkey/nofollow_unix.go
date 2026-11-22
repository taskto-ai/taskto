//go:build unix

package serverkey

import "syscall"

// noFollow makes opening a symlink in place of the key file fail.
const noFollow = syscall.O_NOFOLLOW

// ownerOnly is whether file modes say who can read the file, so a looser one is refused.
const ownerOnly = true
