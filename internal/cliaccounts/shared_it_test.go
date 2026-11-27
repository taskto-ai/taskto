//go:build unix

package cliaccounts

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/cliworker"
)

func TestRealCLIsThroughTheProxy(t *testing.T) {
	base, keyFile, model := os.Getenv("TASKTO_CLI_IT_BASE_URL"), os.Getenv("TASKTO_CLI_IT_KEY_FILE"), os.Getenv("TASKTO_CLI_IT_MODEL")
	if base == "" || keyFile == "" || model == "" {
		t.Skip("set TASKTO_CLI_IT_BASE_URL, TASKTO_CLI_IT_KEY_FILE and TASKTO_CLI_IT_MODEL to run the real CLIs")
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(string(b))
	bins := map[cliworker.CLI]string{}
	for _, cli := range cliworker.CLIs {
		if path, err := exec.LookPath(string(cli)); err == nil {
			if abs, err := filepath.Abs(path); err == nil {
				bins[cli] = abs
			}
		}
	}
	e := newTurnEnvWith(t, func(c *cliworker.Config) {
		c.Bins, c.Path, c.TurnTimeout = bins, os.Getenv("PATH"), 4*time.Minute
	})
	for cli := range bins {
		t.Run(string(cli), func(t *testing.T) {
			rec := e.owner.do(http.MethodPost, sharedPath(e.ws), map[string]string{
				"cli": string(cli), "label": "Gateway", "base_url": strings.TrimRight(base, "/"), "model": model, "api_key": key,
			})
			expect(t, rec, http.StatusCreated)
			a := decode[SharedAccount](t, rec)
			conv := e.conversation(e.other)
			events := e.turn(conv, turnBody{Turn: "real", CLI: string(cli), Model: "default", Prompt: "Reply with exactly: proxied-ok"})
			d := done(events)
			if d.Status != cliworker.StatusOK {
				t.Fatalf("turn: %+v\n%+v", d, events)
			}
			if !strings.Contains(strings.ToLower(d.Text), "proxied-ok") {
				t.Logf("answer %q (the model's wording, not checked strictly)", d.Text)
			}
			dir := e.layout.User(e.other.id).ConfigOf(cli, sharedSlot(a.ID))
			_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), key) {
						t.Errorf("the key is in %s", path)
					}
				}
				return nil
			})
			expect(t, e.owner.do(http.MethodDelete, sharedPath(e.ws)+"/"+itoa(a.ID), nil), http.StatusNoContent)
		})
	}
}
