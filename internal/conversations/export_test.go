package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
)

func exportIDs(t *testing.T, e *env, ws int64, c *client) []int64 {
	t.Helper()
	list, err := e.svc.ExportList(context.Background(), ws, c.id)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, cv := range list {
		ids = append(ids, cv.ID)
	}
	return ids
}

func TestExportListIsTheCallersOwnInOneWorkspace(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	other := e.invite(owner, ws, "other@example.com")
	second := e.workspace(owner, other)

	mine := owner.create(ws, map[string]any{"title": "Mine"})
	old := owner.create(ws, map[string]any{"title": "Old"})
	owner.expect(http.StatusOK, http.MethodPatch, path(ws, fmt.Sprintf("/%d", old.ID)), map[string]any{"archived": true})
	theirs := other.create(ws, map[string]any{"title": "Theirs"})
	elsewhere := owner.create(second, map[string]any{"title": "Elsewhere"})

	if got := exportIDs(t, e, ws, owner); fmt.Sprint(got) != fmt.Sprint([]int64{mine.ID, old.ID}) {
		t.Errorf("owner's list = %v, want %v (archived too, not %d or %d)", got, []int64{mine.ID, old.ID}, theirs.ID, elsewhere.ID)
	}
	if got := exportIDs(t, e, ws, other); fmt.Sprint(got) != fmt.Sprint([]int64{theirs.ID}) {
		t.Errorf("other's list = %v, want only %d", got, theirs.ID)
	}
	if got := exportIDs(t, e, second, owner); fmt.Sprint(got) != fmt.Sprint([]int64{elsewhere.ID}) {
		t.Errorf("owner's list in the other workspace = %v", got)
	}
	if _, err := e.db.Exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, ws, other.id); err != nil {
		t.Fatal(err)
	}
	if got := exportIDs(t, e, ws, other); len(got) != 0 {
		t.Errorf("a person who left lists %v", got)
	}
}

func TestExportEntriesJoinsThePages(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	const n = 2*exportPage + 37
	agent := c.agentIDForTest(t, e)
	for i := range n {
		e.rt.inputs[agent] = append(e.rt.inputs[agent], agentrt.Input{Content: fmt.Sprintf("m%d", i)})
	}
	list, err := e.svc.ExportList(context.Background(), ws, owner.id)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	var got []string
	err = e.svc.ExportEntries(context.Background(), list[0], func(raw json.RawMessage) error {
		var en struct {
			ID      int64  `json:"id"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(raw, &en); err != nil {
			return err
		}
		if en.ID != int64(len(got)+1) {
			t.Fatalf("entry %d has ID %d: out of order", len(got)+1, en.ID)
		}
		got = append(got, en.Content)
		return nil
	})
	if err != nil || len(got) != n || got[0] != "m0" || got[n-1] != fmt.Sprintf("m%d", n-1) {
		t.Fatalf("read %d entries (%v), want %d", len(got), err, n)
	}
}

func TestExportEntriesSaysWhenTheHistoryIsOutOfReach(t *testing.T) {
	for name, tc := range map[string]struct {
		off bool
		err error
	}{"agent runtime off": {off: true}, "agent runtime failing": {err: errors.New("down")}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, tc.off)
			owner, ws := e.setup()
			e.rt.err = tc.err
			if _, err := e.db.Exec(`INSERT INTO conversations (workspace_id, owner_id, agent_id, title) VALUES (?, ?, 7, 'x')`, ws, owner.id); err != nil {
				t.Fatal(err)
			}
			list, err := e.svc.ExportList(context.Background(), ws, owner.id)
			if err != nil || len(list) != 1 {
				t.Fatalf("list = %v, %v", list, err)
			}
			called := false
			err = e.svc.ExportEntries(context.Background(), list[0], func(json.RawMessage) error { called = true; return nil })
			if !errors.Is(err, ErrNoTranscript) || called {
				t.Errorf("ExportEntries = %v (called %v), want ErrNoTranscript and nothing emitted", err, called)
			}
		})
	}
}

type hookRuntime struct {
	Runtime
	onEntries func(ctx context.Context, before int64) error
}

func (h hookRuntime) Entries(ctx context.Context, conv, before int64, limit int) (json.RawMessage, error) {
	if h.onEntries != nil {
		if err := h.onEntries(ctx, before); err != nil {
			return nil, err
		}
	}
	return h.Runtime.Entries(ctx, conv, before, limit)
}

func TestExportEntriesIgnoresEntriesAddedMeanwhile(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	c := owner.create(ws, nil)
	agent := c.agentIDForTest(t, e)
	const n = 2*exportPage + 5
	for i := range n {
		e.rt.inputs[agent] = append(e.rt.inputs[agent], agentrt.Input{Content: fmt.Sprintf("m%d", i)})
	}
	added := false
	e.svc.agent = func(context.Context) (Runtime, error) {
		return hookRuntime{Runtime: e.rt, onEntries: func(_ context.Context, before int64) error {
			if before != 0 && !added {
				added = true
				e.rt.mu.Lock()
				defer e.rt.mu.Unlock()
				for i := range 30 {
					e.rt.inputs[agent] = append(e.rt.inputs[agent], agentrt.Input{Content: fmt.Sprintf("late%d", i)})
				}
			}
			return nil
		}}, nil
	}
	list, err := e.svc.ExportList(context.Background(), ws, owner.id)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	var ids []int64
	err = e.svc.ExportEntries(context.Background(), list[0], func(raw json.RawMessage) error {
		id, err := entryID(raw)
		ids = append(ids, id)
		return err
	})
	if err != nil || !added || len(ids) != n {
		t.Fatalf("exported %d entries (%v, entries added %v), want the %d there at the start", len(ids), err, added, n)
	}
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("entry %d has ID %d: repeated, missing or out of order", i, id)
		}
	}
}

func TestExportEntriesGivesUpOnAStalledAgentRuntime(t *testing.T) {
	for name, stall := range map[string]func(*env, *hookRuntime){
		"a page that never comes": func(e *env, h *hookRuntime) {
			h.onEntries = func(ctx context.Context, _ int64) error { <-ctx.Done(); return ctx.Err() }
			e.svc.agent = func(context.Context) (Runtime, error) { return *h, nil }
		},
		"a runtime that never gets ready": func(e *env, _ *hookRuntime) {
			e.svc.agent = func(ctx context.Context) (Runtime, error) { <-ctx.Done(); return nil, ctx.Err() }
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, false)
			owner, ws := e.setup()
			owner.create(ws, nil)
			list, err := e.svc.ExportList(context.Background(), ws, owner.id)
			if err != nil || len(list) != 1 {
				t.Fatalf("list = %v, %v", list, err)
			}
			e.svc.exportTimeout, e.svc.readyWait = 30*time.Millisecond, 30*time.Millisecond
			stall(e, &hookRuntime{Runtime: e.rt})
			done := make(chan error, 1)
			go func() {
				done <- e.svc.ExportEntries(context.Background(), list[0], func(json.RawMessage) error { return nil })
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrNoTranscript) {
					t.Fatalf("ExportEntries = %v, want ErrNoTranscript", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the export waits for ever on a stalled agent runtime")
			}
		})
	}
}
