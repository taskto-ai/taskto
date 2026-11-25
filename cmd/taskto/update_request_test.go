package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/updatestatus"
)

// An operator's "Update now" (docs/install.md#update-now): the server puts a file in a directory
// of its own and a systemd path unit runs `taskto-update --requested`, which takes the file and
// does the nightly run, behind every gate of it. The directory is the server's to write, so
// everything in it is hostile.

const requestLine = "2026-11-24T12:00:00Z 0123456789abcdef0123456789abcdef\n"

// requestEnv is an install whose host has the request directory in place and a newer release.
func requestEnv(t *testing.T) (*updEnv, string) {
	t.Helper()
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	dir := e.sub("state/request")
	e.extra = []string{"TASKTO_UPDATE_REQUEST=" + dir, "TASKTO_UPDATE_REQUEST_MIN_INTERVAL=0"}
	return e, dir
}

func (e *updEnv) request(dir, content string) string {
	e.t.Helper()
	p := filepath.Join(dir, updatestatus.RequestFile)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *updEnv) wantNoRequestLeft(dir string) {
	e.t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, ent := range entries {
		e.t.Errorf("%s is left in the request directory", ent.Name())
	}
	if e.exists("state/request.taken") {
		e.t.Error("the request taken is left in the state directory")
	}
}

// updaterCalls is how many calls to Docker the updater made (the status it publishes asks systemd
// whether the path unit is on, which isn't a change).
func (e *updEnv) updaterCalls() int {
	n := 0
	for _, c := range e.calls() {
		if !strings.HasPrefix(c, "systemctl ") {
			n++
		}
	}
	return n
}

func requestOutcome(t *testing.T, e *updEnv) *updatestatus.RequestRun {
	t.Helper()
	s := e.public()
	if s.Requests == nil || s.Requests.Last == nil {
		t.Fatalf("no outcome of the request in the status for the server: %+v", s.Requests)
	}
	return s.Requests.Last
}

// A request is the nightly run: the newest on the channel is installed, the request is gone, the
// status says the run was requested and what became of it, and the page's "running" is there
// while it goes on and gone when it ends.
func TestUpdateRequestedDoesTheChannelRun(t *testing.T) {
	e, dir := requestEnv(t)
	e.request(dir, requestLine)
	e.write("fake/block", "") // the run waits in its first pull, after it took the request
	cmd := e.command("--requested")
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = os.Remove(filepath.Join(e.fake, "block")) }()
	for deadline := time.Now().Add(20 * time.Second); e.count(`^pull`) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the run never got to pulling\n%s", out.String())
		}
	}
	e.wantNoRequestLeft(dir)
	if s := e.public(); s.Running == nil || !s.Running.Requested || s.Running.Since.IsZero() || s.Requests == nil || !s.Requests.Supported {
		t.Errorf("while it runs: running = %+v, requests = %+v", s.Running, s.Requests)
	}
	if err := os.Remove(filepath.Join(e.fake, "block")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the run: %v\n%s", err, out.String())
	}
	st := e.wantStatus("updated", "")
	if st["requested"] != true || st["to"] != updNew {
		t.Errorf("status = %v", st)
	}
	s := e.public()
	if s.Running != nil || s.LastRun == nil || s.LastRun.Result != "updated" || !s.LastRun.Requested || s.LastRun.To != updNew || !s.LastRun.BackedUp ||
		s.LastChange == nil || !s.LastChange.Requested || s.RecordedVersion != updNew {
		t.Errorf("status for the server = %+v", s)
	}
	if last := requestOutcome(t, e); last.Result != "updated" || last.At.IsZero() || last.Finished.IsZero() {
		t.Errorf("outcome = %+v", last)
	}
	e.wantNoRequestLeft(dir)
}

// A run nobody requested says it wasn't, and a request is not needed for it.
func TestUpdateNightlyRunIsNotRequested(t *testing.T) {
	e, dir := requestEnv(t)
	e.request(dir, requestLine) // a request waiting is not this run's
	if code, out := e.run(); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, ok := e.status()["requested"]; ok {
		t.Error("a nightly run is marked requested")
	}
	if s := e.public(); s.LastRun.Requested || s.Running != nil {
		t.Errorf("status for the server = %+v", s.LastRun)
	}
	if !e.exists("state/request/update-now") {
		t.Error("a nightly run took the request")
	}
}

