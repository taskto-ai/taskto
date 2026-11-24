package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// How deploy/taskto-update.sh fails: before the new server has started it puts the old version
// back; after, it stops the stack, keeps the backup and the record, and leaves the rest to a
// person; and it never restores data, empties or copies a volume. Plus what it checks before it
// starts anything, how it pages through the releases, and how it alerts.

// installAt makes the install, the server and the published files a version.
func (e *updEnv) installAt(v string) {
	e.setVersion(v)
	e.setInstallVersion(v)
	e.setRunning(v)
	e.publish(v)
}

func (e *updEnv) transaction() map[string]any {
	e.t.Helper()
	var tx map[string]any
	if err := json.Unmarshal([]byte(e.read("state/transaction")), &tx); err != nil {
		e.t.Fatal(err)
	}
	return tx
}

func (e *updEnv) wantOldFilesBack() {
	e.t.Helper()
	for name, want := range map[string]string{
		"srv/compose.yaml": "# compose " + updOld + "\n# TASKTO_IMAGE_PREFIX\n", "srv/deploy/Caddyfile": "caddy\n",
	} {
		if got := e.read(name); got != want {
			e.t.Errorf("%s = %q, want the old one %q", name, got, want)
		}
	}
	if !strings.Contains(e.read("srv/.env"), "TASKTO_VERSION="+updOld+"\n") || !strings.Contains(e.read("etc/install.env"), "TASKTO_VERSION='"+updOld+"'") {
		e.t.Error("a version file isn't the old one")
	}
}

// wantNeverRestores: whatever happened, the updater never restored data, emptied, copied or
// removed a volume, or took the stack down.
func (e *updEnv) wantNeverRestores() {
	e.t.Helper()
	if n := e.count(`restore|^compose down|^compose run|^run |^volume (rm|create|prune)|^rm |-v `); n != 0 {
		e.t.Errorf("a call the updater never makes was made\n%s", strings.Join(e.calls(), "\n"))
	}
	if e.exists("fake/restored") {
		e.t.Error("a backup was restored")
	}
}

// The updater's lines of code never restore a backup or touch a volume: the failure of an
// update is a person's to put right, with deploy/INSTALL.md's Rollback.
func TestUpdaterNeverRestoresOrTouchesVolumes(t *testing.T) {
	script := readRepoFile(t, "deploy/taskto-update.sh")
	var code []string
	for _, l := range strings.Split(script, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			code = append(code, l)
		}
	}
	body := strings.Join(code, "\n")
	for _, never := range []string{
		"taskto restore", "restore -", "restore_data", "move_data_aside", "rolled_back", "compose down", "compose run", `"$DOCKER" run`, "docker run",
		"volume create", "volume rm", "volume prune", "cp -a", "find /from", "busybox", "HELPER", "--volumes", "down -v", "docker system", "docker image prune",
	} {
		if strings.Contains(body, never) {
			t.Errorf("the updater has %q", never)
		}
	}
	// The one thing it does with a volume is read where it is, for the disk check.
	for _, m := range regexp.MustCompile(`(?:"\$DOCKER"|docker) volume (\w+)`).FindAllStringSubmatch(body, -1) {
		if m[1] != "inspect" {
			t.Errorf("the updater runs docker volume %s", m[1])
		}
	}
	for _, want := range []string{"compose stop", "halt stopped stopped", "--clear-block"} {
		if !strings.Contains(body, want) {
			t.Errorf("the updater doesn't have %q", want)
		}
	}
	// The notification's URL never reaches a command line, where ps would show it.
	for _, want := range []string{`"$CURL" -q -K -`, "connect-timeout", "max-time"} {
		if !strings.Contains(body, want) {
			t.Errorf("the notification doesn't use %q", want)
		}
	}
	if strings.Contains(body, `"$CURL" "$url"`) || !strings.Contains(body, `printf 'url = "%s"`) {
		t.Error("the notification's URL isn't handed to curl in its config")
	}
}

