package drain

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func serve(t *testing.T, h http.Handler) (*http.Server, string) {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, "http://" + ln.Addr().String()
}

func get(addr string) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, addr, nil)
	if err != nil {
		return
	}
	if res, err := http.DefaultClient.Do(req); err == nil {
		_ = res.Body.Close()
	}
}

func TestShutdownWaitsForARunningHandlerAndEndsItsContext(t *testing.T) {
	tr := New()
	started := make(chan struct{})
	var finished atomic.Bool
	srv, addr := serve(t, tr.Handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		time.Sleep(200 * time.Millisecond)
		finished.Store(true)
	})))
	go get(addr)
	<-started

	if err := tr.Shutdown(srv, 50*time.Millisecond, 5*time.Second); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	if !finished.Load() {
		t.Fatal("Shutdown returned while a handler was still running")
	}
}

func TestShutdownReportsAHandlerThatNeverReturns(t *testing.T) {
	tr := New()
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	srv, addr := serve(t, tr.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})))
	go get(addr)
	<-started

	began := time.Now()
	err := tr.Shutdown(srv, 0, 100*time.Millisecond)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Shutdown = %v, want ErrBusy", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("Shutdown took %s with a 100 ms wait", took)
	}
}

func TestStoppedTrackerRefusesRequests(t *testing.T) {
	tr := New()
	var calls atomic.Int32
	h := tr.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	tr.Stop()
	tr.Stop()

	rec := &recorder{}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(rec, req)
	if rec.code != http.StatusServiceUnavailable || calls.Load() != 0 {
		t.Fatalf("after Stop: status %d, %d calls; want 503 and none", rec.code, calls.Load())
	}
	if err := tr.Wait(time.Second); err != nil {
		t.Fatalf("Wait with nothing running = %v", err)
	}
}

type recorder struct {
	code int
	h    http.Header
}

func (r *recorder) Header() http.Header {
	if r.h == nil {
		r.h = http.Header{}
	}
	return r.h
}
func (r *recorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *recorder) WriteHeader(code int)        { r.code = code }
