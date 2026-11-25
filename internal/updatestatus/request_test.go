package updatestatus

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

var requestPattern = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ [0-9a-f]{32}\n$`)

func TestRequestWritesOneSmallFileOfAFixedShape(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	if err := Request(dir, now); err != nil {
		t.Fatal(err)
	}
	// The request and nothing else: no temporary file is left.
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != RequestFile {
		t.Fatalf("the directory has %v (%v)", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, RequestFile))
	if err != nil || !requestPattern.Match(b) || string(b[:20]) != "2026-11-24T12:00:00Z" {
		t.Fatalf("the request is %q (%v)", b, err)
	}
	if len(b) > 64 {
		t.Errorf("the request is %d bytes", len(b))
	}
	// No second one while it waits; the first stays as it was.
	if err := Request(dir, now.Add(time.Second)); !errors.Is(err, ErrPending) {
		t.Fatalf("a second request: %v", err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, RequestFile)); string(again) != string(b) {
		t.Errorf("the pending request changed: %q", again)
	}
	// The host takes it: another may be made, with another nonce.
	if err := os.Remove(filepath.Join(dir, RequestFile)); err != nil {
		t.Fatal(err)
	}
	if err := Request(dir, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if next, _ := os.ReadFile(filepath.Join(dir, RequestFile)); string(next[21:]) == string(b[21:]) {
		t.Error("two requests have the same nonce")
	}
}

func TestRequestsAtTheSameTimeMakeOne(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- Request(dir, time.Now())
		}()
	}
	wg.Wait()
	close(results)
	made := 0
	for err := range results {
		switch {
		case err == nil:
			made++
		case !errors.Is(err, ErrPending):
			t.Errorf("error: %v", err)
		}
	}
	if made != 1 {
		t.Errorf("%d requests made, want 1", made)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the directory has %v", entries)
	}
}

func TestRequestNeedsADirectoryTheServerCanWrite(t *testing.T) {
	if err := Request("", time.Now()); !errors.Is(err, ErrNoDir) {
		t.Errorf("no directory: %v", err)
	}
	if err := Request(filepath.Join(t.TempDir(), "gone"), time.Now()); !errors.Is(err, ErrNoDir) {
		t.Errorf("a directory that isn't there: %v", err)
	}
	if os.Getuid() != 0 {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if err := Request(dir, time.Now()); !errors.Is(err, ErrNoDir) {
			t.Errorf("a directory it can't write (root's, made by Docker): %v", err)
		}
	}
}

// Whatever sits at the request's name counts as one waiting, and is never replaced or followed:
// the host removes it, and ignores it if it isn't a small file.
func TestRequestPendingCountsAnythingAtItsName(t *testing.T) {
	now := time.Now()
	for name, setup := range map[string]func(path string) error{
		"a file":      func(p string) error { return os.WriteFile(p, []byte("x"), 0o644) },
		"a link":      func(p string) error { return os.Symlink("/nonexistent/target", p) },
		"a directory": func(p string) error { return os.Mkdir(p, 0o755) },
	} {
		dir := t.TempDir()
		if err := setup(filepath.Join(dir, RequestFile)); err != nil {
			t.Fatal(err)
		}
		if _, pending, err := Pending(dir, now); err != nil || !pending {
			t.Errorf("%s: pending = %v, %v", name, pending, err)
		}
		if err := Request(dir, now); !errors.Is(err, ErrPending) {
			t.Errorf("%s: a request over it: %v", name, err)
		}
	}
	if _, pending, err := Pending("", now); pending || err != nil {
		t.Errorf("no directory: %v, %v", pending, err)
	}
}

func TestRequestExpires(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := Request(dir, now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(ExpireAfter + time.Minute)
	if _, pending, err := Pending(dir, later); err != nil || pending {
		t.Fatalf("an old request is pending: %v, %v", pending, err)
	}
	if err := Request(dir, later); err != nil {
		t.Fatalf("a request over an old one: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, RequestFile))
	if !requestPattern.Match(b) {
		t.Errorf("the request is %q", b)
	}
}

func TestReportSaysWhetherTheButtonCanBePressed(t *testing.T) {
	status, req := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339)
	write(t, status, `{"version":1,"checked":"`+stamp+`","channel":"release","requests":{"supported":true,"last":null}}`)
	rep, err := BuildReport("v0.3.0", status, req, now)
	if err != nil || !rep.Request.Available || rep.Request.Reason != "" {
		t.Fatalf("report = %+v, %v", rep.Request, err)
	}
	cases := map[string]struct {
		status, reason string
	}{
		"off":         {`{"version":1,"checked":"` + stamp + `","channel":"off","requests":{"supported":true,"last":null}}`, ReasonOff},
		"unsupported": {`{"version":1,"checked":"` + stamp + `","channel":"release","requests":{"supported":false,"last":null}}`, ReasonUnsupported},
		"old updater": {`{"version":1,"checked":"` + stamp + `","channel":"release"}`, ReasonUnsupported},
		"running":     {`{"version":1,"checked":"` + stamp + `","channel":"release","requests":{"supported":true,"last":null},"running":{"since":"` + stamp + `"}}`, ReasonRunning},
		"stale":       {`{"version":1,"checked":"2020-01-01T00:00:00Z","channel":"release","requests":{"supported":true,"last":null}}`, ReasonUnsupported},
		// A run that was killed leaves its note; it is believed for a few hours only.
		"an old running note": {`{"version":1,"checked":"` + stamp + `","channel":"release","requests":{"supported":true,"last":null},"running":{"since":"` +
			now.Add(-RunningMaxAge-time.Minute).Format(time.RFC3339) + `"}}`, ""},
	}
	for name, tc := range cases {
		write(t, status, tc.status)
		rep, err := BuildReport("v0.3.0", status, req, now)
		if err != nil || rep.Request.Reason != tc.reason || rep.Request.Available != (tc.reason == "") {
			t.Errorf("%s: %+v, %v", name, rep.Request, err)
		}
	}
	write(t, status, cases["old updater"].status)
	if rep, _ := BuildReport("v0.3.0", status, "", now); rep.Request.Reason != ReasonNotConfigured {
		t.Errorf("no request directory: %+v", rep.Request)
	}
	write(t, status, `{"version":1,"checked":"`+stamp+`","channel":"release","requests":{"supported":true,"last":null}}`)
	if err := Request(req, now); err != nil {
		t.Fatal(err)
	}
	rep, _ = BuildReport("v0.3.0", status, req, now)
	if rep.Request.Available || rep.Request.Reason != ReasonPending || rep.Request.PendingSince.IsZero() {
		t.Errorf("a request waits: %+v", rep.Request)
	}
}

