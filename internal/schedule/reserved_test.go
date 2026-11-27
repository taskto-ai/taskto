package schedule

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

const serverKind Kind = "test.server"

func (w *world) fillKind(kind Kind, n int) {
	w.t.Helper()
	exec(w.t, w.db, `
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO scheduled_jobs (kind, user_id, workspace_id, dedupe_key, run_at, created_at, updated_at)
		SELECT ?, ?, ?, 'fill' || i, ?, ?, ? FROM n`,
		n, string(kind), w.alice, w.ws, at(start.Add(time.Hour)), at(start), at(start))
}

func (w *world) serverJob(key string) Job {
	return Job{Kind: serverKind, UserID: w.alice, WorkspaceID: w.ws, DedupeKey: key, RunAt: w.clock.now().Add(time.Hour)}
}

func TestTheServersJobsHaveRoomWhenThePersonIsAtTheBound(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.register(remind)
	if err := w.s.RegisterServer(serverKind, func(context.Context, Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	w.fillKind(remind, MaxPerUser)
	if _, err := w.s.Schedule(ctx, w.db, w.job("one more", time.Hour)); !errors.Is(err, ErrTooMany) {
		t.Fatalf("a job of their own past the bound = %v, want ErrTooMany", err)
	}
	if created, err := w.s.Reschedule(ctx, w.db, w.serverJob("purge")); err != nil || !created {
		t.Fatalf("the server's job at the person's bound = %v, %v; want created", created, err)
	}
	exec(t, w.db, `UPDATE scheduled_jobs SET state = 'failed' WHERE kind = ?`, string(serverKind))
	if _, err := w.s.Reschedule(ctx, w.db, w.serverJob("purge")); err != nil {
		t.Fatalf("making the server's failed job pending again at the person's bound = %v", err)
	}
	if created, err := w.s.Schedule(ctx, w.db, w.serverJob("another")); err != nil || !created {
		t.Fatalf("Schedule of the server's job at the person's bound = %v, %v", created, err)
	}
}

func TestTheServersJobsAreBoundedApartFromThePersons(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	w.register(remind)
	if err := w.s.RegisterServer(serverKind, func(context.Context, Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	w.fillKind(serverKind, MaxServerPerUser-1)
	w.fillKind(remind, MaxPerUser-1)
	if !w.schedule(w.job("last of theirs", time.Hour)) {
		t.Fatal("the person's last job wasn't made")
	}
	if !w.schedule(w.serverJob("last of the server's")) {
		t.Fatal("the server's last job wasn't made")
	}
	if _, err := w.s.Schedule(ctx, w.db, w.job("past theirs", time.Hour)); !errors.Is(err, ErrTooMany) {
		t.Fatalf("past the person's bound = %v, want ErrTooMany", err)
	}
	for _, j := range []Job{w.serverJob("past the server's")} {
		if _, err := w.s.Schedule(ctx, w.db, j); !errors.Is(err, ErrTooMany) {
			t.Fatalf("Schedule past the server's bound = %v, want ErrTooMany", err)
		}
		if _, err := w.s.Reschedule(ctx, w.db, j); !errors.Is(err, ErrTooMany) {
			t.Fatalf("Reschedule past the server's bound = %v, want ErrTooMany", err)
		}
	}
	var n int
	if err := w.db.QueryRow(`SELECT count(*) FROM scheduled_jobs WHERE user_id = ? AND state IN ('pending', 'running')`, w.alice).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != MaxPerUser+MaxServerPerUser {
		t.Fatalf("%d active jobs, want %d", n, MaxPerUser+MaxServerPerUser)
	}
}

func TestTheServersJobsRunInTheirTurn(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	var order []string
	note := func(_ context.Context, j Job) error { order = append(order, j.DedupeKey); return nil }
	if err := w.s.Register(remind, note); err != nil {
		t.Fatal(err)
	}
	if err := w.s.RegisterServer(serverKind, note); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		w.schedule(w.job(fmt.Sprintf("before%d", i), time.Duration(i+1)*time.Minute))
	}
	purge := w.serverJob("purge")
	purge.RunAt = w.clock.now().Add(10 * time.Minute)
	w.schedule(purge)
	for i := range 3 {
		w.schedule(w.job(fmt.Sprintf("after%d", i), time.Duration(i+11)*time.Minute))
	}
	w.clock.advance(time.Hour)
	w.s.RunDue(ctx)
	want := []string{"before0", "before1", "before2", "purge", "after0", "after1", "after2"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("ran %v, want %v", order, want)
	}
}

func TestTheServersJobTakesNoneOfThePersonsRoom(t *testing.T) {
	ctx := context.Background()
	for _, how := range []string{"schedule", "reactivate"} {
		t.Run(how, func(t *testing.T) {
			w := newWorld(t)
			w.register(remind)
			if err := w.s.RegisterServer(serverKind, func(context.Context, Job) error { return nil }); err != nil {
				t.Fatal(err)
			}
			w.fillKind(remind, MaxPerUser-1)
			if !w.schedule(w.serverJob("purge")) {
				t.Fatal("the server's job wasn't made")
			}
			var err error
			if how == "schedule" {
				_, err = w.s.Schedule(ctx, w.db, w.job("the 1,000th", time.Hour))
			} else {
				exec(t, w.db, `INSERT INTO scheduled_jobs (kind, user_id, workspace_id, dedupe_key, run_at, state, created_at, updated_at)
					VALUES (?, ?, ?, 'finished', ?, 'done', ?, ?)`, string(remind), w.alice, w.ws, at(start), at(start), at(start))
				_, err = w.s.Reschedule(ctx, w.db, w.job("finished", time.Hour))
			}
			if err != nil {
				t.Fatalf("the person's 1,000th job beside the server's: %v", err)
			}
			if _, err := w.s.Schedule(ctx, w.db, w.job("the 1,001st", time.Hour)); !errors.Is(err, ErrTooMany) {
				t.Fatalf("past the person's bound = %v, want ErrTooMany", err)
			}
		})
	}
}
