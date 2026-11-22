//go:build !unix

package main

import (
	"errors"
	"io"
)

// runCLIWorker fails: the CLI worker needs Unix users and process groups.
func runCLIWorker([]string, io.Writer) error {
	return errors.New("the CLI worker needs Linux")
}