func TestSafetyCheckTakesOnlyItsOwnArguments(t *testing.T) {
	script, _ := filepath.Abs(filepath.Join("..", "..", "deploy", "safety-check.sh"))
	for _, args := range [][]string{{"--nope"}, {"--static"}, {"--static", "a", "b"}} {
		out, err := exec.Command("bash", append([]string{script}, args...)...).CombinedOutput()
		var ee *exec.ExitError
		if err == nil || !asExit(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(string(out), "usage") {
			t.Errorf("safety-check.sh %v: %v\n%s", args, err, out)
		}
	}
}

func asExit(err error, target **exec.ExitError) bool { return errors.As(err, target) }

// The new Compose settings are checked against the safety rules before anything starts: a
// candidate that breaks one is refused with nothing changed, and its servers are never started.
func TestUpdateRefusesUnsafeCandidatesWithoutStartingThem(t *testing.T) {
	for name, tc := range map[string]struct {
		edit, reason string
		imageUser    string
	}{
		"the server runs as root":         {edit: `.services.taskto.user = "0"`, reason: "the server wouldn't run as 65532"},
		"the server runs as root by name": {edit: `.services.taskto.user = "root:root"`, reason: "the server wouldn't run as 65532"},
		"the server runs as another user": {edit: `.services.taskto.user = "1000"`, reason: "the server wouldn't run as 65532"},
		// The live check wants exactly 65532:65532; so does this one.
		"the server's user has no group":      {edit: `.services.taskto.user = "65532"`, reason: "the server wouldn't run as 65532:65532"},
		"the server's user has another group": {edit: `.services.taskto.user = "65532:0"`, reason: "the server wouldn't run as 65532:65532"},
		"the image's user has no group":       {imageUser: "65532", reason: "the server wouldn't run as 65532:65532"},
		// Mounts inherited or made by Compose's other means.
		"the worker takes the server's mounts":          {edit: `.services["cli-worker"].volumes_from = ["taskto"]`, reason: "volumes_from"},
		"another service takes the server's mounts":     {edit: `.services.extra = {image: "caddy:2", volumes_from: ["taskto"]}`, reason: "volumes_from"},
		"a config mounted into the worker":              {edit: `.services["cli-worker"].configs = [{source: "c", target: "/c"}]`, reason: "a config or a secret mounted"},
		"a secret mounted into the worker":              {edit: `.services["cli-worker"].secrets = [{source: "s", target: "/run/secrets/s"}]`, reason: "a config or a secret mounted"},
		"the server image's own user is root":           {imageUser: "root", reason: "the server wouldn't run as 65532"},
		"a development switch in the server":            {edit: `.services.taskto.environment.TASKTO_INSECURE_COOKIES = "true"`, reason: "TASKTO_INSECURE_COOKIES"},
		"a secret in the worker":                        {edit: `.services["cli-worker"].environment.OPENAI_API_KEY = "x"`, reason: "OPENAI_API_KEY"},
		"the unsafe agents switch on the server":        {edit: `.services.taskto.command = ["-unsafe-privileged-agents"]`, reason: "a container's command has a development switch"},
		"a capability added to the server":              {edit: `.services.taskto.cap_add = ["NET_BIND_SERVICE"]`, reason: "the server has capabilities added"},
		"NET_ADMIN given to the worker":                 {edit: `.services["cli-worker"].cap_add += ["NET_ADMIN"]`, reason: "the worker has capabilities beyond"},
		"the worker loses a capability it needs":        {edit: `.services["cli-worker"].cap_add -= ["KILL"]`, reason: "the worker has capabilities beyond"},
		"a privileged worker":                           {edit: `.services["cli-worker"].privileged = true`, reason: "or is privileged"},
		"a server that keeps its capabilities":          {edit: `del(.services.taskto.cap_drop)`, reason: "isn't capability-free"},
		"a worker that can gain privileges":             {edit: `.services["cli-worker"].security_opt = []`, reason: "isn't capability-free"},
		"the worker has the server's data":              {edit: `.services["cli-worker"].volumes += [{type: "volume", source: "data", target: "/data", volume: {}}]`, reason: "the worker can see the server's data"},
		"the worker has the server's data under a name": {edit: `.services["cli-worker"].volumes += [{type: "volume", source: "alias", target: "/x", volume: {}}] | .volumes.alias = {name: "taskto_data"}`, reason: "the worker can see the server's data"},
		"the worker has a host path":                    {edit: `.services["cli-worker"].volumes += [{type: "bind", source: "/etc", target: "/host"}]`, reason: "the worker can see the server's data, or has a host path"},
		"the server has the Docker socket":              {edit: `.services.taskto.volumes += [{type: "bind", source: "/var/run/docker.sock", target: "/var/run/docker.sock"}]`, reason: "a container has the Docker socket"},
		"the worker on the server's network":            {edit: `.services["cli-worker"].networks = {default: null}`, reason: "the server is on the worker's network"},
		"the worker on two networks":                    {edit: `.services["cli-worker"].networks = {worker: null, default: null}`, reason: "the worker isn't on one network only"},
		"a worker bridge the firewall doesn't know":     {edit: `.networks.worker.driver_opts["com.docker.network.bridge.name"] = "docker9"`, reason: "the worker's bridge isn't taskto-worker"},
		"the CLI workers off in the server":             {edit: `.services.taskto.environment.TASKTO_CLI_WORKERS = "off"`, reason: "CLI workers aren't in container mode"},
		"no worker at all":                              {edit: `del(.services["cli-worker"])`, reason: "the server or the CLI worker isn't in the Compose settings"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			if tc.edit != "" {
				e.write("fake/config-edit", tc.edit)
			}
			if tc.imageUser != "" {
				e.write("fake/image-user", tc.imageUser)
			}
			if code, _ := e.run(); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			e.wantStatus("failed", "break a safety rule")
			if st := e.status(); !strings.Contains(fmt.Sprint(st["reason"]), tc.reason) {
				t.Errorf("reason %q doesn't say %q", st["reason"], tc.reason)
			}
			// Nothing was started, backed up, or kept as a way back: the candidate was never run.
			e.wantUntouched()
			if e.count(`^compose start`) != 0 || e.exists("state/previous") {
				t.Errorf("an unsafe candidate was started\n%s", strings.Join(e.calls(), "\n"))
			}
			if strings.TrimSpace(e.read("fake/running")) != updOld {
				t.Error("the server was restarted")
			}
		})
	}
}

