package main

import (
	"bytes"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

const limit = 800

const exceptionLabel = "size-exception"

var notCountedFiles = map[string]bool{
	"pnpm-lock.yaml":                true,
	"go.sum":                        true,
	"tools/go.sum":                  true,
	"web/src/api.gen.ts":            true,
	"agent/src/taskto-tools.json":   true,
	"agent/src/test-entry.ts":       true,
	"deploy/worker-network-test.sh": true,
}

var notCountedDirs = map[string]bool{
	"testdata":     true,
	"e2e":          true,
	"clitest":      true,
	"audittest":    true,
	"testprovider": true,
	"locales":      true,
}

var notCountedDirPrefixes = []string{"web/src/test/"}

var notCountedSuffixes = []string{"_test.go", ".test.ts", ".test.tsx", ".md"}

type change struct {
	oldName string
	newName string
	added   int
	deleted int
}

type fileCount struct {
	name  string
	lines int
}

type tally struct {
	production int
	other      int
	files      []fileCount
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
	for _, prefix := range notCountedDirPrefixes {
		if strings.HasPrefix(name, prefix) {
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
	trimmed := bytes.TrimRight(out, "\x00")
	if len(trimmed) == 0 {
		return nil, nil
	}
	fields := bytes.Split(trimmed, []byte{0})
	var changes []change
	for i := 0; i < len(fields); i++ {
		parts := strings.SplitN(string(fields[i]), "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("unexpected numstat entry %q", fields[i])
		}
		oldName, newName := parts[2], parts[2]
		if parts[2] == "" {
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("rename entry without its paths")
			}
			oldName, newName = string(fields[i+1]), string(fields[i+2])
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
		changes = append(changes, change{oldName: oldName, newName: newName, added: added, deleted: deleted})
	}
	return changes, nil
}

func count(changes []change) tally {
	var t tally
	perFile := map[string]int{}
	add := func(name string, lines int) {
		if lines == 0 {
			return
		}
		if counted(name) {
			t.production += lines
			perFile[name] += lines
		} else {
			t.other += lines
		}
	}
	for _, c := range changes {
		add(c.newName, c.added)
		add(c.oldName, c.deleted)
	}
	for name, lines := range perFile {
		t.files = append(t.files, fileCount{name: name, lines: lines})
	}
	sort.Slice(t.files, func(i, j int) bool {
		if t.files[i].lines != t.files[j].lines {
			return t.files[i].lines > t.files[j].lines
		}
		return t.files[i].name < t.files[j].name
	})
	return t
}

type verdict struct {
	pass    bool
	message string
}

func judge(t tally, exception bool) verdict {
	if t.production <= limit {
		return verdict{pass: true}
	}
	if exception {
		return verdict{pass: true, message: fmt.Sprintf("::warning title=Pull request size::%d lines of production code, over the limit of %d; allowed by the %s label.", t.production, limit, exceptionLabel)}
	}
	return verdict{message: fmt.Sprintf("::error title=Pull request size::%d lines of production code, over the limit of %d. Split it into pull requests that each stand on main (CONTRIBUTING.md, \"Sending a change\"); a maintainer can allow it with the %s label.", t.production, limit, exceptionLabel)}
}
