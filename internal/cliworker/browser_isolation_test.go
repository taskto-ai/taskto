//go:build linux

package cliworker_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"taskto.ai/taskto/internal/browser"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/cliworker/clitest"
)

// The agent browser inside the container mode's OS boundary (docs/agent-browser.md): each
// user's Xvfb, Chromium and x11vnc run as their own Unix user, and another user's process can
// reach none of it. These need root and the programs: CI's browser job (browser.yml) runs
// them; they run only with TASKTO_BROWSER_IT=1.

func browserBins(t *testing.T) map[browser.Program]string {
	t.Helper()
	if os.Getenv("TASKTO_BROWSER_IT") != "1" {
		t.Skip("a real browser test: set TASKTO_BROWSER_IT=1 to run it")
	}
	bins := browser.FindBins("")
	if bins[browser.Chromium] == "" || bins[browser.Xvfb] == "" || bins[browser.X11vnc] == "" {
		t.Fatalf("TASKTO_BROWSER_IT=1 but the browser's programs aren't all installed: %v", bins)
	}
	return bins
}

// proc is a process as /proc shows it.
type proc struct {
	pid     int
	uid     int
	comm    string
	cmdline string
}

func procs(t *testing.T) []proc {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var out []proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		p := proc{pid: pid, uid: -1, cmdline: strings.ReplaceAll(string(cmdline), "\x00", " ")}
		for _, line := range strings.Split(string(status), "\n") {
			if v, ok := strings.CutPrefix(line, "Name:"); ok {
				p.comm = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(line, "Uid:"); ok {
				if f := strings.Fields(v); len(f) > 0 {
					p.uid, _ = strconv.Atoi(f[0])
				}
			}
		}
		out = append(out, p)
	}
	return out
}

// tcpListeners lists the listening TCP sockets (IPv4 and IPv6) owned by uid min or more.
func tcpListeners(t *testing.T, min int) []string {
	t.Helper()
	var out []string
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			// sl local_address rem_address st tx:rx tr:when retrnsmt uid …; st 0A is LISTEN.
			if len(fields) < 8 || fields[3] != "0A" {
				continue
			}
			if uid, err := strconv.Atoi(fields[7]); err == nil && uid >= min {
				out = append(out, file+" "+fields[1]+" uid "+fields[7])
			}
		}
		f.Close()
	}
	return out
}