// Every gate of the nightly run holds for a request, the request is gone either way, and nothing
// is changed: the answer is a code the page says in words.
func TestUpdateRequestedRespectsTheGates(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(e *updEnv)
		code    int
		result  string
		reason  string
		wantErr string
	}{
		"a hold": {func(e *updEnv) { e.write("etc/update.hold", "") }, 0, "skipped", "hold", "updates are on hold"},
		"the channel is off": {func(e *updEnv) {
			e.write("etc/install.env", e.read("etc/install.env")+"TASKTO_UPDATE_CHANNEL=off\n")
		}, 0, "skipped", "off", "automatic updates are off"},
		"a release that needs a person": {func(e *updEnv) {
			e.gh.releases = []map[string]any{}
			e.release(updNew, "Notes\nmanual-upgrade: true\n", false, false)
		}, 0, "skipped", "manual-upgrade", "needs manual steps"},
		"nothing newer": {func(e *updEnv) { e.gh.releases = []map[string]any{} }, 0, "up-to-date", "", "no release is newer"},
		"a stopped update": {func(e *updEnv) {
			e.write("state/blocked", "an earlier update stopped\n")
		}, 1, "blocked", "blocked", "taskto-update --clear-block"},
		"an interrupted update": {func(e *updEnv) {
			e.write("state/transaction", `{"from":"`+updOld+`","to":"`+updNew+`","phase":"switching"}`)
		}, 1, "interrupted", "interrupted", "was interrupted"},
		"a server that isn't the version on record": {func(e *updEnv) { e.setRunning("v9.9.9") }, 1, "blocked", "version-mismatch", "the server runs"},
	} {
		t.Run(name, func(t *testing.T) {
			e, dir := requestEnv(t)
			tc.prepare(e)
			e.request(dir, requestLine)
			code, out := e.run("--requested")
			if code != tc.code || !strings.Contains(out, tc.wantErr) {
				t.Fatalf("exit %d, want %d with %q\n%s", code, tc.code, tc.wantErr, out)
			}
			e.wantNoRequestLeft(dir)
			if last := requestOutcome(t, e); last.Result != tc.result || last.Code != tc.reason {
				t.Errorf("outcome = %+v, want %s %q", last, tc.result, tc.reason)
			}
			if s := e.public(); s.Running != nil {
				t.Errorf("still running: %+v", s.Running)
			}
			for _, never := range []string{`^compose create`, `^compose start`, `^compose stop`, `^tag `} {
				if e.count(never) != 0 {
					t.Errorf("a call matching %s was made", never)
				}
			}
			if e.exists("state/transaction") && name != "an interrupted update" {
				t.Error("an update's record was made")
			}
			if got := e.read("srv/.env"); !strings.Contains(got, "TASKTO_VERSION="+updOld+"\n") {
				t.Errorf(".env = %q: the install was changed", got)
			}
		})
	}
}

