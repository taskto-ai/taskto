//go:build linux

package cliworker_test

import (
	"testing"

	"taskto.ai/taskto/internal/cliworker"
)

// The state and process group are read from /proc/<pid>/stat past a command name that holds
// spaces and parentheses, so a zombie isn't taken for a live process.
func TestParseStat(t *testing.T) {
	for _, tc := range []struct {
		stat  string
		state byte
		pgrp  int
		ok    bool
	}{
		{"123 (sleep) S 1 456 456 0 -1 4194304", 'S', 456, true},
		{"123 (a b) c) Z 1 789 789 0 -1 4", 'Z', 789, true},
		{"123 (x)", 0, 0, false},
		{"no parentheses", 0, 0, false},
	} {
		state, pgrp, ok := cliworker.ParseStatForTest(tc.stat)
		if state != tc.state || pgrp != tc.pgrp || ok != tc.ok {
			t.Errorf("%q = %c %d %v, want %c %d %v", tc.stat, state, pgrp, ok, tc.state, tc.pgrp, tc.ok)
		}
	}
}
