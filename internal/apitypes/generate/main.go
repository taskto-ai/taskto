package main

import (
	"fmt"
	"os"

	"taskto.ai/taskto/internal/apitypes"
)

func main() {
	out, err := apitypes.Generate(apitypes.Roots())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(apitypes.Output, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