// Before the new server has started, the old files, .env and images are put back, the old
// version is started and checked, and the release is recorded as failed. The data is never
// touched, so there is nothing to restore.
func TestUpdateRevertsBeforeTheNewServerStarts(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  func(e *updEnv)
		reason string
	}{
		"docker compose start fails before the server starts": {func(e *updEnv) { e.failCalls(`^compose start .* @` + updNew + `$`) }, "docker compose create and start failed"},
		// An image tag moved after the checks, before the start: the start never happens.
		"a tag moves between the checks and the start": {func(e *updEnv) {
			e.write("fake/retag-on-backup", "ghcr.io/taskto-ai/taskto-cli-worker:"+updNew+"\n")
		}, "checking the images failed"},
		// The old tags moved too: the old images come back by the ids the containers had.
		"the old version's tags have moved": {func(e *updEnv) {
			e.write("fake/retagged", "ghcr.io/taskto-ai/taskto:"+updOld+"\nghcr.io/taskto-ai/taskto-cli-worker:"+updOld+"\n")
			e.failCalls(`^compose start .* @` + updNew + `$`)
		}, "docker compose create and start failed"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			tc.setup(e)
			if code, _ := e.run(); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			st := e.wantStatus("rolled-back", tc.reason)
			if st["from"] != updOld || st["to"] != updNew {
				t.Errorf("status = %v", st)
			}
			e.wantOldFilesBack()
			if strings.TrimSpace(e.read("fake/running")) != updOld {
				t.Errorf("the server runs %q", e.read("fake/running"))
			}
			if e.exists("state/transaction") || e.exists("state/blocked") {
				t.Error("a record or a marker is left after a revert that worked")
			}
			if !strings.Contains(e.read("state/failed"), updNew+"\n") {
				t.Error("the release isn't recorded as failed")
			}
			// The old images are the old tags again, by the ids the running containers had.
			for _, c := range []string{"taskto", "cli-worker"} {
				img := map[string]string{"taskto": "taskto", "cli-worker": "taskto-cli-worker"}[c]
				if e.count(`^tag sha256:run-`+c+`-`+updOld+` ghcr.io/taskto-ai/`+img+`:`+updOld+` @`+updOld+`$`) != 1 {
					t.Errorf("the old %s image wasn't put back by its id\n%s", c, strings.Join(e.calls(), "\n"))
				}
			}
			if e.count(`swapped`) != 0 {
				t.Error("an image was put back by what a moved tag points at")
			}
			// The new server never ran, so nothing but the old version was started and the stack
			// was never stopped.
			if e.count(`^compose stop`) != 0 {
				t.Error("the stack was stopped for a failure before the new server started")
			}
			e.wantNeverRestores()
			if b, _ := st["backup"].(string); b == "" || !e.exists(strings.TrimPrefix(b, e.dir+"/")) {
				t.Error("the backup isn't kept")
			}
			// The release that failed isn't tried again every night.
			ups, backups := e.count(`^compose start`), e.count(`^compose exec -T taskto taskto backup`)
			if code, _ := e.run(); code != 0 {
				t.Fatalf("next run: exit %d", code)
			}
			e.wantStatus("skipped", "failed here before")
			if e.count(`^compose start`) != ups || e.count(`^compose exec -T taskto taskto backup`) != backups {
				t.Errorf("the next run tried the failed release again\n%s", strings.Join(e.calls(), "\n"))
			}
		})
	}
	// The tag that moved is never started.
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/retag-on-backup", "ghcr.io/taskto-ai/taskto:"+updNew+"\n")
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if e.count(`^compose start .* @`+updNew+`$`) != 0 {
		t.Errorf("the new version was started on a tag that moved\n%s", strings.Join(e.calls(), "\n"))
	}
}

