//go:build unix

package serverkey

import "syscall"

const noFollow = syscall.O_NOFOLLOW

const ownerOnly = true
