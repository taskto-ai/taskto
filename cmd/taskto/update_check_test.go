package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// taskto-update --check: what a run would install next, found the same way and published for
// Settings → About, with nothing pulled, installed or changed, and no run's status written.

// releasedAt sets when a fake release was published.
func (e *updEnv) releasedAt(tag, at string) {
	for _, r := range e.gh.releases {
		if r["tag_name"] == tag {
			r["published_at"] = at
		}
	}
}

// wantOnlyLooked: a check changed nothing but what it found: no pull, no run's status, no
// notification, the install as it was.
func (e *updEnv) wantOnlyLooked() {
	e.t.Helper()
	e.wantUntouched()
	if e.count(`^pull`) != 0 {
		e.t.Error("a check pulled an image")
	}
	if e.exists("state/status.json") {
		e.t.Error("a check wrote a run's status")
	}
	if len(e.gh.notifications) != 0 {
		e.t.Error("a check notified")
	}
}

func TestUpdateCheckFindsTheNextReleaseAndChangesNothing(t *testing.T) {
	e := newUpdEnv(t)
	e.write("etc/update-notify-url", e.server.URL+"/notify/x\n")
	e.release(updOld, "", false, false)
	e.release(updNew, "Notes.\n", false, false)
	e.releasedAt(updNew, "2026-11-25T10:00:00Z")
	if code, out := e.run("--check"); code != 0 || !strings.Contains(out, "available="+updNew) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	e.wantOnlyLooked()
	s := e.public()
	a := s.Available
	if a == nil || a.Version != updNew || a.SHA != shaOf(updNew) || a.ManualUpgrade || a.Repo != "taskto-ai/taskto" ||
		!a.Published.Equal(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("available = %+v", a)
	}
	if s.Looked.IsZero() || s.LastRun != nil || s.RecordedVersion != updOld || s.Running != nil {
		t.Fatalf("status = %+v", s)
	}
	// It looked at the images in the registry, and pulled none.
	if e.count(`^manifest inspect \S+:`+updNew+` @`) == 0 {
		t.Error("the check didn't look for the images")
	}

	// The run that installs it leaves nothing available, and when it looked.
	if code, _ := e.run(); code != 0 {
		t.Fatal("update")
	}
	if s := e.public(); s.Available != nil || s.Looked.IsZero() || s.RecordedVersion != updNew {
		t.Fatalf("after the update: available %+v, looked %v", s.Available, s.Looked)
	}
	// Nothing newer: none, and the check says when it looked.
	if code, _ := e.run("--check"); code != 0 {
		t.Fatal("check")
	}
	if s := e.public(); s.Available != nil || s.Looked.IsZero() || s.LastRun == nil || s.LastRun.Result != "updated" {
		t.Fatalf("up to date: %+v", s)
	}
}

func TestUpdateCheckMarksAReleaseThatNeedsAPerson(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "Needs a new disk.\n\nmanual-upgrade: true\n", false, false)
	e.release("v0.4.0", "Nothing special.\n", false, false)
	if code, _ := e.run("--check"); code != 0 {
		t.Fatal("check")
	}
	e.wantOnlyLooked()
	s := e.public()
	// The newest is what a person installs, past the release that needs steps by hand.
	if s.Available == nil || s.Available.Version != "v0.4.0" || !s.Available.ManualUpgrade {
		t.Fatalf("available = %+v", s.Available)
	}
	if s.NeedsPerson == nil || s.NeedsPerson.Kind != "manual-upgrade" {
		t.Fatalf("needs = %+v", s.NeedsPerson)
	}
	// A run says the same.
	if code, _ := e.run(); code != 0 {
		t.Fatal("run")
	}
	if s := e.public(); s.Available == nil || s.Available.Version != "v0.4.0" || !s.Available.ManualUpgrade {
		t.Fatalf("after a run: %+v", s.Available)
	}
}

func TestUpdateCheckOnTheMainChannel(t *testing.T) {
	sha := func(c string) string { return strings.Repeat(c, 40) }
	a, unpublished, green := sha("a"), sha("d"), sha("e")
	e := newUpdEnv(t)
	e.setVersion(a)
	e.setInstallVersion(a)
	e.setRunning(a)
	e.publish(a)
	e.write("etc/install.env", e.read("etc/install.env")+"TASKTO_UPDATE_CHANNEL='main'\n")
	run := func(sha, at string) map[string]any {
		return map[string]any{"head_sha": sha, "head_branch": "main", "conclusion": "success", "head_commit": map[string]any{"timestamp": at}}
	}
	e.gh.runs = []map[string]any{run(unpublished, "2026-11-26T09:00:00Z"), run(green, "2026-11-26T08:00:00Z"), run(a, "2026-11-25T08:00:00Z")}
	e.gh.compare = map[string]string{unpublished: "ahead", green: "ahead", a: "identical"}
	e.publish(green)
	e.failCalls(`^manifest inspect \S+:` + unpublished + ` @`)
	if code, _ := e.run("--check"); code != 0 {
		t.Fatal("check")
	}
	if e.count(`^pull`) != 0 || e.exists("state/status.json") {
		t.Error("a check pulled, or wrote a run's status")
	}
	s := e.public()
	if s.Available == nil || s.Available.Version != green || s.Available.SHA != green || s.Available.ManualUpgrade ||
		!s.Available.Published.Equal(time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)) || s.Channel != "main" {
		t.Fatalf("available = %+v", s.Available)
	}
	if !strings.Contains(e.read("srv/.env"), "TASKTO_VERSION="+a+"\n") {
		t.Error("the check changed the version on record")
	}
}