// A request is only an existence: whatever else is in the directory, a link, a directory, a
// fifo or a large file, is removed and ignored, never followed or read, and changes nothing.
func TestUpdateRequestedIgnoresAHostileRequest(t *testing.T) {
	secret := "do-not-read-this"
	for name, setup := range map[string]func(t *testing.T, e *updEnv, dir string) (guard string){
		"a symbolic link": func(t *testing.T, e *updEnv, dir string) string {
			target := filepath.Join(e.dir, "elsewhere.txt")
			if err := os.WriteFile(target, []byte(secret), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, updatestatus.RequestFile)); err != nil {
				t.Fatal(err)
			}
			return target
		},
		"a symbolic link to a directory": func(t *testing.T, e *updEnv, dir string) string {
			target := e.sub("elsewhere")
			if err := os.WriteFile(filepath.Join(target, "keep"), []byte(secret), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, updatestatus.RequestFile)); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(target, "keep")
		},
		"a directory": func(t *testing.T, e *updEnv, dir string) string {
			d := filepath.Join(dir, updatestatus.RequestFile, "inner")
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "x"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			return ""
		},
		"a fifo": func(t *testing.T, e *updEnv, dir string) string {
			if err := syscall.Mkfifo(filepath.Join(dir, updatestatus.RequestFile), 0o644); err != nil {
				t.Skipf("no fifo here: %v", err)
			}
			return ""
		},
		"a large file": func(t *testing.T, e *updEnv, dir string) string {
			e.request(dir, requestLine+strings.Repeat("x", 4096))
			return ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, dir := requestEnv(t)
			guard := setup(t, e, dir)
			done := make(chan struct{})
			var code int
			var out string
			go func() { code, out = e.run("--requested"); close(done) }()
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("the updater hangs on the request")
			}
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			e.wantNoRequestLeft(dir)
			e.wantUntouched()
			if guard != "" {
				if b, err := os.ReadFile(guard); err != nil || string(b) != secret {
					t.Errorf("what the link led to was changed: %q %v", b, err)
				}
			}
			if last := requestOutcome(t, e); last.Result != "ignored" || last.Code != "bad-request" {
				t.Errorf("outcome = %+v, want ignored bad-request", last)
			}
			if e.exists("state/status.json") {
				t.Error("an ignored request wrote a run's status")
			}
			if e.count(`^pull`) != 0 {
				t.Error("an ignored request went on to pull images")
			}
			if strings.Contains(out, secret) {
				t.Error("the updater printed what the link led to")
			}
		})
	}
}

