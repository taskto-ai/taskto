//go:build !unix

package dictation

import "os/exec"

// Local engines and add-ons need process groups: Unix only.
const groupsSupported = false

func newGroup(*exec.Cmd) {}
