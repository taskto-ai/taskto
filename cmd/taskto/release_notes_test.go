package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// .github/scripts/release-notes.sh groups GitHub's generated list by the pull requests' title
// prefixes, as the Release workflow uses it.
func TestReleaseNotesAreGroupedByTitle(t *testing.T) {
	needTools(t, "bash", "awk")
	script, _ := filepath.Abs(filepath.Join("..", "..", ".github", "scripts", "release-notes.sh"))
	run := func(in string) string {
		t.Helper()
		cmd := exec.Command("bash", script)
		cmd.Stdin = strings.NewReader(in)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return string(out)
	}
	got := run("## What's Changed\n" +
		"* feat: icons by @a in https://github.com/o/r/pull/4\n" +
		"* fix: totals by @b in https://github.com/o/r/pull/3\n" +
		"* chore: names by @a in https://github.com/o/r/pull/2\n" +
		"* Fix(web)!: a fix with a scope by @a in https://github.com/o/r/pull/5\n" +
		"* feature flags, with no prefix by @a in https://github.com/o/r/pull/6\n" +
		"* feat: second feature by @a in https://github.com/o/r/pull/1\n" +
		"\n## New Contributors\n* @a made their first contribution in https://github.com/o/r/pull/1\n" +
		"\n**Full Changelog**: https://github.com/o/r/commits/v0.1.0\n")
	want := "## Features\n\n" +
		"* feat: icons by @a in https://github.com/o/r/pull/4\n" +
		"* feat: second feature by @a in https://github.com/o/r/pull/1\n\n" +
		"## Fixes\n\n" +
		"* fix: totals by @b in https://github.com/o/r/pull/3\n" +
		"* Fix(web)!: a fix with a scope by @a in https://github.com/o/r/pull/5\n\n" +
		"## Other\n\n" +
		"* chore: names by @a in https://github.com/o/r/pull/2\n" +
		"* feature flags, with no prefix by @a in https://github.com/o/r/pull/6\n\n" +
		"## New Contributors\n* @a made their first contribution in https://github.com/o/r/pull/1\n" +
		"\n**Full Changelog**: https://github.com/o/r/commits/v0.1.0\n"
	if got != want {
		t.Errorf("grouped notes:\n%s\nwant:\n%s", got, want)
	}
	// No pull request: only the link; no empty group.
	if got := run("**Full Changelog**: https://github.com/o/r/compare/v0.1.0...v0.1.1\n"); got != "**Full Changelog**: https://github.com/o/r/compare/v0.1.0...v0.1.1\n" {
		t.Errorf("no pull requests: %q", got)
	}
}

// The Release workflow writes the grouped list itself, after the operator's notes, and never
// GitHub's ungrouped one.
func TestReleaseWorkflowGroupsTheNotes(t *testing.T) {
	step := workflowStep(t, ".github/workflows/release.yml", "Create the release, and publish its images")
	for _, want := range []string{`releases/generate-notes`, `-f "tag_name=$VERSION"`, `bash .github/scripts/release-notes.sh`} {
		if !strings.Contains(step, want) {
			t.Errorf("the release step doesn't have %q", want)
		}
	}
	if strings.Contains(step, "--generate-notes") {
		t.Error("the release step still asks GitHub for its ungrouped notes")
	}
	if i, j := strings.Index(step, "$NOTES"), strings.Index(step, "release-notes.sh"); i < 0 || j < i {
		t.Error("the operator's notes don't come before the list")
	}
}