// What can't be told keeps what was found; what was found for another version is not shown; the
// channel off shows nothing.
func TestUpdateCheckKeepsOnlyWhatStillHolds(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	if code, _ := e.run("--check"); code != 0 {
		t.Fatal("check")
	}
	e.gh.failPages = map[int]bool{1: true}
	if code, _ := e.run("--check"); code != 0 {
		t.Fatal("check, GitHub down")
	}
	if s := e.public(); s.Available == nil || s.Available.Version != updNew {
		t.Fatalf("GitHub didn't answer, and what was found went: %+v", s.Available)
	}
	e.gh.failPages = nil

	// Found from another version: not this install's.
	e.write("state/available.json", `{"from":"v0.1.0","looked":"2026-11-26T06:00:00Z","version":"v0.3.0","sha":"","published":"","manual_upgrade":false,"repo":"taskto-ai/taskto"}`)
	if code, _ := e.run("--publish-status"); code != 0 {
		t.Fatal("publish")
	}
	if s := e.public(); s.Available != nil || !s.Looked.IsZero() {
		t.Fatalf("another version's: %+v, %v", s.Available, s.Looked)
	}

	e.write("etc/install.env", e.read("etc/install.env")+"TASKTO_UPDATE_CHANNEL='off'\n")
	if code, _ := e.run("--check"); code != 0 {
		t.Fatal("check, off")
	}
	if s := e.public(); s.Available != nil {
		t.Fatalf("the channel is off: %+v", s.Available)
	}
}

// A check never waits for a run or keeps one from starting: while a run holds the lock it records
// what it found, and the run publishes it.
func TestUpdateCheckDoesNotWaitForARun(t *testing.T) {
	needTools(t, "flock")
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	if code, _ := e.run("--publish-status"); code != 0 {
		t.Fatal("publish")
	}
	cmd := e.command("--check")
	flock, _ := exec.LookPath("flock")
	cmd.Path, cmd.Args = flock, append([]string{"flock", filepath.Join(e.dir, "lock")}, cmd.Args...)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "a run is going") {
		t.Fatalf("%v\n%s", err, out)
	}
	if e.public().Available != nil {
		t.Error("the check published while a run held the lock")
	}
	if !strings.Contains(e.read("state/available.json"), updNew) {
		t.Error("the check didn't record what it found")
	}
	if code, _ := e.run("--publish-status"); code != 0 {
		t.Fatal("publish")
	}
	if a := e.public().Available; a == nil || a.Version != updNew {
		t.Fatalf("available = %+v", a)
	}
}

func TestUpdateCheckTakesNoOtherOption(t *testing.T) {
	e := newUpdEnv(t)
	for _, args := range [][]string{{"--check", "--dry-run"}, {"--check", "--to", updNew}, {"--check", "--publish-status"}, {"--check", "--requested"}, {"--check", "--clear-block"}} {
		if code, _ := e.run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if _, err := os.Stat(filepath.Join(e.state, "available.json")); err == nil {
		t.Error("a refused check wrote something")
	}
}

// The check's units: a timer every few hours, a service that runs --check and nothing else, and
// the runbook installs both.
func TestUpdateCheckUnits(t *testing.T) {
	timer := readRepoFile(t, "deploy/taskto-update-check.timer")
	for _, want := range []string{"Persistent=true", "RandomizedDelaySec=", "OnCalendar=", "WantedBy=timers.target"} {
		if !strings.Contains(timer, want) {
			t.Errorf("taskto-update-check.timer doesn't have %q", want)
		}
	}
	service := readRepoFile(t, "deploy/taskto-update-check.service")
	if !strings.Contains(service, "Type=oneshot") || !strings.Contains(service, "\nExecStart=/usr/local/sbin/taskto-update --check\n") {
		t.Error("taskto-update-check.service doesn't run taskto-update --check")
	}
	runbook := readRepoFile(t, "deploy/INSTALL.md")
	for _, want := range []string{"taskto-update-check.service", "taskto-update-check.timer", "taskto-update --check"} {
		if !strings.Contains(runbook, want) {
			t.Errorf("deploy/INSTALL.md doesn't have %q", want)
		}
	}
}

// An install that isn't on a version (a build of no release or commit) can't be told what is
// newer: that is not "nothing newer", so the check records no look.
func TestUpdateCheckOnAnInstallWithNoVersionRecordsNoLook(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.setVersion("dev")
	if code, out := e.run("--check"); code != 0 || !strings.Contains(out, "neither a release nor a commit") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if e.exists("state/available.json") {
		t.Error("the check recorded a look it couldn't make")
	}
	if s := e.public(); !s.Looked.IsZero() || s.Available != nil {
		t.Fatalf("status = %+v", s)
	}
}

// Step 16 copies the check's units only when the version has them, and stops before the VM's
// part when a copy fails.
func TestRunbookCheckUnitsAreOptionalAndACopyFailureStops(t *testing.T) {
	runbook := readRepoFile(t, "deploy/INSTALL.md")
	for _, want := range []string{
		`taskto-update-check.*) git cat-file -e "${TASKTO_VERSION}:deploy/$f"`,
		"  ) && vm <<'EOF'",
		"[ ! -e /etc/systemd/system/taskto-update-check.timer ] || systemctl enable --now taskto-update-check.timer",
	} {
		if !strings.Contains(runbook, want) {
			t.Errorf("deploy/INSTALL.md doesn't have %q", want)
		}
	}
}
