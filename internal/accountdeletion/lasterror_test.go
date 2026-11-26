package accountdeletion

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/providers"
)

const leakedEmail = "someone.private@example.test"

// lastErrors lists what the table keeps of why steps failed.
func (w *world) lastErrors() []string {
	w.t.Helper()
	rows, err := w.db.Query(`SELECT kind || ': ' || coalesce(last_error, '') FROM purge_cleanups ORDER BY id`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// What a subsystem says in an error can hold personal text (a provider's refusal quoting an email, a
// path with an id): the table keeps a fixed word for the kind of failure, never that text, and
// neither does the error the purge answers.
func TestAStepsFailureIsStoredAsAFixedReasonNeverTheUpstreamText(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(w *world)
		kind  string
		want  string
	}{
		{"runtime unreachable", func(w *world) {
			w.agent.down = fmt.Errorf("dial unix /x/%s.sock: connection refused", leakedEmail)
		}, cleanAgentConversation, "sidecar_unreachable"},
		{"runtime busy", func(w *world) {
			w.agent.down = &agentrt.StatusError{Code: http.StatusConflict, Status: "409 Conflict", Message: "busy: " + leakedEmail}
		}, cleanAgentConversation, "sidecar_busy status=409"},
		{"runtime refuses", func(w *world) {
			w.agent.down = &agentrt.StatusError{Code: http.StatusBadRequest, Status: "400 Bad Request", Message: "refused for " + leakedEmail}
		}, cleanAgentConversation, "sidecar_refused status=400"},
		{"CLI worker fails", func(w *world) {
			w.cli.fail = errors.New("removing /home/" + leakedEmail + ": permission denied")
		}, cleanCLIHome, "cli_worker_failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			c.setup(w)
			w.deleteAccount()
			err := w.svc.Purge(ctx, w.leaver)
			if err == nil {
				t.Fatal("the purge went on past a failed step")
			}
			if strings.Contains(err.Error(), leakedEmail) {
				t.Errorf("the purge's error holds the upstream text: %v", err)
			}
			found := false
			for _, got := range w.lastErrors() {
				if strings.Contains(got, leakedEmail) || strings.Contains(got, "/x/") {
					t.Errorf("purge_cleanups holds the upstream text: %q", got)
				}
				if strings.HasPrefix(got, c.kind+": ") {
					found = true
					if got != c.kind+": "+c.want {
						t.Errorf("last_error = %q; want %q", got, c.want)
					}
				}
			}
			if !found {
				t.Errorf("no step of kind %s is recorded: %v", c.kind, w.lastErrors())
			}
		})
	}
}

func TestAProvidersStepsFailureIsStoredAsAFixedReason(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"the runtime refuses the file": {fmt.Errorf("%w: provider %s: bad base URL", providers.ErrRefused, leakedEmail), "provider_reload_failed"},
		"a removed key stays":          {fmt.Errorf("%w (%s)", providers.ErrKeyNotRemoved, "/data/agent/keys/"+leakedEmail+".key"), "provider_key_not_removed"},
		"anything else":                {errors.New("reading " + leakedEmail), "provider_reload_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.svc.SetProviders(func(context.Context) error { return c.err })
			w.deleteAccount()
			if err := w.svc.Purge(ctx, w.leaver); err != nil {
				t.Fatalf("a failure after the account went failed the purge: %v", err)
			}
			if err := w.svc.RunCleanups(ctx); err == nil || strings.Contains(err.Error(), leakedEmail) {
				t.Fatalf("RunCleanups = %v", err)
			}
			got := w.lastErrors()
			if len(got) != 1 || got[0] != cleanProviders+": "+c.want {
				t.Fatalf("purge_cleanups = %q; want only the reason %q", got, c.want)
			}
			// The step is kept, to be tried again, and is done when the key can be removed.
			if n := w.count(`SELECT count(*) FROM purge_cleanups WHERE kind = ?`, cleanProviders); n != 1 {
				t.Fatalf("%d provider steps recorded after a failure, want 1", n)
			}
			w.svc.SetProviders(func(context.Context) error { return nil })
			if err := w.svc.RunCleanups(ctx); err != nil {
				t.Fatal(err)
			}
			if n := w.count(`SELECT count(*) FROM purge_cleanups`); n != 0 {
				t.Errorf("%d steps remain", n)
			}
		})
	}
}
