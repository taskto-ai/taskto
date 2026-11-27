//go:build !unix

package dictation

import "os/exec"

const groupsSupported = false

func newGroup(*exec.Cmd) {}
