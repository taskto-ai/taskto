package agentrt

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

const (
	testEntry       = "../../agent/dist/test-entry.js"
	productionEntry = "../../agent/dist/index.js"
)

const spikeToolsEnv = "TASKTO_AGENT_SPIKE_TOOLS"

type sidecar struct {
	t      *testing.T
	dir    string
	socket string
	cmd    *exec.Cmd
	client *Client
	entry  string
	env    []string
	output lockedBuffer
}

func startSidecar(t *testing.T, env ...string) *sidecar {
	t.Helper()
	return startSidecarAt(t, testEntry, env...)
}

func startSidecarAt(t *testing.T, entry string, env ...string) *sidecar {
	t.Helper()
	if os.Getenv("TASKTO_AGENT_IT") != "1" {
		t.Skip("set TASKTO_AGENT_IT=1 to run the agent sidecar integration tests")
	}
	dir, err := os.MkdirTemp("", "taskto-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	abs, err := filepath.Abs(entry)
	if err != nil {
		t.Fatal(err)
	}
	s := &sidecar{t: t, dir: dir, socket: filepath.Join(dir, "agent.sock"), entry: abs, env: env}
	t.Cleanup(s.stop)
	s.start()
	return s
}

func (s *sidecar) start() {
	s.t.Helper()
	s.cmd = exec.Command("node", s.entry)
	s.cmd.Env = append(os.Environ(),
		"TASKTO_AGENT_SOCKET="+s.socket,
		"TASKTO_AGENT_DB="+filepath.Join(s.dir, "agent.db"),
	)
	s.cmd.Env = append(s.cmd.Env, s.env...)
	s.cmd.Stderr = io.MultiWriter(testWriter{s.t}, &s.output)
	if err := s.cmd.Start(); err != nil {
		s.t.Fatalf("start sidecar: %v", err)
	}
	s.client = New(s.socket)
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := s.client.Healthy(ctx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("sidecar not healthy: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *sidecar) kill() {
	s.t.Helper()
	if err := s.cmd.Process.Kill(); err != nil {
		s.t.Fatal(err)
	}
	_ = s.cmd.Wait()
}

func (s *sidecar) stop() {
	if s.cmd != nil && s.cmd.Process != nil && s.cmd.ProcessState == nil {
		_ = s.cmd.Process.Signal(os.Interrupt)
		_ = s.cmd.Wait()
	}
}

type testWriter struct{ t *testing.T }

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("sidecar: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func ctxFor(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func lastAssistant(t *testing.T, c *Client, conv int64) string {
	t.Helper()
	msgs, err := c.Messages(ctxFor(t), conv)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return msgs[i].Text
		}
	}
	t.Fatalf("no assistant message in %v", msgs)
	return ""
}

func waitForOps(t *testing.T, w *Watch, want string) {
	t.Helper()
	for {
		ev, err := w.Next()
		if err != nil {
			t.Fatalf("watch ended before %q: %v", want, err)
		}
		if ev.Type == "ops" && strings.Contains(string(ev.Ops), want) {
			return
		}
	}
}

func TestRoundTrip(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)

	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.client.Submit(ctx, conv, Input{Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	settled, err := s.client.Wait(ctx, sub)
	if err != nil || settled.Status != "done" {
		t.Fatalf("wait = %+v, %v; want done", settled, err)
	}
	if got := lastAssistant(t, s.client, conv); got != "echo: hello" {
		t.Fatalf("answer = %q, want %q", got, "echo: hello")
	}

	if _, err := s.client.Submit(ctx, conv, Input{Content: "hi", WhenBusy: "sometimes"}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("submit with whenBusy=sometimes: err = %v, want a 400", err)
	}

	again, err := s.client.Submit(ctx, conv, Input{Content: "hi", RequestID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := s.client.Submit(ctx, conv, Input{Content: "hi", RequestID: "r1"})
	if err != nil || repeat != again {
		t.Fatalf("resubmit = %d, %v; want %d", repeat, err, again)
	}
}

func TestTwoWatchersSeeTheSameRun(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var watches [2]*Watch
	for i := range watches {
		w, err := s.client.Watch(ctx, conv)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		if ev, err := w.Next(); err != nil || ev.Type != "value" {
			t.Fatalf("first event = %+v, %v; want the value", ev, err)
		}
		watches[i] = w
	}

	if _, err := s.client.Submit(ctx, conv, Input{Content: "tool:pause_safe:600"}); err != nil {
		t.Fatal(err)
	}
	for _, w := range watches {
		waitForOps(t, w, "waited 200ms")
		waitForOps(t, w, "finished pause_safe")
	}
}

func TestSteerJoinsTheRunningWork(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.client.Watch(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	first, err := s.client.Submit(ctx, conv, Input{Content: "tool:pause_safe:1200"})
	if err != nil {
		t.Fatal(err)
	}
	waitForOps(t, w, "waited 200ms")
	steer, err := s.client.Submit(ctx, conv, Input{Content: "use pnpm", WhenBusy: "steer"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{first, steer} {
		if settled, err := s.client.Wait(ctx, id); err != nil || settled.Status != "done" {
			t.Fatalf("submission %d = %+v, %v; want done", id, settled, err)
		}
	}
	if got := lastAssistant(t, s.client, conv); got != "echo: use pnpm" {
		t.Fatalf("answer = %q, want the steer answered in the same run", got)
	}
}

func TestCrashDuringToolCall(t *testing.T) {
	cases := []struct {
		tool     string
		want     string
		alsoWant string
	}{
		{tool: "pause_safe", want: "finished pause_safe: paused 2500ms"},
		{tool: "pause_once", want: "finished pause_once:", alsoWant: "was interrupted"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			s := startSidecar(t)
			ctx := ctxFor(t)
			conv, err := s.client.CreateConversation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			w, err := s.client.Watch(ctx, conv)
			if err != nil {
				t.Fatal(err)
			}
			sub, err := s.client.Submit(ctx, conv, Input{Content: "tool:" + tc.tool + ":2500"})
			if err != nil {
				t.Fatal(err)
			}
			waitForOps(t, w, "waited 400ms")
			s.kill()
			if _, err := w.Next(); err == nil {
				for err == nil {
					_, err = w.Next()
				}
			} else if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "reset") {
				t.Logf("watch after kill: %v", err)
			}

			s.start()
			settled, err := s.client.Wait(ctx, sub)
			if err != nil || settled.Status != "done" {
				t.Fatalf("after restart: wait = %+v, %v; want done", settled, err)
			}
			got := lastAssistant(t, s.client, conv)
			if !strings.HasPrefix(got, tc.want) || !strings.Contains(got, tc.alsoWant) {
				t.Fatalf("answer = %q, want prefix %q containing %q", got, tc.want, tc.alsoWant)
			}
			t.Logf("answer after crash: %q", got)
		})
	}
}

func TestSecondSidecarRefusesARunningSocket(t *testing.T) {
	s := startSidecar(t)
	entry, err := filepath.Abs("../../agent/dist/index.js")
	if err != nil {
		t.Fatal(err)
	}
	second := exec.Command("node", entry)
	second.Env = append(os.Environ(),
		"TASKTO_AGENT_SOCKET="+s.socket,
		"TASKTO_AGENT_DB="+filepath.Join(s.dir, "agent.db"),
	)
	out, err := second.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("second sidecar: err = %v, output %q; want exit 1", err, out)
	}
	if err := s.client.Healthy(ctxFor(t)); err != nil {
		t.Fatalf("first sidecar after the attempt: %v", err)
	}
	info, err := os.Stat(s.socket)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}

func notFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == 404
}

func TestListAndRetireConversations(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	before := time.Now().UnixMilli()
	keep, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.client.Conversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != keep || list[1].ID != orphan || list[0].Retired || list[1].Retired ||
		list[0].CreatedAt < before || list[1].CreatedAt > time.Now().UnixMilli() {
		t.Fatalf("conversations = %+v, want %d and %d, created since %d, not retired", list, keep, orphan, before)
	}

	w, err := s.client.Watch(ctx, orphan)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	running, err := s.client.Submit(ctx, orphan, Input{Content: "tool:pause_safe:30000"})
	if err != nil {
		t.Fatal(err)
	}
	waitForOps(t, w, "waited 200ms")
	if err := s.client.RetireConversation(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if settled, err := s.client.Wait(waitCtx, running); err != nil || settled.Status != "unanswered" {
		t.Fatalf("the running submission = %+v, %v; want unanswered at once", settled, err)
	}
	if _, err := s.client.Submit(ctx, orphan, Input{Content: "hello"}); !notFound(err) {
		t.Fatalf("submit to a retired conversation: err = %v, want a 404", err)
	}
	if _, err := s.client.Messages(ctx, orphan); !notFound(err) {
		t.Fatalf("messages of a retired conversation: err = %v, want a 404", err)
	}
	if _, err := s.client.Model(ctx, orphan); !notFound(err) {
		t.Fatalf("model of a retired conversation: err = %v, want a 404", err)
	}
	catalog, err := s.client.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.client.SetModel(ctx, orphan, catalog.Default); !notFound(err) {
		t.Fatalf("set model of a retired conversation: err = %v, want a 404", err)
	}
	if _, err := s.client.Watch(ctx, orphan); !notFound(err) {
		t.Fatalf("watch of a retired conversation: err = %v, want a 404", err)
	}
	if err := s.client.RetireConversation(ctx, orphan); !notFound(err) {
		t.Fatalf("retiring twice: err = %v, want a 404", err)
	}
	if err := s.client.RetireConversation(ctx, orphan+1000); !notFound(err) {
		t.Fatalf("retiring an unknown conversation: err = %v, want a 404", err)
	}
	if _, err := s.client.Messages(ctx, keep); err != nil {
		t.Fatalf("messages of the kept conversation: %v", err)
	}

	s.kill()
	s.start()
	list, err = s.client.Conversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Retired || !list[1].Retired {
		t.Fatalf("conversations after a restart = %+v, want %d retired and %d not", list, orphan, keep)
	}
	if _, err := s.client.Messages(ctx, orphan); !notFound(err) {
		t.Fatalf("messages of a retired conversation after a restart: err = %v, want a 404", err)
	}
}

func TestPurgeConversationDeletesItsRecordsFromTheDatabase(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	say := func(conv int64, text string) {
		t.Helper()
		sub, err := s.client.Submit(ctx, conv, Input{Content: text})
		if err != nil {
			t.Fatal(err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if _, err := s.client.Wait(waitCtx, sub); err != nil {
			t.Fatal(err)
		}
	}
	keep, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	doomed, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const keptWord, doomedWord = "kept-quince-4419", "doomed-marmalade-7731"
	say(keep, "remember "+keptWord)
	say(doomed, "remember "+doomedWord)
	holds := func(word string) bool {
		t.Helper()
		for _, name := range []string{"agent.db", "agent.db-wal"} {
			b, err := os.ReadFile(filepath.Join(s.dir, name))
			if err != nil {
				if name == "agent.db-wal" && errors.Is(err, os.ErrNotExist) {
					continue
				}
				t.Fatalf("reading %s: %v", name, err)
			}
			if bytes.Contains(b, []byte(word)) {
				return true
			}
		}
		return false
	}
	if !holds(doomedWord) || !holds(keptWord) {
		t.Fatal("fixture: the database doesn't hold what was said")
	}

	var se *StatusError
	if err := s.client.PurgeConversation(ctx, doomed); !errors.As(err, &se) || se.Code != 409 {
		t.Fatalf("purging a conversation that isn't retired: err = %v, want a 409", err)
	}
	if _, err := s.client.Messages(ctx, doomed); err != nil {
		t.Fatalf("the refused purge changed the conversation: %v", err)
	}

	if err := s.client.RetireConversation(ctx, doomed); err != nil {
		t.Fatal(err)
	}
	if err := s.client.PurgeConversation(ctx, doomed); err != nil {
		t.Fatalf("purging a retired conversation: %v", err)
	}
	if holds(doomedWord) {
		t.Error("what was said in the purged conversation is still in the sidecar's database")
	}
	if !holds(keptWord) {
		t.Error("the purge took the other conversation's records")
	}
	if msgs, err := s.client.Messages(ctx, keep); err != nil || !strings.Contains(fmt.Sprint(msgs), keptWord) {
		t.Fatalf("the kept conversation: %v, %v", msgs, err)
	}
	for _, id := range []int64{doomed, doomed + 1000} {
		if err := s.client.PurgeConversation(ctx, id); err != nil {
			t.Fatalf("purging conversation %d again: %v", id, err)
		}
	}
	s.kill()
	s.start()
	list, err := s.client.Conversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != keep {
		t.Fatalf("conversations after a restart = %+v, want only %d", list, keep)
	}
	fresh, err := s.client.CreateConversation(ctx)
	if err != nil || fresh == doomed {
		t.Fatalf("a new conversation after the purge: %d, %v (it must not reuse %d)", fresh, err, doomed)
	}
	say(fresh, "still working")
}

func TestPurgeConversationIsNotDoneWhileAReaderHoldsTheLog(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	doomed, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const word = "doomed-marmalade-7731"
	sub, err := s.client.Submit(ctx, doomed, Input{Content: "remember " + word})
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := s.client.Wait(waitCtx, sub); err != nil {
		t.Fatal(err)
	}
	inLog := func() bool {
		b, err := os.ReadFile(filepath.Join(s.dir, "agent.db-wal"))
		return err == nil && bytes.Contains(b, []byte(word))
	}
	if !inLog() {
		t.Skip("the database's log doesn't hold the conversation here")
	}
	if err := s.client.RetireConversation(ctx, doomed); err != nil {
		t.Fatal(err)
	}

	reader, err := sql.Open("sqlite", filepath.Join(s.dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	conn, err := reader.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM entries`).Scan(&n); err != nil {
		t.Fatal(err)
	}

	var se *StatusError
	if err := s.client.PurgeConversation(ctx, doomed); !errors.As(err, &se) || se.Code != 503 {
		t.Fatalf("purging while a reader holds the log: err = %v, want a 503", err)
	}
	if !inLog() {
		t.Fatal("fixture: the reader didn't hold the log")
	}
	s.kill()
	s.start()
	if err := s.client.PurgeConversation(ctx, doomed); !errors.As(err, &se) || se.Code != 503 {
		t.Fatalf("purging again, the rows gone, while a reader holds the log: err = %v, want a 503", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := s.client.PurgeConversation(ctx, doomed); err != nil {
		t.Fatalf("the retry once the reader is gone: %v", err)
	}
	if inLog() {
		t.Error("what was said is still in the write-ahead log after the purge was acknowledged")
	}
}

func TestPurgeAskedAgainWhileItRunsHasItsOutcome(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	doomed, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.client.RetireConversation(ctx, doomed); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", filepath.Join(s.dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	conn, err := reader.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM entries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { first <- s.client.PurgeConversation(ctx, doomed) }()
	var se *StatusError
	joined := fmt.Sprintf("conversation %d purge joins the running one", doomed)
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(s.output.String(), joined); {
		if time.Now().After(deadline) {
			t.Fatalf("the purge asked again never joined the running one; output %q", s.output.String())
		}
		if err := s.client.PurgeConversation(ctx, doomed); !errors.As(err, &se) || se.Code != 503 {
			t.Fatalf("the purge asked again while the first waits for the reader: err = %v, want the first's 503", err)
		}
	}
	if err := <-first; !errors.As(err, &se) || se.Code != 503 {
		t.Fatalf("the first purge: err = %v, want a 503", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := s.client.PurgeConversation(ctx, doomed); err != nil {
		t.Fatalf("the retry once the reader is gone: %v", err)
	}
}

func waitForOutput(t *testing.T, s *sidecar, want string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(s.output.String(), want); {
		if time.Now().After(deadline) {
			t.Fatalf("the sidecar never said %q; output %q", want, s.output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSetModelOvertakenByARetireIsRefused(t *testing.T) {
	s := startSidecar(t, "TASKTO_AGENT_TEST_MODEL_PAUSE_MS=600")
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := s.client.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	set := make(chan error, 1)
	go func() { set <- s.client.SetModel(ctx, conv, catalog.Default) }()
	waitForOutput(t, s, "test: the model write is waiting before its commit")
	if err := s.client.RetireConversation(ctx, conv); err != nil {
		t.Fatal(err)
	}
	if err := <-set; !notFound(err) {
		t.Fatalf("a model change the retire overtook: err = %v, want a 404", err)
	}
}

func TestAbortStopsTheRunningWork(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.client.Watch(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	running, err := s.client.Submit(ctx, conv, Input{Content: "tool:pause_safe:30000"})
	if err != nil {
		t.Fatal(err)
	}
	waitForOps(t, w, "waited 200ms")
	if err := s.client.Abort(ctx, conv); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if settled, err := s.client.Wait(waitCtx, running); err != nil || settled.Status != "unanswered" {
		t.Fatalf("the running submission = %+v, %v; want unanswered at once", settled, err)
	}
	next, err := s.client.Submit(ctx, conv, Input{Content: "again"})
	if err != nil {
		t.Fatal(err)
	}
	if settled, err := s.client.Wait(ctx, next); err != nil || settled.Status != "done" {
		t.Fatalf("after the abort: %+v, %v; want done", settled, err)
	}
	if err := s.client.Abort(ctx, conv+1000); !notFound(err) {
		t.Fatalf("aborting an unknown conversation: err = %v, want a 404", err)
	}
}

func TestEntriesPagesBackThroughTheHistory(t *testing.T) {
	s := startSidecar(t)
	ctx := ctxFor(t)
	conv, err := s.client.CreateConversation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"one", "two", "three"} {
		sub, err := s.client.Submit(ctx, conv, Input{Content: m})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.client.Wait(ctx, sub); err != nil {
			t.Fatal(err)
		}
	}
	type page struct {
		Entries []struct {
			ID    int64           `json:"id"`
			Kind  string          `json:"kind"`
			Model json.RawMessage `json:"model"`
		} `json:"entries"`
		Before *int64 `json:"before"`
	}
	get := func(before int64, limit int) page {
		t.Helper()
		raw, err := s.client.Entries(ctx, conv, before, limit)
		if err != nil {
			t.Fatal(err)
		}
		var p page
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return p
	}
	all := get(0, 100)
	if len(all.Entries) < 6 || all.Before != nil {
		t.Fatalf("whole history = %+v, want three questions and answers from the start", all)
	}
	for i := 1; i < len(all.Entries); i++ {
		if all.Entries[i].ID <= all.Entries[i-1].ID {
			t.Fatalf("entries out of order: %+v", all.Entries)
		}
	}
	if first := all.Entries[0]; first.Kind != "pi.user" || !strings.Contains(string(first.Model), `"one"`) {
		t.Fatalf("first entry = %s %s, want the user's message one", first.Kind, first.Model)
	}
	var joined []int64
	before := int64(0)
	for range len(all.Entries) {
		p := get(before, 2)
		if len(p.Entries) == 0 || len(p.Entries) > 2 {
			t.Fatalf("page before %d = %+v", before, p)
		}
		ids := make([]int64, len(p.Entries))
		for i, e := range p.Entries {
			ids[i] = e.ID
		}
		joined = append(ids, joined...)
		if p.Before == nil {
			break
		}
		if *p.Before >= p.Entries[0].ID {
			t.Fatalf("before = %d, not before the page %+v", *p.Before, p)
		}
		before = p.Entries[0].ID
	}
	if fmt.Sprint(joined) != fmt.Sprint(func() []int64 {
		ids := make([]int64, len(all.Entries))
		for i, e := range all.Entries {
			ids[i] = e.ID
		}
		return ids
	}()) {
		t.Fatalf("paged = %v, want %+v", joined, all.Entries)
	}
	if p := get(all.Entries[0].ID, 10); len(p.Entries) != 0 || p.Before != nil {
		t.Fatalf("page before the first entry = %+v, want empty", p)
	}
	if _, err := s.client.Entries(ctx, conv, 0, 101); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("limit 101: err = %v, want a 400", err)
	}
}
