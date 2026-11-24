package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// deploy/taskto-update.sh, the host's updater (docs/install.md#automatic-updates), run for
// real against fakes: a GitHub API and raw-file server, and a `docker` that records its calls
// and fails on demand (testdata/fake-docker). The tests need bash, curl, jq and flock, which
// CI's runner has; elsewhere they skip.

const (
	updOld = "v0.2.0"
	updNew = "v0.3.0"
)

// fakeGitHub serves the few GitHub routes the updater reads, the files of each ref, and the
// /healthz of the server the fake docker "started".
type fakeGitHub struct {
	mu       sync.Mutex
	fakeDir  string
	releases []map[string]any
	runs     []map[string]any
	compare  map[string]string // head -> status
	files    map[string]map[string]string
	requests []string
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, r.URL.RequestURI())
	const repo = "/repos/taskto-ai/taskto/"
	path := r.URL.Path
	switch {
	case path == "/healthz":
		b, err := os.ReadFile(filepath.Join(g.fakeDir, "running"))
		bad, _ := os.ReadFile(filepath.Join(g.fakeDir, "health-bad"))
		if err != nil || strings.Contains(string(bad), strings.TrimSpace(string(b))) {
			http.Error(w, `{"status":"unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"status":"ok","version":%q,"database":"ok","agent":"ready"}`, strings.TrimSpace(string(b)))
	case path == repo+"releases":
		_ = json.NewEncoder(w).Encode(g.releases)
	case path == repo+"actions/workflows/ci.yml/runs":
		var runs []map[string]any
		for _, run := range g.runs {
			if r.URL.Query().Get("status") == "success" && run["conclusion"] != "success" {
				continue
			}
			runs = append(runs, run)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
	case strings.HasPrefix(path, repo+"compare/"):
		head := path[strings.LastIndex(path, "...")+3:]
		st, ok := g.compare[head]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"status":%q}`, st)
	case strings.HasPrefix(path, "/raw/taskto-ai/taskto/"):
		rest := strings.TrimPrefix(path, "/raw/taskto-ai/taskto/")
		ref, file, _ := strings.Cut(rest, "/")
		content, ok := g.files[ref][file]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, content)
	default:
		http.NotFound(w, r)
	}
}

type updEnv struct {
	t                                    *testing.T
	dir, srv, state, backups, fake, tmp  string
	etc, install, hold, firewall, script string
	gh                                   *fakeGitHub
	server                               *httptest.Server
	extra                                []string
}

func newUpdEnv(t *testing.T) *updEnv {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "jq", "flock"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s is needed to test the updater, and CI's runner has it", tool)
			}
			t.Skipf("no %s", tool)
		}
	}
	dir := t.TempDir()
	e := &updEnv{t: t, dir: dir}
	e.srv, e.state, e.backups, e.fake, e.tmp = e.sub("srv"), e.sub("state"), e.sub("backups"), e.sub("fake"), e.sub("tmp")
	e.etc = e.sub("etc")
	e.install, e.hold, e.firewall = filepath.Join(e.etc, "install.env"), filepath.Join(e.etc, "update.hold"), filepath.Join(dir, "taskto-worker-firewall")
	e.script, _ = filepath.Abs(filepath.Join("..", "..", "deploy", "taskto-update.sh"))
	e.sub("srv/deploy")
	e.sub("fake/volume")
	e.sub("fake/root")
	e.sub("bin")
	e.write("fake/volume/data", "x")

	fakeDocker, err := os.ReadFile(filepath.Join("testdata", "fake-docker"))
	if err != nil {
		t.Fatal(err)
	}
	e.write("bin/docker", string(fakeDocker))
	e.write("bin/systemctl", "#!/bin/sh\necho \"systemctl $*\" >>\"$FAKE_DIR/calls\"\n")
	for _, f := range []string{"bin/docker", "bin/systemctl"} {
		if err := os.Chmod(filepath.Join(dir, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	e.gh = &fakeGitHub{fakeDir: e.fake, compare: map[string]string{}, files: map[string]map[string]string{}}
	e.server = httptest.NewServer(e.gh)
	t.Cleanup(e.server.Close)

	// An install on updOld, as deploy/INSTALL.md leaves one.
	e.write("srv/compose.yaml", "# compose "+updOld+"\n# TASKTO_IMAGE_PREFIX\n")
	e.write("srv/deploy/Caddyfile", "caddy "+updOld+"\n")
	e.setVersion(updOld)
	e.write("srv/compose.override.yaml", "services:\n  taskto:\n    environment:\n      TASKTO_PROVIDER_EGRESS_PERSONAL: public\n")
	e.write("etc/install.env", "TASKTO_MODE='caddy'\nTASKTO_REGISTRY='ghcr.io/taskto-ai'\n")
	e.setInstallVersion(updOld)
	e.writeFirewall(updOld)
	e.publish(updOld)
	e.write("fake/running", updOld+"\n")
	return e
}

func (e *updEnv) sub(name string) string {
	p := filepath.Join(e.dir, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *updEnv) write(name, content string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, name), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *updEnv) read(name string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *updEnv) exists(name string) bool {
	_, err := os.Stat(filepath.Join(e.dir, name))
	return err == nil
}

// setVersion writes the install's .env (Compose's settings) for a version.
func (e *updEnv) setVersion(v string) {
	e.write("srv/.env", "# Written by deploy/INSTALL.md (step 6)\nCOMPOSE_PROFILES=cli,https\nTASKTO_IMAGE_PREFIX=ghcr.io/taskto-ai/\nTASKTO_VERSION="+v+"\nTASKTO_DOMAIN=tasks.example.com\n")
}

func (e *updEnv) setInstallVersion(v string) {
	b := e.read("etc/install.env")
	b = regexp.MustCompile(`(?m)^TASKTO_VERSION=.*\n`).ReplaceAllString(b, "")
	e.write("etc/install.env", b+"TASKTO_VERSION='"+v+"'\n")
}

func (e *updEnv) writeFirewall(v string) {
	e.write("taskto-worker-firewall", "#!/bin/sh\n# firewall "+v+"\n")
	if err := os.Chmod(e.firewall, 0o755); err != nil {
		e.t.Fatal(err)
	}
}

// publish puts a ref's files on the fake raw server.
func (e *updEnv) publish(ref string) {
	e.gh.files[ref] = map[string]string{
		"compose.yaml":              "# compose " + ref + "\n# TASKTO_IMAGE_PREFIX\n",
		"deploy/Caddyfile":          "caddy " + ref + "\n",
		"deploy/worker-firewall.sh": "#!/bin/sh\n# firewall " + ref + "\n",
	}
}

func (e *updEnv) release(tag, body string, draft, pre bool) {
	e.gh.releases = append(e.gh.releases, map[string]any{"tag_name": tag, "body": body, "draft": draft, "prerelease": pre})
	e.publish(tag)
}

// failCalls makes the fake docker fail every call whose line matches one of the patterns.
func (e *updEnv) failCalls(patterns ...string) {
	e.write("fake/fail", strings.Join(patterns, "\n")+"\n")
}

// unpublish makes a tag's images missing from the registry.
func (e *updEnv) unpublish(tag string) {
	e.failCalls(`^manifest inspect \S+:` + regexp.QuoteMeta(tag) + ` @`)
}

func (e *updEnv) command(args ...string) *exec.Cmd {
	cmd := exec.Command("bash", append([]string{e.script}, args...)...)
	cmd.Dir = e.dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + e.dir, "TMPDIR=" + e.tmp, "FAKE_DIR=" + e.fake,
		"DOCKER=" + filepath.Join(e.dir, "bin/docker"), "SYSTEMCTL=" + filepath.Join(e.dir, "bin/systemctl"),
		"TASKTO_UPDATE_ROOT=" + e.srv, "TASKTO_UPDATE_INSTALL_ENV=" + e.install, "TASKTO_UPDATE_HOLD=" + e.hold,
		"TASKTO_UPDATE_STATE=" + e.state, "TASKTO_UPDATE_BACKUPS=" + e.backups, "TASKTO_UPDATE_LOCK=" + filepath.Join(e.dir, "lock"),
		"TASKTO_UPDATE_FIREWALL=" + e.firewall, "TASKTO_UPDATE_TOKEN_FILE=" + filepath.Join(e.dir, "no-token"),
		"TASKTO_UPDATE_API=" + e.server.URL, "TASKTO_UPDATE_RAW=" + e.server.URL + "/raw", "TASKTO_UPDATE_HEALTH=" + e.server.URL + "/healthz",
		"TASKTO_UPDATE_HELPER=helper:test", "TASKTO_UPDATE_VERIFY_TIMEOUT=2", "TASKTO_UPDATE_POLL=1",
	}, e.extra...)
	return cmd
}

// run the updater; its exit status and what it printed.
func (e *updEnv) run(args ...string) (int, string) {
	e.t.Helper()
	out, err := e.command(args...).CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			e.t.Fatal(err)
		}
		code = ee.ExitCode()
	}
	e.t.Logf("taskto-update %v: exit %d\n%s", args, code, out)
	// Its staging directory is gone, whatever happened.
	if left, _ := os.ReadDir(e.tmp); len(left) != 0 {
		e.t.Errorf("the updater left %d files in its temporary directory", len(left))
	}
	return code, string(out)
}

func (e *updEnv) status() map[string]any {
	e.t.Helper()
	var st map[string]any
	if err := json.Unmarshal([]byte(e.read("state/status.json")), &st); err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *updEnv) calls() []string {
	b, _ := os.ReadFile(filepath.Join(e.fake, "calls"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// count is how many recorded calls match the pattern.
func (e *updEnv) count(pattern string) int {
	re := regexp.MustCompile(pattern)
	n := 0
	for _, c := range e.calls() {
		if re.MatchString(c) {
			n++
		}
	}
	return n
}

// index is the position of a call in the record, -1 when there is none.
func (e *updEnv) index(pattern string) int {
	re := regexp.MustCompile(pattern)
	for i, c := range e.calls() {
		if re.MatchString(c) {
			return i
		}
	}
	return -1
}

func (e *updEnv) requests() int {
	e.gh.mu.Lock()
	defer e.gh.mu.Unlock()
	return len(e.gh.requests)
}

func (e *updEnv) wantStatus(result, reasonPart string) map[string]any {
	e.t.Helper()
	st := e.status()
	if st["result"] != result || !strings.Contains(fmt.Sprint(st["reason"]), reasonPart) {
		e.t.Fatalf("status = %v, want result %q with a reason that has %q", st, result, reasonPart)
	}
	return st
}

// wantOld: the install is as it was: files, .env, firewall, no backup, no change made.
func (e *updEnv) wantUntouched() {
	e.t.Helper()
	for name, want := range map[string]string{
		"srv/compose.yaml": "# compose " + updOld + "\n# TASKTO_IMAGE_PREFIX\n", "srv/deploy/Caddyfile": "caddy " + updOld + "\n",
		"taskto-worker-firewall": "#!/bin/sh\n# firewall " + updOld + "\n",
	} {
		if got := e.read(name); got != want {
			e.t.Errorf("%s = %q, want the old one %q", name, got, want)
		}
	}
	if !strings.Contains(e.read("srv/.env"), "TASKTO_VERSION="+updOld+"\n") || !strings.Contains(e.read("etc/install.env"), "TASKTO_VERSION='"+updOld+"'") {
		e.t.Error("a version file isn't the old one")
	}
	if left, _ := os.ReadDir(e.backups); len(left) != 0 {
		e.t.Errorf("%d backups made, want none", len(left))
	}
	for _, never := range []string{`^compose exec`, `^compose up`, `^compose down`, `^volume rm`, `^compose run`} {
		if e.count(never) != 0 {
			e.t.Errorf("a call matching %s was made", never)
		}
	}
}

func TestUpdateNothingNewer(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updOld, "", false, false)
	// None of these is a release to install: a pre-release, a draft, tags that aren't vX.Y.Z.
	e.release("v0.4.0", "", false, true)
	e.release("v0.4.1", "", true, false)
	e.release("v0.4.2-rc1", "", false, false)
	e.release("nightly", "", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("up-to-date", "no release is newer")
	e.wantUntouched()
	if e.count(`^pull`) != 0 {
		t.Error("an image was pulled")
	}
}

func TestUpdateImagesNotPublishedYet(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.unpublish(updNew)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("skipped", "images aren't published")
	e.wantUntouched()

	// An older release that is published, and newer than this install, is the one to take.
	e.release("v0.2.5", "", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("updated", "")
	if st := e.status(); st["to"] != "v0.2.5" {
		t.Fatalf("updated to %v, want v0.2.5", st["to"])
	}
}

func TestUpdateSucceeds(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "Notes.\n", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	st := e.wantStatus("updated", "")
	if st["from"] != updOld || st["to"] != updNew {
		t.Fatalf("status = %v", st)
	}
	backup, _ := st["backup"].(string)
	if !strings.HasPrefix(backup, e.backups+"/before-update-from-v0.2.0-to-v0.3.0-") || !strings.HasSuffix(backup, ".tar.gz") {
		t.Fatalf("backup = %q", backup)
	}
	if got, err := os.ReadFile(backup); err != nil || string(got) != "BACKUP-OF-"+updOld+"\n" {
		t.Errorf("backup holds %q (%v): not the old server's", got, err)
	}
	if fi, _ := os.Stat(backup); fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v, want 600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(e.backups); fi.Mode().Perm() != 0o700 {
		t.Errorf("backups directory mode %v, want 700", fi.Mode().Perm())
	}
	digests, _ := st["digests"].(map[string]any)
	for _, name := range []string{"taskto", "taskto-cli-worker", "taskto-backup"} {
		if d, _ := digests[name].(string); !strings.HasPrefix(d, "ghcr.io/taskto-ai/"+name+":"+updNew+"@sha256:") {
			t.Errorf("digest of %s = %q", name, d)
		}
	}
	// The new files, the same settings with the new version, the runbook's inputs on the VM.
	if got := e.read("srv/compose.yaml"); got != "# compose "+updNew+"\n# TASKTO_IMAGE_PREFIX\n" {
		t.Errorf("compose.yaml = %q", got)
	}
	if got := e.read("srv/deploy/Caddyfile"); got != "caddy "+updNew+"\n" {
		t.Errorf("Caddyfile = %q", got)
	}
	if got, want := e.read("srv/.env"), "# Written by deploy/INSTALL.md (step 6)\nCOMPOSE_PROFILES=cli,https\nTASKTO_IMAGE_PREFIX=ghcr.io/taskto-ai/\nTASKTO_VERSION="+updNew+"\nTASKTO_DOMAIN=tasks.example.com\n"; got != want {
		t.Errorf(".env = %q, want %q", got, want)
	}
	if got := e.read("etc/install.env"); got != "TASKTO_MODE='caddy'\nTASKTO_REGISTRY='ghcr.io/taskto-ai'\nTASKTO_VERSION='"+updNew+"'\n" {
		t.Errorf("install.env = %q", got)
	}
	if got := e.read("taskto-worker-firewall"); got != "#!/bin/sh\n# firewall "+updNew+"\n" {
		t.Errorf("firewall = %q", got)
	}
	if e.count(`^systemctl restart taskto-worker-firewall`) != 1 {
		t.Error("the firewall wasn't applied once")
	}
	// The old files are kept for a way back, the order is backup, then switch.
	if got := e.read("state/previous/.env"); !strings.Contains(got, "TASKTO_VERSION="+updOld) {
		t.Errorf("previous .env = %q", got)
	}
	backedUp, up := e.index(`^compose exec -T taskto taskto backup -`), e.index(`^compose up -d --no-build --pull never --remove-orphans --wait .* @`+updNew+`$`)
	if backedUp < 0 || up < 0 || backedUp > up {
		t.Errorf("backup at %d, up at %d: want the backup first", backedUp, up)
	}
	if got := e.read("fake/running"); strings.TrimSpace(got) != updNew {
		t.Errorf("the server runs %q", got)
	}
	if last, _ := e.status()["last_change"].(map[string]any); last["result"] != "updated" {
		t.Errorf("last_change = %v", last)
	}
	// A run with nothing new keeps what the last change was.
	e.gh.releases = e.gh.releases[:0]
	e.release(updNew, "", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("second run exit %d", code)
	}
	st = e.wantStatus("up-to-date", "")
	if last, _ := st["last_change"].(map[string]any); last["to"] != updNew {
		t.Errorf("last_change after a quiet run = %v", last)
	}
}

func TestUpdateNeverDowngradesNorRepeats(t *testing.T) {
	e := newUpdEnv(t)
	e.setVersion(updNew)
	e.setInstallVersion(updNew)
	e.release(updOld, "", false, false)
	e.release(updNew, "", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("up-to-date", "")
	if code, out := e.run("--to", updOld); code != 1 || !strings.Contains(out, "downgrades are refused") {
		t.Fatalf("--to an older release: exit %d\n%s", code, out)
	}
	if code, out := e.run("--to", updNew); code != 1 || !strings.Contains(out, "already on") {
		t.Fatalf("--to this release: exit %d\n%s", code, out)
	}
	if e.count(`^pull|^compose exec|^compose up`) != 0 {
		t.Error("something was pulled, backed up or started")
	}
}

func TestUpdateHoldAndOff(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("etc/update.hold", "")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("skipped", "on hold")
	if e.requests() != 0 || e.count(`.`) != 0 {
		t.Errorf("a held run asked GitHub (%d requests) or docker", e.requests())
	}
	e.wantUntouched()

	if err := os.Remove(e.hold); err != nil {
		t.Fatal(err)
	}
	e.write("etc/install.env", e.read("etc/install.env")+"TASKTO_UPDATE_CHANNEL='off'\n")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("skipped", "are off")
	e.wantUntouched()

	// An operator's own --to goes ahead of both.
	e.write("etc/update.hold", "")
	if code, _ := e.run("--to", updNew); code != 0 {
		t.Fatal("--to didn't update")
	}
	e.wantStatus("updated", "")
}

func TestUpdateSkipsManualUpgrades(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "Needs a new disk.\n\nmanual-upgrade: true\n", false, false)
	e.release("v0.4.0", "Nothing special.\n", false, false)
	// The newer release is fine, but the one it passes needs a person.
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("skipped", updNew+" needs manual steps")
	e.wantUntouched()
	if e.count(`^pull`) != 0 {
		t.Error("an image was pulled for a release that needs manual steps")
	}
	// The person did them: --to goes ahead.
	if code, _ := e.run("--to", "v0.4.0"); code != 0 {
		t.Fatal("--to didn't update")
	}
	e.wantStatus("updated", "")
}

func TestUpdatePreflightFailuresTouchNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  func(e *updEnv)
		reason string
	}{
		"a Compose file that needs a setting this install doesn't have": {
			func(e *updEnv) { e.gh.files[updNew]["compose.yaml"] += "# ${TASKTO_NEEDS_NEW_SETTING:?}\n" },
			"doesn't work with this install's settings",
		},
		"not enough disk": {
			func(e *updEnv) { e.extra = []string{"TASKTO_UPDATE_IMAGES_MB=999999999999"} },
			"not enough free disk",
		},
		"an image that can't be pulled": {
			func(e *updEnv) { e.failCalls(`^pull .*taskto-cli-worker:` + regexp.QuoteMeta(updNew)) },
			"can't pull",
		},
		"a file that isn't there": {
			func(e *updEnv) { delete(e.gh.files[updNew], "deploy/Caddyfile") },
			"can't fetch deploy/Caddyfile",
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			tc.setup(e)
			if code, _ := e.run(); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			e.wantStatus("failed", tc.reason)
			e.wantUntouched()
			if e.exists("state/previous") {
				t.Error("the old files were set aside before the checks passed")
			}
			if strings.TrimSpace(e.read("fake/running")) != updOld {
				t.Error("the server was restarted")
			}
		})
	}
}

func TestUpdateRollsBackOnFailure(t *testing.T) {
	for name, setup := range map[string]func(e *updEnv){
		"the server isn't healthy":               func(e *updEnv) { e.write("fake/health-bad", updNew+"\n") },
		"docker compose up fails":                func(e *updEnv) { e.failCalls(`^compose up .* @` + updNew + `$`) },
		"the server runs with a safety rule off": func(e *updEnv) { e.write("fake/unsafe", updNew+"\n") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			setup(e)
			code, _ := e.run()
			if code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			st := e.wantStatus("rolled-back", "")
			if st["from"] != updOld || st["to"] != updNew {
				t.Errorf("status = %v", st)
			}
			// The old files, .env and firewall are back, and the old server runs.
			for name, want := range map[string]string{
				"srv/compose.yaml": "# compose " + updOld + "\n# TASKTO_IMAGE_PREFIX\n", "srv/deploy/Caddyfile": "caddy " + updOld + "\n",
				"taskto-worker-firewall": "#!/bin/sh\n# firewall " + updOld + "\n",
			} {
				if got := e.read(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			if !strings.Contains(e.read("srv/.env"), "TASKTO_VERSION="+updOld+"\n") || !strings.Contains(e.read("etc/install.env"), "TASKTO_VERSION='"+updOld+"'") {
				t.Error("a version file isn't the old one")
			}
			if strings.TrimSpace(e.read("fake/running")) != updOld {
				t.Errorf("the server runs %q", e.read("fake/running"))
			}
			// In order: down, the data moved aside (copied, then the volume removed), the backup
			// restored by the old version's image (its .env), and the old server started.
			down := e.index(`^compose down`)
			aside := e.index(`^run --rm -v taskto_data:/from -v taskto_data_rolled_back_\d+:/to helper:test cp -a /from/. /to/`)
			rm := e.index(`^volume rm taskto_data @`)
			restore := e.index(`^compose run --rm --no-deps -T taskto restore - @` + updOld + `$`)
			up := -1
			for i, c := range e.calls() {
				if strings.HasPrefix(c, "compose up ") && strings.HasSuffix(c, "@"+updOld) {
					up = i
				}
			}
			if down <= 0 || down >= aside || aside >= rm || rm >= restore || restore >= up {
				t.Errorf("rollback calls out of order: down %d, aside %d, volume rm %d, restore %d, up %d\n%s", down, aside, rm, restore, up, strings.Join(e.calls(), "\n"))
			}
			if got := e.read("fake/restored"); got != "BACKUP-OF-"+updOld+"\n" {
				t.Errorf("restored %q, want the backup of the old server", got)
			}
			if b, _ := st["backup"].(string); b == "" || !e.exists(strings.TrimPrefix(b, e.dir+"/")) {
				t.Error("the backup isn't kept")
			}

			// The release that failed isn't tried again every night.
			ups, backups := e.count(`^compose up`), e.count(`^compose exec`)
			if code, _ := e.run(); code != 0 {
				t.Fatalf("next run: exit %d", code)
			}
			e.wantStatus("skipped", "rolled back")
			if e.count(`^compose up`) != ups || e.count(`^compose exec`) != backups {
				t.Errorf("the next run tried the failed release again\n%s", strings.Join(e.calls(), "\n"))
			}
		})
	}
}

func TestUpdateRollbackFailureStopsAndStaysStopped(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	// Neither version answers: the update fails, and so does the way back.
	e.write("fake/health-bad", updNew+"\n"+updOld+"\n")
	code, out := e.run()
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	e.wantStatus("rollback-failed", "the rollback failed too")
	if !strings.Contains(out, "rollback-failed") {
		t.Error("the journal doesn't say so")
	}
	if n := e.count(`^compose up`); n != 2 {
		t.Errorf("%d starts: the update's and the rollback's, no more", n)
	}
	// The old files are in place, the data aside, the backup kept.
	if got := e.read("srv/compose.yaml"); !strings.Contains(got, updOld) {
		t.Errorf("compose.yaml = %q", got)
	}
	if !e.exists("state/blocked") {
		t.Fatal("nothing stops the next night's run")
	}
	calls := len(e.calls())
	code, out = e.run()
	if code != 1 || !strings.Contains(out, "blocked") || len(e.calls()) != calls {
		t.Fatalf("the next run: exit %d, calls %d -> %d\n%s", code, calls, len(e.calls()), out)
	}
	if e.wantStatus("rollback-failed", "")["to"] != updNew {
		t.Error("the blocked run overwrote the status")
	}
}

func TestUpdateRunsOneAtATime(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/block", "") // the first run waits in its first pull
	first := e.command()
	var firstOut strings.Builder
	first.Stdout, first.Stderr = &firstOut, &firstOut
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- first.Wait() }()
	defer func() { _ = os.Remove(filepath.Join(e.fake, "block")) }()
	for deadline := time.Now().Add(20 * time.Second); e.count(`^pull`) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first run never got to pulling")
		}
	}
	before := len(e.calls())
	out, err := e.command().CombinedOutput() // not run(): the first run's staging directory is in use
	if err != nil || !strings.Contains(string(out), "already running") || len(e.calls()) != before || e.exists("state/status.json") {
		t.Errorf("the second run: %v, calls %d -> %d\n%s", err, before, len(e.calls()), out)
	}
	if err := os.Remove(filepath.Join(e.fake, "block")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the first run: %v\n%s", err, firstOut.String())
	}
	e.wantStatus("updated", "")
}

func TestUpdateDryRunChangesNothing(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	code, out := e.run("--dry-run")
	if code != 0 || !strings.Contains(out, "result=dry-run") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	e.wantUntouched()
	if e.exists("state") && len(mustReadDir(t, e.state)) != 0 {
		t.Error("a dry run wrote to the state directory")
	}
	if e.count(`^pull`) != 3 || e.count(`^compose --project-directory \S+ config --quiet @`+updNew+`$`) != 1 {
		t.Errorf("a dry run checks the images and the Compose settings\n%s", strings.Join(e.calls(), "\n"))
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestUpdateMainChannelTakesAGreenDescendant(t *testing.T) {
	sha := func(c string) string { return strings.Repeat(c, 40) }
	a, red, diverged, unpublished, green := sha("a"), sha("b"), sha("c"), sha("d"), sha("e")
	e := newUpdEnv(t)
	e.setVersion(a)
	e.setInstallVersion(a)
	e.write("etc/install.env", e.read("etc/install.env")+"TASKTO_UPDATE_CHANNEL='main'\n")
	e.release(updNew, "a release the main channel doesn't look at", false, false)
	run := func(sha, conclusion string) map[string]any {
		return map[string]any{"head_sha": sha, "head_branch": "main", "conclusion": conclusion}
	}
	// Newest first: a commit whose CI failed, one off a side branch, one whose images aren't
	// published, the one to take, and the install's own.
	e.gh.runs = []map[string]any{run(red, "failure"), run(diverged, "success"), run(unpublished, "success"), run(green, "success"), run(a, "success")}
	e.gh.compare = map[string]string{red: "ahead", diverged: "diverged", unpublished: "ahead", green: "ahead", a: "identical"}
	for _, c := range []string{unpublished, green} {
		e.publish(c)
	}
	e.failCalls(`^manifest inspect \S+:` + unpublished + ` @`)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	st := e.wantStatus("updated", "")
	if st["to"] != green || st["channel"] != "main" {
		t.Fatalf("status = %v, want the green descendant %s", st, green)
	}
	if !strings.Contains(e.read("srv/.env"), "TASKTO_VERSION="+green+"\n") {
		t.Error(".env isn't on the commit")
	}
	if e.count(red) != 0 || e.count(updNew) != 0 || e.count(diverged) != 0 {
		t.Error("a commit that isn't green, isn't a descendant, or a release was looked at")
	}
	e.gh.mu.Lock()
	asked := strings.Join(e.gh.requests, "\n")
	e.gh.mu.Unlock()
	if !strings.Contains(asked, "status=success") {
		t.Error("the CI runs weren't asked for by success")
	}

	// Now on it: nothing newer.
	e.gh.runs = []map[string]any{run(green, "success"), run(a, "success")}
	e.gh.compare[green] = "identical"
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("up-to-date", "")
}

func TestUpdatePruneKeepsOnlyItsOwnLatest(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	now := time.Now()
	old := func(name string, age time.Duration) {
		p := filepath.Join(e.backups, name)
		e.write("backups/"+name, "old")
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	// Five of the updater's own, and what isn't its own: a runbook's by-hand backup, another
	// file, and a restic repository's directory.
	for i := 1; i <= 5; i++ {
		old(fmt.Sprintf("before-update-from-v0.1.%d-to-v0.1.%d-20260101-00000%d.tar.gz", i, i+1, i), time.Duration(10-i)*24*time.Hour)
	}
	old("before-update-from-v0.1.0-20260101-030405.tar.gz", 100*24*time.Hour)
	old("notes.txt", 100*24*time.Hour)
	if err := os.MkdirAll(filepath.Join(e.backups, "restic", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var left []string
	for _, f := range mustReadDir(t, e.backups) {
		left = append(left, f.Name())
	}
	got := strings.Join(left, " ")
	// The new one, and the two newest of the five (4 and 5); nothing else of the updater's.
	for _, want := range []string{"-to-v0.1.5-", "-to-v0.1.6-", "-to-v0.3.0-", "before-update-from-v0.1.0-20260101-030405.tar.gz", "notes.txt", "restic"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is gone: %s", want, got)
		}
	}
	for _, gone := range []string{"-to-v0.1.2-", "-to-v0.1.3-", "-to-v0.1.4-"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q is kept: %s", gone, got)
		}
	}
	if n := strings.Count(got, "-to-"); n != 3 {
		t.Errorf("%d of the updater's backups left, want 3: %s", n, got)
	}
}

// The updater is a host program, not the server's: nothing in the Compose file gives a
// container the Docker socket, and the units run it as a nightly one-shot that catches up.
func TestUpdaterIsOnTheHostOnly(t *testing.T) {
	if strings.Contains(readRepoFile(t, "compose.yaml"), "docker.sock") {
		t.Error("compose.yaml gives a container the Docker socket")
	}
	script := readRepoFile(t, "deploy/taskto-update.sh")
	if !strings.Contains(script, "\nset -euo pipefail\n") || !strings.Contains(script, `"$FLOCK" -n 9`) {
		t.Error("the updater doesn't stop on errors, or doesn't take its lock")
	}
	for _, never := range []string{"forget", "docker volume prune", "docker system prune", "--force-recreate"} {
		if strings.Contains(script, never) {
			t.Errorf("the updater has %q", never)
		}
	}
	timer := readRepoFile(t, "deploy/taskto-update.timer")
	for _, want := range []string{"Persistent=true", "RandomizedDelaySec=", "OnCalendar=*-*-*"} {
		if !strings.Contains(timer, want) {
			t.Errorf("taskto-update.timer doesn't have %q", want)
		}
	}
	service := readRepoFile(t, "deploy/taskto-update.service")
	for _, want := range []string{"Type=oneshot", "ExecStart=/usr/local/sbin/taskto-update"} {
		if !strings.Contains(service, want) {
			t.Errorf("taskto-update.service doesn't have %q", want)
		}
	}
	runbook := readRepoFile(t, "deploy/INSTALL.md")
	for _, want := range []string{"## Step 16: automatic updates", "taskto-update --dry-run", "taskto-update.timer", "TASKTO_UPDATE_CHANNEL"} {
		if !strings.Contains(runbook, want) {
			t.Errorf("deploy/INSTALL.md doesn't have %q", want)
		}
	}
}