func TestIsolationBrowser(t *testing.T) {
	h := newIsolated(t)
	h.introduce(t, 1, 2)
	bins := browserBins(t)
	m := browser.NewManager(browser.Config{Host: h.w.BrowserHost(bins), NoSandbox: true, StartTimeout: 60 * time.Second})
	t.Cleanup(m.Close)
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "secret", Value: "user-1-session", MaxAge: 3600, HttpOnly: true}) //nolint:gosec // a plain-http test page
		fmt.Fprint(w, "<!doctype html><title>Signed in</title><p>hello</p>")
	}))
	defer site.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := m.Do(ctx, acct(1), browser.Action{Op: browser.OpOpen, Browser: browser.Personal, URL: site.URL})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tab.Title != "Signed in" {
		t.Fatalf("opened %+v", res.Tab)
	}

	// Every process of the browser is user 1's Unix user, with no capabilities. They are
	// told apart from other tests' browsers by this test's directories on their command lines
	// (Chromium's own helper processes inherit its uid, and the sweep below covers them).
	var chromium int
	found := map[string]bool{}
	for _, p := range procs(t) {
		if !strings.Contains(p.cmdline, h.layout.Root) {
			continue
		}
		switch p.comm {
		case "Xvfb", "x11vnc", "chromium":
			found[p.comm] = true
			if p.uid != h.uid(1) {
				t.Errorf("%s (pid %d) runs as uid %d", p.comm, p.pid, p.uid)
			}
			if p.comm == "chromium" && chromium == 0 {
				chromium = p.pid
			}
			status, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", p.pid))
			if !strings.Contains(string(status), "CapEff:\t0000000000000000") {
				t.Errorf("%s (pid %d) has capabilities", p.comm, p.pid)
			}
		}
	}
	if !found["Xvfb"] || !found["x11vnc"] || !found["chromium"] {
		t.Fatalf("the browser's processes: %v", found)
	}
	// None of them listens on TCP: x11vnc's IPv6 port 5900 among them.
	if l := tcpListeners(t, isoBase); len(l) > 0 {
		t.Errorf("a browser listens on TCP: %v", l)
	}

	// User 1's directory, profile and sockets are theirs alone.
	p := h.layout.BrowserPaths(h.w.HomeForTest(acct(1)))
	for _, dir := range []string{p.Root, p.Profile(), p.Run()} {
		var st syscall.Stat_t
		if err := syscall.Lstat(dir, &st); err != nil || int(st.Uid) != h.uid(1) || st.Mode&0o777 != 0o700 {
			t.Errorf("%s: uid %d mode %o (%v)", dir, st.Uid, st.Mode&0o777, err)
		}
	}

	// The live view reaches user 1's display, through the helper as user 1.
	live, err := m.OpenLive(ctx, acct(1))
	if err != nil {
		t.Fatal(err)
	}
	banner := make([]byte, 12)
	if _, err := io.ReadFull(live, banner); err != nil || string(banner) != "RFB 003.008\n" {
		t.Fatalf("the live view said %q, %v", banner, err)
	}
	live.Close()

	// User 2 tries everything of user 1's browser, and gets nothing.
	sockets, err := filepath.Glob("/tmp/.X11-unix/X*")
	if err != nil || len(sockets) == 0 {
		t.Fatalf("no X display socket: %v", err)
	}
	display := sockets[len(sockets)-1]
	probes := []string{
		"connect:" + p.VNCSocket(),
		"list:" + p.Root,
		"read:" + filepath.Join(p.Profile(), "Local State"),
		"read:" + p.Xauthority(),
		"x11:" + display,
		"x11abstract:" + display,
		"kill:" + strconv.Itoa(chromium),
	}
	h.signIn(t, 2, cliworker.Codex)
	h.scenario(t, 2, cliworker.Codex, clitest.Scenario{Probes: probes})
	done := last(run(t, h.w, spec("browser-probe", 2, cliworker.Codex, "look around")))
	if done.Status != cliworker.StatusOK {
		t.Fatalf("user 2's turn: %+v", done)
	}
	want := map[string]string{
		probes[0]: "permission denied",
		probes[1]: "permission denied",
		probes[2]: "permission denied",
		probes[3]: "permission denied",
		probes[4]: "ok refused",
		probes[5]: "connection refused",
		probes[6]: "operation not permitted",
	}
	for _, probe := range probes {
		if got := probeResult(done.Text, probe); !strings.Contains(got, want[probe]) {
			t.Errorf("user 2's %s: %s (want %s)", probe, got, want[probe])
		}
	}

	// User 2's own browser is another profile: user 1's cookie isn't in it.
	res, err = m.Do(ctx, acct(2), browser.Action{Op: browser.OpOpen, Browser: browser.Testing, URL: "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := m.Do(ctx, acct(2), browser.Action{Op: browser.OpSnapshot, Browser: browser.Testing, Tab: res.Tab.ID})
	if err != nil || strings.Contains(snap.Snapshot, "user-1-session") {
		t.Fatalf("user 2's browser: %v %q", err, snap.Snapshot)
	}
	for _, pr := range procs(t) {
		if pr.comm == "chromium" && strings.Contains(pr.cmdline, h.layout.Root) && pr.uid != h.uid(1) && pr.uid != h.uid(2) {
			t.Errorf("a chromium (pid %d) runs as uid %d", pr.pid, pr.uid)
		}
	}

	// A new worker stops every browser process, as it does every CLI's.
	second, err := cliworker.New(h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	eventually(t, "a browser process survived the new worker", func() bool {
		for _, pr := range procs(t) {
			if pr.uid >= isoBase && (pr.comm == "chromium" || pr.comm == "Xvfb" || pr.comm == "x11vnc") && alive(pr.pid) {
				return false
			}
		}
		return true
	})
}