// After the new server has started (it may have migrated the data) nothing is undone: the stack
// is stopped, the backup and the record are kept, the next runs are blocked, and the status
// says which backup to restore.
func TestUpdateStopsAfterTheNewServerStarted(t *testing.T) {
	for name, setup := range map[string]func(e *updEnv){
		"the server isn't healthy":                    func(e *updEnv) { e.write("fake/health-bad", updNew+"\n") },
		"docker compose start fails after it started": func(e *updEnv) { e.write("fake/upfail-after-start", updNew) },
		"the server runs as root":                     func(e *updEnv) { e.mutate(updNew, "root") },
		"the server has a development switch":         func(e *updEnv) { e.mutate(updNew, "insecure") },
		"the server isn't in container mode":          func(e *updEnv) { e.mutate(updNew, "nomode") },
		"the worker is given NET_ADMIN":               func(e *updEnv) { e.mutate(updNew, "netadmin") },
		"the worker has a setuid program":             func(e *updEnv) { e.mutate(updNew, "suid") },
		"the worker can see the data":                 func(e *updEnv) { e.mutate(updNew, "workerdata") },
		"the proxy still answers as the old version":  func(e *updEnv) { e.write("fake/proxy-version", updOld) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			setup(e)
			code, out := e.run()
			if code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
			st := e.wantStatus("stopped", "stack is stopped")
			backup, _ := st["backup"].(string)
			if backup == "" || !e.exists(strings.TrimPrefix(backup, e.dir+"/")) {
				t.Fatalf("the backup isn't kept: %v", st)
			}
			for _, want := range []string{"restore the backup " + backup, "Rollback", "--clear-block"} {
				if !strings.Contains(fmt.Sprint(st["reason"]), want) {
					t.Errorf("the report doesn't say %q: %v", want, st["reason"])
				}
			}
			if !strings.Contains(out, "result=stopped") {
				t.Error("the journal doesn't say so")
			}
			if got := e.read(strings.TrimPrefix(backup, e.dir+"/")); got != "BACKUP-OF-"+updOld+"\n" {
				t.Errorf("the backup holds %q", got)
			}
			// The stack was stopped, nothing was rolled back, nothing was started again.
			if e.count(`^compose stop`) != 1 {
				t.Errorf("the stack wasn't stopped once\n%s", strings.Join(e.calls(), "\n"))
			}
			if n := e.count(`^compose start`); n != 1 {
				t.Errorf("%d starts: the update's only", n)
			}
			if e.count(`^tag sha256:run-\S+ ghcr.io/\S+:`+updOld+` `) != 0 {
				t.Error("the old images were put back")
			}
			e.wantNeverRestores()
			if !strings.Contains(e.read("srv/.env"), "TASKTO_VERSION="+updNew+"\n") {
				t.Error("the files were put back, though the new server ran")
			}
			// The record stays, in its phase; the marker and the failed release are written.
			tx := e.transaction()
			if tx["phase"] != "stopped" || tx["backup"] != backup || tx["from"] != updOld || tx["to"] != updNew || tx["why"] == "" {
				t.Errorf("the record = %v", tx)
			}
			if b := e.read("state/blocked"); !strings.Contains(b, backup) || !strings.Contains(b, "Rollback") {
				t.Errorf("the blocked marker = %q", b)
			}
			if !strings.Contains(e.read("state/failed"), updNew+"\n") {
				t.Error("the release isn't recorded as failed")
			}
			if last, _ := st["last_change"].(map[string]any); last["result"] != "stopped" {
				t.Errorf("last_change = %v", last)
			}
			// Nothing runs again, whatever it is asked, and the status stays what the failed run said.
			calls := len(e.calls())
			for _, args := range [][]string{nil, {"--to", updNew}, {"--to", "v0.9.0"}, {"--dry-run"}} {
				code, out := e.run(args...)
				if code != 1 || !strings.Contains(out, "blocked") {
					t.Fatalf("the run after the stop (%v): exit %d\n%s", args, code, out)
				}
				if len(e.calls()) != calls {
					t.Fatalf("docker was called by a run that is blocked (%v)\n%s", args, strings.Join(e.calls(), "\n"))
				}
			}
			e.wantStatus("stopped", "stack is stopped")
		})
	}
}

// A marker that can't be written changes nothing: the transaction record alone blocks the next
// run, even when the old version answers, healthy, at /healthz.
func TestUpdateStopStaysBlockedWithoutTheBlockedMarker(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/health-bad", updNew+"\n")
	// A link to nowhere: writing the marker fails, and the marker doesn't "exist".
	if err := os.Symlink(filepath.Join(e.dir, "nowhere", "blocked"), filepath.Join(e.state, "blocked")); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	e.wantStatus("stopped", "")
	if _, err := os.Stat(filepath.Join(e.state, "blocked")); err == nil {
		t.Fatal("the marker was written")
	}
	if !e.exists("state/transaction") {
		t.Fatal("no record")
	}
	// Someone starts the old version again, healthy; the files say the new one.
	_ = os.Remove(filepath.Join(e.fake, "health-bad"))
	e.setRunning(updOld)
	calls := len(e.calls())
	for _, args := range [][]string{nil, {"--to", updNew}} {
		code, out := e.run(args...)
		if code != 1 || !strings.Contains(out, "blocked") || len(e.calls()) != calls {
			t.Fatalf("the run (%v) with a healthy old server: exit %d, calls %d -> %d\n%s", args, code, calls, len(e.calls()), out)
		}
	}
}

// When putting the old version back fails too, the install is left as it is, with the record
// and the marker, and nothing runs again until a person has cleared it.
func TestUpdateRevertFailureStopsAndStaysStopped(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	// The new version can't be started, and neither can the old.
	e.failCalls(`^compose start .* @`+updNew+`$`, `^compose start .* @`+updOld+`$`)
	code, out := e.run()
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	e.wantStatus("rollback-failed", "putting "+updOld+" back failed too")
	if !strings.Contains(out, "rollback-failed") {
		t.Error("the journal doesn't say so")
	}
	if n := e.count(`^compose start`); n != 2 {
		t.Errorf("%d starts: the update's and the revert's, no more", n)
	}
	if tx := e.transaction(); tx["phase"] != "rollback-failed" {
		t.Errorf("the record = %v", tx)
	}
	if !e.exists("state/blocked") {
		t.Fatal("nothing stops the next night's run")
	}
	e.wantNeverRestores()
	calls := len(e.calls())
	for _, args := range [][]string{nil, {"--to", updNew}} {
		code, out := e.run(args...)
		if code != 1 || !strings.Contains(out, "blocked") || len(e.calls()) != calls {
			t.Fatalf("the next run (%v): exit %d, calls %d -> %d\n%s", args, code, calls, len(e.calls()), out)
		}
	}
	e.wantStatus("rollback-failed", "")
}

