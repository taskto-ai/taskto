//go:build !unix

package main

import (
	"errors"
	"io"
)

func runCLIWorker([]string, io.Writer) error {
	return errors.New("the CLI worker needs Linux")
}
