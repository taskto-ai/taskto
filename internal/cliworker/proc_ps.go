//go:build unix && !linux

package cliworker

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// groupLive reports whether a process of group pgid can still run: one that exists and is not a
// zombie (killed, not yet reaped by its parent), which `ps` marks with a state starting with Z
// and which signal 0 can't tell from a live process. Where ps can't be read it says true: the
// caller keeps what it was to remove.
func groupLive(pgid int) bool {
	if pgid <= 1 || !groupExists(pgid) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pgid=,stat=").Output() //nolint:gosec // G204: fixed arguments
	if err != nil {
		return true
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if n, err := strconv.Atoi(f[0]); err == nil && n == pgid && !strings.HasPrefix(f[1], "Z") {
			return true
		}
	}
	return false
}