// The revert never depends on the bookkeeping: a `failed` file that can't be written (here, a
// directory) still ends in the old version running. But a release that can't be recorded as
// failed is not forgotten: the update's record stays, which blocks the next run, whatever it is
// asked, with no candidate started, until a person has looked and cleared it.
func TestUpdateRevertThatCantRecordTheFailureStaysBlocked(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.failCalls(`^compose start .* @` + updNew + `$`)
	if err := os.MkdirAll(filepath.Join(e.state, "failed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	e.wantStatus("rolled-back", "couldn't be recorded as failed")
	e.wantOldFilesBack()
	if strings.TrimSpace(e.read("fake/running")) != updOld {
		t.Error("the old version isn't back")
	}
	if !e.exists("state/transaction") || !e.exists("state/blocked") {
		t.Fatal("the record of the update was removed, though the failure wasn't recorded: the next night would try the release again")
	}
	if tx := e.transaction(); tx["phase"] != "rolled-back" {
		t.Errorf("the record = %v", tx)
	}
	// The next runs, whatever they are asked, start nothing.
	starts, calls := e.count(`^compose (create|start)`), len(e.calls())
	for _, args := range [][]string{nil, {"--to", updNew}} {
		code, out := e.run(args...)
		if code != 1 || !strings.Contains(out, "blocked") || !strings.Contains(out, "couldn't be recorded as failed") {
			t.Fatalf("the run after (%v): exit %d\n%s", args, code, out)
		}
	}
	if e.count(`^compose (create|start)`) != starts || len(e.calls()) != calls {
		t.Errorf("a candidate was started by a run that is blocked\n%s", strings.Join(e.calls(), "\n"))
	}
	// Clearing refuses while the failure still can't be recorded, and keeps the record.
	if code, out := e.run("--clear-block"); code != 1 || !strings.Contains(out, "can't record "+updNew+" as failed") || !e.exists("state/transaction") {
		t.Fatalf("clear-block with the record still unwritable: exit %d\n%s", code, out)
	}
	// Once a person has put the state right, clearing records the release, and it isn't tried again.
	if err := os.Remove(filepath.Join(e.state, "failed")); err != nil {
		t.Fatal(err)
	}
	if code, out := e.run("--clear-block"); code != 0 {
		t.Fatalf("clear-block: exit %d\n%s", code, out)
	}
	if !strings.Contains(e.read("state/failed"), updNew+"\n") || e.exists("state/transaction") {
		t.Error("the release wasn't recorded as failed before the record was removed")
	}
	if code, _ := e.run(); code != 0 {
		t.Fatalf("the run after clearing: exit %d", code)
	}
	e.wantStatus("skipped", "failed here before")
	if e.count(`^compose (create|start)`) != starts {
		t.Error("the release that failed was tried again")
	}
}

// An update killed between writing that the release failed and removing the update's record
// leaves both: the next run is blocked (interrupted), and once cleared the release is not
// tried again. The failed list is written to a file of its own and moved into place, before
// the record goes.
func TestUpdateKilledBetweenTheFailureRecordAndTheTransaction(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("state/failed", updNew+"\n")
	e.write("state/transaction", `{"from":"`+updOld+`","to":"`+updNew+`","phase":"starting","backup":"","volume":"taskto_data"}`)
	calls := len(e.calls())
	for _, args := range [][]string{nil, {"--to", updNew}} {
		if code, out := e.run(args...); code != 1 || !strings.Contains(out, "result=interrupted") {
			t.Fatalf("the run (%v): exit %d\n%s", args, code, out)
		}
	}
	if len(e.calls()) != calls {
		t.Error("docker was called by a run that is blocked")
	}
	if code, out := e.run("--clear-block"); code != 0 {
		t.Fatalf("clear-block: exit %d\n%s", code, out)
	}
	if strings.Count(e.read("state/failed"), updNew) != 1 {
		t.Errorf("failed = %q: recorded twice, or not at all", e.read("state/failed"))
	}
	if code, _ := e.run(); code != 0 {
		t.Fatal("the run after clearing")
	}
	e.wantStatus("skipped", "failed here before")
	if e.count(`^compose (create|start)`) != 0 {
		t.Error("the release that failed was tried again")
	}
	// The order, in the script: the failure is recorded (durably, through a file of its own that is
	// moved into place) before the record of the update is removed, in the revert and in the clearing.
	script := readRepoFile(t, "deploy/taskto-update.sh")
	revert := script[strings.Index(script, "if revert; then"):]
	if i, j := strings.Index(revert, `record_failed "$to"`), strings.Index(revert, `rm -f "$TX"`); i < 0 || j < i {
		t.Error("the revert removes the record before it has recorded the failure")
	}
	rf := script[strings.Index(script, "record_failed() {"):]
	rf = rf[:strings.Index(rf, "\n}\n")]
	for _, want := range []string{`>"$f.new"`, "sync", `mv -fT "$f.new" "$f"`} {
		if !strings.Contains(rf, want) {
			t.Errorf("record_failed doesn't have %q", want)
		}
	}
}

// The data volume is the one the running server has mounted, not a name assumed.
func TestUpdateUsesTheServersOwnVolume(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/volname", "acme_data")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("updated", "")
	if e.count(`^volume inspect acme_data --format`) != 1 || e.count(`^volume inspect acme_data @`) != 1 || e.count(`taskto_data`) != 0 {
		t.Errorf("the server's own volume wasn't the one looked at\n%s", strings.Join(e.calls(), "\n"))
	}
	e.wantNeverRestores()
}

// An update killed between putting the old version back and writing what it did is found by the
// next run, which is blocked; once a person clears it, the release that failed isn't tried again.
func TestClearBlock(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	if code, out := e.run("--clear-block"); code != 0 || !strings.Contains(out, "nothing to clear") {
		t.Fatalf("nothing to clear: exit %d\n%s", code, out)
	}
	for _, args := range [][]string{{"--clear-block", "--dry-run"}, {"--clear-block", "--to", updNew}} {
		if code, _ := e.run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}

	e.write("fake/health-bad", updNew+"\n")
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	e.wantStatus("stopped", "")
	// It refuses while the server isn't up as the version in .env (here, the stack is stopped).
	if code, out := e.run("--clear-block"); code != 1 || !strings.Contains(out, "refusing: the server isn't healthy as "+updNew) {
		t.Fatalf("clear-block on a stopped stack: exit %d\n%s", code, out)
	}
	// A person restores the backup with the old version, by hand: the files and the server are the old.
	e.setVersion(updOld)
	e.setRunning(updOld)
	if code, out := e.run("--clear-block"); code != 1 || !strings.Contains(out, "refusing: "+e.install+" says "+updNew) {
		t.Fatalf("clear-block with the two version files apart: exit %d\n%s", code, out)
	}
	e.setInstallVersion(updOld)
	if code, out := e.run("--clear-block"); code != 0 {
		t.Fatalf("clear-block: exit %d\n%s", code, out)
	}
	if e.exists("state/transaction") || e.exists("state/blocked") {
		t.Error("the record or the marker is left")
	}
	e.wantStatus("cleared", "")
	// The release that failed isn't tried again by itself.
	ups := e.count(`^compose start`)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("the next run: exit %d", code)
	}
	e.wantStatus("skipped", "failed here before")
	if e.count(`^compose start`) != ups {
		t.Error("the next run tried the failed release again")
	}

	// A person who finishes the update by hand clears it too, and the release isn't "failed".
	e = newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/health-bad", updNew+"\n")
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	_ = os.Remove(filepath.Join(e.fake, "health-bad"))
	e.setRunning(updNew)
	if code, out := e.run("--clear-block"); code != 0 {
		t.Fatalf("clear-block: exit %d\n%s", code, out)
	}
	if strings.Contains(e.read("state/failed"), updNew) {
		t.Error("the version that is running now is recorded as failed")
	}
	if code, _ := e.run(); code != 0 {
		t.Fatal("the run after")
	}
	e.wantStatus("up-to-date", "")
}

// Pruning is for after a success, and never touches this run's archive: an older one named
// or dated in the future, with the least to keep, doesn't cost the update its way back, and a
// stopped update keeps every archive.
func TestUpdatePruneNeverTakesTheArchiveAStopNeeds(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.extra = []string{"TASKTO_UPDATE_KEEP=1"}
	future := "before-update-from-v0.1.0-to-v0.2.0-29990101-000000.tar.gz"
	e.write("backups/"+future, "FUTURE")
	e.write("fake/health-bad", updNew+"\n")
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d", code)
	}
	st := e.wantStatus("stopped", "")
	if b, _ := st["backup"].(string); !e.exists(strings.TrimPrefix(b, e.dir+"/")) || !e.exists("backups/"+future) {
		t.Error("an archive is gone after a failed update: nothing is pruned before a success")
	}
	// After a success, KEEP=1 is this run's archive and no other of the updater's.
	e2 := newUpdEnv(t)
	e2.release(updNew, "", false, false)
	e2.extra = []string{"TASKTO_UPDATE_KEEP=1"}
	e2.write("backups/before-update-from-v0.0.1-to-v0.0.2-20200101-000000.tar.gz", "old")
	if code, _ := e2.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, _ := e2.status()["backup"].(string)
	if !e2.exists(strings.TrimPrefix(b, e2.dir+"/")) {
		t.Error("this run's archive was pruned")
	}
	if e2.exists("backups/before-update-from-v0.0.1-to-v0.0.2-20200101-000000.tar.gz") {
		t.Error("an older archive is kept past KEEP")
	}
}

