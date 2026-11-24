package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The updater's Compose inputs, the whole of the data mount, the images the containers are made
// from, the stop of a stack, and the clearing of one: each with a test that fails without it.

// composeCalls are the `docker compose` calls the fake saw, flags included.
func (e *updEnv) composeCalls() []string {
	b, _ := os.ReadFile(filepath.Join(e.fake, "calls-raw"))
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(l, "compose ") {
			out = append(out, l)
		}
	}
	return out
}

// wantNothingStarted: nothing was made, started or stopped, and nothing but the checks ran.
func (e *updEnv) wantNothingStarted() {
	e.t.Helper()
	if n := e.count(`^compose (create|start|up|stop)|^update |^rm `); n != 0 {
		e.t.Errorf("%d calls that make, start or stop containers\n%s", n, strings.Join(e.calls(), "\n"))
	}
}

// Every `docker compose` call, the updater's and the safety script's, names its project, its
// .env and exactly the files of the install (compose.yaml, and compose.override.yaml when there
// is one), whether it is on the install or on the staged copy of the new settings.
func TestUpdateGivesComposeOneExplicitListOfFiles(t *testing.T) {
	for name, override := range map[string]bool{"with an override": true, "without one": false} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			if !override {
				if err := os.Remove(filepath.Join(e.srv, "compose.override.yaml")); err != nil {
					t.Fatal(err)
				}
			}
			e.release(updNew, "", false, false)
			if code, _ := e.run(); code != 0 {
				t.Fatalf("exit %d", code)
			}
			calls := e.composeCalls()
			if len(calls) < 15 {
				t.Fatalf("only %d compose calls", len(calls))
			}
			re := regexp.MustCompile(`^compose --project-directory (\S+) --project-name taskto --env-file (\S+)((?: -f \S+)+)(?: |$)`)
			sawLive, sawStaged := false, false
			for _, c := range calls {
				m := re.FindStringSubmatch(c)
				if m == nil {
					t.Errorf("a compose call without its explicit inputs: %s", c)
					continue
				}
				dir := m[1]
				want := " -f " + dir + "/compose.yaml"
				if override {
					want += " -f " + dir + "/compose.override.yaml"
				}
				if m[2] != dir+"/.env" || m[3] != want {
					t.Errorf("compose is given %s and %s, want %s/.env and %s: %s", m[2], m[3], dir, want, c)
				}
				if dir == e.srv {
					sawLive = true
				} else {
					sawStaged = true
				}
			}
			if !sawLive || !sawStaged {
				t.Errorf("calls on the install: %v, on the staged settings: %v", sawLive, sawStaged)
			}
			// What starts the stack is what was rendered: create and start have the install's list.
			for _, c := range calls {
				if strings.Contains(c, " create ") || strings.Contains(c, " start ") {
					if !strings.Contains(c, "--project-directory "+e.srv+" ") {
						t.Errorf("the stack was made or started from other inputs: %s", c)
					}
				}
			}
		})
	}
}

// A Compose file Compose would discover by itself, or COMPOSE_FILE, is a layout the updater
// doesn't render: it refuses, before anything, with zero create or start calls.
func TestUpdateRefusesComposeFilesItDoesNotRender(t *testing.T) {
	unsafe := "services:\n  cli-worker:\n    privileged: true\n    volumes:\n      - /:/host\n"
	for _, name := range []string{"compose.override.yml", "docker-compose.override.yaml", "docker-compose.override.yml", "docker-compose.yml", "docker-compose.yaml", "compose.yml"} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			e.write("srv/"+name, unsafe)
			if code, _ := e.run(); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			e.wantStatus("failed", name+" is a Compose file the updater doesn't render")
			e.wantUntouched()
			e.wantNothingStarted()
			if e.exists("state/previous") || e.count(`^pull`) != 0 {
				t.Error("something was set aside or pulled")
			}
		})
	}
	for name, setup := range map[string]func(e *updEnv){
		"COMPOSE_FILE in .env":            func(e *updEnv) { e.write("srv/.env", e.read("srv/.env")+"COMPOSE_FILE=compose.yaml:evil.yaml\n") },
		"COMPOSE_FILE in the environment": func(e *updEnv) { e.extra = []string{"COMPOSE_FILE=compose.yaml:evil.yaml"} },
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			setup(e)
			if code, _ := e.run(); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			e.wantStatus("failed", "COMPOSE_FILE is set")
			e.wantNothingStarted()
			if e.count(`^pull`) != 0 {
				t.Error("an image was pulled")
			}
		})
	}
}

