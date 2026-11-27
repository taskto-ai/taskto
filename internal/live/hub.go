package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

const (
	TypeReady  = "ready"
	TypeResync = "resync"

	MinBuffer           = 2
	DefaultBuffer       = 64
	DefaultMaxStreams   = 16
	DefaultHeartbeat    = 15 * time.Second
	DefaultRecheck      = 30 * time.Second
	DefaultWriteTimeout = time.Minute

	MaxEventBytes = 64 << 10
	MaxTypeLen    = 64
)

var (
	ErrClosed         = errors.New("live: the hub is closed")
	ErrTooManyStreams = errors.New("live: too many open streams")
	ErrBadEvent       = errors.New("live: not a valid event")
)

type Topic struct {
	Kind       string
	SpaceID    int64
	Generation int64
	ResourceID int64
}

type Event struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

type Ticker func(every time.Duration) (<-chan time.Time, func())

type Config struct {
	WatchSession func(r *http.Request, every time.Duration) (context.Context, context.CancelFunc)
	Buffer       int
	MaxStreams   int
	Heartbeat    time.Duration
	Recheck      time.Duration
	WriteTimeout time.Duration
	Ticker       Ticker
}

type Hub struct {
	cfg     Config
	closing context.Context
	end     context.CancelFunc

	mu      sync.Mutex
	topics  map[Topic]*topicState
	all     map[*subscriber]struct{}
	perUser map[int64]int
}

type topicState struct {
	seq  int64
	subs map[*subscriber]struct{}
}

type subscriber struct {
	user     int64
	topic    Topic
	ch       chan Event
	revoked  chan struct{}
	detached bool
	released bool
	onRevoke func()
}

func New(cfg Config) *Hub {
	if cfg.Buffer <= 0 {
		cfg.Buffer = DefaultBuffer
	}
	if cfg.Buffer < MinBuffer {
		cfg.Buffer = MinBuffer
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = DefaultMaxStreams
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = DefaultHeartbeat
	}
	if cfg.Recheck <= 0 {
		cfg.Recheck = DefaultRecheck
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	closing, end := context.WithCancel(context.Background())
	return &Hub{cfg: cfg, closing: closing, end: end, topics: map[Topic]*topicState{}, all: map[*subscriber]struct{}{}, perUser: map[int64]int{}}
}

func (h *Hub) UseSessions(watch func(r *http.Request, every time.Duration) (context.Context, context.CancelFunc)) {
	h.cfg.WatchSession = watch
}

func (h *Hub) MaxStreams() int { return h.cfg.MaxStreams }

func (h *Hub) Subscribe(ctx context.Context, topic Topic, user int64) (<-chan Event, <-chan struct{}, func(), error) {
	s, err := h.subscribe(topic, user, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { h.unsubscribe(s) })
	return s.ch, s.revoked, func() { stop(); h.unsubscribe(s) }, nil
}

func (h *Hub) subscribe(topic Topic, user int64, onRevoke func()) (*subscriber, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing.Err() != nil {
		return nil, ErrClosed
	}
	if h.perUser[user] >= h.cfg.MaxStreams {
		return nil, ErrTooManyStreams
	}
	ts := h.topics[topic]
	if ts == nil {
		ts = &topicState{subs: map[*subscriber]struct{}{}}
		h.topics[topic] = ts
	}
	s := &subscriber{user: user, topic: topic, ch: make(chan Event, h.cfg.Buffer), revoked: make(chan struct{}), onRevoke: onRevoke}
	ts.subs[s] = struct{}{}
	h.all[s] = struct{}{}
	h.perUser[user]++
	s.ch <- Event{Seq: ts.seq, Type: TypeReady}
	return s, nil
}

func (h *Hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.release(s)
}

func (h *Hub) Publish(topic Topic, typ string, data json.RawMessage) (int64, error) {
	if typ == "" || len(typ) > MaxTypeLen || typ == TypeReady || typ == TypeResync ||
		len(data) > MaxEventBytes || (len(data) > 0 && !json.Valid(data)) {
		return 0, ErrBadEvent
	}
	data = bytes.Clone(data)
	h.mu.Lock()
	defer h.mu.Unlock()
	ts := h.topics[topic]
	if ts == nil {
		return 0, nil
	}
	ts.seq++
	ev := Event{Seq: ts.seq, Type: typ, Data: data}
	for s := range ts.subs {
		select {
		case s.ch <- ev:
		default:
			h.overflow(s, ev.Seq)
		}
	}
	return ev.Seq, nil
}

func (h *Hub) Revoke(match func(topic Topic, user int64) bool) {
	h.mu.Lock()
	var ended []*subscriber
	for s := range h.all {
		if match(s.topic, s.user) {
			h.revoke(s)
			ended = append(ended, s)
		}
	}
	h.mu.Unlock()
	for _, s := range ended {
		if s.onRevoke != nil {
			s.onRevoke()
		}
	}
}

func (h *Hub) EndUserIn(spaceID, user int64) {
	h.Revoke(func(t Topic, u int64) bool { return t.SpaceID == spaceID && u == user })
}

func (h *Hub) EndSpace(spaceID int64) {
	h.Revoke(func(t Topic, _ int64) bool { return t.SpaceID == spaceID })
}

func (h *Hub) EndStreams() {
	h.end()
	h.Revoke(func(Topic, int64) bool { return true })
}

func (h *Hub) overflow(s *subscriber, seq int64) {
	var ready *Event
	for drained := false; !drained; {
		select {
		case ev := <-s.ch:
			if ev.Type == TypeReady {
				ready = &ev
			}
		default:
			drained = true
		}
	}
	if ready != nil {
		s.ch <- *ready
	}
	s.ch <- Event{Seq: seq, Type: TypeResync}
	h.detach(s)
}

func (h *Hub) revoke(s *subscriber) {
	for drained := false; !drained; {
		select {
		case _, ok := <-s.ch:
			drained = !ok
		default:
			drained = true
		}
	}
	h.detach(s)
	select {
	case <-s.revoked:
	default:
		close(s.revoked)
	}
}

func (h *Hub) detach(s *subscriber) {
	if s.detached {
		return
	}
	s.detached = true
	close(s.ch)
	if ts := h.topics[s.topic]; ts != nil {
		delete(ts.subs, s)
		if len(ts.subs) == 0 {
			delete(h.topics, s.topic)
		}
	}
}

func (h *Hub) release(s *subscriber) {
	h.detach(s)
	if s.released {
		return
	}
	s.released = true
	delete(h.all, s)
	if h.perUser[s.user]--; h.perUser[s.user] <= 0 {
		delete(h.perUser, s.user)
	}
}

func (h *Hub) ticks(every time.Duration) (<-chan time.Time, func()) {
	if h.cfg.Ticker != nil {
		return h.cfg.Ticker(every)
	}
	t := time.NewTicker(every)
	return t.C, t.Stop
}
