package live

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

var room = Topic{Kind: "board", SpaceID: 7, Generation: 3, ResourceID: 11}

func take(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("the channel is closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return Event{}
	}
}

func closed(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("got %+v, want a closed channel", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the channel stayed open")
	}
}

func silent(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		t.Fatalf("got %+v (open %v), want nothing", ev, ok)
	default:
	}
}

func subscribe(t *testing.T, h *Hub, topic Topic, user int64) (<-chan Event, func()) {
	t.Helper()
	ch, _, stop, err := h.Subscribe(context.Background(), topic, user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return ch, stop
}

func publish(t *testing.T, h *Hub, topic Topic, typ, data string) int64 {
	t.Helper()
	seq, err := h.Publish(topic, typ, json.RawMessage(data))
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestSubscribersReceiveInOrderWithIncreasingSeq(t *testing.T) {
	h := New(Config{})
	a, _ := subscribe(t, h, room, 1)
	b, _ := subscribe(t, h, room, 2)
	for _, ch := range []<-chan Event{a, b} {
		if ev := take(t, ch); ev.Type != TypeReady || ev.Seq != 0 {
			t.Fatalf("first = %+v, want ready at 0", ev)
		}
	}
	for i := 1; i <= 5; i++ {
		if seq := publish(t, h, room, "stroke", `{"n":`+string(rune('0'+i))+`}`); seq != int64(i) {
			t.Fatalf("seq = %d, want %d", seq, i)
		}
	}
	for _, ch := range []<-chan Event{a, b} {
		for i := 1; i <= 5; i++ {
			ev := take(t, ch)
			if ev.Seq != int64(i) || ev.Type != "stroke" || string(ev.Data) != `{"n":`+string(rune('0'+i))+`}` {
				t.Fatalf("event %d = %+v", i, ev)
			}
		}
	}
}

func TestALateSubscriberStartsAtTheTopicsCurrentSeq(t *testing.T) {
	h := New(Config{})
	subscribe(t, h, room, 1)
	publish(t, h, room, "stroke", `1`)
	publish(t, h, room, "stroke", `2`)
	late, _ := subscribe(t, h, room, 2)
	if ev := take(t, late); ev.Type != TypeReady || ev.Seq != 2 {
		t.Fatalf("ready = %+v, want seq 2", ev)
	}
	if seq := publish(t, h, room, "stroke", `3`); seq != 3 {
		t.Fatalf("seq = %d", seq)
	}
	if ev := take(t, late); ev.Seq != 3 {
		t.Fatalf("next = %+v", ev)
	}
}

func TestATopicOfAnotherGenerationGetsNothing(t *testing.T) {
	h := New(Config{})
	old, _ := subscribe(t, h, room, 1)
	take(t, old)
	recreated := room
	recreated.Generation++
	fresh, _ := subscribe(t, h, recreated, 1)
	take(t, fresh)

	publish(t, h, recreated, "stroke", `1`)
	silent(t, old)
	if ev := take(t, fresh); ev.Seq != 1 {
		t.Fatalf("fresh = %+v", ev)
	}
	publish(t, h, room, "stroke", `2`)
	silent(t, fresh)
	if ev := take(t, old); ev.Seq != 1 {
		t.Fatalf("old = %+v: the old topic counts on its own", ev)
	}
}

func TestATopicOfAnotherKindSpaceOrResourceGetsNothing(t *testing.T) {
	h := New(Config{})
	ch, _ := subscribe(t, h, room, 1)
	take(t, ch)
	for i, other := range []Topic{
		{Kind: "notes", SpaceID: 7, Generation: 3, ResourceID: 11},
		{Kind: "board", SpaceID: 8, Generation: 3, ResourceID: 11},
		{Kind: "board", SpaceID: 7, Generation: 3, ResourceID: 12},
	} {
		theirs, _ := subscribe(t, h, other, int64(10+i))
		take(t, theirs)
		publish(t, h, other, "stroke", `1`)
		if ev := take(t, theirs); ev.Seq != 1 {
			t.Fatalf("%+v got %+v", other, ev)
		}
	}
	silent(t, ch)
}

func TestASlowSubscriberGetsResyncAndIsDroppedWithoutBlockingOthers(t *testing.T) {
	h := New(Config{Buffer: 3})
	slow, stopSlow := subscribe(t, h, room, 1)
	fast, _ := subscribe(t, h, room, 2)
	take(t, fast)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= 10; i++ {
			if seq := publish(t, h, room, "stroke", `0`); seq != int64(i) {
				t.Errorf("seq = %d, want %d", seq, i)
				return
			}
			if ev := take(t, fast); ev.Seq != int64(i) {
				t.Errorf("fast = %+v, want seq %d", ev, i)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a slow subscriber blocked a publish")
	}
	var got []Event
	for end := time.After(5 * time.Second); ; {
		select {
		case ev, ok := <-slow:
			if ok {
				got = append(got, ev)
				continue
			}
		case <-end:
			t.Fatal("the slow subscriber's channel was never closed")
		}
		break
	}
	if len(got) != 2 || got[0].Type != TypeReady || got[1].Type != TypeResync || got[1].Seq != 3 {
		t.Fatalf("slow got %+v, want its ready and then a resync at the event that overflowed", got)
	}
	if n := h.openStreams(); n != 2 {
		t.Fatalf("%d streams, want the fast one and the slow one that has not stopped", n)
	}
	stopSlow()
	if n := h.openStreams(); n != 1 {
		t.Fatalf("%d streams left, want the fast one", n)
	}
}

func TestThePerUserCapCountsAcrossTopicsAndFreesOnStop(t *testing.T) {
	h := New(Config{MaxStreams: 2})
	_, stop := subscribe(t, h, room, 1)
	other := room
	other.ResourceID = 99
	subscribe(t, h, other, 1)
	if _, _, _, err := h.Subscribe(context.Background(), room, 1); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("third = %v, want too many streams", err)
	}
	if _, _, _, err := h.Subscribe(context.Background(), room, 2); err != nil {
		t.Fatalf("another user = %v", err)
	}
	stop()
	stop()
	if _, _, _, err := h.Subscribe(context.Background(), room, 1); err != nil {
		t.Fatalf("after a stop = %v", err)
	}
}

func TestACancelledContextEndsTheSubscription(t *testing.T) {
	h := New(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	ch, _, _, err := h.Subscribe(ctx, room, 1)
	if err != nil {
		t.Fatal(err)
	}
	take(t, ch)
	cancel()
	closed(t, ch)
	if n := h.openStreams(); n != 0 {
		t.Fatalf("%d streams after the context ended", n)
	}
}

func TestEndStreamsClosesEveryoneAndRefusesLaterSubscribes(t *testing.T) {
	h := New(Config{})
	a, _ := subscribe(t, h, room, 1)
	b, _ := subscribe(t, h, Topic{Kind: "notes", SpaceID: 1, Generation: 1, ResourceID: 1}, 2)
	h.EndStreams()
	for _, ch := range []<-chan Event{a, b} {
		closed(t, ch)
	}
	if _, _, _, err := h.Subscribe(context.Background(), room, 3); !errors.Is(err, ErrClosed) {
		t.Fatalf("subscribe after the end = %v", err)
	}
}

func TestEndUserInEndsOnlyThatUsersStreamsInThatSpace(t *testing.T) {
	h := New(Config{})
	gone, stopGone := subscribe(t, h, room, 1)
	stays, _ := subscribe(t, h, room, 2)
	elsewhere := room
	elsewhere.SpaceID = 8
	otherSpace, _ := subscribe(t, h, elsewhere, 1)
	h.EndUserIn(7, 1)
	closed(t, gone)
	take(t, stays)
	take(t, otherSpace)
	publish(t, h, room, "stroke", `1`)
	if ev := take(t, stays); ev.Seq != 1 {
		t.Fatalf("stays = %+v", ev)
	}
	closed(t, gone)
	stopGone()
	if n := h.openStreams(); n != 2 {
		t.Fatalf("%d streams, want 2", n)
	}
}

func TestEndSpaceEndsEveryStreamOfThatSpace(t *testing.T) {
	h := New(Config{})
	a, _ := subscribe(t, h, room, 1)
	b, _ := subscribe(t, h, room, 2)
	elsewhere := room
	elsewhere.SpaceID = 8
	c, _ := subscribe(t, h, elsewhere, 3)
	h.EndSpace(7)
	for _, ch := range []<-chan Event{a, b} {
		closed(t, ch)
	}
	take(t, c)
	silent(t, c)
}

func TestPublishRefusesWhatCannotBeSentSafely(t *testing.T) {
	h := New(Config{})
	ch, _ := subscribe(t, h, room, 1)
	take(t, ch)
	big := make([]byte, MaxEventBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	big[0], big[len(big)-1] = '"', '"'
	long := make([]byte, MaxTypeLen+1)
	for i := range long {
		long[i] = 'a'
	}
	for name, c := range map[string]struct {
		typ  string
		data string
	}{
		"no type":        {"", `1`},
		"a long type":    {string(long), `1`},
		"the ready type": {TypeReady, `1`},
		"resync":         {TypeResync, `1`},
		"not JSON":       {"stroke", `{`},
		"too big":        {"stroke", string(big)},
	} {
		if _, err := h.Publish(room, c.typ, json.RawMessage(c.data)); !errors.Is(err, ErrBadEvent) {
			t.Errorf("%s: %v, want a bad event", name, err)
		}
	}
	silent(t, ch)
	if seq := publish(t, h, room, "stroke", ``); seq != 1 {
		t.Fatalf("seq = %d: a refused event used a number", seq)
	}
	if ev := take(t, ch); len(ev.Data) != 0 {
		t.Fatalf("data = %q, want none", ev.Data)
	}
}

func TestPublishingToATopicNobodyHoldsDoesNothing(t *testing.T) {
	h := New(Config{})
	if seq := publish(t, h, room, "stroke", `1`); seq != 0 {
		t.Fatalf("seq = %d", seq)
	}
	if len(h.topics) != 0 {
		t.Fatal("a topic with no subscriber was kept")
	}
}

func (h *Hub) openStreams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.perUser {
		n += c
	}
	return n
}

func revokedSignal(t *testing.T, revoked <-chan struct{}) {
	t.Helper()
	select {
	case <-revoked:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscription was not told it was revoked")
	}
}

func TestRevokingDiscardsWhatIsQueuedAndSignalsIt(t *testing.T) {
	h := New(Config{})
	ch, revoked, stop, err := h.Subscribe(context.Background(), room, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	publish(t, h, room, "stroke", `1`)
	publish(t, h, room, "stroke", `2`)
	h.EndUserIn(room.SpaceID, 1)
	revokedSignal(t, revoked)
	closed(t, ch)
}

func TestRevokingAStreamThatOverflowedStillEndsIt(t *testing.T) {
	h := New(Config{Buffer: 2})
	ch, revoked, stop, err := h.Subscribe(context.Background(), room, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	for range 3 {
		publish(t, h, room, "stroke", `1`)
	}
	if n := len(h.topics); n != 0 {
		t.Fatalf("%d topics, the overflowed stream should have left the fan-out", n)
	}
	h.EndUserIn(room.SpaceID, 1)
	revokedSignal(t, revoked)
	closed(t, ch)
}

func TestAnOverflowedStreamKeepsItsSlotUntilItIsStopped(t *testing.T) {
	h := New(Config{Buffer: 2, MaxStreams: 1})
	_, stop := subscribe(t, h, room, 1)
	for range 3 {
		publish(t, h, room, "stroke", `1`)
	}
	if _, _, _, err := h.Subscribe(context.Background(), room, 1); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("a second stream while the first has not returned = %v, want too many streams", err)
	}
	stop()
	if _, _, _, err := h.Subscribe(context.Background(), room, 1); err != nil {
		t.Fatalf("after the first stopped = %v", err)
	}
}

func TestOverflowBeforeTheFirstEventKeepsReadyAheadOfResync(t *testing.T) {
	h := New(Config{Buffer: 4})
	ch, _ := subscribe(t, h, room, 1)
	for range 6 {
		publish(t, h, room, "stroke", `1`)
	}
	if ev := take(t, ch); ev.Type != TypeReady || ev.Seq != 0 {
		t.Fatalf("first = %+v, want the ready baseline", ev)
	}
	if ev := take(t, ch); ev.Type != TypeResync {
		t.Fatalf("second = %+v, want the resync", ev)
	}
	closed(t, ch)
}

func TestPublishKeepsItsOwnCopyOfTheValidatedBytes(t *testing.T) {
	h := New(Config{})
	ch, _ := subscribe(t, h, room, 1)
	take(t, ch)
	data := []byte(`{"n":1}`)
	if _, err := h.Publish(room, "stroke", data); err != nil {
		t.Fatal(err)
	}
	copy(data, `x7`)
	if ev := take(t, ch); string(ev.Data) != `{"n":1}` {
		t.Fatalf("data = %q, the publisher's later write reached the queue", ev.Data)
	}
}

func TestAOneEventBufferStillOverflowsWithoutBlockingAndKeepsBothControlFrames(t *testing.T) {
	h := New(Config{Buffer: 1})
	ch, _, _, err := h.Subscribe(context.Background(), room, 1)
	if err != nil {
		t.Fatal(err)
	}
	published := make(chan struct{})
	go func() {
		defer close(published)
		for range 3 {
			if _, err := h.Publish(room, "stroke", json.RawMessage(`1`)); err != nil {
				return
			}
		}
	}()
	select {
	case <-published:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing blocked on a subscriber whose buffer was requested as one event")
	}
	if ev := take(t, ch); ev.Type != TypeReady {
		t.Fatalf("first = %+v, want ready", ev)
	}
	if ev := take(t, ch); ev.Type != TypeResync {
		t.Fatalf("second = %+v, want resync", ev)
	}
	closed(t, ch)
}
