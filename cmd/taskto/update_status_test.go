package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/updatestatus"
)

func (e *updEnv) public() updatestatus.Status {
	e.t.Helper()
	s, ok, err := updatestatus.Read(filepath.Join(e.state, "public"))
	if err != nil || !ok {
		e.t.Fatalf("the status for the server: %v, %v", ok, err)
	}
	return s
}

func TestUpdateWritesTheStatusForTheServer(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("etc/update-notify-url", "https://hooks.example.com/notify-path-token\n")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	s := e.public()
	if s.Version != 1 || s.Channel != "release" || s.Hold || s.RecordedVersion != updNew || s.NeedsPerson != nil {
		t.Fatalf("status = %+v", s)
	}
	if s.LastRun == nil || s.LastRun.Result != "updated" || s.LastRun.From != updOld || s.LastRun.To != updNew || !s.LastRun.BackedUp ||
		s.LastChange == nil || s.LastChange.Result != "updated" || s.LastRun.Finished.Before(s.LastRun.At) {
		t.Fatalf("runs = %+v, %+v", s.LastRun, s.LastChange)
	}
	if time.Since(s.Checked) > time.Minute {
		t.Errorf("checked %v", s.Checked)
	}

	dir := filepath.Join(e.state, "public")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "status.json" {
		t.Fatalf("%s holds %v (%v)", dir, entries, err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o755 {
		t.Errorf("directory mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(dir, "status.json")); fi.Mode().Perm() != 0o644 {
		t.Errorf("file mode %v", fi.Mode().Perm())
	}
	raw := e.read("state/public/status.json")
	for _, never := range []string{"digests", "sha256:", "notify-path-token", "hooks.example.com", `"sha"`} {
		if strings.Contains(raw, never) {
			t.Errorf("the server's status has %q:\n%s", never, raw)
		}
	}

	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	s = e.public()
	if s.LastRun.Result != "up-to-date" || s.LastChange == nil || s.LastChange.To != updNew {
		t.Fatalf("after a quiet run: %+v, %+v", s.LastRun, s.LastChange)
	}
}

func TestUpdateStatusForTheServerSaysHoldAndChannel(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("etc/update.hold", "")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := e.public(); !s.Hold || s.LastRun.Result != "skipped" || s.LastChange != nil || s.NeedsPerson != nil {
		t.Fatalf("on hold: %+v, %+v", s, s.LastRun)
	}
	if err := os.Remove(e.hold); err != nil {
		t.Fatal(err)
	}
	e.write("etc/install.env", e.read("etc/install.env")+"TASKTO_UPDATE_CHANNEL='off'\n")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := e.public(); s.Hold || s.Channel != "off" {
		t.Fatalf("off: %+v", s)
	}
}

func TestUpdateDryRunLeavesTheServersStatusAlone(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	if code, _ := e.run("--dry-run"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if e.exists("state/public") || e.exists("state/status.json") {
		t.Error("a dry run wrote a status")
	}
}

func TestUpdateStatusForTheServerSaysAManualUpgradeIsWaiting(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "Needs a new disk.\n\nmanual-upgrade: true\n", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	s := e.public()
	if s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindManualUpgrade || s.NeedsPerson.Code != "manual-upgrade" || s.LastRun.Code != "manual-upgrade" || s.LastRun.Result != "skipped" {
		t.Fatalf("status = %+v, %+v", s.NeedsPerson, s.LastRun)
	}
	if code, _ := e.run("--to", updNew); code != 0 {
		t.Fatal("--to didn't update")
	}
	if s := e.public(); s.NeedsPerson != nil || s.RecordedVersion != updNew || s.LastRun.Result != "updated" {
		t.Fatalf("after --to: %+v, %+v", s.NeedsPerson, s.LastRun)
	}
}

func TestUpdateStatusForTheServerSaysAFirewallChangeIsWaiting(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.commitFile(updNew, "deploy/worker-firewall.sh", firewallScript+"# changed\n")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := e.public(); s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindManualUpgrade || s.NeedsPerson.Code != "manual-upgrade" || s.LastRun.Code != "manual-upgrade" {
		t.Fatalf("status = %+v", s.NeedsPerson)
	}
}

func TestUpdateStatusForTheServerSaysAStopIsWaitingUntilItIsCleared(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/health-bad", updNew+"\n")
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	s := e.public()
	if s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindBlocked || s.NeedsPerson.Code != "stopped" || s.LastRun.Code != "stopped" ||
		s.LastRun.Result != "stopped" || s.LastChange == nil || s.LastChange.Result != "stopped" {
		t.Fatalf("after a stop: %+v, %+v", s.NeedsPerson, s.LastRun)
	}

	if err := os.Remove(filepath.Join(e.state, "public", "status.json")); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	s = e.public()
	if s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindBlocked || s.LastRun.Result != "stopped" {
		t.Fatalf("a blocked night: %+v, %+v", s.NeedsPerson, s.LastRun)
	}

	e.setVersion(updOld)
	e.setInstallVersion(updOld)
	e.setRunning(updOld)
	_ = os.Remove(filepath.Join(e.fake, "health-bad"))
	if code, out := e.run("--clear-block"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	s = e.public()
	if s.NeedsPerson != nil || s.LastRun.Result != "cleared" || s.RecordedVersion != updOld {
		t.Fatalf("after clearing: %+v, %+v", s.NeedsPerson, s.LastRun)
	}
}

func TestUpdateStatusForTheServerSaysAVersionMismatchIsWaiting(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.setRunning("v0.9.9")
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	s := e.public()
	if s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindBlocked || s.NeedsPerson.Code != "version-mismatch" || s.LastRun.Code != "version-mismatch" ||
		s.RecordedVersion != updOld {
		t.Fatalf("status = %+v, recorded %q", s.NeedsPerson, s.RecordedVersion)
	}
}

func TestUpdateWithoutAWritableServerStatusStillUpdates(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("state/public", "in the way")
	if code, out := e.run(); code != 0 || !strings.Contains(out, "can't write") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	e.wantStatus("updated", "")
}

func TestServerReadsTheUpdaterStatusOnly(t *testing.T) {
	t.Parallel()
	compose := readRepoFile(t, "compose.yaml")
	for _, want := range []string{
		"TASKTO_UPDATE_STATUS_DIR: /update-status",
		"${TASKTO_UPDATE_STATUS_SOURCE:-/var/lib/taskto-update/public}",
		"target: /update-status\n        read_only: true",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose.yaml doesn't have %q", want)
		}
	}
	if strings.Contains(readRepoFile(t, "compose.yaml"), "/var/lib/taskto-update:") {
		t.Error("compose.yaml mounts the updater's whole state")
	}
	if !strings.Contains(readRepoFile(t, "deploy/safety-check.sh"), "the updater's state") {
		t.Error("deploy/safety-check.sh doesn't check what the server sees of the updater")
	}
	docs := readRepoFile(t, "docs/install.md")
	for _, want := range []string{"/var/lib/taskto-update/public", "Settings → About", "needs_person"} {
		if !strings.Contains(docs, want) {
			t.Errorf("docs/install.md doesn't have %q", want)
		}
	}
}

func TestUpdateStatusForTheServerKeepsAPendingManualUpgrade(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "Needs a new disk.\n\nmanual-upgrade: true\n", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := e.public(); s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindManualUpgrade {
		t.Fatalf("pending: %+v", s.NeedsPerson)
	}
	e.write("etc/update.hold", "")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := e.public(); s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindManualUpgrade ||
		s.NeedsPerson.Code != "manual-upgrade" || !s.Hold || s.LastRun.Result != "skipped" || s.LastRun.Code != "hold" {
		t.Fatalf("on hold: %+v, %+v", s.NeedsPerson, s.LastRun)
	}
	if err := os.Remove(e.hold); err != nil {
		t.Fatal(err)
	}
	e.write("etc/install.env", e.read("etc/install.env")+"TASKTO_UPDATE_CHANNEL='off'\n")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := e.public(); s.NeedsPerson == nil || s.Channel != "off" {
		t.Fatalf("off: %+v", s.NeedsPerson)
	}
	e.write("etc/install.env", strings.Replace(e.read("etc/install.env"), "TASKTO_UPDATE_CHANNEL='off'\n", "", 1))
	e.gh.locked(func() { e.gh.releases = []map[string]any{} })
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := e.public(); s.NeedsPerson != nil || s.LastRun.Result != "up-to-date" {
		t.Fatalf("after looking: %+v, %+v", s.NeedsPerson, s.LastRun)
	}
}

func TestUpdateStatusForTheServerKeepsAManualUpgradeWhenTheReleasesCantBeRead(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "manual-upgrade: true\n", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.gh.locked(func() { e.gh.releases = nil })
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := e.public(); s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindManualUpgrade || s.LastRun.Result != "skipped" {
		t.Fatalf("status = %+v, %+v", s.NeedsPerson, s.LastRun)
	}
}

func TestUpdateStatusForTheServerGivesTheCurrentReasonForAStop(t *testing.T) {
	t.Parallel()
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/health-bad", updNew+"\n")
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if err := os.Remove(filepath.Join(e.state, "blocked")); err != nil {
		t.Fatal(err)
	}
	e.write("state/status.json", `{"at":"2020-01-01T00:00:00Z","result":"up-to-date","reason":"an older run"}`)
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	s := e.public()
	if s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindBlocked || s.NeedsPerson.Code != "stopped" {
		t.Fatalf("status = %+v", s.NeedsPerson)
	}
}