// What the host writes about a request is words of a closed list, whatever the file holds.
func TestReadChecksWhatTheHostSaysOfRequests(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"version":1,"checked":"2026-11-20T03:10:00Z","channel":"release",
		"running":{"since":"2026-11-20T03:10:00Z","to":"/etc/passwd","requested":true},
		"requests":{"supported":true,"last":{"at":"2026-11-20T03:00:00Z","result":"/var/backups/x","code":"restore /var/backups/x"}},
		"last_run":{"result":"skipped","code":"rate-limited","requested":true}}`)
	s, ok, err := Read(dir)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if s.Running == nil || s.Running.To != "" || !s.Running.Requested {
		t.Errorf("running = %+v", s.Running)
	}
	if l := s.Requests.Last; l.Result != Unknown || l.Code != CodeOther {
		t.Errorf("last = %+v", l)
	}
	if !s.LastRun.Requested || s.LastRun.Code != "rate-limited" {
		t.Errorf("last_run = %+v", s.LastRun)
	}
	write(t, dir, `{"version":1,"channel":"release","running":{"to":"v0.1.0"},"requests":{"supported":true,"last":{"result":"ignored","code":"bad-request"}}}`)
	if s, _, _ := Read(dir); s.Running != nil || s.Requests.Last.Result != "ignored" || s.Requests.Last.Code != "bad-request" {
		t.Errorf("a running note without a time is kept: %+v / %+v", s.Running, s.Requests.Last)
	}
}

// A prepared request is not there for the host until it is published, so the caller can record it
// first; a discarded one leaves nothing.
func TestPreparedRequestIsHiddenUntilPublished(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	p, err := Prepare(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, pending, _ := Pending(dir, now); pending {
		t.Fatal("a prepared request is visible to the host")
	}
	if _, err := os.Lstat(filepath.Join(dir, RequestFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the request's name exists: %v", err)
	}
	if err := p.Publish(); err != nil {
		t.Fatal(err)
	}
	if _, pending, _ := Pending(dir, now); !pending {
		t.Error("a published request isn't pending")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the directory has %v", entries)
	}

	dir = t.TempDir()
	p, err = Prepare(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	p.Discard()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a discarded request left %v", entries)
	}

	// Another request published in between: the second to publish is refused, the first stands.
	dir = t.TempDir()
	a, _ := Prepare(dir, now)
	b, _ := Prepare(dir, now)
	if err := a.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(); !errors.Is(err, ErrPending) {
		t.Errorf("the second: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the directory has %v", entries)
	}
}
