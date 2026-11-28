package live

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type countingWriter struct {
	http.ResponseWriter
	entered  *atomic.Int64
	returned *atomic.Int64
}

func (w countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w countingWriter) Write(p []byte) (int, error) {
	if len(p) > 1024 {
		w.entered.Add(1)
		defer w.returned.Add(1)
	}
	return w.ResponseWriter.Write(p)
}

func TestAnEarlierWritesDeadlineDoesNotResetAnHTTP2StreamDuringASlowAccessCheck(t *testing.T) {
	const timeout = 300 * time.Millisecond
	g := startRig(t, Config{WriteTimeout: timeout}, true, func(w http.ResponseWriter) http.ResponseWriter { return w })
	var slow atomic.Bool
	g.recheck = func(ctx context.Context) error {
		if slow.Load() {
			select {
			case <-time.After(3 * timeout):
			case <-ctx.Done():
			}
		}
		return nil
	}
	s := g.connect("a")
	if s.res.ProtoMajor != 2 {
		t.Fatalf("the stream runs over HTTP/%d, want HTTP/2", s.res.ProtoMajor)
	}
	g.publish(`{"n":1}`)
	if ev := s.event(); string(ev.Data) != `{"n":1}` {
		t.Fatalf("first = %+v", ev)
	}
	slow.Store(true)
	g.publish(`{"n":2}`)
	if ev := s.event(); string(ev.Data) != `{"n":2}` {
		t.Fatalf("second = %+v", ev)
	}
}

func TestARevocationInterruptsAnHTTP2WriteThatIsStalledOnItsClient(t *testing.T) {
	var entered, returned atomic.Int64
	g := startRig(t, Config{WriteTimeout: time.Hour, Buffer: 32}, true, func(w http.ResponseWriter) http.ResponseWriter {
		return countingWriter{ResponseWriter: w, entered: &entered, returned: &returned}
	})
	g.srv.Client().Transport.(*http.Transport).HTTP2 = &http.HTTP2Config{MaxReceiveBufferPerStream: 64 << 10}
	s := g.connect("a")
	if s.res.ProtoMajor != 2 {
		t.Fatalf("the stream runs over HTTP/%d, want HTTP/2", s.res.ProtoMajor)
	}
	big := `"` + strings.Repeat("x", 48<<10) + `"`
	for range 12 {
		g.publish(big)
	}
	stalledSince := time.Time{}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if entered.Load() > returned.Load() {
			if stalledSince.IsZero() {
				stalledSince = time.Now()
			}
			if time.Since(stalledSince) > 300*time.Millisecond {
				break
			}
		} else {
			stalledSince = time.Time{}
		}
		if time.Now().After(deadline) {
			t.Fatal("the write never stalled on the client's window")
		}
	}
	returnedRevocation := make(chan struct{})
	go func() {
		defer close(returnedRevocation)
		g.hub.EndUserIn(room.SpaceID, 1)
	}()
	select {
	case <-returnedRevocation:
	case <-time.After(5 * time.Second):
		t.Fatal("revocation did not interrupt a write stalled on an HTTP/2 stream")
	}
	g.waitSubscribers(0)
}