// The data is mounted in each of TaskTo's services exactly as the running server has it: the
// volume, its subpath and whether it is read-only, the backup services' included, though their
// profiles are off; and the new settings make no volume: every one they use exists, none is anonymous.
func TestUpdateComparesTheWholeDataMountAndCreatesNoVolume(t *testing.T) {
	for name, tc := range map[string]struct {
		edit, editBackup string
		subpath, ro      bool // the running server's mount
		missing          string
		reason           string
	}{
		"a candidate that mounts a subpath of the volume":  {edit: `.services.taskto.volumes[0].volume.subpath = "other"`, reason: "the /data of taskto is 'taskto_data (subpath other)', not 'taskto_data'"},
		"a candidate that drops the running subpath":       {subpath: true, reason: "the /data of taskto is 'taskto_data', not 'taskto_data (subpath kept)'"},
		"a candidate that mounts the volume read-only":     {edit: `.services.taskto.volumes[0].read_only = true`, reason: "'taskto_data (read-only)', not 'taskto_data'"},
		"a running server whose mount is read-only":        {ro: true, reason: "the /data of taskto is 'taskto_data', not 'taskto_data (read-only)'"},
		"an inactive backup job on another volume":         {editBackup: `.services.backup.volumes[0].source = "replacement" | .volumes.replacement = {name: "replacement"}`, reason: "the /data of backup is 'replacement'"},
		"an inactive nightly backup on a host directory":   {editBackup: `.services["backup-schedule"].volumes[0] = {type: "bind", source: "/srv/x", target: "/data"}`, reason: "the /data of backup-schedule is 'a bind mount'"},
		"an inactive nightly backup on a temporary volume": {editBackup: `.services["backup-schedule"].volumes[0] = {type: "tmpfs", target: "/data"}`, reason: "the /data of backup-schedule is 'a tmpfs mount'"},
		"an inactive backup job on a subpath":              {editBackup: `.services.backup.volumes[0].volume.subpath = "x"`, reason: "the /data of backup is 'taskto_data (subpath x)'"},
		"a candidate that adds a volume that isn't there": {
			edit:    `.services.taskto.volumes += [{type: "volume", source: "extra", target: "/extra", volume: {}}] | .volumes.extra = {name: "taskto_extra"}`,
			missing: "taskto_extra", reason: "the volume 'taskto_extra', which doesn't exist",
		},
		"a candidate with an anonymous volume": {edit: `.services.taskto.volumes += [{type: "volume", target: "/scratch", volume: {}}]`, reason: "an anonymous volume"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			if tc.edit != "" {
				e.write("fake/config-edit", tc.edit)
			}
			if tc.editBackup != "" {
				e.write("fake/config-edit-backup", tc.editBackup)
			}
			if tc.subpath {
				e.write("fake/running-subpath", "kept")
			}
			if tc.ro {
				e.write("fake/running-ro", "")
			}
			if tc.missing != "" {
				e.write("fake/missing-volumes", tc.missing+"\n")
			}
			if code, _ := e.run(); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			e.wantStatus("failed", tc.reason)
			e.wantUntouched()
			e.wantNothingStarted()
			if e.exists("state/previous") {
				t.Error("the old files were set aside before the checks passed")
			}
			if e.count(`^volume (create|rm)`) != 0 {
				t.Error("a volume was made or removed")
			}
		})
	}
	// The backup services are rendered with their profiles on, in this install and in the new settings.
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	n := 0
	for _, c := range e.composeCalls() {
		if strings.Contains(c, "--profile backup ") && strings.Contains(c, "--profile backup-schedule ") && strings.Contains(c, "--profile cli ") && strings.Contains(c, " config --format json") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d renders with the backup profiles on, want 2 (the install's and the new settings')\n%s", n, strings.Join(e.composeCalls(), "\n"))
	}
}

