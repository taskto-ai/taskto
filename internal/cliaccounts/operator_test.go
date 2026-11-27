package cliaccounts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"taskto.ai/taskto/internal/cliworker"
)

// The server's operators see whether the CLI worker lost its authority, and the boot to hand it
// over to; an admin who isn't an operator, and a member, don't see the worker at all.
func TestOperatorsSeeTheCLIWorkersLostAuthority(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Container, nil)
	sw := e.swappable()
	owner, ws := e.setup()
	path := "/api/workspaces/" + strconv.FormatInt(ws, 10) + "/cli-worker"
	if got := decode[WorkerStatus](t, expectOK(t, owner.do(http.MethodGet, path, nil))); got.AuthorityLost || !got.Reachable || got.Boot != "" {
		t.Errorf("an intact worker = %+v", got)
	}
	if err := e.svc.ReconcileForgotten(ctx); err != nil {
		t.Fatal(err)
	}
	e.worker.Close()
	if err := os.Remove(filepath.Join(e.layout.Root, ".worker", "boots.json")); err != nil {
		t.Fatal(err)
	}
	w, err := cliworker.New(cliworker.Config{Layout: e.layout, UnsafePrivileged: os.Geteuid() == 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	sw.use(LocalWorker(w))
	got := decode[WorkerStatus](t, expectOK(t, owner.do(http.MethodGet, path, nil)))
	if !got.AuthorityLost || got.Boot != e.svc.Boot() || got.Mode != string(Container) {
		t.Errorf("a worker that lost its authority = %+v, want it said, with this server's boot", got)
	}
	member := e.invite(owner, ws, "member@example.com")
	if rec := member.do(http.MethodGet, path, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a member's look = %d, want 403", rec.Code)
	}
}

func expectOK(t *testing.T, rec *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	expect(t, rec, http.StatusOK)
	return rec
}