// Every page of the releases is read, since GitHub lists them by date and not by version: a
// manual-upgrade release can be on a page after the newest ones, and a page that can't be read
// stops the update.
func TestUpdateReadsEveryPageOfReleases(t *testing.T) {
	setup := func() *updEnv {
		e := newUpdEnv(t)
		e.installAt("v1.0.0")
		e.extra = []string{"TASKTO_UPDATE_PER_PAGE=2"}
		// Page 1: the newest by version, and an old one. Page 2: a release between, which needs
		// a person, and the release this install is on.
		e.release("v3.0.0", "", false, false)
		e.release("v0.9.0", "", false, false)
		e.release("v2.0.0", "Needs a person.\n\nmanual-upgrade: true\n", false, false)
		e.release("v1.0.0", "", false, false)
		return e
	}
	e := setup()
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("skipped", "v2.0.0 needs manual steps")
	if e.count(`^pull|^compose create|^compose start|^compose exec`) != 0 {
		t.Error("something was pulled, backed up or started")
	}
	e.gh.mu.Lock()
	var asked []string
	for _, r := range e.gh.requests {
		if strings.Contains(r, "/releases?") {
			asked = append(asked, r)
		}
	}
	e.gh.mu.Unlock()
	if len(asked) != 3 || !strings.Contains(asked[1], "&page=2") || !strings.Contains(asked[2], "&page=3") {
		t.Errorf("the pages asked for: %v, want 1, 2 and the empty 3", asked)
	}

	// A page after the first that can't be read: nothing is installed.
	e = setup()
	e.gh.failPages = map[int]bool{2: true}
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("skipped", "didn't answer (page 2")
	if e.count(`^pull|^compose create|^compose start|^compose exec`) != 0 {
		t.Error("something was pulled, backed up or started")
	}

	// The same list without the marker is taken: the newest by version.
	e = setup()
	e.gh.releases[2]["body"] = ""
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if st := e.wantStatus("updated", ""); st["to"] != "v3.0.0" {
		t.Errorf("updated to %v", st["to"])
	}
}

