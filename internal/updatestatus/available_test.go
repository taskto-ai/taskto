package updatestatus

import (
	"strings"
	"testing"
	"time"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// What the updater found to install next is read like the rest: a version, a commit, a time, a
// yes or no and a repository, each checked again.
func TestReadAvailable(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"version":1,"channel":"release","recorded_version":"v0.3.0","looked":"2026-11-26T06:15:00Z",
		"available":{"version":"v0.4.0","sha":"`+shaA+`","published":"2026-11-25T10:00:00Z","manual_upgrade":true,"repo":"taskto-ai/taskto"}}`)
	s, ok, err := Read(dir)
	if err != nil || !ok {
		t.Fatalf("read: %v %v", ok, err)
	}
	a := s.Available
	if a == nil || a.Version != "v0.4.0" || a.SHA != shaA || !a.ManualUpgrade || a.Repo != "taskto-ai/taskto" ||
		!a.Published.Equal(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)) || s.Looked.IsZero() {
		t.Fatalf("available = %+v, looked %v", a, s.Looked)
	}

	for name, tc := range map[string]struct {
		available string
		want      *Available
	}{
		"not a version":         {`{"version":"latest","repo":"taskto-ai/taskto"}`, nil},
		"the version on record": {`{"version":"v0.3.0"}`, nil},
		"prose for a version":   {`{"version":"v0.4.0 see https://example.com"}`, nil},
		"a bad sha and repo": {`{"version":"v0.4.0","sha":"main","repo":"https://example.com/x"}`,
			&Available{Version: "v0.4.0"}},
		"a repo with a path": {`{"version":"v0.4.0","repo":"a/b/../c"}`, &Available{Version: "v0.4.0"}},
		"a commit is its own sha": {`{"version":"` + shaB + `","sha":"` + shaA + `"}`,
			&Available{Version: shaB, SHA: shaB}},
	} {
		write(t, dir, `{"version":1,"channel":"release","recorded_version":"v0.3.0","available":`+tc.available+`}`)
		s, ok, err := Read(dir)
		if err != nil || !ok {
			t.Fatalf("%s: %v %v", name, ok, err)
		}
		switch {
		case tc.want == nil && s.Available != nil:
			t.Errorf("%s: available = %+v, want none", name, s.Available)
		case tc.want != nil && (s.Available == nil || *s.Available != *tc.want):
			t.Errorf("%s: available = %+v, want %+v", name, s.Available, tc.want)
		}
	}
}

func TestNotesURL(t *testing.T) {
	for name, tc := range map[string]struct {
		a       *Available
		current string
		want    string
	}{
		"nothing":       {nil, "v0.3.0", ""},
		"no repository": {&Available{Version: "v0.4.0"}, "v0.3.0", ""},
		"a release":     {&Available{Version: "v0.4.0", Repo: "taskto-ai/taskto"}, shaA, "https://github.com/taskto-ai/taskto/releases/tag/v0.4.0"},
		"a commit":      {&Available{Version: shaB, Repo: "taskto-ai/taskto"}, shaA, "https://github.com/taskto-ai/taskto/compare/" + shaA + "..." + shaB},
		"a commit from a dev build": {&Available{Version: shaB, Repo: "taskto-ai/taskto"}, "dev",
			"https://github.com/taskto-ai/taskto/commit/" + shaB},
		"a fork":                      {&Available{Version: "v1.0.0", Repo: "someone/taskto-fork"}, "v0.9.0", "https://github.com/someone/taskto-fork/releases/tag/v1.0.0"},
		"a repository that isn't one": {&Available{Version: "v1.0.0", Repo: "evil.example/x?y"}, "v0.9.0", ""},
	} {
		if got := NotesURL(tc.a, tc.current); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestBuildReportLinksTheNotes(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"version":1,"checked":"2026-11-26T06:15:00Z","channel":"main","recorded_version":"`+shaA+`",
		"available":{"version":"`+shaB+`","manual_upgrade":false,"repo":"taskto-ai/taskto"}}`)
	rep, err := BuildReport(shaA, dir, "", time.Date(2026, 11, 26, 7, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(rep.NotesURL, "/compare/"+shaA+"..."+shaB) || rep.Status.Available == nil {
		t.Fatalf("report = %+v", rep)
	}
	// A notes_url in the file is not the server's: only the server builds the link.
	write(t, dir, `{"version":1,"checked":"2026-11-26T06:15:00Z","channel":"release","recorded_version":"v0.3.0","notes_url":"https://evil.example/"}`)
	if rep, err := BuildReport("v0.3.0", dir, "", time.Now()); err != nil || rep.NotesURL != "" {
		t.Fatalf("report = %+v, %v", rep, err)
	}
}
