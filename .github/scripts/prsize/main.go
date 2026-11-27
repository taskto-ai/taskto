package main

import (
	"fmt"
	"os"
	"os/exec"
)

func diff(base, head string) ([]change, error) {
	out, err := exec.Command("git", "diff", "--numstat", "-z", "-M", base+"..."+head).Output()
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	return parseNumstat(out)
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./.github/scripts/prsize <base revision> <head revision>")
		os.Exit(2)
	}
	changes, err := diff(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "prsize:", err)
		os.Exit(2)
	}
	t := count(changes)
	fmt.Printf("Production code changed: %d lines (limit %d). Tests, generated files, translations and docs: %d lines, not counted.\n", t.production, limit, t.other)
	for i, f := range t.files {
		if i == 15 {
			fmt.Printf("  … and %d more files\n", len(t.files)-i)
			break
		}
		fmt.Printf("  %6d  %q\n", f.lines, f.name)
	}
	v := judge(t, os.Getenv("PR_SIZE_EXCEPTION") == "true")
	if v.message != "" {
		fmt.Println(v.message)
	}
	if !v.pass {
		os.Exit(1)
	}
}