func (e *updEnv) traefik() {
	e.write("etc/install.env", e.read("etc/install.env")+"TASKTO_MODE='traefik'\nTASKTO_LAN_IP='10.0.0.5'\n")
	e.write("srv/.env", strings.Replace(e.read("srv/.env"), "cli,https", "cli", 1))
}

// In traefik mode the public address, as a visitor reaches it, is checked as well as the LAN
// port: before the update (a broken proxy isn't the update's fault, so nothing is changed) and
// after it (a stop).
func TestUpdateChecksThePublicAddressInTraefikMode(t *testing.T) {
	e := newUpdEnv(t)
	e.traefik()
	e.release(updNew, "", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("updated", "")
	b, _ := os.ReadFile(filepath.Join(e.fake, "curl-calls"))
	calls := string(b)
	if strings.Count(calls, "http://10.0.0.5/healthz") < 2 || strings.Count(calls, "https://tasks.example.com/healthz") < 2 || strings.Contains(calls, "--resolve") {
		t.Errorf("the LAN port and the public address weren't each checked before and after: %s", calls)
	}
	if e.count(`force-recreate`) != 0 {
		t.Error("Caddy was recreated though it isn't in use")
	}

	// Broken before: refused, with nothing changed.
	e = newUpdEnv(t)
	e.traefik()
	e.release(updNew, "", false, false)
	e.write("fake/proxy-version-down", "")
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("failed", "the public address doesn't answer as "+updOld+" before the update")
	e.wantUntouched()

	// A healthy LAN port, and a public address that is stale or failing after the start: the
	// new server ran, so the stack is stopped.
	for name, setup := range map[string]func(e *updEnv){
		"a stale public address": func(e *updEnv) { e.write("fake/proxy-version", updOld) },
		"a stale LAN port":       func(e *updEnv) { e.write("fake/lan-version", updOld) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.traefik()
			e.release(updNew, "", false, false)
			setup(e)
			if code, _ := e.run(); code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
			e.wantStatus("stopped", "isn't healthy as "+updNew)
			if e.count(`^compose stop`) != 1 {
				t.Error("the stack wasn't stopped")
			}
		})
	}
}

// A changed Caddyfile recreates Caddy after the switch, and again when the old version is put
// back.
func TestUpdateRecreatesCaddyForAChangedCaddyfile(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.commitFile(updNew, "deploy/Caddyfile", "caddy changed\n")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("updated", "")
	if e.count(`^compose create .*--no-deps --force-recreate caddy @`+updNew+`$`) != 1 {
		t.Errorf("Caddy wasn't recreated\n%s", strings.Join(e.calls(), "\n"))
	}
	// Caddy's other image is pulled, and its image is the one the staged files name.
	if e.index(`^pull --quiet caddy:2.11.9-alpine@sha256:`) < 0 || e.index(`^pull --quiet caddy:`) > e.index(`^compose exec -T taskto taskto backup`) {
		t.Errorf("Caddy's image wasn't pulled before the backup\n%s", strings.Join(e.calls(), "\n"))
	}

	// Put back before the new server started: the old Caddyfile is back, and Caddy recreated again.
	e = newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.commitFile(updNew, "deploy/Caddyfile", "caddy changed\n")
	e.failCalls(`^compose start .* @` + updNew + `$`)
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("rolled-back", "")
	if got := e.read("srv/deploy/Caddyfile"); got != "caddy\n" {
		t.Errorf("Caddyfile = %q", got)
	}
	if e.count(`^compose create .*--force-recreate caddy @`+updOld+`$`) != 1 {
		t.Errorf("Caddy wasn't recreated after the revert\n%s", strings.Join(e.calls(), "\n"))
	}
}