// What the report and the runbook say about the data volume is the recorded one, never a name
// assumed.
func TestUpdateReportsAndRunbookUseTheRecordedDataVolume(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/volname", "acme_data")
	e.write("fake/health-bad", updNew+"\n")
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	st := e.wantStatus("stopped", "in the data volume 'acme_data'")
	if strings.Contains(fmt.Sprint(st["reason"]), "taskto_data") {
		t.Error("the report names a volume nobody recorded")
	}
	if tx := e.transaction(); tx["volume"] != "acme_data" || tx["data_mount"] != "acme_data" {
		t.Errorf("the record = %v", tx)
	}
	if _, out := e.run(); !strings.Contains(out, "in the data volume 'acme_data'") {
		t.Errorf("the blocked run doesn't name the volume\n%s", out)
	}
	runbook := readRepoFile(t, "deploy/INSTALL.md")
	rollback := runbook[strings.Index(runbook, "\n## Rollback\n"):strings.Index(runbook, "\n## Backups\n")]
	if strings.Contains(rollback, "taskto_data") {
		t.Error("the runbook's Rollback names taskto_data, not the recorded volume")
	}
	for _, want := range []string{"VOLUME=", "/var/lib/taskto-update/transaction", `"$VOLUME"`} {
		if !strings.Contains(rollback, want) {
			t.Errorf("the runbook's Rollback doesn't have %q", want)
		}
	}
}

// A tag can move between the last check and the start. The containers are made first, each is
// checked to have the verified image, and only then started: a container with another image is
// removed and the old version put back.
func TestUpdateChecksTheImagesOfTheContainersBeforeStartingThem(t *testing.T) {
	// Forward: a tag moves after the final check, before `create`.
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/retag-on-create", "ghcr.io/taskto-ai/taskto-cli-worker:"+updNew+"\n")
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	e.wantStatus("rolled-back", "the container of cli-worker was made from the image 'sha256:swapped-")
	if e.count(`^compose start .* @`+updNew+`$`) != 0 {
		t.Errorf("a container made from an image that moved was started\n%s", strings.Join(e.calls(), "\n"))
	}
	if e.count(`^rm id-`) != 1 {
		t.Errorf("the containers that were made and not started weren't removed\n%s", strings.Join(e.calls(), "\n"))
	}
	if made, rm, back := e.index(`^compose create .* @`+updNew+`$`), e.index(`^rm `), e.index(`^compose start .* @`+updOld+`$`); made >= rm || rm >= back {
		t.Errorf("create at %d, removal at %d, the old version's start at %d", made, rm, back)
	}
	e.wantOldFilesBack()
	if strings.TrimSpace(e.read("fake/running")) != updOld || e.exists("state/transaction") {
		t.Error("the old version isn't back, or the record is left")
	}
	if e.count(`^compose stop`) != 0 {
		t.Error("the stack was stopped, though nothing of the new version ran")
	}

	// The way back is checked the same way, against the old ids: a moved old tag is never started.
	e = newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.failCalls(`^compose start .* @` + updNew + `$`)
	e.write("fake/retagged", "ghcr.io/taskto-ai/taskto:"+updOld+"\n")
	e.write("fake/retag-wins", "")
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	e.wantStatus("rollback-failed", "the container of taskto was made from the image 'sha256:swapped-")
	if e.count(`^compose start .* @`+updOld+`$`) != 0 {
		t.Error("the old version was started on an image that moved")
	}
	if e.count(`^rm id-`) != 1 {
		t.Error("the container that was made from the moved image wasn't removed")
	}
}

