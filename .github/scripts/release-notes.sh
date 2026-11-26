#!/usr/bin/env bash
# release-notes.sh: GitHub's generated release notes (POST /repos/{owner}/{repo}/releases/generate-notes,
# its `body`), on standard input, grouped by the pull requests' conventional title prefix, on
# standard output: `feat` under Features, `fix` under Fixes, everything else (docs, chore,
# refactor, ci, test, a title with no prefix) under Other, each in the order GitHub gave. GitHub's
# own release.yml can group only by label, and this repository's pull requests are told apart by
# their titles (AGENTS.md, Workflow), so the Release workflow groups them here. What follows the
# list (New contributors, the Full Changelog link) is kept as it is. Run by
# .github/workflows/release.yml; cmd/taskto/release_notes_test.go runs it on samples.
set -euo pipefail
export LC_ALL=C

awk '
	# A pull request line of the list: "* <title> by @<author> in <url>".
	function kind(line,   t) {
		t = tolower(substr(line, 3))
		if (t ~ /^feat(\([^)]*\))?!?:/) return "feat"
		if (t ~ /^fix(\([^)]*\))?!?:/) return "fix"
		return "other"
	}
	function group(title, n, lines,   i) {
		if (n == 0) return
		printf "## %s\n\n", title
		for (i = 1; i <= n; i++) print lines[i]
		print ""
	}
	BEGIN { inlist = 0; nf = nx = no = nr = 0 }
	/^## What.s Changed[[:space:]]*$/ { inlist = 1; next }
	/^## / || /^\*\*Full Changelog\*\*/ { inlist = 0 }
	inlist && /^\* / {
		k = kind($0)
		if (k == "feat") feat[++nf] = $0
		else if (k == "fix") fix[++nx] = $0
		else other[++no] = $0
		next
	}
	inlist && /^[[:space:]]*$/ { next }
	{ rest[++nr] = $0 }
	END {
		group("Features", nf, feat)
		group("Fixes", nx, fix)
		group("Other", no, other)
		# What came after the list, without the blank lines it began with.
		for (i = 1; i <= nr && rest[i] ~ /^[[:space:]]*$/; i++) {}
		for (; i <= nr; i++) print rest[i]
	}
'
