package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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

// shaOf is the commit a ref is: a full SHA is itself, a tag is a made-up one.
func shaOf(ref string) string {
	if regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(ref) {
		return ref
	}
	sum := sha256.Sum256([]byte("commit of " + ref))
	return hex.EncodeToString(sum[:20])
}

// fakeGitHub serves the few GitHub routes the updater reads, the files of each commit, and the
// /healthz of the server the fake docker "started", and of the proxy in front of it.
type fakeGitHub struct {
	mu        sync.Mutex
	fakeDir   string
	releases  []map[string]any
	runs      []map[string]any
	compare   map[string]string            // head -> status
	files     map[string]map[string]string // by commit SHA
	requests  []string
	failPages map[int]bool // pages of the releases that answer 500

	notifications []notification // what was POSTed to /notify/...
	notifyStatus  int            // the status /notify/... answers with (default 200)
}

type notification struct {
	Path, ContentType string
	Body              map[string]any
}

func (g *fakeGitHub) health(w http.ResponseWriter, file string) {
	b, err := os.ReadFile(filepath.Join(g.fakeDir, "running"))
	if p, perr := os.ReadFile(filepath.Join(g.fakeDir, file)); file != "running" && perr == nil {
		b, err = p, nil
	}
	bad, _ := os.ReadFile(filepath.Join(g.fakeDir, "health-bad"))
	if _, down := os.Stat(filepath.Join(g.fakeDir, file+"-down")); down == nil {
		http.Error(w, `{"status":"unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	if err != nil || strings.Contains(string(bad), strings.TrimSpace(string(b))) {
		http.Error(w, `{"status":"unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, `{"status":"ok","version":%q,"database":"ok","agent":"ready"}`, strings.TrimSpace(string(b)))
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, r.URL.RequestURI())
	const repo = "/repos/taskto-ai/taskto/"
	path := r.URL.Path
	switch {
	case path == "/healthz":
		g.health(w, "running")
	case path == "/proxy/healthz":
		g.health(w, "proxy-version")
	case path == "/lan/healthz":
		g.health(w, "lan-version")
	case strings.HasPrefix(path, "/notify/"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.notifications = append(g.notifications, notification{Path: path, ContentType: r.Header.Get("Content-Type"), Body: body})
		if g.notifyStatus != 0 {
			http.Error(w, "no", g.notifyStatus)
		}
	case path == repo+"releases":
		per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if per < 1 || page < 1 {
			http.Error(w, "bad paging", http.StatusBadRequest)
			return
		}
		if g.failPages[page] {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		lo, hi := (page-1)*per, page*per
		if lo > len(g.releases) {
			lo = len(g.releases)
		}
		if hi > len(g.releases) {
			hi = len(g.releases)
		}
		_ = json.NewEncoder(w).Encode(g.releases[lo:hi])
	case path == repo+"actions/workflows/ci.yml/runs":
		var runs []map[string]any
		for _, run := range g.runs {
			if r.URL.Query().Get("status") == "success" && run["conclusion"] != "success" {
				continue
			}
			runs = append(runs, run)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": runs})
	case strings.HasPrefix(path, repo+"commits/"):
		sha := shaOf(strings.TrimPrefix(path, repo+"commits/"))
		if _, ok := g.files[sha]; !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": sha})
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

// needTools skips the test when a program it runs isn't on PATH, and fails it on CI, whose
// runner has them: a missing program is never taken for the result of what it should have run.
func needTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s is needed to test the updater, and CI's runner has it", tool)
			}
			t.Skipf("no %s", tool)
		}
	}
}

func newUpdEnv(t *testing.T) *updEnv { return newUpdEnvIn(t, t.TempDir()) }

// newUpdEnvIn is newUpdEnv with everything under dir.
func newUpdEnvIn(t *testing.T, dir string) *updEnv {
	t.Helper()
	needTools(t, "bash", "curl", "jq", "flock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
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
	// curl, with the proxy's public address answered by the fake server: what it was asked for
	// is in $FAKE_DIR/curl-calls.
	e.write("bin/curl", `#!/bin/bash
echo "$*" >>"$FAKE_DIR/curl-calls"
args=()
skip=0
for a in "$@"; do
	if [ "$skip" = 1 ]; then skip=0; continue; fi
	case $a in
	--resolve) skip=1; continue ;;
	https://tasks.example.com/healthz) a=$FAKE_PROXY_HEALTH ;;
	http://10.0.0.5/healthz) a=$FAKE_LAN_HEALTH ;;
	esac
	args+=("$a")
done
exec /usr/bin/env curl "${args[@]}"
`)
	for _, f := range []string{"bin/docker", "bin/systemctl", "bin/curl"} {
		if err := os.Chmod(filepath.Join(dir, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	e.gh = &fakeGitHub{fakeDir: e.fake, compare: map[string]string{}, files: map[string]map[string]string{}}
	e.server = httptest.NewServer(e.gh)
	t.Cleanup(e.server.Close)

	// An install on updOld, as deploy/INSTALL.md leaves one.
	e.write("srv/compose.yaml", "# compose "+updOld+"\n# TASKTO_IMAGE_PREFIX\n")
	e.write("srv/deploy/Caddyfile", "caddy\n")
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

// firewallScript is the worker's firewall script of every version here (a version that changes
// it is a manual upgrade); its check fails while $FAKE_DIR/no-rules exists.
const firewallScript = "#!/bin/sh\n# firewall\n[ \"$1\" != check ] || [ ! -e \"$FAKE_DIR/no-rules\" ]\n"

func (e *updEnv) writeFirewall(v string) {
	e.write("taskto-worker-firewall", firewallScript+"# "+v+"\n")
	if err := os.Chmod(e.firewall, 0o755); err != nil {
		e.t.Fatal(err)
	}
}

// publish puts a ref's commit on the fake raw server, and its images' revision label on the
// fake docker. The safety script is the real one.
func (e *updEnv) publish(ref string) {
	e.gh.files[shaOf(ref)] = map[string]string{
		"compose.yaml":              "# compose " + ref + "\n# TASKTO_IMAGE_PREFIX\n",
		"deploy/Caddyfile":          "caddy\n",
		"deploy/worker-firewall.sh": firewallScript + "# " + updOld + "\n",
		"deploy/safety-check.sh":    readRepoFile(e.t, "deploy/safety-check.sh"),
	}
	e.write("fake/revision-"+ref, shaOf(ref))
}

// file of the commit of a ref, to change it.
func (e *updEnv) commitFile(ref, name, content string) {
	e.gh.files[shaOf(ref)][name] = content
}

// mutate makes a version's running containers break a safety rule.
func (e *updEnv) mutate(version string, kinds ...string) {
	var b strings.Builder
	for _, k := range kinds {
		b.WriteString(version + " " + k + "\n")
	}
	e.write("fake/mutate", b.String())
}

// setRunning is the version the fake server answers as.
func (e *updEnv) setRunning(v string) { e.write("fake/running", v+"\n") }

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
		"CURL=" + filepath.Join(e.dir, "bin/curl"), "FAKE_PROXY_HEALTH=" + e.server.URL + "/proxy/healthz", "FAKE_LAN_HEALTH=" + e.server.URL + "/lan/healthz",
		"TASKTO_UPDATE_NOTIFY_FILE=" + filepath.Join(e.etc, "update-notify-url"),
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
		"srv/compose.yaml": "# compose " + updOld + "\n# TASKTO_IMAGE_PREFIX\n", "srv/deploy/Caddyfile": "caddy\n",
		"taskto-worker-firewall": firewallScript + "# " + updOld + "\n",
	} {
		if got := e.read(name); got != want {
			e.t.Errorf("%s = %q, want the old one %q", name, got, want)
		}
	}
	if e.exists("state/transaction") {
		e.t.Error("an update's record is left")
	}
	if !strings.Contains(e.read("srv/.env"), "TASKTO_VERSION="+updOld+"\n") || !strings.Contains(e.read("etc/install.env"), "TASKTO_VERSION='"+updOld+"'") {
		e.t.Error("a version file isn't the old one")
	}
	if left, _ := os.ReadDir(e.backups); len(left) != 0 {
		e.t.Errorf("%d backups made, want none", len(left))
	}
	for _, never := range []string{`^compose exec`, `^compose create`, `^compose start`, `^update `, `^rm `, `^compose down`, `^compose stop`, `^volume rm`, `^compose run`, `^run `, `^tag `, `^systemctl restart`} {
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
	if got := e.read("srv/deploy/Caddyfile"); got != "caddy\n" {
		t.Errorf("Caddyfile = %q", got)
	}
	if got, want := e.read("srv/.env"), "# Written by deploy/INSTALL.md (step 6)\nCOMPOSE_PROFILES=cli,https\nTASKTO_IMAGE_PREFIX=ghcr.io/taskto-ai/\nTASKTO_VERSION="+updNew+"\nTASKTO_DOMAIN=tasks.example.com\n"; got != want {
		t.Errorf(".env = %q, want %q", got, want)
	}
	if got := e.read("etc/install.env"); got != "TASKTO_MODE='caddy'\nTASKTO_REGISTRY='ghcr.io/taskto-ai'\nTASKTO_VERSION='"+updNew+"'\n" {
		t.Errorf("install.env = %q", got)
	}
	// The firewall is never touched by the updater, only checked.
	if got := e.read("taskto-worker-firewall"); got != firewallScript+"# "+updOld+"\n" {
		t.Errorf("firewall = %q", got)
	}
	if e.count(`^systemctl restart`) != 0 {
		t.Error("the firewall's unit was restarted")
	}
	if e.count(`^systemctl is-active`) < 2 {
		t.Error("the firewall wasn't checked before and after")
	}
	if e.exists("state/transaction") {
		t.Error("the update's record is left after a success")
	}
	// Every file came from the one commit, never by the tag; the images are the commit's.
	e.gh.mu.Lock()
	for _, r := range e.gh.requests {
		if strings.HasPrefix(r, "/raw/") && !strings.HasPrefix(r, "/raw/taskto-ai/taskto/"+shaOf(updNew)+"/") && !strings.HasPrefix(r, "/raw/taskto-ai/taskto/"+shaOf(updOld)+"/") {
			t.Errorf("a file was fetched by %s, not by commit", r)
		}
	}
	e.gh.mu.Unlock()
	if st["sha"] != shaOf(updNew) {
		t.Errorf("status sha = %v", st["sha"])
	}
	// The proxy was asked over HTTPS for its own domain, from this machine.
	if b, _ := os.ReadFile(filepath.Join(e.fake, "curl-calls")); !strings.Contains(string(b), "--resolve tasks.example.com:443:127.0.0.1 https://tasks.example.com/healthz") {
		t.Errorf("the proxy wasn't checked: %s", b)
	}
	// An unchanged Caddyfile leaves Caddy as it is.
	if e.count(`force-recreate`) != 0 {
		t.Error("Caddy was recreated for an unchanged Caddyfile")
	}
	// The old files are kept for a way back, the order is backup, then switch.
	if got := e.read("state/previous/.env"); !strings.Contains(got, "TASKTO_VERSION="+updOld) {
		t.Errorf("previous .env = %q", got)
	}
	backedUp, up := e.index(`^compose exec -T taskto taskto backup -`), e.index(`^compose start --wait --wait-timeout \d+ @`+updNew+`$`)
	if backedUp < 0 || up < 0 || backedUp > up {
		t.Errorf("backup at %d, up at %d: want the backup first", backedUp, up)
	}
	// The old images, as the running containers have them (by id), are kept under tags of
	// their own before anything changes.
	for _, n := range []string{"taskto", "taskto-cli-worker"} {
		svc := strings.TrimPrefix(n, "taskto-")
		if n == "taskto" {
			svc = "taskto"
		}
		if i := e.index(`^tag sha256:run-` + svc + `-` + updOld + ` taskto-update-rollback:` + n + ` @`); i < 0 || i > backedUp {
			t.Errorf("the running %s image wasn't kept before the backup (call %d, backup %d)\n%s", n, i, backedUp, strings.Join(e.calls(), "\n"))
		}
	}
	if e.count(`^compose stop|^compose down|^compose run|^volume (rm|create|prune)|^run |^update |^rm `) != 0 {
		t.Errorf("a command the updater never runs here was run\n%s", strings.Join(e.calls(), "\n"))
	}
	// Every named volume of the new settings was looked at, and none made.
	for _, v := range []string{"taskto_data", "taskto_cli-run", "taskto_cli-homes", "taskto_caddy-data", "taskto_caddy-config"} {
		if e.count(`^volume inspect `+v+`( |$)`) < 1 {
			t.Errorf("the volume %s wasn't looked at before the switch", v)
		}
	}
	// The containers were made, checked and only then started; one explicit list of Compose files.
	if made, started := e.index(`^compose create `), e.index(`^compose start `); made < 0 || started < made || e.count(`^compose up`) != 0 {
		t.Errorf("create at %d, start at %d, up %d: the containers are made before they start, never by up", made, started, e.count(`^compose up`))
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
	e.setRunning(updNew)
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
	if e.count(`^pull|^compose exec|^compose create|^compose start`) != 0 {
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
			func(e *updEnv) {
				e.commitFile(updNew, "compose.yaml", e.gh.files[shaOf(updNew)]["compose.yaml"]+"# ${TASKTO_NEEDS_NEW_SETTING:?}\n")
			},
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
		"another image of an enabled service that can't be pulled": {
			func(e *updEnv) { e.failCalls(`^pull --quiet caddy:`) },
			"can't pull caddy:",
		},
		"a file that isn't there": {
			func(e *updEnv) { delete(e.gh.files[shaOf(updNew)], "deploy/Caddyfile") },
			"can't fetch deploy/Caddyfile",
		},
		"images that aren't built from the commit": {
			func(e *updEnv) { e.write("fake/revision-"+updNew, strings.Repeat("f", 40)) },
			"isn't built from " + shaOf(updNew),
		},
		"images without a revision label": {
			func(e *updEnv) { _ = os.Remove(filepath.Join(e.fake, "revision-"+updNew)) },
			"revision label is 'none'",
		},
		"the worker's firewall rules aren't in": {
			func(e *updEnv) { e.write("fake/no-rules", "") },
			"firewall isn't in place",
		},
		"no data volume on the running server": {
			func(e *updEnv) { e.write("fake/nomount", "") },
			"can't tell the server's data volume",
		},
		"a Compose project whose data volume isn't the running server's": {
			func(e *updEnv) { e.write("fake/config-volname", "other_data") },
			"the /data of taskto is 'other_data', not 'taskto_data'",
		},
		// The volume's name in the file is kept, and the server mounts another: the effective
		// volume of the mount is what counts, not volumes.data.
		"a candidate that keeps volumes.data.name and mounts another volume on the server": {
			func(e *updEnv) {
				e.write("fake/config-edit", `.services.taskto.volumes[0].source = "replacement" | .volumes.replacement = {name: "replacement"}`)
			},
			"the /data of taskto is 'replacement', not 'taskto_data'",
		},
		"a candidate whose backup job mounts another volume at /data": {
			func(e *updEnv) {
				e.write("fake/config-edit", `.services.backup = {image: "ghcr.io/taskto-ai/taskto-backup:`+updNew+`", volumes: [{type: "volume", source: "replacement", target: "/data"}]} | .volumes.replacement = {name: "replacement"}`)
			},
			"the /data of backup is 'replacement'",
		},
		"a candidate that mounts a host directory at the server's /data": {
			func(e *updEnv) {
				e.write("fake/config-edit", `.services.taskto.volumes[0] = {type: "bind", source: "/srv/elsewhere", target: "/data"}`)
			},
			"the /data of taskto is 'a bind mount'",
		},
		"a compose override that swaps the worker's image": {
			func(e *updEnv) {
				e.write("fake/config-edit", `.services["cli-worker"].image = "evil.example/worker:1"`)
			},
			"run an image that wasn't checked: cli-worker uses evil.example/worker:1",
		},
		"a compose override that runs an older image of TaskTo's in another service": {
			func(e *updEnv) {
				e.write("fake/config-edit", `.services.extra = {image: "ghcr.io/taskto-ai/taskto:`+updOld+`"}`)
			},
			"run an image that wasn't checked: extra uses ghcr.io/taskto-ai/taskto:" + updOld,
		},
		"the image a running container has is gone from the machine": {
			func(e *updEnv) { e.write("fake/missing-images", "sha256:run-taskto-"+updOld+"\n") },
			"isn't on this machine any more",
		},
		"the public address doesn't answer before the update": {
			func(e *updEnv) { e.write("fake/proxy-version-down", "") },
			"the public address doesn't answer as " + updOld + " before the update",
		},
		"a public address that answers as another version before the update": {
			func(e *updEnv) { e.write("fake/proxy-version", "v0.0.1") },
			"the public address doesn't answer as " + updOld + " before the update",
		},
		"a tag that isn't a commit": {
			func(e *updEnv) { delete(e.gh.files, shaOf(updNew)) },
			"can't resolve " + updNew,
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

// Without the firewall's script there is nothing to hold the worker in: the update is refused.
func TestUpdateRefusesWithoutTheFirewallScript(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	if err := os.Remove(e.firewall); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("failed", "isn't installed")
	if e.count(`^pull|^compose exec|^compose create|^compose start`) != 0 {
		t.Error("something was pulled, backed up or started")
	}
}

// A version that changes the worker's firewall is for a person: the run is skipped, whole.
func TestUpdateChangedFirewallIsAManualUpgrade(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.commitFile(updNew, "deploy/worker-firewall.sh", firewallScript+"# a new rule\n")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("skipped", "the worker firewall changed in "+updNew+": update by hand")
	e.wantUntouched()
	if e.count(`^pull`) != 0 {
		t.Error("an image was pulled")
	}
	// --to doesn't apply it either; once a person has, it goes ahead.
	if code, _ := e.run("--to", updNew); code != 1 {
		t.Fatalf("--to: exit %d", code)
	}
	e.wantUntouched()
	e.write("taskto-worker-firewall", firewallScript+"# a new rule\n")
	if err := os.Chmod(e.firewall, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.run(); code != 0 {
		t.Fatalf("after the hand update: exit %d", code)
	}
	e.wantStatus("updated", "")
	if e.count(`^systemctl restart`) != 0 {
		t.Error("the updater restarted the firewall")
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
	// It makes no backup directory, and doesn't change the mode of one that is there.
	fresh := filepath.Join(e.dir, "not", "yet", "backups")
	e.extra = []string{"TASKTO_UPDATE_BACKUPS=" + fresh}
	if code, out := e.run("--dry-run"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "not")); err == nil {
		t.Error("a dry run made the backup directory")
	}
	if err := os.Chmod(e.backups, 0o755); err != nil {
		t.Fatal(err)
	}
	e.extra = nil
	if code, out := e.run("--dry-run"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if fi, _ := os.Stat(e.backups); fi.Mode().Perm() != 0o755 {
		t.Errorf("a dry run changed the backup directory's mode to %v", fi.Mode().Perm())
	}
	// It pulls: the three images, and the other image of the enabled services (Caddy).
	if e.count(`^pull`) != 4*3 || e.count(`^compose --project-directory \S+ config --quiet @`+updNew+`$`) != 3 {
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
	e.setRunning(a)
	e.publish(a)
	e.write("etc/install.env", e.read("etc/install.env")+"TASKTO_UPDATE_CHANNEL='main'\n")
	e.release(updNew, "a release the main channel doesn't look at\n\nmanual-upgrade: true\n", false, false)
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
	// Five of the updater's own, and what isn't its own: a runbook's by-hand backup, another
	// file, a symbolic link and a directory with its name, and a restic repository's directory.
	// The order is the time in the name, not the file's.
	now := time.Now()
	old := func(name string, age time.Duration) {
		p := filepath.Join(e.backups, name)
		e.write("backups/"+name, "old")
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 5; i++ {
		// The oldest by name is the newest by file time.
		old(fmt.Sprintf("before-update-from-v0.1.%d-to-v0.1.%d-2020010%d-000000.tar.gz", i, i+1, i), time.Duration(i)*24*time.Hour)
	}
	old("before-update-from-v0.1.0-20200101-030405.tar.gz", 100*24*time.Hour)
	old("notes.txt", 100*24*time.Hour)
	if err := os.Symlink("notes.txt", filepath.Join(e.backups, "before-update-from-v0.0.1-to-v0.0.2-20190101-000000.tar.gz")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.backups, "before-update-from-v0.0.3-to-v0.0.4-20190101-000000.tar.gz"), 0o755); err != nil {
		t.Fatal(err)
	}
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
	// The new one, and the two newest by name of the five (4 and 5); nothing else of the updater's.
	for _, want := range []string{"-to-v0.1.5-", "-to-v0.1.6-", "-to-v0.3.0-", "before-update-from-v0.1.0-20200101-030405.tar.gz", "notes.txt", "restic",
		"before-update-from-v0.0.1-to-v0.0.2-20190101-000000.tar.gz", "before-update-from-v0.0.3-to-v0.0.4-20190101-000000.tar.gz"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is gone: %s", want, got)
		}
	}
	for _, gone := range []string{"-to-v0.1.2-", "-to-v0.1.3-", "-to-v0.1.4-"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q is kept: %s", gone, got)
		}
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
	for _, never := range []string{"forget", "docker volume prune", "docker system prune", "volume rm", "systemctl restart"} {
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
	for _, want := range []string{"## Step 16: automatic updates", "taskto-update --dry-run", "taskto-update.timer", "TASKTO_UPDATE_CHANNEL", "update-notify-url", "--clear-block"} {
		if !strings.Contains(runbook, want) {
			t.Errorf("deploy/INSTALL.md doesn't have %q", want)
		}
	}
}

// The server that answers as another version than the .env says was changed outside the
// updater: nothing runs until a person has looked.
func TestUpdateBlocksWhenTheServerIsNotTheVersionOnRecord(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.setVersion(updNew) // the .env says the new one; the server still runs the old
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	st := e.wantStatus("blocked", "the server runs '"+updOld+"' but")
	if st["result"] == "up-to-date" || !e.exists("state/blocked") {
		t.Fatalf("status %v, blocked file %v", st, e.exists("state/blocked"))
	}
	if e.count(`.`) != 0 {
		t.Error("docker was called")
	}
	// It stays blocked, for --to too; a dry run reports it, and writes nothing.
	if code, out := e.run(); code != 1 || !strings.Contains(out, "blocked") {
		t.Fatalf("second run: exit %d\n%s", code, out)
	}
}

// tagOfLength is a vMAJOR.MINOR.PATCH of n characters (a patch of many digits).
func tagOfLength(n int) string { return "v1.0.1" + strings.Repeat("0", n-len("v1.0.1")) }

func TestUpdateOnlyTheCanonicalVersionGrammar(t *testing.T) {
	// Leading zeros, pre-releases and build metadata aren't releases, or --to values.
	for _, to := range []string{"v01.0.0", "v1.02.0", "v1.0.0-rc1", "v1.0.0+build", "1.0.0", "v1.0", "V1.0.0", "main", "abc123"} {
		e := newUpdEnv(t)
		if code, out := e.run("--to", to); code != 2 || !strings.Contains(out, "--to takes") {
			t.Errorf("--to %q: exit %d\n%s", to, code, out)
		}
	}
	// The longest tag is 100 characters, in the updater and in release.yml: one more isn't a release.
	if code, out := newUpdEnv(t).run("--to", tagOfLength(101)); code != 2 || !strings.Contains(out, "--to takes") {
		t.Errorf("--to a tag of 101 characters: exit %d\n%s", code, out)
	}
	if code, out := newUpdEnv(t).run("--to", tagOfLength(100)); code == 2 || strings.Contains(out, "--to takes") {
		t.Errorf("--to a tag of 100 characters: exit %d\n%s", code, out)
	}
	e := newUpdEnv(t)
	e.release(tagOfLength(101), "", false, false)
	for _, tag := range []string{"v01.0.0", "v0.4.0-rc1", "v0.4.0+build", "v0.4", "0.4.0", "v0.4.00"} {
		e.release(tag, "", false, false)
	}
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("up-to-date", "")
	e.wantUntouched()
}

// Versions compare as numbers, which no shell arithmetic or float would get right.
func TestUpdateComparesVersionsAsDecimalStrings(t *testing.T) {
	for name, tc := range map[string]struct{ installed, release string }{
		"1.10.0 is newer than 1.9.0":           {"v1.9.0", "v1.10.0"},
		"2.0.0 is newer than 1.99.99":          {"v1.99.99", "v2.0.0"},
		"a component past any integer":         {"v0.99999999999999999999.0", "v0.100000000000000000000.0"},
		"a component of a hundred digits":      {"v1." + strings.Repeat("9", 60) + ".0", "v1.1" + strings.Repeat("0", 60) + ".0"},
		"patch 10 is newer than patch 9":       {"v3.4.9", "v3.4.10"},
		"the newest of several, not the first": {"v1.9.0", "v1.10.0"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.setVersion(tc.installed)
			e.setInstallVersion(tc.installed)
			e.setRunning(tc.installed)
			e.publish(tc.installed)
			// Releases are listed by creation date: not in order of version.
			e.release(tc.release, "", false, false)
			e.release(tc.installed, "", false, false)
			if code, _ := e.run(); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if st := e.wantStatus("updated", ""); st["to"] != tc.release {
				t.Fatalf("status = %v, want %s", st, tc.release)
			}
		})
	}
	// And never the other way.
	e := newUpdEnv(t)
	e.setVersion("v1.10.0")
	e.setInstallVersion("v1.10.0")
	e.setRunning("v1.10.0")
	e.release("v1.9.0", "", false, false)
	e.release("v1.10.0", "", false, false)
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	e.wantStatus("up-to-date", "")
}

// The registry login is in a Docker config of the updater's own, which is gone after.
func TestUpdateLogsInWithAConfigOfItsOwn(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("etc/install.env", e.read("etc/install.env")+"TASKTO_REGISTRY_USER='puller'\n")
	e.write("no-token", "secret\n")
	if code, _ := e.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	cfgs := strings.Fields(e.read("fake/login-config"))
	if len(cfgs) != 1 || cfgs[0] == "unset" || strings.HasPrefix(cfgs[0], filepath.Join(e.dir, ".docker")) {
		t.Fatalf("login config = %v", cfgs)
	}
	for _, c := range strings.Fields(e.read("fake/pull-config")) {
		if c != cfgs[0] {
			t.Errorf("a pull used the config %q, not the login's", c)
		}
	}
	if _, err := os.Stat(cfgs[0]); err == nil {
		t.Error("the login's config is left")
	}
	if e.exists(".docker") {
		t.Error("the operator's Docker config was written")
	}
}

// workflowStep is the shell script of a step of a workflow, by the step's name.
func workflowStep(t *testing.T, workflow, name string) string {
	t.Helper()
	lines := strings.Split(readRepoFile(t, workflow), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "- name: "+name {
			continue
		}
		var script []string
		in := false
		for _, l := range lines[i+1:] {
			if strings.HasPrefix(l, "      - ") {
				break
			}
			switch {
			case strings.TrimSpace(l) == "run: |":
				in = true
			case in && strings.HasPrefix(l, "          "):
				script = append(script, strings.TrimPrefix(l, "          "))
			case in && strings.TrimSpace(l) == "":
				script = append(script, "")
			}
		}
		return strings.Join(script, "\n")
	}
	t.Fatalf("%s has no step %q", workflow, name)
	return ""
}

// Anything the updater does, the workflow that cuts a release makes possible: one commit,
// the tag made by the API's create-a-reference call at it (which can't take over a tag that
// exists), the notes complete before the release counts.
func TestReleaseWorkflowTagsTheCheckedCommitThroughTheAPI(t *testing.T) {
	wf := readRepoFile(t, ".github/workflows/release.yml")
	for _, want := range []string{
		`--method POST "repos/$GH_REPO/git/refs"`, // the API creates the reference only if there is none
		`-f "ref=refs/tags/$VERSION"`, `-f "sha=$GITHUB_SHA"`,
		`--verify-tag`,
		`manual_upgrade:`, `notes:`,
		`manual-upgrade: true`,
		`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`,
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("release.yml doesn't have %q", want)
		}
	}
	// No git push of the tag (which succeeds for a tag that exists at the same commit), no force.
	for _, never := range []string{"git push", "--target", "--force", "push -f"} {
		if strings.Contains(wf, never) {
			t.Errorf("release.yml has %q: the tag is made by the API, never pushed or forced", never)
		}
	}
	if i, j := strings.Index(wf, `git/refs`), strings.Index(wf, `gh release create`); i < 0 || j < i {
		t.Error("release.yml doesn't make the tag before it creates the release")
	}
	// The job's permissions: contents (the tag and the release) and actions (the images), and nothing broader.
	job := wf[strings.Index(wf, "  release:\n"):]
	perms := job[strings.Index(job, "    permissions:\n"):]
	perms = perms[:strings.Index(perms, "    env:")]
	var granted []string
	for _, l := range strings.Split(perms, "\n")[1:] {
		if l = strings.TrimSpace(l); l != "" {
			granted = append(granted, strings.Fields(l)[0]+" "+strings.Fields(l)[1])
		}
	}
	if strings.Join(granted, ",") != "contents: write,actions: write" || strings.Contains(wf, "write-all") {
		t.Errorf("the release job's permissions are %v, want contents: write and actions: write only", granted)
	}
	if !strings.Contains(wf, "\npermissions:\n  contents: read\n") {
		t.Error("the workflow's default permission isn't contents: read")
	}
	// An input reaches a shell only as an environment variable.
	for i, line := range strings.Split(wf, "\n") {
		if strings.Contains(line, "${{ inputs.") && !regexp.MustCompile(`^\s+[A-Z_]+: \$\{\{ inputs\.[a-z_]+ \}\}$`).MatchString(line) {
			t.Errorf("release.yml:%d uses an input outside an env: %s", i+1, line)
		}
	}
	// The tag's own commit is what the images are built from, and say so.
	if !strings.Contains(readRepoFile(t, ".github/scripts/images.sh"), `--label org.opencontainers.image.revision="$sha"`) {
		t.Error("images.sh doesn't label the images with their commit")
	}
	docs := readRepoFile(t, "docs/releasing.md")
	if !strings.Contains(docs, "POST\n   /repos/{owner}/{repo}/git/refs") || strings.Contains(docs, "git push origin") {
		t.Error("docs/releasing.md doesn't describe the tag as made through the API")
	}
}

// The tag step, run as written, with a gh that records what it is asked: it creates the
// reference through the API, at the commit and for the version, and it stops, with nothing
// else made, when the API refuses (the tag exists).
func TestReleaseWorkflowTagStepStopsWhenTheTagExists(t *testing.T) {
	needTools(t, "bash")
	step := workflowStep(t, ".github/workflows/release.yml", "Create the tag at the commit CI passed on")
	for _, tc := range []struct {
		name     string
		refuse   bool
		wantFail bool
	}{{"the tag is new", false, false}, {"the API says the reference exists", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			// A directory with a space in its name: the stub's log is quoted, whatever TMPDIR is.
			dir := filepath.Join(t.TempDir(), "a directory")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "gh-calls")
			stub := "#!/bin/sh\necho \"$*\" >>\"$GH_CALLS_LOG\"\nif [ -n \"$REFUSE\" ]; then echo 'HTTP 422: Reference already exists' >&2; exit 1; fi\n"
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-e", "-c", step)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "GH_CALLS_LOG="+log, "GH_REPO=o/r", "VERSION=v1.2.3", "GITHUB_SHA="+strings.Repeat("a", 40))
			if tc.refuse {
				cmd.Env = append(cmd.Env, "REFUSE=1")
			}
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantFail {
				t.Fatalf("err = %v, want failure %v\n%s", err, tc.wantFail, out)
			}
			b, _ := os.ReadFile(log)
			for _, want := range []string{"api --method POST repos/o/r/git/refs", "ref=refs/tags/v1.2.3", "sha=" + strings.Repeat("a", 40)} {
				if !strings.Contains(string(b), want) {
					t.Errorf("gh was called with %q, want %q", b, want)
				}
			}
			if strings.Count(string(b), "\n") != 1 {
				t.Errorf("gh was called more than once: %q", b)
			}
			if tc.wantFail && !strings.Contains(string(out), "::error::can't create the tag v1.2.3") {
				t.Errorf("the failure isn't reported: %s", out)
			}
		})
	}
}

// The version check of release.yml, run as it is written there, and the updater's grammar
// agree: vMAJOR.MINOR.PATCH, at most 100 characters, and the whole input is the version: a
// second line that isn't, whatever the first says, is refused.
func TestReleaseWorkflowAndUpdaterAgreeOnATag(t *testing.T) {
	needTools(t, "bash")
	var check string
	for _, line := range strings.Split(readRepoFile(t, ".github/workflows/release.yml"), "\n") {
		if strings.Contains(line, `[[ $VERSION =~ `) {
			check = strings.TrimSpace(line)
		}
	}
	if check == "" {
		t.Fatal("no version check in release.yml")
	}
	for tag, ok := range map[string]bool{
		"v1.2.3": true, tagOfLength(100): true, tagOfLength(101): false,
		"v01.2.3": false, "v1.2.3-rc1": false, "1.2.3": false, "v1.2": false,
		"v1.2.3\nv1.2.3": false, "v1.2.3\nx": false, "x\nv1.2.3": false, "v1.2.3\n": false, "v1.2.3\n\n": false, "": false,
	} {
		cmd := exec.Command("bash", "-c", `fail() { exit 1; }; `+check)
		cmd.Env = append(os.Environ(), "VERSION="+tag)
		err := cmd.Run()
		// Only the check's own refusal (a plain exit status) is a result; bash not starting is not.
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatalf("release.yml's check couldn't be run on %q: %v", tag, err)
		}
		if (err == nil) != ok {
			t.Errorf("release.yml on %q (%d characters): accepted %v, want %v", tag, len(tag), err == nil, ok)
		}
	}
}

// Step 12 of the runbook and the updater run one script, and the updater won't update with
// the rules unchecked.
func TestSafetyChecksAreOneScript(t *testing.T) {
	runbook := readRepoFile(t, "deploy/INSTALL.md")
	if !strings.Contains(runbook, "vm < deploy/safety-check.sh") {
		t.Error("step 12 doesn't run deploy/safety-check.sh")
	}
	if strings.Contains(runbook, "docker inspect --format '{{.Config.User}}'") {
		t.Error("step 12 still has the rules of its own")
	}
	script := readRepoFile(t, "deploy/taskto-update.sh")
	if !strings.Contains(script, "deploy/safety-check.sh") || strings.Contains(script, "CapDrop") || strings.Contains(script, "cap_add") {
		t.Error("the updater doesn't run deploy/safety-check.sh, or checks rules of its own")
	}
	// It runs it twice: on the new settings before anything starts, and live after.
	if !strings.Contains(script, `safety-check.sh" --static "$stage"`) {
		t.Error("the updater doesn't check the new settings statically before it starts anything")
	}
	check := readRepoFile(t, "deploy/safety-check.sh")
	for _, want := range []string{"65532:65532", `["ALL"]`, `no-new-privileges:true`, `"CHOWN","KILL","SETGID","SETUID"`, "TASKTO_CLI_WORKERS=container", "/data", "docker.sock", "taskto-worker", "perm /6000", "--static"} {
		if !strings.Contains(check, want) {
			t.Errorf("deploy/safety-check.sh doesn't check %q", want)
		}
	}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