// After the new server has started, restart policies are turned off before the stop, the stop
// is confirmed, and when it can't be the result says so.
func TestUpdateStopTurnsOffRestartPoliciesAndConfirmsTheStop(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/health-bad", updNew+"\n")
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	st := e.wantStatus("stopped", "stack is stopped")
	for _, want := range []string{"restart policies", "docker update --restart=no", "restore the backup"} {
		if !strings.Contains(fmt.Sprint(st["reason"]), want) {
			t.Errorf("the report doesn't say %q: %v", want, st["reason"])
		}
	}
	policy, stop, check := e.index(`^update --restart=no id-taskto id-cli-worker id-caddy @`), e.index(`^compose stop`), e.index(`^ps -q `)
	if policy < 0 || stop < policy || check < stop {
		t.Errorf("restart policies at %d, stop at %d, check that nothing runs at %d: want that order\n%s", policy, stop, check, strings.Join(e.calls(), "\n"))
	}

	for name, tc := range map[string]struct {
		fail       []string
		noop       bool
		result     string
		reasonPart string
		notIn      string
	}{
		"the stop fails":                   {fail: []string{`^compose stop`}, result: "stop-failed", reasonPart: "could NOT be confirmed stopped", notIn: "The stack is stopped"},
		"the stop does nothing":            {noop: true, result: "stop-failed", reasonPart: "could NOT be confirmed stopped", notIn: "The stack is stopped"},
		"the policies can't be turned off": {fail: []string{`^update `}, result: "stopped", reasonPart: "restart policies of its containers couldn't be turned off"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			e.write("etc/update-notify-url", e.server.URL+"/notify/x\n")
			e.write("fake/health-bad", updNew+"\n")
			if len(tc.fail) > 0 {
				e.failCalls(tc.fail...)
			}
			if tc.noop {
				e.write("fake/stop-noop", "")
			}
			if code, _ := e.run(); code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
			st := e.wantStatus(tc.result, tc.reasonPart)
			if tc.notIn != "" && strings.Contains(fmt.Sprint(st["reason"]), tc.notIn) {
				t.Errorf("the report says %q: %v", tc.notIn, st["reason"])
			}
			// Either way it blocks, the release isn't tried again, the backup is named, a person is told.
			if tx := e.transaction(); tx["phase"] != "stopped" || !e.exists("state/blocked") || !strings.Contains(e.read("state/failed"), updNew+"\n") {
				t.Errorf("the record = %v", tx)
			}
			if b, _ := st["backup"].(string); b == "" || !strings.Contains(fmt.Sprint(st["reason"]), "restore the backup "+b) {
				t.Errorf("the report doesn't name the backup: %v", st)
			}
			e.gh.mu.Lock()
			notified := len(e.gh.notifications) == 1 && e.gh.notifications[0].Body["result"] == tc.result
			e.gh.mu.Unlock()
			if !notified {
				t.Errorf("the alert doesn't say %s: %+v", tc.result, e.gh.notifications)
			}
			if code, out := e.run(); code != 1 || !strings.Contains(out, "blocked") {
				t.Errorf("the next run: exit %d\n%s", code, out)
			}
		})
	}
}

