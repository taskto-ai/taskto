//go:build !linux

package cliworker

func noNewPrivs() error { return nil }
