//go:build unix && !linux

package cliworker

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func groupLive(pgid int) bool {
	if pgid <= 1 || !groupExists(pgid) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pgid=,stat=").Output()
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
