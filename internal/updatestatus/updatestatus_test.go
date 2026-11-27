package updatestatus

import (
	"encoding/json"
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
		"last_run":{"at":"2026-11-20T03:00:00Z","finished":"2026-11-20T03:10:00Z","result":"skipped","from":"v0.3.0","code":"manual-upgrade"},
		"last_change":{"at":"2026-11-10T03:00:00Z","result":"updated","from":"v0.2.0","to":"v0.3.0","backed_up":true},
		"needs_person":{"kind":"manual-upgrade","code":"manual-upgrade"}}`)
	s, ok, err := Read(dir)
	if err != nil || !ok {
		t.Fatalf("read: %v %v", ok, err)
	}
	if s.Channel != "release" || !s.Hold || s.RecordedVersion != "v0.3.0" || s.LastRun.Result != "skipped" ||
		s.LastChange.To != "v0.3.0" || !s.LastChange.BackedUp || s.LastRun.Code != "manual-upgrade" || s.NeedsPerson.Kind != KindManualUpgrade ||
		s.NeedsPerson.Code != "manual-upgrade" ||
		s.LastRun.Finished.Sub(s.LastRun.At) != 10*time.Minute {
		t.Fatalf("status = %+v", s)
	}

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

func TestReadKeepsOnlyStructuredStatus(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"version":1,"checked":"2026-11-20T03:10:00Z","channel":"registry.internal.example/x","recorded_version":"/var/lib/taskto-update",
		"last_run":{"result":"cleaning /var/backups/taskto","from":"v0.3.0 and more","to":"v1.2.3","code":"can't pull registry.internal.example/taskto:v1","reason":"a path /var/backups/b.tar.gz","backup":"/var/backups/b.tar.gz"},
		"last_change":{"result":"stopped","from":"0123456789abcdef0123456789abcdef01234567","to":"v01.2.3","code":"stopped"},
		"needs_person":{"kind":"blocked","code":"volume taskto_data"}}`)
	s, ok, err := Read(dir)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if s.Channel != "" || s.RecordedVersion != "" {
		t.Errorf("channel %q, recorded %q", s.Channel, s.RecordedVersion)
	}
	r := s.LastRun
	if r.Result != Unknown || r.From != "" || r.To != "v1.2.3" || r.Code != CodeOther {
		t.Errorf("last run = %+v", r)
	}
	c := s.LastChange
	if c.Result != "stopped" || c.From != "0123456789abcdef0123456789abcdef01234567" || c.To != "" || c.Code != "stopped" {
		t.Errorf("last change = %+v", c)
	}
	if s.NeedsPerson.Kind != KindBlocked || s.NeedsPerson.Code != CodeOther {
		t.Errorf("needs = %+v", s.NeedsPerson)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, never := range []string{"/var/", "registry.internal", "taskto_data", "reason", "backup\""} {
		if strings.Contains(string(out), never) {
			t.Errorf("the status has %q: %s", never, out)
		}
	}
	write(t, dir, `{"version":1,"needs_person":{"kind":"sudo rm -rf","code":"other"}}`)
	if s, _, err := Read(dir); err != nil || s.NeedsPerson != nil {
		t.Errorf("unknown kind: %+v %v", s.NeedsPerson, err)
	}
}

func TestIsVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.3.0": true, "v10.20.30": true, "0123456789abcdef0123456789abcdef01234567": true,
		"dev": false, "": false, "v01.2.3": false, "v1.2": false, "v1.2.3-rc1": false, "0123456789ABCDEF0123456789abcdef01234567": false,
		"v1.2.3\n": false, strings.Repeat("a", 41): false,
	} {
		if IsVersion(v) != want {
			t.Errorf("IsVersion(%q) = %v", v, !want)
		}
	}
}

func TestStale(t *testing.T) {
	now := time.Now()
	if (Status{Checked: now.Add(-time.Hour)}).Stale(now) || !(Status{Checked: now.Add(-StaleAfter - time.Minute)}).Stale(now) || !(Status{}).Stale(now) {
		t.Fatal("stale")
	}
}

func TestPath(t *testing.T) {
	if got := Path("/update-status"); got != filepath.Join("/update-status", "status.json") {
		t.Fatal(got)
	}
}
