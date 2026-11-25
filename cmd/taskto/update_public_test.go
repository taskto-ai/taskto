package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The paths the safety rules are tried with, by name, for a jq filter or a line of a file in
// the fake docker's directory: the real ones, and symbolic links to them, made on first use.
// {linkstate} leads to the updater's state, {linkprev} to a directory under it, {linkpub} to
// the directory it publishes to (the default one), and {tlsdir} is a directory that has nothing
// to do with it, with {linktls} leading to it.
func (e *updEnv) paths() *strings.Replacer {
	e.t.Helper()
	tls := filepath.Join(e.dir, "tls")
	if err := os.MkdirAll(tls, 0o755); err != nil {
		e.t.Fatal(err)
	}
	for link, to := range map[string]string{
		"link-state": e.state, "link-prev": filepath.Join(e.state, "previous"),
		"link-pub": filepath.Join(e.state, "public"), "link-tls": tls,
	} {
		if err := os.Symlink(to, filepath.Join(e.dir, link)); err != nil && !os.IsExist(err) {
			e.t.Fatal(err)
		}
	}
	return strings.NewReplacer("PARENT", filepath.Dir(e.state), "STATE", e.state,
		"{linkstate}", filepath.Join(e.dir, "link-state"), "{linkprev}", filepath.Join(e.dir, "link-prev"),
		"{linkpub}", filepath.Join(e.dir, "link-pub"), "{linktls}", filepath.Join(e.dir, "link-tls"), "{tlsdir}", tls)
}

// extraBind makes the running containers of a version mount one more host path.
func (e *updEnv) extraBind(version, path string, writable bool) {
	e.t.Helper()
	e.mutate(version, "extrabind")
	e.write("fake/extra-bind", fmt.Sprintf("%s %v\n", e.paths().Replace(path), writable))
}

