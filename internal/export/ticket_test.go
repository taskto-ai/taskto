package export

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTicketIsSingleUseAndBound(t *testing.T) {
	tk := newTickets(time.Minute)
	var left atomic.Int32
	token, err := tk.issue(1, 2, 3, func() { left.Add(1) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, wrong := range [][3]int64{{9, 2, 3}, {1, 9, 3}, {1, 2, 9}} {
		if _, _, ok := tk.redeem(token, wrong[0], wrong[1], wrong[2]); ok {
			t.Errorf("redeemed by %v", wrong)
		}
	}
	if _, _, ok := tk.redeem("not-a-token", 1, 2, 3); ok {
		t.Error("an unknown token was redeemed")
	}
	_, leave, ok := tk.redeem(token, 1, 2, 3)
	if !ok {
		t.Fatal("the owner couldn't redeem")
	}
	if _, _, again := tk.redeem(token, 1, 2, 3); again {
		t.Error("a ticket was redeemed twice")
	}
	if left.Load() != 0 {
		t.Error("the slot was freed before the download ended")
	}
	leave()
	if left.Load() != 1 {
		t.Errorf("slot freed %d times", left.Load())
	}
}

func TestTicketExpiryFreesTheSlotOnce(t *testing.T) {
	tk := newTickets(20 * time.Millisecond)
	var left atomic.Int32
	token, _ := tk.issue(1, 2, 3, func() { left.Add(1) }, nil)
	deadline := time.Now().Add(5 * time.Second)
	for left.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if left.Load() != 1 {
		t.Fatalf("slot freed %d times after the ticket expired", left.Load())
	}
	if _, _, ok := tk.redeem(token, 1, 2, 3); ok {
		t.Error("an expired ticket was redeemed")
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	return nil
}

func TestIdleWriterExtendsTheDeadlineOnEveryWrite(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	w := &idleWriter{w: rec, rc: http.NewResponseController(rec), idle: time.Minute}
	var last time.Time
	for range 3 {
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(rec.deadlines) != 3 {
		t.Fatalf("%d deadlines set for 3 writes", len(rec.deadlines))
	}
	for _, d := range rec.deadlines {
		if d.IsZero() || time.Until(d) > time.Minute || time.Until(d) < 50*time.Second {
			t.Errorf("deadline %v isn't about a minute away", d)
		}
		if !d.After(last) {
			t.Errorf("deadline %v doesn't extend %v", d, last)
		}
		last = d
	}
}
