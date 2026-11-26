package dictation

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/egress"
)

// installVoz puts a fake Voz add-on in dir, answering script's output.
func installVoz(t *testing.T, dir, script string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "desertant-voz"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "desertant-voz", "transcribe"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestDetectAddons(t *testing.T) {
	dir := t.TempDir()
	if got := DetectAddons(dir); len(got) != 0 {
		t.Fatalf("found %v in an empty folder", got)
	}
	installVoz(t, dir, "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(dir, "desertant-voz", "transcribe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DetectAddons(dir); len(got) != 0 {
		t.Fatal("a file that isn't executable was taken for the add-on")
	}
	if err := os.Chmod(filepath.Join(dir, "desertant-voz", "transcribe"), 0o700); err != nil {
		t.Fatal(err)
	}
	got := DetectAddons(dir)
	if len(got) != 1 || got[0].ID != "desertant-voz" || got[0].Path != filepath.Join(dir, "desertant-voz", "transcribe") {
		t.Fatalf("found %+v", got)
	}
}

// The fake Voz checks what it is given: a WAV on standard input, the language, and a clean
// environment.
const vozScript = `#!/bin/sh
[ -z "$TASKTO_TEST_SECRET" ] || exit 3
head -c 4 | grep -q RIFF || exit 4
printf '{"text": "voz heard task to (%s)"}\n' "$*"
`

func TestVozAddon(t *testing.T) {
	if !groupsSupported {
		t.Skip("add-ons need Unix")
	}
	t.Setenv("TASKTO_TEST_SECRET", "the add-on must not see this")
	addons := t.TempDir()
	installVoz(t, addons, vozScript)
	e := newEnv(t, &fakeLocal{state: LocalState{State: LocalReady}, text: "local"}, egress.Policy{}, func(c *Config) {
		c.Addons, c.Scratch = DetectAddons(addons), t.TempDir()
	})
	owner, ws := e.setup()
	member := e.join(owner, ws, "member")
	owner.putDoc(owner.privateSpace(ws), GlossaryFile, "- taskto\n")

	rec := member.do(http.MethodGet, route(ws, ""), nil)
	expect(t, rec, http.StatusOK)
	st := decode[statusReply](t, rec)
	if len(st.Addons) != 1 || !st.Addons[0].Installed || st.Addons[0].Notice == "" || st.Addons[0].LicenceURL != "https://license.desertant.com/" {
		t.Fatalf("addons = %+v", st.Addons)
	}
	// Not without its notice confirmed; a member can't confirm it.
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"engine": "desertant-voz"}), http.StatusBadRequest)
	expect(t, member.do(http.MethodPatch, route(ws, ""), map[string]any{"engine": "desertant-voz", "accept_notice": "desertant-voz"}),
		http.StatusForbidden)
	rec = owner.do(http.MethodPatch, route(ws, ""), map[string]any{"engine": "desertant-voz", "accept_notice": "desertant-voz", "language": "pt"})
	expect(t, rec, http.StatusOK)
	st = decode[statusReply](t, rec)
	if st.Engine != "desertant-voz" || st.Settings.AcceptedNotice != "desertant-voz" || len(st.Credits) != 1 ||
		st.Credits[0].URL != "https://desertant.com" || !strings.Contains(st.Credits[0].Note, "Desert Ant Labs") {
		t.Fatalf("status = %+v", st)
	}
	var by int64
	if err := e.db.QueryRow(`SELECT notice_by FROM dictation_settings WHERE workspace_id = ?`, ws).Scan(&by); err != nil || by != owner.id {
		t.Fatalf("notice_by = %d, %v", by, err)
	}

	// The glossary corrects its text too (the owner reads their own private space).
	rec = owner.do(http.MethodPost, route(ws, "transcriptions"), speech())
	expect(t, rec, http.StatusOK)
	if got := decode[transcribeReply](t, rec); got.Text != "voz heard taskto (--language pt)" || got.Engine != "desertant-voz" {
		t.Fatalf("reply = %+v", got)
	}
	// Back to local, and to the add-on again without confirming twice.
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"engine": "local"}), http.StatusOK)
	expect(t, owner.do(http.MethodPatch, route(ws, ""), map[string]any{"engine": "desertant-voz"}), http.StatusOK)

	// A server without it installed says so.
	bare := newEnv(t, nil, egress.Policy{})
	o2, ws2 := bare.setup()
	expect(t, o2.do(http.MethodPatch, route(ws2, ""), map[string]any{"accept_notice": "desertant-voz"}), http.StatusBadRequest)
	expect(t, o2.do(http.MethodPatch, route(ws2, ""), map[string]any{"engine": "desertant-voz"}), http.StatusBadRequest)
	if _, err := bare.db.Exec(`INSERT INTO dictation_settings (workspace_id, engine, addon_notice) VALUES (?, 'desertant-voz', 'desertant-voz')`, ws2); err != nil {
		t.Fatal(err)
	}
	rec = o2.do(http.MethodPost, route(ws2, "transcriptions"), speech())
	expect(t, rec, http.StatusServiceUnavailable)
	if !strings.Contains(rec.Body.String(), "isn't installed") {
		t.Fatalf("reply = %s", rec.Body.String())
	}
}
