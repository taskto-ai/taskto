package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
)

const limit = 800

const exceptionLabel = "size-exception"

var notCountedFiles = map[string]bool{
	"pnpm-lock.yaml":              true,
	"go.sum":                      true,
	"tools/go.sum":                true,
	"web/src/api.gen.ts":          true,
	"agent/src/taskto-tools.json": true,
}

var notCountedDirs = map[string]bool{
	"testdata":     true,
	"e2e":          true,
	"clitest":      true,
	"audittest":    true,
	"testprovider": true,
	"locales":      true,
}

var notCountedSuffixes = []string{"_test.go", ".test.ts", ".test.tsx", ".md"}

type change struct {
	name  string
	lines int
}

func counted(name string) bool {
	if notCountedFiles[name] {
		return false
	}
	for _, suffix := range notCountedSuffixes {
		if strings.HasSuffix(name, suffix) {
			return false
		}
	}
	for _, dir := range strings.Split(path.Dir(name), "/") {
		if notCountedDirs[dir] {
			return false
		}
	}
	return true
}

func parseNumstat(out []byte) ([]change, error) {
	fields := bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0})
	var changes []change
	for i := 0; i < len(fields); i++ {
		parts := strings.SplitN(string(fields[i]), "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("unexpected numstat entry %q", fields[i])
		}
		name := parts[2]
		if name == "" {
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("rename entry without its paths")
			}
			name = string(fields[i+2])
			i += 2
		}
		if parts[0] == "-" {
			continue
		}
		added, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, err
		}
		deleted, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, err
		}
		changes = append(changes, change{name: name, lines: added + deleted})
	}
	return changes, nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run .github/scripts/prsize.go <base revision>")
		os.Exit(2)
	}
	out, err := exec.Command("git", "diff", "--numstat", "-z", "-M", os.Args[1], "HEAD").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "prsize: git diff:", err)
		os.Exit(2)
	}
	changes, err := parseNumstat(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "prsize:", err)
		os.Exit(2)
	}
	total, other := 0, 0
	var production []change
	for _, c := range changes {
		if counted(c.name) {
			total += c.lines
			production = append(production, c)
		} else {
			other += c.lines
		}
	}
	sort.Slice(production, func(i, j int) bool { return production[i].lines > production[j].lines })
	fmt.Printf("Production code changed: %d lines (limit %d). Tests, generated files, translations and docs: %d lines, not counted.\n", total, limit, other)
	for i, c := range production {
		if i == 15 {
			fmt.Printf("  … and %d more files\n", len(production)-i)
			break
		}
		fmt.Printf("  %6d  %s\n", c.lines, c.name)
	}
	if total <= limit {
		return
	}
	if os.Getenv("PR_SIZE_EXCEPTION") == "true" {
		fmt.Printf("::warning title=Pull request size::%d lines of production code, over the limit of %d; allowed by the %s label.\n", total, limit, exceptionLabel)
		return
	}
	fmt.Printf("::error title=Pull request size::%d lines of production code, over the limit of %d. Split it into pull requests that each stand on main (CONTRIBUTING.md, \"Sending a change\"); a maintainer can allow it with the %s label.\n", total, limit, exceptionLabel)
	os.Exit(1)
}