// Paths that only look like the updater's state are not refused: a volume of TLS files reached by
// a path with `..` in it, a link to it, a named volume or a file that is bound to it, and a
// volume that exists with options that bind it. Each is resolved, and compared where it leads.
func TestUpdateAllowsHostPathsThatOnlyLookLikeTheState(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/config-edit", e.paths().Replace(`
		.services.taskto.volumes += [
			{type: "bind", source: "{tlsdir}/../tls", target: "/tls", read_only: true},
			{type: "bind", source: "{linktls}", target: "/tls2"},
			{type: "bind", source: "STATE/../tls", target: "/tls3", read_only: true},
			{type: "volume", source: "tlsvol", target: "/tls4", volume: {}},
			{type: "volume", source: "tlsvol2", target: "/tls5", volume: {}}]
		| .volumes.tlsvol = {name: "taskto_tlsvol", driver: "local", driver_opts: {type: "none", o: "bind", device: "{linktls}"}}
		| .volumes.tlsvol2 = {name: "taskto_tlsvol2"}
		| .services.taskto.secrets = [{source: "s", target: "/run/secrets/s"}] | .secrets.s = {file: "{tlsdir}/../tls/cert.pem"}`))
	e.write("fake/volume-options", "taskto_tlsvol2 "+e.paths().Replace("{linktls}")+"\n")
	// The running server has one of them too.
	e.extraBind(updNew, "{tlsdir}/../tls", true)
	if code, out := e.run(); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	e.wantStatus("updated", "")
}

// A server that mounts a volume which exists, or an external one, is judged by the options Docker
// has for it, before anything starts, and a volume that is missing is only what Compose says.
func TestUpdateChecksTheDockerOptionsOfExistingVolumesBeforeStarting(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	e.write("fake/config-edit", e.paths().Replace(`
		.services.taskto.volumes += [{type: "volume", source: "statevol", target: "/state", read_only: true, volume: {}}]
		| .volumes.statevol = {name: "taskto_statevol", external: true}`))
	e.write("fake/volume-options", "taskto_statevol "+e.state+"\n")
	if code, _ := e.run(); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	e.wantStatus("failed", "break a safety rule")
	e.wantUntouched()
	if e.count(`^volume inspect --format \{\{json \.Options\}\} taskto_statevol`) == 0 {
		t.Errorf("the volume's options weren't read\n%s", strings.Join(e.calls(), "\n"))
	}
	if e.count(`^compose start`) != 0 {
		t.Error("a candidate that mounts the updater's state was started")
	}
}

// A directory the updater publishes to may be anywhere, but the server may mount it read-only
// only: not by a link, not a directory above it or inside it.
func TestUpdateWithAPublicDirectoryOutsideTheState(t *testing.T) {
	setup := func(t *testing.T) *updEnv {
		e := newUpdEnv(t)
		e.release(updNew, "", false, false)
		pub := filepath.Join(e.dir, "published")
		e.extra = []string{"TASKTO_UPDATE_PUBLIC=" + pub}
		// compose.yaml's TASKTO_UPDATE_STATUS_SOURCE says where the server reads it.
		e.write("fake/config-edit", `(.services.taskto.volumes[] | select(.target == "/update-status") | .source) = "`+pub+`"`)
		return e
	}
	t.Run("read-only, it works", func(t *testing.T) {
		e := setup(t)
		if code, out := e.run(); code != 0 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if s, ok := e.publicAt(filepath.Join(e.dir, "published")); !ok || s.LastRun.Result != "updated" {
			t.Fatalf("not published there: %+v %v", s, ok)
		}
		if e.exists("state/public") {
			t.Error("the default directory was made too")
		}
	})
	for name, source := range map[string]string{
		"writable":                        `"` + "PUB" + `"`,
		"writable, by a link":             `"{linkpubdir}"`,
		"writable, a directory above it":  `"PUB/.."`,
		"writable, a directory inside it": `"PUB/inside"`,
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			pub := filepath.Join(e.dir, "published")
			if err := os.MkdirAll(filepath.Join(pub, "inside"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(pub, filepath.Join(e.dir, "link-published")); err != nil {
				t.Fatal(err)
			}
			src := strings.NewReplacer("PUB", pub, "{linkpubdir}", filepath.Join(e.dir, "link-published")).Replace(source)
			e.write("fake/config-edit", `.services.taskto.volumes += [{type: "bind", source: `+src+`, target: "/pub"}]`)
			if code, _ := e.run(); code != 1 {
				t.Fatalf("exit %d, want 1", code)
			}
			e.wantStatus("failed", "break a safety rule")
			if st := e.status(); !strings.Contains(fmt.Sprint(st["reason"]), "can write where it publishes") &&
				!strings.Contains(fmt.Sprint(st["reason"]), "holds the updater's state") {
				t.Errorf("reason = %v", st["reason"])
			}
			e.wantUntouched()
		})
	}
	t.Run("the running server writes it", func(t *testing.T) {
		e := setup(t)
		e.extraBind(updNew, "PARENT", false)
		e.write("fake/extra-bind", filepath.Join(e.dir, "published")+" true\n")
		if code, _ := e.run(); code != 2 {
			t.Fatalf("exit %d, want 2", code)
		}
		e.wantStatus("stopped", "")
	})
}

// publicAt reads the status the updater left in dir.
func (e *updEnv) publicAt(dir string) (s struct {
	LastRun *struct{ Result string } `json:"last_run"`
}, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		return s, false
	}
	return s, json.Unmarshal(b, &s) == nil && s.LastRun != nil
}

// The directory the server reads must be the updater's alone, with every directory above it:
// owned by root on a host, written by nobody else. Else nothing is published there, the run goes
// on as it was, and the journal says why.
func TestUpdatePublishesOnlyToADirectoryNobodyElseCanWrite(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(e *updEnv)
		extra   func(e *updEnv) []string
	}{
		// Only the last directory of the path is the updater's to make right; the ones above it
		// are the host's.
		"a directory above it is writable by its group": {
			prepare: func(e *updEnv) { chmod(t, e.sub("outer"), 0o775) },
			extra:   func(e *updEnv) []string { return []string{"TASKTO_UPDATE_PUBLIC=" + e.dir + "/outer/public"} },
		},
		"a directory above it is writable by anyone": {
			prepare: func(e *updEnv) { chmod(t, e.sub("outer"), 0o777) },
			extra:   func(e *updEnv) []string { return []string{"TASKTO_UPDATE_PUBLIC=" + e.dir + "/outer/public"} },
		},
		"the directory is somebody else's": {extra: func(e *updEnv) []string {
			return []string{fmt.Sprintf("TASKTO_UPDATE_OWNER=%d", os.Getuid()+1)}
		}},
		"above it, a link to a directory others can write": {
			prepare: func(e *updEnv) {
				chmod(t, e.sub("elsewhere"), 0o777)
				if err := os.Symlink(filepath.Join(e.dir, "elsewhere"), filepath.Join(e.dir, "viaLink")); err != nil {
					t.Fatal(err)
				}
			},
			extra: func(e *updEnv) []string { return []string{"TASKTO_UPDATE_PUBLIC=" + e.dir + "/viaLink/public"} },
		},
		"it is a link to a file": {prepare: func(e *updEnv) {
			e.write("file", "x")
			if err := os.Symlink(filepath.Join(e.dir, "file"), filepath.Join(e.state, "public")); err != nil {
				t.Fatal(err)
			}
		}},
		"it is a relative path": {extra: func(e *updEnv) []string { return []string{"TASKTO_UPDATE_PUBLIC=state/public"} }},
		"it has .. in it": {extra: func(e *updEnv) []string {
			return []string{"TASKTO_UPDATE_PUBLIC=" + e.state + "/x/../public"}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			if tc.prepare != nil {
				tc.prepare(e)
			}
			if tc.extra != nil {
				e.extra = tc.extra(e)
			}
			code, out := e.run()
			if code != 0 || !strings.Contains(out, "can't write") {
				t.Fatalf("exit %d\n%s", code, out)
			}
			// The update itself was made, and nothing was written for the server.
			e.wantStatus("updated", "")
			_ = filepath.WalkDir(e.dir, func(path string, d os.DirEntry, _ error) error {
				if d != nil && d.Name() == "status.json" && !strings.HasPrefix(path, e.state+"/status.json") && path != filepath.Join(e.state, "status.json") {
					t.Errorf("%s was written", path)
				}
				return nil
			})
		})
	}
}

// A directory that others could write is made the updater's again, since it is the last of the
// path and the updater's own to make: that is not a reason to publish nothing.
func TestUpdateMakesItsPublicDirectoryRightAgain(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	chmod(t, e.sub("state/public"), 0o777)
	if code, out := e.run(); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if fi, err := os.Stat(filepath.Join(e.state, "public")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the directory is %v (%v)", fi, err)
	}
	if s := e.public(); s.LastRun == nil || s.LastRun.Result != "updated" {
		t.Fatalf("status = %+v", s)
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// Nothing the server can plant, or guess, is followed: links in the directory, under any name the
// updater might write to, lead nowhere, and no temporary file is left.
func TestUpdatePublishesThroughANewTemporaryFileOnly(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	victim := filepath.Join(e.dir, "victim")
	e.write("victim", "precious\n")
	pub := e.sub("state/public")
	for _, name := range []string{"status.json", "status.json.new", ".status.json.new", ".status.json.tmp", "status.json.tmp"} {
		if err := os.Symlink(victim, filepath.Join(pub, name)); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := e.run(); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := e.read("victim"); got != "precious\n" {
		t.Fatalf("a link in the directory was written through: %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(pub, "status.json")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("status.json is %v (%v), not a file of its own", fi, err)
	}
	if s := e.public(); s.LastRun == nil || s.LastRun.Result != "updated" {
		t.Fatalf("status = %+v", s)
	}
	// What is left besides the file is what was planted, and no leftover of a temporary file.
	entries, _ := os.ReadDir(pub)
	for _, en := range entries {
		if fi, _ := os.Lstat(filepath.Join(pub, en.Name())); fi.Mode()&os.ModeSymlink == 0 && en.Name() != "status.json" {
			t.Errorf("%s was left in the directory", en.Name())
		}
	}
	// A directory in the way of the file is no reason to write elsewhere.
	if err := os.Remove(filepath.Join(pub, "status.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(pub, "status.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out := e.run(); code != 0 || !strings.Contains(out, "can't write") {
		t.Fatalf("a directory in the way: exit %d\n%s", code, out)
	}
	if entries, _ := os.ReadDir(filepath.Join(pub, "status.json")); len(entries) != 0 {
		t.Errorf("a file was put inside the directory in the way: %v", entries)
	}
}

// The copy for the server is structured: every word of it is from a list, and what the updater
// keeps to itself (a backup's path, the name of a volume, the registry's host, where its record
// is) is never in it, whatever went wrong.
func TestUpdateStatusForTheServerHasNoPathHostOrVolumeName(t *testing.T) {
	allowed := map[string]bool{"version": true, "checked": true, "channel": true, "hold": true, "recorded_version": true,
		"last_run": true, "last_change": true, "needs_person": true}
	runKeys := map[string]bool{"at": true, "finished": true, "result": true, "from": true, "to": true, "code": true, "backed_up": true}
	for name, setup := range map[string]func(e *updEnv){
		"an update":        func(e *updEnv) {},
		"a stop":           func(e *updEnv) { e.write("fake/health-bad", updNew+"\n") },
		"an unsafe update": func(e *updEnv) { e.write("fake/config-edit", `.services.taskto.user = "0"`) },
		"a pull that fails": func(e *updEnv) {
			e.failCalls(`^pull `)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			setup(e)
			e.run()
			raw := e.read("state/public/status.json")
			for _, never := range []string{e.dir, e.state, e.backups, "taskto_data", "ghcr.io", "/var/", "transaction", "sha256:", "Rollback", "restore"} {
				if strings.Contains(raw, never) {
					t.Errorf("the server's status has %q:\n%s", never, raw)
				}
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(raw), &doc); err != nil {
				t.Fatal(err)
			}
			for k := range doc {
				if !allowed[k] {
					t.Errorf("unexpected field %q", k)
				}
			}
			for _, k := range []string{"last_run", "last_change"} {
				if run, _ := doc[k].(map[string]any); run != nil {
					for f := range run {
						if !runKeys[f] {
							t.Errorf("%s has the field %q", k, f)
						}
					}
				}
			}
			if need, _ := doc["needs_person"].(map[string]any); need != nil {
				for f := range need {
					if f != "kind" && f != "code" {
						t.Errorf("needs_person has the field %q", f)
					}
				}
			}
		})
	}
}

// Each kind of outcome has its code, from the closed list.
func TestUpdateStatusForTheServerGivesReasonCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(e *updEnv)
		exit  int
		want  string
	}{
		"hold":                     {func(e *updEnv) { e.write("etc/update.hold", "") }, 0, "hold"},
		"the channel is off":       {func(e *updEnv) { e.write("etc/install.env", e.read("etc/install.env")+"TASKTO_UPDATE_CHANNEL='off'\n") }, 0, "off"},
		"images that aren't there": {func(e *updEnv) { e.unpublish(updNew) }, 0, "not-published"},
		"an unsafe candidate":      {func(e *updEnv) { e.write("fake/config-edit", `.services.taskto.user = "0"`) }, 1, "unsafe-settings"},
		"no room for the images":   {func(e *updEnv) { e.extra = []string{"TASKTO_UPDATE_IMAGES_MB=999999999999"} }, 1, "disk"},
		"a server that stops":      {func(e *updEnv) { e.write("fake/health-bad", updNew+"\n") }, 2, "stopped"},
		"a setting that is wrong":  {func(e *updEnv) { e.write("etc/install.env", e.read("etc/install.env")+"TASKTO_UPDATE_KEEP='x'\n") }, 1, "bad-setting"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newUpdEnv(t)
			e.release(updNew, "", false, false)
			tc.setup(e)
			if code, out := e.run(); code != tc.exit {
				t.Fatalf("exit %d, want %d\n%s", code, tc.exit, out)
			}
			if s := e.public(); s.LastRun == nil || s.LastRun.Code != tc.want {
				t.Fatalf("code = %+v, want %q", s.LastRun, tc.want)
			}
		})
	}
}

// --publish-status writes the server's copy from what is there and nothing else: it is what a
// newer updater, installed by hand on a host that had an older one, does at once.
func TestUpdatePublishStatusWritesOnlyTheServersCopy(t *testing.T) {
	e := newUpdEnv(t)
	e.release(updNew, "", false, false)
	if code, out := e.run("--publish-status"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	s := e.public()
	if s.Version != 1 || s.Channel != "release" || s.RecordedVersion != updOld || s.LastRun != nil || s.NeedsPerson != nil {
		t.Fatalf("status = %+v", s)
	}
	// It looked at nothing and changed nothing: no private status, no release list, no docker.
	if e.exists("state/status.json") || strings.TrimSpace(strings.Join(e.calls(), "")) != "" {
		t.Errorf("it did more than publish: %q", e.calls())
	}
	if strings.TrimSpace(e.read("fake/running")) != updOld {
		t.Error("the server was touched")
	}
	// After an update it publishes what that run said, and a pending manual upgrade stays.
	if code, _ := e.run(); code != 0 {
		t.Fatal("update")
	}
	if code, _ := e.run("--publish-status"); code != 0 {
		t.Fatal("publish")
	}
	if s := e.public(); s.LastRun == nil || s.LastRun.Result != "updated" || s.RecordedVersion != updNew {
		t.Fatalf("after an update: %+v", s.LastRun)
	}
	// It takes no other option.
	for _, args := range [][]string{{"--publish-status", "--dry-run"}, {"--publish-status", "--clear-block"}, {"--publish-status", "--to", updNew}} {
		if code, _ := e.run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// A host's updater is a script of the host, and a server's update doesn't change it: the runbook
// and the install guide say it must be reinstalled for the first release with the status, on the
// main channel too, and the page says what the host's updater is when it hasn't reported.
func TestRunbookRequiresReinstallingTheUpdaterForTheStatus(t *testing.T) {
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	runbook := flat(readRepoFile(t, "deploy/INSTALL.md"))
	for _, want := range []string{"required again on a VM set up before the release that added the update status", "`main` channel",
		"taskto-update --publish-status", "too old to report"} {
		if !strings.Contains(runbook, want) {
			t.Errorf("deploy/INSTALL.md doesn't say %q", want)
		}
	}
	guide := flat(readRepoFile(t, "docs/install.md"))
	for _, want := range []string{"The updater of an existing host must be reinstalled once", "`main` channel", "--publish-status", "too old to report"} {
		if !strings.Contains(guide, want) {
			t.Errorf("docs/install.md doesn't say %q", want)
		}
	}
	if !strings.Contains(readRepoFile(t, "web/src/i18n/locales/en.json"), "The updater on this host is too old to report") {
		t.Error("the page doesn't say the updater is too old to report")
	}
}
