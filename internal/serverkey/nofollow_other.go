//go:build !unix

package serverkey

// noFollow is 0: there is no O_NOFOLLOW outside Unix.
const noFollow = 0

// ownerOnly is false: outside Unix, file modes don't say who can read a file.
const ownerOnly = false