// Every curl the updater runs starts with -q: root's .curlrc changes nothing.
func TestUpdateCurlIgnoresTheCurlrc(t *testing.T) {
	script := readRepoFile(t, "deploy/taskto-update.sh")
	n := 0
	for _, m := range regexp.MustCompile(`"\$CURL"( [^\n]{0,6})?`).FindAllStringSubmatch(script, -1) {
		n++
		if m[1] != " -q " && !strings.HasPrefix(m[1], " -q ") {
			t.Errorf("a curl call that doesn't start with -q: %q", m[0])
		}
	}
	if n < 8 {
		t.Errorf("only %d curl calls found", n)
	}
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("etc/update-notify-url", e.server.URL+"/notify/x\n")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(e.fake, "curl-calls"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 10 {
		t.Fatalf("only %d curl calls", len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "-q ") {
			t.Errorf("curl was run as %q", l)
		}
	}
}

// An install on a version that predates the safety script can't be updated: the message points
// at the runbook's Updates section by its anchor.
func TestUpdateRefusesAVersionWithoutSafetyRulesAndPointsAtTheRunbook(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	delete(e.gh.files[shaOf(updOld)], "deploy/safety-check.sh")
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	e.wantStatus("failed", "deploy/INSTALL.md#updates")
	e.wantUntouched()
	e.wantNothingStarted()
}

// Clearing a stop checks what the update checks of a running install: the firewall's rules and
// the safety rules, in the running containers, with the rules of the version that runs.
func TestClearBlockChecksTheFirewallAndTheSafetyRules(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/health-bad", updNew+"\n")
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	// A person has put the old version back, by hand.
	e.setVersion(updOld)
	e.setInstallVersion(updOld)
	e.setRunning(updOld)
	_ = os.Remove(filepath.Join(e.fake, "health-bad"))
	for name, tc := range map[string]struct {
		setup, undo func()
		reason      string
	}{
		"the worker's firewall rules aren't in": {func() { e.write("fake/no-rules", "") }, func() { _ = os.Remove(filepath.Join(e.fake, "no-rules")) }, "the worker's firewall isn't in place"},
		"the server runs as root":               {func() { e.mutate(updOld, "root") }, func() { _ = os.Remove(filepath.Join(e.fake, "mutate")) }, "the safety rules don't hold in the running containers: FAIL: the server doesn't run as 65532"},
		"the worker has a setuid program":       {func() { e.mutate(updOld, "suid") }, func() { _ = os.Remove(filepath.Join(e.fake, "mutate")) }, "setuid or setgid"},
	} {
		tc.setup()
		code, out := e.run("--clear-block")
		if code != 1 || !strings.Contains(out, "refusing: ") || !strings.Contains(out, tc.reason) {
			t.Errorf("%s: exit %d\n%s", name, code, out)
		}
		if !e.exists("state/transaction") || !e.exists("state/blocked") {
			t.Errorf("%s: the stop was cleared", name)
		}
		tc.undo()
	}
	if code, out := e.run("--clear-block"); code != 0 {
		t.Fatalf("clear-block with everything in place: exit %d\n%s", code, out)
	}
	if e.exists("state/transaction") || e.exists("state/blocked") {
		t.Error("the stop wasn't cleared")
	}
	// The safety rules it ran are the running version's own, from its commit.
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	found := false
	for _, r := range e.gh.requests {
		found = found || r == "/raw/taskto-ai/taskto/"+shaOf(updOld)+"/deploy/safety-check.sh"
	}
	if !found {
		t.Error("the safety rules of the running version weren't fetched at its commit")
	}
}

// The docs say what the rollback tags protect the old images from, and what they don't.
func TestDocsSayWhatTheRollbackTagsProtect(t *testing.T) {
	install := readRepoFile(t, "docs/install.md")
	runbook := readRepoFile(t, "deploy/INSTALL.md")
	script := readRepoFile(t, "deploy/taskto-update.sh")
	for name, text := range map[string]string{"docs/install.md": install, "deploy/INSTALL.md": runbook, "the updater": script} {
		if strings.Contains(text, "image prune can't take") || strings.Contains(text, "nothing that prunes images") {
			t.Errorf("%s says the rollback tags protect the images from a prune", name)
		}
	}
	for name, text := range map[string]string{"docs/install.md": install, "deploy/INSTALL.md": runbook} {
		if !strings.Contains(strings.ToLower(text), "don't prune images while an update is blocked") {
			t.Errorf("%s doesn't tell operators not to prune images while an update is blocked", name)
		}
	}
	if !strings.Contains(install, "`docker image prune -a`") || !strings.Contains(install, "an image no container uses is removed whatever its tags") {
		t.Error("docs/install.md doesn't say that prune -a takes tagged images too")
	}
	// The main channel has no gate, and the docs say so where the channels are chosen.
	if !strings.Contains(install, "`main` ignores releases and the `manual-upgrade: true` marker") || !strings.Contains(install, "every merge") {
		t.Error("docs/install.md doesn't say that the main channel ignores the manual-upgrade markers and takes every merge")
	}
}