// Every outcome but up-to-date, skipped and a dry run is POSTed to the URL in the notify file,
// as JSON; the URL is never in a command line, the journal or the status; and a notification
// that fails never changes the outcome.
func TestUpdateNotifies(t *testing.T) {
	const secret = "SECRET-TOKEN-123"
	notify := func(e *updEnv) { e.write("etc/update-notify-url", e.server.URL+"/notify/"+secret+"\n") }
	posts := func(e *updEnv) []notification {
		e.gh.mu.Lock()
		defer e.gh.mu.Unlock()
		return append([]notification(nil), e.gh.notifications...)
	}

	// A success, and what the body is.
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	notify(e)
	code, out := e.run()
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	st := e.wantStatus("updated", "")
	got := posts(e)
	if len(got) != 1 || got[0].Path != "/notify/"+secret || got[0].ContentType != "application/json" {
		t.Fatalf("notifications = %+v", got)
	}
	b := got[0].Body
	if b["result"] != "updated" || b["from"] != updOld || b["to"] != updNew || b["backup"] != st["backup"] || b["reason"] != "" || !strings.Contains(fmt.Sprint(b["text"]), "updated, "+updOld+" to "+updNew) {
		t.Errorf("the body = %v", b)
	}
	// The URL is read from the file and handed to curl on its standard input.
	curl, _ := os.ReadFile(filepath.Join(e.fake, "curl-calls"))
	if strings.Contains(string(curl), secret) || strings.Contains(out, secret) || strings.Contains(e.read("state/status.json"), secret) {
		t.Error("the notification's URL is in curl's arguments, the journal or the status")
	}
	if !strings.Contains(string(curl), "-K -") {
		t.Errorf("curl wasn't given its settings on its standard input: %s", curl)
	}

	// A stop, a failed check, a revert and a blocked run are reported; their reasons come with them.
	e = newUpdEnv(t)
	e.release(updNew, "", false, false)
	notify(e)
	e.write("fake/health-bad", updNew+"\n")
	if code, _ := e.run(); code != 2 {
		t.Fatalf("exit %d", code)
	}
	got = posts(e)
	if len(got) != 1 || got[0].Body["result"] != "stopped" || !strings.Contains(fmt.Sprint(got[0].Body["reason"]), "restore the backup") || got[0].Body["backup"] == "" {
		t.Fatalf("notifications = %+v", got)
	}
	if code, _ := e.run(); code != 1 { // blocked, every night until a person clears it
		t.Fatalf("exit %d", code)
	}
	if got = posts(e); len(got) != 2 || got[1].Body["result"] != "blocked" {
		t.Errorf("the blocked run wasn't reported: %+v", got)
	}

	for name, tc := range map[string]struct {
		setup  func(e *updEnv)
		result string
	}{
		"a refused update":  {func(e *updEnv) { e.write("fake/no-rules", "") }, "failed"},
		"a revert":          {func(e *updEnv) { e.failCalls(`^compose start .* @` + updNew + `$`) }, "rolled-back"},
		"a failed revert":   {func(e *updEnv) { e.failCalls(`^compose start .* @`+updNew+`$`, `^compose start .* @`+updOld+`$`) }, "rollback-failed"},
		"an interrupted":    {func(e *updEnv) { e.write("state/transaction", `{"from":"v0.2.0","to":"v0.3.0","phase":"starting"}`) }, "interrupted"},
		"a version on file": {func(e *updEnv) { e.setVersion(updNew) }, "blocked"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			notify(e)
			tc.setup(e)
			if code, _ := e.run(); code == 0 {
				t.Fatal("exit 0")
			}
			if got := posts(e); len(got) != 1 || got[0].Body["result"] != tc.result {
				t.Errorf("notifications = %+v, want one of %s", got, tc.result)
			}
		})
	}

	// None for an outcome that is nothing to act on, a dry run or a clearing.
	for name, run := range map[string]func(e *updEnv){
		"up to date": func(e *updEnv) { e.run() },
		"a hold":     func(e *updEnv) { e.write("etc/update.hold", ""); e.run() },
		"a dry run":  func(e *updEnv) { e.run("--dry-run") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			if name != "up to date" {
				e.release(updNew, "", false, false)
			} else {
				e.release(updOld, "", false, false)
			}
			notify(e)
			run(e)
			if got := posts(e); len(got) != 0 {
				t.Errorf("notifications = %+v, want none", got)
			}
		})
	}

	// A notification that fails never changes the outcome: an error from the server, nothing
	// listening, and a file that isn't an address.
	for name, setup := range map[string]func(e *updEnv){
		"the server answers 500": func(e *updEnv) { notify(e); e.gh.notifyStatus = 500 },
		"nothing listens":        func(e *updEnv) { e.write("etc/update-notify-url", "http://127.0.0.1:1/notify/"+secret+"\n") },
		"not an address":         func(e *updEnv) { e.write("etc/update-notify-url", "not a url "+secret+"\n") },
		"an address with a quote": func(e *updEnv) {
			e.write("etc/update-notify-url", "http://127.0.0.1:1/\"\nrequest = \"DELETE\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			setup(e)
			code, out := e.run()
			if code != 0 {
				t.Fatalf("exit %d, want 0: a notification failed\n%s", code, out)
			}
			e.wantStatus("updated", "")
			if strings.Contains(out, secret) {
				t.Error("the URL is in the journal")
			}
		})
	}
}