// What is in a request's content never becomes an argument or anything else: a file of any text
// that is small and regular is a request (its existence is the signal), and does the same run.
func TestUpdateRequestedContentIsNeverUsed(t *testing.T) {
	e, dir := requestEnv(t)
	e.request(dir, "--to v9.9.9; touch "+filepath.Join(e.dir, "pwned")+" $(touch "+filepath.Join(e.dir, "pwned2")+")\n")
	if code, out := e.run("--requested"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	e.wantStatus("updated", "")
	if st := e.status(); st["to"] != updNew {
		t.Errorf("the request chose a version: %v", st)
	}
	if e.exists("pwned") || e.exists("pwned2") {
		t.Error("the request's content was run")
	}
}

// One requested run per ten minutes, and never the same request twice. A request turned away is
// answered, and the last run's status stays as it was.
func TestUpdateRequestedIsRateLimited(t *testing.T) {
	e, dir := requestEnv(t)
	e.extra = []string{"TASKTO_UPDATE_REQUEST=" + dir} // the default interval
	e.request(dir, requestLine)
	if code, out := e.run("--requested"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	e.wantStatus("updated", "")
	calls := e.updaterCalls()
	before := e.read("state/status.json")

	e.release("v0.4.0", "", false, false)
	e.setRunning(updNew)
	e.request(dir, "2026-11-24T12:05:00Z fedcba9876543210fedcba9876543210\n")
	if code, out := e.run("--requested"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	e.wantNoRequestLeft(dir)
	if got := e.updaterCalls(); got != calls {
		t.Errorf("a second request within the interval ran: %d calls, was %d", got, calls)
	}
	if e.read("state/status.json") != before {
		t.Error("a turned-away request changed the last run's status")
	}
	if last := requestOutcome(t, e); last.Result != "ignored" || last.Code != "rate-limited" {
		t.Errorf("outcome = %+v, want ignored rate-limited", last)
	}
	if s := e.public(); s.LastRun.Result != "updated" || s.LastRun.To != updNew {
		t.Errorf("last run = %+v", s.LastRun)
	}

	// Without the limit, the same request is still handled once.
	e.extra = []string{"TASKTO_UPDATE_REQUEST=" + dir, "TASKTO_UPDATE_REQUEST_MIN_INTERVAL=0"}
	e.request(dir, requestLine)
	if code, out := e.run("--requested"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if last := requestOutcome(t, e); last.Result != "ignored" {
		t.Errorf("the same request twice: %+v", last)
	}
	e.request(dir, "2026-11-24T12:09:00Z 11111111111111111111111111111111\n")
	if code, out := e.run("--requested"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	e.wantStatus("updated", "")
	if st := e.status(); st["to"] != "v0.4.0" {
		t.Errorf("status = %v", st)
	}
}

// With no request waiting, a requested run does nothing and writes nothing (a path unit that
// started it twice, a request another run took).
func TestUpdateRequestedWithNoRequest(t *testing.T) {
	e, _ := requestEnv(t)
	code, out := e.run("--requested")
	if code != 0 || !strings.Contains(out, "no request waits") || e.exists("state/status.json") || e.count(`^pull`) != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// A request directory that isn't as the updater needs it (missing, a link, another owner) is
// neither read nor emptied, and the status says the host can't take a request.
func TestUpdateRequestedNeedsTheDirectoryInPlace(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		e := newUpdEnv(t)
		e.release(updNew, "", false, false)
		code, out := e.run("--requested")
		if code != 0 || !strings.Contains(out, "isn't in place") || e.exists("state/status.json") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if code, _ := e.run("--publish-status"); code != 0 {
			t.Fatal("publish")
		}
		if s := e.public(); s.Requests == nil || s.Requests.Supported {
			t.Errorf("requests = %+v, want unsupported", s.Requests)
		}
	})
	t.Run("a link", func(t *testing.T) {
		e, dir := requestEnv(t)
		real := e.sub("realrequest")
		if err := os.WriteFile(filepath.Join(real, updatestatus.RequestFile), []byte(requestLine), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, dir); err != nil {
			t.Fatal(err)
		}
		code, out := e.run("--requested")
		if code != 0 || !strings.Contains(out, "isn't in place") || !e.exists("realrequest/update-now") || e.count(`^pull`) != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
	t.Run("another owner", func(t *testing.T) {
		e, dir := requestEnv(t)
		e.request(dir, requestLine)
		// Neither root's (here: uid+1) nor the server's.
		e.extra = append(e.extra, "TASKTO_UPDATE_OWNER="+strconv.Itoa(os.Getuid()+1), "TASKTO_UPDATE_REQUEST_OWNER="+strconv.Itoa(os.Getuid()+2))
		code, out := e.run("--requested")
		if code != 0 || !strings.Contains(out, "isn't in place") || !e.exists("state/request/update-now") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
	t.Run("the path unit is off", func(t *testing.T) {
		e, _ := requestEnv(t)
		e.write("bin/systemctl", "#!/bin/sh\necho \"systemctl $*\" >>\"$FAKE_DIR/calls\"\n[ \"$1\" != is-active ] || [ \"$3\" != taskto-update-request.path ]\n")
		if code, _ := e.run("--publish-status"); code != 0 {
			t.Fatal("publish")
		}
		if s := e.public(); s.Requests == nil || s.Requests.Supported {
			t.Errorf("requests = %+v, want unsupported while the unit is off", s.Requests)
		}
	})
}

// --requested is a run of its own: it takes no version and combines with nothing.
func TestUpdateRequestedTakesNoArguments(t *testing.T) {
	e, dir := requestEnv(t)
	e.request(dir, requestLine)
	for _, args := range [][]string{{"--requested", "--to", updNew}, {"--requested", "--dry-run"}, {"--requested", "--clear-block"}, {"--requested", "--publish-status"}, {"--requested", "extra"}} {
		if code, _ := e.run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if !e.exists("state/request/update-now") {
		t.Error("a refused command line took the request")
	}
}

// The status for the server says whether this host can take a request, when the directory is there.
func TestUpdateStatusSaysTheHostCanTakeARequest(t *testing.T) {
	e, _ := requestEnv(t)
	if code, _ := e.run("--publish-status"); code != 0 {
		t.Fatal("publish")
	}
	s := e.public()
	if s.Requests == nil || !s.Requests.Supported || s.Requests.Last != nil || s.Running != nil {
		t.Errorf("requests = %+v, running = %+v", s.Requests, s.Running)
	}
}
