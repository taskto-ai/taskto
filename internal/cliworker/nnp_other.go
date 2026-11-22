//go:build !linux

package cliworker

// noNewPrivs does nothing here: no_new_privs is Linux's. On macOS (local mode only) the
// server's start-up check is what keeps sudo from a CLI.
func noNewPrivs() error { return nil }
