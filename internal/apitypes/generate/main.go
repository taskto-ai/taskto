// Command generate writes web/src/api.gen.ts: run it with go generate ./internal/apitypes.
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
	// go generate runs in the package's directory.
	if err := os.WriteFile(apitypes.Output, out, 0o644); err != nil { //nolint:gosec // a source file
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
