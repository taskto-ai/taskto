package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/updatestatus"
)

// public is the status the updater leaves for the server, as the server reads it.
func (e *updEnv) public() updatestatus.Status {
	e.t.Helper()
	s, ok, err := updatestatus.Read(filepath.Join(e.state, "public"))
	if err != nil || !ok {
		e.t.Fatalf("the status for the server: %v, %v", ok, err)
	}
	return s
}

// An update leaves the server what it needs to show admins: the last run and change, the
// version on record, and nothing of the updater's own state.
func TestUpdateWritesTheStatusForTheServer(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	// Where the notification goes may hold a secret.
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

	// A copy made for the server: one file, readable by it, and only what it shows.
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

	// A quiet night changes the last run, and keeps the last change.
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	s = e.public()
	if s.LastRun.Result != "up-to-date" || s.LastChange == nil || s.LastChange.To != updNew {
		t.Fatalf("after a quiet run: %+v, %+v", s.LastRun, s.LastChange)
	}
}

func TestUpdateStatusForTheServerSaysHoldAndChannel(t *testing.T) {
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

// A dry run changes nothing, the server's status included.
func TestUpdateDryRunLeavesTheServersStatusAlone(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	if code, _ := e.run("--dry-run"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if e.exists("state/public") || e.exists("state/status.json") {
		t.Error("a dry run wrote a status")
	}
}

// A release that needs a person is said so, until it is done and the install moves on.
func TestUpdateStatusForTheServerSaysAManualUpgradeIsWaiting(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "Needs a new disk.\n\nmanual-upgrade: true\n", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	s := e.public()
	if s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindManualUpgrade || s.NeedsPerson.Code != "manual-upgrade" || s.LastRun.Code != "manual-upgrade" || s.LastRun.Result != "skipped" {
		t.Fatalf("status = %+v, %+v", s.NeedsPerson, s.LastRun)
	}
	// The person did the steps and ran --to: nothing waits.
	if code, _ := e.run("--to", updNew); code != 0 {
		t.Fatal("--to didn't update")
	}
	if s := e.public(); s.NeedsPerson != nil || s.RecordedVersion != updNew || s.LastRun.Result != "updated" {
		t.Fatalf("after --to: %+v, %+v", s.NeedsPerson, s.LastRun)
	}
}

// A changed worker firewall is a manual upgrade too.
func TestUpdateStatusForTheServerSaysAFirewallChangeIsWaiting(t *testing.T) {
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

// An update that stopped waits for a person: every night says so, and the server's status is
// made again each time, until `taskto-update --clear-block`.
func TestUpdateStatusForTheServerSaysAStopIsWaitingUntilItIsCleared(t *testing.T) {
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

	// The next night is refused, and says so again, with the stop's own last run kept.
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

	// A person has put the old version back, and clears it.
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

// A server that isn't the version on record is blocked, and the status says what to do.
func TestUpdateStatusForTheServerSaysAVersionMismatchIsWaiting(t *testing.T) {
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

// A status that can't be written for the server never changes what the run did.
func TestUpdateWithoutAWritableServerStatusStillUpdates(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	// A file where its directory goes.
	e.write("state/public", "in the way")
	if code, out := e.run(); code != 0 || !strings.Contains(out, "can't write") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	e.wantStatus("updated", "")
}

// The server is only given what the updater writes for it: compose.yaml mounts that one
// directory, read-only, the safety check says so, and the server's side only reads.
func TestServerReadsTheUpdaterStatusOnly(t *testing.T) {
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

// A pending manual upgrade stays while runs can't look at the releases (a hold, the channel
// off): the steps weren't done. A run that looks and finds nothing in the way clears it.
func TestUpdateStatusForTheServerKeepsAPendingManualUpgrade(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "Needs a new disk.\n\nmanual-upgrade: true\n", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := e.public(); s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindManualUpgrade {
		t.Fatalf("pending: %+v", s.NeedsPerson)
	}
	// On hold, the run exits before it looks at the releases.
	e.write("etc/update.hold", "")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := e.public(); s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindManualUpgrade ||
		s.NeedsPerson.Code != "manual-upgrade" || !s.Hold || s.LastRun.Result != "skipped" || s.LastRun.Code != "hold" {
		t.Fatalf("on hold: %+v, %+v", s.NeedsPerson, s.LastRun)
	}
	// The same with the channel off.
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
	// Back on, and nothing newer than what is installed (the person updated by hand, outside the
	// updater's own log): a run that looks clears it.
	e.write("etc/install.env", strings.Replace(e.read("etc/install.env"), "TASKTO_UPDATE_CHANNEL='off'\n", "", 1))
	e.gh.releases = []map[string]any{}
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := e.public(); s.NeedsPerson != nil || s.LastRun.Result != "up-to-date" {
		t.Fatalf("after looking: %+v, %+v", s.NeedsPerson, s.LastRun)
	}
}

// A run that couldn't read the releases doesn't clear it either.
func TestUpdateStatusForTheServerKeepsAManualUpgradeWhenTheReleasesCantBeRead(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "manual-upgrade: true\n", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.gh.releases = nil // the fake answers `null`, which isn't a release list
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if s := e.public(); s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindManualUpgrade || s.LastRun.Result != "skipped" {
		t.Fatalf("status = %+v, %+v", s.NeedsPerson, s.LastRun)
	}
}

// The stop's own instructions, from the transaction, come before what an older run said, when
// the blocked marker can't be read.
func TestUpdateStatusForTheServerGivesTheCurrentReasonForAStop(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/health-bad", updNew+"\n")
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if err := os.Remove(filepath.Join(e.state, "blocked")); err != nil {
		t.Fatal(err)
	}
	// An older, unrelated run is what the private status says.
	e.write("state/status.json", `{"at":"2020-01-01T00:00:00Z","result":"up-to-date","reason":"an older run"}`)
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	s := e.public()
	if s.NeedsPerson == nil || s.NeedsPerson.Kind != updatestatus.KindBlocked || s.NeedsPerson.Code != "stopped" {
		t.Fatalf("status = %+v", s.NeedsPerson)
	}
}
