package agentrt

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type wire struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func serveWire(t *testing.T) (*Client, *wire) {
	t.Helper()
	dir, err := os.MkdirTemp("", "tt-wire-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "a.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	w := &wire{}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.mu.Lock()
		w.bodies = append(w.bodies, body)
		w.mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"submissionId":1,"revision":4,"provider":"p","modelId":"m","model":{"provider":"p","modelId":"m"}}`))
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return New(socket), w
}

func (w *wire) last() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bodies[len(w.bodies)-1]
}

func TestAModelWriteAlwaysSendsTheRevisionItExpectsEvenWhenItIsZero(t *testing.T) {
	c, w := serveWire(t)
	next, err := c.SetModel(context.Background(), 1, 0, ModelRef{Provider: "p", ModelID: "m"})
	if err != nil || next != 4 {
		t.Fatalf("SetModel = %d, %v; want the revision the runtime answered, 4", next, err)
	}
	if got, ok := w.last()["expectRevision"]; !ok || got != float64(0) {
		t.Fatalf("the model write sent %v, want expectRevision 0", w.last())
	}
	if _, _, err := c.Continue(context.Background(), 1, 0, ModelRef{Provider: "p", ModelID: "m"}, 3); err != nil {
		t.Fatal(err)
	}
	if got, ok := w.last()["expectRevision"]; !ok || got != float64(0) {
		t.Fatalf("the continue sent %v, want expectRevision 0", w.last())
	}
}

func TestEverySubmissionAndContinueCarriesTheDeadlineOfItsContextAsTheAdmitByTime(t *testing.T) {
	c, w := serveWire(t)
	by := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), by)
	defer cancel()
	revision := int64(2)
	if _, err := c.Submit(ctx, 1, Input{Content: "hello", ExpectRevision: &revision}); err != nil {
		t.Fatal(err)
	}
	if got := w.last()["admitBy"]; got != float64(by.UnixMilli()) {
		t.Fatalf("the submission sent admitBy %v, want %d", got, by.UnixMilli())
	}
	if w.last()["expectRevision"] != float64(2) {
		t.Fatalf("the submission sent %v, want expectRevision 2", w.last())
	}
	if _, _, err := c.Continue(ctx, 1, 2, ModelRef{Provider: "p", ModelID: "m"}, 3); err != nil {
		t.Fatal(err)
	}
	if got := w.last()["admitBy"]; got != float64(by.UnixMilli()) {
		t.Fatalf("the continue sent admitBy %v, want %d", got, by.UnixMilli())
	}
	if _, err := c.Submit(context.Background(), 1, Input{Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.last()["admitBy"]; ok {
		t.Fatalf("a submission whose context has no deadline sent %v, want no admitBy", w.last())
	}
}
