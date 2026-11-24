package updatestatus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(Path(dir), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRead(t *testing.T) {
	if _, ok, err := Read(""); ok || err != nil {
		t.Fatalf("no directory: %v %v", ok, err)
	}
	dir := t.TempDir()
	if _, ok, err := Read(dir); ok || err != nil {
		t.Fatalf("no file: %v %v", ok, err)
	}

	write(t, dir, `{"version":1,"checked":"2026-11-20T03:10:00Z","channel":"release","hold":true,"recorded_version":"v0.3.0",
		"last_run":{"at":"2026-11-20T03:00:00Z","finished":"2026-11-20T03:10:00Z","result":"skipped","from":"v0.3.0","reason":"v0.4.0 needs manual steps"},
		"last_change":{"at":"2026-11-10T03:00:00Z","result":"updated","from":"v0.2.0","to":"v0.3.0","backup":"/var/backups/taskto/x.tar.gz"},
		"needs_person":{"kind":"manual-upgrade","reason":"v0.4.0 needs manual steps"}}`)
	s, ok, err := Read(dir)
	if err != nil || !ok {
		t.Fatalf("read: %v %v", ok, err)
	}
	if s.Channel != "release" || !s.Hold || s.RecordedVersion != "v0.3.0" || s.LastRun.Result != "skipped" ||
		s.LastChange.To != "v0.3.0" || s.LastChange.Backup == "" || s.NeedsPerson.Kind != KindManualUpgrade ||
		s.LastRun.Finished.Sub(s.LastRun.At) != 10*time.Minute {
		t.Fatalf("status = %+v", s)
	}

	// Null runs and no needs are a status too.
	write(t, dir, `{"version":1,"checked":"2026-11-20T03:10:00Z","channel":"off","last_run":null,"last_change":null,"needs_person":null}`)
	if s, ok, err := Read(dir); err != nil || !ok || s.LastRun != nil || s.NeedsPerson != nil {
		t.Fatalf("empty: %+v %v %v", s, ok, err)
	}
}

func TestReadRefusesWhatIsNotAStatus(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"not json":       "{",
		"another format": `{"version":2,"channel":"release"}`,
		"no format":      `{"channel":"release"}`,
	} {
		write(t, dir, content)
		if _, ok, err := Read(dir); ok || err == nil {
			t.Errorf("%s: ok=%v err=%v", name, ok, err)
		}
	}
	// A directory (or a link to anything else) in its place isn't read.
	if err := os.Remove(Path(dir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(Path(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Read(dir); ok || err == nil {
		t.Errorf("a directory: ok=%v err=%v", ok, err)
	}
}

// What people read is bounded, whatever the file says, and cut on a character.
func TestReadClipsTexts(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("é", maxText)
	write(t, dir, `{"version":1,"channel":"release","last_run":{"result":"failed","reason":"`+long+`"},"needs_person":{"kind":"blocked","reason":"`+long+`"}}`)
	s, _, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{s.LastRun.Reason, s.NeedsPerson.Reason} {
		if len(v) > maxText+len("…") || !strings.HasSuffix(v, "…") {
			t.Fatalf("not clipped: %d bytes", len(v))
		}
	}
}

func TestStale(t *testing.T) {
	now := time.Now()
	if (Status{Checked: now.Add(-time.Hour)}).Stale(now) || !(Status{Checked: now.Add(-StaleAfter - time.Minute)}).Stale(now) || !(Status{}).Stale(now) {
		t.Fatal("stale")
	}
}

// The reader never follows the status file out of the directory it was given.
func TestPath(t *testing.T) {
	if got := Path("/update-status"); got != filepath.Join("/update-status", "status.json") {
		t.Fatal(got)
	}
}
