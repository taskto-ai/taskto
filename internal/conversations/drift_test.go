package conversations

import (
	"context"
	"fmt"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
)

// fakeSidecar is the agent runtime's list of conversations.
type fakeSidecar struct {
	list    []agentrt.ConversationInfo
	retired []int64
}

func (f *fakeSidecar) Conversations(context.Context) ([]agentrt.ConversationInfo, error) {
	return f.list, nil
}

func (f *fakeSidecar) RetireConversation(_ context.Context, id int64) error {
	for i, c := range f.list {
		if c.ID == id && !c.Retired {
			f.list[i].Retired = true
			f.retired = append(f.retired, id)
			return nil
		}
	}
	return &agentrt.StatusError{Code: 404, Message: "unknown conversation"}
}

func TestDrift(t *testing.T) {
	e := newEnv(t, false)
	owner, ws := e.setup()
	ctx := context.Background()
	record := func(agentID int64) int64 {
		t.Helper()
		var id int64
		if err := e.db.QueryRow(`INSERT INTO conversations (workspace_id, owner_id, agent_id) VALUES (?, ?, ?) RETURNING id`,
			ws, owner.id, agentID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	kept, missing, retired := record(101), record(102), record(103)

	now := time.Now()
	old := now.Add(-OrphanGrace - time.Second).UnixMilli()
	rt := &fakeSidecar{list: []agentrt.ConversationInfo{
		{ID: 101, CreatedAt: old},
		{ID: 103, CreatedAt: old, Retired: true}, // recorded, but the agent runtime refuses it
		{ID: 200, CreatedAt: old},                // an orphan
		{ID: 201, CreatedAt: now.UnixMilli()},    // maybe being recorded right now
		{ID: 202},                                // an orphan from before creation times were kept
		{ID: 203, CreatedAt: old},                // recorded below, after the check
		{ID: 204, CreatedAt: old, Retired: true}, // an orphan retired already
		{ID: 205, CreatedAt: old},                // gone by the time it is retired
	}}

	d, err := CheckDrift(ctx, e.db, rt, now)
	if err != nil {
		t.Fatal(err)
	}
	var lost []int64
	for _, l := range d.Lost {
		if l.WorkspaceID != ws || l.OwnerID != owner.id || l.CreatedAt == "" {
			t.Errorf("lost %+v: want its workspace, owner and creation time", l)
		}
		lost = append(lost, l.ID)
	}
	if fmt.Sprint(lost) != fmt.Sprint([]int64{missing, retired}) {
		t.Fatalf("lost = %v, want %d and %d", lost, missing, retired)
	}
	var orphans []int64
	for _, o := range d.Orphans {
		orphans = append(orphans, o.AgentID)
	}
	if fmt.Sprint(orphans) != fmt.Sprint([]int64{200, 202, 203, 205}) {
		t.Fatalf("orphans = %v, want 200, 202, 203 and 205", orphans)
	}

	if d.Recorded != 3 || d.AllLost() {
		t.Fatalf("recorded %d, all lost %v; want 3 recorded, not all lost", d.Recorded, d.AllLost())
	}
	n, err := RemoveLost(ctx, e.db, d.Lost)
	if err != nil || n != 2 {
		t.Fatalf("RemoveLost = %d, %v; want 2", n, err)
	}
	var left []int64
	rows, err := e.db.Query(`SELECT id FROM conversations ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(left) != fmt.Sprint([]int64{kept}) {
		t.Fatalf("conversations left = %v, want only %d", left, kept)
	}

	// Recorded since the check: not retired after all.
	record(203)
	rt.list[len(rt.list)-1].Retired = true // 205: gone meanwhile, which isn't an error
	n, err = RetireOrphans(ctx, e.db, rt, d.Orphans)
	if err != nil || n != 2 {
		t.Fatalf("RetireOrphans = %d, %v; want 2", n, err)
	}
	if fmt.Sprint(rt.retired) != fmt.Sprint([]int64{200, 202}) {
		t.Fatalf("retired = %v, want 200 and 202", rt.retired)
	}

	// Nothing is left to find.
	if d, err := CheckDrift(ctx, e.db, rt, now); err != nil || len(d.Lost) != 0 || len(d.Orphans) != 0 {
		t.Fatalf("CheckDrift after cleaning = %+v, %v; want nothing", d, err)
	}
}
